// Package main — /install.ps1 动态模板化服务测试(烘焙外部地址)。
package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newInstallScriptRelay 创建测试 Relay(参照 newBootstrapRelay 构造方式)。
func newInstallScriptRelay(externalURL, installToken string) *Relay {
	return &Relay{
		cfg:     Config{Addr: ":7780", Token: "tk", ExternalURL: externalURL, InstallToken: installToken},
		agents:  NewRegistry(),
		console: NewEventBus(),
		policy:  DefaultCommandPolicy(),
		store:   NewMemoryStore(),
	}
}

// TestInstallScriptBakesExternalURL ExternalURL 配置应被烘焙进脚本默认值。
// https://203.0.113.99/rmm → 派生 wss://203.0.113.99/rmm/agent,占位符全部消失。
func TestInstallScriptBakesExternalURL(t *testing.T) {
	r := newInstallScriptRelay("https://203.0.113.99/rmm", "")
	mux := http.NewServeMux()
	r.registerHTTP(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/install.ps1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/plain") {
		t.Errorf("Content-Type 应含 text/plain,得到 %s", resp.Header.Get("Content-Type"))
	}
	body, _ := io.ReadAll(resp.Body)
	if !bytes.HasPrefix(body, []byte{0xEF, 0xBB, 0xBF}) {
		t.Errorf("服务版本必须保留 UTF-8 BOM——PS 5.1 执行无 BOM 的 .ps1 文件按 ANSI 误读会解析崩溃;字符串路径由一行命令 TrimStart 兼容")
	}
	s := string(body)
	if !strings.Contains(s, "https://203.0.113.99/rmm") {
		t.Errorf("脚本应烘焙 Server 地址,得到(前 200 字) %s", firstN(s, 200))
	}
	if !strings.Contains(s, "wss://203.0.113.99/rmm/agent") {
		t.Errorf("脚本应烘焙派生 Relay 地址 wss://.../agent,得到(前 200 字) %s", firstN(s, 200))
	}
	if strings.Contains(s, "__REINS_") {
		t.Errorf("占位符应全部被替换,仍残留 __REINS_")
	}
	// 主 Token(cfg.Token)绝不烘焙——本端点未鉴权,防管理凭据随脚本泄漏。
	// 烘焙值不是主 Token(install.ps1 源文件为两空格格式,两种空格形态都查)。
	if strings.Contains(s, `$BakedToken  = "tk"`) || strings.Contains(s, `$BakedToken = "tk"`) {
		t.Errorf("主 Token 不得被烘焙进脚本,BakedToken 值应是空串而非 tk")
	}
}

// TestInstallScriptBakesInstallToken 配置 InstallToken 后,部署专用 Token 应被烘焙进脚本,
// 目标机器零参数部署(iex (irm ...) 连 Token 都不用传)。
func TestInstallScriptBakesInstallToken(t *testing.T) {
	r := newInstallScriptRelay("https://203.0.113.99/rmm", "deploy-tok-123")
	mux := http.NewServeMux()
	r.registerHTTP(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/install.ps1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	if !strings.Contains(s, `"deploy-tok-123"`) {
		t.Errorf("脚本应烘焙部署专用 Token deploy-tok-123")
	}
	if strings.Contains(s, "__REINS_") {
		t.Errorf("占位符应全部被替换,仍残留 __REINS_")
	}
}

// TestInstallScriptDerivesFromRequestHost 无 ExternalURL 时应从请求 Host 推导地址。
func TestInstallScriptDerivesFromRequestHost(t *testing.T) {
	r := newInstallScriptRelay("", "")

	req := httptest.NewRequest("GET", "http://10.1.2.3:9999/install.ps1", nil)
	rec := httptest.NewRecorder()
	r.handleInstallScript(rec, req)

	s := rec.Body.String()
	if !strings.Contains(s, "http://10.1.2.3:9999") {
		t.Errorf("脚本应从请求 Host 推导 http://10.1.2.3:9999,得到(前 200 字) %s", firstN(s, 200))
	}
	if !strings.Contains(s, "ws://10.1.2.3:9999/agent") {
		t.Errorf("脚本应派生 ws://10.1.2.3:9999/agent,得到(前 200 字) %s", firstN(s, 200))
	}
	if strings.Contains(s, "__REINS_") {
		t.Errorf("占位符应全部被替换,仍残留 __REINS_")
	}
}

// TestInstallScriptXForwardedProto 反代场景:X-Forwarded-Proto: https 应翻转 scheme。
func TestInstallScriptXForwardedProto(t *testing.T) {
	r := newInstallScriptRelay("", "")

	req := httptest.NewRequest("GET", "http://example.com/install.ps1", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	r.handleInstallScript(rec, req)

	s := rec.Body.String()
	if !strings.Contains(s, "https://example.com") {
		t.Errorf("脚本应根据 X-Forwarded-Proto 烘焙 https://example.com,得到(前 200 字) %s", firstN(s, 200))
	}
	if !strings.Contains(s, "wss://example.com/agent") {
		t.Errorf("脚本应派生 wss://example.com/agent,得到(前 200 字) %s", firstN(s, 200))
	}
	if strings.Contains(s, "__REINS_") {
		t.Errorf("占位符应全部被替换,仍残留 __REINS_")
	}
}

// firstN 截取前 n 字符(测试失败信息用,避免刷屏)。
func firstN(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
