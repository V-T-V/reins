package main

// UI-P3:浏览器 EventSource 无法携带 Authorization header——同源 cookie
// (reins_token)鉴权回退的测试锚。H4 语义保持:query token 依旧拒绝。

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAuthorizeCookieFallback(t *testing.T) {
	r := NewRelay(Config{Token: "c-secret"})

	// 1) 无凭证 → 拒绝
	req, _ := http.NewRequest("GET", "/api/agents", nil)
	if err := r.authorize(req); err == nil {
		t.Fatal("无凭证应拒绝")
	}

	// 2) cookie 携带正确 token → 放行(EventSource 同源自动带 cookie)
	req2, _ := http.NewRequest("GET", "/api/events", nil)
	req2.AddCookie(&http.Cookie{Name: "reins_token", Value: "c-secret"})
	if err := r.authorize(req2); err != nil {
		t.Fatalf("cookie 正确 token 应放行: %v", err)
	}

	// 3) cookie 错误 token → 拒绝
	req3, _ := http.NewRequest("GET", "/api/events", nil)
	req3.AddCookie(&http.Cookie{Name: "reins_token", Value: "wrong"})
	if err := r.authorize(req3); err == nil {
		t.Fatal("cookie 错误 token 应拒绝")
	}

	// 4) H4 红线:query token 依旧拒绝(cookie 的引入不得复活 query 面)
	req4, _ := http.NewRequest("GET", "/api/agents?token=c-secret", nil)
	if err := r.authorize(req4); err == nil {
		t.Fatal("query token 应被拒绝(H4)")
	}

	// 5) header 优先级高于 cookie(header 错 + cookie 对 → 以 header 为准被拒)
	req5, _ := http.NewRequest("GET", "/api/agents", nil)
	req5.Header.Set("Authorization", "Bearer wrong")
	req5.AddCookie(&http.Cookie{Name: "reins_token", Value: "c-secret"})
	if err := r.authorize(req5); err == nil {
		t.Fatal("header 错误时应拒绝(header 优先,不回落 cookie)")
	}
}

// authenticate 同样对齐:cookie 放行 + query 拒绝(与 authorize 行为一致)。
func TestAuthenticateCookieAligned(t *testing.T) {
	r := NewRelay(Config{Token: "r-secret"})

	req, _ := http.NewRequest("POST", "/api/secrets", nil)
	req.AddCookie(&http.Cookie{Name: "reins_token", Value: "r-secret"})
	if _, err := r.authenticate(req); err != nil {
		t.Fatalf("RBAC cookie 正确 token 应放行: %v", err)
	}

	req2, _ := http.NewRequest("POST", "/api/secrets?token=r-secret", nil)
	if _, err := r.authenticate(req2); err == nil {
		t.Fatal("RBAC query token 应被拒绝(与 authorize H4 对齐)")
	}
}

// TestCookieIdentityNotAdmin 红锚(2026 租户加固 P1-cookie):
// 旧实现 identityFromAuth 只读 Authorization 头,cookie 过鉴权的请求一律被当成 admin。
// 现在要求:cookie 与 Bearer 共用 authenticate,身份取服务端绑定值。
func TestCookieIdentityNotAdmin(t *testing.T) {
	r := NewRelay(Config{Token: "acme:operator:s1"})

	// 前端 setToken 用 encodeURIComponent 写 cookie(: → %3A),服务端必须解码
	for _, val := range []string{"acme%3Aoperator%3As1", "acme:operator:s1", "s1"} {
		req, _ := http.NewRequest("POST", "/api/power", nil)
		req.AddCookie(&http.Cookie{Name: "reins_token", Value: val})
		id, err := r.authenticate(req)
		if err != nil {
			t.Fatalf("cookie %q 应放行: %v", val, err)
		}
		if id.Role != RoleOperator || id.Tenant != "acme" {
			t.Errorf("cookie %q 身份 = %s/%s, 期望 acme/operator(不得默认 admin)", val, id.Tenant, id.Role)
		}
	}

	// 端点级:operator cookie 打 admin-only 端点 → 403(旧实现恒 200/放行)
	h := r.RequireRole("secret.put", func(w http.ResponseWriter, req *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	for _, val := range []string{"acme%3Aoperator%3As1", "acme:operator:s1"} {
		req, _ := http.NewRequest("POST", "/api/secrets", nil)
		req.AddCookie(&http.Cookie{Name: "reins_token", Value: val})
		rec := httptest.NewRecorder()
		h(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("operator cookie(%q) 打 secret.put = %d, 期望 403", val, rec.Code)
		}
	}

	// 畸形转义不解码失败回退原值 → 当作错误凭据拒绝,不得 panic
	req, _ := http.NewRequest("GET", "/api/agents", nil)
	req.AddCookie(&http.Cookie{Name: "reins_token", Value: "%E0%A4%A"})
	if _, err := r.authenticate(req); err == nil {
		t.Error("畸形转义 cookie 应拒绝")
	}
}
