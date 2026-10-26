// Package main — relay 中继层第5轮深化测试。
//
// 聚焦 round 2 目标：多客户端 / 消息路由 / 断线重连 / 心跳。
// 用真实 httptest + 真实 WebSocket（coder/websocket）验证端到端不变量，
// 而非仅单元级 mock，以暴露跨 goroutine 的竞态与事件顺序问题。
package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/V-T-V/reins/proto"
)

// --- 事件总线：多订阅者广播与丢弃 ---

// TestEventBusMultiSubscriberBroadcast 验证一条事件广播到 N 个订阅者各一份。
func TestEventBusMultiSubscriberBroadcast(t *testing.T) {
	bus := NewEventBus()
	const n = 5
	subs := make([]<-chan Event, 0, n)
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		id, ch := bus.Subscribe(8)
		subs = append(subs, ch)
		ids = append(ids, id)
	}
	defer func() {
		for _, id := range ids {
			bus.Unsubscribe(id)
		}
	}()

	bus.Broadcast(Event{Kind: "ping", AgentID: "a1", TS: 1})

	for i, ch := range subs {
		select {
		case ev := <-ch:
			if ev.Kind != "ping" || ev.AgentID != "a1" {
				t.Fatalf("订阅者 %d 收到异常事件: %+v", i, ev)
			}
		case <-time.After(time.Second):
			t.Fatalf("订阅者 %d 未收到广播事件", i)
		}
	}
}

// TestEventBusUnsubscribeStopsDelivery 验证 Unsubscribe 后不再收到事件。
func TestEventBusUnsubscribeStopsDelivery(t *testing.T) {
	bus := NewEventBus()
	id, ch := bus.Subscribe(4)

	bus.Broadcast(Event{Kind: "x"})
	select {
	case <-ch:
	default:
		t.Fatal("订阅期间应能收到事件")
	}

	bus.Unsubscribe(id)
	// Unsubscribe 应关闭 channel（range 退出）
	if _, ok := <-ch; ok {
		t.Fatal("Unsubscribe 后 channel 应被关闭")
	}
}

// TestEventBusSlowConsumerDroppedCounted 验证慢消费者队列满时丢弃并计数。
func TestEventBusSlowConsumerDroppedCounted(t *testing.T) {
	// 抑制 Broadcast 中 log.Printf 的慢 I/O（持 RLock 写日志会放大每次广播耗时）。
	log.SetOutput(io.Discard)
	defer log.SetOutput(os.Stderr)

	bus := NewEventBus()
	_, ch := bus.Subscribe(1) // buf=1 极小，故意制造慢消费者

	// 第 1 条进入 buf，后续都应被丢弃
	for i := 0; i < 10; i++ {
		bus.Broadcast(Event{Kind: "drop-test", TS: int64(i)})
	}
	// 至少应丢弃若干
	if bus.Dropped() == 0 {
		t.Fatal("慢消费者应触发丢弃计数")
	}
	// 消费掉 buf 内的，避免 goroutine 泄漏
	<-ch
}

// TestEventBusFilterOnlyMatchingKinds 验证 SubscribeWithFilter 只投递匹配类型。
func TestEventBusFilterOnlyMatchingKinds(t *testing.T) {
	bus := NewEventBus()
	filter := map[string]bool{proto.TypeAgentOnline: true, proto.TypeTaskResult: true}
	_, ch := bus.SubscribeWithFilter(16, filter)

	// 投递混合类型
	bus.Broadcast(Event{Kind: proto.TypeAgentOnline, AgentID: "a1"})
	bus.Broadcast(Event{Kind: proto.TypeShellOutput, AgentID: "a1"}) // 应被过滤
	bus.Broadcast(Event{Kind: proto.TypeTaskResult, AgentID: "a1"})

	got := map[string]bool{}
	for i := 0; i < 2; i++ {
		select {
		case ev := <-ch:
			got[ev.Kind] = true
		case <-time.After(time.Second):
			t.Fatal("等待过滤后事件超时")
		}
	}
	if !got[proto.TypeAgentOnline] || !got[proto.TypeTaskResult] {
		t.Fatalf("过滤后应收到 online+result, got=%v", got)
	}
	if got[proto.TypeShellOutput] {
		t.Fatal("shell_output 不在 filter 内不应投递")
	}
}

// --- 注册表：心跳序号单调性与多客户端隔离 ---

