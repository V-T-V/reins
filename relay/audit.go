// Package main — 任务审计日志(append-only JSON Lines + hash chain 防篡改)。
//
// M2 神级提升:
//   - hash chain: 每条记录含 prev_hash,链式防篡改(本地 root 也无法不露痕迹地修改)
//   - 真异步化: channel + 单 goroutine 写入,绝不阻塞请求 goroutine(原实现是同步 mu.Lock+Write)
//   - 日志轮转: 按天轮转(可选,后续加)
//
// hash chain 格式:
//
//	{"ts":...,"event":...,"prev_hash":"abc123...","this_hash":"def456..."}
//	this_hash = sha256(prev_hash + json(record_without_hashes))
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// AuditLog 任务审计日志(append-only + hash chain + 异步写入)。
type AuditLog struct {
	mu        sync.Mutex // 保护 prevHash 与 hash/写缓冲(性能轮 2026-11-14:整条原子)
	file      *os.File
	prevHash  string        // 上一条记录的 hash(链式)
	hash      hash.Hash     // 复用 sha256 实例(writeEntry 持锁使用)
	lineBuf   bytes.Buffer  // 复用记录行编码缓冲(含尾 \n)
	lineEnc   *json.Encoder // 绑定 lineBuf 的持久 Encoder(免每条重建)
	ch        chan AuditEntry
	closeCh   chan struct{}
	closeOnce sync.Once // 幂等 close(防重复 close panic,round 6 修复)
	closed    bool
}

// AuditEntry 单条审计记录(不含 hash,hash 在写入时计算)。
//
// 2026 租户加固:Tenant/Operator 新增字段均 omitempty——旧记录无值时
// re-marshal 逐字节不变,VerifyChain 的 hash 重算对历史链保持兼容。
// Operator 格式 "tenant/role"(admin 域为 "/admin"→ 实际记 "admin")。
type AuditEntry struct {
	TS       int64  `json:"ts"`
	Event    string `json:"event"`
	AgentID  string `json:"agent_id,omitempty"`
	TaskID   string `json:"task_id,omitempty"`
	Type     string `json:"type,omitempty"`
	Operator string `json:"operator,omitempty"`
	Detail   string `json:"detail,omitempty"`
	Tenant   string `json:"tenant,omitempty"` // 2026 租户加固:操作者凭据绑定的租户(查询/导出按此过滤)
}

// auditRecord 写入磁盘的完整记录(含 hash chain)。
type auditRecord struct {
	AuditEntry
	PrevHash string `json:"prev_hash"`
	ThisHash string `json:"this_hash"`
}

// OpenAuditLog 打开审计日志文件,启动异步写入 goroutine。
func OpenAuditLog(path string) (*AuditLog, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return nil, fmt.Errorf("打开审计日志失败: %w", err)
	}
	a := &AuditLog{
		file:     f,
		hash:     sha256.New(),
		ch:       make(chan AuditEntry, 256), // 缓冲 256 条,满则阻塞(背压)
		closeCh:  make(chan struct{}),
		prevHash: seedPrevHash(path),
	}
	a.lineEnc = json.NewEncoder(&a.lineBuf)
	// 异步写入 goroutine(真异步,不阻塞请求)
	go a.writeLoop()
	return a, nil
}

// seedPrevHash 回放审计文件,取最后一条可解析记录的 this_hash 作为续写链的起点。
//
// 2026 修复(既有缺陷):此前 prevHash 从零值起步——文件以追加模式打开却不回放,
// Relay **每次重启后**的首条记录 prev_hash 都是空串,VerifyChain 在该行报「链断裂」。
// 防篡改审计链每次重启自己断一次,等于链校验对重启后的记录失去证明力。
//
// 只读文件尾部(最后一条记录必在尾部);窗口内找不到完整记录(单条超大)才全量读。
// 文件为空/不存在/无可解析记录 → 空串(与创世一致)。尾部撕裂的半条记录被跳过,
// 它本身仍会被 VerifyChain 判为损坏——撕裂是真实的完整性事件,不掩盖;
// 同理,此前已经断掉的旧链也照旧可见,这里只保证从现在起续得上。
func seedPrevHash(path string) string {
	const window = 64 << 10
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.Size() == 0 {
		return ""
	}
	size := fi.Size()
	read := func(off, n int64) []byte {
		buf := make([]byte, n)
		r, _ := f.ReadAt(buf, off)
		return buf[:r]
	}
	data := read(0, size)
	if size > window {
		if tail := read(size-window, window); len(tail) > 0 {
			if i := bytes.IndexByte(tail, '\n'); i >= 0 {
				data = tail[i+1:] // 窗口首行大概率被截断,丢弃到第一个换行之后
			}
			// 窗口内没有换行 = 单条记录超过窗口,保持全量读
		}
	}
	lines := splitLines(data)
	for i := len(lines) - 1; i >= 0; i-- {
		if len(lines[i]) == 0 {
			continue
		}
		var rec auditRecord
		if json.Unmarshal(lines[i], &rec) == nil && rec.ThisHash != "" {
			return rec.ThisHash
		}
	}
	return ""
}

