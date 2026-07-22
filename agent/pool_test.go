package main

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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

	// 先占住唯一 slot
	blockDone := make(chan struct{})
	p.Submit(context.Background(), func() {
		<-blockDone // 阻塞
	})

	// 提交第二个任务，但 ctx 立即取消
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
