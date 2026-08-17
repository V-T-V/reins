package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/QiuShichang/go-rmm/proto"
)

// registerHTTP 注册 REST 路由（控制台调用）。
func (r *Relay) registerHTTP(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/agents", r.handleListAgents)
	mux.HandleFunc("POST /api/tasks", r.handlePostTask)
	mux.HandleFunc("POST /api/tasks/batch", r.handleBatchTask)              // C10 批量下发
	mux.HandleFunc("POST /api/batch", r.handleBatchStart)                   // P0-1 异步批量(重试+进度)
	mux.HandleFunc("GET /api/batch/{id}", r.handleBatchStatus)              // P0-1 批次进度快照
	mux.HandleFunc("GET /api/batch/{id}/progress", r.handleBatchProgressSSE) // P0-1 批次进度 SSE
	mux.HandleFunc("POST /api/tasks/cancel", r.handleCancelTask)            // C6 取消任务
	mux.HandleFunc("GET /api/tasks", r.handleListTasks)                     // C4/F1 任务历史
	mux.HandleFunc("GET /api/tasks/", r.handleGetTask)                      // C4 单任务详情+结果
	mux.HandleFunc("POST /api/enroll-token", r.RequireRole("agent.enroll-token", r.handleGenEnrollToken)) // C7(短1守卫)
	mux.HandleFunc("POST /api/enroll", r.handleEnroll)
	mux.HandleFunc("POST /api/renew", r.handleRenew)                      // C7 Agent 换证书
	mux.HandleFunc("POST /api/agents/revoke", r.RequireRole("agent.revoke", r.handleRevokeAgent)) // G2(短1守卫)
	mux.HandleFunc("GET /api/crons", r.handleListCrons)                     // H2 cron 列表
	mux.HandleFunc("POST /api/crons", r.handleAddCron)                      // H2 新建 cron
	mux.HandleFunc("DELETE /api/crons/", r.handleDeleteCron)                // H2 删除 cron
	mux.HandleFunc("POST /api/shell/open", r.handleShellOpen)               // H3 开终端会话
	mux.HandleFunc("POST /api/shell/input", r.handleShellInput)             // H3 发 stdin
	mux.HandleFunc("POST /api/shell/close", r.handleShellClose)             // H3 关会话
	mux.HandleFunc("POST /api/transfer/upload", r.handleTransferUpload)     // J2 上传
	mux.HandleFunc("POST /api/transfer/download", r.handleTransferDownload) // J2 下载
	mux.HandleFunc("GET /api/events", r.handleSSE)
	mux.HandleFunc("GET /api/metrics", r.handleMetrics) // C10 可观测性
	mux.HandleFunc("GET /healthz", r.handleHealthz)     // D8 深度健康检查
	// 阶段 3.1:隧道(端口转发)
	mux.HandleFunc("POST /api/tunnel/open", r.handleTunnelOpen)
	mux.HandleFunc("POST /api/tunnel/data", r.handleTunnelData)
	mux.HandleFunc("GET /api/tunnel/poll", r.handleTunnelPoll)
	mux.HandleFunc("POST /api/tunnel/close", r.handleTunnelClose)
	// 自更新:Agent 版本仓库 + 分发
	mux.HandleFunc("GET /api/updates", r.handleUpdateList)
	mux.HandleFunc("POST /api/updates/upload", r.handleUpdateUpload)
	mux.HandleFunc("POST /api/updates/apply", r.handleUpdateApply)
	// 电源操作(标签白名单鉴权)
	mux.HandleFunc("POST /api/power", r.RequireRole("power.shutdown", r.handlePower))
	// S6:Prometheus 文本格式(供 Prometheus scraper)
	// P1:有 MetricsToken 时校验(独立于主 Token),都空才真无鉴权
	mux.HandleFunc("GET /metrics", r.handlePromMetrics)
	// S6:pprof 调试端点。P1 生产硬化:仅 dev 模式或显式 -debug-pprof 才注册,
	// 默认不注册(防 heap/goroutine/内存侦察泄露)。注册到独立 mux 不污染 DefaultServeMux。
	if r.cfg.DebugPprof || r.cfg.Dev {
		mux.Handle("/debug/pprof/", http.DefaultServeMux)
		mux.Handle("/debug/pprof/cmdline", http.DefaultServeMux)
		mux.Handle("/debug/pprof/profile", http.DefaultServeMux)
		mux.Handle("/debug/pprof/symbol", http.DefaultServeMux)
		mux.Handle("/debug/pprof/trace", http.DefaultServeMux)
	}
	// XL1:Secret Management(凭据下发不落 audit)
	mux.HandleFunc("POST /api/secrets", r.RequireRole("secret.put", r.handleSecretPut))
	mux.HandleFunc("GET /api/secrets/", r.handleSecretGet)
	mux.HandleFunc("DELETE /api/secrets/", r.RequireRole("secret.delete", r.handleSecretDelete))
	// XL4:审计 SIEM 导出(CEF/JSON/Syslog)
	mux.HandleFunc("GET /api/audit/export", r.handleAuditExport)
	// XL6:智能调度(负载均衡选择最优 Agent)
	mux.HandleFunc("GET /api/scheduler/best", r.handleSelectBest)
	// D2:工具发现(供 AI Agent)
	mux.HandleFunc("GET /api/tools", r.handleListTools)
	// O2:流式任务(SSE 实时 stdout)
	mux.HandleFunc("POST /api/tasks/stream", r.handleStreamTask)
	// F8:脚本库
	mux.HandleFunc("GET /api/scripts", r.handleScriptList)
	mux.HandleFunc("POST /api/scripts", r.handleScriptUpload)
	mux.HandleFunc("GET /api/scripts/", r.handleScriptGet)
	mux.HandleFunc("DELETE /api/scripts/", r.handleScriptDelete)
	mux.HandleFunc("POST /api/scripts/", r.handleScriptRun)
	// D1:Agent 二进制下载（一行安装用）
	r.registerDownloadDir(mux)
	// B2:部署引导(一行安装命令)
	mux.HandleFunc("GET /api/bootstrap", r.handleBootstrap)
}

