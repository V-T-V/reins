// Package main — Agent 编排(Web 控制台自然语言任务 → LLM 规划 → 工具执行)。
//
// 2026-08-06:补齐"自然语言 → 工具调用"编排层。参照:
//   - agentloop/src/loop.ts:Think→Act→Observe 循环与事件协议
//   - go-agent-research/internal/planexec:Planner/Executor/Replanner 三接口 + 步骤模型
//
// 循环(Plan-and-Execute):
//  1. Planner:   LLM 生成步骤列表 {steps:[{goal,tool,args}]}(≤5 步,JSON 约束)
//  2. Executor:  逐 step 执行工具(内部调用 Relay 逻辑),结果回填
//  3. Replanner: 步骤失败时 LLM 重规划剩余步骤(≤2 次)
//  4. Summarizer: 全部完成后 LLM 流式汇总最终答案
//
// SSE 事件(与 /api/events 相同 data 包裹格式):
//
//	agent_start / agent_plan / step_start / tool_call / tool_result /
//	step_done / step_failed / agent_delta / agent_final / agent_error / agent_queued
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// AgentStep 编排计划中的一步。
type AgentStep struct {
	Index  int            `json:"index"`
	Goal   string         `json:"goal"`   // 自然语言目标
	Tool   string         `json:"tool"`   // 工具名
	Args   map[string]any `json:"args"`   // 工具参数
	Status string         `json:"status"` // pending/running/done/failed/replanned
	Result string         `json:"result,omitempty"`
}

// agentRun 一次编排运行的会话状态。
type agentRun struct {
	ID         string
	Task       string
	UserAgent  string // 用户可选指定 Agent(空=工具自行选)
	UserType   string // 用户可选限定任务类型
	Steps      []AgentStep
	ReplanLeft int
}

// agentPlanResponse LLM 规划输出的 JSON 结构。
type agentPlanResponse struct {
	Steps []struct {
		Goal string         `json:"goal"`
		Tool string         `json:"tool"`
		Args map[string]any `json:"args"`
	} `json:"steps"`
}

// handleAgentRun POST /api/agent/run —— Agent 编排(SSE 流式)。
// 请求体:{"task": "自然语言任务", "agent_id"?: 可选, "type"?: 可选}
func (r *Relay) handleAgentRun(w http.ResponseWriter, req *http.Request) {
	if r.llmClient == nil {
		writeJSON(w, http.StatusServiceUnavailable, errMap(fmt.Errorf("Agent 编排未启用: 未配置 LLM(需 -llm-base-url,如 http://127.0.0.1:11434/v1)")))
		return
	}
	var body struct {
		Task    string `json:"task"`
		AgentID string `json:"agent_id,omitempty"`
		Type    string `json:"type,omitempty"`
	}
	if err := decodeJSON(req, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, errMap(err))
		return
	}
	if strings.TrimSpace(body.Task) == "" {
		writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("task 不能为空")))
		return
	}

	// SSE 响应(禁缓冲,与 handleSSE 一致)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	run := &agentRun{
		ID:         "ar-" + uuid.NewString()[:8],
		Task:       body.Task,
		UserAgent:  body.AgentID,
		UserType:   body.Type,
		ReplanLeft: 2,
	}
	ctx, cancel := context.WithTimeout(req.Context(), 5*time.Minute)
	defer cancel()

	send := func(event string, payload any) {
		data, _ := json.Marshal(payload)
		outer, _ := json.Marshal(map[string]any{
			"agent_run_id": run.ID,
			"data":         json.RawMessage(data),
			"ts":           time.Now().UnixMilli(),
		})
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, outer)
		if flusher != nil {
			flusher.Flush()
		}
	}

	// 并发信号量:空闲立即通过;满则先发 queued 事件再等待(客户端可见排队)
	select {
	case r.agentRunSem <- struct{}{}:
		// 直接通过
	default:
		send("agent_queued", map[string]any{"run_id": run.ID, "status": "queued"})
		select {
		case r.agentRunSem <- struct{}{}:
		case <-ctx.Done():
			return
		case <-req.Context().Done():
			return
		}
	}
	defer func() { <-r.agentRunSem }()

	send("agent_start", map[string]any{
		"run_id": run.ID, "task": run.Task,
		"agent_id": run.UserAgent, "type": run.UserType,
		"model": r.llmClient.Model,
	})

	// --- 编排循环 ---
	finalErr := r.runAgentLoop(ctx, run, send)

	if finalErr != nil {
		log.Printf("[agent-run] %s 失败: %v", run.ID, finalErr)
		send("agent_error", map[string]any{"run_id": run.ID, "error": finalErr.Error()})
		return
	}
	// 汇总由 runAgentLoop 内部流式发送 agent_delta + agent_final
}

