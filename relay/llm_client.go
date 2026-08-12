// Package main — Agent 编排的 LLM 客户端(OpenAI 兼容 /v1/chat/completions)。
//
// 纯标准库实现,参照 agentloop/src/llm.ts 的设计:
//   - 原生 function calling(tools 数组)
//   - Chat 非流式(规划/汇总) + ChatStream 流式(最终答案逐 token)
//   - 重试(429/5xx,指数退避)、上下文超时
//   - StubLLM:脚本式 mock,测试不依赖真实模型(参照 go-agent-research internal/llm/mock.go)
//
// 兼容 Ollama(默认 http://127.0.0.1:11434/v1,新版原生支持 OpenAI 兼容 + tools)
// 与云 API(OpenAI/智谱/DeepSeek 等)。
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ChatMessage 对话消息(OpenAI 协议)。
type ChatMessage struct {
	Role       string     `json:"role"` // system / user / assistant / tool
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ToolCall 模型请求的工具调用。
type ToolCall struct {
	ID       string   `json:"id"`
	Type     string   `json:"type"`
	Function ToolFunc `json:"function"`
}

// ToolFunc 工具调用的函数名与参数(参数为 JSON 字符串,需二次解析)。
type ToolFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolDef 工具定义(function calling 的 tools 项)。
type ToolDef struct {
	Type     string          `json:"type"` // "function"
	Function ToolFunctionDef `json:"function"`
}

// ToolFunctionDef 工具函数定义(parameters 为 JSON Schema 子集)。
type ToolFunctionDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// LLMUsage token 用量。
type LLMUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// LLMClient OpenAI 兼容客户端。
type LLMClient struct {
	BaseURL string // 如 http://127.0.0.1:11434/v1
	APIKey  string // 可空(Ollama)
	Model   string // 如 qwen2.5:7b
	HTTP    *http.Client

	stub *StubLLM // 测试:非 nil 时走脚本式 mock(不发起网络请求)
}

// NewLLMClient 构造客户端(HTTP 超时 120s,流式场景由 ctx 控制)。
func NewLLMClient(baseURL, apiKey, model string) *LLMClient {
	return &LLMClient{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		Model:   model,
		HTTP:    &http.Client{Timeout: 120 * time.Second},
	}
}

// chatRequest OpenAI chat completions 请求体。
type chatRequest struct {
	Model          string        `json:"model"`
	Messages       []ChatMessage `json:"messages"`
	Tools          []ToolDef     `json:"tools,omitempty"`
	ToolChoice     string        `json:"tool_choice,omitempty"` // "auto"(默认)
	ResponseFormat *respFormat   `json:"response_format,omitempty"`
	Stream         bool          `json:"stream"`
	StreamOptions  *streamOpts   `json:"stream_options,omitempty"`
	Temperature    float64       `json:"temperature,omitempty"`
}

type respFormat struct {
	Type string `json:"type"` // "json_object"
}

type streamOpts struct {
	IncludeUsage bool `json:"include_usage"`
}

// chatResponse OpenAI chat completions 响应体(非流式)。
type chatResponse struct {
	Choices []struct {
		Message ChatMessage `json:"message"`
	} `json:"choices"`
	Usage LLMUsage `json:"usage"`
}

// Chat 非流式对话。返回助手消息 + 用量。
func (c *LLMClient) Chat(ctx context.Context, messages []ChatMessage, tools []ToolDef) (ChatMessage, LLMUsage, error) {
	if c.stub != nil {
		return c.stubCall(messages, tools)
	}
	body := chatRequest{
		Model:    c.Model,
		Messages: messages,
		Tools:    tools,
		Stream:   false,
	}
	resp, err := c.do(ctx, body)
	if err != nil {
		return ChatMessage{}, LLMUsage{}, err
	}
	if len(resp.Choices) == 0 {
		return ChatMessage{}, resp.Usage, fmt.Errorf("LLM 返回空 choices")
	}
	return resp.Choices[0].Message, resp.Usage, nil
}

// ChatJSON 非流式对话并要求 JSON 对象输出(规划步骤用)。
func (c *LLMClient) ChatJSON(ctx context.Context, messages []ChatMessage, tools []ToolDef) (ChatMessage, LLMUsage, error) {
	if c.stub != nil {
		return c.stubCall(messages, tools)
	}
	body := chatRequest{
		Model:          c.Model,
		Messages:       messages,
		Tools:          tools,
		ResponseFormat: &respFormat{Type: "json_object"},
		Stream:         false,
	}
	resp, err := c.do(ctx, body)
	if err != nil {
		return ChatMessage{}, LLMUsage{}, err
	}
	if len(resp.Choices) == 0 {
		return ChatMessage{}, resp.Usage, fmt.Errorf("LLM 返回空 choices")
	}
	return resp.Choices[0].Message, resp.Usage, nil
}

// ChatStream 流式对话:最终答案逐 token 调 onDelta(仅 content delta)。
// 返回完整消息 + 用量(部分服务端不报 usage,可为零值)。
func (c *LLMClient) ChatStream(ctx context.Context, messages []ChatMessage, tools []ToolDef, onDelta func(string)) (ChatMessage, LLMUsage, error) {
	if c.stub != nil {
		msg, usage, err := c.stubCall(messages, tools)
		if err == nil && onDelta != nil && msg.Content != "" {
			onDelta(msg.Content)
		}
		return msg, usage, err
	}
	body := chatRequest{
		Model:         c.Model,
		Messages:      messages,
		Tools:         tools,
		Stream:        true,
		StreamOptions: &streamOpts{IncludeUsage: true},
	}
	payload, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", c.BaseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return ChatMessage{}, LLMUsage{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}

	// 审查修复:流式中途失败后不得重试——onDelta 已推送的部分 delta 无法回退,
	// 重试会从头重放导致汇总文本重复。仅当尚未产出任何 delta(连接级失败)才可重试。
	var produced bool
	wrapped := func(delta string) {
		produced = true
		onDelta(delta)
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ChatMessage{}, LLMUsage{}, ctx.Err()
			case <-time.After(time.Duration(1<<attempt) * 500 * time.Millisecond): // 0.5s/1s/2s
			}
		}
		msg, usage, err := c.streamOnce(ctx, req, wrapped)
		if err == nil {
			return msg, usage, nil
		}
		lastErr = err
		if produced || !isRetryable(err) {
			return ChatMessage{}, LLMUsage{}, err
		}
	}
	return ChatMessage{}, LLMUsage{}, fmt.Errorf("LLM 流式请求重试耗尽: %w", lastErr)
}

