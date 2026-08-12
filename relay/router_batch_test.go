// Package main — 批量下发 agent_ids 选择器测试(2026-08-12)。
//
// 覆盖 handleBatchTask 的新 agent_ids 选择逻辑 + 回归(tag 模式 + 校验 + cookie 鉴权)。
// 注:mock agent 用 nil conn,无法验证 dispatchTask 成功(dispatched 会空);
// 那部分由 e2e_integration_test.go / round8 覆盖。本文件聚焦"选择 + 校验"逻辑。
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QiuShichang/go-rmm/proto"
)

func postBatch(t *testing.T, srv *httptest.Server, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(srv.URL+"/api/tasks/batch", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func decodeBatchResp(t *testing.T, resp *http.Response) struct {
	Selector   string            `json:"selector"`
	Dispatched []string          `json:"dispatched"`
	TaskIDs    map[string]string `json:"task_ids"`
} {
	t.Helper()
	var data struct {
		Selector   string            `json:"selector"`
		Dispatched []string          `json:"dispatched"`
		TaskIDs    map[string]string `json:"task_ids"`
	}
	json.NewDecoder(resp.Body).Decode(&data)
	return data
}

// TestBatchByAgentIDs 验证 agent_ids 选择器被接受(202 而非 404=选中了在线 agent)。
func TestBatchByAgentIDs(t *testing.T) {
	r, srv := newTestRelay(t)
	defer srv.Close()
	registerMockAgent(r, "a1", []string{"powershell"})
	registerMockAgent(r, "a2", []string{"powershell"})

	resp := postBatch(t, srv, `{"agent_ids":["a1","a2"],"type":"powershell","payload":{"command":"Write-Output hi"},"timeout":30}`)
	defer resp.Body.Close()
	if resp.StatusCode != 202 {
		t.Fatalf("agent_ids 选中在线 agent 应 202, got %d", resp.StatusCode)
	}
	data := decodeBatchResp(t, resp)
	if data.Selector != "agent_ids(2)" {
		t.Errorf("selector 应 agent_ids(2), got %q", data.Selector)
	}
}

// TestBatchByAgentIDsAllOffline 验证 agent_ids 中的离线 agent 被过滤,全离线 → 404。
func TestBatchByAgentIDsAllOffline(t *testing.T) {
	_, srv := newTestRelay(t)
	defer srv.Close()
	resp := postBatch(t, srv, `{"agent_ids":["ghost-agent"],"type":"powershell","payload":{"command":"x"}}`)
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("全离线应 404, got %d", resp.StatusCode)
	}
}

// TestBatchByAgentIDsFiltersOffline 验证混合(在线+离线)只留在线,不 404。
func TestBatchByAgentIDsFiltersOffline(t *testing.T) {
	r, srv := newTestRelay(t)
	defer srv.Close()
	registerMockAgent(r, "online1", []string{"powershell"})

	resp := postBatch(t, srv, `{"agent_ids":["online1","offline-ghost"],"type":"powershell","payload":{"command":"x"}}`)
	defer resp.Body.Close()
	if resp.StatusCode != 202 {
		t.Fatalf("混合应只留在线 → 202, got %d", resp.StatusCode)
	}
}

// TestBatchByTagStillWorks 回归:tag 模式仍工作。
func TestBatchByTagStillWorks(t *testing.T) {
	r, srv := newTestRelay(t)
	defer srv.Close()
	r.agents.Register(proto.Register{
		AgentID: "tagged1", Hostname: "h", OS: "windows/amd64", Version: "t",
		Capabilities: []string{"powershell"}, Tags: map[string]string{"env": "prod"},
	}, nil)

	resp := postBatch(t, srv, `{"tag":"env=prod","type":"powershell","payload":{"command":"x"}}`)
	defer resp.Body.Close()
	if resp.StatusCode != 202 {
		t.Fatalf("tag 模式应 202(回归), got %d", resp.StatusCode)
	}
	data := decodeBatchResp(t, resp)
	if data.Selector != "tag:env=prod" {
		t.Errorf("selector 应 tag:env=prod, got %q", data.Selector)
	}
}

// TestBatchAgentIDsPrecedenceOverTag 验证 agent_ids 优先于 tag(同时给时用 agent_ids)。
func TestBatchAgentIDsPrecedenceOverTag(t *testing.T) {
	r, srv := newTestRelay(t)
	defer srv.Close()
	registerMockAgent(r, "a1", []string{"powershell"})

	resp := postBatch(t, srv, `{"agent_ids":["a1"],"tag":"nope=nomatch","type":"powershell","payload":{"command":"x"}}`)
	defer resp.Body.Close()
	if resp.StatusCode != 202 {
		t.Fatalf("agent_ids 应优先于 tag → 202, got %d", resp.StatusCode)
	}
	data := decodeBatchResp(t, resp)
	if data.Selector != "agent_ids(1)" {
		t.Errorf("应用 agent_ids 选择器, got %q", data.Selector)
	}
}

func TestBatchNoSelector(t *testing.T) {
	_, srv := newTestRelay(t)
	defer srv.Close()
	resp := postBatch(t, srv, `{"type":"powershell","payload":{"command":"x"}}`)
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("无选择器(agent_ids+tag 都空)应 400, got %d", resp.StatusCode)
	}
}

func TestBatchMissingType(t *testing.T) {
	_, srv := newTestRelay(t)
	defer srv.Close()
	resp := postBatch(t, srv, `{"agent_ids":["a1"]}`)
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("缺 type 应 400, got %d", resp.StatusCode)
	}
}

// TestBatchCookieAuth 验证控制台 session cookie 能鉴权批量下发。
func TestBatchCookieAuth(t *testing.T) {
	r := NewRelay(Config{Addr: ":0", ConsolePassword: "s3cret"})
	mux := http.NewServeMux()
	r.registerHTTP(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	registerMockAgent(r, "a1", []string{"powershell"})

	// 登录拿 cookie
	loginReq, _ := http.NewRequest("POST", srv.URL+"/api/console/login", strings.NewReader(`{"username":"admin","password":"s3cret"}`))
	loginReq.Header.Set("Content-Type", "application/json")
	loginResp, err := http.DefaultClient.Do(loginReq)
	if err != nil {
		t.Fatal(err)
	}
	var sid string
	for _, c := range loginResp.Cookies() {
		if c.Name == "gormm_session" {
			sid = c.Value
		}
	}
	loginResp.Body.Close()
	if sid == "" {
		t.Fatal("登录未颁发 cookie")
	}

	// 带 cookie 提交 batch
	req, _ := http.NewRequest("POST", srv.URL+"/api/tasks/batch", strings.NewReader(`{"agent_ids":["a1"],"type":"powershell","payload":{"command":"x"}}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "gormm_session", Value: sid})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 {
		t.Fatal("cookie 鉴权失败(401)")
	}
	if resp.StatusCode != 202 {
		t.Errorf("cookie 应让 batch 通过 → 202, got %d", resp.StatusCode)
	}
}
