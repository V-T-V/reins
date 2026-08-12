// Package main — 部署引导端点(方案 B:一行命令远程部署)。
//
// GET /api/bootstrap 返回一行式安装命令、Token、外部地址等,
// 让用户在 Web UI / CLI 直接复制粘贴到目标 Win10 机器执行。
//
// 该端点需鉴权(Token 即部署凭据),响应包含:
//   - 一行 PowerShell 安装命令(iex (irm ...))
//   - Relay 外部 Server / Relay URL
//   - Token(已鉴权,可安全返回)
//   - dl/ 端点是否就绪(agent 二进制是否已托管)
//
// 设计意图:让 Agent 部署从"看文档 + 手敲命令"简化为"复制一行命令"。
package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// bootstrapInfo 部署引导信息。
type bootstrapInfo struct {
	// ServerURL Relay 的 HTTP 外部地址(用于下载脚本/二进制)。
	ServerURL string `json:"server_url"`
	// RelayURL Agent 的 WS 端点。
	RelayURL string `json:"relay_url"`
	// Token 鉴权 Token(已鉴权,可返回)。
	Token string `json:"token"`
	// OneLineCmd 一行 PowerShell 安装命令(目标机器执行)。
	OneLineCmd string `json:"one_line_cmd"`
	// OneLineDevCmd dev 模式免 Token 的一行命令。
	OneLineDevCmd string `json:"one_line_dev_cmd"`
	// AgentBinaryReady dl/ 目录的 agent.exe 是否就绪。
	AgentBinaryReady bool `json:"agent_binary_ready"`
	// InstallScriptURL install.ps1 托管地址。
	InstallScriptURL string `json:"install_script_url"`
	// AgentDownloadURL agent.exe 下载地址。
	AgentDownloadURL string `json:"agent_download_url"`
}

// handleBootstrap GET /api/bootstrap — 返回一行安装命令(需鉴权)。
//
// 查询参数:
//   - ?host=<ip>   覆盖自动检测的本机 IP(部署到公网时指定公网 IP)
//   - ?port=<port> 覆盖端口
//   - ?dev=1       生成免 Token 的命令(dev 模式)
//
// URL 生成优先级:
//  1. cfg.ExternalURL(显式配置,经反代/TLS 终端时用,最可靠)
//  2. host/port 查询参数
//  3. 自动检测本机 IP + cfg.Addr 端口
func (r *Relay) handleBootstrap(w http.ResponseWriter, req *http.Request) {
	// 鉴权由 registerHTTP 的 authH 包装器统一处理(v4.5.0)。

	// 优先级 1:ExternalURL(显式配置,最可靠)
	// 例如 https://203.0.113.99/rmm,会自动派生 wss://203.0.113.99/rmm/agent
	extURL := strings.TrimRight(r.cfg.ExternalURL, "/")
	if extURL != "" {
		// 允许 ?host=/?port= 不生效(ExternalURL 已是最终权威)
		serverURL := extURL
		// 派生 Relay URL:把 scheme http→ws / https→wss,路径加 /agent
		relayURL := extURL
		if strings.HasPrefix(relayURL, "https://") {
			relayURL = "wss://" + strings.TrimPrefix(relayURL, "https://")
		} else if strings.HasPrefix(relayURL, "http://") {
			relayURL = "ws://" + strings.TrimPrefix(relayURL, "http://")
		}
		relayURL += "/agent"

		r.emitBootstrap(w, serverURL, relayURL)
		return
	}

	// 优先级 2/3:host/port 查询参数 + 自动检测
	host := getLocalIP()
	port := strings.TrimPrefix(r.cfg.Addr, ":")
	if port == "" {
		port = "7780"
	}
	if q := req.URL.Query().Get("host"); q != "" {
		host = q
	}
	if q := req.URL.Query().Get("port"); q != "" {
		port = q
	}

	// 推导 scheme(http vs https)
	// mTLS 模式用 wss/https;否则 ws/http
	scheme := "http"
	wsScheme := "ws"
	if r.cfg.MTLS {
		scheme = "https"
		wsScheme = "wss"
	}
	serverURL := scheme + "://" + host + ":" + port
	relayURL := wsScheme + "://" + host + ":" + port + "/agent"

	r.emitBootstrap(w, serverURL, relayURL)
}

// emitBootstrap 构造并写入 bootstrap 响应(供 handleBootstrap 复用)。
func (r *Relay) emitBootstrap(w http.ResponseWriter, serverURL, relayURL string) {
	// 检测 dl/ 是否有 agent.exe
	agentReady := checkAgentBinaryReady()

	// 生成一行命令
	// 注意:不能用 iex (irm '...') -Server ...,因为 iex 不接受命名参数。
	// 正确做法:用 [scriptblock]::Create() 把脚本字符串编译成代码块,
	// 再用 & 调用操作符传入命名参数。
	tokenArg := ""
	if r.cfg.Token != "" && !r.cfg.Dev {
		tokenArg = " -Token '" + r.cfg.Token + "'"
	}
	oneLine := "& ([scriptblock]::Create((irm '" + serverURL + "/install.ps1'))) -Server '" + serverURL + "' -Relay '" + relayURL + "'" + tokenArg
	devCmd := "& ([scriptblock]::Create((irm '" + serverURL + "/install.ps1'))) -Server '" + serverURL + "' -Relay '" + relayURL + "'"

	info := bootstrapInfo{
		ServerURL:        serverURL,
		RelayURL:         relayURL,
		Token:            r.cfg.Token,
		OneLineCmd:       oneLine,
		OneLineDevCmd:    devCmd,
		AgentBinaryReady: agentReady,
		InstallScriptURL: serverURL + "/install.ps1",
		AgentDownloadURL: serverURL + "/dl/go-rmm-agent.exe",
	}
	writeJSON(w, http.StatusOK, info)
}

// checkAgentBinaryReady 检查 dl/ 目录是否有可下载的 agent.exe。
func checkAgentBinaryReady() bool {
	path := filepath.Join(dlDir(), "go-rmm-agent.exe")
	if fi, err := os.Stat(path); err == nil && fi.Size() > 1024*1024 {
		return true
	}
	return false
}
