package main

import (
	"sync"
	"time"

	"github.com/QiuShichang/go-rmm/proto"
)

// idempotencyCache 缓存已执行任务的幂等键与结果，防 Relay 重投导致重复执行。
//
// 工作机制：
//   - 任务到达时，查 cache；命中则直接回放缓存结果（不重复执行）。
//   - 未命中则执行，执行后存入 cache。
//   - cache 有 TTL（默认 10 分钟）自动清理，防止无限增长。
//
// 幂等键优先用 task.IdempotencyKey，否则用 TaskID。
var idempotencyCache = &idemCache{
	ttl: 10 * time.Minute,
}

type idemCache struct {
	mu  sync.Mutex
	ttl time.Duration
	m   map[string]idemEntry
}

type idemEntry struct {
	result   proto.TaskResult
	postedAt time.Time
}

// keyFor 计算任务的幂等键。
func keyFor(task proto.TaskRequest) string {
	if task.IdempotencyKey != "" {
		return task.IdempotencyKey
	}
	return task.TaskID
}

// get 查询缓存。命中返回结果与 true。
func (c *idemCache) get(key string) (proto.TaskResult, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		return proto.TaskResult{}, false
	}
	e, ok := c.m[key]
	if !ok {
		return proto.TaskResult{}, false
	}
	if time.Since(e.postedAt) > c.ttl {
		delete(c.m, key) // 过期清理
		return proto.TaskResult{}, false
	}
	return e.result, true
}

// put 存入结果。
func (c *idemCache) put(key string, result proto.TaskResult) {
	c.mu.Lock()
	if c.m == nil {
		c.m = make(map[string]idemEntry)
	}
	c.m[key] = idemEntry{result: result, postedAt: time.Now()}
	c.mu.Unlock()
}

// sweep 清理过期项（周期调用）。
func (c *idemCache) sweep() {
	c.mu.Lock()
	for k, e := range c.m {
		if time.Since(e.postedAt) > c.ttl {
			delete(c.m, k)
		}
	}
	c.mu.Unlock()
}
