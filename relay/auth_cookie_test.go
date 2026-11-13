package main

// UI-P3:浏览器 EventSource 无法携带 Authorization header——同源 cookie
// (reins_token)鉴权回退的测试锚。H4 语义保持:query token 依旧拒绝。

import (
	"net/http"
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

// authorizeRBAC 同样对齐:cookie 放行 + query 拒绝(两处鉴权行为一致性)。
func TestAuthorizeRBACCookieAligned(t *testing.T) {
	r := NewRelay(Config{Token: "r-secret"})

	req, _ := http.NewRequest("POST", "/api/secrets", nil)
	req.AddCookie(&http.Cookie{Name: "reins_token", Value: "r-secret"})
	if _, err := r.authorizeRBAC(req); err != nil {
		t.Fatalf("RBAC cookie 正确 token 应放行: %v", err)
	}

	req2, _ := http.NewRequest("POST", "/api/secrets?token=r-secret", nil)
	if _, err := r.authorizeRBAC(req2); err == nil {
		t.Fatal("RBAC query token 应被拒绝(与 authorize H4 对齐)")
	}
}