// streamOnce 单次流式请求:解析 SSE data 行,收集 content delta。
func (c *LLMClient) streamOnce(ctx context.Context, req *http.Request, onDelta func(string)) (ChatMessage, LLMUsage, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return ChatMessage{}, LLMUsage{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return ChatMessage{}, LLMUsage{}, fmt.Errorf("LLM HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return ChatMessage{}, LLMUsage{}, &retryableError{msg: fmt.Sprintf("LLM HTTP %d", resp.StatusCode)}
	}

	var msg ChatMessage
	var usage LLMUsage
	msg.Role = "assistant"
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *LLMUsage `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if chunk.Usage != nil {
			usage = *chunk.Usage
		}
		if len(chunk.Choices) > 0 {
			delta := chunk.Choices[0].Delta.Content
			if delta != "" {
				msg.Content += delta
				if onDelta != nil {
					onDelta(delta)
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return ChatMessage{}, LLMUsage{}, fmt.Errorf("LLM 流式读取失败: %w", err)
	}
	return msg, usage, nil
}

// do 非流式请求(带重试:429/5xx,指数退避)。
func (c *LLMClient) do(ctx context.Context, body chatRequest) (*chatResponse, error) {
	payload, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", c.BaseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(1<<attempt) * 500 * time.Millisecond):
			}
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			lastErr = err
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			continue // 网络错误也重试
		}
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			lastErr = &retryableError{msg: fmt.Sprintf("LLM HTTP %d", resp.StatusCode)}
			continue
		}
		if resp.StatusCode >= 400 {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			return nil, fmt.Errorf("LLM HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}
		var out chatResponse
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			resp.Body.Close()
			return nil, fmt.Errorf("LLM 响应解析失败: %w", err)
		}
		resp.Body.Close()
		return &out, nil
	}
	return nil, fmt.Errorf("LLM 请求重试耗尽: %w", lastErr)
}

// retryableError 可重试错误标记。
type retryableError struct{ msg string }

func (e *retryableError) Error() string { return e.msg }

func isRetryable(err error) bool {
	_, ok := err.(*retryableError)
	return ok
}

// --- StubLLM:脚本式 mock(测试用,参照 go-agent-research internal/llm/mock.go) ---

// StubLLM 按注入的 handler 返回固定消息,测试不依赖真实模型。
type StubLLM struct {
	// Handler 收到消息+工具后返回助手消息。nil 时返回空 assistant 消息。
	Handler func(messages []ChatMessage, tools []ToolDef) (ChatMessage, error)
	// Calls 记录调用次数(测试断言)。
	Calls int
}

// NewStubLLM 构造脚本式 mock。
func NewStubLLM(handler func(messages []ChatMessage, tools []ToolDef) (ChatMessage, error)) *StubLLM {
	return &StubLLM{Handler: handler}
}

// stubToClient 把 StubLLM 适配成 *LLMClient(编排层只依赖 *LLMClient 接口)。
func stubToClient(s *StubLLM) *LLMClient {
	return &LLMClient{
		BaseURL: "stub://",
		Model:   "stub",
		HTTP:    &http.Client{},
		stub:    s,
	}
}

// stubCall 走脚本式 mock:计数 + 调用注入的 handler。
func (c *LLMClient) stubCall(messages []ChatMessage, tools []ToolDef) (ChatMessage, LLMUsage, error) {
	c.stub.Calls++
	if c.stub.Handler == nil {
		return ChatMessage{Role: "assistant"}, LLMUsage{}, nil
	}
	msg, err := c.stub.Handler(messages, tools)
	if err != nil {
		return ChatMessage{}, LLMUsage{}, err
	}
	if msg.Role == "" {
		msg.Role = "assistant"
	}
	return msg, LLMUsage{}, nil
}

// StubError 测试用错误。
type StubError struct{ Msg string }

func (e *StubError) Error() string { return e.Msg }
