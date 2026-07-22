package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/QiuShichang/go-rmm/proto"
)

// registerHTTP 注册 REST 路由（控制台调用）。
func (r *Relay) registerHTTP(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/agents", r.handleListAgents)
	mux.HandleFunc("POST /api/tasks", r.handlePostTask)
	mux.HandleFunc("POST /api/tasks/cancel", r.handleCancelTask) // C6 取消任务
	mux.HandleFunc("GET /api/tasks", r.handleListTasks)   // C4/F1 任务历史
	mux.HandleFunc("GET /api/tasks/", r.handleGetTask)    // C4 单任务详情+结果
	mux.HandleFunc("GET /api/events", r.handleSSE)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "ok")
	})
}

// GET /api/tasks —— 任务历史（最新在前，?limit=N，默认 50）。
func (r *Relay) handleListTasks(w http.ResponseWriter, req *http.Request) {
	if err := r.authorize(req); err != nil {
		writeJSON(w, http.StatusUnauthorized, errMap(err))
		return
	}
	if r.store == nil {
		writeJSON(w, http.StatusOK, map[string]any{"tasks": []any{}, "count": 0, "enabled": false})
		return
	}
	limit := 50
	if l := req.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}
	tasks, err := r.store.ListTasks(limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errMap(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": tasks, "count": len(tasks), "enabled": true})
}

// POST /api/tasks/cancel —— 取消任务（C6）。转发 task_cancel 到 Agent，并更新状态。
func (r *Relay) handleCancelTask(w http.ResponseWriter, req *http.Request) {
	if err := r.authorize(req); err != nil {
		writeJSON(w, http.StatusUnauthorized, errMap(err))
		return
	}
	var body struct {
		TaskID  string `json:"task_id"`
		AgentID string `json:"agent_id"`
		Reason  string `json:"reason,omitempty"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("请求体解析失败: %w", err)))
		return
	}
	if body.TaskID == "" {
		writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("task_id 不能为空")))
		return
	}

	// 从持久化找 AgentID（若请求未带）
	agentID := body.AgentID
	if agentID == "" && r.store != nil {
		if t, _ := r.store.GetTask(body.TaskID); t != nil {
			agentID = t.Request.AgentID
		}
	}
	if agentID == "" {
		writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("无法确定任务的 agent_id")))
		return
	}

	conn := r.agents.Conn(agentID)
	if conn == nil {
		writeJSON(w, http.StatusNotFound, errMap(fmt.Errorf("agent %s 不在线", agentID)))
		return
	}

	cancel := proto.TaskCancel{TaskID: body.TaskID, Reason: body.Reason}
	env, _ := proto.NewEnvelope(proto.TypeTaskCancel, "relay", agentID, cancel)
	ctx, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	if err := conn.writeJSON(ctx, env); err != nil {
		writeJSON(w, http.StatusBadGateway, errMap(fmt.Errorf("转发取消失败: %w", err)))
		return
	}

	// 更新持久化状态
	if r.store != nil {
		if t, _ := r.store.GetTask(body.TaskID); t != nil {
			t.Status = StatusCancelled
			_ = r.store.SaveTask(*t)
		}
	}
	log.Printf("[task] 取消 task=%s → agent=%s", body.TaskID, agentID)
	writeJSON(w, http.StatusAccepted, map[string]any{
		"task_id":  body.TaskID,
		"status":   StatusCancelled,
		"agent_id": agentID,
	})
}

// GET /api/tasks/{id} —— 单任务详情 + 结果。
func (r *Relay) handleGetTask(w http.ResponseWriter, req *http.Request) {
	if err := r.authorize(req); err != nil {
		writeJSON(w, http.StatusUnauthorized, errMap(err))
		return
	}
	if r.store == nil {
		writeJSON(w, http.StatusServiceUnavailable, errMap(fmt.Errorf("持久化未启用")))
		return
	}
	taskID := strings.TrimPrefix(req.URL.Path, "/api/tasks/")
	if taskID == "" {
		writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("缺少 task_id")))
		return
	}
	t, err := r.store.GetTask(taskID)
	if err != nil || t == nil {
		writeJSON(w, http.StatusNotFound, errMap(fmt.Errorf("任务不存在")))
		return
	}
	result, _ := r.store.GetResult(taskID)
	writeJSON(w, http.StatusOK, map[string]any{"task": t, "result": result})
}

// GET /api/agents —— 返回所有在线 Agent。
func (r *Relay) handleListAgents(w http.ResponseWriter, req *http.Request) {
	if err := r.authorize(req); err != nil {
		writeJSON(w, http.StatusUnauthorized, errMap(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"agents": r.agents.Snapshot(),
		"count":  len(r.agents.Snapshot()),
	})
}

// POST /api/tasks —— 控制台下发任务到指定 Agent。
//
// 请求体即 proto.TaskRequest。Relay 找到目标 Agent 连接后，把任务封装成 Envelope 转发。
func (r *Relay) handlePostTask(w http.ResponseWriter, req *http.Request) {
	if err := r.authorize(req); err != nil {
		writeJSON(w, http.StatusUnauthorized, errMap(err))
		return
	}
	var task proto.TaskRequest
	if err := json.NewDecoder(req.Body).Decode(&task); err != nil {
		writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("请求体解析失败: %w", err)))
		return
	}
	if task.TaskID == "" {
		writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("task_id 不能为空")))
		return
	}
	if task.AgentID == "" {
		writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("agent_id 不能为空")))
		return
	}
	if task.Type == "" {
		writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("type 不能为空")))
		return
	}
	// R5 修复：校验 type 在已知集合内，防透传未知类型给 Agent 制造噪音
	if !proto.IsValidTaskType(task.Type) {
		writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("未知的任务类型: %s", task.Type)))
		return
	}

	// C4 持久化：先存为 pending（即使 Agent 离线也不丢）
	now := time.Now().UnixMilli()
	stored := StoredTask{
		Request:   task,
		Status:    StatusPending,
		Attempt:   0,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if r.store != nil {
		if err := r.store.SaveTask(stored); err != nil {
			log.Printf("[task] 持久化失败(继续投递): %v", err)
		}
	}

	// 尝试立即投递（Agent 在线）
	conn := r.agents.Conn(task.AgentID)
	if conn == nil {
		// Agent 离线：保留 pending，等 Agent 上线后 dispatchPendingTasks 投递
		log.Printf("[task] 缓存 task=%s type=%s (agent=%s 离线，待上线投递)",
			task.TaskID, task.Type, task.AgentID)
		writeJSON(w, http.StatusAccepted, map[string]any{
			"task_id":  task.TaskID,
			"status":   StatusPending,
			"agent_id": task.AgentID,
			"message":  "agent 离线，任务已缓存待投递",
		})
		return
	}

	if err := r.dispatchTask(conn, task); err != nil {
		writeJSON(w, http.StatusBadGateway, errMap(fmt.Errorf("转发任务失败: %w", err)))
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]any{
		"task_id":  task.TaskID,
		"status":   StatusDispatched,
		"agent_id": task.AgentID,
	})
}

// dispatchTask 把任务封装为 Envelope 并转发给 Agent，更新持久化状态为 dispatched。
func (r *Relay) dispatchTask(conn *wsConn, task proto.TaskRequest) error {
	env, err := proto.NewEnvelope(proto.TypeTaskRequest, "relay", task.AgentID, task)
	if err != nil {
		return err
	}
	// 用独立 ctx（不绑请求生命周期，因 Agent 可能慢）
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := conn.writeJSON(ctx, env); err != nil {
		return err
	}
	// 更新持久化状态
	if r.store != nil {
		var st StoredTask
		if existing, err := r.store.GetTask(task.TaskID); err == nil && existing != nil {
			st = *existing
		} else {
			st = StoredTask{Request: task, CreatedAt: time.Now().UnixMilli()}
		}
		st.Status = StatusDispatched
		st.Attempt++
		st.Request = task
		_ = r.store.SaveTask(st)
		log.Printf("[task] 下发 task=%s type=%s → agent=%s (attempt=%d)",
			task.TaskID, task.Type, task.AgentID, st.Attempt)
	} else {
		log.Printf("[task] 下发 task=%s type=%s → agent=%s", task.TaskID, task.Type, task.AgentID)
	}
	return nil
}

// dispatchPendingTasks 在 Agent 上线时调用：找出该 Agent 的 pending 任务并投递。
func (r *Relay) dispatchPendingTasks(agentID string) {
	if r.store == nil {
		return
	}
	pending, err := r.store.ListTasksByStatus(StatusPending)
	if err != nil {
		log.Printf("[task] 查询 pending 任务失败: %v", err)
		return
	}
	conn := r.agents.Conn(agentID)
	if conn == nil {
		return
	}
	for _, t := range pending {
		if t.Request.AgentID != agentID {
			continue
		}
		if err := r.dispatchTask(conn, t.Request); err != nil {
			log.Printf("[task] 补投 task=%s 失败: %v", t.Request.TaskID, err)
			break // 连接可能断了，停止补投
		}
	}
}

// recoverInFlightTasks 在 Relay 启动时调用：把 dispatched 但未 ACK 的任务重新投递。
// 修复 R2：Relay 重启后内存丢失，但持久化的 dispatched 任务可重投。
func (r *Relay) recoverInFlightTasks() {
	if r.store == nil {
		return
	}
	inFlight, err := r.store.ListTasksByStatus(StatusDispatched, StatusAcknowledged)
	if err != nil {
		log.Printf("[task] 恢复在途任务查询失败: %v", err)
		return
	}
	if len(inFlight) > 0 {
		log.Printf("[task] 发现 %d 个在途任务待恢复", len(inFlight))
	}
	for _, t := range inFlight {
		// 重置为 pending，等 Agent 重连后 dispatchPendingTasks 投递
		t.Status = StatusPending
		_ = r.store.SaveTask(t)
	}
}

// GET /api/events —— SSE 推送实时事件给控制台。
//
// 事件流格式：
//
//	event: agent_online
//	data: {"agent_id":"...",...}
//
//	event: task_result
//	data: {"task_id":"...","success":true,...}
func (r *Relay) handleSSE(w http.ResponseWriter, req *http.Request) {
	if err := r.authorize(req); err != nil {
		writeJSON(w, http.StatusUnauthorized, errMap(err))
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // Nginx 关闭缓冲

	subID, ch := r.console.Subscribe(64)
	defer r.console.Unsubscribe(subID)
	log.Printf("[sse] 控制台订阅 %s 建立连接", subID)

	// 先推一个 hello 事件，便于客户端确认连接建立
	writeSSE(w, flusher, "hello", map[string]any{"sub_id": subID})
	flusher.Flush()

	notify := req.Context().Done()
	for {
		select {
		case <-notify:
			log.Printf("[sse] 控制台订阅 %s 断开", subID)
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			writeSSE(w, flusher, ev.Kind, map[string]any{
				"agent_id": ev.AgentID,
				"data":     json.RawMessage(ev.Payload),
				"ts":       ev.TS,
			})
			flusher.Flush()
		}
	}
}

// --- HTTP 辅助 ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func errMap(err error) map[string]any {
	return map[string]any{"error": err.Error()}
}

func writeSSE(w http.ResponseWriter, _ http.Flusher, event string, data any) {
	payload, _ := json.Marshal(data)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, payload)
}
