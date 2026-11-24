// Package main — 部署专用 Token(-install-token)测试。
// 权限收窄语义:仅授权 Agent WS 注册与 GET /api/agents?agent_id=<id> 单机探活,
// 其余 REST 一律 401/403。
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/V-T-V/reins/proto"
)

// newInstallTokenRelay 创建主 Token + 部署 Token 双配置的测试 Relay。
func newInstallTokenRelay() (*Relay, *httptest.Server) {
	r := &Relay{
		cfg:     Config{Addr: ":7780", Token: "admin-tok", InstallToken: "deploy-tok"},
		agents:  NewRegistry(),
		console: NewEventBus(),
		policy:  DefaultCommandPolicy(),
		store:   NewMemoryStore(),
	}
	mux := http.NewServeMux()
	r.registerHTTP(mux)
	mux.HandleFunc("/agent", r.handleAgentWS)
	return r, httptest.NewServer(mux)
}

// dialAgentWS 以指定 Bearer token 拨 /agent WS 端点。
func dialAgentWS(t *testing.T, srv *httptest.Server, token string) (*websocket.Conn, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/agent"
	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{ //nolint:bodyclose // coder/websocket:成功时 resp.Body 为 nil,无需关闭
		HTTPHeader: http.Header{"Authorization": []string{"Bearer " + token}},
	})
	return conn, err
}

// TestAgentWSAcceptsInstallToken 部署专用 Token 可注册 Agent WS;错误 Token 401;主 Token 语义不变。
func TestAgentWSAcceptsInstallToken(t *testing.T) {
	_, srv := newInstallTokenRelay()
	defer srv.Close()

	// ① 部署 Token 拨通(连上即可)
	ac, err := dialAgentWS(t, srv, "deploy-tok")
	if err != nil {
		t.Fatalf("部署 Token 应能建立 Agent WS 连接: %v", err)
	}
	ac.CloseNow()

	// ② 错误 Token 拨号失败(401)
	if _, err := dialAgentWS(t, srv, "wrong"); err == nil {
		t.Fatalf("错误 Token 应被拒绝(401),却拨通了")
	}

	// ③ 主 Token 也能拨通(主 Token 语义不变)
	mc, err := dialAgentWS(t, srv, "admin-tok")
	if err != nil {
		t.Fatalf("主 Token 应仍能建立 Agent WS 连接: %v", err)
	}
	mc.CloseNow()
}

// TestInstallTokenRESTScope 部署 Token 的 REST 权限收窄(2026 收紧后的契约):
//
//	GET /api/agents               无 agent_id → 403(不再暴露全局清单)
//	GET /api/agents?agent_id=X    → 200,仅含 {"agent_id":X};X 不在线 → 200 空列表
//	POST /api/tasks               → 401
//	错误 Token                     → 401
//
// 背景:部署 Token 烘焙在**未鉴权**的 /install.ps1 里,视同半公开;原实现可凭它拉到
// 全局 Agent 清单(主机名/系统/标签/传输画像),超出「装完确认本机上线」所需。
func TestInstallTokenRESTScope(t *testing.T) {
	r, srv := newInstallTokenRelay()
	defer srv.Close()
	// 一台在线 Agent(安装确认要找的那台)+ 一台"别人家"的在线 Agent
	r.agents.Register(proto.Register{AgentID: "mine-01", Hostname: "WIN-KIOSK", OS: "windows", Tags: map[string]string{"env": "prod"}}, &wsConn{})
	r.agents.Register(proto.Register{AgentID: "other-02", Hostname: "SECRET-HOST"}, &wsConn{})

	get := func(path, token string) (int, map[string]any) {
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var body map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&body)
		return resp.StatusCode, body
	}

	// ① 无 agent_id → 403(不是 200+空:否则 install.ps1 会白等到超时)
	if code, _ := get("/api/agents", "deploy-tok"); code != http.StatusForbidden {
		t.Errorf("部署 Token 无 agent_id 应 403,得到 %d", code)
	}

	// ② 指定在线的 Agent → 200,且只含 agent_id(无主机名/系统/标签/传输画像)
	code, body := get("/api/agents?agent_id=mine-01", "deploy-tok")
	if code != http.StatusOK {
		t.Fatalf("部署 Token 单机探活应 200,得到 %d", code)
	}
	agents, _ := body["agents"].([]any)
	if len(agents) != 1 {
		t.Fatalf("应恰好返回 1 台,得到 %d", len(agents))
	}
	entry, _ := agents[0].(map[string]any)
	if entry["agent_id"] != "mine-01" {
		t.Errorf("返回的 agent_id = %v,期望 mine-01", entry["agent_id"])
	}
	for _, leak := range []string{"hostname", "os", "version", "capabilities", "tags", "xfer_stats", "online_since"} {
		if _, present := entry[leak]; present {
			t.Errorf("部署 Token 的探活响应泄漏了字段 %q: %v", leak, entry)
		}
	}
	// 即使探的是"别人家"的 Agent,也只拿回自己提供的 agent_id,别无其他
	_, body = get("/api/agents?agent_id=other-02", "deploy-tok")
	agents, _ = body["agents"].([]any)
	if len(agents) != 1 {
		t.Fatalf("在线 Agent 应可见其在线,得到 %d", len(agents))
	}
	if entry, _ = agents[0].(map[string]any); entry["hostname"] != nil {
		t.Errorf("探活他人 Agent 不应泄漏主机名: %v", entry)
	}

	// ③ 指定不在线/不存在的 Agent → 200 空列表(安装轮询的"还没上线"语义)
	if code, body := get("/api/agents?agent_id=never-seen", "deploy-tok"); code != http.StatusOK || body["count"] != float64(0) {
		t.Errorf("不在线的 Agent 应 200+空,得到 %d %v", code, body)
	}

	// ④ POST /api/tasks 带部署 Token → 401(鉴权在 body 解析之前,空 body 不影响)
	req, _ := http.NewRequest("POST", srv.URL+"/api/tasks", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer deploy-tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("部署 Token 不应能下发任务(POST /api/tasks 应 401),得到 %d", resp.StatusCode)
	}

	// ⑤ 部署 Token 打其余只读端点 → 401(权限仅限注册 + 单机探活)
	for _, path := range []string{"/api/tasks", "/api/audit/query", "/api/bootstrap", "/api/crons"} {
		if code, _ := get(path, "deploy-tok"); code != http.StatusUnauthorized {
			t.Errorf("部署 Token 访问 %s 应 401,得到 %d", path, code)
		}
	}

	// ⑥ 错误 token → 401
	if code, _ := get("/api/agents", "wrong"); code != http.StatusUnauthorized {
		t.Errorf("错误 Token 应 401,得到 %d", code)
	}

	// ⑦ 主凭据不受影响:仍是完整清单(2 台,带主机名)
	code, body = get("/api/agents", "admin-tok")
	if code != http.StatusOK || body["count"] != float64(2) {
		t.Fatalf("主凭据应见全量 2 台,得到 %d %v", code, body)
	}
	agents, _ = body["agents"].([]any)
	if entry, _ = agents[0].(map[string]any); entry["hostname"] == nil {
		t.Errorf("主凭据的清单应含主机名: %v", entry)
	}
}
