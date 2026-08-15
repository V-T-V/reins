package main

import (
	"context"

	"github.com/QiuShichang/go-rmm/executors"
	"github.com/QiuShichang/go-rmm/proto"
)

// updateExecutor 把"自更新"任务纳入统一 Dispatcher 分发(建议 C / P0-1 修复)。
//
// 原实现 runTask 硬编码 `if task.Type==TaskUpdate` 绕过 Dispatcher,导致:
//   - 能力协商断裂:Agent 上报的 disp.Types() 不含 update,Relay agentSupports 会拒绝所有 update 任务
//   - 架构例外:违背 Dispatcher 的"统一分发"原则
//
// 现作为标准 Executor 注册到 Dispatcher(能力自动派生 + 统一路由 + fail-closed)。
// 仅 slave 注册:master 不上报 update 能力 → Relay 不把 update 路由给 master(主永不更新);
// 万一误路由,Dispatcher fail-closed 拒绝未知类型。
type updateExecutor struct{}

func (e *updateExecutor) Type() string { return proto.TaskUpdate }

// Exec 执行自更新。updateExec.execUpdate 内部处理下载/校验/替换/回滚/重启。
func (e *updateExecutor) Exec(_ context.Context, payload []byte, _ executors.ProgressFunc) proto.TaskResult {
	return updateExec.execUpdate(payload)
}
