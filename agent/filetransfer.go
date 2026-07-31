package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/QiuShichang/go-rmm/proto"
)

// TransferBufferSize 流式传输缓冲大小（256KB，平衡吞吐与内存）。
const TransferBufferSize = 256 * 1024

// TransferEngine 管理 Agent 侧的大文件传输会话。
//
// J1 核心：用二进制 WS 帧直接传原始字节（去掉 base64），io.Copy 流式（不全载入内存），
// SHA256 增量校验（io.MultiWriter），.part 临时文件支持断点续传。
// 不经 bbolt/Executor/任务状态机——文件传输是独立会话。
type TransferEngine struct {
	mu       sync.Mutex
	sessions map[string]*transferSession
	policy   *fileTransferPolicy // B3 修复：路径校验
	workspace string              // FT:相对路径解析基准(避免落到 SYSTEM CWD)
}

// fileTransferPolicy 复用 executors.FilePolicy 的路径校验逻辑。
// Agent 侧没有 executors 包依赖，这里用独立类型 + 注入校验函数。
type fileTransferPolicy struct {
	check func(string) error
}

// 全局实例。
var transfers = &TransferEngine{
	sessions: make(map[string]*transferSession),
	policy:   &fileTransferPolicy{check: allowAllPaths}, // 默认放行，由 Agent 初始化时注入
}

// allowAllPaths 默认放行（向后兼容）。
func allowAllPaths(string) error { return nil }

// SetPolicy 注入路径校验函数（由 Agent 从 executors.FilePolicy 适配）。
func (te *TransferEngine) SetPolicy(check func(string) error) {
	te.policy = &fileTransferPolicy{check: check}
}

// SetWorkspace 注入 workspace 目录(相对路径解析基准)。
// FT 修复:Agent 以 SYSTEM 身份运行时 CWD=C:\Windows\System32,
// 相对路径会落到系统目录。注入 workspace 后,相对路径统一解析到 workspace。
func (te *TransferEngine) SetWorkspace(ws string) {
	te.workspace = ws
}

// resolveToWorkspace 把相对路径解析到 workspace(而非 CWD)。
// 绝对路径原样返回。
func resolveToWorkspace(path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	ws := transfers.workspace
	if ws == "" {
		return path // 无 workspace 配置,保持原样(向后兼容)
	}
	return filepath.Join(ws, path)
}

type transferSession struct {
	id        string
	direction string // upload / download
	path      string
	file      *os.File
	hash      hash.Hash // C2:增量 SHA256(每次 Write 时更新,isLast 时直接 Sum)
	bytesDone int64
	total     int64
	expectedSHA256 string // M8: Console 预算的摘要,完成时比对
	startTime time.Time
	lastProg  time.Time // 进度限流
	cancel    context.CancelFunc
	// P0-D1 修复:per-session 互斥锁。
	// recvLoop 对每个上传帧 `go handleUploadData`,多 goroutine 并发操作同一 session。
	// 此锁保证 file.Write + hash.Write + bytesDone 串行化,杜绝乱序写入与 data race。
	mu sync.Mutex
}

// hashWriter 包装 sha256 使其实现 io.Writer。
type hashWriter struct{ sum [32]byte; w io.Writer; h bool }

