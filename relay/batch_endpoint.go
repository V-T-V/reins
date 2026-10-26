// Package main — 批量分发端点化(P0-1)。
//
// round 8 的 BatchDispatcher 引擎(一对多/聚合/指数退避重试/实时进度)此前
// 只有测试在用;现有 POST /api/tasks/batch 是简单并行投递(无重试/无进度)。
// 本文件把引擎暴露为异步 REST 端点:
//
//	POST /api/batch                → 立即返回 batch_id(异步执行)
//	GET  /api/batch/{id}           → 进度快照(轮询用)
//	GET  /api/batch/{id}/progress  → SSE 实时进度,终态后发 done 事件收尾
//
// 批次注册表带 TTL 懒清理(完成 1h 后可回收),防无限增长。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/V-T-V/reins/proto"
)

// batchTTL 已完成批次的注册表保留时长。
const batchTTL = time.Hour

// trackedBatch 注册表中一个批次的条目。
type trackedBatch struct {
	progress  *BatchProgress
	result    *BatchResult // 分发结束后填充(投递层聚合;任务执行结果经 task result 查询)
	done      chan struct{}
	createdAt time.Time
}

// handleBatchStart POST /api/batch —— 异步启动带重试与进度跟踪的批量分发。
//
// 请求体: {"tag":"env=prod","type":"powershell","payload":{...},"timeout":30,"max_retries":2}
// 响应 202: {"batch_id":"b-...","total":N}
func (r *Relay) handleBatchStart(w http.ResponseWriter, req *http.Request) {
	if err := r.authorize(req); err != nil {
		writeJSON(w, http.StatusUnauthorized, errMap(err))
		return
	}
	var body struct {
		Tag        string          `json:"tag"`
		Type       string          `json:"type"`
		Payload    json.RawMessage `json:"payload"`
		Timeout    int             `json:"timeout"`
		MaxRetries int             `json:"max_retries"`
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
	// 策略校验(与 handleBatchTask 同款,批量是策略绕过的高危面)
	if err := r.policy.CheckCommandPayload(body.Type, body.Payload); err != nil {
		log.Printf("[policy] 异步批次被拦截 tag=%s type=%s: %v", body.Tag, body.Type, err)
		metrics.Inc("blocked")
		if r.audit != nil {
			r.audit.Log(AuditEntry{Event: "task_blocked", Type: body.Type, Detail: "batch-async:" + body.Tag + " " + err.Error()})
		}
		writeJSON(w, http.StatusForbidden, errMap(err))
		return
	}

	agentIDs := r.agents.FindByTag(body.Tag)
	if len(agentIDs) == 0 {
		writeJSON(w, http.StatusNotFound, errMap(fmt.Errorf("无带标签 %s 的在线 Agent", body.Tag)))
		return
	}

	batchID := fmt.Sprintf("b-%d-%s", time.Now().UnixNano(), randSuffix(4))
	progress := newBatchProgress(batchID, len(agentIDs))
	tb := &trackedBatch{progress: progress, done: make(chan struct{}), createdAt: time.Now()}
	r.batches.Store(batchID, tb)
	r.gcBatches()

	// 异步执行:引擎主体接受外部 progress,查询端点从启动起即可拿到进度。
	go func() {
		d := newBatchDispatcher(r.dispatchForBatch)
		br := d.dispatchWithRetryTracked(context.Background(), BatchRequest{
			BatchID:    batchID,
			AgentIDs:   agentIDs,
			Type:       body.Type,
			Payload:    body.Payload,
			Timeout:    body.Timeout,
			MaxRetries: body.MaxRetries,
		}, progress)
		tb.result = &br
		close(tb.done)
		log.Printf("[batch] 批次 %s 结束: %d 目标,成功率 %.0f%%", batchID, len(br.Targets), br.SuccessRate()*100)
	}()

	if r.audit != nil {
		r.audit.Log(AuditEntry{Event: "task_submitted", Type: body.Type, Detail: fmt.Sprintf("batch-async:%s targets=%d retries=%d", body.Tag, len(agentIDs), body.MaxRetries)})
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"batch_id":    batchID,
		"total":       len(agentIDs),
		"progress":    "/api/batch/" + batchID + "/progress",
		"max_retries": body.MaxRetries,
	})
}

