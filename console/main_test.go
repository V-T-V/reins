package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestConsoleProxiesAPI 验证控制台把 /api/* 反向代理到 Relay。
func TestConsoleProxiesAPI(t *testing.T) {
	// 假 Relay：/api/agents 返回固定 JSON
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/agents" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"agents":[],"count":0}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer relay.Close()

	// 控制台（用 buildMux 构造，避免启动真实监听）
	mux := buildMux(relay.URL)
	console := httptest.NewServer(mux)
	defer console.Close()

	resp, err := http.Get(console.URL + "/api/agents")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"count":0`) {
		t.Fatalf("代理响应应含 count:0，得到: %s", body)
	}
}

// TestConsoleServesStatic 验证静态资源（index.html）被 embed.FS 正确提供。
func TestConsoleServesStatic(t *testing.T) {
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer relay.Close()

	mux := buildMux(relay.URL)
	console := httptest.NewServer(mux)
	defer console.Close()

	resp, err := http.Get(console.URL + "/")
	if err != nil {
		t.Fatalf("请求首页失败: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "reins") || !strings.Contains(string(body), "html") {
		t.Fatalf("首页应含 reins 与 html 标签，得到前 100 字符: %s", string(body[:min(100, len(body))]))
	}

	// app.js 也应可取
	resp2, err := http.Get(console.URL + "/app.js")
	if err != nil {
		t.Fatalf("请求 app.js 失败: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("app.js 应 200，得到 %d", resp2.StatusCode)
	}
}

// TestPortOrDefault 验证端口提取辅助函数。
func TestPortOrDefault(t *testing.T) {
	cases := map[string]string{
		":7781":          ":7781",
		"0.0.0.0:7781":   ":7781",
		"127.0.0.1:7781": ":7781",
		"7781":           "7781", // 无冒号原样返回
	}
	for in, want := range cases {
		if got := portOrDefault(in); got != want {
			t.Errorf("portOrDefault(%q) = %q, want %q", in, got, want)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
