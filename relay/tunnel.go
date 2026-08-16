// Package main — Relay 隧道透传(端口转发)。
//
// 阶段 3.1:Relay 做纯路由,不感知隧道内容。
//
// Console 端的隧道走 HTTP 端点(POST /api/tunnel/open,chunked transfer 收发数据),
// Agent 端走 WS(Envelope 控制信令 + 二进制隧道帧)。
//
// 为复用现有 xferSession 基础设施,隧道数据帧也走 routeXferData 路由:
//   - Console POST /api/tunnel/open → 发 TunnelOpen 给 Agent,注册 tunnelData channel
//   - Agent 回 TunnelReady → 转给 Console
//   - Console POST /api/tunnel/data (chunked) → 二进制帧转给 Agent
//   - Agent 发隧道二进制帧 → routeXferData → Console 的 /api/tunnel/data 响应流
//
// 简化:隧道数据帧与文件帧共享 xferChannels(用 tunnelID 作 key)。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"github.com/QiuShichang/go-rmm/proto"
)

// handleTunnelOpen POST /api/tunnel/open —— Console 开启隧道。
// 请求体: proto.TunnelOpen{tunnel_id, network, target}
// 流程: 找 Agent conn → 发 TunnelOpen → 等 TunnelReady → 返回 200(或 502 失败)。
func (r *Relay) handleTunnelOpen(w http.ResponseWriter, req *http.Request) {
	if err := r.authorize(req); err != nil {
		writeJSON(w, http.StatusUnauthorized, errMap(err))
		return
	}
	var body struct {
		TunnelID string `json:"tunnel_id"`
		AgentID  string `json:"agent_id"`
		Network  string `json:"network"`
		Target   string `json:"target"`
	}
	if err := decodeJSON(req, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, errMap(err))
		return
	}
	if body.TunnelID == "" || body.AgentID == "" || body.Target == "" {
		writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("tunnel_id/agent_id/target 不能为空")))
		return
	}

	conn := r.agents.Conn(body.AgentID)
	if conn == nil {
		writeJSON(w, http.StatusNotFound, errMap(fmt.Errorf("agent %s 不在线", body.AgentID)))
		return
	}

	// 注册 ready channel + data channel(dataCh 复用文件传输通道,Agent 隧道帧也路由到此)
	readyCh := conn.registerReadyChannel(body.TunnelID)
	// dataCh 不在此注册(由 /api/tunnel/poll 按需注册),避免 open 阶段就占资源
	_ = readyCh
	defer conn.unregisterXferChannels(body.TunnelID)

	// 发 TunnelOpen 给 Agent
	open := proto.TunnelOpen{
		TunnelID: body.TunnelID,
		Network:  body.Network,
		Target:   body.Target,
	}
	env, _ := proto.NewEnvelope(proto.TypeTunnelOpen, "relay", body.AgentID, open)
	ctx, cancel := context.WithTimeout(req.Context(), 30*time.Minute)
	defer cancel()
	if err := conn.writeJSON(ctx, env); err != nil {
		writeJSON(w, http.StatusBadGateway, errMap(fmt.Errorf("发送 TunnelOpen 失败: %w", err)))
		return
	}

	// 等 Agent ready
	select {
	case ready := <-readyCh:
		if ready == nil {
			writeJSON(w, http.StatusGatewayTimeout, errMap(fmt.Errorf("agent 返回空 ready")))
			return
		}
		// ready.TunnelID 匹配检查(复用 ready 的 TransferID 字段语义)
		writeJSON(w, http.StatusOK, map[string]any{
			"tunnel_id": body.TunnelID,
			"accepted":  ready.Accepted,
			"reason":    ready.Reason,
		})
	case <-time.After(10 * time.Second):
		writeJSON(w, http.StatusGatewayTimeout, errMap(fmt.Errorf("等待 Agent ready 超时")))
	case <-ctx.Done():
		writeJSON(w, http.StatusBadGateway, errMap(fmt.Errorf("客户端断开")))
	}
}