// runAgentLoop 编排主循环:plan → execute(+replan) → summarize。
// 返回非 nil 表示编排失败(已发 agent_error 由调用方处理)。
func (r *Relay) runAgentLoop(ctx context.Context, run *agentRun, send func(event string, payload any)) error {
	schemas := agentToolSchemas()

	// 1. Planner:LLM 生成步骤列表(JSON 约束;失败重试 1 次)。
	steps, err := r.agentPlan(ctx, run, schemas)
	if err != nil {
		// 规划失败 = LLM 不可用(如 401 鉴权/未配置/网络)。
		// 不再兜底"把 task 当 powershell 命令"(自然语言当命令永远失败且误导);
		// 直接报错让用户看到真实原因(去配 LLM key / 检查 base-url)。
		log.Printf("[agent-run] %s 规划失败: %v", run.ID, err)
		errMsg := err.Error()
		if strings.Contains(errMsg, "401") || strings.Contains(strings.ToLower(errMsg), "auth") {
			errMsg = "LLM 鉴权失败(401)—— 请检查 -llm-api-key 是否正确配置"
		} else if strings.Contains(errMsg, "未配置 LLM") {
			errMsg = "Agent 编排未启用 —— 服务器需配置 -llm-base-url(如 https://api.deepseek.com/v1)"
		}
		return fmt.Errorf("LLM 规划失败: %s", errMsg)
	}
	if len(steps) == 0 {
		return fmt.Errorf("LLM 未生成任何步骤")
	}
	run.Steps = steps
	send("agent_plan", map[string]any{"run_id": run.ID, "steps": run.Steps})

	// 2. Executor:逐 step 执行(失败 → Replanner ≤2 次)
	history := r.agentPlanHistory(run, schemas) // 规划上下文(含失败反馈重规划时追加)
	for i := 0; i < len(run.Steps); {
		step := &run.Steps[i]
		if ctx.Err() != nil {
			return ctx.Err()
		}
		step.Status = "running"
		send("step_start", map[string]any{"run_id": run.ID, "index": step.Index, "goal": step.Goal, "tool": step.Tool})

		// 工具调用(脱敏参数:命令截断)
		send("tool_call", map[string]any{
			"run_id": run.ID, "index": step.Index, "tool": step.Tool,
			"args": truncateToolArgs(step.Args),
		})
		res := r.agentExecute(ctx, step.Tool, step.Args)
		send("tool_result", map[string]any{
			"run_id": run.ID, "index": step.Index, "tool": step.Tool,
			"ok": res.Ok, "summary": res.Summary,
		})

		if res.Ok {
			step.Status = "done"
			step.Result = res.Summary
			send("step_done", map[string]any{"run_id": run.ID, "index": step.Index, "status": "done"})
			i++
			continue
		}

		// 失败 → Replanner(≤2 次)
		step.Status = "failed"
		step.Result = res.Summary
		send("step_failed", map[string]any{
			"run_id": run.ID, "index": step.Index, "error": res.Summary,
		})
		if run.ReplanLeft <= 0 {
			return fmt.Errorf("步骤 %d(%s) 失败且重规划耗尽: %s", step.Index, step.Tool, res.Summary)
		}
		run.ReplanLeft--
		newSteps, rerr := r.agentReplan(ctx, run, schemas, step, history)
		if rerr != nil {
			return fmt.Errorf("步骤 %d 失败且重规划失败: %v(原始: %s)", step.Index, rerr, res.Summary)
		}
		if len(newSteps) == 0 {
			return fmt.Errorf("步骤 %d 失败且重规划返回空: %s", step.Index, res.Summary)
		}
		// 替换剩余步骤:失败步骤标记 replanned,后续换成新计划。
		step.Status = "replanned"
		run.Steps = append(run.Steps[:i+1], newSteps...)
		// 新步骤序号续排 + 重置状态(直接改 run.Steps 里的副本)。
		for j := i + 1; j < len(run.Steps); j++ {
			run.Steps[j].Index = j
			run.Steps[j].Status = "pending"
		}
		send("agent_plan", map[string]any{"run_id": run.ID, "steps": run.Steps, "replanned": true})
		i++ // 跳过 replanned 的失败步骤,继续执行新步骤
	}

	// 3. Summarizer:LLM 流式汇总
	return r.agentSummarize(ctx, run, send)
}

