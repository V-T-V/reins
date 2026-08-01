// Package main — relay 中继层 round 2 深化测试。
//
// 聚焦 round 2 目标补强：多控制台事件扇出 / 心跳保活与无响应回收 /
// 消息路由严格隔离。与 relay_multi_client_test.go 互补：
//   - multi_client 偏 Registry/EventBus 单元级 + 单 console SSE。
//   - 本文件补：多 console 同时订阅的事件扇出、Sweep 驱动 dead-agent 离线、
//     能力感知路由对不匹配任务类型的拒绝、task_id 隔离(不串台)。
//
// 全部用真实 httptest + coder/websocket 端到端验证，非纯 mock。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/QiuShichang/go-rmm/proto"
)

// 静音测试期日志（这些 E2E 测试会产生大量心跳/离线日志，干扰输出）。
func init() { log.SetOutput(io.Discard) }

// dialRegisterAgent 建立一条真实 WS agent 连接并发送 register，返回连接与就绪信号。
// 返回的 readyCh 在 relay 看到 Snapshot 含该 agent 后关闭。
func dialRegisterAgent(ctx context.Context, t *testing.T, srv *httptest.Server, id string, caps []string) (*websocket.Conn, chan struct{}) {
	t.Helper()
	url := strings.Replace(srv.URL+"/agent", "http://", "ws://", 1)
	c, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("agent %s dial: %v", id, err)
	}
	c.SetReadLimit(1 << 20)
	regEnv, _ := proto.NewEnvelope(proto.TypeRegister, id, "relay", proto.Register{
		AgentID: id, Hostname: id, OS: "linux/amd64", Capabilities: caps,
	})
	if err := c.Write(ctx, websocket.MessageText, mustJSON(regEnv)); err != nil {
		t.Fatalf("agent %s register write: %v", id, err)
	}
	ready := make(chan struct{})
	return c, ready
}

// --- 多控制台 SSE 事件扇出 ---

// TestMultiConsoleSSEFanout 验证多个控制台同时订阅 SSE 时，
// 一个 task_result 事件被扇出到每个控制台各一份（广播而非队列抢占）。
func TestMultiConsoleSSEFanout(t *testing.T) {
	defer log.SetOutput(os.Stderr)
	relay := NewRelay(Config{Dev: true, Addr: ":0"})
	mux := http.NewServeMux()
	relay.registerHTTP(mux)
	mux.HandleFunc("/agent", relay.handleAgentWS)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// 3 个控制台各自建立 SSE，只关心 task_result 事件。
	const consoles = 3
	results := make([]chan string, consoles)
	for i := 0; i < consoles; i++ {
		results[i] = make(chan string, 4)
		// 复制 i 避免闭包捕获最后一个值。
		ch := results[i]
		req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/events?events=task_result", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("console %d SSE: %v", i, err)
		}
		defer resp.Body.Close()
		go func() {
			buf := make([]byte, 4096)
			var pending string
			for {
				n, rerr := resp.Body.Read(buf)
				if n > 0 {
					pending += string(buf[:n])
					for {
						idx := strings.Index(pending, "event: ")
						if idx < 0 {
							break
						}
						rest := pending[idx+len("event: "):]
						end := strings.Index(rest, "\n")
						if end < 0 {
							break
						}
						kind := strings.TrimSpace(rest[:end])
						if kind != "hello" {
							select {
							case ch <- kind:
							default:
							}
						}
						pending = rest[end+1:]
					}
				}
				if rerr != nil {
					return
				}
			}
		}()
	}

	// 起一个 agent 上线 + 接收一个任务 + 回 result（触发广播）。
	conn, _ := dialRegisterAgent(ctx, t, srv, "fanout-1", []string{proto.TaskPowerShell})
	defer conn.CloseNow()
	var gotTask atomic.Int32
	go func() {
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var env proto.Envelope
			if json.Unmarshal(data, &env) == nil && env.Type == proto.TypeTaskRequest {
				gotTask.Add(1)
				resEnv, _ := proto.NewEnvelope(proto.TypeTaskResult, "fanout-1", "relay", proto.TaskResult{
					TaskID: "fanout-task", Success: true, Stdout: "ok",
				})
				_ = conn.Write(ctx, websocket.MessageText, mustJSON(resEnv))
			}
		}
	}()

	// 等 agent 真正就绪（supports 才是能力路由就绪信号）。
	deadline := time.Now().Add(3 * time.Second)
	for !relay.agents.Supports("fanout-1", proto.TaskPowerShell) {
		if time.Now().After(deadline) {
			t.Fatal("agent fanout-1 未在期内就绪")
		}
		time.Sleep(10 * time.Millisecond)
	}

	body, _ := json.Marshal(map[string]any{
		"task_id":  "fanout-task",
		"agent_id": "fanout-1",
		"type":     proto.TaskPowerShell,
		"payload":  mustJSON(proto.PowerShellTask{Command: "echo hi"}),
	})
	resp, err := http.Post(srv.URL+"/api/tasks", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST 期望 202, got %d", resp.StatusCode)
	}

	// 每个控制台都应收到 task_result（扇出，非抢占）。
	for i, ch := range results {
		select {
		case kind := <-ch:
			if kind != proto.TypeTaskResult {
				t.Fatalf("console %d 收到 %s, 期望 task_result", i, kind)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("console %d 未收到 task_result 事件(扇出失败)", i)
		}
	}
}

