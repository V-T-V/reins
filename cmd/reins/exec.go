// CLI exec 命令 — 多 Agent 批量并行执行 + 结果汇总(F5 扩展)。
//
// 增强版 task batch:
//   - 并行下发到多个 Agent(不等单个完成)
//   - 实时汇总各 Agent 结果
//   - 支持 --tag 批量 + --agent 列表混合
//   - JSON/表格 双模式输出
//
// 用法:
//
//	reins exec -a agent-1,agent-2,agent-3 -c "Get-Date"
//	reins exec --tag env=prod -c "Get-Service MyService"
//	reins exec -a agent-1 --tag env=staging -c "hostname"
//	reins exec --tag env=prod -s ./deploy.ps1 --timeout 120
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/V-T-V/reins/proto"
)

// cmdExec exec 子命令入口。
func cmdExec(cfg config, args []string) error {
	// 解析参数
	agentList := extractArg(args, "-a")
	tagFilter := extractArg(args, "--tag")
	command := extractArg(args, "-c")
	scriptPath := extractArg(args, "-s")
	timeoutStr := extractArg(args, "--timeout")
	parallelStr := extractArg(args, "--parallel")

	timeout := 60
	if timeoutStr != "" {
		fmt.Sscanf(timeoutStr, "%d", &timeout)
	}
	parallel := 10 // 默认并行 10
	if parallelStr != "" {
		fmt.Sscanf(parallelStr, "%d", &parallel)
	}
	if command == "" && scriptPath == "" {
		return fmt.Errorf("用法: reins exec -a <agents> [--tag <tag>] -c <cmd> [-s <script>]")
	}

	// 收集目标 Agent 列表
	var agentIDs []string
	if agentList != "" {
		agentIDs = strings.Split(agentList, ",")
		for i := range agentIDs {
			agentIDs[i] = strings.TrimSpace(agentIDs[i])
		}
	}
	if tagFilter != "" {
		// 用 agent list + tag 过滤补充
		var resp struct {
			Agents []struct {
				AgentID string            `json:"agent_id"`
				Tags    map[string]string `json:"tags"`
			} `json:"agents"`
		}
		if err := apiGet(cfg, "/api/agents", &resp); err != nil {
			return fmt.Errorf("获取 Agent 列表失败: %w", err)
		}
		for _, a := range resp.Agents {
			if matchTag(a.Tags, tagFilter) {
				// 避免重复
				if !containsStr(agentIDs, a.AgentID) {
					agentIDs = append(agentIDs, a.AgentID)
				}
			}
		}
	}

	if len(agentIDs) == 0 {
		return fmt.Errorf("无匹配 Agent")
	}

	// 构造 payload
	var payload json.RawMessage
	var taskType string
	if scriptPath != "" {
		content, err := readFileContent(scriptPath)
		if err != nil {
			return fmt.Errorf("读取脚本失败: %w", err)
		}
		lang := "ps1"
		if strings.HasSuffix(scriptPath, ".sh") {
			lang = "bash"
		}
		taskType = "script"
		payload, _ = json.Marshal(struct {
			Lang    string `json:"lang"`
			Content string `json:"content"`
		}{Lang: lang, Content: content})
	} else {
		taskType = "powershell"
		payload, _ = json.Marshal(struct {
			Command string `json:"command"`
		}{Command: command})
	}

	fmt.Printf("🚀 批量执行: %d 个 Agent, 并行 %d\n", len(agentIDs), parallel)
	if command != "" {
		fmt.Printf("   命令: %s\n", command)
	} else {
		fmt.Printf("   脚本: %s\n", scriptPath)
	}
	fmt.Println()

	// 并行下发
	results := make(map[string]*execResult)
	resultMu := sync.Mutex{}
	sem := make(chan struct{}, parallel) // 并发限制
	var wg sync.WaitGroup

	for _, aid := range agentIDs {
		wg.Add(1)
		go func(agentID string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			res := execOnAgent(cfg, agentID, taskType, payload, timeout)
			resultMu.Lock()
			results[agentID] = res
			resultMu.Unlock()
		}(aid)
	}
	wg.Wait()

	// 汇总输出
	printExecResults(results)
	return nil
}

// execResult 单个 Agent 执行结果。
type execResult struct {
	AgentID string
	Success bool
	Stdout  string
	Stderr  string
	Error   string
	DurMS   int
}

// execOnAgent 在单个 Agent 上执行任务并等待结果。
func execOnAgent(cfg config, agentID, taskType string, payload json.RawMessage, timeout int) *execResult {
	taskID := "exec-" + agentID + "-" + fmt.Sprint(time.Now().UnixNano())
	body := taskSubmitBody{
		TaskID: taskID, Type: taskType, AgentID: agentID,
		Payload: payload, Timeout: timeout,
		// 多目标场景静默携带缓存的规则 ack(单目标打印由 task run/deploy 承担)
		RulesAck: resolveRulesAck(agentID, "", false),
	}
	var resp struct {
		TaskID string `json:"task_id"`
		Status string `json:"status"`
	}
	if err := apiPost(cfg, "/api/tasks", body, &resp); err != nil {
		return &execResult{AgentID: agentID, Error: err.Error()}
	}

	// 轮询等结果
	for i := 0; i < timeout+10; i++ {
		var r struct {
			Task *struct {
				Status string `json:"status"`
			} `json:"task"`
			Result *proto.TaskResult `json:"result"`
		}
		if err := apiGet(cfg, "/api/tasks/"+taskID, &r); err == nil && r.Task != nil {
			if r.Task.Status == proto.StatusCompleted || r.Task.Status == proto.StatusFailed {
				if r.Result != nil {
					return &execResult{
						AgentID: agentID,
						Success: r.Result.Success,
						Stdout:  r.Result.Stdout,
						Stderr:  r.Result.Stderr,
						Error:   r.Result.Error,
						DurMS:   r.Result.Duration,
					}
				}
				return &execResult{AgentID: agentID, Error: "无结果"}
			}
		}
		sleepDuration(1)
	}
	return &execResult{AgentID: agentID, Error: "超时"}
}

// printExecResults 汇总输出。
func printExecResults(results map[string]*execResult) {
	if jsonMode {
		data, _ := json.Marshal(results)
		fmt.Println(string(data))
		return
	}

	success := 0
	failed := 0
	for _, r := range results {
		if r.Success {
			success++
		} else {
			failed++
		}
	}

	fmt.Printf("=== 执行结果 (%d 成功, %d 失败) ===\n\n", success, failed)
	for aid, r := range results {
		status := "✅"
		if !r.Success {
			status = "❌"
		}
		fmt.Printf("%s %s (%dms)\n", status, aid, r.DurMS)
		if r.Stdout != "" {
			// 截断长输出(每个 Agent 最多显示 500 字符)
			out := r.Stdout
			if len(out) > 500 {
				out = out[:500] + "..."
			}
			fmt.Printf("   %s\n", strings.ReplaceAll(strings.TrimSpace(out), "\n", "\n   "))
		}
		if r.Error != "" {
			fmt.Printf("   ⚠️ %s\n", r.Error)
		}
		fmt.Println()
	}
}

// readFileContent 读取文件内容。
func readFileContent(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}
