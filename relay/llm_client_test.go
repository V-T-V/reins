// relay/llm_client_test.go
// M1:LLM 客户端测试——非流式/流式协议、重试、StubLLM 脚本式 mock。
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// stubChatServer 构造一个返回固定响应的 OpenAI 兼容测试服务器。
// 记录收到的请求体(供断言);可配置流式/非流式、HTTP 状态码。
func stubChatServer(t *testing.T, status int, streamBody string, nonStreamResp map[string]any, reqCh chan map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if reqCh != nil {
			reqCh <- body
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		if stream, _ := body["stream"].(bool); stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			for _, line := range strings.Split(streamBody, "\n") {
				if line == "" {
					continue
				}
				w.Write([]byte("data: " + line + "\n\n"))
				if flusher != nil {
					flusher.Flush()
				}
			}
			w.Write([]byte("data: [DONE]\n\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(nonStreamResp)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestLLMChatNonStream M1:非流式对话解析 assistant 消息 + tool_calls。
func TestLLMChatNonStream(t *testing.T) {
	reqCh := make(chan map[string]any, 1)
	srv := stubChatServer(t, http.StatusOK, "", map[string]any{
		"choices": []map[string]any{{
			"message": map[string]any{
				"role":    "assistant",
				"content": "",
				"tool_calls": []map[string]any{{
					"id":   "call-1",
					"type": "function",
					"function": map[string]any{
						"name":      "list_agents",
						"arguments": `{}`,
					},
				}},
			},
		}},
		"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5},
	}, reqCh)

	c := NewLLMClient(srv.URL, "test-key", "test-model")
	msg, usage, err := c.Chat(context.Background(), []ChatMessage{{Role: "user", Content: "hi"}}, []ToolDef{{
		Type: "function", Function: ToolFunctionDef{Name: "list_agents", Description: "x", Parameters: map[string]any{"type": "object"}},
	}})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(msg.ToolCalls) != 1 || msg.ToolCalls[0].Function.Name != "list_agents" {
		t.Fatalf("tool_calls 解析异常: %+v", msg.ToolCalls)
	}
	if usage.PromptTokens != 10 || usage.CompletionTokens != 5 {
		t.Errorf("usage 异常: %+v", usage)
	}
	// 请求体断言:model/messages/tools/Authorization
	req := <-reqCh
	if req["model"] != "test-model" {
		t.Errorf("model = %v", req["model"])
	}
	if req["stream"] != false {
		t.Errorf("stream = %v, want false", req["stream"])
	}
}

// TestLLMChatStream M1:流式对话逐 token 回调 + 完整消息。
func TestLLMChatStream(t *testing.T) {
	streamBody := `{"choices":[{"delta":{"content":"Hello "}}]}
{"choices":[{"delta":{"content":"world"}}]}
{"choices":[{"delta":{}}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`
	srv := stubChatServer(t, http.StatusOK, streamBody, nil, nil)

	c := NewLLMClient(srv.URL, "", "m")
	var deltas []string
	msg, _, err := c.ChatStream(context.Background(), []ChatMessage{{Role: "user", Content: "hi"}}, nil, func(d string) {
		deltas = append(deltas, d)
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if msg.Content != "Hello world" {
		t.Errorf("content = %q, want Hello world", msg.Content)
	}
	if strings.Join(deltas, "") != "Hello world" {
		t.Errorf("deltas = %v", deltas)
	}
}

// TestLLMRetryOn5xx M1:500 重试后成功(3 次尝试内)。
func TestLLMRetryOn5xx(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": "ok"}}},
		})
	}))
	t.Cleanup(srv.Close)

	c := NewLLMClient(srv.URL, "", "m")
	msg, _, err := c.Chat(context.Background(), []ChatMessage{{Role: "user", Content: "hi"}}, nil)
	if err != nil {
		t.Fatalf("重试后应成功: %v", err)
	}
	if msg.Content != "ok" {
		t.Errorf("content = %q", msg.Content)
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d, want 2(1 失败 + 1 成功)", calls.Load())
	}
}

// TestLLMRetryExhausted M1:持续 500 重试耗尽返回错误。
func TestLLMRetryExhausted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	c := NewLLMClient(srv.URL, "", "m")
	_, _, err := c.Chat(context.Background(), []ChatMessage{{Role: "user", Content: "hi"}}, nil)
	if err == nil {
		t.Fatal("持续 503 应返回错误")
	}
	if !strings.Contains(err.Error(), "重试耗尽") {
		t.Errorf("错误信息: %v", err)
	}
}

// TestLLMChatJSON M1:ChatJSON 带 response_format=json_object。
func TestLLMChatJSON(t *testing.T) {
	reqCh := make(chan map[string]any, 1)
	srv := stubChatServer(t, http.StatusOK, "", map[string]any{
		"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": `{"steps":[]}`}}},
	}, reqCh)

	c := NewLLMClient(srv.URL, "", "m")
	_, _, err := c.ChatJSON(context.Background(), []ChatMessage{{Role: "user", Content: "plan"}}, nil)
	if err != nil {
		t.Fatalf("ChatJSON: %v", err)
	}
	req := <-reqCh
	rf, ok := req["response_format"].(map[string]any)
	if !ok || rf["type"] != "json_object" {
		t.Errorf("response_format 缺失: %v", req["response_format"])
	}
}

// TestStubLLM M1:StubLLM 脚本式 mock(计数 + handler 注入 + 不发起网络)。
func TestStubLLM(t *testing.T) {
	stub := NewStubLLM(func(messages []ChatMessage, tools []ToolDef) (ChatMessage, error) {
		if len(tools) != 2 {
			t.Errorf("tools = %d, want 2", len(tools))
		}
		return ChatMessage{Content: `{"steps":[]}`}, nil
	})
	c := stubToClient(stub)

	// Chat 走 mock
	msg, _, err := c.Chat(context.Background(), nil, make([]ToolDef, 2))
	if err != nil || msg.Content != `{"steps":[]}` {
		t.Fatalf("stub Chat: %v %q", err, msg.Content)
	}
	// ChatStream 走 mock 且 onDelta 触发
	var delta string
	msg, _, err = c.ChatStream(context.Background(), nil, make([]ToolDef, 2), func(d string) { delta += d })
	if err != nil || delta != `{"steps":[]}` {
		t.Fatalf("stub ChatStream: %v delta=%q", err, delta)
	}
	if stub.Calls != 2 {
		t.Errorf("stub.Calls = %d, want 2", stub.Calls)
	}
	// handler 为 nil 时返回空 assistant(不 panic)
	stub2 := NewStubLLM(nil)
	c2 := stubToClient(stub2)
	msg, _, err = c2.Chat(context.Background(), nil, nil)
	if err != nil || msg.Role != "assistant" {
		t.Fatalf("nil handler: %v %+v", err, msg)
	}
}

// TestLLMContextCancel M1:ctx 取消立即返回。
func TestLLMContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second) // 挂住
	}))
	t.Cleanup(srv.Close)

	c := NewLLMClient(srv.URL, "", "m")
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, _, err := c.Chat(ctx, []ChatMessage{{Role: "user", Content: "hi"}}, nil)
	if err == nil {
		t.Fatal("ctx 取消应返回错误")
	}
	if time.Since(start) > 2*time.Second {
		t.Error("ctx 取消应快速返回")
	}
}
