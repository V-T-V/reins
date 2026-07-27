// Package main — 工具发现端点(D2:供 AI Agent 自动发现 go-rmm 能力)。
//
// GET /api/tools 返回结构化工具清单,AI Agent 据此生成 function calling 定义。
// 无需鉴权(工具清单本身不泄漏敏感信息,便于 AI Agent 自动发现)。
package main

import "net/http"

// ToolEntry 工具描述。
type ToolEntry struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Method      string `json:"method"`
	Path        string `json:"path"`
	Category    string `json:"category"`
}

// allTools 全部可用工具(静态定义,与 router.go 路由同步)。
var allTools = []ToolEntry{
	// 诊断
	{"list_agents", "列出所有在线 Agent", "GET", "/api/agents", "diagnostics"},
	{"get_status", "Relay 健康状态", "GET", "/healthz", "diagnostics"},
	{"get_metrics", "Prometheus 指标", "GET", "/metrics", "diagnostics"},
	{"list_tools", "列出可用工具(此端点)", "GET", "/api/tools", "diagnostics"},

	// 执行
	{"run_task", "下发任务(powershell/script/file/...)并返回结果", "POST", "/api/tasks", "execution"},
	{"batch_task", "按标签批量下发任务", "POST", "/api/tasks/batch", "execution"},
	{"get_task", "查询单个任务状态和结果", "GET", "/api/tasks/{id}", "execution"},
	{"list_tasks", "列出任务历史", "GET", "/api/tasks", "execution"},
	{"cancel_task", "取消任务", "POST", "/api/tasks/cancel", "execution"},

	// Shell
	{"shell_open", "打开交互式终端会话", "POST", "/api/shell/open", "terminal"},
	{"shell_input", "向终端会话发送输入", "POST", "/api/shell/input", "terminal"},
	{"shell_close", "关闭终端会话", "POST", "/api/shell/close", "terminal"},

	// 文件传输
	{"file_upload", "上传文件到 Agent", "POST", "/api/transfer/upload", "transfer"},
	{"file_download", "从 Agent 下载文件", "POST", "/api/transfer/download", "transfer"},

	// 网络(隧道)
	{"tunnel_open", "开启端口转发隧道", "POST", "/api/tunnel/open", "network"},
	{"tunnel_data", "隧道数据传输", "POST", "/api/tunnel/data", "network"},
	{"tunnel_poll", "拉取隧道数据", "GET", "/api/tunnel/poll", "network"},
	{"tunnel_close", "关闭隧道", "POST", "/api/tunnel/close", "network"},

	// 电源
	{"power", "电源操作(shutdown/restart/sleep,需标签授权)", "POST", "/api/power", "management"},

	// 定时任务
	{"cron_list", "列出定时任务规则", "GET", "/api/crons", "scheduler"},
	{"cron_add", "添加定时任务规则", "POST", "/api/crons", "scheduler"},
	{"cron_delete", "删除定时任务规则", "DELETE", "/api/crons/{id}", "scheduler"},

	// Agent 管理
	{"revoke_agent", "撤销 Agent 证书", "POST", "/api/agents/revoke", "management"},
	{"enroll_token", "生成一次性注册令牌", "POST", "/api/enroll-token", "management"},
	{"scheduler_best", "智能选择最优 Agent(负载均衡)", "GET", "/api/scheduler/best", "management"},

	// 版本管理
	{"list_updates", "列出 Agent 版本仓库", "GET", "/api/updates", "update"},
	{"upload_update", "上传新 Agent 版本", "POST", "/api/updates/upload", "update"},
	{"apply_update", "触发 Agent 自更新", "POST", "/api/updates/apply", "update"},

	// Secret
	{"put_secret", "存储凭据(TTL+一次性)", "POST", "/api/secrets", "security"},
	{"get_secret", "拉取凭据", "GET", "/api/secrets/{ref}", "security"},
	{"delete_secret", "删除凭据", "DELETE", "/api/secrets/{ref}", "security"},

	// 审计
	{"export_audit", "导出审计日志(CEF/Syslog/JSON)", "GET", "/api/audit/export", "security"},

	// 事件流
	{"subscribe_events", "SSE 实时事件订阅(agent上线/任务结果/shell输出)", "GET", "/api/events", "streaming"},
}

// handleListTools GET /api/tools — 返回全部可用工具。
func (r *Relay) handleListTools(w http.ResponseWriter, req *http.Request) {
	// 无需鉴权(工具清单不泄漏敏感信息)
	writeJSON(w, http.StatusOK, map[string]any{
		"tools":    allTools,
		"count":    len(allTools),
		"version":  "v3.5",
		"base_url": req.Host,
	})
}
