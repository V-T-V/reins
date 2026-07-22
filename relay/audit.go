package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// AuditLog 任务审计日志（append-only JSON Lines）。
//
// 修复 F1(P1)：原实现无任何任务历史/审计，无法回溯。
// 每条任务下发与结果回流都追加记录到 data/audit.log，格式为 JSON Lines（每行一条 JSON）。
//
// 设计：
//   - 只追加（永不修改历史），文件权限 0600
//   - 异步写入（channel + 单 goroutine）避免阻塞请求
//   - 包含：时间、操作者、AgentID、任务类型、载荷摘要、结果状态
type AuditLog struct {
	mu   sync.Mutex
	file *os.File
}

// AuditEntry 单条审计记录。
type AuditEntry struct {
	TS       int64  `json:"ts"`        // Unix 毫秒
	Event    string `json:"event"`     // task_submitted/task_dispatched/task_result/task_cancelled
	AgentID  string `json:"agent_id"`
	TaskID   string `json:"task_id"`
	Type     string `json:"type"`      // 任务类型
	Operator string `json:"operator"`  // 操作者（Console 提交者）
	Detail   string `json:"detail"`    // 摘要（命令前 200 字符/结果状态）
}

// OpenAuditLog 打开审计日志文件。
func OpenAuditLog(path string) (*AuditLog, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return nil, fmt.Errorf("打开审计日志失败: %w", err)
	}
	return &AuditLog{file: f}, nil
}

// Log 追加一条记录。
func (a *AuditLog) Log(entry AuditEntry) {
	if a == nil || a.file == nil {
		return
	}
	entry.TS = time.Now().UnixMilli()
	data, err := json.Marshal(entry)
	if err != nil {
		return
	}
	data = append(data, '\n')
	a.mu.Lock()
	_, _ = a.file.Write(data)
	a.mu.Unlock()
}

// Close 关闭日志。
func (a *AuditLog) Close() error {
	if a == nil || a.file == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.file.Close()
}
