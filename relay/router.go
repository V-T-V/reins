package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/QiuShichang/go-rmm/proto"
)

// registerHTTP 注册 REST 路由（控制台调用）。
func (r *Relay) registerHTTP(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/agents", r.handleListAgents)
	mux.HandleFunc("POST /api/tasks", r.handlePostTask)
	mux.HandleFunc("POST /api/tasks/batch", r.handleBatchTask) // C10 批量下发
	mux.HandleFunc("POST /api/tasks/cancel", r.handleCancelTask) // C6 取消任务
	mux.HandleFunc("GET /api/tasks", r.handleListTasks)   // C4/F1 任务历史
	mux.HandleFunc("GET /api/tasks/", r.handleGetTask)    // C4 单任务详情+结果
	mux.HandleFunc("POST /api/enroll-token", r.handleGenEnrollToken) // C7 生成注册令牌
	mux.HandleFunc("POST /api/enroll", r.handleEnroll)               // C7 Agent 换证书
	mux.HandleFunc("POST /api/agents/revoke", r.handleRevokeAgent)   // G2 撤销 Agent 证书
	mux.HandleFunc("GET /api/crons", r.handleListCrons)             // H2 cron 列表
	mux.HandleFunc("POST /api/crons", r.handleAddCron)              // H2 新建 cron
	mux.HandleFunc("DELETE /api/crons/", r.handleDeleteCron)        // H2 删除 cron
	mux.HandleFunc("POST /api/shell/open", r.handleShellOpen)       // H3 开终端会话
	mux.HandleFunc("POST /api/shell/input", r.handleShellInput)     // H3 发 stdin
	mux.HandleFunc("POST /api/shell/close", r.handleShellClose)     // H3 关会话
	mux.HandleFunc("POST /api/transfer/upload", r.handleTransferUpload)   // J2 上传
	mux.HandleFunc("POST /api/transfer/download", r.handleTransferDownload) // J2 下载
	mux.HandleFunc("GET /api/events", r.handleSSE)
	mux.HandleFunc("GET /api/metrics", r.handleMetrics) // C10 可观测性
	mux.HandleFunc("GET /healthz", r.handleHealthz) // D8 深度健康检查
}

