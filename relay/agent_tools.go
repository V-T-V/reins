// Package main — Agent 编排工具集。
//
// 2026-08-06:Web 控制台 Agent 化任务执行的工具层。
// 工具执行**内部直接调用** Relay 逻辑(不 HTTP 自调用,避免嵌套开销),
// 统一 ToolResult 返回(错误永不抛出,回填友好信息让 LLM 修正重试)。
//
// 设计参照 agentloop/src/tools/registry.ts:
//   - ToolDef{Name, Description, Parameters(JSON Schema), Run}
//   - 工具错误永不抛出:返回 ToolResult{Ok:false, Summary:可读原因}
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/QiuShichang/go-rmm/proto"
)

// ToolResult 工具执行结果(统一返回,错误不抛出)。
type ToolResult struct {
	Ok      bool   `json:"ok"`
	Summary string `json:"summary"` // 人类可读摘要(截断)
	Data    any    `json:"data,omitempty"`
}

// AgentTool 编排工具定义。
type AgentTool struct {
	Name        string
	Description string
	Parameters  map[string]any // JSON Schema 子集
	Run         func(ctx context.Context, r *Relay, args map[string]any) ToolResult
}

// toolResultOk 构造成功结果(自动截断长输出)。
func toolResultOk(summary string, data any) ToolResult {
	return ToolResult{Ok: true, Summary: truncateTool(summary), Data: data}
}

// toolResultErr 构造失败结果(错误不抛出,回填给 LLM 修正)。
func toolResultErr(err error) ToolResult {
	return ToolResult{Ok: false, Summary: "工具执行失败: " + err.Error()}
}

// truncateTool 长输出截断(2KB),防 LLM 上下文爆炸。
func truncateTool(s string) string {
	const max = 2048
	if len(s) <= max {
		return s
	}
	return s[:max] + fmt.Sprintf("...[截断 %d 字节]", len(s)-max)
}

// jsonArgs 从 args 取字符串参数。
func jsonArgs(args map[string]any, key string) string {
	switch v := args[key].(type) {
	case string:
		return v
	case float64:
		return fmt.Sprintf("%.0f", v)
	case json.Number:
		return string(v)
	}
	return ""
}

// jsonArgsInt 从 args 取整数参数。
func jsonArgsInt(args map[string]any, key string, def int) int {
	switch v := args[key].(type) {
	case float64:
		return int(v)
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return int(n)
		}
	case string:
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	}
	return def
}

