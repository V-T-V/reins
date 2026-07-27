// Package main — Webhook 告警(F11 扩展)。
//
// 任务失败/Agent 离线/策略拦截 时自动发 HTTP POST 到配置的 webhook URL。
// 支持 Slack/DingTalk/企业微信/自定义 HTTP 端点。
//
// 配置(config.json 或环境变量):
//
//	"webhooks": ["https://hooks.slack.com/...", "https://oapi.dingtalk.com/..."]
//	或 GO_RMM_WEBHOOKS="url1,url2"
//
// 触发事件:
//   - task_failed     任务执行失败
//   - agent_offline   Agent 离线
//   - policy_blocked  命令策略拦截
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

// WebhookConfig webhook 配置。
type WebhookConfig struct {
	URLs    []string // webhook URL 列表
	Enabled bool
}

// webhookURLs 全局 webhook URL 列表。
var (
	webhookURLs   []string
	webhookURLsMu sync.RWMutex
	webhookClient = &http.Client{Timeout: 10 * time.Second}
)

// initWebhooks 从环境变量初始化 webhook URLs。
func initWebhooks() {
	urls := os.Getenv("GO_RMM_WEBHOOKS")
	if urls != "" {
		webhookURLsMu.Lock()
		webhookURLs = strings.Split(urls, ",")
		webhookURLsMu.Unlock()
		if len(webhookURLs) > 0 {
			slog.Info("webhook 告警已启用", "count", len(webhookURLs))
		}
	}
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

// truncate 截断字符串。
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
