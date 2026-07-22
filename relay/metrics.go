package main

import (
	"encoding/json"
	"net/http"
	"runtime"
	"sync/atomic"
	"time"
)

// Metrics Relay 运行指标（C10 可观测性）。
//
// 用原子计数器（无锁），通过 GET /api/metrics 暴露。
// 不引入 Prometheus client（保持零额外依赖），输出 JSON 格式便于采集。
type Metrics struct {
	AgentsOnline      int64 // 当前在线 Agent 数
	TasksSubmitted    int64 // 累计提交任务数
	TasksDispatched   int64 // 累计投递任务数
	TasksCompleted    int64 // 累计完成（成功）任务数
	TasksFailed       int64 // 累计失败任务数
	TasksBlocked      int64 // 被策略拦截的任务数
	StartedAt         time.Time
}

var metrics = &Metrics{StartedAt: time.Now()}

// Inc 原子自增计数器。
func (m *Metrics) Inc(field string) {
	switch field {
	case "submitted":
		atomic.AddInt64(&m.TasksSubmitted, 1)
	case "dispatched":
		atomic.AddInt64(&m.TasksDispatched, 1)
	case "completed":
		atomic.AddInt64(&m.TasksCompleted, 1)
	case "failed":
		atomic.AddInt64(&m.TasksFailed, 1)
	case "blocked":
		atomic.AddInt64(&m.TasksBlocked, 1)
	}
}

// SetAgentsOnline 设置在线 Agent 数。
func (m *Metrics) SetAgentsOnline(n int) { atomic.StoreInt64(&m.AgentsOnline, int64(n)) }

// Snapshot 返回指标快照。
func (m *Metrics) Snapshot() map[string]any {
	return map[string]any{
		"agents_online":    atomic.LoadInt64(&m.AgentsOnline),
		"tasks_submitted":  atomic.LoadInt64(&m.TasksSubmitted),
		"tasks_dispatched": atomic.LoadInt64(&m.TasksDispatched),
		"tasks_completed":  atomic.LoadInt64(&m.TasksCompleted),
		"tasks_failed":     atomic.LoadInt64(&m.TasksFailed),
		"tasks_blocked":    atomic.LoadInt64(&m.TasksBlocked),
		"uptime_seconds":   int(time.Since(m.StartedAt).Seconds()),
		"goroutines":       runtime.NumGoroutine(),
	}
}

// handleMetrics GET /api/metrics 暴露运行指标。
func (r *Relay) handleMetrics(w http.ResponseWriter, req *http.Request) {
	// 更新在线数
	metrics.SetAgentsOnline(len(r.agents.Snapshot()))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(metrics.Snapshot())
}
