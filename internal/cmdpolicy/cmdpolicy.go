// Package cmdpolicy 命令安全策略(单一真相源)。
//
// 架构短2:原 relay/policy.go 与 executors/command_policy.go 各持一份规则表
// (Relay 15 条 / Agent 32 条),已实质漂移——Relay 拦不住的 Agent 才拦,
// 两边无漂移提示。现收敛为本包一份,Relay(快速过滤+审计)与 Agent(强制底线)
// 都引用同一份,新增规则只需改这里。
//
// 纵深防御语义:Relay 侧是"快速过滤+审计"(可在反代/网关层进一步收紧),
// Agent 侧是"强制底线"(Dispatcher.Dispatch 前必过)。
// 黑名单本质 best-effort(别名/换序/Base64/拼接可绕),根本防线是
// fail-closed 路径策略 + 网络隔离 + 审计。
package cmdpolicy

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/QiuShichang/go-rmm/proto"
)

// Policy 命令安全策略。
type Policy struct {
	// BlockedPatterns 高危命令正则(大小写不敏感)。
	BlockedPatterns []*regexp.Regexp
	// quickChecks 每条正则对应的快速检查子串(小写),命中才跑正则。
	quickChecks []string
}

// Default 返回默认高危命令拦截策略(32 条)。
//
// 覆盖:Windows/Unix 破坏性命令、外联下载执行、Base64 编码命令、
// 凭据窃取、持久化后门。
func Default() *Policy {
	rules := []struct {
		pattern string
		quick   string
	}{
		// Windows 破坏性命令
		{`(?i)Format-Volume`, "format-volume"},
		{`(?i)Remove-Item\s+(?:-[A-Za-z]+\s+)*-?Recurse\s+(?:-[A-Za-z]+\s+)*-?Force`, "remove-item"},
		{`(?i)\b(?:Remove-Item|del|erase|rd|rmdir)\s+(?:-[A-Za-z]+\s+)*-?Recurse\s+(?:-[A-Za-z]+\s+)*-?Force\s+[A-Z]:\\(?:Windows|Users|Program)`, "remove-item"},
		{`(?i)Stop-Computer`, "stop-computer"},
		{`(?i)Restart-Computer\s+-Force`, "restart-computer"},
		{`(?i)diskpart`, "diskpart"},
		{`(?i)\breg\s+(?:delete|import|load)\s+HKLM\\SYSTEM`, "reg "},
		{`(?i)bcdedit\s+/,`, "bcdedit"},
		// Unix 破坏性命令
		{`(?i)\brm\s+-[a-zA-Z]*r[a-zA-Z]*f?\s+/(?:\s|$|\*|\.|\?)`, "rm "},
		{`(?i)\brm\s+-[a-zA-Z]*f[a-zA-Z]*r?\s+/(?:\s|$|\*|\.|\?)`, "rm "},
		{`(?i)\bmkfs\.[a-z0-9]+\s+/dev/`, "mkfs"},
		{`(?i)\bdd\s+if=.*of=/dev/(?:sd|nvme|hd|vd|xvd)`, "dd "},
		{`(?i):\(\)\s*\{\s*:\s*\|\s*:&\s*\}\s*;:`, ":()"},
		{`(?i)\bchmod\s+-R\s+777\s+/`, "chmod"},
		{`(?i)\bchown\s+-R\s+\S+\s+/`, "chown"},
		// 外联下载执行(常见投递手法)
		{`(?i)Invoke-WebRequest.*\|\s*(?:Invoke-Expression|\biex\b)`, "invoke-webrequest"},
		{`(?i)Invoke-RestMethod.*\|\s*(?:Invoke-Expression|\biex\b)`, "invoke-restmethod"},
		{`(?i)\bIEX\s*\(\s*(?:New-Object\s+Net\.WebClient|Invoke-WebRequest)`, "iex"},
		{`(?i)\b(?:curl|wget)\b.*\|\s*(?:sh|bash)\b`, "curl"},
		{`(?i)Start-BitsTransfer`, "start-bitstransfer"},
		// Base64 编码命令(绕过检测的常见手法)
		{`(?i)-EncodedCommand\s+[A-Za-z0-9+/=]{20,}`, "encodedcommand"},
		{`(?i)-Enc\s+[A-Za-z0-9+/=]{20,}`, "-enc "},
		// 凭据窃取
		{`(?i)\breg\s+save\s+HKLM\\SECURITY`, "reg "},
		{`(?i)\breg\s+save\s+HKLM\\SAM`, "reg "},
		{`(?i)\breg\s+save\s+HKLM\\SYSTEM`, "reg "},
		{`(?i)ntds\.util`, "ntds"},
		{`(?i)vssadmin\s+delete\s+shadows`, "vssadmin"},
		{`(?i)\bGet-Content\s+.*NTUSER\.DAT`, "ntuser.dat"},
		// 持久化后门
		{`(?i)New-Item\s+.*HKLM:\\SOFTWARE\\Microsoft\\Windows\\CurrentVersion\\Run`, "currentversion\\run"},
		{`(?i)New-ItemProperty\s+.*HKLM:\\SOFTWARE\\Microsoft\\Windows\\CurrentVersion\\Run`, "currentversion\\run"},
		{`(?i)schtasks\s+/create`, "schtasks"},
		{`(?i)\bat\s+\d+(?::\d+)?\s+\S+\\`, " at "},
	}
	var compiled []*regexp.Regexp
	var quicks []string
	for _, r := range rules {
		if re, err := regexp.Compile(r.pattern); err == nil {
			compiled = append(compiled, re)
			quicks = append(quicks, r.quick)
		}
	}
	return &Policy{BlockedPatterns: compiled, quickChecks: quicks}
}

// CheckCommand 校验命令文本是否安全。返回 nil 放行,非 nil 命中拦截。
// 先小写化做 quickCheck 预过滤,不含危险关键词的命令 O(1) 跳过全部正则。
func (p *Policy) CheckCommand(command string) error {
	if p == nil {
		return nil
	}
	lower := strings.ToLower(command)
	for i, re := range p.BlockedPatterns {
		if i < len(p.quickChecks) && p.quickChecks[i] != "" {
			if !strings.Contains(lower, p.quickChecks[i]) {
				continue
			}
		}
		if re.MatchString(command) {
			return fmt.Errorf("命令命中安全策略拦截(高危模式: %s)", re.String())
		}
	}
	return nil
}

// CheckCommandPayload 根据任务类型校验载荷中的命令字段。
// 命令提取走 proto.ExtractCommand 注册表(executor 自描述,加新命令类无需改这里)。
func (p *Policy) CheckCommandPayload(taskType string, payload []byte) error {
	if p == nil {
		return nil
	}
	cmdText := proto.ExtractCommand(taskType, payload)
	if cmdText == "" {
		return nil // 非命令类或无命令文本
	}
	return p.CheckCommand(cmdText)
}