// HandleInit 处理 file_xfer_init：准备文件，返回 ready（含续传偏移）。
func (te *TransferEngine) HandleInit(init proto.FileTransferInit) proto.FileTransferReady {
	ready := proto.FileTransferReady{TransferID: init.TransferID}

	// FT 修复:相对路径解析到 workspace(而非 SYSTEM 的 CWD C:\Windows\System32)。
	// 之前相对路径 "foo.zip" 会落到 SYSTEM 的 CWD,导致文件散落在系统目录。
	// 现在统一 join 到 workspace(Agent 启动时配置),与路径策略 Check 的行为一致。
	init.Path = resolveToWorkspace(init.Path)

	// B3 修复：路径穿越校验（upload 和 download 都校验）
	if te.policy != nil && te.policy.check != nil {
		if err := te.policy.check(init.Path); err != nil {
			ready.Reason = "路径策略拒绝: " + err.Error()
			return ready
		}
	}

	if init.Direction == "upload" {
		// 上传:检查 .part 文件是否存在(续传)
		partPath := init.Path + ".part"
		var resumeOffset int64 = 0
		if info, err := os.Stat(partPath); err == nil {
			resumeOffset = info.Size()
			log.Printf("[xfer] 续传 %s: 从 offset=%d 继续", init.TransferID, resumeOffset)
		}
		// 打开/创建 .part 文件(追加模式)
		f, err := os.OpenFile(partPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			ready.Reason = "打开文件失败: " + err.Error()
			return ready
		}
		// P0-D2 修复:续传时重算已有 .part 前 resumeOffset 字节的 hash
		// 旧实现新建 hash 是空的,只算续传后追加部分 → SHA256 必然校验失败。
		// 现在把已有 .part 内容喂给 hash,使最终 Sum 覆盖完整文件。
		h := sha256.New()
		if resumeOffset > 0 {
			if err := feedExistingToHash(partPath, resumeOffset, h); err != nil {
				log.Printf("[xfer] 续传 hash 初始化失败 %s: %v(从头开始)", init.TransferID, err)
				// hash 初始化失败:截断 .part 从头开始
				f.Close()
				os.Truncate(partPath, 0)
				f, err = os.OpenFile(partPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
				if err != nil {
					ready.Reason = "重置续传文件失败: " + err.Error()
					return ready
				}
				h = sha256.New()
				resumeOffset = 0
			}
		}
		te.mu.Lock()
		te.sessions[init.TransferID] = &transferSession{
			id: init.TransferID, direction: "upload", path: init.Path,
			file: f, total: init.Size, startTime: time.Now(),
			expectedSHA256: init.SHA256,
			hash: h, // C2/P0-D2:增量 hash(续传时已喂入已有部分)
		}
		te.mu.Unlock()
		ready.Accepted = true
		ready.ResumeOffset = resumeOffset
		log.Printf("[xfer] 上传会话 %s 就绪: path=%s resume=%d", init.TransferID, init.Path, resumeOffset)

	} else if init.Direction == "download" {
		// 下载：打开源文件
		info, err := os.Stat(init.Path)
		if err != nil {
			ready.Reason = "文件不存在: " + err.Error()
			return ready
		}
		f, err := os.Open(init.Path)
		if err != nil {
			ready.Reason = "打开文件失败: " + err.Error()
			return ready
		}
		te.mu.Lock()
		te.sessions[init.TransferID] = &transferSession{
			id: init.TransferID, direction: "download", path: init.Path,
			file: f, total: info.Size(), startTime: time.Now(),
		}
		te.mu.Unlock()
		ready.Accepted = true
		ready.Size = info.Size()
		log.Printf("[xfer] 下载会话 %s 就绪: path=%s size=%d", init.TransferID, init.Path, info.Size())
	}

	return ready
}

// feedExistingToHash 把 path 文件前 n 字节喂给 hasher(用于续传 hash 初始化)。
// P0-D2 修复:续传时 hash 必须覆盖已有 .part 内容,否则最终 Sum 必然校验失败。
func feedExistingToHash(path string, n int64, h hash.Hash) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	buf := make([]byte, 64*1024)
	var read int64
	for read < n {
		want := int64(len(buf))
		if read+want > n {
			want = n - read
		}
		nr, err := f.Read(buf[:want])
		if nr > 0 {
			h.Write(buf[:nr])
			read += int64(nr)
		}
		if err != nil {
			if err == io.EOF && read == n {
				break // 正好读完
			}
			return err
		}
	}
	return nil
}

