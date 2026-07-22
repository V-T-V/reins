package main

import (
	"context"

	"github.com/QiuShichang/go-rmm/proto"
)

// Executor 是任务执行器的统一接口。
//
// 每种任务类型（powershell/script/file/screenshot/browse/guiclick）实现一个 Executor。
// dispatcher 按 task.Type 路由到对应实例。返回 TaskResult，由 Agent 回流给 Relay。
//
// 设计要点：
//   - ctx 承载超时（已按 task.Timeout 设置 deadline），执行器应尊重 ctx.Done()。
//   - progress 回调用于长任务（browse/guiclick）流式上报进度，可为 nil。
type Executor interface {
	// Type 返回处理的任务类型，取 proto.Task* 常量。
	Type() string
	// Exec 执行任务。payload 为任务原始载荷，执行器自行反序列化。
	Exec(ctx context.Context, payload []byte, progress ProgressFunc) proto.TaskResult
}

// ProgressFunc 长任务上报进度。step 为当前步骤标识，percent 0-100。
type ProgressFunc func(step, message string, percent int)

// dispatcher 持有所有已注册的执行器，按任务类型路由。
type dispatcher struct {
	executors map[string]Executor
}

func newDispatcher() *dispatcher {
	return &dispatcher{executors: make(map[string]Executor)}
}

// register 注册一个执行器。重复注册同一类型会覆盖（便于测试替换）。
func (d *dispatcher) register(e Executor) {
	d.executors[e.Type()] = e
}

// types 返回所有已注册的任务类型（用于注册时上报 capabilities）。
func (d *dispatcher) types() []string {
	out := make([]string, 0, len(d.executors))
	for t := range d.executors {
		out = append(out, t)
	}
	return out
}

// dispatch 路由并执行任务。未知类型返回失败结果。
func (d *dispatcher) dispatch(ctx context.Context, task proto.TaskRequest, progress ProgressFunc) proto.TaskResult {
	e, ok := d.executors[task.Type]
	if !ok {
		return proto.TaskResult{
			TaskID:  task.TaskID,
			Success: false,
			Error:   "不支持的任务类型: " + task.Type,
		}
	}
	return e.Exec(ctx, task.Payload, progress)
}
