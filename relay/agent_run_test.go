// relay/agent_run_test.go
// M2:Agent 编排测试——完整流程(plan→execute→summarize)、失败重规划、
// 未配置 LLM 拒绝、规划 JSON 容错、工具错误不抛出。
// 全部用 StubLLM 脚本式 mock,不依赖真实模型。
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// startAgentRelay 启动带 StubLLM 的测试 Relay(可注入 handler)。
func startAgentRelay(t *testing.T, handler func(messages []ChatMessage, tools []ToolDef) (ChatMessage, error)) (*httptest.Server, *Relay, *StubLLM) {
	t.Helper()
	stub := NewStubLLM(handler)
	relay := NewRelay(Config{Dev: true, Addr: ":0", LLMBaseURL: "http://stub"})
	relay.llmClient = stubToClient(stub)
	relay.WithStore(NewMemoryStore())
	mux := http.NewServeMux()
	relay.registerHTTP(mux)
	mux.HandleFunc("/agent", relay.handleAgentWS)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, relay, stub
}

// agentSSEEvent SSE 事件(kind + 解析后的 data payload)。
type agentSSEEvent struct {
	Kind string
	Data map[string]any
}

// agentRunSSE 提交自然语言任务,返回事件流(kind → data 顺序)。
func agentRunSSE(t *testing.T, ctx context.Context, srv *httptest.Server, task string) []agentSSEEvent {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"task": task})
	req, _ := http.NewRequestWithContext(ctx, "POST", srv.URL+"/api/agent/run", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /api/agent/run: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("期望 200, got %d", resp.StatusCode)
	}

	var events []agentSSEEvent
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	var pendingKind string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event: ") {
			pendingKind = strings.TrimSpace(strings.TrimPrefix(line, "event: "))
			continue
		}
		if strings.HasPrefix(line, "data: ") && pendingKind != "" {
			var outer struct {
				Data json.RawMessage `json:"data"`
			}
			json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &outer)
			var payload map[string]any
			json.Unmarshal(outer.Data, &payload)
			events = append(events, agentSSEEvent{pendingKind, payload})
			pendingKind = ""
		}
	}
	return events
}

// eventKinds 提取事件序列(kind 列表)。
func eventKinds(events []agentSSEEvent) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e.Kind)
	}
	return out
}

// hasEvent 判断事件序列是否包含指定 kind。
func hasEvent(events []agentSSEEvent, kind string) bool {
	for _, e := range events {
		if e.Kind == kind {
			return true
		}
	}
	return false
}