// GET /healthz —— 深度健康检查(D8 / P3-1)。
// 检查 store 可用性,返回各组件状态 JSON。
// P1-S7 修复:加鉴权(原无鉴权泄漏 agents 数 + store 错误信息)。
// 浅健康检查(无需鉴权)用 /healthz/simple,深度需鉴权。
func (r *Relay) handleHealthz(w http.ResponseWriter, req *http.Request) {
	if err := r.authorize(req); err != nil {
		// 无鉴权时只返回最基本的 "ok",不泄漏组件信息
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
		return
	}
	status := map[string]any{
		"relay":    "ok",
		"store":    "disabled",
		"agents":   len(r.agents.Snapshot()),
		"uptime_s": int(time.Since(metrics.StartedAt).Seconds()),
	}
	// V5:附带文件传输指标(快速判断传输健康度)
	snap := metrics.Snapshot()
	status["xfer_uploads"] = snap["xfer_uploads_total"]
	status["xfer_downloads"] = snap["xfer_downloads_total"]
	status["xfer_upload_bytes"] = snap["xfer_upload_bytes"]
	status["xfer_download_bytes"] = snap["xfer_download_bytes"]
	status["xfer_failures"] = snap["xfer_upload_failures"].(int64) + snap["xfer_download_failures"].(int64)
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
		Tag     string          `json:"tag"` // 如 "env=prod"
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
	if err := r.policy.CheckCommandPayload(body.Type, body.Payload); err != nil {
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
	// D2 优化:并行投递(当前串行 dispatchTask → 并行 goroutine)
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, aid := range agentIDs {
		taskID := "batch-" + body.Type + "-" + aid + "-" + fmt.Sprint(time.Now().UnixNano()) + "-" + randSuffix(4)
		task := proto.TaskRequest{
			TaskID:  taskID,
			Type:    body.Type,
			AgentID: aid,
			Payload: body.Payload,
			Timeout: body.Timeout,
		}
		metrics.Inc("submitted")
		if r.store != nil {
			r.store.SaveTask(StoredTask{Request: task, Status: StatusPending, CreatedAt: time.Now().UnixMilli()})
		}
		wg.Add(1)
		go func(agentID, tid string, t proto.TaskRequest) {
			defer wg.Done()
			conn := r.agents.Conn(agentID)
			if conn != nil {
				if err := r.dispatchTask(conn, t); err == nil {
					mu.Lock()
					dispatched = append(dispatched, agentID)
					taskIDs[agentID] = tid
					mu.Unlock()
				}
			}
		}(aid, taskID, task)
		if r.audit != nil {
			r.audit.Log(AuditEntry{Event: "task_submitted", AgentID: aid, TaskID: taskID, Type: body.Type, Detail: "batch:" + body.Tag})
		}
	}
	wg.Wait() // D2:等所有并行投递完成
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
	// 复用 proto.ExtractCommand 注册表(命令类摘要=命令文本;非命令类回退字节数)
	text := proto.ExtractCommand(taskType, payload)
	if text == "" {
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
	// V1:为每个 agent 附带传输画像(ub/db/uc/dc/fc,来自心跳上报)
	agentsWithStats := make([]map[string]any, 0, len(list))
	for _, a := range list {
		entry := map[string]any{
			"agent_id":     a.AgentID,
			"hostname":     a.Hostname,
			"os":           a.OS,
			"version":      a.Version,
			"capabilities": a.Capabilities,
			"tags":         a.Tags,
		}
		if stats, ok := r.agents.XferStatsOf(a.AgentID); ok {
			entry["xfer_stats"] = map[string]any{
				"upload_bytes":   stats.UploadBytes,
				"download_bytes": stats.DownloadBytes,
				"upload_count":   stats.UploadCount,
				"download_count": stats.DownloadCount,
				"failed_count":   stats.FailedCount,
			}
		}
		agentsWithStats = append(agentsWithStats, entry)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"agents": agentsWithStats,
		"count":  len(agentsWithStats),
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
	// 短1:高危任务类型 RBAC(power/update)
	if task.Type == proto.TaskPower || task.Type == proto.TaskUpdate {
		id := identityFromAuth(req)
		action := "power.shutdown"
		if task.Type == proto.TaskUpdate {
			action = "update.push"
		}
		if !id.CanPerform(action) {
			writeJSON(w, http.StatusForbidden, errMap(fmt.Errorf("角色 %s 无权下发 %s 任务", id.Role, task.Type)))
			return
		}
	}
	// H-2:离线分支——曾注册且不支持→400;从未上线或支持→缓存 202
	conn := r.agents.Conn(task.AgentID)
	caps := r.agentCapabilities(task.AgentID)
	if conn == nil {
		if len(caps) > 0 && !r.agentSupports(task.AgentID, task.Type) {
			writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("agent %s 不支持任务类型 %s (能力: %v)", task.AgentID, task.Type, caps)))
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{
			"task_id": task.TaskID, "status": StatusPending, "agent_id": task.AgentID,
			"message": "agent 离线,任务已缓存待投递",
		})
		return
	}
	if !r.agentSupports(task.AgentID, task.Type) {
		writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("agent %s 不支持任务类型 %s (能力: %v)", task.AgentID, task.Type, caps)))
		return
	}

	// C9 命令安全策略校验：拦截高危命令
	if err := r.policy.CheckCommandPayload(task.Type, task.Payload); err != nil {
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

	// S3 修复:能力感知路由——校验 Agent 是否支持该任务类型(在投递前,不管在线否)
	// 避免向不支持 browse 的 Agent 投递 browse,浪费投递配额
	if !r.agentSupports(task.AgentID, task.Type) {
		log.Printf("[task] Agent %s 不支持任务类型 %s,拒绝", task.AgentID, task.Type)
		writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("agent %s 不支持任务类型 %s (能力: %v)",
			task.AgentID, task.Type, r.agentCapabilities(task.AgentID))))
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