// ReceiveUploadData 接收上传的二进制数据帧,写入文件 + SHA256。
// 由 recvLoop 在收到 MessageBinary 时调用。
//
// P0-D1 修复:整个函数体在 sess.mu.Lock 内串行执行,
// 保证 file.Write + hash.Write + bytesDone 的顺序一致(多 goroutine 并发调用时)。
func (te *TransferEngine) ReceiveUploadData(transferID string, data []byte, isLast bool, progressCb func(proto.FileTransferProgress), completeCb func(proto.FileTransferComplete)) {
	te.mu.Lock()
	sess, ok := te.sessions[transferID]
	te.mu.Unlock()
	if !ok {
		return
	}

	// P0-D1:per-session 锁,串行化写文件+hash+计数
	sess.mu.Lock()
	defer sess.mu.Unlock()

	// A7 修复:写入前检查累计大小不超限
	if sess.total > 0 && sess.bytesDone+int64(len(data)) > sess.total*2 {
		te.completeUpload(transferID, false, sess.bytesDone, "上传数据超过声明大小的 2 倍,拒绝", completeCb)
		return
	}
	// 写入文件 + C2 增量 hash
	n, err := sess.file.Write(data)
	if err != nil {
		te.completeUpload(transferID, false, 0, "写文件失败: "+err.Error(), completeCb)
		return
	}
	if sess.hash != nil {
		sess.hash.Write(data[:n]) // C2:流过即算,不再重读
	}
	sess.bytesDone += int64(n)

	// 进度上报(500ms 限流)
	now := time.Now()
	if now.Sub(sess.lastProg) > 500*time.Millisecond || isLast {
		sess.lastProg = now
		prog := te.makeProgress(sess)
		progressCb(prog)
	}

	if isLast {
		// C2:直接用增量 hash 的结果(不再重读文件)
		sess.file.Close()
		partPath := sess.path + ".part"
		var hashHex string
		if sess.hash != nil {
			hashHex = hex.EncodeToString(sess.hash.Sum(nil))
		} else {
			// 降级:重读文件算 hash
			hashHex = te.computeFileHash(partPath)
		}
		if hashHex == "" {
			te.completeUpload(transferID, false, sess.bytesDone, "SHA256 计算失败", completeCb)
			return
		}
		// M8:双边校验——比对 Console 预算的摘要
		if sess.expectedSHA256 != "" && hashHex != sess.expectedSHA256 {
			os.Remove(partPath)
			te.completeUpload(transferID, false, sess.bytesDone,
				fmt.Sprintf("SHA256 校验失败: 期望 %s 实际 %s", sess.expectedSHA256[:16], hashHex[:16]), completeCb)
			return
		}
		if err := os.Rename(partPath, sess.path); err != nil {
			te.completeUpload(transferID, false, sess.bytesDone, "重命名失败: "+err.Error(), completeCb)
			return
		}
		te.completeUpload(transferID, true, sess.bytesDone, hashHex, completeCb)
	}
}

// completeUpload 完成上传会话。失败时清理 .part 文件（M1 修复）。
func (te *TransferEngine) completeUpload(id string, success bool, bytes int64, hashOrError string, cb func(proto.FileTransferComplete)) {
	te.mu.Lock()
	sess, hadSession := te.sessions[id]
	delete(te.sessions, id)
	te.mu.Unlock()
	if !success && hadSession {
		// M1 修复：上传失败删 .part 文件，防磁盘泄漏
		partPath := sess.path + ".part"
		os.Remove(partPath)
		if sess.file != nil {
			sess.file.Close()
		}
	}
	if success {
		cb(proto.FileTransferComplete{TransferID: id, Success: true, SHA256Actual: hashOrError, Bytes: bytes})
		log.Printf("[xfer] 上传完成 %s: %d bytes sha256=%s", id, bytes, hashOrError[:16])
	} else {
		cb(proto.FileTransferComplete{TransferID: id, Success: false, Bytes: bytes, Error: hashOrError})
		log.Printf("[xfer] 上传失败 %s: %s", id, hashOrError)
	}
}