// agentTools 第一版工具集(8 个)。
func agentTools() []AgentTool {
	return []AgentTool{
		{
			Name:        "list_agents",
			Description: "列出所有在线 Agent(agent_id/hostname/os/version/capabilities)。执行远程任务前先查可用 Agent 及其能力。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{},
			},
			Run: func(ctx context.Context, r *Relay, args map[string]any) ToolResult {
				snap := r.agents.Snapshot()
				if len(snap) == 0 {
					return toolResultOk("没有在线 Agent", []any{})
				}
				agents := make([]map[string]any, 0, len(snap))
				for _, a := range snap {
					agents = append(agents, map[string]any{
						"agent_id":     a.AgentID,
						"hostname":     a.Hostname,
						"os":           a.OS,
						"version":      a.Version,
						"capabilities": a.Capabilities,
					})
				}
				return toolResultOk(fmt.Sprintf("%d 个在线 Agent", len(snap)), agents)
			},
		},
		{
			Name:        "scheduler_best",
			Description: "按负载均衡推荐最优 Agent(可选 type 能力过滤与 tag 标签过滤,如 tag=env=prod)。适合不知道选哪个 Agent 时。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"type": map[string]any{"type": "string", "description": "任务类型能力过滤,如 powershell"},
					"tag":  map[string]any{"type": "string", "description": "标签过滤,如 env=prod"},
				},
			},
			Run: func(ctx context.Context, r *Relay, args map[string]any) ToolResult {
				taskType := jsonArgs(args, "type")
				if taskType == "" {
					taskType = proto.TaskPowerShell
				}
				best, err := r.SelectBestAgent(jsonArgs(args, "tag"), taskType)
				if err != nil {
					return toolResultErr(err)
				}
				return toolResultOk(fmt.Sprintf("推荐 Agent: %s", best), map[string]any{"agent_id": best})
			},
		},
		{
			Name:        "run_task",
			Description: "在指定 Agent 上执行任务并等待终态结果。支持 powershell(给 command 字符串)、script(给 content,lang 默认 ps1)、screenshot、monitor 等。执行远程命令、查进程、查磁盘、跑脚本都用它。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"agent_id": map[string]any{"type": "string", "description": "目标 Agent ID(list_agents 或 scheduler_best 获取)"},
					"type":     map[string]any{"type": "string", "description": "任务类型: powershell/script/screenshot", "enum": []string{"powershell", "script", "screenshot"}},
					"command":  map[string]any{"type": "string", "description": "PowerShell 命令(type=powershell 时必填)"},
					"content":  map[string]any{"type": "string", "description": "脚本内容(type=script 时必填)"},
					"timeout":  map[string]any{"type": "integer", "description": "超时秒数,默认 60"},
				},
				"required": []string{"agent_id"},
			},
			Run: func(ctx context.Context, r *Relay, args map[string]any) ToolResult {
				agentID := jsonArgs(args, "agent_id")
				if agentID == "" {
					return toolResultErr(fmt.Errorf("agent_id 必填"))
				}
				taskType := jsonArgs(args, "type")
				if taskType == "" {
					taskType = proto.TaskPowerShell
				}
				if !proto.IsValidTaskType(taskType) {
					return toolResultErr(fmt.Errorf("未知任务类型 %s", taskType))
				}
				// 构建 payload
				var payload json.RawMessage
				switch taskType {
				case proto.TaskPowerShell:
					cmd := jsonArgs(args, "command")
					if cmd == "" {
						return toolResultErr(fmt.Errorf("type=powershell 需要 command"))
					}
					payload, _ = json.Marshal(proto.PowerShellTask{Command: cmd})
				case proto.TaskScript:
					content := jsonArgs(args, "content")
					if content == "" {
						return toolResultErr(fmt.Errorf("type=script 需要 content"))
					}
					payload, _ = json.Marshal(proto.ScriptTask{Lang: "ps1", Content: content})
				case proto.TaskScreenshot:
					payload, _ = json.Marshal(proto.ScreenshotTask{})
				default:
					return toolResultErr(fmt.Errorf("工具 run_task 暂不支持 type=%s", taskType))
				}
				return r.agentRunTask(ctx, agentID, taskType, payload, jsonArgsInt(args, "timeout", 60))
			},
		},
		{
			Name:        "get_task_result",
			Description: "查询任务结果(task_id 来自 run_task 返回)。任务已完成返回结果,未完成返回当前状态。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"task_id": map[string]any{"type": "string", "description": "任务 ID"},
				},
				"required": []string{"task_id"},
			},
			Run: func(ctx context.Context, r *Relay, args map[string]any) ToolResult {
				taskID := jsonArgs(args, "task_id")
				if taskID == "" {
					return toolResultErr(fmt.Errorf("task_id 必填"))
				}
				if r.store == nil {
					return toolResultErr(fmt.Errorf("任务持久化未启用(Relay 未配置 store)"))
				}
				st, err := r.store.GetTask(taskID)
				if err != nil || st == nil {
					return toolResultErr(fmt.Errorf("任务 %s 不存在", taskID))
				}
				res, _ := r.store.GetResult(taskID)
				if res != nil {
					return toolResultOk(fmt.Sprintf("任务 %s 状态=%s 成功=%v stdout=%q",
						taskID, st.Status, res.Success, truncateTool(res.Stdout)), map[string]any{
						"task_id": taskID, "status": st.Status, "success": res.Success,
						"stdout": res.Stdout, "exit_code": res.ExitCode,
					})
				}
				return toolResultOk(fmt.Sprintf("任务 %s 状态=%s(尚未完成)", taskID, st.Status), map[string]any{
					"task_id": taskID, "status": st.Status,
				})
			},
		},
		{
			Name:        "run_script",
			Description: "运行脚本库中的脚本(Relay 已存储的 ps1/bat/sh)。返回执行结果。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":     map[string]any{"type": "string", "description": "脚本名(如 deploy.ps1)"},
					"agent_id": map[string]any{"type": "string", "description": "目标 Agent ID"},
					"timeout":  map[string]any{"type": "integer", "description": "超时秒数,默认 60"},
				},
				"required": []string{"name", "agent_id"},
			},
			Run: func(ctx context.Context, r *Relay, args map[string]any) ToolResult {
				name := jsonArgs(args, "name")
				agentID := jsonArgs(args, "agent_id")
				if name == "" || agentID == "" {
					return toolResultErr(fmt.Errorf("name/agent_id 必填"))
				}
				if scriptLib == nil {
					return toolResultErr(fmt.Errorf("脚本库未启用"))
				}
				content, err := scriptLib.Read(name)
				if err != nil {
					return toolResultErr(fmt.Errorf("脚本 %s 不存在: %v", name, err))
				}
				lang := jsonArgs(args, "lang")
				if lang == "" {
					lang = "ps1"
				}
				payload, _ := json.Marshal(proto.ScriptTask{Lang: lang, Content: content})
				return r.agentRunTask(ctx, agentID, proto.TaskScript, payload, jsonArgsInt(args, "timeout", 60))
			},
		},
		{
			Name:        "shell_exec",
			Description: "在指定 Agent 上执行一条 PowerShell 命令并立即返回结果(与 run_task 的 powershell 等价,快捷入口)。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"agent_id": map[string]any{"type": "string", "description": "目标 Agent ID"},
					"command":  map[string]any{"type": "string", "description": "PowerShell 命令"},
					"timeout":  map[string]any{"type": "integer", "description": "超时秒数,默认 60"},
				},
				"required": []string{"agent_id", "command"},
			},
			Run: func(ctx context.Context, r *Relay, args map[string]any) ToolResult {
				agentID := jsonArgs(args, "agent_id")
				cmd := jsonArgs(args, "command")
				if agentID == "" || cmd == "" {
					return toolResultErr(fmt.Errorf("agent_id/command 必填"))
				}
				payload, _ := json.Marshal(proto.PowerShellTask{Command: cmd})
				return r.agentRunTask(ctx, agentID, proto.TaskPowerShell, payload, jsonArgsInt(args, "timeout", 60))
			},
		},
		{
			Name:        "list_tasks",
			Description: "列出最近任务(默认 10 条,含状态与结果摘要)。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"limit": map[string]any{"type": "integer", "description": "返回条数,默认 10"},
				},
			},
			Run: func(ctx context.Context, r *Relay, args map[string]any) ToolResult {
				if r.store == nil {
					return toolResultErr(fmt.Errorf("任务持久化未启用"))
				}
				limit := jsonArgsInt(args, "limit", 10)
				if limit > 50 {
					limit = 50
				}
				tasks, err := r.store.ListTasks(limit)
				if err != nil {
					return toolResultErr(err)
				}
				out := make([]map[string]any, 0, len(tasks))
				for _, t := range tasks {
					out = append(out, map[string]any{
						"task_id": t.Request.TaskID, "type": t.Request.Type,
						"agent_id": t.Request.AgentID, "status": t.Status,
					})
				}
				return toolResultOk(fmt.Sprintf("%d 条任务", len(out)), out)
			},
		},
		{
			Name:        "read_agent_config",
			Description: "读取远端 Agent 配置关键字段(allowed_roots 白名单/workspace/role 等),排查文件路径被拒问题。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"agent_id": map[string]any{"type": "string", "description": "目标 Agent ID"},
				},
				"required": []string{"agent_id"},
			},
			Run: func(ctx context.Context, r *Relay, args map[string]any) ToolResult {
				agentID := jsonArgs(args, "agent_id")
				if agentID == "" {
					return toolResultErr(fmt.Errorf("agent_id 必填"))
				}
				// 通过远端命令读取 config.json(Agent 默认 <exeDir>/config.json)
				script := `$p = @("$env:ProgramFiles\go-rmm\config.json","C:\go-rmm\config.json","$env:ProgramFiles\go-rmm-master\config.json") | Where-Object { Test-Path $_ } | Select-Object -First 1; if ($p) { Get-Content $p -Raw } else { 'NOT_FOUND' }`
				payload, _ := json.Marshal(proto.PowerShellTask{Command: script})
				res := r.agentRunTask(ctx, agentID, proto.TaskPowerShell, payload, 30)
				if !res.Ok {
					return res
				}
				var cfgJSON map[string]any
				if err := json.Unmarshal([]byte(res.Summary), &cfgJSON); err != nil {
					return toolResultOk("远端配置不存在或非 JSON(仅读取 Agent 默认位置)", map[string]any{"raw": res.Summary})
				}
				keys := []string{"agent_id", "relay_url", "role", "workspace", "allowed_roots", "allow_all_roots", "max_concurrent_tasks"}
				subset := map[string]any{}
				for _, k := range keys {
					if v, ok := cfgJSON[k]; ok {
						subset[k] = v
					}
				}
				return toolResultOk(fmt.Sprintf("Agent %s 配置: %s", agentID, truncateTool(res.Summary)), subset)
			},
		},
	}
}

