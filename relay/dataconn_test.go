package main

// Phase2 数据面分离测试:
//   1. E2E:fake Agent 主连接注册 + 数据连接(?data=0) join→ack→入池;
//      POST /api/transfer/download 后,Agent 在【数据连接】上发二进制帧,
//      HTTP 响应收到字节(证明数据连接帧路由到主连接注册的通道)。
//   2. 离线 agent 的 data_join 被拒(无 ack,连接关闭)。
//   3. Registry 池:bind/unbind/轮转。

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

	"github.com/QiuShichang/go-rmm/proto"
)

func TestDataConnDownloadE2E(t *testing.T) {
	relay := NewRelay(Config{Dev: true, Addr: ":0"})
	mux := http.NewServeMux()
	relay.registerHTTP(mux)
	mux.HandleFunc("/agent", relay.handleAgentWS)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	wsBase := strings.Replace(srv.URL+"/agent", "http://", "ws://", 1)

	// 主连接:注册 + 应答 init
	main, _, err := websocket.Dial(ctx, wsBase, nil) //nolint:bodyclose
	if err != nil {
		t.Fatal(err)
	}
	defer main.CloseNow()
	main.SetReadLimit(1 << 20)
	regEnv, _ := proto.NewEnvelope(proto.TypeRegister, "dc-agent", "relay", proto.Register{
		AgentID: "dc-agent", Hostname: "h", OS: "linux/amd64", Capabilities: []string{"file", "xfer_offset"},
	})
	if err := main.Write(ctx, websocket.MessageText, mustJSON(regEnv)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for len(relay.agents.Snapshot()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("agent 未上线")
		}
		time.Sleep(10 * time.Millisecond)
	}
	go func() { // 主连接读循环:应答 transfer init
		for {
			mt, data, rerr := main.Read(ctx)
			if rerr != nil {
				return
			}
			if mt != websocket.MessageText {
				continue
			}
			var env proto.Envelope
			if json.Unmarshal(data, &env) != nil || env.Type != proto.TypeFileTransferInit {
				continue
			}
			readyEnv, _ := proto.NewEnvelope(proto.TypeFileTransferReady, "dc-agent", "relay",
				proto.FileTransferReady{TransferID: env.ReplyTo, Accepted: true})
			var init proto.FileTransferInit
			_ = env.Decode(&init)
			readyEnv2, _ := proto.NewEnvelope(proto.TypeFileTransferReady, "dc-agent", "relay",
				proto.FileTransferReady{TransferID: init.TransferID, Accepted: true})
			_ = main.Write(ctx, websocket.MessageText, mustJSON(readyEnv2))
			_ = readyEnv
		}
	}()

	// 数据连接:join → 等 ack
	dc, _, err := websocket.Dial(ctx, wsBase+"?data=0", nil) //nolint:bodyclose
	if err != nil {
		t.Fatal(err)
	}
	defer dc.CloseNow()
	dc.SetReadLimit(1 << 20)
	joinEnv, _ := proto.NewEnvelope(proto.TypeDataConnJoin, "dc-agent", "relay",
		proto.DataConnJoin{AgentID: "dc-agent", Index: 0})
	if err := dc.Write(ctx, websocket.MessageText, mustJSON(joinEnv)); err != nil {
		t.Fatal(err)
	}
	ackCtx, ackCancel := context.WithTimeout(ctx, 3*time.Second)
	defer ackCancel()
	acked := false
	for !acked {
		mt, data, rerr := dc.Read(ackCtx)
		if rerr != nil {
			t.Fatal("未收到 data_join_ack(3s 超时)")
		}
		if mt != websocket.MessageText {
			t.Fatal("ack 阶段不应收到二进制帧")
		}
		var env proto.Envelope
		if json.Unmarshal(data, &env) == nil && env.Type == proto.TypeDataConnAck {
			acked = true
		}
	}
	if n := relay.agents.DataConnCount("dc-agent"); n != 1 {
		t.Fatalf("数据连接池应=1,实际 %d", n)
	}

	// 发起下载;随后在【数据连接】上发送该 transferID 的二进制帧
	content := []byte("phase2-data-plane-payload")
	frame := proto.BuildBinaryFrame("dc-dl-1", false, content)
	lastFrame := proto.BuildBinaryFrame("dc-dl-1", true, nil)
	respCh := make(chan []byte, 1)
	go func() {
		resp, err := http.Post(srv.URL+"/api/transfer/download?agent_id=dc-agent&path=ws%2Fp.bin&transfer_id=dc-dl-1", "application/json", nil)
		if err != nil {
			respCh <- nil
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		respCh <- b
	}()
	// 等 relay 下发 init 并注册通道(主连接 fake 已应答 ready),再发帧
	time.Sleep(500 * time.Millisecond)
	if err := dc.Write(ctx, websocket.MessageBinary, frame); err != nil {
		t.Fatal(err)
	}
	if err := dc.Write(ctx, websocket.MessageBinary, lastFrame); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-respCh:
		if string(got) != string(content) {
			t.Errorf("HTTP 响应内容不符: %q", string(got))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("下载响应超时(数据连接帧未路由到主连接通道)")
	}
}

func TestDataConnJoinRejectedOffline(t *testing.T) {
	relay := NewRelay(Config{Dev: true, Addr: ":0"})
	mux := http.NewServeMux()
	relay.registerHTTP(mux)
	mux.HandleFunc("/agent", relay.handleAgentWS)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dc, _, err := websocket.Dial(ctx, strings.Replace(srv.URL+"/agent?data=0", "http://", "ws://", 1), nil) //nolint:bodyclose
	if err != nil {
		t.Fatal(err)
	}
	defer dc.CloseNow()
	joinEnv, _ := proto.NewEnvelope(proto.TypeDataConnJoin, "nobody", "relay",
		proto.DataConnJoin{AgentID: "nobody", Index: 0})
	if err := dc.Write(ctx, websocket.MessageText, mustJSON(joinEnv)); err != nil {
		t.Fatal(err)
	}
	// 离线 agent:relay 应关闭连接(读到 EOF/close),且不回 ack
	readCtx, readCancel := context.WithTimeout(ctx, 3*time.Second)
	defer readCancel()
	_, data, rerr := dc.Read(readCtx)
	if rerr == nil {
		var env proto.Envelope
		if json.Unmarshal(data, &env) == nil && env.Type == proto.TypeDataConnAck {
			t.Fatal("离线 agent 的 data_join 不应收到 ack")
		}
	}
}

func TestRegistryDataConnPool(t *testing.T) {
	r := NewRegistry()
	// 未注册主连接 → bind 拒绝
	if r.BindDataConn("a", 0, &wsConn{}) {
		t.Error("主连接不在线时 bind 应拒绝")
	}
	// 注册 + 绑两条 → 轮转交替
	r.Register(proto.Register{AgentID: "a", Capabilities: []string{"file"}}, &wsConn{})
	c1, c2 := &wsConn{id: "d1"}, &wsConn{id: "d2"}
	if !r.BindDataConn("a", 0, c1) || !r.BindDataConn("a", 1, c2) {
		t.Fatal("bind 应成功")
	}
	if r.DataConnCount("a") != 2 {
		t.Fatalf("池应=2,实际 %d", r.DataConnCount("a"))
	}
	// 稳定绑定:不同 key 分散到两条连接,同 key 恒定同一条
	if r.DataConnFor("a", "k1") == r.DataConnFor("a", "k1") {
	} else {
		t.Error("同 key 应稳定")
	}
	distinct := 0
	seen := map[*wsConn]bool{}
	for _, k := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		c := r.DataConnFor("a", k)
		if !seen[c] {
			seen[c] = true
			distinct++
		}
	}
	if distinct < 2 {
		t.Errorf("多 key 应分散到多条连接,实际 %d", distinct)
	}
	// 解绑一条 → 池=1;unregister 主连接 → 池清空
	r.UnbindDataConn("a", c1)
	if r.DataConnCount("a") != 1 {
		t.Fatalf("解绑后池应=1,实际 %d", r.DataConnCount("a"))
	}
	r.Unregister("a", r.Conn("a"))
	if r.DataConnCount("a") != 0 {
		t.Error("主连接注销应清空数据池")
	}
}
