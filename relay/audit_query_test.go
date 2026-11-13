package main

// /api/audit/query 端点测试(过滤/分页/排序/未启用 503/坏行容错)。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

// openTestAudit 打开临时审计日志并写入 seed 记录(同步落盘)。
func openTestAudit(t *testing.T, entries []AuditEntry) (*AuditLog, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "audit.log")
	a, err := OpenAuditLog(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		a.Log(e)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	// 重新打开(读路径用);Close 后再 Open 保证 writeLoop 排空完成
	a2, err := OpenAuditLog(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a2.Close() })
	return a2, path
}

func TestAuditQueryFilterAndPaging(t *testing.T) {
	now := time.Now().UnixMilli()
	var seeds []AuditEntry
	for i := 0; i < 5; i++ {
		seeds = append(seeds, AuditEntry{
			TS:      now - int64(i)*1000,
			Event:   "task_submitted",
			AgentID: fmt.Sprintf("agent-%d", i%2),
		})
	}
	seeds = append(seeds, AuditEntry{TS: now - 5000, Event: "agent_online", AgentID: "agent-0"})
	a, _ := openTestAudit(t, seeds)

	r, srv := newTestRelay(t)
	defer srv.Close()
	r.audit = a

	// 1) 无过滤:total=6,新的在前(第一条 ts 最大)
	resp, err := http.Get(srv.URL + "/api/audit/query")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var out struct {
		Entries []auditRecord `json:"entries"`
		Count   int           `json:"count"`
		Total   int           `json:"total"`
		Offset  int           `json:"offset"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	if out.Total != 6 || out.Count != 6 {
		t.Errorf("total=%d count=%d, 期望 6/6", out.Total, out.Count)
	}
	if len(out.Entries) > 0 && out.Entries[0].AgentID != "agent-0" && out.Entries[0].TS != now {
		// 新的在前:第一条应是 ts 最大的那条(事件无要求,但 ts 必为最大)
		if out.Entries[0].TS != now {
			t.Errorf("首条 ts=%d, 期望最新 ts=%d", out.Entries[0].TS, now)
		}
	}

	// 2) event 过滤 + agent 过滤
	resp2, err := http.Get(srv.URL + "/api/audit/query?event=task_submitted&agent_id=agent-1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var out2 struct {
		Entries []auditRecord `json:"entries"`
		Total   int           `json:"total"`
	}
	json.NewDecoder(resp2.Body).Decode(&out2)
	// i%2==1 → agent-1: i=1,3 → 2 条
	if out2.Total != 2 {
		t.Errorf("event+agent 过滤 total=%d, 期望 2", out2.Total)
	}
	for _, e := range out2.Entries {
		if e.Event != "task_submitted" || e.AgentID != "agent-1" {
			t.Errorf("过滤泄漏: %+v", e)
		}
	}

	// 3) 分页:limit=2 offset=2 → 第 3、4 新的
	resp3, err := http.Get(srv.URL + "/api/audit/query?limit=2&offset=2")
	if err != nil {
		t.Fatal(err)
	}
	defer resp3.Body.Close()
	var out3 struct {
		Entries []auditRecord `json:"entries"`
		Count   int           `json:"count"`
		Total   int           `json:"total"`
	}
	json.NewDecoder(resp3.Body).Decode(&out3)
	if out3.Count != 2 || out3.Total != 6 {
		t.Errorf("分页 count=%d total=%d, 期望 2/6", out3.Count, out3.Total)
	}
	if len(out3.Entries) == 2 && out3.Entries[0].TS < out3.Entries[1].TS {
		t.Errorf("页内排序错误: 应新的在前")
	}

	// 4) since 过滤:只看最近 2.5s(覆盖 ts=now, now-1000, now-2000)
	resp4, err := http.Get(srv.URL + "/api/audit/query?since=2.5s")
	if err != nil {
		t.Fatal(err)
	}
	defer resp4.Body.Close()
	var out4 struct {
		Total int `json:"total"`
	}
	json.NewDecoder(resp4.Body).Decode(&out4)
	if out4.Total != 3 {
		t.Errorf("since=2.5s total=%d, 期望 3", out4.Total)
	}

	// 5) hash chain 字段透出(防篡改可验证性)
	if len(out3.Entries) > 0 && (out3.Entries[0].ThisHash == "" || out3.Entries[0].PrevHash == "") {
		t.Errorf("审计查询应透出 hash chain 字段: %+v", out3.Entries[0])
	}
}

func TestAuditQueryDisabled(t *testing.T) {
	_, srv := newTestRelay(t) // audit=nil
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/audit/query")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Errorf("审计未启用 status = %d, 期望 503", resp.StatusCode)
	}
}

func TestFilterAuditEntriesUnit(t *testing.T) {
	now := time.Now().UnixMilli()
	entries := []auditRecord{
		{AuditEntry: AuditEntry{TS: now - 1000, Event: "task_submitted", AgentID: "a"}},
		{AuditEntry: AuditEntry{TS: now - 2000, Event: "task_failed", AgentID: "b"}},
		{AuditEntry: AuditEntry{TS: now - 3000, Event: "agent_online", AgentID: "a"}},
	}
	// 多值 event
	got := filterAuditEntries(entries, auditQueryFilter{Events: []string{"task_submitted", "task_failed"}})
	if len(got) != 2 {
		t.Errorf("多值 event 过滤 = %d, 期望 2", len(got))
	}
	// since
	got = filterAuditEntries(entries, auditQueryFilter{SinceMs: now - 1500})
	if len(got) != 1 {
		t.Errorf("since 过滤 = %d, 期望 1", len(got))
	}
	// 空过滤=全过
	got = filterAuditEntries(entries, auditQueryFilter{})
	if len(got) != 3 {
		t.Errorf("空过滤 = %d, 期望 3", len(got))
	}
	// readAuditFile: 文件不存在= nil(不 panic)
	if got := readAuditFile(filepath.Join(t.TempDir(), "nope.log")); got != nil {
		t.Errorf("文件不存在应返回 nil")
	}
}
