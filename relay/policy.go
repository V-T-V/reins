// Package main — Relay 命令策略薄引用(单一真相源在 internal/cmdpolicy)。
//
// 架构短2:原此文件自带 15 条规则表,与 executors 侧(32 条)漂移——Relay 拦不住的
// Agent 才拦,无漂移提示。现共享 internal/cmdpolicy 的 32 条。
//
// Relay 侧语义:任务提交入口的"快速过滤 + 审计"(真正底线在 Agent Dispatcher)。
// 调用方式:r.policy.CheckCommandPayload(taskType, payload) / r.policy.CheckCommand(cmd)。
package main

import (
	"github.com/QiuShichang/go-rmm/internal/cmdpolicy"
)

// CommandPolicy 命令安全策略(共享实现,方法 CheckCommand/CheckCommandPayload)。
type CommandPolicy = cmdpolicy.Policy

// DefaultCommandPolicy 返回默认高危命令拦截策略(共享 32 条规则)。
func DefaultCommandPolicy() *CommandPolicy { return cmdpolicy.Default() }
