// Package main — 登录限流测试(2026-08-12 安全加固)。
package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLoginLimiterAllowsBeforeThreshold(t *testing.T) {
	l := NewLoginLimiter()
	defer l.Close()
	for i := 0; i < loginMaxFails-1; i++ {
		if d := l.RecordFail("1.2.3.4"); d > 0 {
			t.Fatalf("未达阈值不应锁定, 第 %d 次返回 %v", i+1, d)
		}
		if ok, _ := l.Allow("1.2.3.4"); !ok {
			t.Fatalf("未达阈值应允许, 第 %d 次", i+1)
		}
	}
}

func TestLoginLimiterLocksAfterThreshold(t *testing.T) {
	l := NewLoginLimiter()
	defer l.Close()
	for i := 0; i < loginMaxFails; i++ {
		l.RecordFail("1.2.3.4")
	}
	ok, rem := l.Allow("1.2.3.4")
	if ok {
		t.Fatal("达阈值应锁定")
	}
	if rem <= 0 {
		t.Fatalf("锁定应剩时间, got %v", rem)
	}
}

func TestLoginLimiterLockDoesNotAffectOtherIP(t *testing.T) {
	l := NewLoginLimiter()
	defer l.Close()
	for i := 0; i < loginMaxFails; i++ {
		l.RecordFail("attacker")
	}
	if ok, _ := l.Allow("other-ip"); !ok {
		t.Fatal("不同 IP 不应被连带锁定")
	}
}

func TestLoginLimiterSuccessClears(t *testing.T) {
	l := NewLoginLimiter()
	defer l.Close()
	for i := 0; i < loginMaxFails-2; i++ {
		l.RecordFail("1.2.3.4")
	}
	l.RecordSuccess("1.2.3.4")
	if l.Fails("1.2.3.4") != 0 {
		t.Fatal("成功后应清零失败计数")
	}
	// 成功后再失败 1 次不应锁定(计数已清)
	if d := l.RecordFail("1.2.3.4"); d > 0 {
		t.Fatalf("清零后单次失败不应锁定, got %v", d)
	}
}

func TestLoginLimiterLockResetsFailCount(t *testing.T) {
	l := NewLoginLimiter()
	defer l.Close()
	for i := 0; i < loginMaxFails; i++ {
		l.RecordFail("1.2.3.4")
	}
	// 锁定后 fails 应重置为 0(解锁后重新累计)
	if l.Fails("1.2.3.4") != 0 {
		t.Fatalf("锁定后 fails 应重置, got %d", l.Fails("1.2.3.4"))
	}
}

func TestLoginLimiterEmptyIPNoLimit(t *testing.T) {
	l := NewLoginLimiter()
	defer l.Close()
	l.RecordFail("")
	if ok, _ := l.Allow(""); !ok {
		t.Fatal("空 IP 不应限流(防误伤)")
	}
}

// TestConsoleLoginRateLimitedViaHTTP 端到端:连续错密码 5 次 → 第 6 次 429。
func TestConsoleLoginRateLimitedViaHTTP(t *testing.T) {
	r := NewRelay(Config{Addr: ":0", ConsolePassword: "s3cret"})
	mux := http.NewServeMux()
	r.registerHTTP(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	defer r.loginLimiter.Close()

	body := `{"username":"admin","password":"wrong"}`
	lastStatus := 0
	for i := 0; i < loginMaxFails+1; i++ {
		resp, err := http.Post(srv.URL+"/api/console/login", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		lastStatus = resp.StatusCode
		resp.Body.Close()
	}
	if lastStatus != http.StatusTooManyRequests {
		t.Fatalf("连续 %d 次错密码后应 429, 最后一次 got %d", loginMaxFails+1, lastStatus)
	}
}
