// Package main — 流式任务端点(O1/O2:实时 stdout SSE 推送)。
//
// POST /api/tasks/stream —— 下发任务 + SSE 实时推送进度/输出/结果。
//
// 与 POST /api/tasks 的区别:
//   - /api/tasks    → 返回 task_id,需轮询 GET /api/tasks/{id} 拿结果
//   - /api/tasks/stream → 单连接 SSE,实时推送 task_progress + task_result
//
// AI Agent / CLI 用此端点拿实时输出(类似 kubectl logs -f)。
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/QiuShichang/go-rmm/proto"
)

// handleStreamTask POST /api/tasks/stream —— 下发任务 + SSE 实时推送。
//
// 请求体: proto.TaskRequest(同 POST /api/tasks)
// 响应: SSE 事件流
//
//	event: task_progress
//	data: {"task_id":"...","step":"...","message":"...","percent":50}
//
//	event: task_result
//	data: {"task_id":"...","success":true,"stdout":"..."}
func (r *Relay) handleStreamTask(w http.ResponseWriter, req *http.Request) {
	var task proto.TaskRequest
	if err := json.NewDecoder(req.Body).Decode(&task); err != nil {
		writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("请求体解析失败: %w", err)))
		return
	}
	if task.TaskID == "" || task.AgentID == "" || task.Type == "" {
		writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("task_id/agent_id/type 不能为空")))
		return
	}
	if !proto.IsValidTaskType(task.Type) {
		writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("未知任务类型: %s", task.Type)))
		return
	}

	// 策略校验
	if err := r.policy.CheckCommandPayload(task.Type, task.Payload); err != nil {
		writeJSON(w, http.StatusForbidden, errMap(err))
		return
	}

	// 能力校验
	if !r.agentSupports(task.AgentID, task.Type) {
		writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("agent %s 不支持 %s", task.AgentID, task.Type)))
		return
	}

	// SSE 头
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, errMap(fmt.Errorf("streaming 不支持")))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	// 推 hello
	writeSSE(w, flusher, "task_submitted", map[string]any{
		"task_id": task.TaskID, "agent_id": task.AgentID, "type": task.Type,
	})
	flusher.Flush()

	// 持久化
	now := time.Now().UnixMilli()
	if r.store != nil {
		r.store.SaveTask(StoredTask{Request: task, Status: StatusPending, CreatedAt: now, UpdatedAt: now})
	}

	// 投递
	conn := r.agents.Conn(task.AgentID)
	if conn == nil {
		writeSSE(w, flusher, "error", map[string]any{"message": "agent 离线"})
		flusher.Flush()
		return
	}
	if err := r.dispatchTask(conn, task); err != nil {
		writeSSE(w, flusher, "error", map[string]any{"message": fmt.Sprintf("投递失败: %v", err)})
		flusher.Flush()
		return
	}
	writeSSE(w, flusher, "task_dispatched", map[string]any{"task_id": task.TaskID})
	flusher.Flush()

	// 订阅 EventBus 拿 task_progress + task_result
	subID, ch := r.console.Subscribe(128)
	defer r.console.Unsubscribe(subID)

	notify := req.Context().Done()
	timeout := time.Duration(task.Timeout+10) * time.Second
	if timeout > 600*time.Second {
		timeout = 600 * time.Second
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case <-notify:
			return
		case <-timer.C:
			writeSSE(w, flusher, "timeout", map[string]any{"task_id": task.TaskID})
			flusher.Flush()
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			// 只推当前 task 的事件
			// 从 Payload 解析 task_id
			var p struct {
				TaskID string `json:"task_id"`
			}
			json.Unmarshal(ev.Payload, &p)
			if p.TaskID != task.TaskID {
				continue
			}
			// 推送
			writeSSE(w, flusher, ev.Kind, map[string]any{
				"agent_id": ev.AgentID,
				"data":     json.RawMessage(ev.Payload),
				"ts":       ev.TS,
			})
			flusher.Flush()
			// task_result 是终态
			if ev.Kind == proto.TypeTaskResult {
				return
			}
		}
	}
}

// 确保 context 被引用(streamTask 不直接用,但 import 保持一致)。
