package executors

import (
	"context"
	"encoding/base64"
	"os"

	"github.com/QiuShichang/go-rmm/proto"
)

// FileMaxBytes 单次文件任务的大小上限（10MB，避免 WS 帧过大）。
const FileMaxBytes = 10 * 1024 * 1024

// FileExecutor 文件上传/下载。
//
//   - op=upload：把 ContentB64（base64）解码后写到 Path
//   - op=download：读 Path 文件，base64 编码后放进结果 Data
type FileExecutor struct{}

func NewFile() *FileExecutor { return &FileExecutor{} }

func (e *FileExecutor) Type() string { return proto.TaskFile }

func (e *FileExecutor) Exec(ctx context.Context, payload []byte, _ ProgressFunc) proto.TaskResult {
	var task proto.FileTask
	if err := decode(payload, &task); err != nil {
		return fail("", err.Error())
	}
	if task.Path == "" {
		return fail("", "path 不能为空")
	}

	switch task.Op {
	case "upload", "write":
		return e.upload(task)
	case "download", "read":
		return e.download(task)
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
		"path":       task.Path,
		"bytes":      len(data),
		"content_b64": base64.StdEncoding.EncodeToString(data),
	})
	return r
}