// TestAgentRunHappyPath M2 核心:完整流程 plan→execute→summarize,事件序列正确。
func TestAgentRunHappyPath(t *testing.T) {
	var calls int
	srv, _, _ := startAgentRelay(t, func(messages []ChatMessage, tools []ToolDef) (ChatMessage, error) {
		calls++
		switch calls {
		case 1: // 规划:单步 list_agents
			return ChatMessage{Content: `{"steps":[{"goal":"查看在线 Agent","tool":"list_agents","args":{}}]}`}, nil
		default: // 汇总
			return ChatMessage{Content: "共 0 个在线 Agent,无可用主机"}, nil
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	events := agentRunSSE(t, ctx, srv, "看看有哪些 Agent 在线")

	kinds := eventKinds(events)
	// 事件顺序:start → plan → step_start → tool_call → tool_result → step_done → delta → final
	joined := strings.Join(kinds, ",")
	for _, want := range []string{"agent_start", "agent_plan", "step_start", "tool_call", "tool_result", "step_done", "agent_delta", "agent_final"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("事件序列缺少 %s: %s", want, joined)
		}
	}
	// plan 含步骤
	for _, e := range events {
		if e.Kind == "agent_plan" {
			steps, ok := e.Data["steps"].([]any)
			if !ok || len(steps) != 1 {
				t.Fatalf("agent_plan steps 异常: %v", e.Data["steps"])
			}
		}
		if e.Kind == "agent_final" {
			if e.Data["answer"] != "共 0 个在线 Agent,无可用主机" {
				t.Fatalf("最终答案异常: %v", e.Data["answer"])
			}
		}
	}
}

// TestAgentRunReplan M2:第一步工具失败 → Replanner 重规划 → 新步骤成功。
func TestAgentRunReplan(t *testing.T) {
	var calls int
	srv, _, _ := startAgentRelay(t, func(messages []ChatMessage, tools []ToolDef) (ChatMessage, error) {
		calls++
		switch calls {
		case 1: // 规划:第一步用不存在的工具 → 执行失败
			return ChatMessage{Content: `{"steps":[{"goal":"用不存在的工具","tool":"nonexistent_tool","args":{}}]}`}, nil
		case 2: // 重规划:改用 list_agents
			return ChatMessage{Content: `{"steps":[{"goal":"改为查 Agent","tool":"list_agents","args":{}}]}`}, nil
		default: // 汇总
			return ChatMessage{Content: "重规划后成功"}, nil
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	events := agentRunSSE(t, ctx, srv, "测试重规划")

	joined := strings.Join(eventKinds(events), ",")
	for _, want := range []string{"step_failed", "agent_plan", "step_done", "agent_final"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("重规划流程缺少 %s: %s", want, joined)
		}
	}
	// agent_plan 出现 2 次(初始 + 重规划)
	planCount := 0
	for _, e := range events {
		if e.Kind == "agent_plan" {
			planCount++
		}
	}
	if planCount < 2 {
		t.Fatalf("agent_plan 应出现 2 次(初始+重规划), got %d", planCount)
	}
	// 重规划的 plan 带 replanned 标记
	var sawReplanned bool
	for _, e := range events {
		if e.Kind == "agent_plan" {
			if rp, ok := e.Data["replanned"].(bool); ok && rp {
				sawReplanned = true
			}
		}
	}
	if !sawReplanned {
		t.Fatal("重规划 plan 应带 replanned=true")
	}
}

// TestAgentRunNotConfigured M2:未配置 LLM 时 503。
func TestAgentRunNotConfigured(t *testing.T) {
	relay := NewRelay(Config{Dev: true, Addr: ":0"}) // 无 LLMBaseURL
	mux := http.NewServeMux()
	relay.registerHTTP(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	body, _ := json.Marshal(map[string]string{"task": "hi"})
	resp, err := http.Post(srv.URL+"/api/agent/run", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("期望 503, got %d", resp.StatusCode)
	}
}

// TestAgentRunPlanParseFallback M2/v4.3.9:规划失败(LLM 返回非 JSON)→ 直接 agent_error,
// 不再"把 task 当 powershell 命令"兜底(自然语言当命令永远失败且误导)。
func TestAgentRunPlanParseFallback(t *testing.T) {
	var calls int
	srv, _, _ := startAgentRelay(t, func(messages []ChatMessage, tools []ToolDef) (ChatMessage, error) {
		calls++
		return ChatMessage{Content: "不是 JSON"}, nil // 规划解析失败(agentPlan 重试 1 次仍失败)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	events := agentRunSSE(t, ctx, srv, "随便说点什么")

	joined := strings.Join(eventKinds(events), ",")
	// 规划失败 → 直接 agent_error,不应发出 agent_plan(无兜底)
	if hasEvent(events, "agent_plan") {
		t.Fatalf("规划失败不应发出 agent_plan(已移除兜底): %s", joined)
	}
	if !hasEvent(events, "agent_error") {
		t.Fatalf("规划失败应发出 agent_error: %s", joined)
	}
}

// TestParseAgentPlan M2:规划 JSON 容错解析。
func TestParseAgentPlan(t *testing.T) {
	// 正常
	steps, err := parseAgentPlan(`{"steps":[{"goal":"a","tool":"list_agents","args":{}}]}`)
	if err != nil || len(steps) != 1 || steps[0].Tool != "list_agents" {
		t.Fatalf("正常解析失败: %v %+v", err, steps)
	}
	// markdown 围栏
	steps, err = parseAgentPlan("```json\n{\"steps\":[{\"goal\":\"b\",\"tool\":\"run_task\",\"args\":{\"agent_id\":\"x\"}}]}\n```")
	if err != nil || len(steps) != 1 || steps[0].Tool != "run_task" {
		t.Fatalf("围栏解析失败: %v %+v", err, steps)
	}
	// 非法 JSON
	if _, err = parseAgentPlan("not json"); err == nil {
		t.Fatal("非法 JSON 应报错")
	}
	// 空 steps
	if _, err = parseAgentPlan(`{"steps":[]}`); err == nil {
		t.Fatal("空 steps 应报错")
	}
	// 超过 5 步截断
	big := `{"steps":[`
	for i := 0; i < 8; i++ {
		big += fmt.Sprintf(`{"goal":"g%d","tool":"list_agents","args":{}}`, i)
		if i < 7 {
			big += ","
		}
	}
	big += `]}`
	steps, err = parseAgentPlan(big)
	if err != nil || len(steps) != 5 {
		t.Fatalf("超步数应截断为 5, got %d err=%v", len(steps), err)
	}
}

// TestAgentRunConcurrencyLimit M2:并发上限——第 2 个 run 收到 agent_queued。
func TestAgentRunConcurrencyLimit(t *testing.T) {
	var calls int
	srv, relay, _ := startAgentRelay(t, func(messages []ChatMessage, tools []ToolDef) (ChatMessage, error) {
		calls++
		if calls == 1 {
			return ChatMessage{Content: `{"steps":[{"goal":"慢步骤","tool":"list_agents","args":{}}]}`}, nil
		}
		return ChatMessage{Content: "done"}, nil
	})
	// 并发上限 1,先占满
	relay.agentRunSem = make(chan struct{}, 1)
	relay.agentRunSem <- struct{}{}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// 异步跑 run(信号量被占,应排队)
	done := make(chan []agentSSEEvent, 1)
	go func() {
		events := agentRunSSE(t, ctx, srv, "排队测试")
		done <- events
	}()
	// 释放信号量,让 run 继续
	select {
	case <-done:
		t.Fatal("信号量被占时应排队等待,而非直接结束")
	case <-time.After(300 * time.Millisecond):
	}
	<-relay.agentRunSem

	events := <-done
	if !hasEvent(events, "agent_queued") {
		t.Fatalf("排队场景应收到 agent_queued: %s", strings.Join(eventKinds(events), ","))
	}
}

// TestAgentRunToolErrorNoThrow M2:工具错误不 panic,返回 ToolResult(Ok=false)。
func TestAgentRunToolErrorNoThrow(t *testing.T) {
	relay := NewRelay(Config{Dev: true, Addr: ":0", LLMBaseURL: "http://stub"})
	res := relay.agentExecute(context.Background(), "run_task", map[string]any{"agent_id": "no-such-agent", "type": "powershell", "command": "x"})
	if res.Ok {
		t.Fatal("离线 Agent 的任务应失败")
	}
	if res.Summary == "" {
		t.Fatal("失败摘要不能为空")
	}
	// 未知工具
	res = relay.agentExecute(context.Background(), "no_such_tool", nil)
	if res.Ok || !strings.Contains(res.Summary, "未知工具") {
		t.Fatalf("未知工具处理异常: %+v", res)
	}
}
