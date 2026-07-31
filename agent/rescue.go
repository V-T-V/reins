// Package main — 经 Relay 救援从 Agent(M1 主从架构)。
//
// 这是"主救援从"的第二通道(本地看门狗是第一通道)。
// 当从故障且本地救援不足以解决(如需要降版本/换二进制)时,
// 主作为 Relay 的 API 客户端,调 POST /api/updates/apply 给从推更新。
//
// 复用 Relay 现有的 handleUpdateApply + 从的 rename+copy+SCM 全套,无需改 Relay。
//
// 触发时机:
//   - 本地看门狗连续救援失败且 .bak 回滚也失败时
//   - 主收到 watchdog 任务(CLI/Relay 主动触发)时
//   - 从版本与主已知好版本不一致时(未来扩展)

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

// rescueSlaveViaRelay 经 Relay 给从推版本更新(救援)。
//
// 参数:
//   - cfg: 主的配置(含 RelayURL/Token/SlaveAgentID)
//   - version: 要推的版本号(必须在 Relay 的版本仓库中存在)
//   - force: 是否强制更新(跳过版本防降级检查)
//
// 流程:
//  1. 主调 Relay: POST /api/updates/apply {agent_id, version, force}
//  2. Relay 读版本仓库 → base64 内联 → 下发 proto.TaskUpdate 给从
//  3. 从走现有 rename+copy+SCM 重启
//
// 返回 Relay 的响应(含 task_id,可后续查询)。
func rescueSlaveViaRelay(cfg Config, version string, force bool) (string, error) {
	if cfg.SlaveAgentID == "" {
		return "", fmt.Errorf("slave_agent_id 未配置,无法经 Relay 救援")
	}
	if cfg.RelayURL == "" {
		return "", fmt.Errorf("relay_url 未配置")
	}

	// 构造 API URL:ws://host:7780/agent → http://host:7780
	apiURL := relayURLToAPI(cfg.RelayURL) + "/api/updates/apply"

	body := map[string]any{
		"agent_id": cfg.SlaveAgentID,
		"version":  version,
		"force":    force,
	}
	bodyBytes, _ := json.Marshal(body)

	req, err := http.NewRequest("POST", apiURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
	}

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("Relay 请求失败: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	respStr := string(respBody)

	if resp.StatusCode != http.StatusOK {
		return respStr, fmt.Errorf("Relay 返回 HTTP %d", resp.StatusCode)
	}
	log.Printf("[rescue] 经 Relay 救援从 %s: 推版本 %s force=%v → %s",
		cfg.SlaveAgentID, version, force, respStr)
	return respStr, nil
}

// relayURLToAPI 把 Relay 的 Agent WS 端点转成 HTTP API base。
// ws://host:7780/agent → http://host:7780
// wss://host/rmm/agent → https://host/rmm
func relayURLToAPI(relayURL string) string {
	s := relayURL
	s = trimSuffix(s, "/agent")
	if len(s) > 0 && s[0:1] == "/" {
		s = s[1:]
	}
	// ws:// → http://, wss:// → https://
	if len(s) >= 5 && s[:5] == "ws://" {
		return "http://" + s[5:]
	}
	if len(s) >= 6 && s[:6] == "wss://" {
		return "https://" + s[6:]
	}
	return s
}

func trimSuffix(s, suffix string) string {
	for len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix {
		return s[:len(s)-len(suffix)]
	}
	return s
}
