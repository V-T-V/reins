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
// D7 扩展：新增 events_dropped / tasks_cancelled / tasks_acked / tasks_nacked / tasks_reaped。
// 用原子计数器（无锁），通过 GET /api/metrics 暴露。
type Metrics struct {
	AgentsOnline    int64 // 当前在线 Agent 数
	TasksSubmitted  int64 // 累计提交任务数
	TasksDispatched int64 // 累计投递任务数
	TasksCompleted  int64 // 累计完成（成功）任务数
	TasksFailed     int64 // 累计失败任务数
	TasksBlocked    int64 // 被策略拦截的任务数
	// D7 新增
	TasksCancelled int64 // 被取消的任务数
	TasksAcked     int64 // 收到 ACK 数
	TasksNacked    int64 // 收到 NACK 数
	TasksReaped    int64 // 被 reaper 回退的任务数
	EventsDropped  int64 // SSE 丢弃事件数（慢消费者）
	StartedAt      time.Time
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
	case "cancelled":
		atomic.AddInt64(&m.TasksCancelled, 1)
	case "acked":
		atomic.AddInt64(&m.TasksAcked, 1)
	case "nacked":
		atomic.AddInt64(&m.TasksNacked, 1)
	case "reaped":
		atomic.AddInt64(&m.TasksReaped, 1)
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
		"tasks_cancelled":  atomic.LoadInt64(&m.TasksCancelled),
		"tasks_acked":      atomic.LoadInt64(&m.TasksAcked),
		"tasks_nacked":     atomic.LoadInt64(&m.TasksNacked),
		"tasks_reaped":     atomic.LoadInt64(&m.TasksReaped),
		"events_dropped":   atomic.LoadInt64(&m.EventsDropped),
		"uptime_seconds":   int(time.Since(m.StartedAt).Seconds()),
		"goroutines":       runtime.NumGoroutine(),
	}
}

// SetEventsDropped 从 EventBus 同步丢弃计数（周期调用）。
func (m *Metrics) SetEventsDropped(n int64) {
	atomic.StoreInt64(&m.EventsDropped, n)
}

// handleMetrics GET /api/metrics 暴露运行指标。
func (r *Relay) handleMetrics(w http.ResponseWriter, req *http.Request) {
	// 更新在线数 + events_dropped（D7 从 EventBus 同步）
	metrics.SetAgentsOnline(len(r.agents.Snapshot()))
	metrics.SetEventsDropped(r.console.Dropped())
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(metrics.Snapshot())
}