// agentPlan Planner:LLM 生成步骤列表(JSON 约束,重试 1 次)。
func (r *Relay) agentPlan(ctx context.Context, run *agentRun, schemas []ToolDef) ([]AgentStep, error) {
	messages := []ChatMessage{
		{Role: "system", Content: agentPlanSystemPrompt(schemas, run)},
		{Role: "user", Content: run.Task},
	}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		// tools=nil:规划走 system prompt 描述工具 + response_format=json_object,
		// 避免与 V4 原生 function calling 冲突(否则 V4 强 tool-use 倾向返回 tool_calls 而非计划 JSON)。
		// schemas 仅用于在 prompt 文本里描述工具清单(下方 agentPlanSystemPrompt)。
		msg, _, err := r.llmClient.ChatJSON(ctx, messages, nil)
		if err != nil {
			lastErr = err
			continue
		}
		steps, perr := parseAgentPlan(msg.Content)
		if perr != nil {
			lastErr = perr
			continue
		}
		return steps, nil
	}
	return nil, lastErr
}

// agentReplan Replanner:注入失败反馈,LLM 重新规划剩余步骤。
func (r *Relay) agentReplan(ctx context.Context, run *agentRun, schemas []ToolDef, failed *AgentStep, history []ChatMessage) ([]AgentStep, error) {
	prompt := fmt.Sprintf(
		"原任务: %s\n\n步骤 %d(%s) 执行失败: %s\n\n"+
			"请重新规划剩余步骤(不要重复失败的步骤,换一种方式完成目标),只输出 JSON: {\"steps\":[{\"goal\",\"tool\",\"args\"}]}",
		run.Task, failed.Index, failed.Tool, failed.Result)
	messages := append(append([]ChatMessage{}, history...), ChatMessage{Role: "user", Content: prompt})
	// tools=nil:同 agentPlan,规划走 prompt + json_object,避免与 V4 function calling 冲突。
	msg, _, err := r.llmClient.ChatJSON(ctx, messages, nil)
	if err != nil {
		return nil, err
	}
	return parseAgentPlan(msg.Content)
}

// agentSummarize Summarizer:LLM 流式汇总所有步骤结果。
func (r *Relay) agentSummarize(ctx context.Context, run *agentRun, send func(event string, payload any)) error {
	var sb strings.Builder
	for _, s := range run.Steps {
		if s.Status != "done" && s.Status != "replanned" {
			continue
		}
		if s.Status == "done" {
			fmt.Fprintf(&sb, "步骤%d [%s] %s: %s\n", s.Index, s.Tool, s.Goal, truncateTool(s.Result))
		}
	}
	messages := []ChatMessage{
		{Role: "system", Content: "你是 go-rmm 远程运维平台的结果汇报员。请用简洁中文总结以下各步骤的执行结果,指出哪些成功、哪些失败,给出关键数据。不要编造步骤之外的信息。"},
		{Role: "user", Content: fmt.Sprintf("原始任务: %s\n\n执行记录:\n%s", run.Task, sb.String())},
	}
	msg, _, err := r.llmClient.ChatStream(ctx, messages, nil, func(delta string) {
		send("agent_delta", map[string]any{"run_id": run.ID, "delta": delta})
	})
	if err != nil {
		return err
	}
	send("agent_final", map[string]any{"run_id": run.ID, "answer": msg.Content})
	return nil
}

// agentExecute 执行工具(错误不抛出,返回 ToolResult)。
func (r *Relay) agentExecute(ctx context.Context, toolName string, args map[string]any) ToolResult {
	tool, ok := findAgentTool(toolName)
	if !ok {
		return toolResultErr(fmt.Errorf("未知工具 %s(可用: list_agents scheduler_best run_task get_task_result run_script shell_exec list_tasks read_agent_config)", toolName))
	}
	return tool.Run(ctx, r, args)
}

