// relay/tunnel_ws_test.go
// P1-A:隧道 WebSocket 全双工测试——E2E 双向数据、心跳 Ping/Pong、判死清理、Agent 拒绝路径。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/QiuShichang/go-rmm/proto"
)

// startTunnelWSTestRelay 启动带 WS 隧道端点的测试 Relay(心跳参数可调)。
func startTunnelWSTestRelay(t *testing.T, pingInterval, deadline time.Duration) (*httptest.Server, *Relay) {
	t.Helper()
	relay := NewRelay(Config{Dev: true, Addr: ":0"})
	relay.tunnelPingInterval = pingInterval
	relay.tunnelDeadline = deadline
	mux := http.NewServeMux()
	relay.registerHTTP(mux)
	mux.HandleFunc("/agent", relay.handleAgentWS)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, relay
}

// wsTunnelClient 连接 WS 隧道端点,返回连接 + open 结果。
func wsTunnelClient(t *testing.T, ctx context.Context, srv *httptest.Server, tunnelID, agentID, target string) (*websocket.Conn, error) {
	t.Helper()
	u := srv.URL + "/api/tunnel/ws?tunnel_id=" + tunnelID + "&agent_id=" + agentID + "&target=" + target
	wsURL := strings.Replace(u, "http://", "ws://", 1)
	c, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { c.CloseNow() })
	return c, nil
}

// wsMockAgent 处理 tunnel_open → 回 ready + 立即推首包;收集 CLI 数据;统计 close/RST。
func wsMockAgent(t *testing.T, ctx context.Context, conn *websocket.Conn, tunnelID string, firstPacket []byte) (gotData *atomic.Int32, closes *atomic.Int32, done chan struct{}) {
	t.Helper()
	gotData = &atomic.Int32{}
	closes = &atomic.Int32{}
	done = make(chan struct{})
	go func() {
		defer close(done)
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var env proto.Envelope
			if json.Unmarshal(data, &env) == nil {
				switch env.Type {
				case proto.TypeTunnelOpen:
					readyEnv, _ := proto.NewEnvelope(proto.TypeTunnelReady, "tunnel-agent", "relay", proto.TunnelReady{
						TunnelID: tunnelID, Accepted: true, Reason: "ok",
					})
					_ = conn.Write(ctx, websocket.MessageText, mustJSON(readyEnv))
					if len(firstPacket) > 0 {
						frame := proto.BuildTunnelFrame(proto.ChanTypeTunnelData, tunnelID, 0, firstPacket)
						_ = conn.Write(ctx, websocket.MessageBinary, frame)
					}
				case proto.TypeTunnelClose:
					closes.Add(1)
					continue
				}
				continue
			}
			if tid, _, _, payload, err := proto.ParseTunnelFrame(data); err == nil && tid == tunnelID {
				gotData.Add(int32(len(payload)))
			}
		}
	}()
	return gotData, closes, done
}

