// Package main — Agent 本地 Prometheus 指标端点(P2-10)。
//
// 默认监听 127.0.0.1:7781(仅本机,不暴露公网;远程采集经 tunnel 转发)。
// config metrics_addr 可改地址,设 "off" 禁用。
// 指标命名与其他端统一 reins_*(三 m)命名空间。
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sync/atomic"
	"time"
)

// 默认指标监听地址(仅本机回环)。
const defaultMetricsAddr = "127.0.0.1:7781"

// reconnectsTotal 重连尝试计数(含首次连接失败的重试)。
var reconnectsTotal atomic.Int64

// agentStartAt Agent 进程启动时刻(uptime 指标)。
var agentStartAt = time.Now()

// activeSessionsFn 由 TransferEngine 注入的活跃传输会话数读取函数
// (避免 metrics 模块直接依赖 transferEngine 内部锁)。
var activeSessionsFn atomic.Pointer[func() int]

// SetActiveSessionsSource 注入活跃会话计数来源。
func SetActiveSessionsSource(f func() int) {
	activeSessionsFn.Store(&f)
}

// startMetricsServer 启动本地指标 HTTP 服务,随 ctx 退出。
func startMetricsServer(ctx context.Context, addr string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = fmt.Fprint(w, renderMetrics())
	})
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		// 指标端点失败不阻塞主功能(端口占用等),仅记日志。
		log.Printf("[agent] 指标端点 %s 启动失败: %v", addr, err)
	}
}

// renderMetrics 输出 Prometheus 文本格式(独立函数便于测试)。
func renderMetrics() string {
	ub, db, uc, dc, fc := transfers.SnapshotStats()
	active := 0
	if f := activeSessionsFn.Load(); f != nil {
		active = (*f)()
	}
	help := func(name, help string) string {
		return fmt.Sprintf("# HELP %s %s\n# TYPE %s counter\n", name, help, name)
	}
	out := ""
	out += help("reins_agent_xfer_upload_bytes_total", "累计上传字节数")
	out += fmt.Sprintf("reins_agent_xfer_upload_bytes_total %d\n", ub)
	out += help("reins_agent_xfer_download_bytes_total", "累计下载字节数")
	out += fmt.Sprintf("reins_agent_xfer_download_bytes_total %d\n", db)
	out += help("reins_agent_xfer_uploads_total", "累计上传完成次数")
	out += fmt.Sprintf("reins_agent_xfer_uploads_total %d\n", uc)
	out += help("reins_agent_xfer_downloads_total", "累计下载完成次数")
	out += fmt.Sprintf("reins_agent_xfer_downloads_total %d\n", dc)
	out += help("reins_agent_xfer_failures_total", "累计传输失败次数")
	out += fmt.Sprintf("reins_agent_xfer_failures_total %d\n", fc)
	out += "# HELP reins_agent_xfer_sessions_active 活跃传输会话数\n# TYPE reins_agent_xfer_sessions_active gauge\n"
	out += fmt.Sprintf("reins_agent_xfer_sessions_active %d\n", active)
	out += "# HELP reins_agent_reconnects_total 到 Relay 的重连尝试次数(含首次连接失败)\n# TYPE reins_agent_reconnects_total counter\n"
	out += fmt.Sprintf("reins_agent_reconnects_total %d\n", reconnectsTotal.Load())
	out += "# HELP reins_agent_uptime_seconds 进程运行秒数\n# TYPE reins_agent_uptime_seconds gauge\n"
	out += fmt.Sprintf("reins_agent_uptime_seconds %d\n", int(time.Since(agentStartAt).Seconds()))
	out += fmt.Sprintf("# HELP reins_agent_build_info 构建信息\n# TYPE reins_agent_build_info gauge\nreins_agent_build_info{version=%q} 1\n", version)
	return out
}
