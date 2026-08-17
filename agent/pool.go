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
		// W3:任务 panic 隔离
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[pool] 任务 panic(已恢复): %v\n%s", r, debug.Stack())
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
			job()
		}()
		return true
	default:
		return false // 池满
	}
}

// Wait 等待所有已提交任务完成。
func (p *WorkerPool) Wait() { p.wg.Wait() }

// handleTaskWithPool 用 worker pool 包装任务执行（并发限流）。
//
// P1-6 修复：用 TrySubmit（非阻塞）。池满时不再静默丢弃，而是发 task_nack
// 让 Relay 把任务回退 pending，等有空位时重新投递。
func (a *Agent) handleTaskWithPool(ctx context.Context, wc *wsConn, env proto.Envelope, pool *WorkerPool) {
	var task proto.TaskRequest
	if err := env.Decode(&task); err != nil {
		log.Printf("[agent] 任务解析失败: %v", err)
		return
	}
	if !pool.TrySubmit(func() { a.runTask(ctx, wc, env) }) {
		// 池满：发 NACK，Relay 收到后回退 pending
		log.Printf("[agent] 任务 %s 被拒（worker pool 满），发 NACK", task.TaskID)
		nack := proto.TaskNack{TaskID: task.TaskID, Reason: "pool_full"}
		nctx, ncancel := contextWithTimeout(5 * time.Second)
		defer ncancel()
		_ = wc.writeEnvelope(nctx, proto.TypeTaskNack, "relay", nack)
	}
}
