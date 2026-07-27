package client

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// mockServer 创建模拟 Relay server(返回预设响应)。
func mockServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	return httptest.NewServer(handler)
}

func TestSDKAgentsList(t *testing.T) {
	srv := mockServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agents" {
			t.Errorf("路径 = %s, 期望 /api/agents", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("Authorization = %s", r.Header.Get("Authorization"))
		}
		json.NewEncoder(w).Encode(map[string]any{
			"agents": []map[string]any{
				{"agent_id": "agent-1", "hostname": "host1"},
				{"agent_id": "agent-2", "hostname": "host2"},
			},
			"count": 2,
		})
	})
	defer srv.Close()

	c := New(srv.URL, "test-token")
	agents, err := c.Agents().List()
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(agents) != 2 {
		t.Errorf("agents 数 = %d, 期望 2", len(agents))
	}
	if agents[0].AgentID != "agent-1" {
		t.Errorf("agent_id = %s", agents[0].AgentID)
	}
}

func TestSDKGetStatus(t *testing.T) {
	srv := mockServer(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"relay":    "ok",
			"store":    "ok",
			"agents":   5,
			"uptime_s": 3600,
		})
	})
	defer srv.Close()

	c := New(srv.URL, "")
	status, err := c.GetStatus()
	if err != nil {
		t.Fatalf("GetStatus 失败: %v", err)
	}
	if status.Relay != "ok" {
		t.Errorf("Relay = %s", status.Relay)
	}
	if status.Agents != 5 {
		t.Errorf("Agents = %d", status.Agents)
	}
}

func TestSDKErrorHandling(t *testing.T) {
	srv := mockServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid token"})
	})
	defer srv.Close()

	c := New(srv.URL, "bad-token")
	_, err := c.Agents().List()
	if err == nil {
		t.Error("应返回 error")
	}
}

func TestSDKTaskRun(t *testing.T) {
	callCount := 0
	srv := mockServer(t, func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if r.URL.Path == "/api/tasks" && r.Method == "POST" {
			// 返回任务已接受
			json.NewEncoder(w).Encode(map[string]any{
				"task_id":  "sdk-test",
				"status":   "dispatched",
				"agent_id": "agent-1",
			})
			return
		}
		// GET /api/tasks/{id} 返回终态
		if callCount > 1 {
			json.NewEncoder(w).Encode(map[string]any{
				"task":   map[string]string{"status": "completed"},
				"result": map[string]any{"task_id": "sdk-test", "success": true, "stdout": "hello"},
			})
		}
	})
	defer srv.Close()

	c := New(srv.URL, "")
	result, err := c.Tasks().Run("agent-1", "powershell",
		json.RawMessage(`{"command":"echo hello"}`), 5)
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if result == nil {
		t.Fatal("result 不应为 nil")
	}
	if !result.Success {
		t.Error("Success 应为 true")
	}
}
