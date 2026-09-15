// Package main — Webhook 告警(F11 扩展)。
//
// 任务失败/Agent 离线/策略拦截/电源操作/证书撤销 时自动发 HTTP POST 到配置的 webhook URL。
// 支持 Slack/DingTalk/企业微信/自定义 HTTP 端点。
//
// 配置(flag 或环境变量,优先级 flag > env):
//
//	-webhooks "https://hooks.slack.com/...,https://oapi.dingtalk.com/..."
//	或 GO_RMM_WEBHOOKS="url1,url2"
//
// 触发事件(2026-09-14 收口:5 类事件全接线):
//   - task_failed     任务执行失败(Agent 回传 result.Success=false)
//   - agent_offline   Agent 离线
//   - policy_blocked  命令策略拦截(单任务/批量/DAG 节点)
//   - power_executed  电源操作执行
//   - agent_revoked   Agent 证书撤销
package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// webhookURLs 全局 webhook URL 列表。
var (
	webhookURLs   []string
	webhookURLsMu sync.RWMutex
	webhookClient = &http.Client{Timeout: 10 * time.Second}
)

// parseWebhookURLs 解析逗号分隔的 webhook URL 列表(去空白/去空项)。
func parseWebhookURLs(raw string) []string {
	if raw == "" {
		return nil
	}
	var urls []string
	for _, u := range strings.Split(raw, ",") {
		if u = strings.TrimSpace(u); u != "" {
			urls = append(urls, u)
		}
	}
	return urls
}

// initWebhooks 初始化 webhook URLs。
// 优先级:flag 显式配置(-webhooks) > 环境变量(GO_RMM_WEBHOOKS)。
func initWebhooks(flagURLs string) {
	urls := parseWebhookURLs(flagURLs)
	if len(urls) == 0 {
		urls = parseWebhookURLs(os.Getenv("GO_RMM_WEBHOOKS"))
	}
	setWebhookURLs(urls)
	if len(urls) > 0 {
		slog.Info("webhook 告警已启用", "count", len(urls))
	}
}

// setWebhookURLs 设置全局 webhook URL 列表(测试/运行时配置用)。
func setWebhookURLs(urls []string) {
	webhookURLsMu.Lock()
	webhookURLs = urls
	webhookURLsMu.Unlock()
}

// notifyWebhook 异步发送 webhook 通知(非阻塞,失败静默)。
func notifyWebhook(event string, payload map[string]any) {
	webhookURLsMu.RLock()
	urls := webhookURLs
	webhookURLsMu.RUnlock()
	if len(urls) == 0 {
		return
	}
	// 构造通知体(通用 JSON,适配大多数 webhook 接收端)
	notification := map[string]any{
		"source":    "go-rmm",
		"event":     event,
		"timestamp": time.Now().Format(time.RFC3339),
		"data":      payload,
	}
	body, _ := json.Marshal(notification)

	go func() {
		for _, url := range urls {
			resp, err := webhookClient.Post(url, "application/json", bytes.NewReader(body))
			if err != nil {
				slog.Debug("webhook 发送失败", "url", url, "error", err)
				continue
			}
			resp.Body.Close()
			if resp.StatusCode >= 400 {
				slog.Debug("webhook 返回错误", "url", url, "status", resp.StatusCode)
			}
		}
	}()
}

// === 事件触发点(在 relay 各 handler 中调用) ===

// notifyTaskFailed 任务失败通知。
func notifyTaskFailed(agentID, taskID, errMsg string) {
	notifyWebhook("task_failed", map[string]any{
		"agent_id": agentID,
		"task_id":  taskID,
		"error":    truncate(errMsg, 500),
	})
}

// notifyAgentOffline Agent 离线通知。
func notifyAgentOffline(agentID, hostname string) {
	notifyWebhook("agent_offline", map[string]any{
		"agent_id": agentID,
		"hostname": hostname,
	})
}

// notifyPolicyBlocked 策略拦截通知。
func notifyPolicyBlocked(agentID, taskType, detail string) {
	notifyWebhook("policy_blocked", map[string]any{
		"agent_id": agentID,
		"type":     taskType,
		"detail":   truncate(detail, 500),
	})
}

// notifyPowerExecuted 电源操作通知。
func notifyPowerExecuted(agentID, op string, delay int) {
	notifyWebhook("power_executed", map[string]any{
		"agent_id": agentID,
		"op":       op,
		"delay":    delay,
	})
}

// notifyAgentRevoked Agent 证书撤销通知。
func notifyAgentRevoked(agentID string) {
	notifyWebhook("agent_revoked", map[string]any{
		"agent_id": agentID,
	})
}

// truncate 截断字符串。
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