// GET /healthz —— 深度健康检查（D8 / P3-1）。
// 检查 store 可用性，返回各组件状态 JSON。
func (r *Relay) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	status := map[string]any{
		"relay":     "ok",
		"store":     "disabled",
		"agents":    len(r.agents.Snapshot()),
		"uptime_s":  int(time.Since(metrics.StartedAt).Seconds()),
	}
	httpStatus := http.StatusOK
	if r.store != nil {
		// 探测 store：能否查询（不实际读数据）
		if _, err := r.store.ListTasksByStatus(StatusPending); err != nil {
			status["store"] = "error: " + err.Error()
			httpStatus = http.StatusServiceUnavailable
		} else {
			status["store"] = "ok"
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpStatus)
	json.NewEncoder(w).Encode(status)
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

// POST /api/tasks/batch —— 按标签批量下发任务（C10）。
//
// 请求体:
//
//	{"tag": "env=prod", "type": "powershell", "payload": {...}, "timeout": 30}
//
// 响应: {"dispatched": ["agent-1","agent-2"], "offline": [], "task_ids": {...}}
func (r *Relay) handleBatchTask(w http.ResponseWriter, req *http.Request) {
	if err := r.authorize(req); err != nil {
		writeJSON(w, http.StatusUnauthorized, errMap(err))
		return
	}
	var body struct {
		Tag     string          `json:"tag"`     // 如 "env=prod"
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
		Timeout int             `json:"timeout"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("请求体解析失败: %w", err)))
		return
	}
	if body.Tag == "" || body.Type == "" {
		writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("tag 和 type 不能为空")))
		return
	}
	if !proto.IsValidTaskType(body.Type) {
		writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("未知的任务类型: %s", body.Type)))
		return
	}

	// P1-1 修复（致命安全洞）：批量下发必须经过命令策略校验，否则可绕过 S4 下发 Format-Volume
	if err := r.policy.CheckPayload(body.Type, body.Payload); err != nil {
		log.Printf("[policy] 批量任务被拦截 tag=%s type=%s: %v", body.Tag, body.Type, err)
		metrics.Inc("blocked")
		if r.audit != nil {
			r.audit.Log(AuditEntry{Event: "task_blocked", Type: body.Type, Detail: "batch:" + body.Tag + " " + err.Error()})
		}
		writeJSON(w, http.StatusForbidden, errMap(err))
		return
	}

	// 找匹配 Agent
	agentIDs := r.agents.FindByTag(body.Tag)
	if len(agentIDs) == 0 {
		writeJSON(w, http.StatusNotFound, errMap(fmt.Errorf("无带标签 %s 的在线 Agent", body.Tag)))
		return
	}

	taskIDs := make(map[string]string)
	dispatched := []string{}
	for _, aid := range agentIDs {
		taskID := "batch-" + body.Type + "-" + aid + "-" + fmt.Sprint(time.Now().UnixMilli())
		task := proto.TaskRequest{
			TaskID:  taskID,
			Type:    body.Type,
			AgentID: aid,
			Payload: body.Payload,
			Timeout: body.Timeout,
		}
		// P2-1 修复：批量路径补 metrics 打点
		metrics.Inc("submitted")
		// 持久化 + 投递（复用 handlePostTask 的逻辑）
		if r.store != nil {
			r.store.SaveTask(StoredTask{Request: task, Status: StatusPending, CreatedAt: time.Now().UnixMilli()})
		}
		conn := r.agents.Conn(aid)
		if conn != nil {
			if err := r.dispatchTask(conn, task); err == nil {
				dispatched = append(dispatched, aid)
				taskIDs[aid] = taskID
			}
		}
		if r.audit != nil {
			r.audit.Log(AuditEntry{Event: "task_submitted", AgentID: aid, TaskID: taskID, Type: body.Type, Detail: "batch:" + body.Tag})
		}
	}
	log.Printf("[task] 批量下发 tag=%s → %d agents", body.Tag, len(dispatched))
	writeJSON(w, http.StatusAccepted, map[string]any{
		"tag":        body.Tag,
		"dispatched": dispatched,
		"count":      len(dispatched),
		"task_ids":   taskIDs,
	})
}

// summarizePayload 提取载荷摘要用于审计（命令前 200 字符，避免完整记录敏感数据）。
func summarizePayload(taskType string, payload []byte) string {
	const maxLen = 200
	var text string
	switch taskType {
	case "powershell":
		var t struct{ Command string `json:"command"` }
		json.Unmarshal(payload, &t)
		text = t.Command
	case "script":
		var t struct{ Content string `json:"content"` }
		json.Unmarshal(payload, &t)
		text = t.Content
	default:
		return fmt.Sprintf("%s(%d bytes)", taskType, len(payload))
	}
	if len(text) > maxLen {
		text = text[:maxLen] + "..."
	}
	return text
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

	// 更新持久化状态（D5：状态机校验，终态不可变更）
	if r.store != nil {
		if err := r.store.UpdateTaskStatus(body.TaskID, StatusCancelled); err == nil {
			metrics.Inc("cancelled") // D7
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
	// P2-10 修复：只调一次 Snapshot（原双次调用有 2x 拷贝开销 + 可能不一致）
	list := r.agents.Snapshot()
	writeJSON(w, http.StatusOK, map[string]any{
		"agents": list,
		"count":  len(list),
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

	// C9 命令安全策略校验：拦截高危命令
	if err := r.policy.CheckPayload(task.Type, task.Payload); err != nil {
		log.Printf("[policy] 拦截高危任务 task=%s type=%s: %v", task.TaskID, task.Type, err)
		metrics.Inc("blocked")
		if r.audit != nil {
			r.audit.Log(AuditEntry{
				Event:   "task_blocked",
				AgentID: task.AgentID,
				TaskID:  task.TaskID,
				Type:    task.Type,
				Detail:  err.Error(),
			})
		}
		writeJSON(w, http.StatusForbidden, errMap(err))
		return
	}

	// C9 审计：记录任务提交
	metrics.Inc("submitted")
	if r.audit != nil {
		r.audit.Log(AuditEntry{
			Event:   "task_submitted",
			AgentID: task.AgentID,
			TaskID:  task.TaskID,
			Type:    task.Type,
			Detail:  summarizePayload(task.Type, task.Payload),
		})
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
	// E2/W1：maxAttempts 兜底，防 poison task 无限重投
	if r.store != nil {
		if existing, _ := r.store.GetTask(task.TaskID); existing != nil && existing.Attempt >= MaxTaskAttempts {
			log.Printf("[task] task=%s 达到最大投递次数 %d，转 failed", task.TaskID, MaxTaskAttempts)
			r.store.UpdateTaskStatus(task.TaskID, StatusFailed)
			r.store.SaveResult(task.TaskID, proto.TaskResult{
				TaskID: task.TaskID, Success: false,
				Error: fmt.Sprintf("达到最大投递次数 %d", MaxTaskAttempts),
			})
			metrics.Inc("failed")
			if r.audit != nil {
				r.audit.Log(AuditEntry{Event: "task_max_attempts", TaskID: task.TaskID, Detail: fmt.Sprintf("超过 %d 次", MaxTaskAttempts)})
			}
			return fmt.Errorf("任务 %s 超过最大投递次数", task.TaskID)
		}
	}

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
		metrics.Inc("dispatched")
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
			// M5 修复：补投失败用 continue 而非 break，
			// 避免尾部任务在 Agent 频繁闪断时长期饥饿
			log.Printf("[task] 补投 task=%s 失败(跳过，继续下一个): %v", t.Request.TaskID, err)
			continue
		}
	}
}

// recoverInFlightTasks 在 Relay 启动时调用：把 dispatched 但未 ACK 的任务重新投递。
// 修复 R2：Relay 重启后内存丢失，但持久化的 dispatched 任务可重投。
//
// P1-5 修复：只重置 StatusDispatched（Agent 未 ACK），**不动 StatusAcknowledged**。
// acknowledged 表示 Agent 已收到正在执行，重投会导致重复执行（即使有幂等兜底，
// Agent 重启后幂等缓存也空了）。acknowledged 任务由 reaper 按超时阈值单独处理。
func (r *Relay) recoverInFlightTasks() {
	if r.store == nil {
		return
	}
	// 只恢复 dispatched（未 ACK），acknowledged 保持原状
	dispatched, err := r.store.ListTasksByStatus(StatusDispatched)
	if err != nil {
		log.Printf("[task] 恢复在途任务查询失败: %v", err)
		return
	}
	if len(dispatched) > 0 {
		log.Printf("[task] 发现 %d 个未 ACK 的 dispatched 任务待恢复", len(dispatched))
	}
	for _, t := range dispatched {
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

	// M7 修复：SSE 写超时，防止慢 Console 挂住 goroutine
	rc := http.NewResponseController(w)
	rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
	defer rc.SetWriteDeadline(time.Time{}) // 清除 deadline

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

// POST /api/enroll-token —— 管理员为指定 agentID 生成一次性注册令牌（C7）。
//
// 请求体: {"agent_id": "agent-host01"}
// 响应: {"agent_id":"...", "token":"<32字节hex>", "expires_in":3600}
func (r *Relay) handleGenEnrollToken(w http.ResponseWriter, req *http.Request) {
	if err := r.authorize(req); err != nil {
		writeJSON(w, http.StatusUnauthorized, errMap(err))
		return
	}
	if r.ca == nil {
		writeJSON(w, http.StatusServiceUnavailable, errMap(fmt.Errorf("CA 未启用")))
		return
	}
	var body struct {
		AgentID string `json:"agent_id"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil || body.AgentID == "" {
		writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("agent_id 不能为空")))
		return
	}
	token, err := r.ca.GenerateEnrollmentToken(body.AgentID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errMap(err))
		return
	}
	log.Printf("[ca] 为 %s 生成注册令牌", body.AgentID)
	writeJSON(w, http.StatusOK, map[string]any{
		"agent_id":   body.AgentID,
		"token":      token,
		"expires_in": 3600,
	})
}

