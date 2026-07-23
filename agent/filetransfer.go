package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
}

type transferSession struct {
	id        string
	direction string // upload / download
	path      string
	file      *os.File
	hash      hashWriter // SHA256 增量
	bytesDone int64
	total     int64
	startTime time.Time
	lastProg  time.Time // 进度限流
	cancel    context.CancelFunc
}

// hashWriter 包装 sha256 使其实现 io.Writer。
type hashWriter struct{ sum [32]byte; w io.Writer; h bool }

// TransferEngine 全局实例。
var transfers = &TransferEngine{sessions: make(map[string]*transferSession)}

// HandleInit 处理 file_xfer_init：准备文件，返回 ready（含续传偏移）。
func (te *TransferEngine) HandleInit(init proto.FileTransferInit) proto.FileTransferReady {
	ready := proto.FileTransferReady{TransferID: init.TransferID}

	partPath := init.Path + ".part"
	var resumeOffset int64 = 0

	if init.Direction == "upload" {
		// 上传：检查 .part 文件是否存在（续传）
		if info, err := os.Stat(partPath); err == nil {
			resumeOffset = info.Size()
			log.Printf("[xfer] 续传 %s: 从 offset=%d 继续", init.TransferID, resumeOffset)
		}
		// 打开/创建 .part 文件（追加模式）
		f, err := os.OpenFile(partPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			ready.Reason = "打开文件失败: " + err.Error()
			return ready
		}
		te.mu.Lock()
		te.sessions[init.TransferID] = &transferSession{
			id: init.TransferID, direction: "upload", path: init.Path,
			file: f, total: init.Size, startTime: time.Now(),
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

// ReceiveUploadData 接收上传的二进制数据帧，写入文件 + SHA256。
// 由 recvLoop 在收到 MessageBinary 时调用。
func (te *TransferEngine) ReceiveUploadData(transferID string, data []byte, isLast bool, progressCb func(proto.FileTransferProgress), completeCb func(proto.FileTransferComplete)) {
	te.mu.Lock()
	sess, ok := te.sessions[transferID]
	te.mu.Unlock()
	if !ok {
		return
	}

	// 写入文件
	n, err := sess.file.Write(data)
	if err != nil {
		te.completeUpload(transferID, false, 0, "写文件失败: "+err.Error(), completeCb)
		return
	}
	sess.bytesDone += int64(n)

	// 进度上报（500ms 限流）
	now := time.Now()
	if now.Sub(sess.lastProg) > 500*time.Millisecond || isLast {
		sess.lastProg = now
		prog := te.makeProgress(sess)
		progressCb(prog)
	}

	if isLast {
		// 计算完整文件 SHA256（重新读 .part 文件）
		sess.file.Close()
		partPath := sess.path + ".part"
		hash := te.computeFileHash(partPath)
		// 重命名为最终文件
		if err := os.Rename(partPath, sess.path); err != nil {
			te.completeUpload(transferID, false, sess.bytesDone, "重命名失败: "+err.Error(), completeCb)
			return
		}
		te.completeUpload(transferID, true, sess.bytesDone, hash, completeCb)
	}
}

// completeUpload 完成上传会话。
func (te *TransferEngine) completeUpload(id string, success bool, bytes int64, hashOrError string, cb func(proto.FileTransferComplete)) {
	te.mu.Lock()
	delete(te.sessions, id)
	te.mu.Unlock()
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
			frame := buildBinaryFrame(transferID, false, buf[:n])
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
	frame := buildBinaryFrame(transferID, true, nil)
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
func (te *TransferEngine) Cancel(transferID string) {
	te.mu.Lock()
	sess, ok := te.sessions[transferID]
	if ok {
		delete(te.sessions, transferID)
	}
	te.mu.Unlock()
	if ok && sess.file != nil {
		sess.file.Close()
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
	io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil))
}

// buildBinaryFrame 构造二进制 WS 帧。
// 格式：[transferID_len u8][transferID][isLast u8][data]
func buildBinaryFrame(transferID string, isLast bool, data []byte) []byte {
	idBytes := []byte(transferID)
	if len(idBytes) > 255 {
		idBytes = idBytes[:255]
	}
	frame := make([]byte, 0, 1+len(idBytes)+1+len(data))
	frame = append(frame, byte(len(idBytes)))
	frame = append(frame, idBytes...)
	if isLast {
		frame = append(frame, 1)
	} else {
		frame = append(frame, 0)
	}
	frame = append(frame, data...)
	return frame
}

// parseBinaryFrame 解析二进制 WS 帧，返回 transferID + isLast + data。
func parseBinaryFrame(frame []byte) (transferID string, isLast bool, data []byte, err error) {
	if len(frame) < 2 {
		return "", false, nil, fmt.Errorf("帧太短")
	}
	idLen := int(frame[0])
	if len(frame) < 1+idLen+1 {
		return "", false, nil, fmt.Errorf("帧不完整")
	}
	transferID = string(frame[1 : 1+idLen])
	isLast = frame[1+idLen] != 0
	data = frame[2+idLen:]
	return transferID, isLast, data, nil
}

// partFilePath 返回 .part 文件的完整路径。
func partFilePath(finalPath string) string {
	return finalPath + ".part"
}

// marshalJSON 辅助。
func marshalJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// filePath 辅助（避免冲突）。
func xferFilePath(p string) string { return filepath.Clean(p) }
