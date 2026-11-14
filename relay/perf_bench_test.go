package main

// 性能优化轮(2026-11-14)基准套件——5 条热路径:
//
//  1. WS 中继消息转发吞吐(真实 WS 环回:agent→serveAgent→EventBus)
//  2. 任务结果序列化/处理管线(serveAgent 每结果的工作量,除 store/网络)
//  3. 审计 hash chain 追加(writeEntry 全工作量:双 marshal+sha256+落盘)
//  4. tasks/audit 查询过滤分页(昨夜新增端点的完整 handler 路径)
//  5. agent 心跳处理(环回 RTT + 纯管线两口径)
//
// 全部走真实代码路径:handler 直调(httptest Recorder)或真实 WS 环回
// (httptest.NewServer + websocket.Dial),禁止 mock 掉被测逻辑。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/V-T-V/reins/proto"
)

// --- 1. WS 中继消息转发吞吐(真实环回) ---

// benchLoopbackRelay 起一个真实 Relay(httptest + 真 WS),返回 server、agent 连接与 Relay。
// 调用方负责 srv.Close / conn.CloseNow。
func benchLoopbackRelay(b *testing.B, agentID string) (*httptest.Server, *websocket.Conn, *Relay) {
	b.Helper()
	relay := NewRelay(Config{Dev: true, Addr: ":0"})
	mux := http.NewServeMux()
	relay.registerHTTP(mux)
	mux.HandleFunc("/agent", relay.handleAgentWS)
	srv := httptest.NewServer(mux)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	b.Cleanup(cancel)

	agentURL := strings.Replace(srv.URL+"/agent", "http://", "ws://", 1)
	c, _, err := websocket.Dial(ctx, agentURL, nil) //nolint:bodyclose // 成功时 resp.Body 为 nil
	if err != nil {
		srv.Close()
		b.Fatalf("agent 连接失败: %v", err)
	}
	c.SetReadLimit(32 << 20)
	regEnv, _ := proto.NewEnvelope(proto.TypeRegister, agentID, "relay", proto.Register{
		AgentID: agentID, Hostname: agentID, OS: "linux/amd64",
		Capabilities: []string{proto.TaskPowerShell},
	})
	if err := c.Write(ctx, websocket.MessageText, mustJSON(regEnv)); err != nil {
		b.Fatalf("register 写失败: %v", err)
	}
	// 等 register 被 serveAgent 处理完(在线可见)
	deadline := time.Now().Add(5 * time.Second)
	for len(relay.agents.Snapshot()) == 0 {
		if time.Now().After(deadline) {
			b.Fatal("agent 未在 5s 内上线")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return srv, c, relay
}

// BenchmarkRelayWSForwardLoopback agent→Relay→EventBus 转发吞吐。
// 预 marshal 单条 task_progress 帧重复发送——度量的是 Relay 侧处理
// (Read/Unmarshal/Decode/Broadcast),剥离 agent 侧序列化噪声。
// 订阅者 1 个(单 Console 场景);丢失即 Fatal(Broadcast 丢弃=假绿)。
func BenchmarkRelayWSForwardLoopback(b *testing.B) {
	srv, c, relay := benchLoopbackRelay(b, "bench-fwd")
	defer srv.Close()
	defer c.CloseNow()

	id, events := relay.console.Subscribe(8192)
	defer relay.console.Unsubscribe(id)

	frame := mustJSON(mustNewEnvelopeBench(proto.TypeTaskProgress, "bench-fwd", "relay", proto.TaskProgress{
		TaskID: "bench-task", Step: "run", Message: strings.Repeat("x", 120), Percent: 50,
	}))
	b.SetBytes(int64(len(frame)))
	ctx := context.Background()

	var got atomic.Int64
	go func() {
		for range events {
			got.Add(1)
		}
	}()

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := c.Write(ctx, websocket.MessageText, frame); err != nil {
			b.Fatalf("写失败: %v", err)
		}
	}
	// 等全部事件落地(发送方受 TCP 背压,relay 处理完才有剩余容量)
	deadline := time.Now().Add(60 * time.Second)
	for got.Load() < int64(b.N) {
		if time.Now().After(deadline) {
			b.Fatalf("事件丢失: got %d want %d(EventBus 丢弃?)", got.Load(), b.N)
		}
		time.Sleep(time.Millisecond)
	}
	b.StopTimer()
	if got.Load() != int64(b.N) {
		b.Fatalf("事件数不符: got %d want %d", got.Load(), b.N)
	}
}

// BenchmarkRelayHeartbeatLoopback 心跳全路径环回 RTT:
// agent 发 heartbeat → serveAgent(TouchWithStats + NewEnvelope(ack) + writeControlJSON)
// → agent 收 heartbeat_ack。含 wsx 优先级写引擎的每消息 done channel 分配。
func BenchmarkRelayHeartbeatLoopback(b *testing.B) {
	srv, c, _ := benchLoopbackRelay(b, "bench-hb")
	defer srv.Close()
	defer c.CloseNow()

	frame := mustJSON(mustNewEnvelopeBench(proto.TypeHeartbeat, "bench-hb", "relay", proto.Heartbeat{
		AgentID: "bench-hb", Seq: 1, UploadBytes: 12345, DownloadBytes: 6789,
		UploadCount: 3, DownloadCount: 2,
	}))
	ctx := context.Background()

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := c.Write(ctx, websocket.MessageText, frame); err != nil {
			b.Fatalf("heartbeat 写失败: %v", err)
		}
		// 等 heartbeat_ack(控制优先级,P0)
		msgType, data, err := c.Read(ctx)
		if err != nil {
			b.Fatalf("ack 读失败: %v", err)
		}
		if msgType != websocket.MessageText {
			b.Fatalf("期望文本帧,收到二进制")
		}
		var env proto.Envelope
		if err := json.Unmarshal(data, &env); err != nil || env.Type != proto.TypeHeartbeatAck {
			b.Fatalf("期望 heartbeat_ack,收到 type=%q err=%v", env.Type, err)
		}
	}
}

