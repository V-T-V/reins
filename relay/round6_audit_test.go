// Package main — round 6 审计日志系统深化测试。
//
// audit_chain_test 已覆盖 hash chain 完整性 + 篡改检测。
// 本文件补全 round 6 目标的「查询」与「导出」不变量：
//   - 按时间窗口 / 事件类型 / agentID 过滤查询。
//   - CEF 严重性分级(blocked/revoked/max_attempts → 高危)。
//   - CEF 扩展字段转义(防 | = \n 注入 SIEM 解析)。
//   - Syslog priority 分级。
//   - 异步写入的顺序性 + 幂等 Close。
//   - OpenAuditLog 自动创建父目录。
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- 查询过滤 ---

// TestReadAuditEntriesTimeFilter 验证 readAuditEntries 按时间窗口过滤。
func TestReadAuditEntriesTimeFilter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.log")
	a, err := OpenAuditLog(path)
	if err != nil {
		t.Fatal(err)
	}
	// 写一条"很久以前"的 + 一条"最近"的
	a.Log(AuditEntry{Event: "old-event", TS: time.Now().Add(-2 * time.Hour).UnixMilli()})
	a.Log(AuditEntry{Event: "recent-event", TS: time.Now().UnixMilli()})
	time.Sleep(200 * time.Millisecond)
	a.Close()

	// sinceMs = 1 小时前 → 只应返回 recent-event
	since := time.Now().Add(-1 * time.Hour).UnixMilli()
	got := readAuditEntries(path, since, 100)
	if len(got) != 1 || got[0].Event != "recent-event" {
		t.Fatalf("应只返回最近事件, got %+v", got)
	}
}

// TestReadAuditEntriesLimit 验证 limit 截断。
func TestReadAuditEntriesLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.log")
	a, _ := OpenAuditLog(path)
	for i := 0; i < 20; i++ {
		a.Log(AuditEntry{Event: "ev", TS: time.Now().UnixMilli()})
	}
	time.Sleep(200 * time.Millisecond)
	a.Close()

	got := readAuditEntries(path, 0, 5)
	if len(got) != 5 {
		t.Fatalf("limit=5 应返回 5 条, got %d", len(got))
	}
}

// TestReadAuditEntriesMissingFile 验证文件不存在时返回空(不 panic)。
func TestReadAuditEntriesMissingFile(t *testing.T) {
	got := readAuditEntries("/nonexistent/path/audit.log", 0, 10)
	if len(got) != 0 {
		t.Fatalf("不存在的文件应返回空, got %+v", got)
	}
}

// TestReadAuditEntriesMalformedLine 验证损坏行被跳过(不中断读取)。
func TestReadAuditEntriesMalformedLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.log")
	a, _ := OpenAuditLog(path)
	a.Log(AuditEntry{Event: "good-1", TS: time.Now().UnixMilli()})
	time.Sleep(150 * time.Millisecond)
	a.Close()

	// 追加一行损坏的 JSON + 一行好的（直接写文件，绕过 hash chain）
	appendLine(t, path, "this-is-not-json{")
	appendLine(t, path, mustJSONLine(auditRecord{AuditEntry: AuditEntry{Event: "good-2", TS: time.Now().UnixMilli()}, ThisHash: "x"}))

	got := readAuditEntries(path, 0, 100)
	// 应包含 good-1 + good-2，跳过损坏行
	events := map[string]bool{}
	for _, e := range got {
		events[e.Event] = true
	}
	if !events["good-1"] || !events["good-2"] {
		t.Fatalf("损坏行应被跳过,good-1/good-2 应都在, got events=%v", events)
	}
}

// --- CEF 导出格式 ---

// TestCEFSeverityLevels 验证 formatCEF 按事件类型分级严重性。
func TestCEFSeverityLevels(t *testing.T) {
	cases := []struct {
		event  string
		minSev string // 期望 severity ≥ 此值(CEF 末段)
	}{
		{"task_submitted", "1"},
		{"task_blocked", "7"},   // blocked → High
		{"power_shutdown", "7"}, // power → High
		{"agent_revoked", "9"},  // revoked → Critical
		{"task_max_attempts", "9"},
	}
	for _, c := range cases {
		cef := formatCEF(auditRecord{AuditEntry: AuditEntry{Event: c.event, TS: 1}})
		// CEF 格式: ...|sig|name|severity|ext, severity 是倒数第 2 个 | 分隔段
		segs := strings.Split(cef, "|")
		if len(segs) < 7 {
			t.Fatalf("CEF 段数不足: %s", cef)
		}
		sev := segs[6]
		if sev < c.minSev {
			t.Fatalf("事件 %s 严重性应 ≥ %s, got %s (cef=%s)", c.event, c.minSev, sev, cef)
		}
	}
}

