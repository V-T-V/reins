package main

// v4.7.1 自动更新 Agent 侧测试:
//   1. deriveRelayHTTPBase:ws/wss/子路径/裸地址推导
//   2. shouldAutoUpdate 升级决策表(含 dev 当前版本被拉正)
//   3. downloadTo:流式落盘+sha 校验+失败清理
// 完整自更新闭环(下载→rename→重启→健康回滚)由本地三组件 E2E 覆盖。

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeriveRelayHTTPBase(t *testing.T) {
	cases := []struct{ in, want string }{
		{"ws://host:7780/agent", "http://host:7780"},
		{"wss://host:7780/agent", "https://host:7780"},
		{"wss://203.0.113.99/rmm/agent", "https://203.0.113.99/rmm"},
		{"ws://127.0.0.1:7780/agent", "http://127.0.0.1:7780"},
		{"ws://host/agent/", "http://host"},
	}
	for _, c := range cases {
		if got := deriveRelayHTTPBase(c.in); got != c.want {
			t.Errorf("deriveRelayHTTPBase(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestShouldAutoUpdate(t *testing.T) {
	cases := []struct {
		current, latest string
		want            bool
	}{
		{"v4.7.0", "v4.7.1", true},                // 升级
		{"v4.7.0", "v4.7.0", false},               // 相同
		{"v4.7.0", "v4.6.9", false},               // 降级
		{"v2.0.0", "v10.0.0", true},               // 数值感知(字典序陷阱)
		{"dev", "v4.5.7", true},                   // dev 当前版本按 0.0.0 → 拉正线上 dev 构建
		{"v4.7.0", "", false},                     // 空最新
		{"v4.7.0-abc123", "v4.7.0-abc123", false}, // 同构建号
		{"v4.7.0", "v4.10.0", true},               // 两位数次版本
	}
	for _, c := range cases {
		if got := shouldAutoUpdate(c.current, c.latest); got != c.want {
			t.Errorf("shouldAutoUpdate(%q, %q) = %v, want %v", c.current, c.latest, got, c.want)
		}
	}
}

func TestDownloadTo(t *testing.T) {
	content := "fake-new-agent-binary-0123456789"
	sum := sha256.Sum256([]byte(content))
	wantSHA := hex.EncodeToString(sum[:])

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("v") == "err" {
			w.WriteHeader(500)
			return
		}
		_, _ = w.Write([]byte(content))
	}))
	t.Cleanup(srv.Close)

	a := &Agent{}
	// 正常下载:内容+sha 校验通过
	if err := a.downloadTo(t.Context(), srv.URL+"?v=ok", wantSHA); err != nil {
		t.Fatalf("下载应成功: %v", err)
	}
	b, _ := os.ReadFile(autoUpdateBinPath())
	if string(b) != content {
		t.Errorf("下载内容不符: %d bytes", len(b))
	}

	// sha 不符:报错且清理残留
	if err := a.downloadTo(t.Context(), srv.URL+"?v=ok", strings.Repeat("0", 64)); err == nil {
		t.Error("sha 不符应报错")
	}
	if _, err := os.Stat(autoUpdateBinPath()); !os.IsNotExist(err) {
		t.Error("失败时应清理 .auto 残留")
	}

	// HTTP 500:报错
	if err := a.downloadTo(t.Context(), srv.URL+"?v=err", wantSHA); err == nil {
		t.Error("HTTP 500 应报错")
	}
}

func TestAutoUpdateConfigValidate(t *testing.T) {
	// 过短间隔被拒
	c := DefaultConfig()
	c.RelayURL = "ws://h/agent"
	c.AgentID = "a"
	c.AutoUpdateInterval = 30e9 // 30s
	if err := c.Validate(); err == nil {
		t.Error("30s 间隔应被拒(最小 1m)")
	}
	c.AutoUpdateInterval = 3600e9 // 1h
	if err := c.Validate(); err != nil {
		t.Errorf("1h 间隔应通过: %v", err)
	}
}

var _ = filepath.Join // 保持 import(失败清理断言间接使用)