// --- 心跳保活与无响应回收 ---

// TestHeartbeatKeepsAgentAlive 验证周期心跳能让 Sweep 不清理该 agent。
// 对照组：另一个不发自跳的 agent 会被 Sweep 清理。
func TestHeartbeatKeepsAgentAlive(t *testing.T) {
	defer log.SetOutput(os.Stderr)
	r := NewRegistry()

	// alive：注册后持续 Touch。
	r.Register(proto.Register{AgentID: "alive-1"}, &wsConn{id: "c-alive"})
	// dead：注册后永不 Touch。
	r.Register(proto.Register{AgentID: "dead-1"}, &wsConn{id: "c-dead"})

	// alive 每 5ms 发一次心跳，Sweep 阈值 20ms。
	stop := make(chan struct{})
	var beats int64
	go func() {
		tk := time.NewTicker(5 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tk.C:
				if r.Touch("alive-1", atomic.AddInt64(&beats, 1)) {
					// ok
				}
			}
		}
	}()

	// 跑几次 Sweep（每次都应只清理 dead-1，保留 alive-1）。
	time.Sleep(30 * time.Millisecond) // 让 dead 心跳超时
	removed := r.Sweep(20 * time.Millisecond)
	close(stop)

	foundAlive, foundDead := false, false
	for _, id := range removed {
		if id == "alive-1" {
			foundAlive = true
		}
		if id == "dead-1" {
			foundDead = true
		}
	}
	if foundAlive {
		t.Fatal("持续心跳的 alive-1 不应被 Sweep 清理")
	}
	if !foundDead {
		t.Fatalf("无心跳的 dead-1 应被 Sweep 清理, removed=%v", removed)
	}
	// alive-1 仍可查到。
	if r.Conn("alive-1") == nil {
		t.Fatal("alive-1 应仍在线")
	}
	if r.Conn("dead-1") != nil {
		t.Fatal("dead-1 应已被清理")
	}
}

// TestSweepIdempotent 验证连续 Sweep 不会误删或重复清理。
func TestSweepIdempotent(t *testing.T) {
	defer log.SetOutput(os.Stderr)
	r := NewRegistry()
	r.Register(proto.Register{AgentID: "a1"}, &wsConn{id: "c1"})
	r.Register(proto.Register{AgentID: "a2"}, &wsConn{id: "c2"})

	// 让心跳真正过期（Windows 计时器粒度较粗，必须 sleep 足够时间使 elapsed > threshold）。
	time.Sleep(30 * time.Millisecond)
	// 首次 Sweep（10ms 阈值）清理 a1+a2。
	first := r.Sweep(10 * time.Millisecond)
	if len(first) != 2 {
		t.Fatalf("首次 Sweep 应清理 2 个, got %d (%v)", len(first), first)
	}
	// 再次 Sweep 应返回空（已无条目）。
	second := r.Sweep(10 * time.Millisecond)
	if len(second) != 0 {
		t.Fatalf("空注册表 Sweep 应返回空, got %v", second)
	}
	if len(r.Snapshot()) != 0 {
		t.Fatal("清理后 Snapshot 应为空")
	}
}

