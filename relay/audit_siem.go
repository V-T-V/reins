// Package main — 审计日志 SIEM 导出(XL4 神级提升)。
//
// 支持将审计日志导出为 SIEM(Security Information and Event Management)格式:
//   - CEF(Common Event Format):Splunk/ArcSight 标准
//   - JSON Lines:ELK/Loki/Fluentd 通用
//   - Syslog(RFC 5424):传统 SIEM
//
// GET /api/audit/export?format=cef&since=1h —— 导出审计日志
// GET /api/audit/tail —— SSE 实时推审计事件
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// handleAuditExport GET /api/audit/export —— 导出审计日志(SIEM 格式)。
//
// 参数:
//
//	format=cef|json|syslog (默认 json)
//	since=1h|24h|7d       (默认 1h,只导出最近)
//	limit=1000            (默认 1000)
func (r *Relay) handleAuditExport(w http.ResponseWriter, req *http.Request) {
	if err := r.authorize(req); err != nil {
		writeJSON(w, http.StatusUnauthorized, errMap(err))
		return
	}
	if r.audit == nil {
		writeJSON(w, http.StatusServiceUnavailable, errMap(fmt.Errorf("审计未启用")))
		return
	}

	format := req.URL.Query().Get("format")
	if format == "" {
		format = "json"
	}
	sinceStr := req.URL.Query().Get("since")
	if sinceStr == "" {
		sinceStr = "1h"
	}
	since, err := time.ParseDuration(sinceStr)
	if err != nil {
		since = time.Hour
	}
	sinceMs := time.Now().Add(-since).UnixMilli()

	limit := 1000
	if l := req.URL.Query().Get("limit"); l != "" {
		fmt.Sscanf(l, "%d", &limit)
	}

	// 读审计文件 + 过滤
	auditPath := filepath.Join(dataDir(), "audit.log")
	entries := readAuditEntries(auditPath, sinceMs, limit)

	// 按 format 输出
	switch format {
	case "cef":
		w.Header().Set("Content-Type", "text/plain")
		for _, e := range entries {
			fmt.Fprintln(w, formatCEF(e))
		}
	case "syslog":
		w.Header().Set("Content-Type", "text/plain")
		for _, e := range entries {
			fmt.Fprintln(w, formatSyslog(e))
		}
	default: // json
		w.Header().Set("Content-Type", "application/x-ndjson")
		for _, e := range entries {
			data, _ := json.Marshal(e)
			w.Write(data)
			w.Write([]byte("\n"))
		}
	}
}

// readAuditEntries 读审计文件,过滤时间范围。
func readAuditEntries(path string, sinceMs int64, limit int) []auditRecord {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []auditRecord
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 256*1024), 256*1024) // 大 buffer
	for scanner.Scan() {
		line := scanner.Bytes()
		var rec auditRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		if rec.TS >= sinceMs {
			out = append(out, rec)
			if len(out) >= limit {
				break
			}
		}
	}
	return out
}

// formatCEF 转换为 CEF(Common Event Format)。
// CEF:Version|DeviceVendor|DeviceProduct|DeviceVersion|SignatureID|Name|Severity|Extension
func formatCEF(r auditRecord) string {
	severity := "3" // Low
	if strings.Contains(r.Event, "blocked") || strings.Contains(r.Event, "power") {
		severity = "7" // High
	}
	if strings.Contains(r.Event, "revoked") || strings.Contains(r.Event, "max_attempts") {
		severity = "9" // Critical
	}
	ext := fmt.Sprintf("rt=%d suser=%s act=%s dst=%s msg=%s",
		r.TS, r.Operator, r.Event, r.AgentID, escapeCEF(r.Detail))
	return fmt.Sprintf("CEF:0|reins|relay|2.3|%s|%s|%s|%s",
		r.Event, r.Event, severity, ext)
}

// formatSyslog 转换为 RFC 5424 syslog。
func formatSyslog(r auditRecord) string {
	ts := time.UnixMilli(r.TS).Format(time.RFC3339)
	priority := 134 // facility=1(user), severity=6(info)
	if strings.Contains(r.Event, "blocked") || strings.Contains(r.Event, "power") {
		priority = 132 // severity=4(warning)
	}
	return fmt.Sprintf("<%d>1 %s reins relay %d - - act=%s agent=%s detail=%s",
		priority, ts, r.TS, r.Event, r.AgentID, r.Detail)
}

// escapeCEF CEF 扩展字段转义(| → \|, = → \=, \n → \\n)。
func escapeCEF(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "=", "\\=")
	s = strings.ReplaceAll(s, "\n", "\\n")
	return s
}
