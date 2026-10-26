// Package main — MCP 工具注册表:curated 14 工具,逐个映射到 client SDK 导出方法。
// inputSchema 用 JSON Schema(object)描述,由 AI 客户端自动生成参数表单。
package main

import (
	"encoding/json"
	"fmt"

	"github.com/V-T-V/reins/client"
)

type mcpTool struct {
	Name        string
	Description string
	InputSchema map[string]any
	Handler     func(c *client.Client, args json.RawMessage) (any, error)
}

// schema 构造 object 型 JSON Schema 的简写。
func schema(props map[string]any, required ...string) map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": props,
		"required":   required,
	}
}

func prop(desc, typ string, def any) map[string]any {
	p := map[string]any{"type": typ, "description": desc}
	if def != nil {
		p["default"] = def
	}
	return p
}

func propEnum(desc string, def string, enum ...string) map[string]any {
	return map[string]any{"type": "string", "description": desc, "enum": enum, "default": def}
}

func propAny(desc string) map[string]any {
	return map[string]any{"type": "object", "description": desc}
}

var registry = []mcpTool{
	{
		Name:        "reins_list_agents",
		Description: "列出所有在线 Agent(id/主机名/系统/能力)。其他工具的 agent_id 参数从这里取。",
		InputSchema: schema(map[string]any{}),
		Handler: func(c *client.Client, _ json.RawMessage) (any, error) {
			return c.Agents().List()
		},
	},
	{
		Name:        "reins_status",
		Description: "Relay 健康状态(版本/存储后端/在线 Agent 数/uptime)。",
		InputSchema: schema(map[string]any{}),
		Handler: func(c *client.Client, _ json.RawMessage) (any, error) {
			return c.GetStatus()
		},
	},
	{
		Name:        "reins_run_command",
		Description: "在指定 Agent 上执行 PowerShell/shell 命令并返回 stdout。Windows 走 PowerShell -EncodedCommand,Linux 走 sh。",
		InputSchema: schema(map[string]any{
			"agent_id": prop("目标 Agent ID(先 reins_list_agents 查)", "string", nil),
			"command":  prop("要执行的命令", "string", nil),
			"timeout":  prop("超时秒数", "integer", 30),
		}, "agent_id", "command"),
		Handler: func(c *client.Client, args json.RawMessage) (any, error) {
			var a struct {
				AgentID string `json:"agent_id"`
				Command string `json:"command"`
				Timeout int    `json:"timeout"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, err
			}
			if a.Timeout <= 0 {
				a.Timeout = 30
			}
			out, err := c.RunCommand(a.AgentID, a.Command, a.Timeout)
			return map[string]any{"stdout": out}, err
		},
	},
	{
		Name:        "reins_task_run",
		Description: "下发任意类型任务(9 类执行器:powershell/script/file/screenshot/browse/guiclick/update/power/monitor)并等待结果。",
		InputSchema: schema(map[string]any{
			"agent_id":  prop("目标 Agent ID", "string", nil),
			"task_type": prop("任务类型", "string", nil),
			"payload":   propAny("任务载荷(如 {\"command\":\"Get-Date\"} 或 {\"url\":\"...\"})"),
			"timeout":   prop("超时秒数", "integer", 60),
		}, "agent_id", "task_type", "payload"),
		Handler: func(c *client.Client, args json.RawMessage) (any, error) {
			var a struct {
				AgentID  string          `json:"agent_id"`
				TaskType string          `json:"task_type"`
				Payload  json.RawMessage `json:"payload"`
				Timeout  int             `json:"timeout"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, err
			}
			if a.Timeout <= 0 {
				a.Timeout = 60
			}
			return c.Tasks().Run(a.AgentID, a.TaskType, a.Payload, a.Timeout)
		},
	},
	{
		Name:        "reins_task_get",
		Description: "查询单个任务的状态与结果。",
		InputSchema: schema(map[string]any{
			"task_id": prop("任务 ID", "string", nil),
		}, "task_id"),
		Handler: func(c *client.Client, args json.RawMessage) (any, error) {
			var a struct {
				TaskID string `json:"task_id"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, err
			}
			res, status, err := c.Tasks().Get(a.TaskID)
			return map[string]any{"status": status, "result": res}, err
		},
	},
	{
		Name:        "reins_task_list",
		Description: "列出最近的任务历史。",
		InputSchema: schema(map[string]any{
			"limit": prop("返回条数", "integer", 20),
		}),
		Handler: func(c *client.Client, args json.RawMessage) (any, error) {
			limit := 20
			var a struct {
				Limit int `json:"limit"`
			}
			if len(args) > 0 {
				if err := json.Unmarshal(args, &a); err != nil {
					return nil, err
				}
			}
			if a.Limit > 0 {
				limit = a.Limit
			}
			return c.Tasks().List(limit)
		},
	},
	{
		Name:        "reins_batch_run",
		Description: "按标签批量下发任务(一对多,并行投递),返回各目标 task_id。",
		InputSchema: schema(map[string]any{
			"tag":       prop("标签选择器(如 env=prod)", "string", nil),
			"task_type": prop("任务类型", "string", nil),
			"payload":   propAny("任务载荷"),
			"timeout":   prop("超时秒数", "integer", 60),
		}, "tag", "task_type", "payload"),
		Handler: func(c *client.Client, args json.RawMessage) (any, error) {
			var a struct {
				Tag      string          `json:"tag"`
				TaskType string          `json:"task_type"`
				Payload  json.RawMessage `json:"payload"`
				Timeout  int             `json:"timeout"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, err
			}
			if a.Timeout <= 0 {
				a.Timeout = 60
			}
			ids, err := c.Batch().RunByTag(a.Tag, a.TaskType, a.Payload, a.Timeout)
			return map[string]any{"task_ids": ids, "dispatched": len(ids)}, err
		},
	},
	{
		Name:        "reins_sysinfo",
		Description: "远端系统体检:主机名/OS/CPU 占用/内存总量与空闲。",
		InputSchema: schema(map[string]any{
			"agent_id": prop("目标 Agent ID", "string", nil),
		}, "agent_id"),
		Handler: func(c *client.Client, args json.RawMessage) (any, error) {
			var a struct {
				AgentID string `json:"agent_id"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, err
			}
			return c.Sysinfo(a.AgentID)
		},
	},
	{
		Name:        "reins_power",
		Description: "电源操作:关机/重启/休眠(受标签白名单与审计约束)。",
		InputSchema: schema(map[string]any{
			"action":   propEnum("电源动作", "restart", "shutdown", "restart", "sleep"),
			"agent_id": prop("目标 Agent ID", "string", nil),
			"delay":    prop("延迟秒数", "integer", 0),
		}, "action", "agent_id"),
		Handler: func(c *client.Client, args json.RawMessage) (any, error) {
			var a struct {
				Action  string `json:"action"`
				AgentID string `json:"agent_id"`
				Delay   int    `json:"delay"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, err
			}
			switch a.Action {
			case "shutdown":
				return "ok", c.Power().Shutdown(a.AgentID, a.Delay)
			case "restart":
				return "ok", c.Power().Restart(a.AgentID, a.Delay)
			case "sleep":
				return "ok", c.Power().Sleep(a.AgentID)
			default:
				return nil, fmt.Errorf("未知 action: %s", a.Action)
			}
		},
	},
	{
		Name:        "reins_cron_list",
		Description: "列出定时任务规则。",
		InputSchema: schema(map[string]any{}),
		Handler: func(c *client.Client, _ json.RawMessage) (any, error) {
			return c.Cron().List()
		},
	},
	{
		Name:        "reins_cron_add",
		Description: "添加定时任务规则(cron 表达式驱动)。",
		InputSchema: schema(map[string]any{
			"agent_id":  prop("目标 Agent ID", "string", nil),
			"cron":      prop("cron 表达式(5 段)", "string", nil),
			"task_type": prop("任务类型", "string", nil),
			"payload":   propAny("任务载荷"),
			"timeout":   prop("超时秒数", "integer", 60),
		}, "agent_id", "cron", "task_type", "payload"),
		Handler: func(c *client.Client, args json.RawMessage) (any, error) {
			var a struct {
				AgentID  string          `json:"agent_id"`
				Cron     string          `json:"cron"`
				TaskType string          `json:"task_type"`
				Payload  json.RawMessage `json:"payload"`
				Timeout  int             `json:"timeout"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, err
			}
			if a.Timeout <= 0 {
				a.Timeout = 60
			}
			return c.Cron().Add(a.AgentID, a.Cron, a.TaskType, a.Payload, a.Timeout)
		},
	},
	{
		Name:        "reins_cron_delete",
		Description: "删除定时任务规则。",
		InputSchema: schema(map[string]any{
			"rule_id": prop("规则 ID", "string", nil),
		}, "rule_id"),
		Handler: func(c *client.Client, args json.RawMessage) (any, error) {
			var a struct {
				RuleID string `json:"rule_id"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, err
			}
			return "ok", c.Cron().Delete(a.RuleID)
		},
	},
	{
		Name:        "reins_enroll_token",
		Description: "为目标 Agent 生成一次性注册令牌(轮换/重装场景)。",
		InputSchema: schema(map[string]any{
			"agent_id": prop("目标 Agent ID", "string", nil),
		}, "agent_id"),
		Handler: func(c *client.Client, args json.RawMessage) (any, error) {
			var a struct {
				AgentID string `json:"agent_id"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, err
			}
			tok, err := c.Agents().EnrollToken(a.AgentID)
			return map[string]any{"enroll_token": tok}, err
		},
	},
	{
		Name:        "reins_list_relay_tools",
		Description: "列出 Relay 侧全部 REST 工具清单(33+,含 MCP 未直连的隧道/文件传输等端点),供进一步编排。",
		InputSchema: schema(map[string]any{}),
		Handler: func(c *client.Client, _ json.RawMessage) (any, error) {
			return c.ListTools()
		},
	},
}

func findTool(name string) *mcpTool {
	for i := range registry {
		if registry[i].Name == name {
			return &registry[i]
		}
	}
	return nil
}

// toolDefs 输出 MCP tools/list 格式。
func toolDefs() []map[string]any {
	defs := make([]map[string]any, 0, len(registry))
	for _, t := range registry {
		defs = append(defs, map[string]any{
			"name":        t.Name,
			"description": t.Description,
			"inputSchema": t.InputSchema,
		})
	}
	return defs
}
