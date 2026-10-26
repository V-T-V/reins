// Package main — reins MCP Server(stdio 传输)。
//
// 把 reins 的远程运维能力暴露为 Model Context Protocol 工具,供
// Claude Desktop/Cursor/ZCode 等支持 MCP 的 AI 客户端原生调用——
// AI 代理从此用标准工具调用协议驱动远程舰队,而不是解析 CLI 文本。
//
// 配置(环境变量,与 CLI 同名):
//
//	REINS_RELAY  Relay 地址,默认 http://127.0.0.1:7780
//	REINS_TOKEN  鉴权 token
//
// 协议:MCP stdio 传输(JSON-RPC 2.0,换行分隔,消息内不得嵌换行)。
// 日志一律走 stderr——stdout 是协议通道,混入即毁约。
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"

	"github.com/V-T-V/reins/client"
)

var version = "dev" // -ldflags 注入

const protocolVersion = "2025-06-18"

// --- JSON-RPC 2.0 帧 ---

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func main() {
	relay := envOr("REINS_RELAY", "http://127.0.0.1:7780")
	c := client.New(relay, os.Getenv("REINS_TOKEN"))
	fmt.Fprintf(os.Stderr, "reins-mcp %s → %s\n", version, relay)

	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 1<<20), 8<<20)
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	for in.Scan() {
		line := bytes.TrimSpace(in.Bytes())
		if len(line) == 0 {
			continue
		}
		if resp := handleMessage(c, line); resp != nil {
			b, err := json.Marshal(resp)
			if err != nil {
				continue
			}
			out.Write(b)
			out.WriteByte('\n')
			out.Flush()
		}
	}
}

// handleMessage 处理一行请求,返回响应;通知(无 id)返回 nil。
func handleMessage(c *client.Client, line []byte) *rpcResponse {
	var req rpcRequest
	if err := json.Unmarshal(line, &req); err != nil {
		return &rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"),
			Error: &rpcError{Code: -32700, Message: "parse error"}}
	}
	if len(req.ID) == 0 { // 通知:不回应
		return nil
	}
	var result any
	var rerr *rpcError
	switch req.Method {
	case "initialize":
		result = map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "reins-mcp", "version": version},
		}
	case "ping":
		result = map[string]any{}
	case "tools/list":
		result = map[string]any{"tools": toolDefs()}
	case "tools/call":
		result, rerr = callTool(c, req.Params)
	case "notifications/initialized", "notifications/cancelled":
		return nil
	default:
		rerr = &rpcError{Code: -32601, Message: "method not found: " + req.Method}
	}
	resp := &rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result}
	if rerr != nil {
		resp.Result = nil
		resp.Error = rerr
	}
	return resp
}

// callTool 执行 tools/call。工具内部错误按 MCP 语义回 isError:true(协议层成功)。
func callTool(c *client.Client, params json.RawMessage) (any, *rpcError) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpcError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	tool := findTool(p.Name)
	if tool == nil {
		return nil, &rpcError{Code: -32602, Message: "unknown tool: " + p.Name}
	}
	out, err := tool.Handler(c, p.Arguments)
	text := marshalIndent(out)
	isErr := false
	if err != nil {
		isErr = true
		text = err.Error()
	}
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isErr,
	}, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func marshalIndent(v any) string {
	if v == nil {
		return "ok"
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}