// TestTunnelWSE2EBidirectional 核心:E2E 双向数据 + close 信令。
func TestTunnelWSE2EBidirectional(t *testing.T) {
	srv, relay := startTunnelWSTestRelay(t, time.Hour, time.Hour) // 心跳不干扰
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const agentID = "ws-agent-1"
	const tunnelID = "tun-ws-001"
	agentConn := dialTunnelTestAgent(t, ctx, srv, relay, agentID)

	first := []byte("HTTP/1.1 200 OK\r\n\r\nws-first")
	gotData, closes, _ := wsMockAgent(t, ctx, agentConn, tunnelID, first)

	// CLI 侧连 WS 隧道端点(握手即 open,无竞态窗口)。
	cli, err := wsTunnelClient(t, ctx, srv, tunnelID, agentID, "127.0.0.1:8000")
	if err != nil {
		t.Fatalf("WS 隧道握手失败: %v", err)
	}

	// 1. CLI 收到 Agent 首包(open 后立即到达,无丢失)。
	_, data, err := cli.Read(ctx)
	if err != nil {
		t.Fatalf("读首包: %v", err)
	}
	_, chanType, _, payload, perr := proto.ParseTunnelFrame(data)
	if perr != nil || chanType != proto.ChanTypeTunnelData || !bytes.Equal(payload, first) {
		t.Fatalf("首包异常: chanType=%x payload=%q want %q", chanType, payload, first)
	}

	// 2. CLI→Agent 方向:发数据帧。
	cliData := []byte("cli-ws-data")
	if err := cli.Write(ctx, websocket.MessageBinary,
		proto.BuildTunnelFrame(proto.ChanTypeTunnelData, tunnelID, 0, cliData)); err != nil {
		t.Fatalf("CLI 写: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for gotData.Load() != int32(len(cliData)) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if gotData.Load() != int32(len(cliData)) {
		t.Fatalf("Agent 未收到 CLI 数据: got %d want %d", gotData.Load(), len(cliData))
	}

	// 3. Agent 再发一帧,CLI 持续收到。
	second := []byte("ws-second")
	if err := agentConn.Write(ctx, websocket.MessageBinary,
		proto.BuildTunnelFrame(proto.ChanTypeTunnelData, tunnelID, 0, second)); err != nil {
		t.Fatalf("Agent 写: %v", err)
	}
	_, data, err = cli.Read(ctx)
	if err != nil {
		t.Fatalf("读第二帧: %v", err)
	}
	if !bytes.Contains(data, second) {
		t.Fatalf("第二帧异常: %q want 含 %q", data, second)
	}

	// 4. CLI 发 close 信令 → Agent 收到 tunnel_close + Relay 清理。
	closeEnv, _ := proto.NewEnvelope(proto.TypeTunnelClose, "console", agentID, proto.TunnelClose{
		TunnelID: tunnelID, Reason: "test close",
	})
	closeJSON, _ := json.Marshal(closeEnv)
	if err := cli.Write(ctx, websocket.MessageText, closeJSON); err != nil {
		t.Fatalf("CLI close 信令: %v", err)
	}
	deadline2 := time.Now().Add(3 * time.Second)
	for closes.Load() == 0 && time.Now().Before(deadline2) {
		time.Sleep(10 * time.Millisecond)
	}
	if closes.Load() == 0 {
		t.Fatal("Agent 未收到 tunnel_close")
	}
	// Relay 应关闭 WS 连接(读返回错误)。
	if _, _, err := cli.Read(ctx); err == nil {
		t.Fatal("close 后 WS 连接应被 Relay 关闭")
	}
}

// TestTunnelWSPingPong 心跳:Ping 后回 Pong 保活,连接不断。
func TestTunnelWSPingPong(t *testing.T) {
	srv, relay := startTunnelWSTestRelay(t, 200*time.Millisecond, 2*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const agentID = "ws-agent-2"
	const tunnelID = "tun-ws-002"
	agentConn := dialTunnelTestAgent(t, ctx, srv, relay, agentID)
	wsMockAgent(t, ctx, agentConn, tunnelID, []byte("pong-test"))

	cli, err := wsTunnelClient(t, ctx, srv, tunnelID, agentID, "127.0.0.1:8000")
	if err != nil {
		t.Fatalf("WS 握手: %v", err)
	}

	// 连续收帧,遇 Ping 回 Pong,持续 1.2s(> 3 个 Ping 周期)。
	gotPing := 0
	doneAt := time.Now().Add(1200 * time.Millisecond)
	for time.Now().Before(doneAt) {
		_, data, rerr := cli.Read(ctx)
		if rerr != nil {
			t.Fatalf("连接被断开(心跳保活失败): %v", rerr)
		}
		if !proto.IsTunnelFrame(data) {
			continue
		}
		_, chanType, flags, _, perr := proto.ParseTunnelFrame(data)
		if perr != nil {
			continue
		}
		if chanType == proto.ChanTypeTunnelCtrl && flags == proto.TunnelCtrlPing {
			gotPing++
			if err := cli.Write(ctx, websocket.MessageBinary,
				proto.BuildTunnelFrame(proto.ChanTypeTunnelCtrl, tunnelID, proto.TunnelCtrlPong, nil)); err != nil {
				t.Fatalf("回 Pong: %v", err)
			}
		}
	}
	if gotPing < 3 {
		t.Fatalf("应收到至少 3 个 Ping(200ms 间隔 × 1.2s),got %d", gotPing)
	}
}

// TestTunnelWSHeartbeatDeadline 判死:不回 Pong 且无数据 → Relay 超时关闭。
func TestTunnelWSHeartbeatDeadline(t *testing.T) {
	srv, relay := startTunnelWSTestRelay(t, 200*time.Millisecond, 600*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const agentID = "ws-agent-3"
	const tunnelID = "tun-ws-003"
	agentConn := dialTunnelTestAgent(t, ctx, srv, relay, agentID)
	wsMockAgent(t, ctx, agentConn, tunnelID, []byte("deadline-test"))

	cli, err := wsTunnelClient(t, ctx, srv, tunnelID, agentID, "127.0.0.1:8000")
	if err != nil {
		t.Fatalf("WS 握手: %v", err)
	}

	// 只读不回应(模拟死 CLI):收到 Ping 不回 Pong。
	for {
		_, _, rerr := cli.Read(ctx)
		if rerr != nil {
			// 判死后 Relay 关闭连接 → 读错误 = 通过
			return
		}
	}
}

// TestTunnelWSAgentReject Agent 拒绝(accepted=false)→ CLI 收到 RST 控制帧。
func TestTunnelWSAgentReject(t *testing.T) {
	srv, relay := startTunnelWSTestRelay(t, time.Hour, time.Hour)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const agentID = "ws-agent-4"
	const tunnelID = "tun-ws-004"
	agentConn := dialTunnelTestAgent(t, ctx, srv, relay, agentID)

	// mock:回 accepted=false。
	go func() {
		for {
			_, data, err := agentConn.Read(ctx)
			if err != nil {
				return
			}
			var env proto.Envelope
			if json.Unmarshal(data, &env) == nil && env.Type == proto.TypeTunnelOpen {
				readyEnv, _ := proto.NewEnvelope(proto.TypeTunnelReady, "tunnel-agent", "relay", proto.TunnelReady{
					TunnelID: tunnelID, Accepted: false, Reason: "dial 失败: connection refused",
				})
				_ = agentConn.Write(ctx, websocket.MessageText, mustJSON(readyEnv))
				return
			}
		}
	}()

	cli, err := wsTunnelClient(t, ctx, srv, tunnelID, agentID, "10.0.0.1:9999")
	if err != nil {
		t.Fatalf("WS 握手: %v", err)
	}

	_, data, err := cli.Read(ctx)
	if err != nil {
		t.Fatalf("应收到 RST 帧,却连接错误: %v", err)
	}
	_, chanType, flags, payload, perr := proto.ParseTunnelFrame(data)
	if perr != nil || chanType != proto.ChanTypeTunnelCtrl || flags != proto.TunnelCtrlRST {
		t.Fatalf("期望 RST 控制帧,got chanType=%x flags=%x", chanType, flags)
	}
	if !strings.Contains(string(payload), "dial 失败") {
		t.Fatalf("RST 应透传拒绝原因: %q", payload)
	}
}