// POST /api/enroll —— Agent 用一次性令牌 + CSR 换取客户端证书（C7）。
//
// 请求体: proto.Enroll
// 响应: proto.EnrollResp
func (r *Relay) handleEnroll(w http.ResponseWriter, req *http.Request) {
	if r.ca == nil {
		writeJSON(w, http.StatusServiceUnavailable, errMap(fmt.Errorf("CA 未启用")))
		return
	}
	var enroll proto.Enroll
	if err := json.NewDecoder(req.Body).Decode(&enroll); err != nil {
		writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("请求体解析失败: %w", err)))
		return
	}
	if enroll.AgentID == "" || enroll.Token == "" || enroll.CSRPEM == "" {
		writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("agent_id/token/csr_pem 不能为空")))
		return
	}

	// 校验令牌（一次性，校验后作废）
	if !r.ca.ValidateEnrollmentToken(enroll.Token, enroll.AgentID) {
		log.Printf("[ca] 注册拒绝: 无效令牌 agent=%s", enroll.AgentID)
		writeJSON(w, http.StatusUnauthorized, errMap(fmt.Errorf("无效或已使用的注册令牌")))
		return
	}

	// 签发证书（24h）
	const certTTL = 24 * time.Hour
	certPEM, err := r.ca.SignAgentCSR([]byte(enroll.CSRPEM), enroll.AgentID, certTTL)
	if err != nil {
		log.Printf("[ca] 签发失败 agent=%s: %v", enroll.AgentID, err)
		writeJSON(w, http.StatusInternalServerError, errMap(err))
		return
	}

	resp := proto.EnrollResp{
		AgentID:   enroll.AgentID,
		CertPEM:   string(certPEM),
		CAPEM:     string(r.ca.CAPEM()),
		ExpiresAt: time.Now().Add(certTTL).Unix(),
		Success:   true,
	}
	log.Printf("[ca] 签发证书 agent=%s (24h)", enroll.AgentID)
	writeJSON(w, http.StatusOK, resp)
}

