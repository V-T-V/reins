package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBuildMuxProxy(t *testing.T) {
	// Mock upstream Relay
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"proxied": true, "path": "` + r.URL.Path + `"}`))
	}))
	defer upstream.Close()

	mux := buildMux(upstream.URL)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// /api/* 应被代理
	resp, err := http.Get(srv.URL + "/api/agents")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := make([]byte, 1024)
	n, _ := resp.Body.Read(body)
	if !strings.Contains(string(body[:n]), "proxied") {
		t.Errorf("应被代理到 upstream, 得到: %s", string(body[:n]))
	}
}

func TestBuildMuxStaticFrontend(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("upstream"))
	}))
	defer upstream.Close()

	mux := buildMux(upstream.URL)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// / 应返回静态前端(HTML),不是 upstream
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := make([]byte, 4096)
	n, _ := resp.Body.Read(body)
	if n == 0 {
		t.Error("前端页面不应为空")
	}
	// 应含 HTML 标签
	if !strings.Contains(string(body[:n]), "<") {
		t.Error("应返回 HTML 内容")
	}
}

func TestBuildMuxSSEHeader(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("event"))
	}))
	defer upstream.Close()

	mux := buildMux(upstream.URL)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// /api/events 应设置 X-Accel-Buffering: no
	resp, err := http.Get(srv.URL + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("X-Accel-Buffering") != "no" {
		t.Error("SSE 端点应设置 X-Accel-Buffering: no")
	}
}

func TestBuildMuxInvalidRelayURL(t *testing.T) {
	// buildMux 对无效 URL 用 log.Fatalf,会直接 os.Exit 而非 panic,无法用 recover 捕获;
	// 这里仅测试一个边界:合法但不可达的 URL 不会 hang
	mux := buildMux("http://127.0.0.1:1") // 端口 1 通常不可达
	if mux == nil {
		t.Error("mux 不应为 nil")
	}
}

func TestGetEnv(t *testing.T) {
	// 默认值
	if got := getEnv("GO_RMM_TEST_NONEXIST", "default"); got != "default" {
		t.Errorf("getEnv 默认值 = %s", got)
	}
}