// writeLoop 异步写入循环(单 goroutine 串行写,保证顺序)。
func (a *AuditLog) writeLoop() {
	for {
		select {
		case entry := <-a.ch:
			a.writeEntry(entry)
		case <-a.closeCh:
			// 排空剩余条目后退出
			for len(a.ch) > 0 {
				a.writeEntry(<-a.ch)
			}
			return
		}
	}
}

// Path 返回审计日志文件路径(查询端点读文件用)。
// 未启用(file 为 nil)返回空串。
func (a *AuditLog) Path() string {
	if a == nil || a.file == nil {
		return ""
	}
	return a.file.Name()
}

// writeEntry 写入单条记录(含 hash chain 计算)。
// 性能轮 2026-11-14:
//   - 两段锁合并为整条单锁——原"读 prev→算 hash→写 prev"分两段加锁,
//     理论上两个并发 writeEntry 可读到同一 prev 造成链分叉(当前唯一调用方
//     writeLoop 串行,不触发;单锁后并发调用也不可能在链上分叉)
//   - sha256 实例与行编码缓冲复用(每条 -3 allocs 左右;hash 输入
//     json.Marshal(entry) 与 VerifyChain 逐字节一致,链校验兼容不变)
func (a *AuditLog) writeEntry(entry AuditEntry) {
	if entry.TS == 0 {
		entry.TS = time.Now().UnixMilli()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	prev := a.prevHash

	rec := auditRecord{
		AuditEntry: entry,
		PrevHash:   prev,
	}
	// this_hash = sha256(prev_hash + JSON(record_without_hashes))
	a.hash.Reset()
	a.hash.Write([]byte(prev))
	entryData, _ := json.Marshal(entry)
	a.hash.Write(entryData)
	rec.ThisHash = hex.EncodeToString(a.hash.Sum(nil))

	// 写文件(持久 Encoder 输出与 json.Marshal(rec) 逐字节一致 + 自带 \n;
	// 缓冲复用免"marshal 精确尺寸切片再 append '\n'"的整条重分配)
	a.lineBuf.Reset()
	if err := a.lineEnc.Encode(rec); err != nil {
		return
	}
	data := a.lineBuf.Bytes()
	// D8 加固:审计写盘失败必须显式记日志(原 Write 返回值被丢弃,
	// 磁盘满/权限拒绝时审计记录静默丢失,违背审计可靠性)。
	if n, werr := a.file.Write(data); werr != nil {
		log.Printf("[audit] 写盘失败(可能丢记录 event=%s): %v", entry.Event, werr)
	} else if n < len(data) {
		log.Printf("[audit] 短写(可能丢记录 event=%s): 写入 %d/%d 字节", entry.Event, n, len(data))
	}
	a.prevHash = rec.ThisHash
}

// Log 异步追加一条记录(非阻塞,投入 channel 即返回)。
func (a *AuditLog) Log(entry AuditEntry) {
	if a == nil || a.file == nil || a.closed {
		return
	}
	select {
	case a.ch <- entry:
	default:
		metrics.Inc("audit_dropped")
		log.Printf("[audit] channel 满,丢弃审计记录 event=%s", entry.Event)
	}
}

// Close 优雅关闭(排空 channel 后关闭文件)。
// 幂等:多次调用安全(round 6 修复,防 close 已关闭 channel 的 panic)。
func (a *AuditLog) Close() error {
	if a == nil || a.file == nil {
		return nil
	}
	a.closeOnce.Do(func() {
		a.closed = true
		close(a.closeCh)                   // 触发 writeLoop 排空退出
		time.Sleep(100 * time.Millisecond) // 给 writeLoop 排空时间
	})
	return a.file.Close()
}

// VerifyChain 校验审计日志的 hash chain 完整性(防篡改检测)。
// 返回第一个被篡改的行号(0-based),-1 表示完整无误。
func VerifyChain(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return -1, err
	}
	lines := splitLines(data)
	prevHash := ""
	for i, line := range lines {
		if len(line) == 0 {
			continue
		}
		var rec auditRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			return i, fmt.Errorf("行 %d JSON 解析失败: %w", i, err)
		}
		// 重算 hash
		h := sha256.New()
		h.Write([]byte(rec.PrevHash))
		entryData, _ := json.Marshal(rec.AuditEntry)
		h.Write(entryData)
		expected := hex.EncodeToString(h.Sum(nil))
		if expected != rec.ThisHash {
			return i, fmt.Errorf("行 %d hash 不匹配(篡改或损坏)", i)
		}
		if rec.PrevHash != prevHash {
			return i, fmt.Errorf("行 %d prev_hash 链断裂", i)
		}
		prevHash = rec.ThisHash
	}
	return -1, nil
}

// splitLines 按 \n 分割(保留每行原始字节)。
func splitLines(data []byte) [][]byte {
	var lines [][]byte
	start := 0
	for i, b := range data {
		if b == '\n' {
			lines = append(lines, data[start:i])
			start = i + 1
		}
	}
	if start < len(data) {
		lines = append(lines, data[start:])
	}
	return lines
}