// SendDownloadData 发送下载的二进制数据帧（Agent→Relay）。
// 用 websocket.Writer 流式发送，不全载入内存。
func (te *TransferEngine) SendDownloadData(ctx context.Context, wc *wsConn, transferID string, progressCb func(proto.FileTransferProgress), completeCb func(proto.FileTransferComplete)) {
	te.mu.Lock()
	sess, ok := te.sessions[transferID]
	te.mu.Unlock()
	if !ok {
		completeCb(proto.FileTransferComplete{TransferID: transferID, Success: false, Error: "会话不存在"})
		return
	}
	defer sess.file.Close()

	// SHA256 增量
	h := sha256.New()
	buf := make([]byte, TransferBufferSize)
	for {
		n, err := sess.file.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
			// 构造二进制帧：[transferID_len u8][transferID][isLast u8][data]
			frame := proto.BuildBinaryFrame(transferID, false, buf[:n])
			wc.writeMu.Lock()
			wErr := wc.conn.Write(ctx, websocket.MessageBinary, frame)
			wc.writeMu.Unlock()
			if wErr != nil {
				completeCb(proto.FileTransferComplete{TransferID: transferID, Success: false, Error: "WS写失败: " + wErr.Error(), Bytes: sess.bytesDone})
				return
			}
			sess.bytesDone += int64(n)
			// 进度限流
			now := time.Now()
			if now.Sub(sess.lastProg) > 500*time.Millisecond {
				sess.lastProg = now
				progressCb(te.makeProgress(sess))
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			completeCb(proto.FileTransferComplete{TransferID: transferID, Success: false, Error: "读文件失败: " + err.Error(), Bytes: sess.bytesDone})
			return
		}
	}

	// 发送 isLast 标记帧（空数据）
	frame := proto.BuildBinaryFrame(transferID, true, nil)
	wc.writeMu.Lock()
	wc.conn.Write(ctx, websocket.MessageBinary, frame)
	wc.writeMu.Unlock()

	hash := hex.EncodeToString(h.Sum(nil))
	te.mu.Lock()
	delete(te.sessions, transferID)
	te.mu.Unlock()
	completeCb(proto.FileTransferComplete{TransferID: transferID, Success: true, SHA256Actual: hash, Bytes: sess.bytesDone})
	log.Printf("[xfer] 下载完成 %s: %d bytes sha256=%s", transferID, sess.bytesDone, hash[:16])
}

// Cancel 取消传输。
// P2 修复:upload 会话取消时删除 .part 文件,防磁盘垃圾 + 防同 ID 重试基于废数据续传。
func (te *TransferEngine) Cancel(transferID string) {
	te.mu.Lock()
	sess, ok := te.sessions[transferID]
	if ok {
		delete(te.sessions, transferID)
	}
	te.mu.Unlock()
	if ok {
		if sess.file != nil {
			sess.file.Close()
		}
		// upload 取消删 .part,防残留垃圾 + 防续传基于废数据
		if sess.direction == "upload" {
			os.Remove(sess.path + ".part")
		}
	}
	log.Printf("[xfer] 取消 %s", transferID)
}

// CloseAll 关闭所有传输会话（连接断开时）。
func (te *TransferEngine) CloseAll() {
	te.mu.Lock()
	ids := make([]string, 0, len(te.sessions))
	for id, s := range te.sessions {
		if s.file != nil {
			s.file.Close()
		}
		ids = append(ids, id)
	}
	te.sessions = make(map[string]*transferSession)
	te.mu.Unlock()
	if len(ids) > 0 {
		log.Printf("[xfer] 关闭 %d 个传输会话", len(ids))
	}
}

// makeProgress 构造进度（含速度估算）。
func (te *TransferEngine) makeProgress(sess *transferSession) proto.FileTransferProgress {
	elapsed := time.Since(sess.startTime).Seconds()
	var speed int64
	if elapsed > 0 {
		speed = int64(float64(sess.bytesDone) / elapsed)
	}
	pct := 0
	if sess.total > 0 {
		pct = int(sess.bytesDone * 100 / sess.total)
	}
	return proto.FileTransferProgress{
		TransferID: sess.id, BytesDone: sess.bytesDone,
		Total: sess.total, Percent: pct, SpeedBps: speed,
	}
}

// computeFileHash 计算文件的 SHA256。
func (te *TransferEngine) computeFileHash(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil { // M3 修复：不忽略错误
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}
