package client

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestSDKListTools(t *testing.T) {
	srv := mockServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tools" {
			t.Errorf("path = %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"tools": []map[string]any{
				{"name": "run_task", "method": "POST", "path": "/api/tasks"},
				{"name": "list_agents", "method": "GET", "path": "/api/agents"},
			},
			"count": 2,
		})
	})
	defer srv.Close()

	c := New(srv.URL, "")
	tools, err := c.ListTools()
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 2 {
		t.Errorf("tools count = %d", len(tools))
	}
	if tools[0].Name != "run_task" {
		t.Errorf("tools[0].Name = %s", tools[0].Name)
	}
}

func TestSDKRunCommand(t *testing.T) {
	callCount := 0
	srv := mockServer(t, func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if r.URL.Path == "/api/tasks" && r.Method == "POST" {
			json.NewEncoder(w).Encode(map[string]any{"task_id": "cmd-test", "status": "dispatched"})
			return
		}
		if callCount > 1 {
			json.NewEncoder(w).Encode(map[string]any{
				"task":   map[string]string{"status": "completed"},
				"result": map[string]any{"success": true, "stdout": "command output"},
			})
		}
	})
	defer srv.Close()

	c := New(srv.URL, "")
	stdout, err := c.RunCommand("agent-1", "Get-Date", 5)
	if err != nil {
		t.Fatalf("RunCommand: %v", err)
	}
	if stdout != "command output" {
		t.Errorf("stdout = %s", stdout)
	}
}

func TestSDKBatchRunByTag(t *testing.T) {
	srv := mockServer(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"dispatched": []string{"a1", "a2"},
			"count":      2,
		})
	})
	defer srv.Close()

	c := New(srv.URL, "")
	agents, err := c.Batch().RunByTag("env=prod", "powershell", json.RawMessage(`{"command":"hostname"}`), 30)
	if err != nil {
		t.Fatalf("RunByTag: %v", err)
	}
	if len(agents) != 2 {
		t.Errorf("agents = %v", agents)
	}
}

func TestSDKPowerShutdown(t *testing.T) {
	srv := mockServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/power" {
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if body["op"] != "shutdown" {
				t.Errorf("op = %v", body["op"])
			}
			w.WriteHeader(202)
			json.NewEncoder(w).Encode(map[string]any{"status": "accepted"})
			return
		}
		w.WriteHeader(404)
	})
	defer srv.Close()

	c := New(srv.URL, "")
	err := c.Power().Shutdown("agent-1", 30)
	if err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func TestSDKSecretPutGet(t *testing.T) {
	srv := mockServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/api/secrets" {
			json.NewEncoder(w).Encode(map[string]any{"ref": "sec_test123"})
			return
		}
		if r.Method == "GET" && r.URL.Path == "/api/secrets/sec_test123" {
			json.NewEncoder(w).Encode(map[string]any{"value": "my-password"})
			return
		}
		w.WriteHeader(404)
	})
	defer srv.Close()

	c := New(srv.URL, "")
	ref, err := c.Secrets().Put("my-password", 60, true)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if ref != "sec_test123" {
		t.Errorf("ref = %s", ref)
	}
	val, err := c.Secrets().Get(ref)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if val != "my-password" {
		t.Errorf("value = %s", val)
	}
}

func TestSDKCronAddList(t *testing.T) {
	srv := mockServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/api/crons" {
			json.NewEncoder(w).Encode(map[string]any{"id": "cron-1", "cron_expr": "*/5 * * * *"})
			return
		}
		if r.Method == "GET" && r.URL.Path == "/api/crons" {
			json.NewEncoder(w).Encode(map[string]any{
				"crons": []map[string]any{
					{"id": "cron-1", "cron_expr": "*/5 * * * *"},
				},
				"count": 1,
			})
			return
		}
		w.WriteHeader(404)
	})
	defer srv.Close()

	c := New(srv.URL, "")
	rule, err := c.Cron().Add("agent-1", "*/5 * * * *", "powershell", json.RawMessage(`{"command":"Get-Date"}`), 30)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if rule.ID != "cron-1" {
		t.Errorf("ID = %s", rule.ID)
	}
	rules, err := c.Cron().List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rules) != 1 {
		t.Errorf("rules = %d", len(rules))
	}
}

func TestSDKShellOpen(t *testing.T) {
	srv := mockServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/shell/open" {
			json.NewEncoder(w).Encode(map[string]any{"session_id": "sess-123"})
			return
		}
		if r.URL.Path == "/api/shell/input" {
			w.WriteHeader(202)
			return
		}
		if r.URL.Path == "/api/shell/close" {
			w.WriteHeader(200)
			return
		}
		w.WriteHeader(404)
	})
	defer srv.Close()

	c := New(srv.URL, "")
	sess, err := c.Shell().Open("agent-1", 80, 24)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if sess.SessionID != "sess-123" {
		t.Errorf("SessionID = %s", sess.SessionID)
	}
	if err := sess.Send("ls\n"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := sess.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestSDKAgentRevoke(t *testing.T) {
	srv := mockServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/agents/revoke" {
			json.NewEncoder(w).Encode(map[string]any{"agent_id": "a1", "status": "revoked"})
			return
		}
		w.WriteHeader(404)
	})
	defer srv.Close()

	c := New(srv.URL, "")
	err := c.Agents().Revoke("a1")
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}
}

func TestSDKEnrollToken(t *testing.T) {
	srv := mockServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/enroll-token" {
			json.NewEncoder(w).Encode(map[string]any{"token": "enroll-xyz", "expires_in": 3600})
			return
		}
		w.WriteHeader(404)
	})
	defer srv.Close()

	c := New(srv.URL, "")
	token, err := c.Agents().EnrollToken("new-agent")
	if err != nil {
		t.Fatalf("EnrollToken: %v", err)
	}
	if token != "enroll-xyz" {
		t.Errorf("token = %s", token)
	}
}