// parseAgentPlan 解析 LLM 规划 JSON(剥 markdown 围栏容错)。
func parseAgentPlan(content string) ([]AgentStep, error) {
	s := strings.TrimSpace(content)
	// 剥 ```json ... ``` 围栏
	if strings.HasPrefix(s, "```") {
		if idx := strings.Index(s, "\n"); idx >= 0 {
			s = s[idx+1:]
		}
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	}
	var plan agentPlanResponse
	if err := json.Unmarshal([]byte(s), &plan); err != nil {
		return nil, fmt.Errorf("规划 JSON 解析失败: %v", err)
	}
	if len(plan.Steps) == 0 {
		return nil, fmt.Errorf("规划 JSON 无 steps")
	}
	if len(plan.Steps) > 5 {
		plan.Steps = plan.Steps[:5]
	}
	steps := make([]AgentStep, 0, len(plan.Steps))
	for i, st := range plan.Steps {
		if strings.TrimSpace(st.Tool) == "" {
			continue
		}
		steps = append(steps, AgentStep{
			Index: i, Goal: st.Goal, Tool: st.Tool,
			Args: st.Args, Status: "pending",
		})
	}
	if len(steps) == 0 {
		return nil, fmt.Errorf("规划 JSON 无有效步骤")
	}
	return steps, nil
}

// agentPlanHistory 构造规划上下文(供 Replanner 使用)。
func (r *Relay) agentPlanHistory(run *agentRun, schemas []ToolDef) []ChatMessage {
	return []ChatMessage{
		{Role: "system", Content: agentPlanSystemPrompt(schemas, run)},
		{Role: "user", Content: run.Task},
	}
}

// agentPlanSystemPrompt 规划系统提示词(含工具清单,约束 JSON 输出)。
// 针对 DeepSeek V4(强 agentic / tool-use)优化:结构化工具选择决策树 + 反幻觉规则。
func agentPlanSystemPrompt(schemas []ToolDef, run *agentRun) string {
	var toolsDesc strings.Builder
	for _, t := range schemas {
		fmt.Fprintf(&toolsDesc, "- %s: %s\n", t.Function.Name, t.Function.Description)
	}
	agentHint := ""
	if run.UserAgent != "" {
		agentHint = fmt.Sprintf("用户指定目标 Agent: %s(工具参数中尽量使用它)\n", run.UserAgent)
	}
	return fmt.Sprintf(`你是 go-rmm 远程运维平台的智能 Agent。用户用自然语言描述运维任务,你把它拆解成 1-5 个可执行步骤,每步选一个工具并给出参数。

【可用工具】
%s

%s
【意图 → 工具映射(最重要:先识别用户意图,再选工具)】
- "列出/查看/有哪些 agent/客户端/机器" → list_agents(无需参数)
- "推荐/选哪个 agent" → scheduler_best(可给 type/tag 过滤)
- "在 agent 上跑命令/查进程/查磁盘/查系统信息/执行 PowerShell" → run_task(type=powershell, command=具体命令) 或 shell_exec
- "查任务结果/任务怎么样了" → get_task_result(task_id) 或 list_tasks
- "读 agent 配置/路径/allowed_roots" → read_agent_config
- "跑脚本库的 xxx 脚本" → run_script(name)
⚠️ 关键:用户的自然语言描述(如"列出在线 agent")是【任务意图】,不是 PowerShell 命令本身 —— 绝不能把它原样塞进 command 参数。command 必须是真实可执行的 PowerShell 代码(如 Get-Process、Get-PSDrive)。"列出 agent"这种意图对应 list_agents 工具,不是 run_task。

【run_task / shell_exec 的 command 参数示例】(只能是真实 PowerShell)
- 查进程: Get-Process | Select-Object Name,Id,CPU -First 10
- 查磁盘: Get-PSDrive -PSProvider FileSystem | Select-Object Name,Used,Free
- 查系统信息: Get-ComputerInfo | Select-Object OsName,OsVersion,CsProcessors
- 查 IP: Get-NetIPAddress -AddressFamily IPv4 | Select-Object IPAddress,InterfaceAlias

【输出规则】
- 必须是合法 JSON:{"steps":[{"goal":"步骤目标","tool":"工具名","args":{...}}]}
- 只输出 JSON,不要任何解释、不要 Markdown 围栏
- 步骤数 1-5,能一步完成就一步,宁少勿多
- args 必须符合工具参数 schema;不要编造工具名;不要塞工具未定义的字段
- run_task 必须给 agent_id(若用户没指定,先 list_agents 拿到 agent_id 再用,或让 scheduler_best 推荐)
- 涉及多 Agent 时每步明确 agent_id`, toolsDesc.String(), agentHint)
}

// truncateToolArgs 参数脱敏(命令截断,防日志/SSE 泄敏感长命令)。
func truncateToolArgs(args map[string]any) map[string]any {
	out := make(map[string]any, len(args))
	for k, v := range args {
		if s, ok := v.(string); ok && len(s) > 200 {
			out[k] = s[:200] + "...[截断]"
			continue
		}
		out[k] = v
	}
	return out
}
