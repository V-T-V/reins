package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// CommandPolicy 命令安全策略（S4 修复）。
//
// 修复 S4(P1)：原实现可下发任意命令（含 Format-Volume、rm -rf 等破坏性操作），
// 无任何拦截。现在对 powershell/script 命令做高危模式匹配，命中则拒绝。
//
// 策略在 Relay 侧校验（不下发到 Agent），避免 Agent 被绕过。
//
// F2 优化：每条正则配一个 quickCheck 小写子串，Check 时先做廉价的 strings.Contains
// 预过滤，只有命中的才跑正则。绝大多数安全命令不含任何危险关键词，可 O(1) 跳过全部正则。
type CommandPolicy struct {
	// BlockedPatterns 高危命令正则（大小写不敏感）。
	BlockedPatterns []*regexp.Regexp
	// quickChecks 每条正则对应的快速检查子串（小写），命中才跑正则。
	quickChecks []string
	// BypassTokens 允许绕过的令牌（如运维紧急通道，P2 实现，P0 留空）
}

// DefaultCommandPolicy 返回默认高危命令拦截策略。
func DefaultCommandPolicy() *CommandPolicy {
	// 每条规则：{正则模式, 快速检查子串}
	// 快速检查子串取命令中必然出现的小写片段
	rules := []struct {
		pattern string
		quick   string
	}{
		// Windows 破坏性命令
		{`(?i)Format-Volume`, "format-volume"},
		{`(?i)Remove-Item\s+-Recurse\s+-Force\s+[A-Z]:\\`, "remove-item"},
		{`(?i)Stop-Computer`, "stop-computer"},
		{`(?i)Restart-Computer\s+-Force`, "restart-computer"},
		{`(?i)diskpart`, "diskpart"},
		{`(?i)reg\s+delete\s+HKLM\\SYSTEM`, "reg "},
		{`(?i)net\s+user\s+\S+\s+\S+\s+/add`, "net "},
		// Unix 破坏性命令
		{`(?i)\brm\s+-rf\s+/(?:\s|$)`, "rm "},
		{`(?i)mkfs`, "mkfs"},
		{`(?i)dd\s+if=.*of=/dev/[sh]d`, "dd "},
		{`(?i):\(\)\{.*\};:`, ":(){"},
		// 外联下载执行
		{`(?i)Invoke-WebRequest.*\|\s*(?:Invoke-Expression|iex)`, "invoke-webrequest"},
		{`(?i)IEX\s*\(.*Net\.WebClient`, "iex"},
		{`(?i)curl\s+.*\|\s*(?:sh|bash)`, "curl "},
		{`(?i)wget\s+.*\|\s*(?:sh|bash)`, "wget "},
	}
	var compiled []*regexp.Regexp
	var quicks []string
	for _, r := range rules {
		if re, err := regexp.Compile(r.pattern); err == nil {
			compiled = append(compiled, re)
			quicks = append(quicks, r.quick)
		}
	}
	return &CommandPolicy{BlockedPatterns: compiled, quickChecks: quicks}
}

// Check 校验命令文本是否安全。返回 nil 放行，非 nil 说明命中拦截。
//
// F2 优化：先对命令做小写化，然后用 strings.Contains 做 quickCheck 预过滤，
// 只有包含危险关键词的命令才跑对应正则。安全命令 O(1) 跳过全部 15 个正则。
func (p *CommandPolicy) Check(command string) error {
	if p == nil {
		return nil
	}
	lower := strings.ToLower(command)
	for i, re := range p.BlockedPatterns {
		// 快速预过滤：不含关键词的规则直接跳过
		if i < len(p.quickChecks) && p.quickChecks[i] != "" {
			if !strings.Contains(lower, p.quickChecks[i]) {
				continue
			}
		}
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
