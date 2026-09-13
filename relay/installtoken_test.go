// Package main — 部署专用 Token(-install-token)测试。
// 权限收窄语义:仅授权 Agent WS 注册与 GET /api/agents,其余 REST 一律 401。
package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
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

// TestInstallTokenRESTScope 部署 Token 的 REST 权限收窄:
// GET /api/agents 放行(装完上线确认),POST /api/tasks 拒绝(401),错误 Token 拒绝。
func TestInstallTokenRESTScope(t *testing.T) {
	_, srv := newInstallTokenRelay()
	defer srv.Close()

	get := func(path, token string) int {
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	// ① GET /api/agents 带部署 Token → 200
	if code := get("/api/agents", "deploy-tok"); code != http.StatusOK {
		t.Errorf("部署 Token 应能读 /api/agents,得到 %d", code)
	}
	// ② POST /api/tasks 带部署 Token → 401(鉴权在 body 解析之前,空 body 不影响)
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
	// ③ GET /api/agents 带错误 token → 401
	if code := get("/api/agents", "wrong"); code != http.StatusUnauthorized {
		t.Errorf("错误 Token 应 401,得到 %d", code)
	}
}
