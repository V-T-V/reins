package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"github.com/QiuShichang/go-rmm/proto"
)

// registerHTTP 注册 REST 路由（控制台调用）。
func (r *Relay) registerHTTP(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/agents", r.handleListAgents)
	mux.HandleFunc("POST /api/tasks", r.handlePostTask)
	mux.HandleFunc("GET /api/events", r.handleSSE)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "ok")
	})
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

	conn := r.agents.Conn(task.AgentID)
	if conn == nil {
		writeJSON(w, http.StatusNotFound, errMap(fmt.Errorf("agent %s 不在线", task.AgentID)))
		return
	}

	// 封装为 Envelope 转发给 Agent
	env, err := proto.NewEnvelope(proto.TypeTaskRequest, "relay", task.AgentID, task)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errMap(err))
		return
	}
	ctx := req.Context()
	if err := conn.writeJSON(ctx, env); err != nil {
		writeJSON(w, http.StatusBadGateway, errMap(fmt.Errorf("转发任务失败: %w", err)))
		return
	}

	log.Printf("[task] 下发 task=%s type=%s → agent=%s", task.TaskID, task.Type, task.AgentID)
	writeJSON(w, http.StatusAccepted, map[string]any{
		"task_id":  task.TaskID,
		"status":   "dispatched",
		"agent_id": task.AgentID,
	})
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
