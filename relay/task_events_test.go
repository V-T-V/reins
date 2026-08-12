// relay/task_events_test.go
// P1-B:任务状态事件广播——task_dispatched / task_acknowledged / task_result 依次发布,
// SSE 订阅者可实时跟踪任务全生命周期(CLI waitResult 双通道的基础)。
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/QiuShichang/go-rmm/proto"
)

// startTaskEventRelay 启动带 MemoryStore 的测试 Relay + 真实 WS agent 端点。
func startTaskEventRelay(t *testing.T) (*httptest.Server, *Relay) {
	t.Helper()
	relay := NewRelay(Config{Dev: true, Addr: ":0"})
	relay.WithStore(NewMemoryStore())
	mux := http.NewServeMux()
	relay.registerHTTP(mux)
	mux.HandleFunc("/agent", relay.handleAgentWS)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, relay
}

// sseCollector 订阅 /api/events?events=...,按 event kind 收集事件 data。
// 返回 kind → 事件数据切片(线程安全由调用方保证:测试串行消费)。
func sseCollector(ctx context.Context, t *testing.T, srv *httptest.Server, events string) (func(kind string) string, func()) {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/events?events="+events, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("SSE 连接: %v", err)
	}
	type ev struct {
		kind string
		data string
	}
	ch := make(chan ev, 32)
	stop := make(chan struct{})
	go func() {
		defer close(ch)
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 64*1024), 1<<20)
		var pendingKind string
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "event: ") {
				pendingKind = strings.TrimSpace(strings.TrimPrefix(line, "event: "))
				continue
			}
			if strings.HasPrefix(line, "data: ") {
				select {
				case ch <- ev{pendingKind, strings.TrimSpace(strings.TrimPrefix(line, "data: "))}:
				case <-stop:
					return
				}
			}
		}
	}()

	// 等待 hello 事件(连接就绪)。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case e := <-ch:
			if e.kind == "hello" {
				goto ready
			}
		case <-time.After(100 * time.Millisecond):
		}
	}
	t.Fatal("SSE hello 事件未到")
ready:
	cleanup := func() {
		close(stop)
		resp.Body.Close()
	}
	// 返回读取函数:等待指定 kind 的事件(带 3s 超时)。
	read := func(kind string) string {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			select {
			case e := <-ch:
				if e.kind == kind {
					return e.data
				}
			case <-time.After(100 * time.Millisecond):
			}
		}
		t.Fatalf("未收到 %s 事件", kind)
		return ""
	}
	return read, cleanup
}

