package main

import (
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestMetricsIncXfer 验证 M1 文件传输计数器自增。
func TestMetricsIncXfer(t *testing.T) {
	m := &Metrics{StartedAt: time.Now()}

	m.IncXfer("upload_success", 1)
	m.IncXfer("upload_success", 1)
	m.IncXfer("upload_bytes", 10485760)
	m.IncXfer("upload_fail", 1)

	m.IncXfer("download_success", 1)
	m.IncXfer("download_bytes", 5242880)
	m.IncXfer("download_fail", 2)

	snap := m.Snapshot()

	if snap["xfer_uploads_total"] != int64(2) {
		t.Errorf("uploads_total = %v, want 2", snap["xfer_uploads_total"])
	}
	if snap["xfer_upload_bytes"] != int64(10485760) {
		t.Errorf("upload_bytes = %v, want 10485760", snap["xfer_upload_bytes"])
	}
	if snap["xfer_upload_failures"] != int64(1) {
		t.Errorf("upload_failures = %v, want 1", snap["xfer_upload_failures"])
	}
	if snap["xfer_downloads_total"] != int64(1) {
		t.Errorf("downloads_total = %v, want 1", snap["xfer_downloads_total"])
	}
	if snap["xfer_download_bytes"] != int64(5242880) {
		t.Errorf("download_bytes = %v, want 5242880", snap["xfer_download_bytes"])
	}
	if snap["xfer_download_failures"] != int64(2) {
		t.Errorf("download_failures = %v, want 2", snap["xfer_download_failures"])
	}
}

// TestMetricsSnapshotContainsXferKeys 验证 Snapshot 包含所有 xfer key。
func TestMetricsSnapshotContainsXferKeys(t *testing.T) {
	m := &Metrics{StartedAt: time.Now()}
	snap := m.Snapshot()

	requiredKeys := []string{
		"xfer_uploads_total", "xfer_downloads_total",
		"xfer_upload_bytes", "xfer_download_bytes",
		"xfer_upload_failures", "xfer_download_failures",
	}
	for _, k := range requiredKeys {
		if _, ok := snap[k]; !ok {
			t.Errorf("Snapshot 缺少 key: %s", k)
		}
	}
}

// TestPromMetricsIncludesXfer 验证 Prometheus 格式输出包含 xfer 指标。
func TestPromMetricsIncludesXfer(t *testing.T) {
	metrics = &Metrics{StartedAt: time.Now()}
	metrics.IncXfer("upload_bytes", 1024)

	// 直接测 prom 输出逻辑(metrics 数据 → prom 文本)
	snap := metrics.Snapshot()
	promText := buildPromForTest(snap)

	requiredProm := []string{
		"gormmm_xfer_uploads_total",
		"gormmm_xfer_download_bytes_total",
		"gormmm_xfer_upload_failures_total",
		"# HELP gormmm_xfer_upload_bytes_total",
		"# TYPE gormmm_xfer_upload_bytes_total counter",
	}
	for _, p := range requiredProm {
		if !strings.Contains(promText, p) {
			t.Errorf("Prometheus 输出缺少: %s", p)
		}
	}
	// 值验证
	if !strings.Contains(promText, "gormmm_xfer_upload_bytes_total 1024") {
		t.Errorf("Prometheus 输出应包含 upload_bytes=1024, got:\n%s", promText)
	}
}

// TestPromMetricsAuth 验证 P1 生产硬化:有 Token 时 /metrics 拒绝无 token 请求。
func TestPromMetricsAuth(t *testing.T) {
	metrics = &Metrics{StartedAt: time.Now()}
	// stub agents/console 避免 handlePromMetrics 访问 nil
	stubAgents := NewRegistry()
	stubConsole := NewEventBus()

	t.Run("有主Token_无Authorization_拒绝", func(t *testing.T) {
		r := &Relay{cfg: Config{Token: "secret123"}, agents: stubAgents, console: stubConsole}
		req := httptest.NewRequest("GET", "/metrics", nil)
		rec := httptest.NewRecorder()
		r.handlePromMetrics(rec, req)
		if rec.Code != 401 {
			t.Errorf("无 Authorization 应 401, got %d", rec.Code)
		}
	})

	t.Run("有主Token_正确Bearer_通过", func(t *testing.T) {
		r := &Relay{cfg: Config{Token: "secret123"}, agents: stubAgents, console: stubConsole}
		req := httptest.NewRequest("GET", "/metrics", nil)
		req.Header.Set("Authorization", "Bearer secret123")
		rec := httptest.NewRecorder()
		r.handlePromMetrics(rec, req)
		if rec.Code != 200 {
			t.Errorf("正确 Bearer 应 200, got %d", rec.Code)
		}
	})

	t.Run("MetricsToken优先于主Token", func(t *testing.T) {
		r := &Relay{cfg: Config{Token: "main", MetricsToken: "metrics-only"}, agents: stubAgents, console: stubConsole}
		// 主 token 应被拒绝
		req1 := httptest.NewRequest("GET", "/metrics", nil)
		req1.Header.Set("Authorization", "Bearer main")
		rec1 := httptest.NewRecorder()
		r.handlePromMetrics(rec1, req1)
		if rec1.Code != 401 {
			t.Errorf("MetricsToken 配置时主 Token 应被拒, got %d", rec1.Code)
		}
		// metrics token 应通过
		req2 := httptest.NewRequest("GET", "/metrics", nil)
		req2.Header.Set("Authorization", "Bearer metrics-only")
		rec2 := httptest.NewRecorder()
		r.handlePromMetrics(rec2, req2)
		if rec2.Code != 200 {
			t.Errorf("MetricsToken 应通过, got %d", rec2.Code)
		}
	})

	t.Run("都无Token_放行(仅内网)", func(t *testing.T) {
		r := &Relay{cfg: Config{}, agents: stubAgents, console: stubConsole}
		req := httptest.NewRequest("GET", "/metrics", nil)
		rec := httptest.NewRecorder()
		r.handlePromMetrics(rec, req)
		if rec.Code != 200 {
			t.Errorf("都无 Token 时应放行(靠 nginx IP 白名单兜底), got %d", rec.Code)
		}
	})
}

// TestMetricsConcurrentInc 验证并发自增安全(无 data race)。
func TestMetricsConcurrentInc(t *testing.T) {
	m := &Metrics{StartedAt: time.Now()}
	done := make(chan struct{})
	// 100 个 goroutine 各自增 100 次
	for i := 0; i < 100; i++ {
		go func() {
			for j := 0; j < 100; j++ {
				m.IncXfer("upload_success", 1)
				m.IncXfer("upload_bytes", 1)
			}
			done <- struct{}{}
		}()
	}
	for i := 0; i < 100; i++ {
		<-done
	}
	snap := m.Snapshot()
	// 100×100 = 10000
	if snap["xfer_uploads_total"] != int64(10000) {
		t.Errorf("并发自增后 uploads_total = %v, want 10000", snap["xfer_uploads_total"])
	}
	if snap["xfer_upload_bytes"] != int64(10000) {
		t.Errorf("并发自增后 upload_bytes = %v, want 10000", snap["xfer_upload_bytes"])
	}
}

// TestPromMetricsNamespaceConsistency 验证所有 Prom 指标名都用统一 gormmm_ 前缀。
// 回归:agents_online 曾误写为 gormm_(两 m),破坏 PromQL 命名空间匹配。
func TestPromMetricsNamespaceConsistency(t *testing.T) {
	metrics = &Metrics{StartedAt: time.Now()}
	metrics.SetAgentsOnline(3)
	r := &Relay{cfg: Config{}, agents: NewRegistry(), console: NewEventBus()}
	req := httptest.NewRequest("GET", "/metrics", nil)
	rec := httptest.NewRecorder()
	r.handlePromMetrics(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status=%d", rec.Code)
	}
	body := rec.Body.String()

	// 1. agents_online 必须以 gormmm_ 暴露(三 m)
	if !strings.Contains(body, "gormmm_agents_online") {
		t.Errorf("输出应含 gormmm_agents_online(三 m 前缀):\n%s", body)
	}
	// 2. 任何指标名不得使用 gormm_(两 m,后非第三个 m)
	//    匹配 "gormm_" 但非 "gormmm_" 的行
	for _, line := range strings.Split(body, "\n") {
		// 跳过注释行(# HELP / # TYPE)
		if strings.HasPrefix(line, "#") {
			continue
		}
		if strings.Contains(line, "gormm_") && !strings.Contains(line, "gormmm_") {
			t.Errorf("指标使用了不一致的前缀 gormm_(两 m): %q", line)
		}
	}
	// 3. HELP/TYPE 注释也应一致
	helpLines := strings.Count(body, "# HELP gormmm_")
	if helpLines < 10 {
		t.Errorf("gormmm_ HELP 行数=%d 期望 >=10", helpLines)
	}
}

// buildPromForTest 复刻 handlePromMetrics 的 xfer 输出逻辑(避免依赖 Relay 实例)。
func buildPromForTest(snap map[string]any) string {
	var sb strings.Builder
	writeProm := func(name, help, typ string, val int64) {
		sb.WriteString("# HELP " + name + " " + help + "\n")
		sb.WriteString("# TYPE " + name + " " + typ + "\n")
		sb.WriteString(name + " " + strconv.FormatInt(val, 10) + "\n")
	}
	writeProm("gormmm_xfer_uploads_total", "累计上传次数(成功)", "counter", toInt64(snap["xfer_uploads_total"]))
	writeProm("gormmm_xfer_downloads_total", "累计下载次数", "counter", toInt64(snap["xfer_downloads_total"]))
	writeProm("gormmm_xfer_upload_bytes_total", "累计上传字节", "counter", toInt64(snap["xfer_upload_bytes"]))
	writeProm("gormmm_xfer_download_bytes_total", "累计下载字节", "counter", toInt64(snap["xfer_download_bytes"]))
	writeProm("gormmm_xfer_upload_failures_total", "上传失败次数", "counter", toInt64(snap["xfer_upload_failures"]))
	writeProm("gormmm_xfer_download_failures_total", "下载失败次数", "counter", toInt64(snap["xfer_download_failures"]))
	return sb.String()
}