// POST /api/agents/revoke —— 撤销 Agent 证书（G2/A2 修复）。
//
// 请求体: {"agent_id": "agent-prod-01"}
// 撤销后该 Agent 的 mTLS 握手会被 VerifyPeerCertificate 钩子拒绝（CRL 校验）。
// 撤销状态持久化到 ca-state.json，Relay 重启不丢失。
func (r *Relay) handleRevokeAgent(w http.ResponseWriter, req *http.Request) {
	if err := r.authorize(req); err != nil {
		writeJSON(w, http.StatusUnauthorized, errMap(err))
		return
	}
	if r.ca == nil {
		writeJSON(w, http.StatusServiceUnavailable, errMap(fmt.Errorf("CA 未启用")))
		return
	}
	var body struct {
		AgentID string `json:"agent_id"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil || body.AgentID == "" {
		writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("agent_id 不能为空")))
		return
	}

	r.ca.RevokeAgent(body.AgentID)
	// C3 修复：撤销后主动关闭 WS 连接（踢下线）。
	// 已建立的 mTLS 连接不会被 CRL 中断（握手已过），必须主动断开。
	// Agent 重连时在握手阶段被 CRL 拒绝。
	conn := r.agents.Conn(body.AgentID)
	if conn != nil {
		conn.close(websocket.StatusPolicyViolation, "certificate revoked")
	}
	r.agents.Unregister(body.AgentID, conn)
	r.console.Broadcast(Event{
		Kind:    proto.TypeAgentOffline,
		AgentID: body.AgentID,
		TS:      time.Now().UnixMilli(),
	})
	if r.audit != nil {
		r.audit.Log(AuditEntry{Event: "agent_revoked", AgentID: body.AgentID, Detail: "证书撤销"})
	}
	log.Printf("[ca] 撤销 Agent %s 证书", body.AgentID)
	writeJSON(w, http.StatusOK, map[string]any{
		"agent_id": body.AgentID,
		"status":   "revoked",
	})
}

// --- H2 定时任务 ---

// GET /api/crons —— 列出所有定时规则。
func (r *Relay) handleListCrons(w http.ResponseWriter, req *http.Request) {
	if err := r.authorize(req); err != nil {
		writeJSON(w, http.StatusUnauthorized, errMap(err))
		return
	}
	if r.scheduler == nil {
		writeJSON(w, http.StatusOK, map[string]any{"crons": []any{}, "enabled": false})
		return
	}
	rules := r.scheduler.ListRules()
	writeJSON(w, http.StatusOK, map[string]any{"crons": rules, "count": len(rules), "enabled": true})
}

// POST /api/crons —— 新建定时规则。
func (r *Relay) handleAddCron(w http.ResponseWriter, req *http.Request) {
	if err := r.authorize(req); err != nil {
		writeJSON(w, http.StatusUnauthorized, errMap(err))
		return
	}
	if r.scheduler == nil {
		writeJSON(w, http.StatusServiceUnavailable, errMap(fmt.Errorf("调度器未启用")))
		return
	}
	var rule CronRule
	if err := json.NewDecoder(req.Body).Decode(&rule); err != nil {
		writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("请求体解析失败: %w", err)))
		return
	}
	if rule.CronExpr == "" || rule.AgentID == "" || rule.TaskType == "" {
		writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("cron_expr/agent_id/task_type 不能为空")))
		return
	}
	if !proto.IsValidTaskType(rule.TaskType) {
		writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("未知的任务类型: %s", rule.TaskType)))
		return
	}
	// 生成 ID
	if rule.ID == "" {
		rule.ID = "cron-" + fmt.Sprint(time.Now().UnixMilli())
	}
	if err := r.scheduler.AddRule(rule); err != nil {
		writeJSON(w, http.StatusBadRequest, errMap(err))
		return
	}
	if r.audit != nil {
		r.audit.Log(AuditEntry{Event: "cron_added", AgentID: rule.AgentID, Detail: rule.CronExpr})
	}
	writeJSON(w, http.StatusCreated, rule)
}

// DELETE /api/crons/{id} —— 删除定时规则。
func (r *Relay) handleDeleteCron(w http.ResponseWriter, req *http.Request) {
	if err := r.authorize(req); err != nil {
		writeJSON(w, http.StatusUnauthorized, errMap(err))
		return
	}
	ruleID := strings.TrimPrefix(req.URL.Path, "/api/crons/")
	if ruleID == "" {
		writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("缺少规则 ID")))
		return
	}
	if r.scheduler == nil {
		writeJSON(w, http.StatusServiceUnavailable, errMap(fmt.Errorf("调度器未启用")))
		return
	}
	if err := r.scheduler.RemoveRule(ruleID); err != nil {
		writeJSON(w, http.StatusInternalServerError, errMap(err))
		return
	}
	if r.audit != nil {
		r.audit.Log(AuditEntry{Event: "cron_deleted", Detail: ruleID})
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": ruleID, "deleted": true})
}

// --- H3 交互式终端 REST ---

// POST /api/shell/open —— 开启终端会话。
// 请求体: {"agent_id":"...", "cols":80, "rows":24, "shell":"powershell"}
func (r *Relay) handleShellOpen(w http.ResponseWriter, req *http.Request) {
	if err := r.authorize(req); err != nil {
		writeJSON(w, http.StatusUnauthorized, errMap(err))
		return
	}
	var body struct {
		AgentID string `json:"agent_id"`
		Cols    int    `json:"cols"`
		Rows    int    `json:"rows"`
		Shell   string `json:"shell"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil || body.AgentID == "" {
		writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("agent_id 不能为空")))
		return
	}
	conn := r.agents.Conn(body.AgentID)
	if conn == nil {
		writeJSON(w, http.StatusNotFound, errMap(fmt.Errorf("agent %s 不在线", body.AgentID)))
		return
	}
	sessionID := "shell-" + fmt.Sprint(time.Now().UnixNano())
	open := proto.ShellOpen{
		SessionID: sessionID, Cols: body.Cols, Rows: body.Rows, Shell: body.Shell,
	}
	env, _ := proto.NewEnvelope(proto.TypeShellOpen, "relay", body.AgentID, open)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.writeJSON(ctx, env); err != nil {
		writeJSON(w, http.StatusBadGateway, errMap(fmt.Errorf("转发 shell_open 失败: %w", err)))
		return
	}
	if r.audit != nil {
		r.audit.Log(AuditEntry{Event: "shell_opened", AgentID: body.AgentID, Detail: sessionID})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id": sessionID,
		"agent_id":   body.AgentID,
	})
}

