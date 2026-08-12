// Package main — 隧道 WebSocket 全双工端点(P1-A)。
//
// 背景:HTTP 模式(/api/tunnel/open|data|poll)每条连接占两个 HTTP 长连接,
// 且 open→poll 之间存在首包丢失竞态(已在 P0-A 修复,但连接开销仍在)。
// P1-A 引入单一 WS 连接承载 open 信令 + 双向数据 + 心跳:
//
//	CLI ──WS──► Relay ──(现有 Agent WS 通道)──► Agent ──net.Dial──► target
//	   JSON 信令(Envelope TunnelOpen/Close)+ 隧道二进制帧(proto/frame.go)
//
// 帧协议:
//   - CLI→Relay:文本帧 = Envelope 信令(目前仅 tunnel_close);二进制帧 = 隧道数据
//   - Relay→CLI:二进制帧 = 隧道数据 + 控制帧(Ping 0x02 / RST 0x01)
//
// 心跳:Relay 每 15s 发 Ping,30s 无任何收包(含 Pong)判死连接。
// 兼容:旧 HTTP 端点保留,老 CLI 不受影响。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"github.com/QiuShichang/go-rmm/proto"
)

// handleTunnelWS GET /api/tunnel/ws?tunnel_id=&agent_id=&target=&network=
// 隧道 WebSocket 全双工端点。握手成功即执行 open(等 Agent ready),
// 之后单一连接内双向转发数据 + 心跳,无首包竞态窗口。
func (r *Relay) handleTunnelWS(w http.ResponseWriter, req *http.Request) {
	q := req.URL.Query()
	tunnelID := q.Get("tunnel_id")
	agentID := q.Get("agent_id")
	target := q.Get("target")
	if tunnelID == "" || agentID == "" || target == "" {
		writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("tunnel_id/agent_id/target 不能为空")))
		return
	}
	conn := r.agents.Conn(agentID)
	if conn == nil {
		writeJSON(w, http.StatusNotFound, errMap(fmt.Errorf("agent %s 不在线", agentID)))
		return
	}

	c, err := websocket.Accept(w, req, &websocket.AcceptOptions{InsecureSkipVerify: r.cfg.Dev})
	if err != nil {
		return
	}
	c.SetReadLimit(1 << 20)
	defer c.CloseNow()

	// Review:不用 30 分钟 ctx 超时(长连接隧道如 SSH/DB 会保持 30+ 分钟被强制切断),
	// 改 WithCancel:客户端断开即取消,存活由心跳判死(30s 无收包)保证。
	ctx, cancel := context.WithCancel(req.Context())
	defer cancel()

	// 1. open:与 HTTP 模式同流程(readyCh + 提前注册 dataCh,P0-A 修复),无竞态窗口。
	readyCh := conn.registerReadyChannel(tunnelID)
	dataCh := conn.registerXferChannel(tunnelID)
	defer conn.unregisterReadyChannel(tunnelID)

	network := q.Get("network")
	if network == "" {
		network = "tcp"
	}
	open := proto.TunnelOpen{TunnelID: tunnelID, Network: network, Target: target}
	env, _ := proto.NewEnvelope(proto.TypeTunnelOpen, "relay", agentID, open)
	if err := conn.writeJSON(ctx, env); err != nil {
		return
	}
	select {
	case ready := <-readyCh:
		if ready == nil || !ready.Accepted {
			// Review:清理提前注册的 dataCh(CLI 收到 RST 后不会发 close 信令,原实现泄漏到 Agent 断线)
			conn.unregisterXferChannels(tunnelID)
			// Agent 拒绝(dial 失败/策略):回 RST 控制帧透传原因
			reason := "tunnel rejected"
			if ready != nil {
				reason = ready.Reason
			}
			rst := proto.BuildTunnelFrame(proto.ChanTypeTunnelCtrl, tunnelID, proto.TunnelCtrlRST, []byte(reason))
			_ = c.Write(ctx, websocket.MessageBinary, rst)
			return
		}
	case <-time.After(10 * time.Second):
		conn.unregisterXferChannels(tunnelID) // Review:ready 超时清理 dataCh
		return
	case <-ctx.Done():
		conn.unregisterXferChannels(tunnelID)
		return
	}

	// 2. Agent→CLI:dataCh → WS(数据帧流)。
	// 注意:routeXferData 传的是帧内 payload(裸数据),必须重新封装隧道帧,
	// 与 CLI→Relay 方向的帧格式保持一致。
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case data, ok := <-dataCh:
				if !ok || data == nil {
					return // EOF / unregister 唤醒
				}
				frame := proto.BuildTunnelFrame(proto.ChanTypeTunnelData, tunnelID, 0, data)
				wctx, wcancel := context.WithTimeout(ctx, 5*time.Second)
				_ = c.Write(wctx, websocket.MessageBinary, frame)
				wcancel()
			}
		}
	}()

	// 3. CLI→Agent + 心跳主循环。
	// 注意:读必须放 goroutine 用 channel 传递,否则 select 的 default+阻塞读
	// 会让 Ping 定时器永远无法触发(程序阻塞在 c.Read 内)。
	// 间隔/阈值可从 Relay 字段覆盖(测试用),默认 15s/30s。
	pingInterval := r.tunnelPingInterval
	if pingInterval <= 0 {
		pingInterval = 15 * time.Second
	}
	deadline := r.tunnelDeadline
	if deadline <= 0 {
		deadline = 30 * time.Second
	}
	type wsFrame struct {
		msgType websocket.MessageType
		data    []byte
	}
	readCh := make(chan wsFrame, 8)
	readErr := make(chan error, 1)
	go func() {
		for {
			mt, d, rerr := c.Read(ctx)
			if rerr != nil {
				select {
				case readErr <- rerr:
				default:
				}
				return
			}
			select {
			case readCh <- wsFrame{mt, d}:
			case <-ctx.Done():
				return
			}
		}
	}()

	lastRecv := time.Now()
	ping := time.NewTicker(pingInterval)
	defer ping.Stop()
	closeTunnel := func() {
		conn.unregisterXferChannels(tunnelID)
	}
	for {
		select {
		case <-ping.C:
			// 超时无任何收包(含 Pong)→ 判死,清理隧道。
			if time.Since(lastRecv) > deadline {
				log.Printf("[tunnel-ws] 隧道 %s 心跳超时,关闭", tunnelID)
				closeTunnel()
				return
			}
			pframe := proto.BuildTunnelFrame(proto.ChanTypeTunnelCtrl, tunnelID, proto.TunnelCtrlPing, nil)
			wctx, wcancel := context.WithTimeout(ctx, 5*time.Second)
			_ = c.Write(wctx, websocket.MessageBinary, pframe)
			wcancel()
		case <-ctx.Done():
			closeTunnel()
			return
		case fr := <-readCh:
			lastRecv = time.Now()
			if fr.msgType == websocket.MessageText {
				// JSON 信令:close。
				var env proto.Envelope
				if json.Unmarshal(fr.data, &env) == nil && env.Type == proto.TypeTunnelClose {
					var tc proto.TunnelClose
					_ = env.Decode(&tc)
					tcEnv, _ := proto.NewEnvelope(proto.TypeTunnelClose, "relay", agentID, tc)
					_ = conn.writeJSON(ctx, tcEnv) // 通知 Agent 关闭远端连接
					closeTunnel()
					return
				}
				continue
			}
			// 二进制帧:数据或控制(Pong 保活)。
			if !proto.IsTunnelFrame(fr.data) {
				continue
			}
			_, chanType, _, payload, perr := proto.ParseTunnelFrame(fr.data)
			if perr != nil {
				continue
			}
			if chanType == proto.ChanTypeTunnelCtrl {
				continue // Pong/其他控制:仅保活(更新 lastRecv)
			}
			// 数据帧 → Agent(与 handleTunnelData 相同:经 Agent WS 二进制帧转发)。
			frame := proto.BuildTunnelFrame(proto.ChanTypeTunnelData, tunnelID, 0, payload)
			conn.writeMu.Lock()
			werr := conn.conn.Write(ctx, websocket.MessageBinary, frame)
			conn.writeMu.Unlock()
			if werr != nil {
				log.Printf("[tunnel-ws] 隧道 %s 转发 Agent 失败: %v", tunnelID, werr)
				closeTunnel()
				return
			}
		case err := <-readErr:
			log.Printf("[tunnel-ws] 隧道 %s 读错误: %v", tunnelID, err)
			closeTunnel()
			return
		}
	}
}
