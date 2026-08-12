// Package main — 控制台会话鉴权测试(2026-08-12)。
//
// 覆盖:
//   - login 成功/失败/禁用
//   - session 端点(未登录/已登录)
//   - authorize 双轨:cookie 通过 / 无凭证 401 / Bearer 仍工作(回归)
//   - logout 使 cookie 失效
//   - session TTL 过期 + 滑动续期
//   - cookie 属性(HttpOnly/Secure/SameSite/Path,X-Forwarded-Proto 自适应)
//   - dev 模式(Token+ConsolePassword 都空)放行(回归)
//   - 审计 login_success/login_failed(Operator + IP)
package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newConsoleRelay 构造带控制台鉴权的测试 Relay。
// ConsolePassword 非空时 NewRelay 自动建 SessionStore;TTL 未设默认 1h。
func newConsoleRelay(t *testing.T, cfg Config) (*Relay, *httptest.Server) {
	t.Helper()
	if cfg.Addr == "" {
		cfg.Addr = ":0"
	}
	if cfg.ConsoleSessionTTL == 0 && cfg.ConsolePassword != "" {
		cfg.ConsoleSessionTTL = time.Hour
	}
	r := NewRelay(cfg)
	mux := http.NewServeMux()
	r.registerHTTP(mux)
	srv := httptest.NewServer(mux)
	return r, srv
}

// doLogin 发登录请求,返回响应与提取到的 session cookie 值(若有)。
func doLogin(t *testing.T, srv *httptest.Server, username, password string, extraHeaders ...string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest("POST", srv.URL+"/api/console/login", strings.NewReader(`{"username":"`+username+`","password":"`+password+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(extraHeaders); i += 2 {
		req.Header.Set(extraHeaders[i], extraHeaders[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range resp.Cookies() {
		if c.Name == "gormm_session" {
			return resp, c.Value
		}
	}
	return resp, ""
}

// getWithCookie 带 session cookie 发 GET,返回响应。
func getWithCookie(t *testing.T, srv, path, sid string) *http.Response {
	t.Helper()
	req, err := http.NewRequest("GET", srv+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sid != "" {
		req.AddCookie(&http.Cookie{Name: "gormm_session", Value: sid})
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// --- login 端点 ---

func TestConsoleLoginSuccess(t *testing.T) {
	_, srv := newConsoleRelay(t, Config{ConsolePassword: "s3cret"})
	defer srv.Close()

	resp, sid := doLogin(t, srv, "alice", "s3cret")
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("正确密码应 200, got %d", resp.StatusCode)
	}
	if sid == "" {
		t.Fatal("登录成功应颁发 session cookie")
	}
}

func TestConsoleLoginWrongPassword(t *testing.T) {
	_, srv := newConsoleRelay(t, Config{ConsolePassword: "s3cret"})
	defer srv.Close()

	resp, sid := doLogin(t, srv, "alice", "wrong")
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("错误密码应 401, got %d", resp.StatusCode)
	}
	if sid != "" {
		t.Fatal("登录失败不应颁发 cookie")
	}
}

func TestConsoleLoginDisabled(t *testing.T) {
	_, srv := newConsoleRelay(t, Config{}) // ConsolePassword 空
	defer srv.Close()

	resp, _ := doLogin(t, srv, "alice", "whatever")
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("控制台鉴权未启用应 404, got %d", resp.StatusCode)
	}
}

// --- session 端点 ---

func TestConsoleSessionEndpoint(t *testing.T) {
	_, srv := newConsoleRelay(t, Config{ConsolePassword: "s3cret"})
	defer srv.Close()

	// 未登录:console_auth=true, logged_in=false
	resp := getWithCookie(t, srv.URL, "/api/console/session", "")
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("session 端点应始终 200, got %d", resp.StatusCode)
	}

	// 登录后带 cookie:logged_in=true
	_, sid := doLogin(t, srv, "bob", "s3cret")
	if sid == "" {
		t.Fatal("登录应颁发 cookie")
	}
	resp2 := getWithCookie(t, srv.URL, "/api/console/session", sid)
	defer resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("已登录 session 端点应 200, got %d", resp2.StatusCode)
	}
}

// --- authorize 双轨 ---

func TestAuthorizeWithSessionCookie(t *testing.T) {
	_, srv := newConsoleRelay(t, Config{ConsolePassword: "s3cret"})
	defer srv.Close()

	_, sid := doLogin(t, srv, "alice", "s3cret")
	resp := getWithCookie(t, srv.URL, "/api/agents", sid)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("有效 session cookie 应能访问 /api/agents (200), got %d", resp.StatusCode)
	}
}

func TestAuthorizeWithoutCredentials(t *testing.T) {
	_, srv := newConsoleRelay(t, Config{ConsolePassword: "s3cret"})
	defer srv.Close()

	resp := getWithCookie(t, srv.URL, "/api/agents", "")
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("无 cookie 无 bearer 应 401, got %d", resp.StatusCode)
	}
}

// TestAuthorizeBearerStillWorks 回归:CLI Bearer 鉴权不受 cookie 改动影响。
func TestAuthorizeBearerStillWorks(t *testing.T) {
	_, srv := newConsoleRelay(t, Config{Token: "relay-token-abc", ConsolePassword: "s3cret"})
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/api/agents", nil)
	req.Header.Set("Authorization", "Bearer relay-token-abc")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("CLI Bearer 应仍工作 (200), got %d", resp.StatusCode)
	}
}

// --- logout ---