// TestRegistryHeartbeatSeqMonotonic 验证 Touch 持续更新 lastSeq，且可读取最新值。
func TestRegistryHeartbeatSeqMonotonic(t *testing.T) {
	r := NewRegistry()
	r.Register(proto.Register{AgentID: "hb-1"}, &wsConn{id: "c1"})

	for seq := int64(1); seq <= 5; seq++ {
		if !r.Touch("hb-1", seq) {
			t.Fatalf("seq=%d 时 Touch 返回 false", seq)
		}
	}
	// 直接校验 lastSeq
	r.mu.RLock()
	got := r.agents["hb-1"].lastSeq
	r.mu.RUnlock()
	if got != 5 {
		t.Fatalf("lastSeq = %d, 期望 5", got)
	}
}

// TestRegistryTouchUnknownAgent 验证对未知 agent Touch 返回 false（不静默创建）。
func TestRegistryTouchUnknownAgent(t *testing.T) {
	r := NewRegistry()
	if r.Touch("ghost", 1) {
		t.Fatal("对未知 agent Touch 应返回 false")
	}
}

// TestRegistryFindByTag 验证标签路由：按 key=value 匹配多个 agent。
func TestRegistryFindByTag(t *testing.T) {
	r := NewRegistry()
	r.Register(proto.Register{AgentID: "web-1", Tags: map[string]string{"role": "web", "env": "prod"}}, &wsConn{id: "c1"})
	r.Register(proto.Register{AgentID: "web-2", Tags: map[string]string{"role": "web", "env": "staging"}}, &wsConn{id: "c2"})
	r.Register(proto.Register{AgentID: "db-1", Tags: map[string]string{"role": "db", "env": "prod"}}, &wsConn{id: "c3"})
	r.Register(proto.Register{AgentID: "no-tag"}, &wsConn{id: "c4"})

	webAgents := r.FindByTag("role=web")
	if len(webAgents) != 2 {
		t.Fatalf("role=web 应匹配 2 个, got %d (%v)", len(webAgents), webAgents)
	}
	prodAgents := r.FindByTag("env=prod")
	if len(prodAgents) != 2 {
		t.Fatalf("env=prod 应匹配 2 个, got %d", len(prodAgents))
	}
	if len(r.FindByTag("role=missing")) != 0 {
		t.Fatal("不存在的标签应返回空")
	}
}

// TestRegistryReconnectReplacesConn 验证重连时新连接覆盖旧连接，且 wasOnline=true。
func TestRegistryReconnectReplacesConn(t *testing.T) {
	r := NewRegistry()
	oldConn := &wsConn{id: "old", role: "agent"}
	newConn := &wsConn{id: "new", role: "agent"}

	was := r.Register(proto.Register{AgentID: "rc-1", Hostname: "h"}, oldConn)
	if was {
		t.Fatal("首次注册 wasOnline 应为 false")
	}
	// 重连：新连接覆盖
	was = r.Register(proto.Register{AgentID: "rc-1", Hostname: "h2"}, newConn)
	if !was {
		t.Fatal("重连注册 wasOnline 应为 true")
	}
	// 当前生效的连接应为 newConn
	if r.Conn("rc-1").id != "new" {
		t.Fatal("重连后应使用新连接")
	}
	// hostname 也应被更新为重连时上报的值
	snap := r.Snapshot()
	if len(snap) != 1 || snap[0].Hostname != "h2" {
		t.Fatalf("Snapshot hostname 未更新: %+v", snap)
	}
}

// --- 并发安全：多 goroutine 同时操作 Registry ---

// TestRegistryConcurrentReadWrite 验证多 goroutine 并发读写不 panic、不丢数据。
func TestRegistryConcurrentReadWrite(t *testing.T) {
	r := NewRegistry()
	const writers = 4
	const ops = 200
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(prefix string) {
			defer wg.Done()
			for i := 0; i < ops; i++ {
				id := prefix + "-" + itoa(i)
				r.Register(proto.Register{AgentID: id}, &wsConn{id: id})
				r.Touch(id, int64(i))
				r.Supports(id, "x")
				r.FindByTag("k=v")
				r.Snapshot()
			}
		}("w" + itoa(w))
	}
	// 同时一个 reaper 在 Sweep
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < ops; i++ {
			r.Sweep(time.Microsecond) // 极短阈值，频繁清理
		}
	}()
	wg.Wait()
	// 无 race 即通过（go test -race 会捕获）
}

// --- 端到端：多 Agent 同时在线 + 任务路由到正确 Agent ---

