package main

// Phase 1 内网直连传输:Relay 侧票据签发端点。
//
// 动机:Agent 反向 WS 是唯一数据通道,内网两台机器互传也要绕道云端 Relay
// (出口上行带宽 + 单 TCP 流 + TLS 两跳),实测 0.54 MB/s 量级。直连模式下
// Relay 退化为信令/授权面,字节流走 CLI↔Agent 内网直连(千兆线速)。
//
// 信任链(与既有零信任姿态一致):
//   CLI --(RBAC/token 鉴权)--> Relay --(控制 WS 推 hash)--> Agent
//   Relay 生成 32B 随机 secret:明文只回 CLI(hash 推给 Agent),
//   Agent 直连校验时 SHA256(secret) 恒时比对,一次性消费 + 60s 过期。
//   Agent 侧路径策略/per-chunk SHA256/断点续传全部照走(HandleInit 复用)。
//
// 降级语义:Agent 未开启直连(注册无 XferAddrs)→ 200 {"direct":false},
// CLI 据此静默回落既有中继路径,零配置成本。

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/QiuShichang/reins/proto"
)

// xferTicketTTL 票据有效期。只约束 begin(直连握手)时刻,
// 传输本身可以跑任意久——票据在 begin 时一次性消费。
const xferTicketTTL = 60 * time.Second

// handleTransferDirect POST /api/transfer/direct —— CLI 请求一次性直连票据。
//
// 请求: {"agent_id":"...", "path":"...", "direction":"upload|download"}
// 响应: {"direct":true, "ticket_id":"...", "secret":"<64 hex>",
//
//	"addrs":["ip:port",...], "expiry_unix":<sec>}
//	或 {"direct":false, "reason":"..."}(Agent 未开启直连,CLI 回落中继)
func (r *Relay) handleTransferDirect(w http.ResponseWriter, req *http.Request) {
	if err := r.authorize(req); err != nil {
		writeJSON(w, http.StatusUnauthorized, errMap(err))
		return
	}
	var body struct {
		AgentID   string `json:"agent_id"`
		Path      string `json:"path"`
		Direction string `json:"direction"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("请求体解析失败: %w", err)))
		return
	}
	if body.AgentID == "" || body.Path == "" {
		writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("agent_id 和 path 不能为空")))
		return
	}
	if body.Direction != "upload" && body.Direction != "download" {
		writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("direction 必须是 upload 或 download,得到 %q", body.Direction)))
		return
	}

	conn := r.agents.Conn(body.AgentID)
	if conn == nil {
		writeJSON(w, http.StatusNotFound, errMap(fmt.Errorf("agent %s 不在线", body.AgentID)))
		return
	}

	addrs := r.agents.XferAddrsOf(body.AgentID)
	if len(addrs) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"direct": false,
			"reason": "agent 未开启直连(xfer_listen 未配置),走中继路径",
		})
		return
	}

	// 生成一次性票据:32B 随机 secret(64 hex 字符串,即交付形态),hash 推给 Agent。
	// hash 契约:SHA256(secret 的交付形态字节)——CLI 呈交什么形态,Agent 就对
	// 什么形态算 hash,两侧零解码步骤,杜绝 bytes/string 两形态错位。
	secretBytes := make([]byte, 32)
	if _, err := rand.Read(secretBytes); err != nil {
		writeJSON(w, http.StatusInternalServerError, errMap(fmt.Errorf("生成票据随机数失败: %w", err)))
		return
	}
	secret := hex.EncodeToString(secretBytes)
	sum := sha256.Sum256([]byte(secret))
	grant := proto.XferTicketGrant{
		TicketID:   uuid.NewString(),
		SecretHash: hex.EncodeToString(sum[:]),
		Path:       body.Path,
		Direction:  body.Direction,
		ExpiryUnix: time.Now().Add(xferTicketTTL).Unix(),
	}

	// 经控制 WS 推给 Agent(P0 高优先,5s 超时)。推送完成后才回 CLI——
	// 保证 CLI 拿到 secret 直连时 Agent 必然已持有 hash。
	env, err := proto.NewEnvelope(proto.TypeXferTicket, "relay", body.AgentID, grant)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errMap(err))
		return
	}
	pushCtx, pushCancel := context.WithTimeout(req.Context(), 5*time.Second)
	defer pushCancel()
	if err := conn.writeControlJSON(pushCtx, env); err != nil {
		writeJSON(w, http.StatusBadGateway, errMap(fmt.Errorf("推送票据给 agent 失败: %w", err)))
		return
	}

	if r.audit != nil {
		r.audit.Log(AuditEntry{
			Event:   "xfer_ticket",
			AgentID: body.AgentID,
			Detail:  fmt.Sprintf("%s %s ticket=%s", body.Direction, body.Path, grant.TicketID[:8]),
		})
	}
	metrics.IncXfer("direct_ticket", 1)

	writeJSON(w, http.StatusOK, map[string]any{
		"direct":      true,
		"ticket_id":   grant.TicketID,
		"secret":      secret,
		"addrs":       addrs,
		"expiry_unix": grant.ExpiryUnix,
	})
}
