package executors

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/QiuShichang/go-rmm/proto"
)

// FileMaxBytes 单次文件任务的大小上限（10MB，避免 WS 帧过大）。
const FileMaxBytes = 10 * 1024 * 1024

// FileExecutor 文件上传/下载。
//
//   - op=upload：把 ContentB64（base64）解码后写到 Path
//   - op=download：读 Path 文件，base64 编码后放进结果 Data
//
// 安全（S5 修复）：所有路径经 FilePolicy.Check 校验，禁止穿越到系统目录/敏感文件。
type FileExecutor struct {
	policy FilePolicy
}

// NewFile 构造执行器，使用给定策略。策略为零值时用 DefaultFilePolicy。
func NewFile(opts ...func(*FileExecutor)) *FileExecutor {
	e := &FileExecutor{policy: DefaultFilePolicy()}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// WithFilePolicy 注入自定义策略。
func WithFilePolicy(p FilePolicy) func(*FileExecutor) {
	return func(e *FileExecutor) { e.policy = p }
}

// WithAllowedRoots 便捷设置允许根目录。
func WithAllowedRoots(roots ...string) func(*FileExecutor) {
	return func(e *FileExecutor) {
		e.policy.AllowedRoots = roots
	}
}

func (e *FileExecutor) Type() string { return proto.TaskFile }

func (e *FileExecutor) Exec(ctx context.Context, payload []byte, _ ProgressFunc) proto.TaskResult {
	var task proto.FileTask
	if err := decode(payload, &task); err != nil {
		return fail("", err.Error())
	}
	if task.Path == "" {
		return fail("", "path 不能为空")
	}

	// S5 修复：路径穿越校验（分块上传的目标路径也要校验）
	switch task.Op {
	case "upload", "write", "chunk_upload", "download", "read", "chunk_download":
		if err := e.policy.Check(task.Path); err != nil {
			return fail("", err.Error())
		}
	}

	switch task.Op {
	case "upload", "write":
		return e.upload(task)
	case "download", "read":
		return e.download(task)
	case "chunk_upload":
		return e.chunkUpload(task)
	case "chunk_download":
		return e.chunkDownload(task)
	default:
		return fail("", "不支持的 op: "+task.Op)
	}
}

func (e *FileExecutor) upload(task proto.FileTask) proto.TaskResult {
	data, err := base64.StdEncoding.DecodeString(task.ContentB64)
	if err != nil {
		return fail("", "base64 解码失败: "+err.Error())
	}
	if len(data) > FileMaxBytes {
		return fail("", "文件过大")
	}
	if err := os.WriteFile(task.Path, data, 0644); err != nil {
		return fail("", "写文件失败: "+err.Error())
	}
	r := ok("")
	r.Stdout = "uploaded"
	r.Data, _ = jsonMarshal(map[string]any{"path": task.Path, "bytes": len(data)})
	return r
}

func (e *FileExecutor) download(task proto.FileTask) proto.TaskResult {
	info, err := os.Stat(task.Path)
	if err != nil {
		return fail("", "文件不存在: "+err.Error())
	}
	if info.Size() > FileMaxBytes {
		return fail("", "文件过大")
	}
	data, err := os.ReadFile(task.Path)
	if err != nil {
		return fail("", "读文件失败: "+err.Error())
	}
	r := ok("")
	r.Data, _ = jsonMarshal(map[string]any{
		"path":        task.Path,
		"bytes":       len(data),
		"content_b64": base64.StdEncoding.EncodeToString(data),
	})
	return r
}

// chunkChunkSize 单个分块大小（1MB，base64 后约 1.3MB，远低于 WS 帧限制）。
const chunkChunkSize = 1024 * 1024

// M2 修复：per-UploadID 合并锁，防并发合并竞态。
var chunkMergeMu sync.Mutex                        // 保护 chunkMergeLocks map
var chunkMergeLocks = make(map[string]*sync.Mutex) // UploadID → 互斥锁

// getChunkMergeLock 获取（或创建）指定 UploadID 的互斥锁。
func getChunkMergeLock(uploadID string) *sync.Mutex {
	chunkMergeMu.Lock()
	defer chunkMergeMu.Unlock()
	mu, ok := chunkMergeLocks[uploadID]
	if !ok {
		mu = &sync.Mutex{}
		chunkMergeLocks[uploadID] = mu
	}
	return mu
}

// chunkUpload 处理分块上传：按 UploadID 写临时分片，最后一块（ChunkIndex==TotalChunks-1）合并。
func (e *FileExecutor) chunkUpload(task proto.FileTask) proto.TaskResult {
	if task.UploadID == "" || task.TotalChunks == 0 {
		return fail("", "upload_id 和 total_chunks 不能为空")
	}
	// 临时目录存放分片
	tmpDir := filepath.Join(os.TempDir(), "go-rmm-chunks-"+task.UploadID)
	if err := os.MkdirAll(tmpDir, 0755); err != nil {
		return fail("", "创建临时目录失败: "+err.Error())
	}
	// 写当前分片
	chunkPath := filepath.Join(tmpDir, fmt.Sprintf("chunk-%06d", task.ChunkIndex))
	data, err := base64.StdEncoding.DecodeString(task.ContentB64)
	if err != nil {
		return fail("", "base64 解码失败: "+err.Error())
	}
	// A8 修复：单块大小校验
	if len(data) > chunkChunkSize*2 {
		return fail("", fmt.Sprintf("分块 %d 超过上限 %d", len(data), chunkChunkSize*2))
	}
	if err := os.WriteFile(chunkPath, data, 0644); err != nil {
		return fail("", "写分片失败: "+err.Error())
	}

	// M2 修复：per-UploadID 互斥锁保护合并临界区，防并发合并
	mergeMu := getChunkMergeLock(task.UploadID)
	mergeMu.Lock()
	defer mergeMu.Unlock()

	// 检查是否所有分片到齐
	entries, _ := os.ReadDir(tmpDir)
	if len(entries) < task.TotalChunks {
		r := ok("")
		r.Stdout = fmt.Sprintf("chunk %d/%d received", task.ChunkIndex+1, task.TotalChunks)
		r.Data, _ = jsonMarshal(map[string]any{"chunk_index": task.ChunkIndex, "received": len(entries), "total": task.TotalChunks})
		return r
	}

	// 所有分片到齐，合并到目标路径
	out, err := os.Create(task.Path)
	if err != nil {
		return fail("", "创建目标文件失败: "+err.Error())
	}
	defer out.Close()
	totalBytes := 0
	for i := 0; i < task.TotalChunks; i++ {
		cp := filepath.Join(tmpDir, fmt.Sprintf("chunk-%06d", i))
		chunkData, err := os.ReadFile(cp)
		if err != nil {
			return fail("", fmt.Sprintf("读分片 %d 失败: %s", i, err.Error()))
		}
		n, _ := out.Write(chunkData)
		totalBytes += n
	}
	// 清理临时分片 + A8 修复:清理 merge 锁 map 防内存泄漏
	os.RemoveAll(tmpDir)
	// P2 修复:delete chunkMergeLocks 必须在 chunkMergeMu 保护内,
	// 否则与 getChunkMergeLock 的 map 读写并发(race detector 必报 + 极端情况误删他人锁)
	chunkMergeMu.Lock()
	delete(chunkMergeLocks, task.UploadID)
	chunkMergeMu.Unlock()

	r := ok("")
	r.Stdout = fmt.Sprintf("merged %d chunks, %d bytes", task.TotalChunks, totalBytes)
	r.Data, _ = jsonMarshal(map[string]any{"path": task.Path, "bytes": totalBytes, "chunks": task.TotalChunks})
	return r
}

// chunkDownload 处理分块下载：按 ChunkIndex 读取文件对应段返回。
func (e *FileExecutor) chunkDownload(task proto.FileTask) proto.TaskResult {
	info, err := os.Stat(task.Path)
	if err != nil {
		return fail("", "文件不存在: "+err.Error())
	}
	fileSize := int(info.Size())
	offset := task.ChunkIndex * chunkChunkSize
	if offset >= fileSize {
		// 超出文件范围，返回空（表示分块结束）
		r := ok("")
		r.Stdout = "eof"
		r.Data, _ = jsonMarshal(map[string]any{"chunk_index": task.ChunkIndex, "bytes": 0, "eof": true})
		return r
	}
	end := offset + chunkChunkSize
	if end > fileSize {
		end = fileSize
	}
	// 读取指定段
	f, err := os.Open(task.Path)
	if err != nil {
		return fail("", "打开文件失败: "+err.Error())
	}
	defer f.Close()
	if _, err := f.Seek(int64(offset), 0); err != nil {
		return fail("", "Seek 失败: "+err.Error())
	}
	buf := make([]byte, end-offset)
	n, err := f.Read(buf)
	if err != nil && n == 0 {
		return fail("", "读文件失败: "+err.Error())
	}
	totalChunks := (fileSize + chunkChunkSize - 1) / chunkChunkSize
	r := ok("")
	r.Data, _ = jsonMarshal(map[string]any{
		"chunk_index":  task.ChunkIndex,
		"total_chunks": totalChunks,
		"bytes":        n,
		"content_b64":  base64.StdEncoding.EncodeToString(buf[:n]),
		"eof":          task.ChunkIndex+1 >= totalChunks,
	})
	return r
}
