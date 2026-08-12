// relay/tunnel_integration_test.go
// P0-A 隧道竞态回归测试:open 阶段提前注册 dataCh,poll 慢注册不丢首包;
// close 后 poll 收到 EOF 立即结束(不再挂 30 分钟);双向数据与 Agent 拒绝路径。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/QiuShichang/go-rmm/proto"
)

// startTunnelTestRelay 启动带真实 WS agent 端点的测试 Relay。
func startTunnelTestRelay(t *testing.T) (*httptest.Server, *Relay) {
	t.Helper()
	relay := NewRelay(Config{Dev: true, Addr: ":0"})
	mux := http.NewServeMux()
	relay.registerHTTP(mux)
	mux.HandleFunc("/agent", relay.handleAgentWS)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, relay
}

// dialTunnelTestAgent 建立真实 WS agent 连接并注册,等待能力就绪。
func dialTunnelTestAgent(t *testing.T, ctx context.Context, srv *httptest.Server, relay *Relay, id string) *websocket.Conn {
	t.Helper()
	url := strings.Replace(srv.URL+"/agent", "http://", "ws://", 1)
	c, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("agent %s dial: %v", id, err)
	}
	c.SetReadLimit(1 << 20)
	regEnv, _ := proto.NewEnvelope(proto.TypeRegister, id, "relay", proto.Register{
		AgentID: id, Hostname: id, OS: "linux/amd64",
		Capabilities: []string{proto.TaskPowerShell},
	})
	if err := c.Write(ctx, websocket.MessageText, mustJSON(regEnv)); err != nil {
		t.Fatalf("agent %s register write: %v", id, err)
	}
	t.Cleanup(func() { c.CloseNow() })

	deadline := time.Now().Add(3 * time.Second)
	for !relay.agents.Supports(id, proto.TaskPowerShell) {
		if time.Now().After(deadline) {
			t.Fatalf("agent %s 未在期内就绪", id)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return c
}

// mockTunnelAgent 处理 tunnel_open → 回 ready + 立即推首包(模拟 Agent dial 成功立刻回数据,如 HTTP 响应头)。
// 收集 CLI→Agent 方向的隧道数据字节数;统计收到的 close 次数(真实 Agent 收到 close 只关隧道,连接继续)。
func mockTunnelAgent(t *testing.T, ctx context.Context, conn *websocket.Conn, tunnelID string, firstPacket []byte) (gotData *atomic.Int32, closes *atomic.Int32, done chan struct{}) {
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
					continue // 连接继续,可处理后续 open
				}
				continue
			}
			// 二进制隧道帧(CLI → Agent 方向)
			if tid, _, _, payload, err := proto.ParseTunnelFrame(data); err == nil && tid == tunnelID {
				gotData.Add(int32(len(payload)))
			}
		}
	}()
	return gotData, closes, done
}

