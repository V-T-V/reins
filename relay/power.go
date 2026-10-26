// Package main — Relay 电源操作端点(标签白名单鉴权)。
//
// POST /api/power —— 关机/重启/休眠。
// 强制校验目标 Agent 带 power_allowed=true 标签。
// 审计 power_executed 事件。
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/QiuShichang/reins/proto"
)

// PowerTagRequired 允许电源操作的标签。
const PowerTagKey = "power_allowed"
const PowerTagValue = "true"

// handlePower POST /api/power —— 触发电源操作。
//
// 请求体:
//
//	{"agent_id":"...", "op":"shutdown", "force":false, "delay":30, "message":"维护关机"}
//
// 安全:
//  1. authorize token 校验
//  2. 目标 Agent 必须带 power_allowed=true 标签(否则 403)
//  3. 审计记录
func (r *Relay) handlePower(w http.ResponseWriter, req *http.Request) {
	if err := r.authorize(req); err != nil {
		writeJSON(w, http.StatusUnauthorized, errMap(err))
		return
	}
	var body proto.PowerTask
	// 注:agent_id 不在 PowerTask 里,额外字段
	var raw struct {
		AgentID string `json:"agent_id"`
		Op      string `json:"op"`
		Force   bool   `json:"force"`
		Delay   int    `json:"delay"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(req.Body).Decode(&raw); err != nil {
		writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("请求体解析失败: %w", err)))
		return
	}
	if raw.AgentID == "" || raw.Op == "" {
		writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("agent_id 和 op 不能为空")))
		return
	}

	// 标签白名单校验:只允许带 power_allowed=true 的 Agent
	agent := r.findAgentByID(raw.AgentID)
	if agent == nil {
		writeJSON(w, http.StatusNotFound, errMap(fmt.Errorf("agent %s 不在线", raw.AgentID)))
		return
	}
	if agent.Tags == nil || agent.Tags[PowerTagKey] != PowerTagValue {
		writeJSON(w, http.StatusForbidden, errMap(fmt.Errorf("agent %s 未授权电源操作(需配置标签 %s=%s)",
			raw.AgentID, PowerTagKey, PowerTagValue)))
		return
	}

	// 构造 power 任务
	body = proto.PowerTask{
		Op:      raw.Op,
		Force:   raw.Force,
		Delay:   raw.Delay,
		Message: raw.Message,
	}
	payload, _ := json.Marshal(body)
	taskID := "power-" + raw.AgentID + "-" + fmt.Sprint(time.Now().UnixNano())
	task := proto.TaskRequest{
		TaskID:  taskID,
		Type:    proto.TaskPower,
		AgentID: raw.AgentID,
		Payload: payload,
		Timeout: 30,
	}

	// 持久化
	if r.store != nil {
		now := time.Now().UnixMilli()
		r.store.SaveTask(StoredTask{Request: task, Status: StatusPending, CreatedAt: now, UpdatedAt: now})
	}

	// 找连接投递
	conn := r.agents.Conn(raw.AgentID)
	if conn == nil {
		writeJSON(w, http.StatusNotFound, errMap(fmt.Errorf("agent %s 不在线", raw.AgentID)))
		return
	}
	if err := r.dispatchTask(conn, task); err != nil {
		writeJSON(w, http.StatusBadGateway, errMap(fmt.Errorf("下发失败: %w", err)))
		return
	}

	// 审计
	if r.audit != nil {
		r.audit.Log(AuditEntry{
			Event:   "power_" + raw.Op,
			AgentID: raw.AgentID,
			TaskID:  taskID,
			Detail:  fmt.Sprintf("op=%s force=%v delay=%d", raw.Op, raw.Force, raw.Delay),
		})
	}
	notifyPowerExecuted(raw.AgentID, raw.Op, raw.Delay) // F11:webhook 告警(电源操作)

	writeJSON(w, http.StatusAccepted, map[string]any{
		"agent_id": raw.AgentID,
		"task_id":  taskID,
		"op":       raw.Op,
		"delay":    raw.Delay,
		"message":  "电源操作已下发",
	})
}

// findAgentByID 从 registry 查 agent info(含 Tags)。
func (r *Relay) findAgentByID(agentID string) *proto.Register {
	for _, a := range r.agents.Snapshot() {
		if a.AgentID == agentID {
			return &a
		}
	}
	return nil
}