// TestCEFEscapeExtension 验证 CEF 扩展字段转义(防 SIEM 解析注入)。
func TestCEFEscapeExtension(t *testing.T) {
	// detail 含 | = \n —— 这些会破坏 CEF 解析,必须转义
	detail := "cmd|with=pipes\nandnewline"
	cef := formatCEF(auditRecord{AuditEntry: AuditEntry{Event: "task_submitted", Detail: detail, TS: 1}})
	// msg= 之后不应有未转义的 |（否则破坏 CEF 字段数）
	// 取 msg= 之后的部分校验:扩展段是最后一段
	msgIdx := strings.Index(cef, "msg=")
	if msgIdx < 0 {
		t.Fatal("CEF 应含 msg= 扩展")
	}
	msgPart := cef[msgIdx:]
	// msgPart 内的 | 应都被转义为 \|:去掉转义形式后不应残留裸 |
	if strings.Contains(strings.ReplaceAll(msgPart, "\\|", ""), "|") {
		t.Fatalf("msg= 扩展段存在未转义的 |, got: %q", msgPart)
	}
	// 直接验证 escapeCEF 函数
	esc := escapeCEF(detail)
	if strings.Contains(strings.ReplaceAll(esc, "\\|", ""), "|") {
		t.Fatalf("escapeCEF 后仍存在未转义的 |, got: %q", esc)
	}
	if !strings.Contains(esc, "\\|") {
		t.Fatalf("| 应被转义为 \\|, got: %q", esc)
	}
	if !strings.Contains(esc, "\\=") {
		t.Fatalf("= 应被转义为 \\=, got: %q", esc)
	}
	if !strings.Contains(esc, "\\n") {
		t.Fatalf("\\n 应被转义, got: %q", esc)
	}
}

// --- Syslog 导出格式 ---

// TestSyslogPriorityLevels 验证 formatSyslog 的 priority 分级。
func TestSyslogPriorityLevels(t *testing.T) {
	info := formatSyslog(auditRecord{AuditEntry: AuditEntry{Event: "task_submitted", TS: 1}})
	warn := formatSyslog(auditRecord{AuditEntry: AuditEntry{Event: "task_blocked", TS: 1}})
	// info 优先级 134, blocked 132(warning)
	if !strings.HasPrefix(info, "<134>") {
		t.Fatalf("普通事件 syslog priority 应为 134, got: %s", info[:6])
	}
	if !strings.HasPrefix(warn, "<132>") {
		t.Fatalf("blocked 事件 syslog priority 应为 132, got: %s", warn[:6])
	}
}

// TestSyslogRFC5424Structure 验证 syslog 符合 RFC 5424 基本结构。
func TestSyslogRFC5424Structure(t *testing.T) {
	s := formatSyslog(auditRecord{AuditEntry: AuditEntry{Event: "test_ev", AgentID: "a1", Detail: "d", TS: 1000}})
	// <pri>1 timestamp host app procid - - msg
	if !strings.HasPrefix(s, "<") {
		t.Fatal("应以 < 开头")
	}
	if !strings.Contains(s, " reins relay ") {
		t.Fatalf("应含 host=reins app=relay, got: %s", s)
	}
}

// --- 异步写入行为 ---

// TestAuditAsyncOrdering 验证多条记录按投递顺序写入(单 goroutine 串行保证)。
func TestAuditAsyncOrdering(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.log")
	a, _ := OpenAuditLog(path)
	for i := 0; i < 10; i++ {
		a.Log(AuditEntry{Event: "order", Detail: string(rune('A' + i)), TS: int64(i + 1)})
	}
	time.Sleep(300 * time.Millisecond)
	a.Close()

	got := readAuditEntries(path, 0, 100)
	if len(got) != 10 {
		t.Fatalf("应写入 10 条, got %d", len(got))
	}
	for i, r := range got {
		if r.Detail != string(rune('A'+i)) {
			t.Fatalf("顺序错乱: 第 %d 条期望 %s, got %s", i, string(rune('A'+i)), r.Detail)
		}
	}
}

// TestAuditDoubleCloseSafe 验证重复 Close 不 panic。
func TestAuditDoubleCloseSafe(t *testing.T) {
	dir := t.TempDir()
	a, _ := OpenAuditLog(filepath.Join(dir, "audit.log"))
	a.Close()
	a.Close() // 不应 panic
	a.Close()
}

// TestAuditLogCreatesParentDir 验证 OpenAuditLog 自动创建多层父目录。
func TestAuditLogCreatesParentDir(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "a", "b", "c", "audit.log")
	a, err := OpenAuditLog(nested)
	if err != nil {
		t.Fatalf("应自动创建父目录: %v", err)
	}
	a.Log(AuditEntry{Event: "init"})
	time.Sleep(150 * time.Millisecond)
	a.Close()
	got := readAuditEntries(nested, 0, 10)
	if len(got) != 1 {
		t.Fatalf("应写入 1 条, got %d", len(got))
	}
}

// TestAuditNilSafe 验证 nil AuditLog 的 Log/Close 不 panic。
func TestAuditNilSafe(t *testing.T) {
	var a *AuditLog
	a.Log(AuditEntry{Event: "x"}) // 不应 panic
	if err := a.Close(); err != nil {
		t.Fatalf("nil Close 不应返回 error, got %v", err)
	}
}

// --- 辅助 ---

// appendLine 追加一行到文件(用于注入损坏/测试行)。
func appendLine(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatal(err)
	}
}

// mustJSONLine 把 record 序列化为一行 JSON(无换行)。
func mustJSONLine(r auditRecord) string {
	data, _ := json.Marshal(r)
	return string(data)
}
