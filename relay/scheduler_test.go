package main

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/QiuShichang/reins/proto"
)

// TestCronRulePersist 验证 H2：cron 规则持久化 + 加载。
func TestCronRulePersist(t *testing.T) {
	dir := t.TempDir()
	s, _ := OpenStore(filepath.Join(dir, "cron.db"))
	defer s.Close()

	rule := CronRule{
		ID:       "cron-test-1",
		CronExpr: "*/5 * * * *",
		TaskType: proto.TaskPowerShell,
		AgentID:  "agent-1",
		Payload:  json.RawMessage(`{"command":"Get-Process"}`),
		Timeout:  30,
		Enabled:  true,
	}

	// 保存
	if err := s.SaveCronRule(rule); err != nil {
		t.Fatalf("SaveCronRule 失败: %v", err)
	}

	// 加载
	rules := s.LoadCronRules()
	if len(rules) != 1 {
		t.Fatalf("应加载 1 条规则，有 %d", len(rules))
	}
	if rules[0].ID != "cron-test-1" || rules[0].CronExpr != "*/5 * * * *" {
		t.Fatalf("规则数据不符: %+v", rules[0])
	}

	// 删除
	if err := s.DeleteCronRule("cron-test-1"); err != nil {
		t.Fatalf("DeleteCronRule 失败: %v", err)
	}
	rules = s.LoadCronRules()
	if len(rules) != 0 {
		t.Fatalf("删除后应为 0 条，有 %d", len(rules))
	}
}

// TestCronRuleSurviveReopen 验证规则跨 db 重开恢复。
func TestCronRuleSurviveReopen(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "cron-reopen.db")

	s1, _ := OpenStore(dbPath)
	s1.SaveCronRule(CronRule{ID: "r1", CronExpr: "0 * * * *", TaskType: "powershell", AgentID: "a1", Enabled: true})
	s1.SaveCronRule(CronRule{ID: "r2", CronExpr: "0 0 * * *", TaskType: "script", AgentID: "a2", Enabled: false})
	s1.Close()

	s2, _ := OpenStore(dbPath)
	defer s2.Close()
	rules := s2.LoadCronRules()
	if len(rules) != 2 {
		t.Fatalf("重开后应有 2 条规则，有 %d", len(rules))
	}
}

// TestSchedulerAddAndRemove 验证调度器增删规则。
func TestSchedulerAddAndRemove(t *testing.T) {
	dir := t.TempDir()
	store, _ := OpenStore(filepath.Join(dir, "sched.db"))
	defer store.Close()
	relay := NewRelay(Config{})
	relay.WithStore(store)

	sched := NewScheduler(store, relay)
	sched.Start()
	defer sched.Stop()

	rule := CronRule{
		ID:       "sched-test",
		CronExpr: "*/1 * * * *",
		TaskType: proto.TaskPowerShell,
		AgentID:  "agent-test",
		Payload:  json.RawMessage(`{"command":"echo hi"}`),
	}
	if err := sched.AddRule(rule); err != nil {
		t.Fatalf("AddRule 失败: %v", err)
	}

	// 验证已调度
	sched.mu.Lock()
	_, scheduled := sched.entryMap["sched-test"]
	sched.mu.Unlock()
	if !scheduled {
		t.Fatal("规则应被调度")
	}

	// 验证已持久化
	rules := store.LoadCronRules()
	if len(rules) != 1 {
		t.Fatalf("应持久化 1 条规则，有 %d", len(rules))
	}

	// 删除
	if err := sched.RemoveRule("sched-test"); err != nil {
		t.Fatalf("RemoveRule 失败: %v", err)
	}
	rules = store.LoadCronRules()
	if len(rules) != 0 {
		t.Fatalf("删除后应为 0 条，有 %d", len(rules))
	}
}