// mustNewEnvelopeBench 测试辅助:NewEnvelope 失败直接 Fatal 语义(此处 panic,bench 不会触发)。
func mustNewEnvelopeBench(typ, from, to string, payload any) proto.Envelope {
	env, err := proto.NewEnvelope(typ, from, to, payload)
	if err != nil {
		panic(err)
	}
	return env
}

// --- 2. 任务结果序列化/处理管线(serveAgent 每结果的工作量) ---

// BenchmarkTaskResultPipeline serveAgent 处理一条 task_result 的完整 CPU 工作
// (除 store 持久化与网络写):Unmarshal 信封 → onTaskResult(Decode + 广播)。
// 性能轮 2026-11-14 起直调真实 onTaskResult(不测镜像),广播载荷走 env.Payload
// 原始字节——重复 marshal 消除后的真实每结果成本。
func BenchmarkTaskResultPipeline(b *testing.B) {
	result := proto.TaskResult{
		TaskID: "bench-result", Success: true,
		Stdout: strings.Repeat("x", 1024), ExitCode: 0, Duration: 523,
	}
	frame := mustJSON(mustNewEnvelopeBench(proto.TypeTaskResult, "a1", "relay", result))
	relay := NewRelay(Config{Dev: true})

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		var env proto.Envelope
		if err := json.Unmarshal(frame, &env); err != nil {
			b.Fatal(err)
		}
		relay.onTaskResult("a1", &env)
	}
}

// --- 3. 审计 hash chain 追加 ---