// dispatchTask 把任务封装为 Envelope 并转发给 Agent,更新持久化状态为 dispatched。
//
// P0-C3 修复:投递前 CAS 占坑(CASDispatchToPending),只允许 pending→dispatched。
// 防止 reaper.tryRedispatch 与 dispatchPendingTasks 并发向 Agent 发同一任务两次。
func (r *Relay) dispatchTask(conn *wsConn, task proto.TaskRequest) error {
	// E2/W1:maxAttempts 兜底,防 poison task 无限重投
	if r.store != nil {
		if existing, _ := r.store.GetTask(task.TaskID); existing != nil && existing.Attempt >= MaxTaskAttempts {
			log.Printf("[task] task=%s 达到最大投递次数 %d,转 failed", task.TaskID, MaxTaskAttempts)
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
		// P0-C3:CAS 占坑——只允许 pending→dispatched,防止并发重复投递
		// 已 dispatched/acknowledged/终态的任务跳过(返回 error,调用方可 log 后继续)
		if err := r.store.CASDispatchToPending(task.TaskID); err != nil {
			log.Printf("[task] task=%s 已被并发投递或终态,跳过: %v", task.TaskID, err)
			return err
		}
	}

	env, err := proto.NewEnvelope(proto.TypeTaskRequest, "relay", task.AgentID, task)
	if err != nil {
		return err
	}
	// 用独立 ctx(不绑请求生命周期,因 Agent 可能慢)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := conn.writeJSON(ctx, env); err != nil {
		// WS 写失败:回退到 pending 让 reaper 补投
		if r.store != nil {
			r.store.UpdateTaskStatus(task.TaskID, StatusPending)
		}
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
		st.UpdatedAt = time.Now().UnixMilli()
		_ = r.store.SaveTask(st)
		metrics.Inc("dispatched")
		r.agents.IncInFlight(task.AgentID) // H2
		log.Printf("[task] 下发 task=%s type=%s → agent=%s (attempt=%d)",
			task.TaskID, task.Type, task.AgentID, st.Attempt)
	} else {
		log.Printf("[task] 下发 task=%s type=%s → agent=%s", task.TaskID, task.Type, task.AgentID)
	}
	return nil
}