// POST /api/shell/input —— 向会话发送 stdin。
// 请求体: {"session_id":"...", "agent_id":"...", "data_b64":"..."}
func (r *Relay) handleShellInput(w http.ResponseWriter, req *http.Request) {
	if err := r.authorize(req); err != nil {
		writeJSON(w, http.StatusUnauthorized, errMap(err))
		return
	}
	var body struct {
		SessionID string `json:"session_id"`
		AgentID   string `json:"agent_id"`
		DataB64   string `json:"data_b64"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil || body.SessionID == "" {
		writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("session_id 不能为空")))
		return
	}
	conn := r.agents.Conn(body.AgentID)
	if conn == nil {
		writeJSON(w, http.StatusNotFound, errMap(fmt.Errorf("agent 不在线")))
		return
	}
	input := proto.ShellInput{SessionID: body.SessionID, DataB64: body.DataB64}
	env, _ := proto.NewEnvelope(proto.TypeShellInput, "relay", body.AgentID, input)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.writeJSON(ctx, env); err != nil {
		writeJSON(w, http.StatusBadGateway, errMap(err))
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"session_id": body.SessionID})
}

// POST /api/shell/close —— 关闭会话。
func (r *Relay) handleShellClose(w http.ResponseWriter, req *http.Request) {
	if err := r.authorize(req); err != nil {
		writeJSON(w, http.StatusUnauthorized, errMap(err))
		return
	}
	var body struct {
		SessionID string `json:"session_id"`
		AgentID   string `json:"agent_id"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil || body.SessionID == "" {
		writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("session_id 不能为空")))
		return
	}
	conn := r.agents.Conn(body.AgentID)
	if conn != nil {
		sc := proto.ShellClose{SessionID: body.SessionID, Reason: "console closed"}
		env, _ := proto.NewEnvelope(proto.TypeShellClose, "relay", body.AgentID, sc)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = conn.writeJSON(ctx, env)
	}
	if r.audit != nil {
		r.audit.Log(AuditEntry{Event: "shell_closed", AgentID: body.AgentID, Detail: body.SessionID})
	}
	writeJSON(w, http.StatusOK, map[string]any{"session_id": body.SessionID, "closed": true})
}

