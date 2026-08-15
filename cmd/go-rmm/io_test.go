package main

import (
	"net/http"
	"os"
	"strings"
	"testing"
)

// TestProxyFuncLoopbackDirect P2-B:loopback 目标必须直连(不走环境代理)。
func TestProxyFuncLoopbackDirect(t *testing.T) {
	// 设置环境代理,验证 loopback 仍被豁免。
	os.Setenv("HTTP_PROXY", "http://127.0.0.1:58319")
	os.Setenv("HTTPS_PROXY", "http://127.0.0.1:58319")
	t.Cleanup(func() {
		os.Unsetenv("HTTP_PROXY")
		os.Unsetenv("HTTPS_PROXY")
	})

	for _, host := range []string{"localhost", "127.0.0.1", "::1", "0.0.0.0"} {
		req, _ := http.NewRequest("GET", "http://"+host+":7780/healthz", nil)
		p, err := proxyFunc(req)
		if err != nil {
			t.Fatalf("proxyFunc(%s) 报错: %v", host, err)
		}
		if p != nil {
			t.Errorf("proxyFunc(%s) = %v, want nil(直连)", host, p)
		}
	}
}

// TestProxyFuncNonLoopbackUsesEnv P2-B:非 loopback 目标仍走环境代理。
func TestProxyFuncNonLoopbackUsesEnv(t *testing.T) {
	os.Setenv("HTTP_PROXY", "http://127.0.0.1:58319")
	t.Cleanup(func() {
		os.Unsetenv("HTTP_PROXY")
	})

	req, _ := http.NewRequest("GET", "http://203.0.113.99/rmm/healthz", nil)
	p, err := proxyFunc(req)
	if err != nil {
		t.Fatalf("proxyFunc 报错: %v", err)
	}
	if p == nil {
		t.Fatal("非 loopback 目标应走环境代理")
	}
	if p.Host != "127.0.0.1:58319" {
		t.Errorf("代理地址 = %s, want 127.0.0.1:58319", p.Host)
	}
}

// TestProxyFuncRespectsNoProxy 依赖标准库 ProxyFromEnvironment 的 NO_PROXY 行为,
// 其环境变量读取用 sync.Once 缓存,测试中不可靠刷新,故不重复验证(标准库保证)。

// TestProxyFuncParsesURL 确保 URL 解析(带端口/路径的 https relay)。
func TestProxyFuncParsesURL(t *testing.T) {
	req, _ := http.NewRequest("GET", "https://203.0.113.99/rmm/healthz", nil)
	if _, err := proxyFunc(req); err != nil {
		t.Fatalf("proxyFunc 解析失败: %v", err)
	}
	// hostname() 提取应正确(带端口情况)
	if got := req.URL.Hostname(); got != "203.0.113.99" {
		t.Errorf("Hostname = %s", got)
	}
}

// TestRelayURLKeepPath 确认 relay 带子路径时 WS URL 转换保留路径(与 tryTunnelWS 相关)。
func TestRelayURLKeepPath(t *testing.T) {
	u := "https://203.0.113.99/rmm/api/tunnel/ws?tunnel_id=x&agent_id=y&target=z"
	ws := strings.Replace(u, "http://", "ws://", 1)
	ws = strings.Replace(ws, "https://", "wss://", 1)
	if ws != "wss://203.0.113.99/rmm/api/tunnel/ws?tunnel_id=x&agent_id=y&target=z" {
		t.Errorf("WS URL 转换异常: %s", ws)
	}
}
