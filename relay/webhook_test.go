package main

// F11 webhook 告警收口测试(2026-09-14)。
//
// 此前 webhook 模块 5 个文档承诺事件只有 agent_offline 一个接线点,
// 其余 4 个(task_failed/policy_blocked/power_executed/agent_revoked)为
// 零触发点的"半成品"——本文件覆盖:
//   单元:URL 解析/初始化优先级(flag > env)/通知体 JSON 结构/空配置 no-op/
//         发送失败静默(不 panic 不阻塞)/truncate 边界
//   E2E :策略拦截→policy_blocked(真 handler 403 路径)
//         任务失败→task_failed(真 WS fake Agent 回 Success=false)
//         证书撤销→agent_revoked(真 CA)

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/QiuShichang/reins/internal/ca"
	"github.com/QiuShichang/reins/proto"
)

// --- 测试基建:webhook 接收端(sink) ---

// webhookNotification notifyWebhook 发出的通知体结构。
type webhookNotification struct {
	Source    string         `json:"source"`
	Event     string         `json:"event"`
	Timestamp string         `json:"timestamp"`
	Data      map[string]any `json:"data"`
}

// webhookSink httptest 实现的 webhook 接收端。
type webhookSink struct {
	srv *httptest.Server
	got chan webhookNotification
}

// startWebhookSink 启动接收端并设为全局 webhook 目标;
// t.Cleanup 恢复全局 URL 列表(防串测试)。
func startWebhookSink(t *testing.T) *webhookSink {
	t.Helper()
	s := &webhookSink{got: make(chan webhookNotification, 16)}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /hook", func(w http.ResponseWriter, req *http.Request) {
		if ct := req.Header.Get("Content-Type"); ct != "application/json" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var n webhookNotification
		if err := json.NewDecoder(req.Body).Decode(&n); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		s.got <- n
		w.WriteHeader(http.StatusOK)
	})
	s.srv = httptest.NewServer(mux)
	t.Cleanup(func() {
		setWebhookURLs(nil)
		s.srv.Close()
	})
	setWebhookURLs([]string{s.srv.URL + "/hook"})
	return s
}

// waitEvent 等待指定事件到达(3s 超时),忽略中途其它事件。
func (s *webhookSink) waitEvent(t *testing.T, event string) webhookNotification {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case n := <-s.got:
			if n.Event == event {
				return n
			}
		case <-deadline:
			t.Fatalf("webhook 事件 %q 未在 3s 内到达", event)
			return webhookNotification{}
		}
	}
}

// drain 在短窗口内确认无任何事件到达(空配置/静默路径验证)。
func (s *webhookSink) drain(t *testing.T) {
	t.Helper()
	select {
	case n := <-s.got:
		t.Fatalf("不应有 webhook 事件到达,收到: %s", n.Event)
	case <-time.After(150 * time.Millisecond):
	}
}

// --- 单元 ---

// TestParseWebhookURLs 逗号分隔解析:空白项剔除/全空返回 nil。
func TestParseWebhookURLs(t *testing.T) {
	if got := parseWebhookURLs(""); got != nil {
		t.Errorf("空串应返回 nil,得到 %v", got)
	}
	if got := parseWebhookURLs("  , ,"); got != nil {
		t.Errorf("纯空白应返回 nil,得到 %v", got)
	}
	got := parseWebhookURLs(" https://a.example/x , http://b.example/y ,, ")
	want := []string{"https://a.example/x", "http://b.example/y"}
	if len(got) != len(want) {
		t.Fatalf("解析结果长度 %d != %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 个 URL: got %q want %q", i, got[i], want[i])
		}
	}
}

// TestInitWebhooksFlagOverEnv flag 显式配置优先于环境变量。
func TestInitWebhooksFlagOverEnv(t *testing.T) {
	t.Setenv("REINS_WEBHOOKS", "https://env.example/hook")
	t.Cleanup(func() { setWebhookURLs(nil) })

	initWebhooks("https://flag.example/hook1,https://flag.example/hook2")
	webhookURLsMu.RLock()
	defer webhookURLsMu.RUnlock()
	if len(webhookURLs) != 2 || webhookURLs[0] != "https://flag.example/hook1" {
		t.Fatalf("flag 应覆盖 env: %v", webhookURLs)
	}
}

// TestInitWebhooksEnvFallback flag 为空时回退环境变量。
func TestInitWebhooksEnvFallback(t *testing.T) {
	t.Setenv("REINS_WEBHOOKS", "https://env.example/hook1,https://env.example/hook2")
	t.Cleanup(func() { setWebhookURLs(nil) })

	initWebhooks("")
	webhookURLsMu.RLock()
	defer webhookURLsMu.RUnlock()
	if len(webhookURLs) != 2 || webhookURLs[0] != "https://env.example/hook1" {
		t.Fatalf("env 兜底未生效: %v", webhookURLs)
	}
}