// agentSupports 检查 Agent 是否支持任务类型(S3 能力感知路由)。
func (r *Relay) agentSupports(agentID, taskType string) bool {
	return r.agents.Supports(agentID, taskType)
}

// agentCapabilities 返回 Agent 能力列表。
func (r *Relay) agentCapabilities(agentID string) []string {
	return r.agents.Capabilities(agentID)
}

// dispatchPendingTasks 在 Agent 上线时调用:找出该 Agent 的 pending 任务并投递。
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

	// P1-C4 修复:SSE 写超时改为 per-write(每次写前重置 deadline),
	// 而非绝对 deadline(旧实现连接 30s 后所有写 deadline exceeded 静默丢事件)。
	rc := http.NewResponseController(w)
	defer rc.SetWriteDeadline(time.Time{}) // 清除 deadline

	// P1 集成 SubscribeWithFilter:支持 ?events= 过滤,减少无关事件推送
	// 用法: GET /api/events?events=task_result,agent_online
	eventsParam := req.URL.Query().Get("events")
	var subID string
	var ch <-chan Event
	if eventsParam != "" {
		filter := make(map[string]bool)
		for _, e := range strings.Split(eventsParam, ",") {
			filter[strings.TrimSpace(e)] = true
		}
		subID, ch = r.console.SubscribeWithFilter(64, filter)
	} else {
		subID, ch = r.console.Subscribe(64)
	}
	defer r.console.Unsubscribe(subID)
	log.Printf("[sse] 控制台订阅 %s 建立连接 (filter=%s)", subID, eventsParam)

	// 先推一个 hello 事件,便于客户端确认连接建立
	rc.SetWriteDeadline(time.Now().Add(30 * time.Second)) // per-write
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
			// P1-C4:每次写前重置 deadline(空闲超时,而非连接绝对超时)
			rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
			if writeSSEErr(w, flusher, ev.Kind, map[string]any{
				"agent_id": ev.AgentID,
				"data":     ev.Payload,
				"ts":       ev.TS,
			}) {
				return // 写失败,退出
			}
			flusher.Flush()
		}
	}
}

