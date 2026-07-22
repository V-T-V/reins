package main

import (
	"testing"
)

// TestWsToHTTP 验证 A3 修复：WS URL → HTTP URL 转换。
func TestWsToHTTP(t *testing.T) {
	cases := []struct {
		ws   string
		want string
	}{
		{"wss://relay.example.com:7780/agent", "https://relay.example.com:7780"},
		{"ws://127.0.0.1:7780/agent", "http://127.0.0.1:7780"},
		{"wss://relay:443/agent", "https://relay:443"},
		{"ws://host:80/agent", "http://host:80"},
	}
	for _, c := range cases {
		got := wsToHTTP(c.ws)
		if got != c.want {
			t.Errorf("wsToHTTP(%q) = %q, want %q", c.ws, got, c.want)
		}
	}
}

// TestWsToHTTPEnrollURL 验证 enroll URL 拼接正确。
func TestWsToHTTPEnrollURL(t *testing.T) {
	ws := "wss://relay.example.com:7780/agent"
	base := wsToHTTP(ws)
	enrollURL := base + "/api/enroll"
	want := "https://relay.example.com:7780/api/enroll"
	if enrollURL != want {
		t.Fatalf("enroll URL = %q, want %q", enrollURL, want)
	}
}