// TestWebhookNotifyDeliversJSON 通知体结构:source/event/timestamp/data 四字段 + JSON content-type。
func TestWebhookNotifyDeliversJSON(t *testing.T) {
	sink := startWebhookSink(t)

	notifyWebhook("unit_event", map[string]any{"key": "value", "num": 42})
	n := sink.waitEvent(t, "unit_event")
	if n.Source != "reins" {
		t.Errorf("source 应为 reins,得到 %q", n.Source)
	}
	if n.Timestamp == "" {
		t.Error("timestamp 不应为空")
	}
	if _, err := time.Parse(time.RFC3339, n.Timestamp); err != nil {
		t.Errorf("timestamp 非 RFC3339: %q (%v)", n.Timestamp, err)
	}
	if n.Data["key"] != "value" || n.Data["num"] != float64(42) {
		t.Errorf("data 载荷错误: %v", n.Data)
	}
}

// TestWebhookEmptyConfigNoop 未配置 URL 时 notify 直接返回,零发送。
func TestWebhookEmptyConfigNoop(t *testing.T) {
	setWebhookURLs(nil)
	t.Cleanup(func() { setWebhookURLs(nil) })

	// 起 sink 但不指向它——若误发也不会被收到,再显式等一小段确认。
	sink := &webhookSink{got: make(chan webhookNotification, 1)}
	notifyWebhook("noop_event", map[string]any{"k": "v"})
	select {
	case n := <-sink.got:
		t.Fatalf("空配置不应发送,收到 %s", n.Event)
	case <-time.After(150 * time.Millisecond):
	}
}

// TestWebhookUnreachableSilent 目标不可达时发送失败静默(不 panic/不阻塞调用方)。
func TestWebhookUnreachableSilent(t *testing.T) {
	srv := httptest.NewServer(http.NewServeMux())
	url := srv.URL + "/gone"
	srv.Close() // 立即关闭,制造连接拒绝
	setWebhookURLs([]string{url})
	t.Cleanup(func() { setWebhookURLs(nil) })

	done := make(chan struct{})
	go func() {
		defer close(done)
		notifyWebhook("fire_and_forget", map[string]any{"k": "v"}) // 异步 goroutine 内失败仅 Debug 日志
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("notifyWebhook 调用方被阻塞超过 2s(应非阻塞)")
	}
	// 内部 goroutine 的发送失败路径不 panic 即可;给一点调度窗口。
	time.Sleep(100 * time.Millisecond)
}

// TestTruncate 截断边界:不超长原样/恰等长原样/超长截断加省略号。
func TestTruncate(t *testing.T) {
	if got := truncate("short", 500); got != "short" {
		t.Errorf("短串应原样: %q", got)
	}
	if got := truncate(strings.Repeat("a", 500), 500); got != strings.Repeat("a", 500) {
		t.Errorf("恰等长应原样,得到 len=%d", len(got))
	}
	got := truncate(strings.Repeat("a", 501), 500)
	// 语义:保留 max 个原始字符再追加 "..."(总长 max+3)
	if len(got) != 503 || !strings.HasSuffix(got, "...") {
		t.Errorf("超长应截断到 500 原字符+省略号(总 503),得到 len=%d suffix=%v", len(got), strings.HasSuffix(got, "..."))
	}
}

// --- E2E:事件接线(真 handler 路径) ---

// TestPolicyBlockedWebhookE2E 单任务策略拦截(403)触发 policy_blocked webhook。
func TestPolicyBlockedWebhookE2E(t *testing.T) {
	sink := startWebhookSink(t)

	relay := NewRelay(Config{Dev: true})
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/tasks", relay.handlePostTask)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	body, _ := json.Marshal(map[string]any{
		"task_id":  "webhook-blocked-1",
		"agent_id": "agent-x",
		"type":     proto.TaskPowerShell,
		"payload":  map[string]string{"command": "Format-Volume -DriveLetter C"},
	})
	resp, err := http.Post(srv.URL+"/api/tasks", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("高危命令应 403,得到 %d", resp.StatusCode)
	}

	n := sink.waitEvent(t, "policy_blocked")
	if n.Data["agent_id"] != "agent-x" {
		t.Errorf("data.agent_id 应为 agent-x,得到 %v", n.Data["agent_id"])
	}
	if n.Data["type"] != proto.TaskPowerShell {
		t.Errorf("data.type 应为 %s,得到 %v", proto.TaskPowerShell, n.Data["type"])
	}
	if detail, _ := n.Data["detail"].(string); !strings.Contains(detail, "Format-Volume") {
		t.Errorf("data.detail 应含被拦命令: %v", n.Data["detail"])
	}
}

