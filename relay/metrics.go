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
// M1 扩展：新增文件传输指标(uploads/downloads/bytes/failures),Relay 作转发中枢天然能统计。
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
	EventsDropped  int64 // SSE 丢弃事件数
	AuditDropped   int64 // 审计丢弃数(audit channel 满)
	taskDurBuckets [10]int64
	taskDurCount   int64
	taskDurSumMs   int64
	// M1 文件传输指标(Relay 转发时统计,Agent 侧也有自己的计数器但无 HTTP server 暴露)
	UploadsTotal     int64 // 累计上传次数(成功)
	DownloadsTotal   int64 // 累计下载次数(含部分成功)
	UploadBytes      int64 // 累计上传字节(Agent 校验通过的)
	DownloadBytes    int64 // 累计下载字节(发给 Console 的)
	UploadFailures   int64 // 上传失败次数(Agent 校验失败/中断)
	DownloadFailures int64 // 下载失败次数(ctx 超时/连接断)
	StartedAt        time.Time
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
	case "audit_dropped":
		atomic.AddInt64(&m.AuditDropped, 1)
	case "reaped":
		atomic.AddInt64(&m.TasksReaped, 1)
	}
}

// SetAgentsOnline 设置在线 Agent 数。
func (m *Metrics) SetAgentsOnline(n int) { atomic.StoreInt64(&m.AgentsOnline, int64(n)) }

// IncXfer 文件传输计数器自增(M1)。field 取值:
// upload_success / download_success / upload_bytes / download_bytes / upload_fail / download_fail
func (m *Metrics) IncXfer(field string, n int64) {
	switch field {
	case "upload_success":
		atomic.AddInt64(&m.UploadsTotal, n)
	case "download_success":
		atomic.AddInt64(&m.DownloadsTotal, n)
	case "upload_bytes":
		atomic.AddInt64(&m.UploadBytes, n)
	case "download_bytes":
		atomic.AddInt64(&m.DownloadBytes, n)
	case "upload_fail":
		atomic.AddInt64(&m.UploadFailures, n)
	case "download_fail":
		atomic.AddInt64(&m.DownloadFailures, n)
	}
}

// Snapshot 返回指标快照。
func (m *Metrics) Snapshot() map[string]any {
	return map[string]any{
		"agents_online":          atomic.LoadInt64(&m.AgentsOnline),
		"tasks_submitted":        atomic.LoadInt64(&m.TasksSubmitted),
		"tasks_dispatched":       atomic.LoadInt64(&m.TasksDispatched),
		"tasks_completed":        atomic.LoadInt64(&m.TasksCompleted),
		"tasks_failed":           atomic.LoadInt64(&m.TasksFailed),
		"tasks_blocked":          atomic.LoadInt64(&m.TasksBlocked),
		"tasks_cancelled":        atomic.LoadInt64(&m.TasksCancelled),
		"tasks_reaped":           atomic.LoadInt64(&m.TasksReaped),
		"events_dropped":         atomic.LoadInt64(&m.EventsDropped),
		"audit_dropped":          atomic.LoadInt64(&m.AuditDropped),
		"xfer_uploads_total":     atomic.LoadInt64(&m.UploadsTotal),
		"xfer_downloads_total":   atomic.LoadInt64(&m.DownloadsTotal),
		"xfer_upload_bytes":      atomic.LoadInt64(&m.UploadBytes),
		"xfer_download_bytes":    atomic.LoadInt64(&m.DownloadBytes),
		"xfer_upload_failures":   atomic.LoadInt64(&m.UploadFailures),
		"xfer_download_failures": atomic.LoadInt64(&m.DownloadFailures),
		"uptime_seconds":         int(time.Since(m.StartedAt).Seconds()),
		"goroutines":             runtime.NumGoroutine(),
	}
}