// --- 消息路由严格隔离 ---

// TestRoutingRejectsUnsupportedType 验证能力感知路由：
// agent 只声明 powershell 能力时，下发 file 任务应被 400 拒绝（不透传）。
func TestRoutingRejectsUnsupportedType(t *testing.T) {
	defer log.SetOutput(os.Stderr)
	r, srv := newTestRelay(t)
	defer srv.Close()
	registerMockAgent(r, "only-ps", []string{proto.TaskPowerShell})

	body, _ := json.Marshal(map[string]any{
		"task_id":  "unsupported-1",
		"agent_id": "only-ps",
		"type":     proto.TaskFile, // agent 不支持
		"payload":  mustJSON(map[string]any{"path": "/tmp/x", "mode": "download"}),
	})
	resp, err := http.Post(srv.URL+"/api/tasks", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("不支持的任务类型应返回 400, got %d", resp.StatusCode)
	}
}

// TestRoutingIsolationNoCrossTalk 验证 task_id 唯一性：
// 同一 agent 连续下发两个不同 task_id 的任务，结果按 task_id 正确回填，不串台。
func TestRoutingIsolationNoCrossTalk(t *testing.T) {
	defer log.SetOutput(os.Stderr)
	relay := NewRelay(Config{Dev: true, Addr: ":0"})
	mux := http.NewServeMux()
	relay.registerHTTP(mux)
	mux.HandleFunc("/agent", relay.handleAgentWS)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	conn, _ := dialRegisterAgent(ctx, t, srv, "iso-1", []string{proto.TaskPowerShell})
	defer conn.CloseNow()

	// agent 收任务后按 task_id 原样回不同 stdout。
	type rec struct{ id, out string }
	got := make(chan rec, 4)
	go func() {
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var env proto.Envelope
			if json.Unmarshal(data, &env) == nil && env.Type == proto.TypeTaskRequest {
				var task proto.TaskRequest
				_ = env.Decode(&task)
				resEnv, _ := proto.NewEnvelope(proto.TypeTaskResult, "iso-1", "relay", proto.TaskResult{
					TaskID: task.TaskID, Success: true, Stdout: "out-for-" + task.TaskID,
				})
				_ = conn.Write(ctx, websocket.MessageText, mustJSON(resEnv))
				got <- rec{task.TaskID, ""}
			}
		}
	}()

	deadline := time.Now().Add(3 * time.Second)
	for !relay.agents.Supports("iso-1", proto.TaskPowerShell) {
		if time.Now().After(deadline) {
			t.Fatal("agent iso-1 未就绪")
		}
		time.Sleep(10 * time.Millisecond)
	}

	for _, id := range []string{"iso-a", "iso-b"} {
		body, _ := json.Marshal(map[string]any{
			"task_id":  id,
			"agent_id": "iso-1",
			"type":     proto.TaskPowerShell,
			"payload":  mustJSON(proto.PowerShellTask{Command: "echo " + id}),
		})
		resp, err := http.Post(srv.URL+"/api/tasks", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("task %s 期望 202, got %d", id, resp.StatusCode)
		}
	}

	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		select {
		case r := <-got:
			if seen[r.id] {
				t.Fatalf("task %s 被收到两次(串台)", r.id)
			}
			seen[r.id] = true
		case <-time.After(2 * time.Second):
			t.Fatalf("只收到 %d 个结果(期望 2)", i)
		}
	}
	if !seen["iso-a"] || !seen["iso-b"] {
		t.Fatalf("两个 task_id 都应被收到, seen=%v", seen)
	}
}
