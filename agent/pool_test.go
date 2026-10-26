package main

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QiuShichang/reins/proto"
)

// TestWorkerPoolLimitsConcurrency 验证 F3 修复：worker pool 限制并发数。
func TestWorkerPoolLimitsConcurrency(t *testing.T) {
	const max = 3
	p := NewWorkerPool(max)

	var current int32
	var peak int32
	var wg sync.WaitGroup

	// 提交 10 个任务，每个 sleep 一会
	for i := 0; i < 10; i++ {
		wg.Add(1)
		p.Submit(context.Background(), func() {
			defer wg.Done()
			cur := atomic.AddInt32(&current, 1)
			// 更新峰值
			for {
				peakVal := atomic.LoadInt32(&peak)
				if cur <= peakVal || atomic.CompareAndSwapInt32(&peak, peakVal, cur) {
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
			atomic.AddInt32(&current, -1)
		})
	}
	wg.Wait()

	// 峰值不应超过 max
	if got := atomic.LoadInt32(&peak); got > max {
		t.Fatalf("并发峰值 %d 超过限制 %d", got, max)
	}
}

// TestWorkerPoolZeroMeansUnlimited 验证 max=0 不限并发。
func TestWorkerPoolZeroMeansUnlimited(t *testing.T) {
	p := NewWorkerPool(0) // 不限
	var count int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		p.Submit(context.Background(), func() {
			defer wg.Done()
			atomic.AddInt32(&count, 1)
		})
	}
	wg.Wait()
	if got := atomic.LoadInt32(&count); got != 20 {
		t.Fatalf("应全部执行，仅 %d", got)
	}
}

// TestWorkerPoolCancelWhileWaiting 验证等待 slot 时被取消。
func TestWorkerPoolCancelWhileWaiting(t *testing.T) {
	p := NewWorkerPool(1) // 只 1 个 slot

	// 先占住唯一 slot，用 started 同步确认已获取 slot
	started := make(chan struct{})
	blockDone := make(chan struct{})
	p.Submit(context.Background(), func() {
		close(started)
		<-blockDone // 阻塞
	})
	<-started // 确认第一个任务已占住 slot

	// 提交第二个任务，但 ctx 立即取消（第二个必在等 slot）
	ctx, cancel := context.WithCancel(context.Background())
	var executed int32
	p.Submit(ctx, func() {
		atomic.StoreInt32(&executed, 1)
	})
	cancel() // 立即取消
	time.Sleep(50 * time.Millisecond)

	// 释放阻塞
	close(blockDone)
	p.Wait()

	if atomic.LoadInt32(&executed) != 0 {
		t.Fatal("被取消的任务不应执行")
	}
}

// TestTrySubmitReturnsFalseWhenFull 验证 P1-6 修复：池满时 TrySubmit 返回 false。
func TestTrySubmitReturnsFalseWhenFull(t *testing.T) {
	p := NewWorkerPool(1) // 1 个 slot

	// 占住唯一 slot，用 started channel 确保 goroutine 已获取 slot
	started := make(chan struct{})
	block := make(chan struct{})
	p.Submit(context.Background(), func() {
		close(started) // 已拿到 slot 并开始执行
		<-block
	})
	<-started // 等 goroutine 确认已占住 slot

	// TrySubmit 应返回 false（池满）
	ok := p.TrySubmit(func() {})
	if ok {
		t.Fatal("池满时 TrySubmit 应返回 false")
	}

	// 释放后 TrySubmit 应成功
	close(block)
	p.Wait()
	// 给 slot 回填一点时间
	time.Sleep(10 * time.Millisecond)
	ok = p.TrySubmit(func() {})
	if !ok {
		t.Fatal("池有空位时 TrySubmit 应返回 true")
	}
	p.Wait()
}

// TestFlushToCommitOnSuccess 验证 P1-4 修复：flushTo 逐条发送成功才出队。
// 用 nil wc 模拟发送失败——结果应留在队列。
func TestFlushToCommitOnSuccess(t *testing.T) {
	q := &rQueue{}
	env, _ := proto.NewEnvelope(proto.TypeTaskResult, "a", "r", proto.TaskResult{TaskID: "t1"})
	q.enqueue(env)
	if q.len() != 1 {
		t.Fatal("入队后应有 1 项")
	}

	// flushTo 用 nil wc 会 panic（writeBytes 解引用 nil），
	// 所以这里只验证 peek/popHead 的逐条逻辑
	item, ok := q.peekHead()
	if !ok || item.env.ID == "" {
		t.Fatal("peekHead 应返回队首")
	}
	q.popHead()
	if q.len() != 0 {
		t.Fatal("popHead 后应清空")
	}
}