// BenchmarkAuditHashChainAppend writeEntry 全工作量:
// sha256(prev + json(entry)) + json(rec) + file.Write(每条一次 syscall,无 bufio)。
// 直调 writeEntry(绕过异步 channel——channel 只影响时延不影响单条成本,
// 且 writeLoop 串行消费,单条成本即吞吐上限)。
func BenchmarkAuditHashChainAppend(b *testing.B) {
	dir := b.TempDir()
	a, err := OpenAuditLog(filepath.Join(dir, "audit-bench.jsonl"))
	if err != nil {
		b.Fatal(err)
	}
	// 等价于 Close 但排空后不再写;benchmark 期间直调 writeEntry
	a.closeOnce.Do(func() {
		a.closed = true
		close(a.closeCh)
	})
	time.Sleep(150 * time.Millisecond) // 等 writeLoop 退出
	defer a.file.Close()

	entry := AuditEntry{
		Event: "task_result", AgentID: "a1", TaskID: "t-bench",
		Detail: "success=true exit=0 duration=523ms",
	}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		entry.TS = int64(1700000000000 + i) // 避免全零 ts(会被填 now,额外 time.Now)
		a.writeEntry(entry)
	}
}

// --- 4. tasks/audit 查询过滤分页(handler 完整路径) ---

