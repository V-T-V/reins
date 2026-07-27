package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	_ "net/http/pprof" // S6:pprof 注册到 DefaultServeMux(通过 /debug/pprof/ 暴露)
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

// handleMetrics GET /api/metrics 暴露运行指标(JSON 格式,需鉴权)。
// P1-S7 修复:加鉴权(原无鉴权泄漏在线数/任务计数/goroutine数,利于侦察)。
func (r *Relay) handleMetrics(w http.ResponseWriter, req *http.Request) {
	if err := r.authorize(req); err != nil {
		writeJSON(w, http.StatusUnauthorized, errMap(err))
		return
	}
	// 更新在线数 + events_dropped(D7 从 EventBus 同步)
	metrics.SetAgentsOnline(len(r.agents.Snapshot()))
	metrics.SetEventsDropped(r.console.Dropped())
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(metrics.Snapshot())
}

// handlePromMetrics GET /metrics —— Prometheus 文本格式(无需鉴权,供 Prometheus scraper)。
// S6:零依赖手写 Prometheus exposition 格式(text 0.0.4)。
func (r *Relay) handlePromMetrics(w http.ResponseWriter, req *http.Request) {
	// metrics 端点通常不需鉴权(Prometheus 无法带 Bearer),
	// 但可配置 --metrics-token 保护
	metrics.SetAgentsOnline(len(r.agents.Snapshot()))
	metrics.SetEventsDropped(r.console.Dropped())
	snap := metrics.Snapshot()

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	// 手写 Prometheus exposition 格式
	writeProm := func(name, help, typ string, val int64) {
		fmt.Fprintf(w, "# HELP %s %s\n", name, help)
		fmt.Fprintf(w, "# TYPE %s %s\n", name, typ)
		fmt.Fprintf(w, "%s %d\n", name, val)
	}
	writeProm("gormm_agents_online", "当前在线 Agent 数", "gauge", toInt64(snap["agents_online"]))
	writeProm("gormmm_tasks_submitted_total", "累计提交任务数", "counter", toInt64(snap["tasks_submitted"]))
	writeProm("gormmm_tasks_dispatched_total", "累计投递任务数", "counter", toInt64(snap["tasks_dispatched"]))
	writeProm("gormmm_tasks_completed_total", "累计完成任务数", "counter", toInt64(snap["tasks_completed"]))
	writeProm("gormmm_tasks_failed_total", "累计失败任务数", "counter", toInt64(snap["tasks_failed"]))
	writeProm("gormmm_tasks_blocked_total", "被策略拦截的任务数", "counter", toInt64(snap["tasks_blocked"]))
	writeProm("gormmm_tasks_cancelled_total", "被取消的任务数", "counter", toInt64(snap["tasks_cancelled"]))
	writeProm("gormmm_tasks_reaped_total", "被 reaper 回退的任务数", "counter", toInt64(snap["tasks_reaped"]))
	writeProm("gormmm_events_dropped_total", "SSE 丢弃事件数", "counter", toInt64(snap["events_dropped"]))
	writeProm("gormmm_uptime_seconds", "Relay 运行时长(秒)", "gauge", toInt64(snap["uptime_seconds"]))
	writeProm("gormmm_goroutines", "Goroutine 数", "gauge", toInt64(snap["goroutines"]))
}

// toInt64 安全类型转换(any → int64)。
func toInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	default:
		return 0
	}
}
