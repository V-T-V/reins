package main

import (
	"context"
	"testing"
	"time"
)

// TestByteRateLimiterUnlimited nil/零速率立即返回(不限速语义)。
func TestByteRateLimiterUnlimited(t *testing.T) {
	var l *byteRateLimiter // nil = 不限
	start := time.Now()
	l.Wait(context.Background(), 1<<20)
	if time.Since(start) > 10*time.Millisecond {
		t.Fatalf("nil limiter 应立即返回,耗时 %v", time.Since(start))
	}
	l2 := newByteRateLimiter(0)
	l2.Wait(context.Background(), 1<<20) // 速率 0 同样立即
}

// TestByteRateLimiterThrottles 限速语义:突发额度(1s 满速)耗尽后,
// 后续等待耗时符合速率约束。
func TestByteRateLimiterThrottles(t *testing.T) {
	// 1 MB/s,桶容量 1MB:首次 512KB 立即(突发),第二次 512KB 立即(桶刚好),
	// 第三次 256KB 需等 ~256ms。
	l := newByteRateLimiter(1 << 20)
	ctx := context.Background()

	start := time.Now()
	l.Wait(ctx, 512<<10)
	l.Wait(ctx, 512<<10)
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("突发额度内不应等待,耗时 %v", elapsed)
	}

	start = time.Now()
	l.Wait(ctx, 256<<10)
	elapsed := time.Since(start)
	// 理论 256ms,容差下界 200ms(调度抖动),上界 2s(防慢机器 flaky)
	if elapsed < 200*time.Millisecond {
		t.Fatalf("超出突发额度后应按速率等待(理论 256ms),实际 %v", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("等待过久(实现缺陷?),实际 %v", elapsed)
	}
}

// TestByteRateLimiterContextCancel ctx 取消时立即返回。
func TestByteRateLimiterContextCancel(t *testing.T) {
	l := newByteRateLimiter(1) // 1 byte/s:任何等待都极长
	l.Wait(context.Background(), 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	l.Wait(ctx, 1<<10)
	if time.Since(start) > 50*time.Millisecond {
		t.Fatalf("ctx 已取消应立即返回,耗时 %v", time.Since(start))
	}
}

// TestSetSendRateLimit 引擎注入开关:0 关闭(nil limiter),正值启用。
func TestSetSendRateLimit(t *testing.T) {
	te := &TransferEngine{}
	te.SetSendRateLimit(0)
	te.mu.Lock()
	off := te.sendLimiter
	te.mu.Unlock()
	if off != nil {
		t.Fatal("速率 0 应关闭限速(nil limiter)")
	}
	te.SetSendRateLimit(10 << 20)
	te.mu.Lock()
	on := te.sendLimiter
	te.mu.Unlock()
	if on == nil || on.rate != 10<<20 {
		t.Fatalf("应启用 10MB/s 限速: %+v", on)
	}
}
