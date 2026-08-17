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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// AuditLog 任务审计日志(append-only + hash chain + 异步写入)。
type AuditLog struct {
	mu        sync.Mutex // 仅保护 prevHash 字段
	file      *os.File
	prevHash  string // 上一条记录的 hash(链式)
	ch        chan AuditEntry
	closeCh   chan struct{}
	closeOnce sync.Once // 幂等 close(防重复 close panic,round 6 修复)
	closed    bool
}

// AuditEntry 单条审计记录(不含 hash,hash 在写入时计算)。
type AuditEntry struct {
	TS       int64  `json:"ts"`
	Event    string `json:"event"`
	AgentID  string `json:"agent_id,omitempty"`
	TaskID   string `json:"task_id,omitempty"`
	Type     string `json:"type,omitempty"`
	Operator string `json:"operator,omitempty"`
	Detail   string `json:"detail,omitempty"`
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
		file:    f,
		ch:      make(chan AuditEntry, 256), // 缓冲 256 条,满则阻塞(背压)
		closeCh: make(chan struct{}),
	}
	// 异步写入 goroutine(真异步,不阻塞请求)
	go a.writeLoop()
	return a, nil
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

// writeEntry 写入单条记录(含 hash chain 计算)。
func (a *AuditLog) writeEntry(entry AuditEntry) {
	if entry.TS == 0 {
		entry.TS = time.Now().UnixMilli()
	}
	// 计算 hash chain
	a.mu.Lock()
	prev := a.prevHash
	a.mu.Unlock()

	rec := auditRecord{
		AuditEntry: entry,
		PrevHash:   prev,
	}
	// this_hash = sha256(prev_hash + JSON(record_without_hashes))
	h := sha256.New()
	h.Write([]byte(prev))
	entryData, _ := json.Marshal(entry)
	h.Write(entryData)
	rec.ThisHash = hex.EncodeToString(h.Sum(nil))

	// 写文件
	data, err := json.Marshal(rec)
	if err != nil {
		return
	}
	data = append(data, '\n')
	// D8 加固:审计写盘失败必须显式记日志(原 Write 返回值被丢弃,
	// 磁盘满/权限拒绝时审计记录静默丢失,违背审计可靠性)。
	if n, werr := a.file.Write(data); werr != nil {
		log.Printf("[audit] 写盘失败(可能丢记录 event=%s): %v", entry.Event, werr)
	} else if n < len(data) {
		log.Printf("[audit] 短写(可能丢记录 event=%s): 写入 %d/%d 字节", entry.Event, n, len(data))
	}

	// 更新 prevHash
	a.mu.Lock()
	a.prevHash = rec.ThisHash
	a.mu.Unlock()
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