// writeSSEErr 写 SSE 事件并返回是否出错(用于决定是否退出循环)。
func writeSSEErr(w http.ResponseWriter, _ http.Flusher, event string, data any) bool {
	payload, err := json.Marshal(data)
	if err != nil {
		return false // marshal 失败不算写失败
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, payload); err != nil {
		return true // 写失败(客户端断开)
	}
	return false
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

// randSuffix 生成 n 字符的十六进制随机后缀,用于 taskID 去重。
func randSuffix(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = "0123456789abcdef"[time.Now().UnixNano()>>uint((i+1)*4)&0xf]
	}
	return string(b)
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
	// P0-S2 修复:cron 规则必须经过命令策略校验(原来完全绕过)
	// cron 是持久化执行,绕过策略 = 永久后门,危害比单次任务更大
	if err := r.policy.CheckCommandPayload(rule.TaskType, rule.Payload); err != nil {
		log.Printf("[policy] 拦截高危 cron 规则 agent=%s type=%s: %v", rule.AgentID, rule.TaskType, err)
		metrics.Inc("blocked")
		if r.audit != nil {
			r.audit.Log(AuditEntry{Event: "cron_blocked", AgentID: rule.AgentID, Type: rule.TaskType, Detail: err.Error()})
		}
		writeJSON(w, http.StatusForbidden, errMap(err))
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
//
// P0-S1 修复:shell input 之前完全绕过命令策略,可执行 Format-Volume 等高危命令。
// 现在解码 base64 后过 CommandPolicy.CheckCommand,拦截高危命令。
// 局限:shell 是交互式流,用户可分多次输入绕过单行检测(如 'Forma'+'t-Volume')。
// 根本防线是 P0-S3(Agent 侧强制校验),这里做行级快速过滤。
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
	// P0-S1:解码 base64 后过命令策略校验
	if body.DataB64 != "" {
		if decoded, err := base64.StdEncoding.DecodeString(body.DataB64); err == nil {
			if err := r.policy.CheckCommand(string(decoded)); err != nil {
				log.Printf("[policy] 拦截高危 shell input agent=%s: %v", body.AgentID, err)
				metrics.Inc("blocked")
				if r.audit != nil {
					r.audit.Log(AuditEntry{Event: "shell_blocked", AgentID: body.AgentID, Detail: err.Error()})
				}
				writeJSON(w, http.StatusForbidden, errMap(err))
				return
			}
		}
	}
	conn := r.agents.Conn(body.AgentID)
	if conn == nil {
		writeJSON(w, http.StatusNotFound, errMap(fmt.Errorf("agent 不在线")))
		return
	}
	// P2-S4 修复:shell input 必须审计(原来完全无审计)
	if r.audit != nil {
		r.audit.Log(AuditEntry{Event: "shell_input", AgentID: body.AgentID, Detail: summarizeBase64(body.DataB64, 200)})
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

// summarizeBase64 解码 base64 截取前 maxLen 字符,用于审计日志。
func summarizeBase64(b64 string, maxLen int) string {
	decoded, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "<decode error>"
	}
	s := string(decoded)
	s = strings.ReplaceAll(s, "\n", "\\n")
	s = strings.ReplaceAll(s, "\r", "")
	if len(s) > maxLen {
		return s[:maxLen] + "..."
	}
	return s
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
	xferStart := time.Now() // Q4:传输耗时日志
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

	// 1. 注册 ready + complete channel（A4/A5 修复）
	readyCh := conn.registerReadyChannel(transferID)
	completeCh := conn.registerCompleteChannel(transferID)
	defer conn.unregisterXferChannels(transferID)

	// 2. 发 init
	sha256sum := req.URL.Query().Get("sha256")
	// FT 修复:大文件经 nginx 反代时,Content-Length 可能不可靠或 -1(chunked)。
	// 优先用 query 参数 size(CLI 显式传),其次用 ContentLength。
	fileSize := req.ContentLength
	if sizeStr := req.URL.Query().Get("size"); sizeStr != "" {
		if parsed, err := strconv.ParseInt(sizeStr, 10, 64); err == nil && parsed > 0 {
			fileSize = parsed
		}
	}
	init := proto.FileTransferInit{
		TransferID: transferID,
		Direction:  "upload",
		Path:       path,
		Size:       fileSize,
		SHA256:     sha256sum,
	}
	initEnv, _ := proto.NewEnvelope(proto.TypeFileTransferInit, "relay", agentID, init)
	ctx, cancel := context.WithTimeout(req.Context(), 30*time.Minute)
	defer cancel()
	if err := conn.writeJSON(ctx, initEnv); err != nil {
		writeJSON(w, http.StatusBadGateway, errMap(fmt.Errorf("发送 init 失败: %w", err)))
		return
	}

	// A4 修复:等 Agent ready(检查路径校验是否通过 + 获取续传偏移)
	// P1-P10 修复:加 case <-ctx.Done() 让客户端断开及时取消
	select {
	case ready := <-readyCh:
		if !ready.Accepted {
			writeJSON(w, http.StatusForbidden, errMap(fmt.Errorf("agent 拒绝: %s", ready.Reason)))
			return
		}
		// A6 修复:续传偏移——丢弃 HTTP body 前 N 字节
		if ready.ResumeOffset > 0 {
			if _, err := io.CopyN(io.Discard, req.Body, ready.ResumeOffset); err != nil {
				writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("续传 seek 失败: %w", err)))
				return
			}
			log.Printf("[xfer] 续传 %s: 跳过 %d 字节", transferID, ready.ResumeOffset)
		}
	case <-time.After(30 * time.Second): // S5:10s→30s,慢 Agent 初始化/已满快路径算 hash
		writeJSON(w, http.StatusGatewayTimeout, errMap(fmt.Errorf("等待 Agent ready 超时")))
		return
	case <-ctx.Done():
		writeJSON(w, http.StatusBadGateway, errMap(fmt.Errorf("客户端断开等待 ready")))
		return
	}

	// 3. stream-through:读 HTTP body → 二进制 WS 帧
	// P1-#7 修复:只有 Read 返回 io.EOF(正常结束)才发 isLast;
	// 其它错误(客户端断开/超时)不发 isLast,避免 Agent 把截断的文件当完整文件校验。
	buf := Get256K()
	defer Put256K(buf)
	totalSent := int64(0)
	aborted := false
	for {
		n, readErr := req.Body.Read(buf)
		if n > 0 {
			frame := proto.BuildBinaryFrame(transferID, false, buf[:n])
			if err := conn.writeBinaryData(ctx, frame); err != nil {
				writeJSON(w, http.StatusBadGateway, errMap(fmt.Errorf("WS写失败: %w", err)))
				return
			}
			totalSent += int64(n)
		}
		if readErr != nil {
			if readErr != io.EOF {
				// 客户端中断或读错误:不发 isLast,通知 Agent 取消
				aborted = true
				log.Printf("[xfer] 上传中断 %s: 已发 %d bytes, 错误: %v", transferID, totalSent, readErr)
				cancelEnv, _ := proto.NewEnvelope(proto.TypeFileTransferCancel, "relay", agentID,
					proto.FileTransferCancel{TransferID: transferID, Reason: "client aborted: " + readErr.Error()})
				conn.writeJSON(ctx, cancelEnv)
			}
			break
		}
	}
	if !aborted {
		// 正常结束:发 isLast 帧
		frame := proto.BuildBinaryFrame(transferID, true, nil)
		// H-5:isLast 写错检查(原 ignoring 让 Agent 悬挂→Relay 卡 completeCh 至 30 分钟)
		if wErr := conn.writeBinaryData(ctx, frame); wErr != nil {
			writeJSON(w, http.StatusBadGateway, errMap(fmt.Errorf("isLast 帧写失败: %w", wErr)))
			return
		}
	} else {
		// 中断:不等 complete,直接返回错误
		writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("上传中断: 已传 %d bytes", totalSent)))
		return
	}

	// A5 修复:等 Agent 完成校验后再返回(而非发完就 200)
	// P1-P10:加 ctx.Done() 让客户端断开及时取消
	select {
	case complete := <-completeCh:
		elapsed := time.Since(xferStart) // Q4:传输耗时
		if complete.Success {
			rate := float64(complete.Bytes) / 1024 / 1024 / elapsed.Seconds()
			log.Printf("[xfer] 上传完成 %s agent=%s: %d bytes %.2f MB/s %s sha=%s",
				transferID, agentID, complete.Bytes, rate, elapsed.Truncate(time.Millisecond), complete.SHA256Actual[:16])
			// M1:文件传输指标埋点(成功次数 + 字节)
			metrics.IncXfer("upload_success", 1)
			metrics.IncXfer("upload_bytes", complete.Bytes)
			if r.audit != nil {
				r.audit.Log(AuditEntry{Event: "file_upload", AgentID: agentID, Detail: fmt.Sprintf("%s %d bytes sha=%s", transferID, complete.Bytes, complete.SHA256Actual)})
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"transfer_id": transferID,
				"bytes":       complete.Bytes,
				"sha256":      complete.SHA256Actual,
				"status":      "uploaded",
			})
		} else {
			log.Printf("[xfer] 上传失败 %s agent=%s %s: %s", transferID, agentID, elapsed.Truncate(time.Millisecond), complete.Error)
			metrics.IncXfer("upload_fail", 1) // M1:失败计数
			writeJSON(w, http.StatusInternalServerError, errMap(fmt.Errorf("agent 校验失败: %s", complete.Error)))
		}
	case <-ctx.Done():
		writeJSON(w, http.StatusBadGateway, errMap(fmt.Errorf("客户端断开等待校验完成")))
	case <-time.After(5 * time.Minute):
		// 超时:已发送但 Agent 校验未完成
		writeJSON(w, http.StatusAccepted, map[string]any{
			"transfer_id": transferID,
			"bytes":       totalSent,
			"status":      "uploaded_pending_verification",
		})
	}
}