// --- J2 大文件传输 stream-through ---

// handleTransferUpload 上传：Console HTTP body → Relay 转发 init → Agent ready → 二进制帧流。
//
// Console 用 multipart 或 raw body 上传文件。Relay 做 stream-through：
// 1. 发 file_xfer_init 给 Agent
// 2. 等 Agent ready（含续传偏移）
// 3. 读 HTTP body 分块转二进制 WS 帧发给 Agent
// 4. Agent 收完算 SHA256 + 回 complete
// Relay 不落盘，仅 256KB 缓冲转发。
func (r *Relay) handleTransferUpload(w http.ResponseWriter, req *http.Request) {
	if err := r.authorize(req); err != nil {
		writeJSON(w, http.StatusUnauthorized, errMap(err))
		return
	}
	agentID := req.URL.Query().Get("agent_id")
	path := req.URL.Query().Get("path")
	transferID := req.URL.Query().Get("transfer_id")
	if agentID == "" || path == "" {
		writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("agent_id 和 path 不能为空")))
		return
	}
	if transferID == "" {
		transferID = "xfer-" + fmt.Sprint(time.Now().UnixNano())
	}

	conn := r.agents.Conn(agentID)
	if conn == nil {
		writeJSON(w, http.StatusNotFound, errMap(fmt.Errorf("agent %s 不在线", agentID)))
		return
	}

	// 1. 发 init
	init := proto.FileTransferInit{
		TransferID: transferID,
		Direction:  "upload",
		Path:       path,
		Size:       req.ContentLength,
	}
	initEnv, _ := proto.NewEnvelope(proto.TypeFileTransferInit, "relay", agentID, init)
	ctx, cancel := context.WithTimeout(req.Context(), 30*time.Minute) // 大文件给足时间
	defer cancel()
	if err := conn.writeJSON(ctx, initEnv); err != nil {
		writeJSON(w, http.StatusBadGateway, errMap(fmt.Errorf("发送 init 失败: %w", err)))
		return
	}

	// 2. stream-through：读 HTTP body → 二进制 WS 帧
	buf := make([]byte, 256*1024)
	totalSent := int64(0)
	for {
		n, readErr := req.Body.Read(buf)
		if n > 0 {
			frame := buildRelayBinaryFrame(transferID, false, buf[:n])
			conn.writeMu.Lock()
			if err := conn.conn.Write(ctx, websocket.MessageBinary, frame); err != nil {
				conn.writeMu.Unlock()
				writeJSON(w, http.StatusBadGateway, errMap(fmt.Errorf("WS写失败: %w", err)))
				return
			}
			conn.writeMu.Unlock()
			totalSent += int64(n)
		}
		if readErr != nil {
			break
		}
	}
	// 发 isLast 帧
	frame := buildRelayBinaryFrame(transferID, true, nil)
	conn.writeMu.Lock()
	conn.conn.Write(ctx, websocket.MessageBinary, frame)
	conn.writeMu.Unlock()

	if r.audit != nil {
		r.audit.Log(AuditEntry{Event: "file_upload", AgentID: agentID, Detail: fmt.Sprintf("%s %d bytes", transferID, totalSent)})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"transfer_id": transferID,
		"bytes":       totalSent,
		"status":      "uploaded",
	})
}

