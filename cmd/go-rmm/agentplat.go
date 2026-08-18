// CLI 平台感知(P1-4 Linux Agent):按目标 Agent 的 OS 生成 PowerShell 或
// POSIX shell 方言,替代旧的"先 PS 失败再试 sh"盲发双任务。
package main

import "strings"

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