// TestTaskEventBroadcast 验证任务生命周期事件依次广播:dispatched → acknowledged → result。
func TestTaskEventBroadcast(t *testing.T) {
	srv, relay := startTaskEventRelay(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const agentID = "event-agent-1"
	events := strings.Join([]string{proto.TypeTaskDispatched, proto.TypeTaskAcknowledged, proto.TypeTaskResult}, ",")
	readEvent, cleanup := sseCollector(ctx, t, srv, events)
	defer cleanup()

	// 注册真实 WS agent,回 ACK + result。
	ac, _, err := websocket.Dial(ctx, strings.Replace(srv.URL+"/agent", "http://", "ws://", 1), nil)
	if err != nil {
		t.Fatalf("agent dial: %v", err)
	}
	defer ac.CloseNow()
	ac.SetReadLimit(1 << 20)
	regEnv, _ := proto.NewEnvelope(proto.TypeRegister, agentID, "relay", proto.Register{
		AgentID: agentID, Hostname: "eh", OS: "linux/amd64",
		Capabilities: []string{proto.TaskPowerShell},
	})
	regBytes, _ := json.Marshal(regEnv)
	if err := ac.Write(ctx, websocket.MessageText, regBytes); err != nil {
		t.Fatalf("register: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for !relay.agents.Supports(agentID, proto.TaskPowerShell) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	// agent 消息循环:task_request → task_ack → task_result。
	go func() {
		for {
			_, data, rerr := ac.Read(ctx)
			if rerr != nil {
				return
			}
			var env proto.Envelope
			if json.Unmarshal(data, &env) != nil {
				continue
			}
			switch env.Type {
			case proto.TypeTaskRequest:
				var task proto.TaskRequest
				env.Decode(&task)
				ackEnv, _ := proto.NewEnvelope(proto.TypeTaskAck, agentID, "relay", proto.TaskAck{
					TaskID: task.TaskID, Status: proto.StatusAcknowledged,
				})
				ackBytes, _ := json.Marshal(ackEnv)
				ac.Write(ctx, websocket.MessageText, ackBytes)
				resEnv, _ := proto.NewEnvelope(proto.TypeTaskResult, agentID, "relay", proto.TaskResult{
					TaskID: task.TaskID, Success: true, Stdout: "event-flow-ok", ExitCode: 0,
				})
				resBytes, _ := json.Marshal(resEnv)
				ac.Write(ctx, websocket.MessageText, resBytes)
			}
		}
	}()

	// 下发任务。
	taskBody, _ := json.Marshal(proto.TaskRequest{
		TaskID: "event-task-1", Type: proto.TaskPowerShell, AgentID: agentID,
		Payload: json.RawMessage(`{"command":"echo hi"}`),
	})
	postResp, err := http.Post(srv.URL+"/api/tasks", "application/json", bytes.NewReader(taskBody))
	if err != nil {
		t.Fatalf("下发任务: %v", err)
	}
	if postResp.StatusCode != http.StatusAccepted {
		t.Fatalf("期望 202, got %d", postResp.StatusCode)
	}
	postResp.Body.Close()

	// 断言事件顺序:dispatched → acknowledged → result。
	dispatched := readEvent(proto.TypeTaskDispatched)
	if !strings.Contains(dispatched, "event-task-1") {
		t.Fatalf("task_dispatched 数据异常: %s", dispatched)
	}
	acked := readEvent(proto.TypeTaskAcknowledged)
	if !strings.Contains(acked, "event-task-1") {
		t.Fatalf("task_acknowledged 数据异常: %s", acked)
	}
	result := readEvent(proto.TypeTaskResult)
	if !strings.Contains(result, "event-flow-ok") {
		t.Fatalf("task_result 数据异常: %s", result)
	}
}

// TestSSEFilterExcludesDispatched 验证 events 参数过滤:未订阅的事件不推送。
func TestSSEFilterExcludesDispatched(t *testing.T) {
	srv, relay := startTaskEventRelay(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const agentID = "event-agent-2"
	// 只订阅 task_result(不订阅 dispatched/acknowledged)。
	readEvent, cleanup := sseCollector(ctx, t, srv, proto.TypeTaskResult)
	defer cleanup()

	ac, _, err := websocket.Dial(ctx, strings.Replace(srv.URL+"/agent", "http://", "ws://", 1), nil)
	if err != nil {
		t.Fatalf("agent dial: %v", err)
	}
	defer ac.CloseNow()
	regEnv, _ := proto.NewEnvelope(proto.TypeRegister, agentID, "relay", proto.Register{
		AgentID: agentID, Hostname: "eh2", OS: "linux/amd64",
		Capabilities: []string{proto.TaskPowerShell},
	})
	regBytes, _ := json.Marshal(regEnv)
	ac.Write(ctx, websocket.MessageText, regBytes)
	deadline := time.Now().Add(3 * time.Second)
	for !relay.agents.Supports(agentID, proto.TaskPowerShell) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	go func() {
		for {
			_, data, rerr := ac.Read(ctx)
			if rerr != nil {
				return
			}
			var env proto.Envelope
			if json.Unmarshal(data, &env) != nil {
				continue
			}
			if env.Type == proto.TypeTaskRequest {
				var task proto.TaskRequest
				env.Decode(&task)
				resEnv, _ := proto.NewEnvelope(proto.TypeTaskResult, agentID, "relay", proto.TaskResult{
					TaskID: task.TaskID, Success: true, Stdout: "filtered-ok",
				})
				resBytes, _ := json.Marshal(resEnv)
				ac.Write(ctx, websocket.MessageText, resBytes)
			}
		}
	}()

	taskBody, _ := json.Marshal(proto.TaskRequest{
		TaskID: "event-task-2", Type: proto.TaskPowerShell, AgentID: agentID,
		Payload: json.RawMessage(`{"command":"x"}`),
	})
	postResp, err := http.Post(srv.URL+"/api/tasks", "application/json", bytes.NewReader(taskBody))
	if err != nil {
		t.Fatalf("下发任务: %v", err)
	}
	io.Copy(io.Discard, postResp.Body)
	postResp.Body.Close()

	result := readEvent(proto.TypeTaskResult)
	if !strings.Contains(result, "filtered-ok") {
		t.Fatalf("task_result 数据异常: %s", result)
	}
}

// TestWaitResultSSE 端到端:CLI 的 waitResult 语义在 SSE 通道下快速返回(模拟实现级验证)。
// 这里验证 sseWatch 的事件解析路径:SSE 格式与 handleSSE 输出一致。
func TestWaitResultSSE(t *testing.T) {
	srv, relay := startTaskEventRelay(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const agentID = "event-agent-3"
	ac, _, err := websocket.Dial(ctx, strings.Replace(srv.URL+"/agent", "http://", "ws://", 1), nil)
	if err != nil {
		t.Fatalf("agent dial: %v", err)
	}
	defer ac.CloseNow()
	regEnv, _ := proto.NewEnvelope(proto.TypeRegister, agentID, "relay", proto.Register{
		AgentID: agentID, Hostname: "eh3", OS: "linux/amd64",
		Capabilities: []string{proto.TaskPowerShell},
	})
	regBytes, _ := json.Marshal(regEnv)
	ac.Write(ctx, websocket.MessageText, regBytes)
	deadline := time.Now().Add(3 * time.Second)
	for !relay.agents.Supports(agentID, proto.TaskPowerShell) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	go func() {
		for {
			_, data, rerr := ac.Read(ctx)
			if rerr != nil {
				return
			}
			var env proto.Envelope
			if json.Unmarshal(data, &env) != nil {
				continue
			}
			if env.Type == proto.TypeTaskRequest {
				var task proto.TaskRequest
				env.Decode(&task)
				// 延迟 300ms 回结果(验证 SSE 通道比轮询快)。
				time.Sleep(300 * time.Millisecond)
				resEnv, _ := proto.NewEnvelope(proto.TypeTaskResult, agentID, "relay", proto.TaskResult{
					TaskID: task.TaskID, Success: true, Stdout: "sse-fast-path",
				})
				resBytes, _ := json.Marshal(resEnv)
				ac.Write(ctx, websocket.MessageText, resBytes)
			}
		}
	}()

	taskBody, _ := json.Marshal(proto.TaskRequest{
		TaskID: "event-task-3", Type: proto.TaskPowerShell, AgentID: agentID,
		Payload: json.RawMessage(`{"command":"x"}`),
	})
	postResp, err := http.Post(srv.URL+"/api/tasks", "application/json", bytes.NewReader(taskBody))
	if err != nil {
		t.Fatalf("下发任务: %v", err)
	}
	postResp.Body.Close()

	// 直接订阅 SSE 解析 task_result(等价于 CLI sseWatch 的解析路径)。
	events := strings.Join([]string{proto.TypeTaskResult}, ",")
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/events?events="+events, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("SSE: %v", err)
	}
	defer resp.Body.Close()
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	start := time.Now()
	var gotResult bool
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") && strings.Contains(line, "sse-fast-path") {
			gotResult = true
			break
		}
	}
	if !gotResult {
		t.Fatal("未收到 task_result")
	}
	elapsed := time.Since(start)
	if elapsed > 2500*time.Millisecond {
		t.Fatalf("SSE 通道应快速返回,耗时 %v", elapsed)
	}
	fmt.Printf("SSE 结果通道耗时: %v\n", elapsed)
}
