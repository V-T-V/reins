package main

import (
	"log"
	"sync"
	"time"

	"github.com/QiuShichang/go-rmm/proto"
)

// resultQueue 缓存待发送的任务结果。
//
// 修复 R3(P1)：任务结果回流失败时，原实现仅记日志，结果永久丢失。
// 现在结果先进队列，发送成功才出队；连接断开重连后补发队列中的结果。
//
// 实现为内存队列（Agent 单进程，崩溃即重启，配合 Relay 的重投机制兜底）。
// 不落盘的原因：Agent 侧结果丢失的代价 < Relay 侧重投的开销，
// 且 Agent 重启后 Relay 会因心跳超时标记离线，pending 任务由 Relay 补投。
var resultQueue = &rQueue{}

type rQueue struct {
	mu      sync.Mutex
	results []pendingResult
}

type pendingResult struct {
	env     proto.Envelope
	addedAt time.Time
	attempt int
}

// enqueue 把结果入队。
func (q *rQueue) enqueue(env proto.Envelope) {
	q.mu.Lock()
	q.results = append(q.results, pendingResult{env: env, addedAt: time.Now()})
	q.mu.Unlock()
}

// drain 取出全部待发送结果（发送成功后调用）。
func (q *rQueue) drain() []pendingResult {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := q.results
	q.results = nil
	return out
}

// peek 查看但不出队。
func (q *rQueue) peek() []pendingResult {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]pendingResult, len(q.results))
	copy(out, q.results)
	return out
}

// requeue 把发送失败的结果放回（带递增 attempt）。
func (q *rQueue) requeue(items []pendingResult) {
	q.mu.Lock()
	for _, it := range items {
		it.attempt++
		q.results = append(q.results, it)
	}
	q.mu.Unlock()
}

// len 返回待发送结果数。
func (q *rQueue) len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.results)
}

// flushTo 把队列中所有结果尝试发送到 wc，发送失败的放回队列。
func (q *rQueue) flushTo(wc *wsConn) {
	items := q.drain()
	if len(items) == 0 {
		return
	}
	var failed []pendingResult
	for _, it := range items {
		ctx, cancel := contextWithTimeout(10 * time.Second)
		data, _ := marshalEnvelopeBytes(it.env)
		err := wc.writeBytes(ctx, data)
		cancel()
		if err != nil {
			if it.attempt < 3 {
				failed = append(failed, it)
			} else {
				log.Printf("[queue] 结果 %s 放弃发送(超3次)", it.env.ID)
			}
		}
	}
	if len(failed) > 0 {
		q.requeue(failed)
	}
}
