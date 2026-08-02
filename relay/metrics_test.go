package main

import (
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