// dispatchForBatch 单目标投递函数(供 batchDispatcher 引擎):持久化 + WS 投递。
func (r *Relay) dispatchForBatch(ctx context.Context, agentID string, task proto.TaskRequest) (string, error) {
	_ = ctx
	now := time.Now().UnixMilli()
	if r.store != nil {
		r.store.SaveTask(StoredTask{Request: task, Status: StatusPending, CreatedAt: now, UpdatedAt: now})
	}
	conn := r.agents.Conn(agentID)
	if conn == nil {
		return "", fmt.Errorf("agent %s 离线", agentID)
	}
	metrics.Inc("submitted")
	if err := r.dispatchTask(conn, task); err != nil {
		return "", err
	}
	return task.TaskID, nil
}

// handleBatchStatus GET /api/batch/{id} —— 进度快照(轮询用)。
func (r *Relay) handleBatchStatus(w http.ResponseWriter, req *http.Request) {
	if err := r.authorize(req); err != nil {
		writeJSON(w, http.StatusUnauthorized, errMap(err))
		return
	}
	tb, ok := r.lookupBatch(req.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, errMap(fmt.Errorf("批次不存在或已过期")))
		return
	}
	writeJSON(w, http.StatusOK, batchStatusView(tb))
}

// handleBatchProgressSSE GET /api/batch/{id}/progress —— SSE 实时进度。
// 每秒推送一帧 snapshot;终态后推送 done 事件(带聚合结果)并关闭流。
func (r *Relay) handleBatchProgressSSE(w http.ResponseWriter, req *http.Request) {
	if err := r.authorize(req); err != nil {
		writeJSON(w, http.StatusUnauthorized, errMap(err))
		return
	}
	tb, ok := r.lookupBatch(req.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, errMap(fmt.Errorf("批次不存在或已过期")))
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, errMap(fmt.Errorf("streaming 不支持")))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	writeSSE(w, flusher, "progress", batchStatusView(tb))
	flusher.Flush()

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-req.Context().Done():
			return
		case <-tb.done:
			writeSSE(w, flusher, "done", batchStatusView(tb))
			flusher.Flush()
			return
		case <-ticker.C:
			writeSSE(w, flusher, "progress", batchStatusView(tb))
			flusher.Flush()
		}
	}
}

// lookupBatch 查注册表。
func (r *Relay) lookupBatch(batchID string) (*trackedBatch, bool) {
	v, ok := r.batches.Load(batchID)
	if !ok {
		return nil, false
	}
	return v.(*trackedBatch), true
}

// gcBatches 懒清理过期批次(完成超过 batchTTL)。
func (r *Relay) gcBatches() {
	now := time.Now()
	r.batches.Range(func(k, v any) bool {
		tb := v.(*trackedBatch)
		select {
		case <-tb.done:
			if now.Sub(tb.createdAt) > batchTTL {
				r.batches.Delete(k)
			}
		default:
		}
		return true
	})
}

// batchStatusView 批次的对外视图(快照 + 终态聚合)。
func batchStatusView(tb *trackedBatch) map[string]any {
	snap := tb.progress.Snapshot()
	out := map[string]any{
		"batch_id":   snap.BatchID,
		"total":      snap.Total,
		"dispatched": snap.Dispatched,
		"succeeded":  snap.Succeeded,
		"failed":     snap.Failed,
		"pending":    snap.Pending,
		"done":       snap.Done,
		"percent":    snap.Percent,
		"elapsed_ms": snap.Elapsed,
		"eta_ms":     snap.ETA,
		"states":     snap.States,
	}
	select {
	case <-tb.done:
		out["status"] = "finished"
		if tb.result != nil {
			out["success_rate"] = tb.result.SuccessRate()
		}
	default:
		out["status"] = "running"
	}
	return out
}
