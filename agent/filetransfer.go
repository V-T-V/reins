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
	"strings"
	"sync"
	"sync/atomic"
	"time"

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
	pathInUse map[string]struct{} // R1 修复:upload path 互斥,防并发同 path 双写 .part
	// Q4:文件传输累计计数器(原子操作,无锁)。供未来 metrics 端点/健康检查读取。
	uploadBytes   atomic.Int64
	downloadBytes atomic.Int64
	uploadCount   atomic.Int64
	downloadCount atomic.Int64
	failedCount   atomic.Int64
}

// fileTransferPolicy 复用 executors.FilePolicy 的路径校验逻辑。
// Agent 侧没有 executors 包依赖，这里用独立类型 + 注入校验函数。
type fileTransferPolicy struct {
	check func(string) error
}

// 全局实例。
var transfers = &TransferEngine{
	sessions:  make(map[string]*transferSession),
	pathInUse: make(map[string]struct{}),
	policy:    &fileTransferPolicy{check: allowAllPaths}, // 默认放行，由 Agent 初始化时注入
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

// SnapshotStats 读取传输计数器快照(V1:心跳上报给 Relay)。
// 返回 5 个累计值,原子读无锁。
func (te *TransferEngine) SnapshotStats() (ub, db, uc, dc, fc int64) {
	return te.uploadBytes.Load(), te.downloadBytes.Load(),
		te.uploadCount.Load(), te.downloadCount.Load(), te.failedCount.Load()
}

// releasePath 释放 path 占用标记(R1 互斥的配套释放)。
// 所有 upload session 退出路径(成功/失败/取消)都应调用。
func (te *TransferEngine) releasePath(path string) {
	te.mu.Lock()
	delete(te.pathInUse, path)
	te.mu.Unlock()
}

// tryCompleteFullPart 是 R2 已满快路径:当 .part 文件已达到声明大小,
// 直接算 hash → 校验 → rename 成正式文件,返回 (sha256Hex, 是否成功)。
// 成功后 .part 已清理(renamed),调用方需释放 pathInUse 并发 complete。
// 失败时 .part 被删除(校验/rename 失败),返回 ("", false) 让调用方从头开始。
func (te *TransferEngine) tryCompleteFullPart(init proto.FileTransferInit, partPath string, size int64) (string, bool) {
	h := sha256.New()
	if err := feedExistingToHash(partPath, size, h); err != nil {
		log.Printf("[xfer] 已满快路径 hash 失败 %s: %v", init.TransferID, err)
		os.Remove(partPath)
		return "", false
	}
	hashHex := hex.EncodeToString(h.Sum(nil))
	// 校验(若 Console 提供了 expectedSHA256)
	if init.SHA256 != "" && hashHex != init.SHA256 {
		log.Printf("[xfer] 已满快路径校验失败 %s: 期望 %s 实际 %s", init.TransferID, init.SHA256[:16], hashHex[:16])
		os.Remove(partPath)
		return "", false
	}
	// rename 成正式文件
	if err := os.Rename(partPath, init.Path); err != nil {
		log.Printf("[xfer] 已满快路径 rename 失败 %s: %v(删除 .part 强制重传)", init.TransferID, err)
		os.Remove(partPath) // 强制删除,避免死循环(下次重传从头开始)
		return "", false
	}
	log.Printf("[xfer] 已满快路径完成 %s: %d bytes sha256=%s", init.TransferID, size, hashHex[:16])
	return hashHex, true
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
	limit     int64 // O3:download 分片模式本次最多读取的字节数(0=读到 EOF)
	expectedSHA256 string // M8: Console 预算的摘要,完成时比对
	startTime time.Time
	lastProg  time.Time // 进度限流
	cancel    context.CancelFunc
	// P0-D1 修复:per-session 互斥锁。
	// recvLoop 对每个上传帧 `go handleUploadData`,多 goroutine 并发操作同一 session。
	// 此锁保证 file.Write + hash.Write + bytesDone 串行化,杜绝乱序写入与 data race。
	mu sync.Mutex
}

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
		// R1 修复:path 互斥。session map 以 transferID 为 key,但 .part 文件以 path 为 key。
		// CLI 重试换新 transferID 但相同 path,两个 session 同时 O_APPEND 同一 .part →
		// 数据双写 → SHA256 不匹配 → HTTP 500。这里用 pathInUse 拒绝并发同 path。
		te.mu.Lock()
		if _, inUse := te.pathInUse[init.Path]; inUse {
			te.mu.Unlock()
			ready.Reason = "该路径正在被另一个传输占用,请稍后重试"
			log.Printf("[xfer] 拒绝并发同 path %s(被占用)", init.Path)
			return ready
		}
		te.pathInUse[init.Path] = struct{}{}
		te.mu.Unlock()

		// 上传:检查 .part 文件是否存在(续传)
		partPath := init.Path + ".part"
		var resumeOffset int64 = 0
		if info, err := os.Stat(partPath); err == nil {
			resumeOffset = info.Size()
			log.Printf("[xfer] 续传 %s: 从 offset=%d 继续", init.TransferID, resumeOffset)
		}

		// R2 修复:已满快路径。.part 已满(resumeOffset >= init.Size)时,
		// 直接算 hash → 校验 → rename → 发 complete,不再等 sender 跳过全部字节 + 发 isLast。
		// 之前等 sender 走完整流程,跨网络时序脆弱(实测"跳过 10485760 字节"后间歇 500)。
		// 成功后 ready 带上 actualSHA256,handleTransferInit 据此直接发 complete(不建 session)。
		if init.Size > 0 && resumeOffset >= init.Size {
			actualSHA, ok := te.tryCompleteFullPart(init, partPath, resumeOffset)
			if ok {
				te.releasePath(init.Path) // 已满快路径不建 session,手动释放
				ready.Accepted = true
				ready.ResumeOffset = resumeOffset
				ready.Size = init.Size
				ready.Reason = "ALREADY_COMPLETE:" + actualSHA // 约定前缀,携带 SHA
				log.Printf("[xfer] 已满快路径 %s: .part=%d 直接完成 sha256=%s", init.TransferID, resumeOffset, actualSHA[:16])
				return ready
			}
			// 快路径失败(hash 校验失败等):.part 已被清理,从头开始
			resumeOffset = 0
		}

		// 打开/创建 .part 文件(追加模式)
		f, err := os.OpenFile(partPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			te.releasePath(init.Path)
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
					te.releasePath(init.Path)
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
		// R5+Q5:offset 边界检查。
		// offset 超过文件大小直接拒绝(用 >= 而非 >,offset==size 是空下载,静默成功易误导)。
		if init.Offset > 0 && init.Offset >= info.Size() {
			ready.Reason = fmt.Sprintf("offset %d 超过或等于文件大小 %d(无数据可读)", init.Offset, info.Size())
			return ready
		}
		f, err := os.Open(init.Path)
		if err != nil {
			ready.Reason = "打开文件失败: " + err.Error()
			return ready
		}
		// O3+R5:分片下载支持 + limit 收敛。
		// - Offset > 0 时 Seek 到指定偏移开始读
		// - limit > 0 时只读 limit 字节(Size 字段复用为本次长度),否则读到 EOF
		// - offset+limit 超过文件大小时,收敛 limit 到剩余可读长度
		var limit int64
		if init.Size > 0 {
			limit = init.Size
			// R5:offset+limit 超界时收敛,避免读到 EOF 返回不足
			if init.Offset+limit > info.Size() {
				limit = info.Size() - init.Offset
				if limit < 0 {
					limit = 0
				}
				// Q5:收敛到 0 说明 offset 已在文件末尾,拒绝而非静默返回空数据
				if limit == 0 {
					f.Close()
					ready.Reason = fmt.Sprintf("offset %d + limit %d 越界(文件大小 %d),无可读数据",
						init.Offset, init.Size, info.Size())
					return ready
				}
				log.Printf("[xfer] limit 收敛 %s: offset=%d + limit=%d > size=%d → limit=%d",
					init.TransferID, init.Offset, init.Size, info.Size(), limit)
			}
		}
		if init.Offset > 0 {
			if _, err := f.Seek(init.Offset, 0); err != nil {
				f.Close()
				ready.Reason = "Seek 失败: " + err.Error()
				return ready
			}
		}
		te.mu.Lock()
		te.sessions[init.TransferID] = &transferSession{
			id: init.TransferID, direction: "download", path: init.Path,
			file: f, total: info.Size(), limit: limit, startTime: time.Now(),
		}
		te.mu.Unlock()
		ready.Accepted = true
		ready.Size = info.Size()
		log.Printf("[xfer] 下载会话 %s 就绪: path=%s size=%d offset=%d limit=%d", init.TransferID, init.Path, info.Size(), init.Offset, limit)
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
	buf := Get64K() // H1:pool 复用
	defer Put64K(buf)
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
// R1 修复:统一释放 pathInUse(所有 upload 出口的汇聚点)。
func (te *TransferEngine) completeUpload(id string, success bool, bytes int64, hashOrError string, cb func(proto.FileTransferComplete)) {
	te.mu.Lock()
	sess, hadSession := te.sessions[id]
	delete(te.sessions, id)
	if hadSession && sess.path != "" {
		delete(te.pathInUse, sess.path) // R1:释放 path 占用
	}
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
		// Q4:累加成功计数(只对真正的成功传输计数,非 ALREADY_COMPLETE 快路径)
		if hashOrError != "" && len(hashOrError) >= 32 { // sha256 hex 才算真正完成
			te.uploadBytes.Add(bytes)
			te.uploadCount.Add(1)
		}
		log.Printf("[xfer] 上传完成 %s: %d bytes sha256=%s", id, bytes, hashOrError[:16])
	} else {
		cb(proto.FileTransferComplete{TransferID: id, Success: false, Bytes: bytes, Error: hashOrError})
		te.failedCount.Add(1) // Q4:失败计数
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
	buf := Get256K() // H1:pool 复用(原 make 256K,1GB 下载少 4096 次 256K 分配)
	defer Put256K(buf)
	for {
		// O3:分片模式 limit > 0 时,只读到 limit 字节就停(而非文件 EOF)
		if sess.limit > 0 && sess.bytesDone >= sess.limit {
			break
		}
		n, err := sess.file.Read(buf)
		if n > 0 {
			// 分片模式:最后一帧可能超过 limit,截断
			if sess.limit > 0 && sess.bytesDone+int64(n) > sess.limit {
				n = int(sess.limit - sess.bytesDone)
			}
			h.Write(buf[:n])
			// 构造二进制帧：[transferID_len u8][transferID][isLast u8][data]
			frame := proto.BuildBinaryFrame(transferID, false, buf[:n])
			writeCtx, writeCancel := context.WithTimeout(ctx, 15*time.Second) // 弱网:下载写 15s deadline(不无限持锁,给心跳间歇窗口)
			wErr := wc.writeBinaryData(writeCtx, frame)
			writeCancel()
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
		// 分片模式:达到 limit 不算 EOF 错误,正常结束
		if sess.limit > 0 && sess.bytesDone >= sess.limit {
			break
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			completeCb(proto.FileTransferComplete{TransferID: transferID, Success: false, Error: "读文件失败: " + err.Error(), Bytes: sess.bytesDone})
			return
		}
	}

	// 发送 isLast 标记帧（空数据）。H-5:检查写错——WS 半坏时 isLast 写失败,
	// 原 ignoring 让 Relay 收不到 EOF→下载 consumer 卡 <-dataCh 直到断线 cleanup(最长 30 分钟)。
	// 现写错即发 complete(Success=false) 让 Relay 端立即收尾(而非悬挂)。
	frame := proto.BuildBinaryFrame(transferID, true, nil)
	writeErr := wc.writeBinaryData(ctx, frame)
	if writeErr != nil {
		completeCb(proto.FileTransferComplete{TransferID: transferID, Success: false, Error: "isLast 帧写失败: " + writeErr.Error(), Bytes: sess.bytesDone})
		return
	}

	hash := hex.EncodeToString(h.Sum(nil))
	te.mu.Lock()
	delete(te.sessions, transferID)
	te.mu.Unlock()
	completeCb(proto.FileTransferComplete{TransferID: transferID, Success: true, SHA256Actual: hash, Bytes: sess.bytesDone})
	te.downloadBytes.Add(sess.bytesDone) // Q4:下载字节计数
	te.downloadCount.Add(1)              // Q4:下载次数计数
	log.Printf("[xfer] 下载完成 %s: %d bytes sha256=%s", transferID, sess.bytesDone, hash[:16])
}

// Cancel 取消传输。
// P2 修复:upload 会话取消时删除 .part 文件,防磁盘垃圾 + 防同 ID 重试基于废数据续传。
func (te *TransferEngine) Cancel(transferID string) {
	te.mu.Lock()
	sess, ok := te.sessions[transferID]
	if ok {
		delete(te.sessions, transferID)
		if sess.path != "" {
			delete(te.pathInUse, sess.path) // R1:释放 path 占用
		}
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
	te.pathInUse = make(map[string]struct{}) // R1:清空占用标记
	te.mu.Unlock()
	if len(ids) > 0 {
		log.Printf("[xfer] 关闭 %d 个传输会话", len(ids))
	}
}

// SweepOrphanPartsLoop 周期扫描 workspace,删除孤儿分片(S3:防磁盘泄漏)。
// CLI 分片上传崩溃/网络断时,.ftpartNN 残留无清理,长期堆积占满磁盘。
// 策略:mtime > 2 小时 且 path 不在 pathInUse 中(非活跃传输)的 *.ftpart* 删除。
func (te *TransferEngine) SweepOrphanPartsLoop(ctx context.Context) {
	interval := 30 * time.Minute
	threshold := 2 * time.Hour
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			te.sweepOrphanParts(threshold)
		}
	}
}

// sweepOrphanParts 执行一次扫描清理(可独立测试)。
func (te *TransferEngine) sweepOrphanParts(threshold time.Duration) {
	ws := te.workspace
	if ws == "" {
		return
	}
	entries, err := os.ReadDir(ws)
	if err != nil {
		return
	}
	// 收集活跃 path(排除这些 path 对应的分片)
	te.mu.Lock()
	activePaths := make(map[string]struct{}, len(te.pathInUse))
	for p := range te.pathInUse {
		activePaths[p] = struct{}{}
	}
	te.mu.Unlock()

	cutoff := time.Now().Add(-threshold)
	removed := 0
	for _, e := range entries {
		name := e.Name()
		// 只关心分片文件(.ftpartNN / .ftpartNN.part / .part)
		if !strings.Contains(name, ".ftpart") && !strings.HasSuffix(name, ".part") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(cutoff) {
			continue // 未过期
		}
		// 排除活跃传输的 path(分片名是 path 的前缀)
		fullPath := filepath.Join(ws, name)
		busy := false
		for ap := range activePaths {
			if strings.HasPrefix(name, filepath.Base(ap)) {
				busy = true
				break
			}
		}
		if busy {
			continue
		}
		if err := os.Remove(fullPath); err == nil {
			removed++
		}
	}
	if removed > 0 {
		log.Printf("[xfer] 孤儿分片清理: 删除 %d 个(阈值 %s)", removed, threshold)
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
