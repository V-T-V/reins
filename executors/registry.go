// Package executors 实现并注册 go-rmm 的 6 类任务执行器。
//
// 本包同时定义 Executor 接口与 Dispatcher（任务路由器），供 agent 包引用。
// 依赖方向（单向）：agent → executors → {win32, vision, proto}。
package executors

import (
	"context"
	"sync"

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

// Dispatcher 持有所有已注册的执行器,按任务类型路由。
//
// 并发安全(Q1 修复):executors map 用 RWMutex 保护。
// 典型用法是启动时 Register(写)、运行时 Dispatch/Types(读),但为防御
// 运行期动态注册(如热加载执行器)+ race detector 严格性,统一加锁。
//
// P0-S3 修复:新增 commandPolicy 字段,Dispatch 前强制过 CheckPayload。
// 这是 Agent 侧的最后防线,杜绝 Relay 被绕过(shell/cron/未来新类型)的场景。
type Dispatcher struct {
	mu        sync.RWMutex
	executors map[string]Executor
	// commandPolicy 命令策略(nil 时用默认策略)。
	// 由 SetCommandPolicy 注入;Dispatch 前强制校验。
	commandPolicy *CommandPolicy
}

// New 构造空的 Dispatcher,默认启用 DefaultCommandPolicy。
func New() *Dispatcher {
	return &Dispatcher{
		executors:     make(map[string]Executor),
		commandPolicy: DefaultCommandPolicy(),
	}
}

// SetCommandPolicy 注入自定义命令策略(nil 则禁用校验)。
// Agent 启动时注入与 Relay 一致的策略;Agent 是最后防线。
func (d *Dispatcher) SetCommandPolicy(p *CommandPolicy) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.commandPolicy = p
}

// Register 注册一个执行器。重复注册同一类型会覆盖（便于测试替换）。
func (d *Dispatcher) Register(e Executor) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.executors[e.Type()] = e
}

// Types 返回所有已注册的任务类型（用于注册时上报 capabilities）。
func (d *Dispatcher) Types() []string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]string, 0, len(d.executors))
	for t := range d.executors {
		out = append(out, t)
	}
	return out
}

// Dispatch 路由并执行任务。未知类型返回失败结果。
//
// P0-S3 修复:执行前强制过 commandPolicy.CheckPayload。
// 这是 Agent 侧的最后防线——即使 Relay 被绕过(shell/cron/未来新类型),
// Agent 也会拒绝高危命令。策略命中返回失败结果,不执行。
func (d *Dispatcher) Dispatch(ctx context.Context, task proto.TaskRequest, progress ProgressFunc) proto.TaskResult {
	// P0-S3:Agent 侧命令策略强制校验(最后防线)
	d.mu.RLock()
	policy := d.commandPolicy
	d.mu.RUnlock()
	if policy != nil {
		if err := policy.CheckCommandPayload(task.Type, task.Payload); err != nil {
			return proto.TaskResult{
				TaskID:  task.TaskID,
				Success: false,
				Error:   "Agent 侧策略拒绝: " + err.Error(),
			}
		}
	}

	d.mu.RLock()
	e, ok := d.executors[task.Type]
	d.mu.RUnlock()
	if !ok {
		return proto.TaskResult{
			TaskID: task.TaskID,
			Success: false,
			Error:   "不支持的任务类型: " + task.Type,
		}
	}
	// 执行器调用在锁外进行(Exec 可能很慢,不能持锁)
	return e.Exec(ctx, task.Payload, progress)
}

// --- 辅助：构造失败/成功结果 ---

func fail(taskID, err string) proto.TaskResult {
	return proto.TaskResult{TaskID: taskID, Success: false, Error: err}
}

func ok(taskID string) proto.TaskResult {
	return proto.TaskResult{TaskID: taskID, Success: true, ExitCode: 0}
}