// agentRunTask 下发任务并等待终态结果(工具 run_task/shell_exec/run_script 共用)。
// 流程:构造 TaskRequest → SaveTask(pending) → dispatchTask → 轮询 GetResult(≤timeout)。
func (r *Relay) agentRunTask(ctx context.Context, agentID, taskType string, payload json.RawMessage, timeout int) ToolResult {
	conn := r.agents.Conn(agentID)
	if conn == nil {
		return toolResultErr(fmt.Errorf("Agent %s 不在线", agentID))
	}
	if r.store == nil {
		return toolResultErr(fmt.Errorf("任务持久化未启用(Relay 需配置 store)"))
	}
	if timeout <= 0 {
		timeout = 60
	}
	if timeout > 600 {
		timeout = 600
	}
	// 安全(审查修复):与 /api/tasks POST 一致复用命令策略黑名单。
	// 工具 run_task/run_script/shell_exec 的命令由 LLM 生成(可被提示注入),
	// 若不在此拦截,高危命令可绕过 handlePostTask 的策略检查直接投递。
	if err := r.policy.CheckPayload(taskType, payload); err != nil {
		return toolResultErr(fmt.Errorf("命令被策略拦截: %v", err))
	}
	taskID := "agt-" + strings.Replace(uuid.NewString()[:8], "-", "", -1) + "-" + fmt.Sprint(time.Now().UnixMilli())
	task := proto.TaskRequest{
		TaskID:  taskID,
		Type:    taskType,
		AgentID: agentID,
		Payload: payload,
		Timeout: timeout,
	}
	if err := r.store.SaveTask(StoredTask{Request: task, Status: proto.StatusPending, CreatedAt: time.Now().UnixMilli()}); err != nil {
		return toolResultErr(fmt.Errorf("保存任务失败: %v", err))
	}
	if err := r.dispatchTask(conn, task); err != nil {
		return toolResultErr(fmt.Errorf("投递任务失败: %v", err))
	}

	// 轮询结果(1s ticker,≤timeout+5s)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	deadline := time.After(time.Duration(timeout+5) * time.Second)
	for {
		select {
		case <-ctx.Done():
			return toolResultErr(ctx.Err())
		case <-deadline:
			return toolResultErr(fmt.Errorf("任务 %s 等待结果超时(状态可能仍进行中)", taskID))
		case <-ticker.C:
			res, err := r.store.GetResult(taskID)
			if err == nil && res != nil {
				if res.Success {
					out := res.Stdout
					if out == "" {
						out = res.Error
					}
					if out == "" {
						out = "(无输出)"
					}
					return toolResultOk(fmt.Sprintf("任务 %s 成功(exit=%d): %s", taskID, res.ExitCode, out), map[string]any{
						"task_id": taskID, "success": true, "stdout": res.Stdout, "exit_code": res.ExitCode,
					})
				}
				errMsg := res.Error
				if errMsg == "" {
					errMsg = res.Stderr
				}
				if errMsg == "" {
					errMsg = "任务执行失败(exit=" + fmt.Sprint(res.ExitCode) + ")"
				}
				return toolResultErr(fmt.Errorf("任务 %s 失败: %s", taskID, errMsg))
			}
		}
	}
}

// agentToolSchemas 把工具表转成 LLM function calling 的 ToolDef 列表。
func agentToolSchemas() []ToolDef {
	tools := agentTools()
	out := make([]ToolDef, 0, len(tools))
	for _, t := range tools {
		out = append(out, ToolDef{
			Type: "function",
			Function: ToolFunctionDef{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Parameters,
			},
		})
	}
	return out
}

// findAgentTool 按名字查找工具。
func findAgentTool(name string) (*AgentTool, bool) {
	for i := range agentToolRegistry {
		if agentToolRegistry[i].Name == name {
			return &agentToolRegistry[i], true
		}
	}
	return nil, false
}

// agentToolRegistry 工具表缓存(避免每次调用重建)。
var agentToolRegistry = agentTools()