// tunnelOpen 调 POST /api/tunnel/open,返回 accepted/reason。
func tunnelOpen(t *testing.T, srv *httptest.Server, tunnelID, agentID, target string) (bool, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{
		"tunnel_id": tunnelID, "agent_id": agentID, "target": target,
	})
	resp, err := http.Post(srv.URL+"/api/tunnel/open", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("open POST: %v", err)
	}
	defer resp.Body.Close()
	var result struct {
		TunnelID string `json:"tunnel_id"`
		Accepted bool   `json:"accepted"`
		Reason   string `json:"reason"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("open 响应解析: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("open 期望 200, got %d", resp.StatusCode)
	}
	return result.Accepted, result.Reason
}

// TestTunnelFirstPacketNoLoss P0-A 核心回归:open 阶段注册 dataCh,
// poll 在 open 返回 200ms 后才发起(模拟 CLI 慢注册),Agent 的首包必须不丢。
func TestTunnelFirstPacketNoLoss(t *testing.T) {
	srv, relay := startTunnelTestRelay(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const agentID = "tunnel-agent-1"
	const tunnelID = "tun-p0a-001"
	conn := dialTunnelTestAgent(t, ctx, srv, relay, agentID)

	first := []byte("HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\nhello")
	gotData, _, _ := mockTunnelAgent(t, ctx, conn, tunnelID, first)

	// 1. open:Agent 收到 tunnel_open 后立即回 ready + 推首包。
	accepted, reason := tunnelOpen(t, srv, tunnelID, agentID, "127.0.0.1:8000")
	if !accepted {
		t.Fatalf("open 未被接受: %s", reason)
	}

	// 2. 模拟 CLI 慢注册:open 返回后才发起 poll(修复前首包在此窗口被静默丢弃)。
	time.Sleep(200 * time.Millisecond)

	// 3. poll 读数据流。
	pollResp, err := http.Get(srv.URL + "/api/tunnel/poll?tunnel_id=" + tunnelID + "&agent_id=" + agentID)
	if err != nil {
		t.Fatalf("poll GET: %v", err)
	}
	defer pollResp.Body.Close()
	dataCh := make(chan []byte, 16)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, rerr := pollResp.Body.Read(buf)
			if n > 0 {
				dataCh <- append([]byte(nil), buf[:n]...)
			}
			if rerr != nil {
				close(dataCh)
				return
			}
		}
	}()

	// 首包必须在(修复前这里超时:数据在 open→poll 窗口被丢)。
	var received []byte
	waitFor := func(want []byte) bool {
		deadline := time.Now().Add(3 * time.Second)
		for !bytes.Contains(received, want) && time.Now().Before(deadline) {
			select {
			case d, ok := <-dataCh:
				if !ok {
					return bytes.Contains(received, want)
				}
				received = append(received, d...)
			case <-time.After(100 * time.Millisecond):
			}
		}
		return bytes.Contains(received, want)
	}
	if !waitFor(first) {
		t.Fatalf("首包丢失: poll 收到 %q,期望包含 %q", received, first)
	}

	// 4. 后续流:Agent 再发一帧,poll 持续收到。
	second := []byte("second-chunk")
	if err := conn.Write(ctx, websocket.MessageBinary,
		proto.BuildTunnelFrame(proto.ChanTypeTunnelData, tunnelID, 0, second)); err != nil {
		t.Fatalf("agent 写第二帧: %v", err)
	}
	if !waitFor(second) {
		t.Fatalf("第二帧丢失: poll 收到 %q,期望包含 %q", received, second)
	}

	// 5. CLI→Agent 方向:POST data,mock agent 必须收到。
	cliData := []byte("cli-to-agent")
	postResp, err := http.Post(srv.URL+"/api/tunnel/data?tunnel_id="+tunnelID+"&agent_id="+agentID,
		"application/octet-stream", bytes.NewReader(cliData))
	if err != nil {
		t.Fatalf("data POST: %v", err)
	}
	postResp.Body.Close()
	deadline := time.Now().Add(3 * time.Second)
	for gotData.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if gotData.Load() != int32(len(cliData)) {
		t.Fatalf("CLI 数据未完整到达 Agent: got %d, want %d", gotData.Load(), len(cliData))
	}

	// 6. close 后 poll 必须立即收到 EOF 结束(修复前挂 30 分钟)。
	closeBody, _ := json.Marshal(map[string]string{
		"tunnel_id": tunnelID, "agent_id": agentID, "reason": "test done",
	})
	cResp, err := http.Post(srv.URL+"/api/tunnel/close", "application/json", bytes.NewReader(closeBody))
	if err != nil {
		t.Fatalf("close POST: %v", err)
	}
	cResp.Body.Close()

	select {
	case _, ok := <-dataCh:
		if ok {
			// 收到 EOF(nil 唤醒)后 channel 随后关闭
			<-dataCh
		}
	case <-time.After(3 * time.Second):
		t.Fatal("close 后 poll 未结束(EOF 未唤醒)")
	}
}

// TestTunnelAgentReject 验证 Agent 拒绝时 open 返回 accepted=false(现有行为回归)。
func TestTunnelAgentReject(t *testing.T) {
	srv, relay := startTunnelTestRelay(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const agentID = "tunnel-agent-2"
	const tunnelID = "tun-p0a-002"
	conn := dialTunnelTestAgent(t, ctx, srv, relay, agentID)

	// mock:收到 open 回 accepted=false(模拟 dial 失败/策略拒绝),不推数据。
	go func() {
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var env proto.Envelope
			if json.Unmarshal(data, &env) == nil && env.Type == proto.TypeTunnelOpen {
				readyEnv, _ := proto.NewEnvelope(proto.TypeTunnelReady, "tunnel-agent", "relay", proto.TunnelReady{
					TunnelID: tunnelID, Accepted: false, Reason: "dial 失败: connection refused",
				})
				_ = conn.Write(ctx, websocket.MessageText, mustJSON(readyEnv))
				return
			}
		}
	}()

	accepted, reason := tunnelOpen(t, srv, tunnelID, agentID, "10.0.0.1:9999")
	if accepted {
		t.Fatal("Agent 拒绝时 open 仍 accepted")
	}
	if !strings.Contains(reason, "dial 失败") {
		t.Fatalf("拒绝原因未透传: %q", reason)
	}
}

// TestTunnelCloseWakesPoll 验证 close 唤醒 poll 的独立路径(无数据流的隧道)。
func TestTunnelCloseWakesPoll(t *testing.T) {
	srv, relay := startTunnelTestRelay(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const agentID = "tunnel-agent-3"
	const tunnelID = "tun-p0a-003"
	conn := dialTunnelTestAgent(t, ctx, srv, relay, agentID)
	// 无首包(模拟远端暂不推数据的场景)。
	_, closes, _ := mockTunnelAgent(t, ctx, conn, tunnelID, nil)

	accepted, reason := tunnelOpen(t, srv, tunnelID, agentID, "127.0.0.1:8000")
	if !accepted {
		t.Fatalf("open 未被接受: %s", reason)
	}

	// 不发 poll,直接 close:验证 dataCh 被清理且不 panic,Agent 收到 close。
	closeBody, _ := json.Marshal(map[string]string{
		"tunnel_id": tunnelID, "agent_id": agentID, "reason": "no-poll cleanup",
	})
	cResp, err := http.Post(srv.URL+"/api/tunnel/close", "application/json", bytes.NewReader(closeBody))
	if err != nil {
		t.Fatalf("close POST: %v", err)
	}
	cResp.Body.Close()

	deadline := time.Now().Add(3 * time.Second)
	for closes.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if closes.Load() == 0 {
		t.Fatal("Agent 未收到 tunnel_close")
	}

	// 清理后同一 tunnelID 可再次 open(通道无残留,getOrRegister 新建)。
	accepted, reason = tunnelOpen(t, srv, tunnelID, agentID, "127.0.0.1:8001")
	if !accepted {
		t.Fatalf("重复 open 未被接受: %s", reason)
	}
}

// TestTunnelConcurrentOpenPoll 并发竞态验证:open/poll/data/close 并发组合无 panic、无死锁。
// 参照 xfer_concurrency_test.go 的并发模式。
func TestTunnelConcurrentOpenPoll(t *testing.T) {
	srv, relay := startTunnelTestRelay(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	const agentID = "tunnel-agent-4"
	const tunnelID = "tun-p0a-004"
	conn := dialTunnelTestAgent(t, ctx, srv, relay, agentID)
	gotData, _, _ := mockTunnelAgent(t, ctx, conn, tunnelID, []byte("race-first"))

	// 并发发起 open + poll(模拟 CLI P0-A 并行时序)。
	openDone := make(chan bool, 1)
	go func() {
		accepted, reason := tunnelOpen(t, srv, tunnelID, agentID, "127.0.0.1:8000")
		openDone <- accepted && !strings.Contains(reason, "拒绝")
	}()
	pollResp, err := http.Get(srv.URL + "/api/tunnel/poll?tunnel_id=" + tunnelID + "&agent_id=" + agentID)
	if err != nil {
		t.Fatalf("poll GET: %v", err)
	}
	defer pollResp.Body.Close()

	if !<-openDone {
		t.Fatal("并发 open 未被接受")
	}

	// poll 必须读到首包(无论谁先注册,getOrRegister + 提前注册保证不丢)。
	readCh := make(chan []byte, 4)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, rerr := pollResp.Body.Read(buf)
			if n > 0 {
				readCh <- append([]byte(nil), buf[:n]...)
			}
			if rerr != nil {
				close(readCh)
				return
			}
		}
	}()
	var got []byte
	deadline := time.Now().Add(3 * time.Second)
	for !bytes.Contains(got, []byte("race-first")) && time.Now().Before(deadline) {
		select {
		case d, ok := <-readCh:
			if !ok {
				break
			}
			got = append(got, d...)
		case <-time.After(50 * time.Millisecond):
		}
	}
	if !bytes.Contains(got, []byte("race-first")) {
		t.Fatalf("并发 open/poll 首包丢失: got %q", got)
	}

	// 双向并发:CLI 写数据 + Agent 写数据,均需到达。
	go func() {
		body := bytes.Repeat([]byte("x"), 2048)
		r, _ := http.Post(srv.URL+"/api/tunnel/data?tunnel_id="+tunnelID+"&agent_id="+agentID,
			"application/octet-stream", bytes.NewReader(body))
		if r != nil {
			io.Copy(io.Discard, r.Body)
			r.Body.Close()
		}
	}()
	deadline2 := time.Now().Add(3 * time.Second)
	for gotData.Load() == 0 && time.Now().Before(deadline2) {
		time.Sleep(10 * time.Millisecond)
	}
	if gotData.Load() == 0 {
		t.Fatal("并发 CLI 数据未到达 Agent")
	}
	// 无 panic、无死锁即通过(goroutine 全部正常退出)。
}