// seedTasksForBench 填充 N 个任务(5 agent × 2 类型交错,全 completed 除最后 20 个 dispatched)。
func seedTasksForBench(b *testing.B, s *Store, n int) {
	b.Helper()
	for i := 0; i < n; i++ {
		status := StatusCompleted
		if i >= n-20 {
			status = StatusDispatched
		}
		task := proto.TaskRequest{
			TaskID:  fmt.Sprintf("task-%06d", i),
			Type:    []string{proto.TaskPowerShell, proto.TaskScript}[i%2],
			AgentID: fmt.Sprintf("agent-%d", i%5),
			Payload: json.RawMessage(`{"command":"Get-Process | Select-Object -First 5"}`),
		}
		if err := s.SaveTask(StoredTask{
			Request: task, Status: status,
			CreatedAt: 1700000000000 + int64(i)*1000,
			UpdatedAt: 1700000000000 + int64(i)*1000,
		}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkTasksQueryFilterPage GET /api/tasks?agent_id=agent-1&type=powershell&limit=50
// —— 2000 任务规模,handler 全路径(ListTasks 20000 → 内存过滤 → 排序 → 分页 → writeJSON)。
func BenchmarkTasksQueryFilterPage(b *testing.B) {
	dir := b.TempDir()
	s, err := OpenStore(filepath.Join(dir, "tasks-bench.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	seedTasksForBench(b, s, 2000)

	relay := NewRelay(Config{Dev: true}).WithStore(s)

	req := httptest.NewRequest(http.MethodGet, "/api/tasks?agent_id=agent-1&type=powershell&limit=50&offset=10", nil)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		w := httptest.NewRecorder()
		relay.handleListTasks(w, req)
		if w.Code != http.StatusOK {
			b.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
	}
}

// BenchmarkTasksQueryStatusPage GET /api/tasks?status=completed&limit=50
// —— 状态索引路径(F1),对照上一条全量+过滤路径。
func BenchmarkTasksQueryStatusPage(b *testing.B) {
	dir := b.TempDir()
	s, err := OpenStore(filepath.Join(dir, "tasks-bench-st.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	seedTasksForBench(b, s, 2000)

	relay := NewRelay(Config{Dev: true}).WithStore(s)

	req := httptest.NewRequest(http.MethodGet, "/api/tasks?status=completed&limit=50", nil)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		w := httptest.NewRecorder()
		relay.handleListTasks(w, req)
		if w.Code != http.StatusOK {
			b.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
	}
}

// seedAuditForBench 直调 writeEntry 填充 n 条审计记录(事件/agent 交错,ts 递增)。
func seedAuditForBench(b *testing.B, path string, n int) {
	b.Helper()
	a, err := OpenAuditLog(path)
	if err != nil {
		b.Fatal(err)
	}
	a.closeOnce.Do(func() {
		a.closed = true
		close(a.closeCh)
	})
	time.Sleep(150 * time.Millisecond)
	events := []string{"task_submitted", "task_result", "task_failed", "policy_blocked", "agent_online"}
	base := time.Now().Add(-24 * time.Hour).UnixMilli()
	for i := 0; i < n; i++ {
		a.writeEntry(AuditEntry{
			TS:      base + int64(i)*1000,
			Event:   events[i%len(events)],
			AgentID: fmt.Sprintf("agent-%d", i%5),
			TaskID:  fmt.Sprintf("task-%06d", i),
			Type:    proto.TaskPowerShell,
			Detail:  "bench seed entry with some detail text",
		})
	}
	if err := a.file.Close(); err != nil {
		b.Fatal(err)
	}
}

// BenchmarkAuditQueryFilterPage GET /api/audit/query?event=task_result&agent_id=agent-1&since=24h&limit=50
// —— 20000 条审计记录规模,handler 全路径(readAuditFile → 过滤 → 逆序排序 → 分页 → writeJSON)。
func BenchmarkAuditQueryFilterPage(b *testing.B) {
	dir := b.TempDir()
	path := filepath.Join(dir, "audit-bench.jsonl")
	seedAuditForBench(b, path, 20000)

	relay := NewRelay(Config{Dev: true})
	var openErr error
	relay.audit, openErr = OpenAuditLog(path)
	if openErr != nil {
		b.Fatal(openErr)
	}
	defer relay.audit.Close()

	req := httptest.NewRequest(http.MethodGet,
		"/api/audit/query?event=task_result&agent_id=agent-1&since=24h&limit=50&offset=0", nil)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		w := httptest.NewRecorder()
		relay.handleAuditQuery(w, req)
		if w.Code != http.StatusOK {
			b.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
	}
}

// BenchmarkAuditQueryNoFilter 无过滤全量分页(最重路径:20000 条全匹配 → 全量排序)。
func BenchmarkAuditQueryNoFilter(b *testing.B) {
	dir := b.TempDir()
	path := filepath.Join(dir, "audit-bench-all.jsonl")
	seedAuditForBench(b, path, 20000)

	relay := NewRelay(Config{Dev: true})
	var openErr error
	relay.audit, openErr = OpenAuditLog(path)
	if openErr != nil {
		b.Fatal(openErr)
	}
	defer relay.audit.Close()

	req := httptest.NewRequest(http.MethodGet, "/api/audit/query?limit=50", nil)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		w := httptest.NewRecorder()
		relay.handleAuditQuery(w, req)
		if w.Code != http.StatusOK {
			b.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
	}
}

// --- 5. agent 心跳处理(纯管线口径) ---

// BenchmarkHeartbeatPipeline serveAgent 处理一条心跳的 CPU 工作(除网络写):
// Unmarshal 信封 → Decode → TouchWithStats → NewEnvelope(ack) → writeControlJSON 的 marshal。
func BenchmarkHeartbeatPipeline(b *testing.B) {
	srv, _, relay := benchLoopbackRelay(b, "bench-hb-pipe")
	defer srv.Close()

	frame := mustJSON(mustNewEnvelopeBench(proto.TypeHeartbeat, "bench-hb-pipe", "relay", proto.Heartbeat{
		AgentID: "bench-hb-pipe", Seq: 42, UploadBytes: 999, DownloadBytes: 888,
	}))
	ctx := context.Background()

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		var env proto.Envelope
		if err := json.Unmarshal(frame, &env); err != nil {
			b.Fatal(err)
		}
		var hb proto.Heartbeat
		if err := env.Decode(&hb); err != nil {
			b.Fatal(err)
		}
		relay.agents.TouchWithStats(hb)
		ackCtx, ackCancel := context.WithTimeout(ctx, 5*time.Second)
		_ = ackCtx // 保留分配不计值——真实路径每心跳一次 cancelCtx+timer,计入成本
		if ackEnv, ackErr := proto.NewEnvelope(proto.TypeHeartbeatAck, "relay", "bench-hb-pipe", hb); ackErr == nil {
			_ = relayMustMarshalAck(ackEnv)
		}
		ackCancel()
	}
}

// relayMustMarshalAck 复刻 wsConn.writeControlJSON 的 marshal 步骤(不写网络)。
func relayMustMarshalAck(env proto.Envelope) error {
	_, err := json.Marshal(env)
	return err
}