// ObserveDuration 记录任务耗时(C1:延迟直方图,手搓零依赖)。
func (m *Metrics) ObserveDuration(ms int64) {
	if ms <= 0 {
		return
	}
	atomic.AddInt64(&m.taskDurCount, 1)
	atomic.AddInt64(&m.taskDurSumMs, ms)
	for i, b := range durationBoundaries {
		if ms <= b {
			atomic.AddInt64(&m.taskDurBuckets[i], 1)
		}
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

// handlePromMetrics GET /metrics —— Prometheus 文本格式。
// P1 生产硬化:鉴权策略——
//   - 配了 MetricsToken:校验 Bearer(独立低权 token,供 Prometheus 专用)
//   - 没配 MetricsToken 但有主 Token:校验主 Token(回退兼容)
//   - 都没配:无鉴权(仅适合内网/nginx IP 白名单兜底,生产不推荐)
func (r *Relay) handlePromMetrics(w http.ResponseWriter, req *http.Request) {
	// 鉴权:优先 MetricsToken,回退主 Token
	expectedToken := r.cfg.MetricsToken
	if expectedToken == "" {
		expectedToken = r.cfg.Token // 回退主 Token(兼容)
	}
	if expectedToken != "" {
		got := req.Header.Get("Authorization")
		if got != "Bearer "+expectedToken {
			writeJSON(w, http.StatusUnauthorized, errMap(fmt.Errorf("metrics 端点需要 Bearer token")))
			return
		}
	}
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
	// 注:曾为 gormm_agents_online(两 m),与其他 gormmm_* 前缀不一致,破坏 PromQL 命名空间匹配。
	writeProm("gormmm_agents_online", "当前在线 Agent 数", "gauge", toInt64(snap["agents_online"]))
	writeProm("gormmm_tasks_submitted_total", "累计提交任务数", "counter", toInt64(snap["tasks_submitted"]))
	writeProm("gormmm_tasks_dispatched_total", "累计投递任务数", "counter", toInt64(snap["tasks_dispatched"]))
	writeProm("gormmm_tasks_completed_total", "累计完成任务数", "counter", toInt64(snap["tasks_completed"]))
	writeProm("gormmm_tasks_failed_total", "累计失败任务数", "counter", toInt64(snap["tasks_failed"]))
	writeProm("gormmm_tasks_blocked_total", "被策略拦截的任务数", "counter", toInt64(snap["tasks_blocked"]))
	writeProm("gormmm_tasks_cancelled_total", "被取消的任务数", "counter", toInt64(snap["tasks_cancelled"]))
	writeProm("gormmm_tasks_reaped_total", "被 reaper 回退的任务数", "counter", toInt64(snap["tasks_reaped"]))
	writeProm("gormmm_events_dropped_total", "SSE 丢弃事件数", "counter", toInt64(snap["events_dropped"]))
	// M1 文件传输指标
	writeProm("gormmm_xfer_uploads_total", "累计上传次数(成功)", "counter", toInt64(snap["xfer_uploads_total"]))
	writeProm("gormmm_xfer_downloads_total", "累计下载次数", "counter", toInt64(snap["xfer_downloads_total"]))
	writeProm("gormmm_xfer_upload_bytes_total", "累计上传字节", "counter", toInt64(snap["xfer_upload_bytes"]))
	writeProm("gormmm_xfer_download_bytes_total", "累计下载字节", "counter", toInt64(snap["xfer_download_bytes"]))
	writeProm("gormmm_xfer_upload_failures_total", "上传失败次数", "counter", toInt64(snap["xfer_upload_failures"]))
	writeProm("gormmm_xfer_download_failures_total", "下载失败次数", "counter", toInt64(snap["xfer_download_failures"]))
	writeProm("gormmm_tasks_acked_total", "收到 ACK 数", "counter", toInt64(snap["tasks_acked"]))
	writeProm("gormmm_tasks_nacked_total", "收到 NACK 数", "counter", toInt64(snap["tasks_nacked"]))
	writeProm("gormmm_audit_dropped_total", "审计丢弃数", "counter", toInt64(snap["audit_dropped"]))
	fmt.Fprintf(w, "# HELP gormmm_task_duration_ms 任务执行耗时分布\n# TYPE gormmm_task_duration_ms histogram\n")
	for i, b := range durationBoundaries {
		fmt.Fprintf(w, "gormmm_task_duration_ms_bucket{le=\"%d\"} %d\n", b, atomic.LoadInt64(&metrics.taskDurBuckets[i]))
	}
	fmt.Fprintf(w, "gormmm_task_duration_ms_bucket{le=\"+Inf\"} %d\n", atomic.LoadInt64(&metrics.taskDurCount))
	fmt.Fprintf(w, "gormmm_task_duration_ms_count %d\n", atomic.LoadInt64(&metrics.taskDurCount))
	fmt.Fprintf(w, "gormmm_task_duration_ms_sum %d\n", atomic.LoadInt64(&metrics.taskDurSumMs))
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

// durationBoundaries 任务延迟直方图桶上界(ms)。
var durationBoundaries = []int64{100, 500, 1000, 2000, 5000, 10000, 30000, 60000, 120000}