// handleTransferDownload 下载：Agent 读文件 → 二进制帧 → Relay → Console HTTP response。
func (r *Relay) handleTransferDownload(w http.ResponseWriter, req *http.Request) {
	if err := r.authorize(req); err != nil {
		writeJSON(w, http.StatusUnauthorized, errMap(err))
		return
	}
	agentID := req.URL.Query().Get("agent_id")
	path := req.URL.Query().Get("path")
	transferID := req.URL.Query().Get("transfer_id")
	if agentID == "" || path == "" {
		writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("agent_id 和 path 不能为空")))
		return
	}
	if transferID == "" {
		transferID = "xfer-" + fmt.Sprint(time.Now().UnixNano())
	}

	conn := r.agents.Conn(agentID)
	if conn == nil {
		writeJSON(w, http.StatusNotFound, errMap(fmt.Errorf("agent %s 不在线", agentID)))
		return
	}

	// 发 init（download 方向）
	init := proto.FileTransferInit{
		TransferID: transferID,
		Direction:  "download",
		Path:       path,
	}
	initEnv, _ := proto.NewEnvelope(proto.TypeFileTransferInit, "relay", agentID, init)
	ctx, cancel := context.WithTimeout(req.Context(), 30*time.Minute)
	defer cancel()
	if err := conn.writeJSON(ctx, initEnv); err != nil {
		writeJSON(w, http.StatusBadGateway, errMap(fmt.Errorf("发送 init 失败: %w", err)))
		return
	}

	// B1 修复：不再直接 Read WS（会与 serveAgent 竞态），
	// 改为注册 channel，serveAgent 的 Read 循环会路由二进制帧过来。
	ch := conn.registerXferChannel(transferID)
	defer conn.unregisterXferChannel(transferID)

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", filepath.Base(path)))
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	totalWritten := int64(0)
	for {
		select {
		case <-ctx.Done():
			goto done
		case data, ok := <-ch:
			if !ok {
				goto done // channel 关闭
			}
			if len(data) > 0 {
				w.Write(data)
				if flusher != nil {
					flusher.Flush()
				}
				totalWritten += int64(len(data))
			}
		}
	}
done:
	if r.audit != nil {
		r.audit.Log(AuditEntry{Event: "file_download", AgentID: agentID, Detail: fmt.Sprintf("%s %d bytes", transferID, totalWritten)})
	}
}

// buildRelayBinaryFrame 构造二进制帧（与 Agent 的 buildBinaryFrame 格式一致）。
func buildRelayBinaryFrame(transferID string, isLast bool, data []byte) []byte {
	idBytes := []byte(transferID)
	if len(idBytes) > 255 {
		idBytes = idBytes[:255]
	}
	frame := make([]byte, 0, 1+len(idBytes)+1+len(data))
	frame = append(frame, byte(len(idBytes)))
	frame = append(frame, idBytes...)
	if isLast {
		frame = append(frame, 1)
	} else {
		frame = append(frame, 0)
	}
	frame = append(frame, data...)
	return frame
}

// parseRelayBinaryFrame 解析二进制帧。
func parseRelayBinaryFrame(frame []byte) (transferID string, isLast bool, data []byte, err error) {
	if len(frame) < 2 {
		return "", false, nil, fmt.Errorf("帧太短")
	}
	idLen := int(frame[0])
	if len(frame) < 1+idLen+1 {
		return "", false, nil, fmt.Errorf("帧不完整")
	}
	transferID = string(frame[1 : 1+idLen])
	isLast = frame[1+idLen] != 0
	data = frame[2+idLen:]
	return transferID, isLast, data, nil
}
