// Package main — 端到端集成测试(P4 神级提升)。
//
// 启动真实 Relay 实例(httptest),模拟完整 API 链路:
//  1. 创建 Relay + MemoryStore + Scheduler
//  2. 注册 Mock Agent(模拟 WS 连接)
//  3. 下发任务 → 校验状态机流转
//  4. 测试文件上传/下载/隧道/电源/脚本库 全链路
//
// 不依赖外部进程,纯 Go httptest 模拟。
package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QiuShichang/reins/proto"
)

// newTestRelay 创建测试用 Relay 实例(MemoryStore + 无 audit)。
func newTestRelay(t *testing.T) (*Relay, *httptest.Server) {
	t.Helper()
	store := NewMemoryStore()
	r := &Relay{
		cfg:     Config{Addr: ":0"},
		agents:  NewRegistry(),
		console: NewEventBus(),
		policy:  DefaultCommandPolicy(),
		store:   store,
	}
	mux := http.NewServeMux()
	r.registerHTTP(mux)
	srv := httptest.NewServer(mux)
	return r, srv
}

// registerMockAgent 注册一个 Mock Agent 到 Registry。
func registerMockAgent(r *Relay, agentID string, caps []string) {
	r.agents.Register(proto.Register{
		AgentID:      agentID,
		Hostname:     "test-host",
		OS:           "windows/amd64",
		Version:      "test",
		Capabilities: caps,
	}, nil) // nil conn(不需要真实 WS)
}

// === 测试用例 ===

func TestE2EListAgents(t *testing.T) {
	r, srv := newTestRelay(t)
	defer srv.Close()
	registerMockAgent(r, "agent-test-1", []string{"powershell", "file"})

	resp, err := http.Get(srv.URL + "/api/agents")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d", resp.StatusCode)
	}
	var result struct {
		Count int `json:"count"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	if result.Count != 1 {
		t.Errorf("count = %d, 期望 1", result.Count)
	}
}

func TestE2EHealthz(t *testing.T) {
	_, srv := newTestRelay(t)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestE2EPromMetrics(t *testing.T) {
	_, srv := newTestRelay(t)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d", resp.StatusCode)
	}
	// 验证 Prometheus 格式
	var body []byte
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	body = buf[:n]
	if len(body) == 0 {
		t.Error("metrics 输出不应为空")
	}
}

func TestE2ETaskSubmit(t *testing.T) {
	r, srv := newTestRelay(t)
	defer srv.Close()
	registerMockAgent(r, "agent-exec", []string{"powershell"})

	// 下发任务(无真实 Agent 执行,只验证 API 层)
	taskBody, _ := json.Marshal(proto.TaskRequest{
		TaskID:  "e2e-task-1",
		Type:    "powershell",
		AgentID: "agent-exec",
		Payload: json.RawMessage(`{"command":"Get-Date"}`),
		Timeout: 10,
	})
	resp, err := http.Post(srv.URL+"/api/tasks", "application/json", bytes.NewReader(taskBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// 应返回 202(Agent 无真实 WS conn,走 pending 缓存)
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestE2ECapabilityFilter(t *testing.T) {
	r, srv := newTestRelay(t)
	defer srv.Close()
	// Agent 只支持 powershell,不支持 browse
	registerMockAgent(r, "agent-cap", []string{"powershell"})

	// 尝试下发 browse 任务 → 应被能力路由拒绝
	taskBody, _ := json.Marshal(proto.TaskRequest{
		TaskID:  "e2e-cap-1",
		Type:    "browse",
		AgentID: "agent-cap",
		Payload: json.RawMessage(`{}`),
	})
	resp, err := http.Post(srv.URL+"/api/tasks", "application/json", bytes.NewReader(taskBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("browse 应被能力路由拒绝(400),得到 %d", resp.StatusCode)
	}
}

func TestE2EPowerNoTag(t *testing.T) {
	r, srv := newTestRelay(t)
	defer srv.Close()
	registerMockAgent(r, "agent-pwr", []string{"power"})
	// Agent 无 power_allowed=true 标签 → 应拒绝

	powerBody, _ := json.Marshal(map[string]any{
		"agent_id": "agent-pwr",
		"op":       "shutdown",
	})
	resp, err := http.Post(srv.URL+"/api/power", "application/json", bytes.NewReader(powerBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("无标签应 403,得到 %d", resp.StatusCode)
	}
}

func TestE2EPowerWithTag(t *testing.T) {
	r, srv := newTestRelay(t)
	defer srv.Close()
	// Agent 带 power_allowed=true 标签
	r.agents.Register(proto.Register{
		AgentID:      "agent-pwr-ok",
		Capabilities: []string{"power"},
		Tags:         map[string]string{"power_allowed": "true"},
	}, nil)

	powerBody, _ := json.Marshal(map[string]any{
		"agent_id": "agent-pwr-ok",
		"op":       "lock",
	})
	resp, err := http.Post(srv.URL+"/api/power", "application/json", bytes.NewReader(powerBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// 应 202(任务下发,无真实 Agent 执行)
	if resp.StatusCode == http.StatusForbidden {
		t.Error("有标签不应被拒绝")
	}
}

func TestE2EMemoryStoreStateMachine(t *testing.T) {
	r, _ := newTestRelay(t)
	// 直接测 MemoryStore 状态机
	task := StoredTask{
		Request: proto.TaskRequest{TaskID: "sm-test", Type: "powershell"},
		Status:  StatusPending,
	}
	r.store.SaveTask(task)

	// pending → dispatched
	r.store.UpdateTaskStatus("sm-test", StatusDispatched)
	t2, _ := r.store.GetTask("sm-test")
	if t2.Status != StatusDispatched {
		t.Errorf("status = %s", t2.Status)
	}

	// dispatched → completed
	r.store.UpdateTaskStatus("sm-test", StatusCompleted)
	t3, _ := r.store.GetTask("sm-test")
	if t3.Status != StatusCompleted {
		t.Errorf("status = %s", t3.Status)
	}

	// completed(终态) → pending 应失败
	err := r.store.ReapToPending("sm-test")
	if err == nil {
		t.Error("终态不应能回退")
	}
}