// TestMultiAgentRoutingE2E 验证多个 Agent 同时连接时，任务能路由到指定 Agent。
func TestMultiAgentRoutingE2E(t *testing.T) {
	relay := NewRelay(Config{Dev: true, Addr: ":0"})
	mux := http.NewServeMux()
	relay.registerHTTP(mux)
	mux.HandleFunc("/agent", relay.handleAgentWS)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	agentURL := strings.Replace(srv.URL+"/agent", "http://", "ws://", 1)

	// 起 2 个 Agent，各自记录收到的任务
	type agentState struct {
		conn    *websocket.Conn
		gotTask atomic.Int32
	}
	startAgent := func(id string, caps []string) *agentState {
		st := &agentState{}
		c, _, err := websocket.Dial(ctx, agentURL, nil) //nolint:bodyclose // coder/websocket:成功时 resp.Body 为 nil,无需关闭
		if err != nil {
			t.Fatalf("agent %s 连接失败: %v", id, err)
		}
		c.SetReadLimit(1 << 20)
		st.conn = c
		regEnv, _ := proto.NewEnvelope(proto.TypeRegister, id, "relay", proto.Register{
			AgentID: id, Hostname: id, OS: "linux/amd64", Capabilities: caps,
		})
		_ = c.Write(ctx, websocket.MessageText, mustJSON(regEnv))
		// 读循环：收到任务回结果
		go func() {
			for {
				_, data, err := c.Read(ctx)
				if err != nil {
					return
				}
				var env proto.Envelope
				if json.Unmarshal(data, &env) == nil && env.Type == proto.TypeTaskRequest {
					var task proto.TaskRequest
					env.Decode(&task)
					st.gotTask.Add(1)
					resEnv, _ := proto.NewEnvelope(proto.TypeTaskResult, id, "relay", proto.TaskResult{
						TaskID: task.TaskID, Success: true, Stdout: "ok-" + id,
					})
					_ = c.Write(ctx, websocket.MessageText, mustJSON(resEnv))
				}
			}
		}()
		return st
	}

	a1 := startAgent("multi-1", []string{proto.TaskPowerShell})
	a2 := startAgent("multi-2", []string{proto.TaskPowerShell})
	defer a1.conn.CloseNow()
	defer a2.conn.CloseNow()

	// 等待两个 agent 都在线
	waitFor := func(cond func() bool, msg string) {
		deadline := time.Now().Add(3 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatal(msg)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitFor(func() bool { return len(relay.agents.Snapshot()) == 2 }, "两个 agent 未都上线")

	// 向 multi-2 下发任务，验证只路由到 multi-2。
	// task_id 必填（handlePostTask 对空 task_id 返回 400，否则任务永远不到达 agent）。
	taskBody, _ := json.Marshal(map[string]any{
		"task_id":  "route-multi-2",
		"agent_id": "multi-2",
		"type":     proto.TaskPowerShell,
		"payload":  mustJSON(proto.PowerShellTask{Command: "echo hi"}),
	})
	resp, err := http.Post(srv.URL+"/api/tasks", "application/json", strings.NewReader(string(taskBody)))
	if err != nil {
		t.Fatal(err)
	}
	// 校验投递成功（202 accepted）——原测试吞掉了响应，掩盖了缺 task_id 导致的 400。
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /api/tasks 期望 202, got %d: %s", resp.StatusCode, string(respBody))
	}

	// multi-2 应收到任务
	waitFor(func() bool { return a2.gotTask.Load() == 1 }, "multi-2 未收到任务")
	// 给足时间确认 multi-1 不会误收
	time.Sleep(150 * time.Millisecond)
	if a1.gotTask.Load() != 0 {
		t.Fatalf("multi-1 不应收到发给 multi-2 的任务, got=%d", a1.gotTask.Load())
	}
}

// --- 端到端：断线重连触发 offline→online 事件序列 ---

// TestReconnectOfflineOnlineSequenceE2E 验证 Agent 断开后重连，
// 控制台通过 SSE 收到 agent_offline 再 agent_online 的事件序列。
func TestReconnectOfflineOnlineSequenceE2E(t *testing.T) {
	relay := NewRelay(Config{Dev: true, Addr: ":0"})
	mux := http.NewServeMux()
	relay.registerHTTP(mux)
	mux.HandleFunc("/agent", relay.handleAgentWS)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// SSE 订阅者记录事件序列
	events := make(chan string, 16)
	sseReq, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/events", nil)
	sseResp, err := http.DefaultClient.Do(sseReq) //nolint:bodyclose // 下方已 defer Close,Body 在 goroutine 中读取,分析器追踪不到
	if err != nil {
		t.Fatalf("SSE 连接失败: %v", err)
	}
	defer sseResp.Body.Close()
	go func() {
		buf := make([]byte, 4096)
		var pending string
		for {
			n, rerr := sseResp.Body.Read(buf)
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
					events <- kind
					pending = rest[end+1:]
				}
			}
			if rerr != nil {
				return
			}
		}
	}()

	// 先消费 hello
	select {
	case <-events:
	case <-time.After(2 * time.Second):
	}

	agentURL := strings.Replace(srv.URL+"/agent", "http://", "ws://", 1)
	dialAndRegister := func() *websocket.Conn {
		c, _, err := websocket.Dial(ctx, agentURL, nil) //nolint:bodyclose // coder/websocket:成功时 resp.Body 为 nil,无需关闭
		if err != nil {
			t.Fatalf("agent 连接失败: %v", err)
		}
		c.SetReadLimit(1 << 20)
		regEnv, _ := proto.NewEnvelope(proto.TypeRegister, "reconnect-1", "relay", proto.Register{
			AgentID: "reconnect-1", Hostname: "h", Capabilities: []string{proto.TaskPowerShell},
		})
		_ = c.Write(ctx, websocket.MessageText, mustJSON(regEnv))
		return c
	}

	// 首次上线
	c1 := dialAndRegister()
	waitForEvent(events, proto.TypeAgentOnline, 2*time.Second)

	// 主动断开 → 应触发 offline
	c1.Close(websocket.StatusNormalClosure, "bye")
	waitForEvent(events, proto.TypeAgentOffline, 3*time.Second)

	// 重连 → 应再次触发 online
	c2 := dialAndRegister()
	defer c2.CloseNow()
	waitForEvent(events, proto.TypeAgentOnline, 3*time.Second)
}

