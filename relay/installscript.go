// Package main — /install.ps1 动态模板化服务(一行零参数部署)。
// Relay 服务脚本时把外部地址烘焙进默认值,目标机器 iex (irm ...) 免交互全自动。
// 静态副本(未经本端点服务)时占位符被脚本自身视为空,退回交互兜底。
package main

import (
	"net/http"
	"strings"
)

// install.ps1 内的地址占位符(服务时替换为真实外部地址)。
const (
	phServerURL = "__REINS_SERVER_URL__"
	phRelayURL  = "__REINS_RELAY_URL__"

	phInstallToken = "__REINS_TOKEN__" // 部署专用 Token(-install-token;空则替换为空串)
)

// externalURLs 推导 Relay 对外可达的 Server / Agent WS 地址(供 install.ps1 烘焙)。
//
// 优先级:
//  1. cfg.ExternalURL(显式配置,经反代/TLS 终端时用,最可靠),派生 wss://.../agent
//  2. 请求探测:X-Forwarded-Proto/req.TLS 判 scheme,req.Host 判地址;
//     Host 为空时兜底 getLocalIP() + cfg.Addr 端口(空则 7780)
func (r *Relay) externalURLs(req *http.Request) (serverURL, relayURL string) {
	// 优先级 1:ExternalURL(显式配置,最可靠)
	// 例如 https://203.0.113.10/rmm,自动派生 wss://203.0.113.10/rmm/agent
	extURL := strings.TrimRight(r.cfg.ExternalURL, "/")
	if extURL != "" {
		// 派生 Relay URL:把 scheme http→ws / https→wss(其余前缀不翻转),路径加 /agent
		relayURL = extURL
		if strings.HasPrefix(relayURL, "https://") {
			relayURL = "wss://" + strings.TrimPrefix(relayURL, "https://")
		} else if strings.HasPrefix(relayURL, "http://") {
			relayURL = "ws://" + strings.TrimPrefix(relayURL, "http://")
		}
		return extURL, relayURL + "/agent"
	}

	// 优先级 2:从请求推导 scheme(X-Forwarded-Proto / req.TLS 判 TLS)
	scheme := "http"
	if req.Header.Get("X-Forwarded-Proto") == "https" || req.TLS != nil {
		scheme = "https"
	}
	// Host 为空时兜底:本机 IP + 监听端口(Addr 形如 ":7780")
	host := req.Host
	if host == "" {
		port := strings.TrimPrefix(r.cfg.Addr, ":")
		if port == "" {
			port = "7780"
		}
		host = getLocalIP() + ":" + port
	}
	wsScheme := "ws"
	if scheme == "https" {
		wsScheme = "wss"
	}
	return scheme + "://" + host, wsScheme + "://" + host + "/agent"
}

// registerInstallScript 注册 /install.ps1 端点(一行零参数部署入口)。
// 精确匹配优先于 "/" 静态兜底,故经此端点服务的脚本一定是烘焙版。
func (r *Relay) registerInstallScript(mux *http.ServeMux) {
	mux.HandleFunc("/install.ps1", r.handleInstallScript)
}

// handleInstallScript GET /install.ps1 — 读内嵌脚本,烘焙外部地址后返回。
// 目标机器一行命令零参数全自动部署(免交互/UAC 自动提权)。
// 编码契约:响应保留源文件的 UTF-8 BOM——PS 5.1 执行无 BOM 的 .ps1 文件会按
// ANSI 误读,UTF-8 中文多字节序列会吃掉收尾引号导致解析崩溃;而 irm 字符串路径
// 由一行命令统一 .TrimStart([char]0xFEFF) 兼容(scriptblock/iex 对 BOM 字符报错)。
func (r *Relay) handleInstallScript(w http.ResponseWriter, req *http.Request) {
	raw, err := webFS.ReadFile("web/install.ps1")
	if err != nil {
		http.Error(w, "install.ps1 读取失败", http.StatusInternalServerError)
		return
	}
	serverURL, relayURL := r.externalURLs(req)
	script := strings.ReplaceAll(string(raw), phServerURL, serverURL)
	script = strings.ReplaceAll(script, phRelayURL, relayURL)
	// 仅烘焙部署专用 Token(InstallToken 为空则替换为空串,脚本退回交互兜底);
	// 主 Token(cfg.Token)绝不烘焙——本端点未鉴权,防管理凭据随脚本泄漏。
	script = strings.ReplaceAll(script, phInstallToken, r.cfg.InstallToken)

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write([]byte(script))
}
