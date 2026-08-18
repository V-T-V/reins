// Package main — 字节级令牌桶(Agent 发送限速,P2-12)。
//
// 场景:并发下载会把 Agent 上行带宽打满,心跳 WS 挤死(无包超时即被判死
// 重连,任务/隧道随机拒绝)。发送侧限速后心跳始终有带宽可用。
// rate=0 时 Wait 立即返回(nil limiter 同),行为与不限速完全一致。
package main

import (
	"context"
	"sync"
	"time"
)

// byteRateLimiter 字节级令牌桶:令牌随时间线性恢复,不足则睡眠等待。
type byteRateLimiter struct {
	mu     sync.Mutex
	rate   float64 // bytes/s
	burst  float64 // 桶容量(允许的瞬时突发)
	tokens float64
	last   time.Time
}

// newByteRateLimiter bytesPerSec<=0 返回 nil(不限速)。
func newByteRateLimiter(bytesPerSec float64) *byteRateLimiter {
	if bytesPerSec <= 0 {
		return nil
	}
	return &byteRateLimiter{
		rate:   bytesPerSec,
		burst:  bytesPerSec, // 突发额度 = 1 秒满速率
		tokens: bytesPerSec,
		last:   time.Now(),
	}
}

// Wait 等待 n 字节的发送额度。ctx 取消时立即返回(放弃节流,发送方自行
// 决定是否继续——通常随后写 WS 也会因 ctx 失败)。
func (l *byteRateLimiter) Wait(ctx context.Context, n int) {
	if l == nil || l.rate <= 0 || n <= 0 {
		return
	}
	need := float64(n)
	for {
		l.mu.Lock()
		now := time.Now()
		l.tokens += now.Sub(l.last).Seconds() * l.rate
		if l.tokens > l.burst {
			l.tokens = l.burst
		}
		l.last = now
		if l.tokens >= need {
			l.tokens -= need
			l.mu.Unlock()
			return
		}
		deficit := need - l.tokens
		l.mu.Unlock()

		wait := time.Duration(deficit / l.rate * float64(time.Second))
		if wait < time.Millisecond {
			wait = time.Millisecond
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}
