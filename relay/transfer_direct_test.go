package main

// Phase1 直连票据端点 E2E(真 WS fake Agent,复用 transfer_upload_sha_test 模式):
//   1. Agent 带 XferAddrs 注册 → POST /api/transfer/direct → 200 direct=true,
//      fake Agent 收到 TypeXferTicket 推送,SecretHash == SHA256(响应里的明文 secret)
//   2. Agent 不带 XferAddrs → 200 direct=false(CLI 回落中继的信号)
//   3. Agent 不在线 → 404
//   4. direction 非法 → 400

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/QiuShichang/reins/proto"
)

// directGrantSink 线程安全收集 fake Agent 收到的票据推送(race 检测合规:
// 原指针直写+测试裸读在 -race 下 6 处竞争报警)。
type directGrantSink struct {
	mu    sync.Mutex
	grant proto.XferTicketGrant
	typ   string
}

func (s *directGrantSink) set(g proto.XferTicketGrant, typ string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.grant = g
	s.typ = typ
}

func (s *directGrantSink) get() (proto.XferTicketGrant, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.grant, s.typ
}

// startDirectFakeAgent 起 relay + fake Agent WS,票据推送写入 sink,返回 server URL。
func startDirectFakeAgent(t *testing.T, xferAddrs []string, sink *directGrantSink) string {
	t.Helper()
	relay := NewRelay(Config{Dev: true, Addr: ":0"})
	mux := http.NewServeMux()
	relay.registerHTTP(mux)
	mux.HandleFunc("/agent", relay.handleAgentWS)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	conn, _, err := websocket.Dial(ctx, strings.Replace(srv.URL+"/agent", "http://", "ws://", 1), nil) //nolint:bodyclose // 成功时 resp.Body 为 nil
	if err != nil {
		t.Fatalf("agent 连接失败: %v", err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	conn.SetReadLimit(1 << 20)

	reg := proto.Register{
		AgentID: "direct-agent", Hostname: "h", OS: "linux/amd64",
		Capabilities: []string{"file"}, XferAddrs: xferAddrs,
	}
	regEnv, _ := proto.NewEnvelope(proto.TypeRegister, "direct-agent", "relay", reg)
	if err := conn.Write(ctx, websocket.MessageText, mustJSON(regEnv)); err != nil {
		t.Fatal(err)
	}

	// 等 agent 上线
	deadline := time.Now().Add(3 * time.Second)
	for len(relay.agents.Snapshot()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("agent 未上线")
		}
		time.Sleep(10 * time.Millisecond)
	}

	go func() {
		for {
			mt, data, rerr := conn.Read(ctx)
			if rerr != nil {
				return
			}
			if mt != websocket.MessageText {
				continue
			}
			var env proto.Envelope
			if json.Unmarshal(data, &env) != nil || env.Type != proto.TypeXferTicket {
				continue
			}
			var g proto.XferTicketGrant
			if env.Decode(&g) != nil {
				continue
			}
			sink.set(g, env.Type)
		}
	}()
	return srv.URL
}

func TestTransferDirectTicketE2E(t *testing.T) {
	sink := &directGrantSink{}
	srvURL := startDirectFakeAgent(t, []string{"192.168.1.5:7783", "10.0.0.7:7783"}, sink)

	resp, err := http.Post(srvURL+"/api/transfer/direct", "application/json",
		strings.NewReader(`{"agent_id":"direct-agent","path":"C:\\reins\\ws\\big.tar","direction":"upload"}`))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("期望 200,实际 %d: %s", resp.StatusCode, body)
	}
	var result struct {
		Direct     bool     `json:"direct"`
		TicketID   string   `json:"ticket_id"`
		Secret     string   `json:"secret"`
		Addrs      []string `json:"addrs"`
		ExpiryUnix int64    `json:"expiry_unix"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("解析响应失败: %v (%s)", err, body)
	}
	if !result.Direct {
		t.Fatalf("应 direct=true: %s", body)
	}
	if len(result.Secret) != 64 {
		t.Errorf("secret 应为 64 hex 字符,实际 %d", len(result.Secret))
	}
	if len(result.Addrs) != 2 || result.Addrs[0] != "192.168.1.5:7783" {
		t.Errorf("addrs 未透传: %v", result.Addrs)
	}
	if result.ExpiryUnix <= time.Now().Unix() {
		t.Errorf("expiry 应在未来: %d", result.ExpiryUnix)
	}

	// fake Agent 必须已收到票据推送,且 hash 与响应明文 secret 对拍
	deadline := time.Now().Add(3 * time.Second)
	gotGrant, gotType := sink.get()
	for gotGrant.TicketID == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		gotGrant, gotType = sink.get()
	}
	if gotGrant.TicketID == "" {
		t.Fatal("Agent 未收到票据推送")
	}
	if gotType != proto.TypeXferTicket {
		t.Errorf("推送帧类型 = %q,want %q", gotType, proto.TypeXferTicket)
	}
	if gotGrant.TicketID != result.TicketID {
		t.Errorf("ticket_id 不一致: agent=%s cli=%s", gotGrant.TicketID, result.TicketID)
	}
	// hash 契约闭环:Agent 收到的 SecretHash == SHA256(CLI 拿到的 secret 交付形态)
	sum := sha256.Sum256([]byte(result.Secret))
	if gotGrant.SecretHash != hex.EncodeToString(sum[:]) {
		t.Errorf("SecretHash 与明文 secret 不对拍: agent=%s sha256(secret)=%s",
			gotGrant.SecretHash, hex.EncodeToString(sum[:]))
	}
	if gotGrant.Path != `C:\reins\ws\big.tar` || gotGrant.Direction != "upload" {
		t.Errorf("票据绑定字段不符: %+v", gotGrant)
	}
	if gotGrant.ExpiryUnix <= time.Now().Unix() {
		t.Errorf("票据过期时间应在未来: %d", gotGrant.ExpiryUnix)
	}
}

func TestTransferDirectNoAddrsFallback(t *testing.T) {
	sink := &directGrantSink{}
	srvURL := startDirectFakeAgent(t, nil, sink)

	resp, err := http.Post(srvURL+"/api/transfer/direct", "application/json",
		strings.NewReader(`{"agent_id":"direct-agent","path":"foo.bin","direction":"download"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("期望 200,实际 %d: %s", resp.StatusCode, body)
	}
	var result struct {
		Direct bool   `json:"direct"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatal(err)
	}
	if result.Direct {
		t.Errorf("无 XferAddrs 应 direct=false: %s", body)
	}
	if result.Reason == "" {
		t.Errorf("应带回落原因: %s", body)
	}
	if g, typ := sink.get(); g.TicketID != "" || typ != "" {
		t.Errorf("未开启直连时不应推送票据")
	}
}

func TestTransferDirectOfflineAgent(t *testing.T) {
	relay := NewRelay(Config{Dev: true, Addr: ":0"})
	mux := http.NewServeMux()
	relay.registerHTTP(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/transfer/direct", "application/json",
		strings.NewReader(`{"agent_id":"nobody","path":"foo","direction":"upload"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("离线 agent 应 404,实际 %d", resp.StatusCode)
	}
}

func TestTransferDirectBadDirection(t *testing.T) {
	relay := NewRelay(Config{Dev: true, Addr: ":0"})
	mux := http.NewServeMux()
	relay.registerHTTP(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/transfer/direct", "application/json",
		strings.NewReader(`{"agent_id":"x","path":"foo","direction":"sideways"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("非法 direction 应 400,实际 %d", resp.StatusCode)
	}
}
