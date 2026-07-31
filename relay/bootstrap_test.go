// Package main — bootstrap 端点测试(方案 B:一行命令远程部署)。
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newBootstrapRelay 创建带 Token 的 Relay(用 httptest 真实 server)。
func newBootstrapRelay(token string, dev bool) (*Relay, *httptest.Server) {
	r := &Relay{
		cfg:     Config{Addr: ":7780", Token: token, Dev: dev},
		agents:  NewRegistry(),
		console: NewEventBus(),
		policy:  DefaultCommandPolicy(),
		store:   NewMemoryStore(),
	}
	mux := http.NewServeMux()
	r.registerHTTP(mux)
	return r, httptest.NewServer(mux)
}

// TestBootstrapRequiresAuth 无 Token 应返回 401。
func TestBootstrapRequiresAuth(t *testing.T) {
	_, srv := newBootstrapRelay("secret-token", false)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("无 Token 应返回 401,得到 %d", resp.StatusCode)
	}
}

// TestBootstrapWithToken 带 Token + ?host= 应返回完整部署信息。
func TestBootstrapWithToken(t *testing.T) {
	_, srv := newBootstrapRelay("my-secret-1234567890", false)
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/api/bootstrap?host=14.1.2.3", nil)
	req.Header.Set("Authorization", "Bearer my-secret-1234567890")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	var info bootstrapInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		t.Fatal(err)
	}

	// 验证字段
	if !strings.Contains(info.ServerURL, "14.1.2.3") {
		t.Errorf("ServerURL 应包含查询参数 host,得到 %s", info.ServerURL)
	}
	if info.Token != "my-secret-1234567890" {
		t.Errorf("Token 不匹配")
	}
	if !strings.Contains(info.OneLineCmd, "scriptblock") {
		t.Errorf("OneLineCmd 应是 scriptblock 调用(支持命名参数),得到 %s", info.OneLineCmd)
	}
	if !strings.Contains(info.OneLineCmd, "14.1.2.3") {
		t.Errorf("OneLineCmd 应包含 host,得到 %s", info.OneLineCmd)
	}
	if !strings.Contains(info.OneLineCmd, "my-secret-1234567890") {
		t.Errorf("OneLineCmd 应包含 Token,得到 %s", info.OneLineCmd)
	}
	if info.InstallScriptURL == "" || info.AgentDownloadURL == "" {
		t.Errorf("InstallScriptURL/AgentDownloadURL 不应为空")
	}
}

// TestBootstrapDevMode dev 模式(Token 空)authorize 跳过,OneLineCmd 不应含 -Token。
func TestBootstrapDevMode(t *testing.T) {
	_, srv := newBootstrapRelay("", true)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/bootstrap?host=10.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("dev 模式应放行,得到 %d", resp.StatusCode)
	}
	var info bootstrapInfo
	json.NewDecoder(resp.Body).Decode(&info)
	if strings.Contains(info.OneLineCmd, "-Token") {
		t.Errorf("dev 模式 OneLineCmd 不应含 -Token,得到 %s", info.OneLineCmd)
	}
}

// TestBootstrapPortOverride 验证 ?port= 参数。
func TestBootstrapPortOverride(t *testing.T) {
	_, srv := newBootstrapRelay("tk", false)
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/api/bootstrap?host=1.2.3.4&port=443", nil)
	req.Header.Set("Authorization", "Bearer tk")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var info bootstrapInfo
	json.NewDecoder(resp.Body).Decode(&info)
	if !strings.Contains(info.ServerURL, "1.2.3.4:443") {
		t.Errorf("应使用 port 覆盖,得到 %s", info.ServerURL)
	}
}

// TestBootstrapExternalURL 验证 ExternalURL 配置(经 nginx 反代/TLS 终端)。
// 配置 https://203.0.113.99/rmm 应派生出:
//   - server_url = https://203.0.113.99/rmm
//   - relay_url  = wss://203.0.113.99/rmm/agent
//   - ?host=/?port= 应被忽略(ExternalURL 权威)
func TestBootstrapExternalURL(t *testing.T) {
	r := &Relay{
		cfg: Config{
			Addr:        ":7780",
			Token:       "tk",
			ExternalURL: "https://203.0.113.99/rmm",
		},
		agents:  NewRegistry(),
		console: NewEventBus(),
		policy:  DefaultCommandPolicy(),
		store:   NewMemoryStore(),
	}
	mux := http.NewServeMux()
	r.registerHTTP(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// 传 host=?? port=?? 应被忽略
	req, _ := http.NewRequest("GET", srv.URL+"/api/bootstrap?host=IGNORED&port=9999", nil)
	req.Header.Set("Authorization", "Bearer tk")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var info bootstrapInfo
	json.NewDecoder(resp.Body).Decode(&info)

	if info.ServerURL != "https://203.0.113.99/rmm" {
		t.Errorf("ServerURL 应为 ExternalURL,得到 %s", info.ServerURL)
	}
	if info.RelayURL != "wss://203.0.113.99/rmm/agent" {
		t.Errorf("RelayURL 应派生为 wss://.../rmm/agent,得到 %s", info.RelayURL)
	}
	if !strings.Contains(info.OneLineCmd, "https://203.0.113.99/rmm") {
		t.Errorf("OneLineCmd 应含 ExternalURL,得到 %s", info.OneLineCmd)
	}
	if strings.Contains(info.OneLineCmd, "IGNORED") || strings.Contains(info.OneLineCmd, "9999") {
		t.Errorf("ExternalURL 模式应忽略 host/port 参数,得到 %s", info.OneLineCmd)
	}
}
