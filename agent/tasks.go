package main

import (
	"context"
	"sync"
)

// activeTasks 维护当前正在执行的任务的取消句柄。
//
// 用于 task_cancel：Relay→Agent 下发取消时，按 taskID 找到并触发对应 context 的 cancel。
// 线程安全：多个 handleTask goroutine 并发读写。
var activeTasks = &taskTable{}

type taskTable struct {
	mu   sync.RWMutex
	jobs map[string]context.CancelFunc
}

func (t *taskTable) store(id string, cancel context.CancelFunc) {
	t.mu.Lock()
	if t.jobs == nil {
		t.jobs = make(map[string]context.CancelFunc)
	}
	t.jobs[id] = cancel
	t.mu.Unlock()
}

func (t *taskTable) load(id string) (context.CancelFunc, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	c, ok := t.jobs[id]
	return c, ok
}

func (t *taskTable) delete(id string) {
	t.mu.Lock()
	delete(t.jobs, id)
	t.mu.Unlock()
}

// count 返回当前活跃任务数（供 worker pool 限流 C6 用）。
func (t *taskTable) count() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.jobs)
}