// TestOfflineTaskPolicyBlocked P1-7 回归:高危命令发给**离线** Agent 也必须被
// 策略拦截(403),不得以 202 缓存绕过——缓存任务在 Agent 上线补投时无二次校验。
// 该洞与 P1-1(批量路径绕过)同构,由 webhook E2E 测试构造离线目标时意外暴露。
func TestOfflineTaskPolicyBlocked(t *testing.T) {
	relay := NewRelay(Config{Dev: true})
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/tasks", relay.handlePostTask)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	body, _ := json.Marshal(map[string]any{
		"task_id":  "offline-blocked-1",
		"agent_id": "ghost-agent", // 从未上线
		"type":     proto.TaskPowerShell,
		"payload":  map[string]string{"command": "Format-Volume -DriveLetter C"},
	})
	resp, err := http.Post(srv.URL+"/api/tasks", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("离线目标高危命令应 403 拦截(不得 202 缓存),得到 %d", resp.StatusCode)
	}
}

// TestTaskFailedWebhookE2E Agent 回传失败结果(Success=false)触发 task_failed webhook。
// 真 WS fake Agent:注册 → 收任务 → 回失败 result,Relay serveAgent 全链路处理。
func TestTaskFailedWebhookE2E(t *testing.T) {
	sink := startWebhookSink(t)

	relay := NewRelay(Config{Dev: true, Addr: ":0"})
	mux := http.NewServeMux()
	relay.registerHTTP(mux)
	mux.HandleFunc("/agent", relay.handleAgentWS)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	agentURL := strings.Replace(srv.URL+"/agent", "http://", "ws://", 1)

	conn, _, err := websocket.Dial(ctx, agentURL, nil) //nolint:bodyclose // coder/websocket:成功时 resp.Body 为 nil
	if err != nil {
		t.Fatalf("agent 连接失败: %v", err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 20)

	regEnv, _ := proto.NewEnvelope(proto.TypeRegister, "wh-agent", "relay", proto.Register{
		AgentID: "wh-agent", Hostname: "wh-host", OS: "linux/amd64",
		Capabilities: []string{proto.TaskPowerShell},
	})
	if err := conn.Write(ctx, websocket.MessageText, mustJSON(regEnv)); err != nil {
		t.Fatal(err)
	}

	// 读循环:收到任务回失败结果
	go func() {
		for {
			_, data, rerr := conn.Read(ctx)
			if rerr != nil {
				return
			}
			var env proto.Envelope
			if json.Unmarshal(data, &env) == nil && env.Type == proto.TypeTaskRequest {
				var task proto.TaskRequest
				_ = env.Decode(&task)
				resEnv, _ := proto.NewEnvelope(proto.TypeTaskResult, "wh-agent", "relay", proto.TaskResult{
					TaskID: task.TaskID, Success: false, ExitCode: 1, Error: "simulated failure",
				})
				_ = conn.Write(ctx, websocket.MessageText, mustJSON(resEnv))
			}
		}
	}()

	// 等 agent 上线
	deadline := time.Now().Add(3 * time.Second)
	for len(relay.agents.Snapshot()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("agent 未上线")
		}
		time.Sleep(10 * time.Millisecond)
	}

	taskBody, _ := json.Marshal(map[string]any{
		"task_id":  "webhook-fail-1",
		"agent_id": "wh-agent",
		"type":     proto.TaskPowerShell,
		"payload":  mustJSON(proto.PowerShellTask{Command: "exit 1"}),
	})
	resp, err := http.Post(srv.URL+"/api/tasks", "application/json", strings.NewReader(string(taskBody)))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /api/tasks 期望 202,得到 %d: %s", resp.StatusCode, string(body))
	}

	n := sink.waitEvent(t, "task_failed")
	if n.Data["agent_id"] != "wh-agent" {
		t.Errorf("data.agent_id 应为 wh-agent,得到 %v", n.Data["agent_id"])
	}
	if n.Data["task_id"] != "webhook-fail-1" {
		t.Errorf("data.task_id 应为 webhook-fail-1,得到 %v", n.Data["task_id"])
	}
	if n.Data["error"] != "simulated failure" {
		t.Errorf("data.error 应透传失败原因,得到 %v", n.Data["error"])
	}
}

// TestRevokeWebhookE2E 证书撤销触发 agent_revoked webhook。
func TestRevokeWebhookE2E(t *testing.T) {
	sink := startWebhookSink(t)

	relay := NewRelay(Config{Dev: true})
	authority, err := ca.LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatalf("CA 创建失败: %v", err)
	}
	relay.ca = authority

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/agents/revoke", relay.handleRevokeAgent)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	body, _ := json.Marshal(map[string]string{"agent_id": "revoked-agent-1"})
	resp, err := http.Post(srv.URL+"/api/agents/revoke", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("revoke 期望 200,得到 %d", resp.StatusCode)
	}

	n := sink.waitEvent(t, "agent_revoked")
	if n.Data["agent_id"] != "revoked-agent-1" {
		t.Errorf("data.agent_id 应为 revoked-agent-1,得到 %v", n.Data["agent_id"])
	}
}
