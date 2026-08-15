package main

import (
	"context"
	"log"
	"runtime/debug"
	"sync"
	"time"

	"github.com/QiuShichang/go-rmm/proto"
)

// WorkerPool 限制 Agent 同时执行的任务数，防止被并发任务打爆。
//
// 修复 F3(P1)：原实现每个任务一个 goroutine，无上限。
// console 连发 N 个 PowerShell 任务，Agent 会同时起 N 个 powershell 进程，
// 可能耗尽 CPU/内存/句柄。
//
// 设计：带缓冲 channel 作为信号量，Acquire 阻塞直到有空位。
type WorkerPool struct {
	slots chan struct{} // 信号量
	wg    sync.WaitGroup
}

// NewWorkerPool 构造池，max 为最大并发任务数。max<=0 表示不限（直接 go）。
func NewWorkerPool(max int) *WorkerPool {
	p := &WorkerPool{}
	if max > 0 {
		p.slots = make(chan struct{}, max)
		for i := 0; i < max; i++ {
			p.slots <- struct{}{}
		}
	}
	return p
}

// Submit 提交任务执行。如果池满则阻塞等待空位。
// job 在获取到 slot 后于独立 goroutine 执行。
func (p *WorkerPool) Submit(ctx context.Context, job func()) {
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		// 获取 slot（无限模式 p.slots 为 nil，直接跳过）
		if p.slots != nil {
			select {
			case <-p.slots:
				defer func() { p.slots <- struct{}{} }()
			case <-ctx.Done():
				log.Printf("[pool] 任务等待 slot 时被取消")
				return
			}
		}
		// W3:任务 panic 隔离,防止单任务 panic 崩溃整个 Agent
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[pool] 任务 panic(已恢复,防崩溃): %v\n%s", r, debug.Stack())
			}
		}()
		job()
	}()
}

// TrySubmit 非阻塞提交。池满立即返回 false（调用方可据此发 NACK）。
// 成功获取 slot 返回 true，job 在独立 goroutine 执行。
func (p *WorkerPool) TrySubmit(job func()) bool {
	if p.slots == nil {
		// 无限模式，直接提交
		p.wg.Add(1)
		go func() { defer p.wg.Done(); job() }()
		return true
	}
	select {
	case <-p.slots:
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			defer func() { p.slots <- struct{}{} }()
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[pool] 任务 panic(已恢复,防崩溃): %v\n%s", r, debug.Stack())
				}
			}()
			job()
		}()
		return true
	default:
		return false // 池满
	}
}

// Wait 等待所有已提交任务完成。
func (p *WorkerPool) Wait() { p.wg.Wait() }

// handleTaskWithPool 用 worker pool 包装任务执行(并发限流)。
//
// P2-D 修复:TrySubmit(池满立即 NACK → Relay 回退 pending → 重投)改为
// 阻塞排队(Submit)。会话实测:master 串行处理 3-4 分钟长任务时,
// NACK 风暴让后续任务反复回退重投,CLI 表现为"任务卡住/超时"。
// 排队语义:
//  1. 立即发 ACK → Relay 侧保持 acknowledged(超时阈值 timeout×3,≥300s),
//     排队期间不被 reaper 误杀(dispatched 超时仅 timeout×2,≥120s)
//  2. 池满时阻塞等待空位,任务按到达顺序排队执行
//  3. 排队超过任务 timeout(极端高并发)→ 不执行,Relay ack 超时后回退重投(自愈)
func (a *Agent) handleTaskWithPool(ctx context.Context, wc *wsConn, env proto.Envelope, pool *WorkerPool) {
	var task proto.TaskRequest
	if err := env.Decode(&task); err != nil {
		log.Printf("[agent] 任务解析失败: %v", err)
		return
	}
	// P2-D:排队前先 ACK(Relay 侧 acknowledged,防 reaper 误杀)。
	ack := proto.TaskAck{TaskID: task.TaskID, Status: proto.StatusAcknowledged}
	ackCtx, ackCancel := contextWithTimeout(5 * time.Second)
	_ = wc.writeEnvelope(ackCtx, proto.TypeTaskAck, "relay", ack)
	ackCancel()
	// 阻塞排队执行(池满等待空位)。排队上限 = 任务 timeout。
	// A3:Timeout==0 时兜底 DefaultTaskTimeout,防无限排队 → reaper 180s 回退重投 → 重复执行。
	timeout := task.Timeout
	if timeout <= 0 {
		timeout = proto.DefaultTaskTimeout
	}
	qctx, qcancel := contextWithTimeout(time.Duration(timeout) * time.Second)
	defer qcancel()
	pool.Submit(qctx, func() { a.runTask(ctx, wc, env) })
}
