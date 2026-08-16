// Package main — console round 4 API 转发深化测试。
//
// 现有 console_test / main_test 已覆盖「/api/* 被代理」+「静态前端」+「SSE header」。
// 本文件补全 round 4 目标的三个具体 API 链路契约（console → relay 透传不变量）：
//  1. 设备列表：GET /api/agents 透传 + JSON 契约(count/agents)保真。
//  2. 任务下发：POST /api/tasks 透传请求体 + 鉴权 header + 回传 relay 状态码(202)。
//  3. 结果收集：GET /api/tasks/{id} 透传路径参数 + 回传 task/result 契约。
//
// 并覆盖：未匹配路由回静态前端、relay 5xx 透传不吞、Authorization header 透传。
package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// recordingRelay 记录收到的请求，便于断言透传契约。
type recordingRelay struct {
	*httptest.Server
	lastMethod string
	lastPath   string
	lastBody   []byte
	lastAuth   string
}

func newRecordingRelay(status int, respBody string) *recordingRelay {
	r := &recordingRelay{}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.lastMethod = req.Method
		r.lastPath = req.URL.Path
		r.lastAuth = req.Header.Get("Authorization")
		if req.Body != nil {
			r.lastBody, _ = io.ReadAll(req.Body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write([]byte(respBody))
	}))
	return r
}

// --- 1. 设备列表 ---

// TestConsoleDeviceListPassthrough 验证 GET /api/agents 的设备列表 JSON 透传保真。
func TestConsoleDeviceListPassthrough(t *testing.T) {
	relay := newRecordingRelay(200, `{"agents":[{"agent_id":"dev-1","hostname":"web-01","os":"windows"}],"count":1}`)
	defer relay.Close()

	console := httptest.NewServer(buildMux(relay.URL))
	defer console.Close()

	resp, err := http.Get(console.URL + "/api/agents")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if relay.lastMethod != "GET" {
		t.Fatalf("relay 应收到 GET, got %s", relay.lastMethod)
	}
	if relay.lastPath != "/api/agents" {
		t.Fatalf("relay 应收到 /api/agents, got %s", relay.lastPath)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("状态码应透传 200, got %d", resp.StatusCode)
	}
	bodyStr := string(body)
	if !contains(bodyStr, `"agent_id":"dev-1"`) || !contains(bodyStr, `"count":1`) {
		t.Fatalf("设备列表 JSON 应保真透传, got: %s", bodyStr)
	}
	if resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("Content-Type 应透传, got %q", resp.Header.Get("Content-Type"))
	}
}

// --- 2. 任务下发 ---

// TestConsoleTaskDispatchPassthrough 验证 POST /api/tasks 透传请求体 + 鉴权 header + 状态码。
func TestConsoleTaskDispatchPassthrough(t *testing.T) {
	relay := newRecordingRelay(202, `{"task_id":"t-1","status":"dispatched","agent_id":"a-1"}`)
	defer relay.Close()

	console := httptest.NewServer(buildMux(relay.URL))
	defer console.Close()

	taskBody := `{"task_id":"t-1","agent_id":"a-1","type":"powershell","payload":{"command":"echo hi"}}`
	req, _ := http.NewRequest("POST", console.URL+"/api/tasks", bytes.NewReader([]byte(taskBody)))
	req.Header.Set("Authorization", "Bearer secret-token-123")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// relay 应收到完整请求体
	if string(relay.lastBody) != taskBody {
		t.Fatalf("请求体应原样透传, got: %s", string(relay.lastBody))
	}
	// 鉴权 header 应透传（控制台不剥离）
	if relay.lastAuth != "Bearer secret-token-123" {
		t.Fatalf("Authorization 应透传, got %q", relay.lastAuth)
	}
	// 状态码应透传 202
	if resp.StatusCode != 202 {
		t.Fatalf("dispatch 状态码应透传 202, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !contains(string(body), `"status":"dispatched"`) {
		t.Fatalf("dispatch 响应应保真, got: %s", string(body))
	}
}

// TestConsoleTaskDispatchBatchPath 验证批量任务端点路径正确转发。
func TestConsoleTaskDispatchBatchPath(t *testing.T) {
	relay := newRecordingRelay(202, `{"dispatched":["a1"],"count":1}`)
	defer relay.Close()

	console := httptest.NewServer(buildMux(relay.URL))
	defer console.Close()

	req, _ := http.NewRequest("POST", console.URL+"/api/tasks/batch",
		bytes.NewReader([]byte(`{"tag":"env=prod","type":"powershell","payload":{}}`)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if relay.lastPath != "/api/tasks/batch" {
		t.Fatalf("批量任务路径应转发 /api/tasks/batch, got %s", relay.lastPath)
	}
}

// --- 3. 结果收集 ---

// TestConsoleTaskResultPassthrough 验证 GET /api/tasks/{id} 路径参数 + task/result 契约透传。
func TestConsoleTaskResultPassthrough(t *testing.T) {
	relay := newRecordingRelay(200, `{"task":{"task_id":"r-1","status":"completed"},"result":{"stdout":"done"}}`)
	defer relay.Close()

	console := httptest.NewServer(buildMux(relay.URL))
	defer console.Close()

	resp, err := http.Get(console.URL + "/api/tasks/r-1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if relay.lastPath != "/api/tasks/r-1" {
		t.Fatalf("任务 id 应作为路径透传, got %s", relay.lastPath)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("状态码应透传, got %d", resp.StatusCode)
	}
	if !contains(string(body), `"status":"completed"`) || !contains(string(body), `"stdout":"done"`) {
		t.Fatalf("task/result 契约应保真, got: %s", string(body))
	}
}

// --- 4. 错误透传 + 边界 ---

// TestConsoleRelayErrorNotSwallowed 验证 relay 返回 5xx 时控制台原样透传（不吞成 200）。
func TestConsoleRelayErrorNotSwallowed(t *testing.T) {
	relay := newRecordingRelay(503, `{"error":"store unavailable"}`)
	defer relay.Close()

	console := httptest.NewServer(buildMux(relay.URL))
	defer console.Close()

	resp, err := http.Get(console.URL + "/api/tasks")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Fatalf("5xx 应原样透传, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !contains(string(body), "store unavailable") {
		t.Fatalf("错误体应透传, got: %s", string(body))
	}
}

// TestConsoleUnauthorizedPassthrough 验证 401 透传（鉴权失败不本地兜底）。
func TestConsoleUnauthorizedPassthrough(t *testing.T) {
	relay := newRecordingRelay(401, `{"error":"unauthorized"}`)
	defer relay.Close()

	console := httptest.NewServer(buildMux(relay.URL))
	defer console.Close()

	resp, err := http.Get(console.URL + "/api/agents")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("401 应原样透传, got %d", resp.StatusCode)
	}
}

// contains 简化子串断言（避免引入 strings 仅此一处）。
func contains(s, sub string) bool {
	return len(s) >= len(sub) && (indexOf(s, sub) >= 0)
}

// indexOf 朴素子串查找。
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