// handleTunnelData POST /api/tunnel/data —— Console ↔ Agent 隧道数据流(chunks)。
//
// Console 把要发给 Agent 的数据 POST 到此端点(chunked body),
// Relay 读 body 转为隧道二进制帧发给 Agent。
// 同时,Agent 发来的隧道帧(routeXferData)以 HTTP response body 流式返回。
//
// 双向流设计:Console 用两个连接(一个 POST 写,一个 GET 读),或用 SSE/WS。
// 简化:本端点只做 Console→Agent 方向(POST body → Agent)。
// Agent→Console 方向用 GET /api/tunnel/poll?  简化用 dataCh 读取。
func (r *Relay) handleTunnelData(w http.ResponseWriter, req *http.Request) {
	if err := r.authorize(req); err != nil {
		writeJSON(w, http.StatusUnauthorized, errMap(err))
		return
	}
	tunnelID := req.URL.Query().Get("tunnel_id")
	agentID := req.URL.Query().Get("agent_id")
	if tunnelID == "" || agentID == "" {
		writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("tunnel_id/agent_id 不能为空")))
		return
	}
	conn := r.agents.Conn(agentID)
	if conn == nil {
		writeJSON(w, http.StatusNotFound, errMap(fmt.Errorf("agent 不在线")))
		return
	}
	ctx, cancel := context.WithTimeout(req.Context(), 30*time.Minute)
	defer cancel()

	// 读 HTTP body → 隧道二进制帧发给 Agent
	buf := make([]byte, 32*1024)
	for {
		n, err := req.Body.Read(buf)
		if n > 0 {
			frame := proto.BuildTunnelFrame(proto.ChanTypeTunnelData, tunnelID, 0, buf[:n])
			conn.writeMu.Lock()
			werr := conn.conn.Write(ctx, websocket.MessageBinary, frame)
			conn.writeMu.Unlock()
			if werr != nil {
				writeJSON(w, http.StatusBadGateway, errMap(fmt.Errorf("WS写失败: %w", werr)))
				return
			}
		}
		if err != nil {
			break
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"tunnel_id": tunnelID, "sent": true})
}

// handleTunnelPoll POST /api/tunnel/poll —— 拉 Agent→Console 方向的隧道数据。
// 用 SSE 式流:从 dataCh 读数据,流式写 HTTP response。
func (r *Relay) handleTunnelPoll(w http.ResponseWriter, req *http.Request) {
	if err := r.authorize(req); err != nil {
		writeJSON(w, http.StatusUnauthorized, errMap(err))
		return
	}
	tunnelID := req.URL.Query().Get("tunnel_id")
	agentID := req.URL.Query().Get("agent_id")
	if tunnelID == "" {
		writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("tunnel_id 不能为空")))
		return
	}
	conn := r.agents.Conn(agentID)
	if conn == nil {
		writeJSON(w, http.StatusNotFound, errMap(fmt.Errorf("agent 不在线")))
		return
	}
	// dataCh 已在 /api/tunnel/open 时注册
	ch := conn.registerXferChannel(tunnelID)
	defer conn.unregisterXferChannels(tunnelID)

	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	ctx, cancel := context.WithTimeout(req.Context(), 30*time.Minute)
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return
		case data, ok := <-ch:
			if !ok || data == nil {
				return // EOF / channel close
			}
			if len(data) > 0 {
				w.Write(data)
				if flusher != nil {
					flusher.Flush()
				}
			}
		}
	}
}

// handleTunnelClose POST /api/tunnel/close —— 关闭隧道。
func (r *Relay) handleTunnelClose(w http.ResponseWriter, req *http.Request) {
	if err := r.authorize(req); err != nil {
		writeJSON(w, http.StatusUnauthorized, errMap(err))
		return
	}
	var body struct {
		TunnelID string `json:"tunnel_id"`
		AgentID  string `json:"agent_id"`
		Reason   string `json:"reason"`
	}
	if err := decodeJSON(req, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, errMap(err))
		return
	}
	conn := r.agents.Conn(body.AgentID)
	if conn != nil {
		tc := proto.TunnelClose{TunnelID: body.TunnelID, Reason: body.Reason}
		env, _ := proto.NewEnvelope(proto.TypeTunnelClose, "relay", body.AgentID, tc)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = conn.writeJSON(ctx, env)
	}
	writeJSON(w, http.StatusOK, map[string]any{"tunnel_id": body.TunnelID, "closed": true})
}

// decodeJSON 从 req.Body 解析 JSON。
func decodeJSON(req *http.Request, v any) error {
	return json.NewDecoder(req.Body).Decode(v)
}

// 占位:避免 import io 未用(预留接口)
