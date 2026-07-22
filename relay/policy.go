package main

import (
	"encoding/json"
	"fmt"
	"regexp"
)

// CommandPolicy 命令安全策略（S4 修复）。
//
// 修复 S4(P1)：原实现可下发任意命令（含 Format-Volume、rm -rf 等破坏性操作），
// 无任何拦截。现在对 powershell/script 命令做高危模式匹配，命中则拒绝。
//
// 策略在 Relay 侧校验（不下发到 Agent），避免 Agent 被绕过。
type CommandPolicy struct {
	// BlockedPatterns 高危命令正则（大小写不敏感）。
	BlockedPatterns []*regexp.Regexp
	// BypassTokens 允许绕过的令牌（如运维紧急通道，P2 实现，P0 留空）
}

// DefaultCommandPolicy 返回默认高危命令拦截策略。
func DefaultCommandPolicy() *CommandPolicy {
	// 高危模式：破坏性/擦除/关机/外联下载执行
	patterns := []string{
		// Windows 破坏性命令
		`(?i)Format-Volume`,
		`(?i)Remove-Item\s+-Recurse\s+-Force\s+[A-Z]:\\`, // 递归强删盘根
		`(?i)Stop-Computer`,
		`(?i)Restart-Computer\s+-Force`,
		`(?i)diskpart`,
		`(?i)reg\s+delete\s+HKLM\\SYSTEM`, // 删系统注册表
		`(?i)net\s+user\s+\S+\s+\S+\s+/add`, // 加用户
		// Unix 破坏性命令（script 也可能下发）
		`(?i)\brm\s+-rf\s+/(?:\s|$)`, // rm -rf /
		`(?i)mkfs`,
		`(?i)dd\s+if=.*of=/dev/[sh]d`, // dd 写磁盘
		`(?i):\(\)\{.*\};:`,           // fork bomb
		// 外联下载执行（常见恶意手法）
		`(?i)Invoke-WebRequest.*\|\s*(?:Invoke-Expression|iex)`,
		`(?i)IEX\s*\(.*Net\.WebClient`,
		`(?i)curl\s+.*\|\s*(?:sh|bash)`,
		`(?i)wget\s+.*\|\s*(?:sh|bash)`,
	}
	var compiled []*regexp.Regexp
	for _, p := range patterns {
		if re, err := regexp.Compile(p); err == nil {
			compiled = append(compiled, re)
		}
	}
	return &CommandPolicy{BlockedPatterns: compiled}
}

// Check 校验命令文本是否安全。返回 nil 放行，非 nil 说明命中拦截。
func (p *CommandPolicy) Check(command string) error {
	if p == nil {
		return nil
	}
	for _, re := range p.BlockedPatterns {
		if re.MatchString(command) {
			return fmt.Errorf("命令命中安全策略拦截（高危模式: %s）", re.String())
		}
	}
	return nil
}

// CheckPayload 根据任务类型校验载荷中的命令字段。
func (p *CommandPolicy) CheckPayload(taskType string, payload []byte) error {
	if p == nil {
		return nil
	}
	var cmdText string
	switch taskType {
	case "powershell":
		var t struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal(payload, &t)
		cmdText = t.Command
	case "script":
		var t struct {
			Content string `json:"content"`
		}
		_ = json.Unmarshal(payload, &t)
		cmdText = t.Content
	default:
		return nil // 非命令类任务不校验
	}
	return p.Check(cmdText)
}