// handleTransferDownload 下载：Agent 读文件 → 二进制帧 → Relay → Console HTTP response。
func (r *Relay) handleTransferDownload(w http.ResponseWriter, req *http.Request) {
	xferStart := time.Now() // Q4:传输耗时
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

	// B1/D9 修复：先注册 channel 再发 init，防止 Agent 处理过快时
	// 二进制帧在 channel 就绪前到达被 routeXferData 丢弃。
	ch := conn.registerXferChannel(transferID)
	readyCh := conn.registerReadyChannel(transferID)
	defer conn.unregisterXferChannels(transferID)

	// 发 init（download 方向）
	init := proto.FileTransferInit{
		TransferID: transferID,
		Direction:  "download",
		Path:       path,
	}
	// O3:分片下载支持。offset > 0 时 Agent Seek 到指定偏移;
	// limit > 0 时只读 limit 字节(Size 字段在分片模式下复用为本次长度)。
	if v := req.URL.Query().Get("offset"); v != "" {
		if parsed, err := strconv.ParseInt(v, 10, 64); err == nil && parsed > 0 {
			init.Offset = parsed
		}
	}
	if v := req.URL.Query().Get("limit"); v != "" {
		if parsed, err := strconv.ParseInt(v, 10, 64); err == nil && parsed > 0 {
			init.Size = parsed
		}
	}
	// S4:能力协商。分片下载(offset>0)需要 Agent 支持 xfer_offset。
	// 旧 Agent 无此能力 → 显式拒绝(而非静默从头读返回错误内容,导致 CLI 死循环重试)。
	if init.Offset > 0 && !r.agents.Supports(agentID, "xfer_offset") {
		writeJSON(w, http.StatusBadGateway, errMap(fmt.Errorf("agent 不支持分片下载(xfer_offset),请升级 Agent")))
		return
	}
	initEnv, _ := proto.NewEnvelope(proto.TypeFileTransferInit, "relay", agentID, init)
	ctx, cancel := context.WithTimeout(req.Context(), 30*time.Minute)
	defer cancel()
	if err := conn.writeJSON(ctx, initEnv); err != nil {
		writeJSON(w, http.StatusBadGateway, errMap(fmt.Errorf("发送 init 失败: %w", err)))
		return
	}

	// D9 修复：等待 Agent ready（检查文件是否存在、路径策略是否通过）
	select {
	case ready := <-readyCh:
		if !ready.Accepted {
			writeJSON(w, http.StatusForbidden, errMap(fmt.Errorf("agent 拒绝: %s", ready.Reason)))
			return
		}
	case <-time.After(30 * time.Second): // S5:10s→30s
		writeJSON(w, http.StatusGatewayTimeout, errMap(fmt.Errorf("等待 Agent 就绪超时")))
		return
	}

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
			if data == nil {
				// isLast 空帧 → EOF（D9 修复：不能忽略 nil，否则永久阻塞）
				goto done
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
	elapsed := time.Since(xferStart) // Q4:下载耗时日志
	rate := float64(totalWritten) / 1024 / 1024 / elapsed.Seconds()
	if elapsed.Seconds() > 0 && totalWritten > 0 {
		log.Printf("[xfer] 下载完成 %s agent=%s: %d bytes %.2f MB/s %s",
			transferID, agentID, totalWritten, rate, elapsed.Truncate(time.Millisecond))
	}
	// M1:下载指标埋点。totalWritten>0 算成功(可能部分),=0 且 ctx 超时算失败
	if totalWritten > 0 {
		metrics.IncXfer("download_success", 1)
		metrics.IncXfer("download_bytes", totalWritten)
	} else {
		metrics.IncXfer("download_fail", 1)
	}
	if r.audit != nil {
		r.audit.Log(AuditEntry{Event: "file_download", AgentID: agentID, Detail: fmt.Sprintf("%s %d bytes", transferID, totalWritten)})
	}
}

// POST /api/renew —— Agent 用现有 mTLS 客户端证书续签(W7:不依赖一次性 token)。
func (r *Relay) handleRenew(w http.ResponseWriter, req *http.Request) {
	if r.ca == nil {
		writeJSON(w, http.StatusServiceUnavailable, errMap(fmt.Errorf("CA 未启用")))
		return
	}
	if req.TLS == nil || len(req.TLS.PeerCertificates) == 0 {
		writeJSON(w, http.StatusUnauthorized, errMap(fmt.Errorf("renew 需要 mTLS 客户端证书")))
		return
	}
	leaf := req.TLS.PeerCertificates[0]
	agentID := leaf.Subject.CommonName
	if agentID == "" {
		writeJSON(w, http.StatusUnauthorized, errMap(fmt.Errorf("客户端证书无 CN")))
		return
	}
	if r.ca.IsRevoked(leaf.SerialNumber) {
		writeJSON(w, http.StatusForbidden, errMap(fmt.Errorf("证书已撤销")))
		return
	}
	var body struct {
		CSRPEM string `json:"csr_pem"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil || body.CSRPEM == "" {
		writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("csr_pem 不能为空")))
		return
	}
	const certTTL = 24 * time.Hour
	certPEM, err := r.ca.SignAgentCSRNoRevoke([]byte(body.CSRPEM), agentID, certTTL)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errMap(err))
		return
	}
	log.Printf("[ca] 续签证书 agent=%s (renew,不撤销旧证)", agentID)
	writeJSON(w, http.StatusOK, proto.EnrollResp{
		AgentID: agentID, CertPEM: string(certPEM), CAPEM: string(r.ca.CAPEM()),
		ExpiresAt: time.Now().Add(certTTL).Unix(), Success: true,
	})
}
