// CLI 平台感知(P1-4 Linux Agent):按目标 Agent 的 OS 生成 PowerShell 或
// POSIX shell 方言,替代旧的"先 PS 失败再试 sh"盲发双任务。
package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/QiuShichang/go-rmm/proto"
)

// agentOS 查询目标 Agent 的平台标识(如 "windows/amd64"/"linux/amd64")。
// Agent 不在线或查询失败返回 ""——调用方回退到旧的双发行为(向后兼容)。
func agentOS(cfg config, agentID string) string {
	var resp struct {
		Agents []struct {
			AgentID string `json:"agent_id"`
			OS      string `json:"os"`
		} `json:"agents"`
	}
	if err := apiGet(cfg, "/api/agents", &resp); err != nil {
		return ""
	}
	for _, a := range resp.Agents {
		if a.AgentID == agentID {
			return a.OS
		}
	}
	return ""
}

// isLinuxAgent 目标 Agent 是否 Linux 平台(未知平台返回 false,走 Windows 方言)。
func isLinuxAgent(cfg config, agentID string) bool {
	return strings.HasPrefix(agentOS(cfg, agentID), "linux")
}

// runOpsScript 向 Agent 下发 POSIX shell 脚本(script lang=sh)并轮询结果。
// 语义与 runOpsTask 一致,仅任务类型不同(供 Linux 方言命令使用)。
func runOpsScript(cfg config, agentID, script string, timeout int) (string, error) {
	if agentID == "" {
		return "", fmt.Errorf("-a <agent_id> 必填")
	}
	payload, _ := json.Marshal(struct {
		Content string `json:"content"`
		Lang    string `json:"lang"`
	}{Content: script, Lang: "sh"})
	taskID := "ops-" + fmt.Sprint(time.Now().UnixNano())
	body := struct {
		TaskID  string          `json:"task_id"`
		Type    string          `json:"type"`
		AgentID string          `json:"agent_id"`
		Payload json.RawMessage `json:"payload"`
		Timeout int             `json:"timeout"`
	}{
		TaskID: taskID, Type: "script", AgentID: agentID,
		Payload: payload, Timeout: timeout,
	}
	var resp struct {
		TaskID string `json:"task_id"`
		Status string `json:"status"`
	}
	if err := apiPost(cfg, "/api/tasks", body, &resp); err != nil {
		return "", err
	}
	for i := 0; i < timeout+10; i++ {
		var r struct {
			Task *struct {
				Status string `json:"status"`
			} `json:"task"`
			Result *struct {
				Stdout  string `json:"stdout"`
				Success bool   `json:"success"`
				Error   string `json:"error"`
			} `json:"result"`
		}
		if err := apiGet(cfg, "/api/tasks/"+taskID, &r); err == nil && r.Task != nil {
			if r.Task.Status == proto.StatusCompleted || r.Task.Status == proto.StatusFailed {
				if r.Result != nil {
					if !r.Result.Success {
						return "", fmt.Errorf("%s", r.Result.Error)
					}
					return r.Result.Stdout, nil
				}
				return "", nil
			}
		}
		time.Sleep(time.Second)
	}
	return "", fmt.Errorf("任务超时")
}