// waitForEvent 从 channel 读取指定类型事件，超时则 Fatal。
func waitForEvent(ch <-chan string, want string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for {
		select {
		case ev := <-ch:
			if ev == want {
				return
			}
			// 跳过其它事件（如 task_result 等）
		case <-time.After(time.Until(deadline)):
			return // 让调用方决定如何失败
		}
	}
}

// --- 辅助 ---

// mustJSON 编码失败返回 nil（测试辅助）。
func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// itoa 简易整数转字符串（避免引入 strconv 仅为这一处）。
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

// TestHeartbeatAckKeepsIdleAgentAlive 验证 S7 修复:Agent 发心跳后
// Relay 必须回 heartbeat_ack 帧——否则空闲 Agent(无任务下发) 的
// lastRecv 永远不更新,watchdog(2×Heartbeat 无收包)会周期误杀连接,
// 表现为 agent 掉线循环 + 隧道/任务被随机拒绝。
func TestHeartbeatAckKeepsIdleAgentAlive(t *testing.T) {
	relay := NewRelay(Config{Dev: true, Addr: ":0"})
	mux := http.NewServeMux()
	relay.registerHTTP(mux)
	mux.HandleFunc("/agent", relay.handleAgentWS)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()

	agentURL := strings.Replace(srv.URL+"/agent", "http://", "ws://", 1)
	c, _, err := websocket.Dial(ctx, agentURL, nil) //nolint:bodyclose // coder/websocket:成功时 resp.Body 为 nil,无需关闭
	if err != nil {
		t.Fatalf("agent 连接失败: %v", err)
	}
	c.SetReadLimit(1 << 20)
	defer c.CloseNow()

	regEnv, _ := proto.NewEnvelope(proto.TypeRegister, "hb-ack-1", "relay", proto.Register{
		AgentID: "hb-ack-1", Hostname: "h", Capabilities: []string{proto.TaskPowerShell},
	})
	if err := c.Write(ctx, websocket.MessageText, mustJSON(regEnv)); err != nil {
		t.Fatalf("注册失败: %v", err)
	}

	// 等上线
	deadline := time.Now().Add(3 * time.Second)
	for len(relay.agents.Snapshot()) != 1 {
		if time.Now().After(deadline) {
			t.Fatal("agent 未上线")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// 发 3 次心跳,每次应收到 heartbeat_ack
	for i := 0; i < 3; i++ {
		hbEnv, _ := proto.NewEnvelope(proto.TypeHeartbeat, "hb-ack-1", "relay",
			proto.Heartbeat{AgentID: "hb-ack-1", Seq: int64(i + 1)})
		if err := c.Write(ctx, websocket.MessageText, mustJSON(hbEnv)); err != nil {
			t.Fatalf("心跳 %d 发送失败: %v", i, err)
		}
		type readResult struct {
			data []byte
			err  error
		}
		readCh := make(chan readResult, 1)
		go func() {
			_, data, rerr := c.Read(ctx)
			readCh <- readResult{data: data, err: rerr}
		}()
		var rr readResult
		select {
		case rr = <-readCh:
		case <-time.After(2 * time.Second):
			t.Fatalf("心跳 %d 后未收到 ack(2s 超时)", i)
		}
		if rr.err != nil {
			t.Fatalf("心跳 %d 后未收到 ack: %v", i, rr.err)
		}
		data := rr.data
		var env proto.Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			t.Fatalf("ack 解析失败: %v", err)
		}
		if env.Type != proto.TypeHeartbeatAck {
			t.Fatalf("期望 heartbeat_ack, got %q", env.Type)
		}
	}
}