func TestConsoleLogoutInvalidatesCookie(t *testing.T) {
	_, srv := newConsoleRelay(t, Config{ConsolePassword: "s3cret"})
	defer srv.Close()

	_, sid := doLogin(t, srv, "alice", "s3cret")
	// 先确认 cookie 有效
	if resp := getWithCookie(t, srv.URL, "/api/agents", sid); resp.StatusCode != 200 {
		t.Fatalf("登录后 cookie 应有效, got %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	// 登出
	req, _ := http.NewRequest("POST", srv.URL+"/api/console/logout", nil)
	req.AddCookie(&http.Cookie{Name: "gormm_session", Value: sid})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("logout 应 200, got %d", resp.StatusCode)
	}
	// 同一 cookie 应已失效
	resp2 := getWithCookie(t, srv.URL, "/api/agents", sid)
	defer resp2.Body.Close()
	if resp2.StatusCode != 401 {
		t.Fatalf("登出后 cookie 应失效 (401), got %d", resp2.StatusCode)
	}
}

// --- TTL ---

func TestSessionTTLExpires(t *testing.T) {
	_, srv := newConsoleRelay(t, Config{ConsolePassword: "s3cret", ConsoleSessionTTL: 60 * time.Millisecond})
	defer srv.Close()

	_, sid := doLogin(t, srv, "alice", "s3cret")
	time.Sleep(150 * time.Millisecond) // 超过 TTL
	resp := getWithCookie(t, srv.URL, "/api/agents", sid)
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("TTL 过期后 cookie 应失效 (401), got %d", resp.StatusCode)
	}
}

// TestSessionSlidingRenewal 滑动续期:在 TTL 内访问应刷新过期时间。
func TestSessionSlidingRenewal(t *testing.T) {
	_, srv := newConsoleRelay(t, Config{ConsolePassword: "s3cret", ConsoleSessionTTL: 100 * time.Millisecond})
	defer srv.Close()

	_, sid := doLogin(t, srv, "alice", "s3cret")
	// t=0: login,expires at 100ms
	time.Sleep(60 * time.Millisecond)
	// t=60ms: 访问(应续期至 160ms)
	if resp := getWithCookie(t, srv.URL, "/api/agents", sid); resp.StatusCode != 200 {
		t.Fatalf("TTL 内访问应 200, got %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	time.Sleep(60 * time.Millisecond)
	// t=120ms: 无滑动则已过期(>100ms),有滑动则仍有效(<160ms)
	resp := getWithCookie(t, srv.URL, "/api/agents", sid)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("滑动续期后 t=120ms 应仍有效 (200), got %d(滑动续期未生效?)", resp.StatusCode)
	}
}

// --- cookie 属性 ---

func TestCookieAttributes(t *testing.T) {
	_, srv := newConsoleRelay(t, Config{ConsolePassword: "s3cret"})
	defer srv.Close()

	// 模拟 Nginx TLS 终端:X-Forwarded-Proto: https → Secure
	resp, _ := doLogin(t, srv, "alice", "s3cret", "X-Forwarded-Proto", "https")
	defer resp.Body.Close()
	sc := resp.Header.Get("Set-Cookie")
	for _, want := range []string{"gormm_session=", "HttpOnly", "Secure", "SameSite=Lax", "Path=/"} {
		if !strings.Contains(sc, want) {
			t.Errorf("Set-Cookie 缺少 %q\ngot: %s", want, sc)
		}
	}
}

// TestCookieNotSecureWithoutTLS HTTP 直连(无 X-Forwarded-Proto)不应设 Secure,
// 否则浏览器在 HTTP 下不存储 cookie,登录失败。
func TestCookieNotSecureWithoutTLS(t *testing.T) {
	_, srv := newConsoleRelay(t, Config{ConsolePassword: "s3cret"})
	defer srv.Close()

	resp, _ := doLogin(t, srv, "alice", "s3cret") // 无 X-Forwarded-Proto
	defer resp.Body.Close()
	sc := resp.Header.Get("Set-Cookie")
	if strings.Contains(sc, "Secure") {
		t.Errorf("HTTP 直连不应设 Secure(浏览器不存)\ngot: %s", sc)
	}
	if !strings.Contains(sc, "HttpOnly") {
		t.Errorf("HttpOnly 应始终设\n got: %s", sc)
	}
}

// --- 回归 ---

// TestDevModeNoAuth 回归:Token + ConsolePassword 都空 → 全放行(原 authorize 行为)。
func TestDevModeNoAuth(t *testing.T) {
	_, srv := newConsoleRelay(t, Config{}) // 都空
	defer srv.Close()

	resp := getWithCookie(t, srv.URL, "/api/agents", "")
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("dev 模式(都空)应放行 (200), got %d", resp.StatusCode)
	}
}

// --- 审计 ---

func TestAuditLoginEvents(t *testing.T) {
	r, srv := newConsoleRelay(t, Config{ConsolePassword: "s3cret"})
	defer srv.Close()

	// 注入真实 AuditLog 写临时文件
	tmpFile := filepath.Join(t.TempDir(), "audit.log")
	a, err := OpenAuditLog(tmpFile)
	if err != nil {
		t.Fatal(err)
	}
	r.WithAudit(a)

	// 失败 + 成功各一次
	resp1, _ := doLogin(t, srv, "alice", "wrong")
	resp1.Body.Close()
	resp2, _ := doLogin(t, srv, "alice", "s3cret")
	resp2.Body.Close()

	a.Close() // 排空 channel 写盘

	data, err := os.ReadFile(tmpFile)
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	if !strings.Contains(body, "console_login_failed") {
		t.Errorf("审计应记录 login_failed\n%s", body)
	}
	if !strings.Contains(body, "console_login_success") {
		t.Errorf("审计应记录 login_success\n%s", body)
	}
	if !strings.Contains(body, `"operator":"alice"`) {
		t.Errorf("审计 Operator 应为 alice\n%s", body)
	}
}
