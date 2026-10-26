package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TestRenderMetricsFormat 指标输出格式:Prometheus 文本规范(HELP/TYPE 行 +
// reins_ 三 m 命名空间统一),关键指标齐全。
func TestRenderMetricsFormat(t *testing.T) {
	out := renderMetrics()
	required := []string{
		"reins_agent_xfer_upload_bytes_total",
		"reins_agent_xfer_download_bytes_total",
		"reins_agent_xfer_uploads_total",
		"reins_agent_xfer_downloads_total",
		"reins_agent_xfer_failures_total",
		"reins_agent_xfer_sessions_active",
		"reins_agent_reconnects_total",
		"reins_agent_uptime_seconds",
		"reins_agent_build_info",
	}
	for _, name := range required {
		if !strings.Contains(out, name+" ") && !strings.Contains(out, name+"{") {
			t.Fatalf("指标输出缺少 %s:\n%s", name, out)
		}
	}
	// 命名空间:不得残留重命名前的旧前缀(deep-r7 统一过 Relay 侧,Agent 侧同规;v4.8.0 全量退役 gormm*/gormmm*)
	if strings.Contains(out, "gormm_") || strings.Contains(out, "gormmm_") {
		t.Fatal("指标前缀应为 reins_,不得残留旧前缀")
	}
	if !strings.Contains(out, "# HELP reins_agent_reconnects_total") {
		t.Fatal("应含 HELP 行(Prometheus 文本规范)")
	}
}

// TestMetricsHTTPEndpoint 端点层:GET /metrics 返回 200 + 文本格式。
func TestMetricsHTTPEndpoint(t *testing.T) {
	reconnectsTotal.Store(0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(renderMetrics()))
	}))
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("应 200,实际 %d", resp.StatusCode)
	}
	body := make([]byte, 4096)
	n, _ := resp.Body.Read(body)
	if !strings.Contains(string(body[:n]), "reins_agent_reconnects_total 0") {
		t.Fatalf("响应应含 reconnects 指标: %s", body[:n])
	}
}

// TestActiveSessionsSource 活跃会话来源注入(解耦指标模块与引擎锁)。
func TestActiveSessionsSource(t *testing.T) {
	var n atomic.Int64
	n.Store(3)
	SetActiveSessionsSource(func() int { return int(n.Load()) })
	out := renderMetrics()
	if !strings.Contains(out, "reins_agent_xfer_sessions_active 3") {
		t.Fatalf("注入的活跃会话数应出现在输出: %s", out)
	}
}

// TestTransferEngineActiveSessions 引擎 gauge 读数(无会话=0)。
func TestTransferEngineActiveSessions(t *testing.T) {
	if got := transfers.ActiveSessions(); got != 0 {
		t.Fatalf("无传输时活跃会话应为 0,实际 %d", got)
	}
}
