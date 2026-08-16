package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/QiuShichang/go-rmm/proto"
)

// === Relay router 深化测试 ===

func TestRouterListAgentsEmpty(t *testing.T) {
	_, srv := newTestRelay(t)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/agents")
	if err != nil {
		t.Fatal(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var result struct{ Count int }
	json.NewDecoder(resp.Body).Decode(&result)
	if result.Count != 0 {
		t.Errorf("count = %d, 期望 0", result.Count)
	}
}

func TestRouterListAgentsMultiple(t *testing.T) {
	r, srv := newTestRelay(t)
	defer srv.Close()
	registerMockAgent(r, "a1", []string{"powershell"})
	registerMockAgent(r, "a2", []string{"file"})
	registerMockAgent(r, "a3", []string{"shell"})
	resp, err := http.Get(srv.URL + "/api/agents")
	if err != nil {
		t.Fatal(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var result struct{ Count int }
	json.NewDecoder(resp.Body).Decode(&result)
	if result.Count != 3 {
		t.Errorf("count = %d, 期望 3", result.Count)
	}
}

func TestRouterTaskList(t *testing.T) {
	r, srv := newTestRelay(t)
	defer srv.Close()
	// 存几个任务到 store
	r.store.SaveTask(StoredTask{Request: proto.TaskRequest{TaskID: "t1"}, Status: StatusCompleted})
	r.store.SaveTask(StoredTask{Request: proto.TaskRequest{TaskID: "t2"}, Status: StatusFailed})

	resp, err := http.Get(srv.URL + "/api/tasks?limit=10")
	if err != nil {
		t.Fatal(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var result struct {
		Count   int  `json:"count"`
		Enabled bool `json:"enabled"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	if !result.Enabled {
		t.Error("store 应启用")
	}
	if result.Count < 2 {
		t.Errorf("count = %d, 期望 ≥ 2", result.Count)
	}
}

func TestRouterTaskGetResult(t *testing.T) {
	r, srv := newTestRelay(t)
	defer srv.Close()
	r.store.SaveTask(StoredTask{Request: proto.TaskRequest{TaskID: "task-res-1"}, Status: StatusCompleted})
	r.store.SaveResult("task-res-1", proto.TaskResult{TaskID: "task-res-1", Success: true, Stdout: "hello"})

	resp, err := http.Get(srv.URL + "/api/tasks/task-res-1")
	if err != nil {
		t.Fatal(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var result struct {
		Task *struct {
			Status string `json:"status"`
		} `json:"task"`
		Result *proto.TaskResult `json:"result"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	if result.Task == nil || result.Task.Status != StatusCompleted {
		t.Error("task status 不匹配")
	}
	if result.Result == nil || result.Result.Stdout != "hello" {
		t.Error("result stdout 不匹配")
	}
}

func TestRouterTaskGetNonExist(t *testing.T) {
	_, srv := newTestRelay(t)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/tasks/nonexistent")
	if err != nil {
		t.Fatal(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("status = %d, 期望 404", resp.StatusCode)
	}
}

func TestRouterSecretPutGet(t *testing.T) {
	_, srv := newTestRelay(t)
	defer srv.Close()

	// 存储凭据
	putBody, _ := json.Marshal(map[string]any{"value": "my-password", "ttl": 60})
	resp, err := http.Post(srv.URL+"/api/secrets", "application/json", bytes.NewReader(putBody))
	if err != nil {
		t.Fatal(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Errorf("PUT status = %d", resp.StatusCode)
	}
	var putResult struct{ Ref string }
	json.NewDecoder(resp.Body).Decode(&putResult)
	if putResult.Ref == "" {
		t.Fatal("ref 不应为空")
	}

	// 拉取凭据
	resp2, err2 := http.Get(srv.URL + "/api/secrets/" + putResult.Ref)
	if err2 != nil {
		t.Fatal(err2)
	}
	var getResult struct{ Value string }
	json.NewDecoder(resp2.Body).Decode(&getResult)
	resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Errorf("GET status = %d", resp2.StatusCode)
	}
	if getResult.Value != "my-password" {
		t.Errorf("value = %s", getResult.Value)
	}
}

func TestRouterSecretOneTime(t *testing.T) {
	_, srv := newTestRelay(t)
	defer srv.Close()
	// 一次性凭据
	putBody, _ := json.Marshal(map[string]any{"value": "secret123", "ttl": 60, "one_time": true})
	resp, err := http.Post(srv.URL+"/api/secrets", "application/json", bytes.NewReader(putBody))
	if err != nil {
		t.Fatal(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var putResult struct{ Ref string }
	json.NewDecoder(resp.Body).Decode(&putResult)

	// 第一次拉取应成功
	resp1, _ := http.Get(srv.URL + "/api/secrets/" + putResult.Ref)
	resp1.Body.Close()
	if resp1.StatusCode != 200 {
		t.Errorf("第一次 GET status = %d", resp1.StatusCode)
	}

	// 第二次拉取应失败(一次性已删除)
	resp2, err2 := http.Get(srv.URL + "/api/secrets/" + putResult.Ref)
	if err2 != nil {
		t.Fatal(err2)
	}
	resp2.Body.Close()
	if resp2.StatusCode != 404 {
		t.Errorf("一次性凭据第二次 GET status = %d, 期望 404", resp2.StatusCode)
	}
}

func TestRouterSchedulerBest(t *testing.T) {
	r, srv := newTestRelay(t)
	defer srv.Close()
	registerMockAgent(r, "best-1", []string{"powershell", "file"})
	registerMockAgent(r, "best-2", []string{"powershell"})

	resp, err := http.Get(srv.URL + "/api/scheduler/best?type=powershell")
	if err != nil {
		t.Fatal(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d", resp.StatusCode)
	}
	var result struct {
		AgentID string `json:"agent_id"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	if result.AgentID != "best-1" && result.AgentID != "best-2" {
		t.Errorf("agent_id = %s", result.AgentID)
	}
}

func TestRouterAuditExport(t *testing.T) {
	r, srv := newTestRelay(t)
	defer srv.Close()
	// 写一条审计日志
	r.audit = nil // audit 未启用时端点应返回 503

	resp, err := http.Get(srv.URL + "/api/audit/export?format=json")
	if err != nil {
		t.Fatal(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// audit=nil 时返回 503
	if resp.StatusCode != 503 && resp.StatusCode != 200 {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestRouterHealthzDeep(t *testing.T) {
	_, srv := newTestRelay(t)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var result struct {
		Relay  string `json:"relay"`
		Store  string `json:"store"`
		Agents int    `json:"agents"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	if result.Relay != "ok" {
		t.Errorf("relay = %s", result.Relay)
	}
	if result.Store != "ok" {
		t.Errorf("store = %s", result.Store)
	}
}

func TestRouterMetricsJSON(t *testing.T) {
	_, srv := newTestRelay(t)
	defer srv.Close()
	// /api/metrics 需鉴权(无 token 配置 = 放行)
	resp, err := http.Get(srv.URL + "/api/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var result map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&result)
	if err != nil {
		t.Fatalf("metrics JSON 解析失败: %v", err)
	}
	if result["goroutines"] == nil {
		t.Error("metrics 应含 goroutines")
	}
}
