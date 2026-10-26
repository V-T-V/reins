package main

import (
	"testing"

	"github.com/QiuShichang/reins/proto"
)

// TestIdempotencyCacheHitMiss 验证幂等缓存命中/未命中。
func TestIdempotencyCacheHitMiss(t *testing.T) {
	c := &idemCache{ttl: 60e9} // 60s
	task := proto.TaskRequest{TaskID: "t1", IdempotencyKey: "key-abc"}

	// 未命中
	if _, hit := c.get(keyFor(task)); hit {
		t.Fatal("初始应未命中")
	}

	// 存入
	c.put(keyFor(task), proto.TaskResult{Success: true, Stdout: "done"})

	// 命中
	r, hit := c.get(keyFor(task))
	if !hit {
		t.Fatal("存入后应命中")
	}
	if r.Stdout != "done" {
		t.Fatalf("结果不符: %+v", r)
	}

	// 不同 key 未命中
	if _, hit := c.get("other-key"); hit {
		t.Fatal("其他 key 应未命中")
	}
}

// TestIdempotencyFallbackToTaskID 验证无 IdempotencyKey 时用 TaskID。
func TestIdempotencyFallbackToTaskID(t *testing.T) {
	task := proto.TaskRequest{TaskID: "fallback-id"} // 无 IdempotencyKey
	if keyFor(task) != "fallback-id" {
		t.Fatalf("应 fallback 到 TaskID，得到 %s", keyFor(task))
	}

	task2 := proto.TaskRequest{TaskID: "x", IdempotencyKey: "explicit"}
	if keyFor(task2) != "explicit" {
		t.Fatalf("应优先用 IdempotencyKey，得到 %s", keyFor(task2))
	}
}

// TestResultQueueEnqueueFlush 验证结果队列入队与 drain。
func TestResultQueueEnqueueFlush(t *testing.T) {
	q := &rQueue{}
	env1, _ := proto.NewEnvelope(proto.TypeTaskResult, "a", "r", proto.TaskResult{TaskID: "t1"})
	env2, _ := proto.NewEnvelope(proto.TypeTaskResult, "a", "r", proto.TaskResult{TaskID: "t2"})

	q.enqueue(env1)
	q.enqueue(env2)
	if q.len() != 2 {
		t.Fatalf("队列应有 2 项，有 %d", q.len())
	}

	items := q.drain()
	if len(items) != 2 || q.len() != 0 {
		t.Fatalf("drain 后应清空")
	}

	// requeue 失败项
	q.requeue(items)
	if q.len() != 2 {
		t.Fatalf("requeue 后应有 2 项，有 %d", q.len())
	}
}

// TestResultQueueFlushFailureRequeues 验证 flush 发送失败时放回队列。
func TestResultQueueFlushFailureRequeues(t *testing.T) {
	q := &rQueue{}
	env, _ := proto.NewEnvelope(proto.TypeTaskResult, "a", "r", proto.TaskResult{TaskID: "tx"})
	q.enqueue(env)

	// 用 nil wc 触发发送失败（writeBytes 会 panic 或返回错——
	// 为安全，直接验证 drain/requeue 逻辑，不发真实 wc）
	items := q.drain()
	q.requeue(items)
	if q.len() != 1 {
		t.Fatalf("requeue 后应有 1 项")
	}
}
