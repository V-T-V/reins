// Package executors — 命令策略薄引用(单一真相源在 internal/cmdpolicy)。
//
// 架构短2:原此文件与 relay/policy.go 各持一份规则表且已漂移(Relay 15 条/Agent 32 条)。
// 现规则表收敛到 internal/cmdpolicy(32 条),本文件只保留类型别名与构造函数,
// Agent 侧语义不变:Dispatcher.Dispatch 调用前强制过 CheckCommand(最后防线,
// 杜绝 Relay 被绕过的场景——交互式 shell、cron、新增任务类型)。
package executors

import (
	"github.com/V-T-V/reins/internal/cmdpolicy"
)

// CommandPolicy 命令安全策略(共享实现)。
type CommandPolicy = cmdpolicy.Policy

// DefaultCommandPolicy 返回默认高危命令拦截策略(共享 32 条规则)。
func DefaultCommandPolicy() *CommandPolicy { return cmdpolicy.Default() }
