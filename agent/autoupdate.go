package main

// Agent 自动更新(轮询 Relay 版本仓库,可选功能,默认关闭)。
//
// 动机:更新链路原本是纯推送制(update upload + update push),Agent 没有任何
// 自主升级路径——生产实证:仓库最新停在 v4.5.7(8/16),此后无人推送,线上
// agent 永远停在旧版;且 CLI -f 推送的默认路径(base64 内联)在公网必超时。
//
// 流程(每 AutoUpdateInterval 一轮):
//   1. GET {relay}/api/updates/latest(token/mTLS 鉴权,与 WS 同凭证)
//   2. 数值比较 latest.version > 当前 version → 决定升级
//      (dev 当前版本按 0.0.0 处理——线上 dev 构建也能被拉正)
//   3. GET {relay}/api/updates/download?version= 流式落 exeDir/.auto,
//      边下边算 SHA256 与仓库摘要比对
//   4. 复用 execUpdate 统一收尾:防降级 → .bak 备份 → 两步 rename →
//      pendingRestart → 进程退出 → SCM 拉新版本 → 60s 健康自检失败自动回滚
//
// 安全边界:仅 slave 角色(master 永不自动更新);下载通道与 WS 同鉴权;
// 版本只升不降;与推送式更新共用 updateMu 互斥(两条路不会同时 rename)。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// deriveRelayHTTPBase 把 Relay WS 端点推导为 HTTP API 基地址。
// ws://host:7780/agent → http://host:7780
// wss://host/rmm/agent → https://host/rmm
func deriveRelayHTTPBase(relayURL string) string {
	u, err := url.Parse(relayURL)
	if err != nil {
		return ""
	}
	switch u.Scheme {
	case "wss":
		u.Scheme = "https"
	case "ws":
		u.Scheme = "http"
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	u.Path = strings.TrimSuffix(u.Path, "/agent")
	if u.Path == "/" {
		u.Path = ""
	}
	return u.String()
}

// autoUpdateHTTPClient 构造访问 Relay API 的 client(复用 mTLS 凭证或明文)。
func autoUpdateHTTPClient(a *Agent) *http.Client {
	if a.certs == nil {
		return &http.Client{Timeout: 10 * time.Minute}
	}
	return &http.Client{
		Timeout:   10 * time.Minute,
		Transport: &http.Transport{TLSClientConfig: a.certs.TLSConfig()},
	}
}

// autoUpdateRequest 带 Agent 自身凭证请求 Relay API。
func (a *Agent) autoUpdateRequest(ctx context.Context, method, rawURL string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
	if err != nil {
		return nil, err
	}
	if a.cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+a.cfg.Token)
	}
	return req, nil
}

// shouldAutoUpdate 升级决策:latest 数值大于 current 才升级。
// dev 当前版本按 0.0.0(parseSemver 全零)→ 任何正式版本都能拉正线上 dev 构建。
func shouldAutoUpdate(current, latest string) bool {
	if latest == "" || latest == current {
		return false
	}
	return versionGE(latest, current)
}

// autoUpdateLoop 周期检查并自动升级(进程级,独立于 WS 连接起伏)。
func (a *Agent) autoUpdateLoop(ctx context.Context) {
	if a.cfg.Role == "master" {
		log.Printf("[auto-update] master 角色不自动更新(设计如此,由 slave 承担)")
		return
	}
	interval := a.cfg.AutoUpdateInterval
	log.Printf("[auto-update] 已启用: 每 %v 检查 Relay 版本仓库", interval)
	// 首轮延迟 90s:避开启动期健康自检窗口(60s),防止连环更新回滚抖动
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	first := time.NewTimer(90 * time.Second)
	defer first.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-first.C:
		case <-ticker.C:
		}
		if pendingRestart {
			return // 已有更新待重启,不再叠加
		}
		if err := a.tryAutoUpdate(ctx); err != nil {
			log.Printf("[auto-update] 本轮失败(下轮重试): %v", err)
		}
	}
}

// tryAutoUpdate 执行一轮检查+下载+应用。
func (a *Agent) tryAutoUpdate(ctx context.Context) error {
	base := deriveRelayHTTPBase(a.cfg.RelayURL)
	if base == "" {
		return fmt.Errorf("relay_url 无法推导 HTTP 基地址: %s", a.cfg.RelayURL)
	}

	// 1. 查最新
	req, err := a.autoUpdateRequest(ctx, http.MethodGet, base+"/api/updates/latest")
	if err != nil {
		return err
	}
	resp, err := autoUpdateHTTPClient(a).Do(req)
	if err != nil {
		return fmt.Errorf("查 latest 失败: %w", err)
	}
	var lr struct {
		Enabled bool `json:"enabled"`
		Latest  *struct {
			Version string `json:"version"`
			SHA256  string `json:"sha256"`
			Size    int64  `json:"size"`
		} `json:"latest"`
	}
	err = json.NewDecoder(resp.Body).Decode(&lr)
	resp.Body.Close()
	if err != nil {
		return fmt.Errorf("解析 latest 失败(HTTP %d): %w", resp.StatusCode, err)
	}
	if lr.Latest == nil {
		return nil // 仓库空/未启用,无事可做
	}
	if !shouldAutoUpdate(version, lr.Latest.Version) {
		log.Printf("[auto-update] 已是最新(%s,仓库最新 %s)", version, lr.Latest.Version)
		return nil
	}
	log.Printf("[auto-update] 发现新版本: %s → %s(%d bytes)", version, lr.Latest.Version, lr.Latest.Size)

	// 2. 下载(流式 + 边下边算 hash)
	dlURL := base + "/api/updates/download?version=" + url.QueryEscape(lr.Latest.Version)
	if err := a.downloadTo(ctx, dlURL, lr.Latest.SHA256); err != nil {
		return fmt.Errorf("下载失败: %w", err)
	}

	// 3. 统一收尾(防降级/.bak/rename/pendingRestart/健康回滚全套复用)
	result := updateExec.execUpdate([]byte(fmt.Sprintf(
		`{"download_url":null,"binary_path":%q,"sha256":%q,"version":%q,"min_version":%q}`,
		autoUpdateBinPath(), lr.Latest.SHA256, lr.Latest.Version, version)))
	if !result.Success {
		return fmt.Errorf("应用更新失败: %s", result.Error)
	}
	log.Printf("[auto-update] %s", result.Stdout)
	return nil
}

// autoUpdateBinPath 自动更新下载的暂存文件路径。
func autoUpdateBinPath() string {
	return filepath.Join(exeDir(), "go-rmm-agent.exe.auto")
}

// downloadTo 流式下载 URL 到 .auto 暂存文件,校验 SHA256。
func (a *Agent) downloadTo(ctx context.Context, rawURL, wantSHA string) error {
	req, err := a.autoUpdateRequest(ctx, http.MethodGet, rawURL)
	if err != nil {
		return err
	}
	resp, err := autoUpdateHTTPClient(a).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	dst := autoUpdateBinPath()
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(f, h), resp.Body)
	closeErr := f.Close()
	if copyErr != nil {
		os.Remove(dst)
		return copyErr
	}
	if closeErr != nil {
		os.Remove(dst)
		return closeErr
	}
	got := hex.EncodeToString(h.Sum(nil))
	if wantSHA != "" && got != wantSHA {
		os.Remove(dst)
		return fmt.Errorf("SHA256 不符: 仓库=%s 下载=%s", wantSHA[:16], got[:16])
	}
	return nil
}
