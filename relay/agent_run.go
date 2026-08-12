// Package main — Agent 编排(Web 控制台自然语言任务 → LLM 规划 → 工具执行)。
//
// 2026-08-06:补齐"自然语言 → 工具调用"编排层。参照:
//   - agentloop/src/loop.ts:Think→Act→Observe 循环与事件协议
//   - go-agent-research/internal/planexec:Planner/Executor/Replanner 三接口 + 步骤模型
//
// 循环(Plan-and-Execute):
//   1. Planner:   LLM 生成步骤列表 {steps:[{goal,tool,args}]}(≤5 步,JSON 约束)
//   2. Executor:  逐 step 执行工具(内部调用 Relay 逻辑),结果回填
//   3. Replanner: 步骤失败时 LLM 重规划剩余步骤(≤2 次)
//   4. Summarizer: 全部完成后 LLM 流式汇总最终答案
//
// SSE 事件(与 /api/events 相同 data 包裹格式):
//   agent_start / agent_plan / step_start / tool_call / tool_result /
//   step_done / step_failed / agent_delta / agent_final / agent_error / agent_queued
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
	if err := r.authorize(req); err != nil {
		writeJSON(w, http.StatusUnauthorized, errMap(err))
		return
	}
	if r.llmClient == nil {
		writeJSON(w, http.StatusServiceUnavailable, errMap(fmt.Errorf("Agent 编排未启用: 未配置 LLM(需 -llm-base-url,如 http://127.0.0.1:11434/v1)")))
		return
	}
	var body struct {
		Task     string `json:"task"`
		AgentID  string `json:"agent_id,omitempty"`
		Type     string `json:"type,omitempty"`
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

	// 1. Planner:LLM 生成步骤列表(JSON 约束;失败重试 1 次,再失败转单步兜底)
	steps, err := r.agentPlan(ctx, run, schemas)
	if err != nil {
		// 解析失败兜底:单步 run_task(把整句当命令)
		log.Printf("[agent-run] %s 规划失败,兜底单步: %v", run.ID, err)
		steps = []AgentStep{{
			Index: 0, Goal: run.Task, Tool: "run_task",
			Args: map[string]any{"type": "powershell", "command": run.Task}, Status: "pending",
		}}
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
		msg, _, err := r.llmClient.ChatJSON(ctx, messages, schemas)
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
	msg, _, err := r.llmClient.ChatJSON(ctx, messages, schemas)
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
func agentPlanSystemPrompt(schemas []ToolDef, run *agentRun) string {
	var toolsDesc strings.Builder
	for _, t := range schemas {
		fmt.Fprintf(&toolsDesc, "- %s: %s\n", t.Function.Name, t.Function.Description)
	}
	agentHint := ""
	if run.UserAgent != "" {
		agentHint = fmt.Sprintf("用户指定目标 Agent: %s(工具参数中尽量使用它)\n", run.UserAgent)
	}
	return fmt.Sprintf(`你是 go-rmm 远程运维平台的智能 Agent。用户会用自然语言描述一个运维任务,你需要:
1. 把它拆解成 1-5 个可执行步骤
2. 每个步骤选择一个工具并给出参数

可用工具:
%s

%s
规则:
- 输出必须是合法 JSON,格式: {"steps":[{"goal":"步骤目标","tool":"工具名","args":{...}}]}
- 不要输出任何解释文字,只输出 JSON
- 远程执行命令用 run_task 或 shell_exec;不知道选哪个 Agent 先用 list_agents 或 scheduler_best
- args 必须符合工具参数要求;命令类工具的 command 参数给完整 PowerShell 命令`, toolsDesc.String(), agentHint)
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
