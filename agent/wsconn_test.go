package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/QiuShichang/go-rmm/executors"
	"github.com/QiuShichang/go-rmm/proto"
)

// startWSServer 起一个测试用 WS 服务器，accept 后把收到的消息原样回传（echo）。
// 返回 ws:// URL。每条消息会按 JSON 解析，写一条 type=echo 的回声。
func startWSServer(t *testing.T, handler func(env proto.Envelope) proto.Envelope) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx := r.Context()
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			var env proto.Envelope
			if json.Unmarshal(data, &env) != nil {
				continue
			}
			resp := handler(env)
			if resp.Type == "" {
				continue // 不回声
			}
			respBytes, _ := json.Marshal(resp)
			if err := c.Write(ctx, websocket.MessageText, respBytes); err != nil {
				return
			}
		}
	}))
	return srv
}

func wsURL(srv *httptest.Server) string {
	return strings.Replace(srv.URL, "http://", "ws://", 1) + "/agent"
}

// TestWSConnConcurrentWriteSerializes 验证 R1 修复：N 个 goroutine 并发写 wsConn，
// 所有消息都应完整到达服务器（无帧交错损坏）。
func TestWSConnConcurrentWriteSerializes(t *testing.T) {
	var received int64
	srv := startWSServer(t, func(env proto.Envelope) proto.Envelope {
		atomic.AddInt64(&received, 1)
		return proto.Envelope{} // 不回声，仅计数
	})
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, _, err := websocket.Dial(ctx, wsURL(srv), nil)
	if err != nil {
		t.Fatalf("dial 失败: %v", err)
	}
	defer c.CloseNow()
	wc := newWSConn(c)
	agentBizID = "test-agent"

	// 100 个 goroutine 并发写不同消息
	const N = 100
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(i int) {
			defer wg.Done()
			msg := proto.Heartbeat{AgentID: "test", Seq: int64(i)}
			_ = wc.writeEnvelope(ctx, proto.TypeHeartbeat, "relay", msg)
		}(i)
	}
	wg.Wait()

	// 等待服务器收到全部
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt64(&received) == N {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := atomic.LoadInt64(&received); got != N {
		t.Fatalf("期望收到 %d 条，实际 %d 条（并发写有丢失/损坏）", N, got)
	}
}

// TestAgentReconnectAfterDisconnect 验证 Agent 在连接断开后自动重连。
func TestAgentReconnectAfterDisconnect(t *testing.T) {
	var connects int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&connects, 1)
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		// 立即关闭，模拟服务器拒绝/断开
		c.Close(websocket.StatusGoingAway, "test-disconnect")
	}))
	defer srv.Close()

	cfg := DefaultConfig()
	cfg.RelayURL = wsURL(srv)
	cfg.AgentID = "reconnect-test"
	cfg.Heartbeat = 50 * time.Millisecond // 加速心跳

	// 临时缩短退避起点，加速重连测试（全局变量，测试后恢复）
	origInitial := initialBackoff
	origMax := maxBackoff
	initialBackoff = 50 * time.Millisecond
	maxBackoff = 200 * time.Millisecond
	defer func() { initialBackoff = origInitial; maxBackoff = origMax }()

	disp := newTestDispatcher()
	a := NewAgent(cfg, disp)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { _ = a.Run(ctx) }()

	// 等待重连发生（至少 3 次连接）
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&connects) >= 3 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()

	if got := atomic.LoadInt32(&connects); got < 3 {
		t.Fatalf("期望至少 3 次连接（重连），实际 %d 次", got)
	}
}

// newTestDispatcher 返回一个最小 dispatcher（仅注册 echo 执行器）。
func newTestDispatcher() *executors.Dispatcher {
	return executors.New()
}
