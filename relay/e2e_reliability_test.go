package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/V-T-V/reins/proto"
)

// TestReaperRedispatchE2E 验证 D2/E2 核心链路：
// 任务超时 → reaper 回退 pending → 可被 ListTasksByStatus 查到供补投。
//
// 这是 at-least-once 语义的关键护栏：dispatched 任务不会永远卡住。
// 补投的 WS 转发逻辑由 TestRelayEndToEnd（main_test.go）+ TestMaxAttempts（reaper_test.go）覆盖。
func TestReaperRedispatchE2E(t *testing.T) {
	dir := t.TempDir()
	s, _ := OpenStore(filepath.Join(dir, "e2e-reaper.db"))
	defer s.Close()

	// 插入一个超时的 dispatched 任务（10 分钟前）
	oldTS := time.Now().Add(-10 * time.Minute).UnixMilli()
	task := proto.TaskRequest{TaskID: "e2e-reaped", Type: proto.TaskPowerShell, AgentID: "e2e-agent", Timeout: 30}
	s.SaveTask(StoredTask{Request: task, Status: StatusDispatched, UpdatedAt: oldTS})

	r := NewRelay(Config{Dev: true})
	r.WithStore(s)

	// 1. reaper 应回退为 pending
	r.reapStaleTasks()
	t1, _ := s.GetTask("e2e-reaped")
	if t1.Status != StatusPending {
		t.Fatalf("reaper 后应为 pending，得到 %s", t1.Status)
	}

	// 2. 验证 reaper 回退后任务可被 ListTasksByStatus(StatusPending) 查到（供 dispatchPendingTasks 补投）
	pending, _ := s.ListTasksByStatus(StatusPending)
	found := false
	for _, p := range pending {
		if p.Request.TaskID == "e2e-reaped" {
			found = true
		}
	}
	if !found {
		t.Fatal("reaper 回退的 pending 任务应可被 ListTasksByStatus 查到")
	}
}

// TestNACKRedispatchE2E 验证 D4/E1 核心链路：
// Agent 发 task_nack → Relay serveAgent → UpdateTaskStatus 回退 pending。
//
// 这是 pool 满场景的护栏：被拒任务能正确回退重投。
func TestNACKRedispatchE2E(t *testing.T) {
	// 用 httptest 起真实 Relay，Agent WS 连入并发 NACK
	dir := t.TempDir()
	s, _ := OpenStore(filepath.Join(dir, "e2e-nack.db"))
	defer s.Close()

	r := NewRelay(Config{Dev: true})
	r.WithStore(s)

	mux := http.NewServeMux()
	mux.HandleFunc("/agent", r.handleAgentWS)
	relaySrv := httptest.NewServer(mux)
	defer relaySrv.Close()

	// 先存一个 dispatched 任务（模拟已投递）
	task := proto.TaskRequest{TaskID: "e2e-nack-task", Type: proto.TaskPowerShell, AgentID: "nack-agent", Timeout: 30}
	s.SaveTask(StoredTask{Request: task, Status: StatusDispatched})

	// Agent 连入，发 register + task_nack
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	agentURL := strings.Replace(relaySrv.URL+"/agent", "http://", "ws://", 1)
	ac, _, err := websocket.Dial(ctx, agentURL, nil) //nolint:bodyclose // coder/websocket:成功时 resp.Body 为 nil,无需关闭
	if err != nil {
		t.Fatalf("Agent 连接失败: %v", err)
	}
	defer ac.CloseNow()

	// register
	reg := proto.Register{AgentID: "nack-agent", Hostname: "test", OS: "test/64", Capabilities: []string{"powershell"}}
	regEnv, _ := proto.NewEnvelope(proto.TypeRegister, "nack-agent", "relay", reg)
	regBytes, _ := json.Marshal(regEnv)
	ac.Write(ctx, websocket.MessageText, regBytes)

	// 发 task_nack
	nack := proto.TaskNack{TaskID: "e2e-nack-task", Reason: "pool_full"}
	nackEnv, _ := proto.NewEnvelope(proto.TypeTaskNack, "nack-agent", "relay", nack)
	nackBytes, _ := json.Marshal(nackEnv)
	ac.Write(ctx, websocket.MessageText, nackBytes)

	// 等待 Relay 处理
	time.Sleep(300 * time.Millisecond)

	// 验证任务已回退 pending
	t2, _ := s.GetTask("e2e-nack-task")
	if t2 == nil {
		t.Fatal("任务应存在")
	}
	if t2.Status != StatusPending {
		t.Fatalf("NACK 后任务应回退 pending，得到 %s", t2.Status)
	}
}

// TestStateMachineVsReaperE2E 验证 D5/E1 核心链路：
// 任务已 completed（终态）→ reaper 尝试回退 → 验证拒绝（终态锁）。
//
// 这是状态机的关键护栏：reaper 不会覆盖已完成任务。
func TestStateMachineVsReaperE2E(t *testing.T) {
	dir := t.TempDir()
	s, _ := OpenStore(filepath.Join(dir, "e2e-sm.db"))
	defer s.Close()

	// 任务先到 completed，UpdatedAt 设为很久以前（模拟 reaper 会扫到）
	oldTS := time.Now().Add(-20 * time.Minute).UnixMilli()
	task := proto.TaskRequest{TaskID: "e2e-terminal", Type: proto.TaskPowerShell, AgentID: "sm-agent", Timeout: 30}
	s.SaveTask(StoredTask{Request: task, Status: StatusCompleted, UpdatedAt: oldTS})

	r := NewRelay(Config{Dev: true})
	r.WithStore(s)

	// reaper 扫描——completed 是终态，不应被回退
	r.reapStaleTasks()

	t2, _ := s.GetTask("e2e-terminal")
	if t2.Status != StatusCompleted {
		t.Fatalf("终态 completed 不应被 reaper 改动，得到 %s", t2.Status)
	}
}
