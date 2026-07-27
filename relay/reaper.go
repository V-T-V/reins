package main

import (
	"context"
	"log"
	"time"

	"github.com/QiuShichang/go-rmm/proto"
)

// reaperLoop 周期扫描在途任务,回退超时项。
//
// 修复 P1-2(致命):dispatchTask 写 WS 失败或 Agent 离线后,任务永远卡在 dispatched,
// at-least-once 语义失效。reaper 周期把"超时未完成"的任务回退 pending,
// 下次 Agent 上线时 dispatchPendingTasks 自动补投。
//
// 超时阈值计算:
//   - dispatched 状态:任务 timeout × 2(留足 ACK + 执行时间),最小 120s
//   - acknowledged 状态:任务 timeout × 3(Agent 已在执行,给更多时间),最小 300s
//
// P2 修复:支持优雅退出(响应 shutdown ctx)。
func reaperLoop(ctx context.Context, r *Relay) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.reapStaleTasks()
		}
	}
}

// reapStaleTasks 扫描并回退超时的在途任务。
//
// P0-C2 修复:用 ReapToPending(原子+终态校验)替代裸 SaveTask,
// 防止"reaper 快照含 dispatched X → Agent 补发结果转 completed → reaper 覆盖回 pending → 重复执行"。
// 现在终态任务拒绝回退,ReapToPending 返回 error 时 skip。
func (r *Relay) reapStaleTasks() {
	if r.store == nil {
		return
	}
	now := time.Now().UnixMilli()

	// 扫描 dispatched:超时阈值 = timeout×2(最小 120s)
	dispatched, err := r.store.ListTasksByStatus(StatusDispatched)
	if err != nil {
		log.Printf("[reaper] 查询 dispatched 失败: %v", err)
		return
	}
	reapedDispatched := 0
	for _, t := range dispatched {
		threshold := taskTimeoutSeconds(t.Request) * 2 * 1000 // 毫秒
		if threshold < 120*1000 {
			threshold = 120 * 1000
		}
		staleSecs := (now - t.UpdatedAt) / 1000 // P1-C6 修复:先算 stale 再改 UpdatedAt
		if now-t.UpdatedAt > int64(threshold) {
			// P0-C2:用 ReapToPending(原子+终态校验),不用裸 SaveTask
			if err := r.store.ReapToPending(t.Request.TaskID); err != nil {
				continue // 已是终态或并发改动,skip(不重复执行)
			}
			reapedDispatched++
			metrics.Inc("reaped")
			log.Printf("[reaper] 回退 dispatched→pending task=%s (超时 %ds)", t.Request.TaskID, staleSecs)
			r.tryRedispatch(t)
		}
	}

	// 扫描 acknowledged:超时阈值 = timeout×3(最小 300s)
	// acknowledged 是 Agent 已收到正在执行,只处理明显超时的(如 Agent 崩溃)
	acknowledged, err := r.store.ListTasksByStatus(StatusAcknowledged)
	if err != nil {
		return
	}
	reapedAck := 0
	for _, t := range acknowledged {
		threshold := taskTimeoutSeconds(t.Request) * 3 * 1000
		if threshold < 300*1000 {
			threshold = 300 * 1000
		}
		staleSecs := (now - t.UpdatedAt) / 1000
		if now-t.UpdatedAt > int64(threshold) {
			if err := r.store.ReapToPending(t.Request.TaskID); err != nil {
				continue // P0-C2:终态拒绝回退
			}
			reapedAck++
			metrics.Inc("reaped")
			log.Printf("[reaper] 回退 acknowledged→pending task=%s (超时 %ds,Agent 可能崩溃)", t.Request.TaskID, staleSecs)
			r.tryRedispatch(t)
		}
	}

	if reapedDispatched+reapedAck > 0 {
		log.Printf("[reaper] 本轮回退 %d dispatched + %d acknowledged", reapedDispatched, reapedAck)
	}
}

// tryRedispatch 尝试立即补投(若目标 Agent 在线)。
func (r *Relay) tryRedispatch(t StoredTask) {
	conn := r.agents.Conn(t.Request.AgentID)
	if conn == nil {
		return // Agent 离线，等上线后 dispatchPendingTasks 补投
	}
	if err := r.dispatchTask(conn, t.Request); err != nil {
		log.Printf("[reaper] 立即补投 task=%s 失败(等 Agent 重连): %v", t.Request.TaskID, err)
	}
}

// taskTimeoutSeconds 取任务的 timeout 秒数（默认 60）。
func taskTimeoutSeconds(req proto.TaskRequest) int {
	if req.Timeout > 0 {
		return req.Timeout
	}
	return proto.DefaultTaskTimeout
}
