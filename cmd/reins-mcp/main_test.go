package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/V-T-V/reins/client"
)

// --- 测试用 JSON-RPC 帧构造/断言 ---

func call(t *testing.T, c *client.Client, method string, params any, id string) *rpcResponse {
	t.Helper()
	p, _ := json.Marshal(params)
	req, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": json.RawMessage(p)})
	resp := handleMessage(c, req)
	if resp == nil {
		t.Fatalf("%s: 无响应(被当通知?)", method)
	}
	return resp
}

// fakeRelayRunCommand 模拟 POST /api/tasks + GET /api/tasks/{id} 的完整投递链,
// success 控制任务成败、stdout 控制输出。
func fakeRelayRunCommand(t *testing.T, success bool, stdout string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/tasks":
			var body struct {
				TaskID string `json:"task_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			_ = json.NewEncoder(w).Encode(map[string]any{"task_id": body.TaskID, "status": "dispatched"})
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/tasks/"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"task":   map[string]any{"status": "completed"},
				"result": map[string]any{"task_id": "t-1", "success": success, "stdout": stdout, "exit_code": 0, "duration_ms": 5},
			})
		default:
			http.NotFound(w, r)
		}
	}))
}

// --- 协议层 ---

func TestMCPInitialize(t *testing.T) {
	resp := call(t, client.New("http://127.0.0.1:1", ""), "initialize",
		map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}}, "1")
	m, _ := resp.Result.(map[string]any)
	if m == nil || m["protocolVersion"] != protocolVersion {
		t.Fatalf("initialize result 异常: %v", resp.Result)
	}
	info, _ := m["serverInfo"].(map[string]any)
	if info == nil || info["name"] != "reins-mcp" {
		t.Fatalf("serverInfo 异常: %v", m["serverInfo"])
	}
	if _, ok := m["capabilities"].(map[string]any); !ok {
		t.Fatal("缺 capabilities")
	}
}

func TestMCPToolsList(t *testing.T) {
	resp := call(t, client.New("http://127.0.0.1:1", ""), "tools/list", map[string]any{}, "2")
	m, _ := resp.Result.(map[string]any)
	tools, _ := m["tools"].([]map[string]any)
	if len(tools) < 14 {
		t.Fatalf("工具数 = %d,期望 >=14", len(tools))
	}
	names := map[string]bool{}
	for _, tl := range tools {
		name, _ := tl["name"].(string)
		if name == "" || names[name] {
			t.Fatalf("工具名缺失或重复: %q", name)
		}
		names[name] = true
		sc, _ := tl["inputSchema"].(map[string]any)
		if sc == nil || sc["type"] != "object" {
			t.Fatalf("%s inputSchema 非 object: %v", name, tl["inputSchema"])
		}
	}
	for _, want := range []string{"reins_run_command", "reins_list_agents", "reins_power"} {
		if !names[want] {
			t.Fatalf("缺关键工具 %s", want)
		}
	}
}

func TestMCPPingAndUnknownMethod(t *testing.T) {
	c := client.New("http://127.0.0.1:1", "")
	if r := call(t, c, "ping", map[string]any{}, "3"); r.Error != nil {
		t.Fatalf("ping 报错: %v", r.Error)
	}
	r := call(t, c, "resources/list", map[string]any{}, "4")
	if r.Error == nil || r.Error.Code != -32601 {
		t.Fatalf("未知方法应 -32601,实际: %v", r.Error)
	}
	if string(r.ID) != `"4"` {
		t.Fatalf("id 未回显: %s", r.ID)
	}
}

func TestMCPNotificationNoResponse(t *testing.T) {
	line, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	if resp := handleMessage(client.New("http://127.0.0.1:1", ""), line); resp != nil {
		t.Fatalf("通知不应有响应: %v", resp)
	}
}

// --- 工具执行层(走假 relay) ---

func toolsCall(t *testing.T, c *client.Client, name string, args any) (map[string]any, map[string]any) {
	t.Helper()
	resp := call(t, c, "tools/call", map[string]any{"name": name, "arguments": args}, "5")
	m, _ := resp.Result.(map[string]any)
	if m == nil {
		t.Fatalf("tools/call 无 result(协议错): %+v", resp)
	}
	content, _ := m["content"].([]map[string]any)
	if len(content) == 0 {
		t.Fatalf("无 content: %v", m)
	}
	return m, content[0]
}

func TestMCPToolsCallRunCommand(t *testing.T) {
	srv := fakeRelayRunCommand(t, true, "WIN-KIOSK-01")
	defer srv.Close()
	m, content := toolsCall(t, client.New(srv.URL, ""), "reins_run_command",
		map[string]any{"agent_id": "a1", "command": "hostname", "timeout": 5})
	if m["isError"] == true {
		t.Fatalf("成功任务被标 isError: %v", content)
	}
	if !strings.Contains(content["text"].(string), "WIN-KIOSK-01") {
		t.Fatalf("stdout 未回流: %v", content)
	}
}

func TestMCPToolsCallTaskFailureIsError(t *testing.T) {
	srv := fakeRelayRunCommand(t, false, "")
	defer srv.Close()
	m, content := toolsCall(t, client.New(srv.URL, ""), "reins_run_command",
		map[string]any{"agent_id": "a1", "command": "boom"})
	if m["isError"] != true {
		t.Fatalf("失败任务应 isError:true: %v", m)
	}
	if content["text"].(string) == "" {
		t.Fatal("错误文本为空")
	}
}

func TestMCPToolsCallUnknownTool(t *testing.T) {
	resp := call(t, client.New("http://127.0.0.1:1", ""), "tools/call",
		map[string]any{"name": "no_such_tool", "arguments": map[string]any{}}, "6")
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Fatalf("未知工具应 -32602: %v", resp.Error)
	}
}

func TestMCPToolsCallListAgents(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agents" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"agents": []map[string]any{
			{"agent_id": "a1", "hostname": "H1", "os": "windows/amd64"},
		}, "count": 1})
	}))
	defer srv.Close()
	m, content := toolsCall(t, client.New(srv.URL, ""), "reins_list_agents", map[string]any{})
	if m["isError"] == true {
		t.Fatalf("list_agents 失败: %v", content)
	}
	if !strings.Contains(content["text"].(string), "a1") {
		t.Fatalf("Agent 未回流: %v", content)
	}
}
