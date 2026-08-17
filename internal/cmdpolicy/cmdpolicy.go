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
	// hints 每条黑名单规则命中时的附加指引(平行数组,可空串)。
	hints []string
	// AllowedPatterns 白名单正则(可选,P1-6)。非空时切换 allow-only 语义:
	// 命令必须命中至少一条白名单规则,且仍不得命中黑名单(交集——白名单
	// 写宽(如 ^Get-)也有黑名单兜底,防 NTUSER.DAT/凭据导出等漏网)。
	AllowedPatterns []*regexp.Regexp
}

// SetAllowlist 设置白名单规则(正则,自动加 (?i) 大小写不敏感)。
// 空 slice 清除白名单,回到纯黑名单模式。
// 正则编译失败返回错误——调用方应启动报错而非静默忽略(fail-fast)。
func (p *Policy) SetAllowlist(patterns []string) error {
	if p == nil {
		return fmt.Errorf("policy 为 nil,无法设置白名单")
	}
	compiled := make([]*regexp.Regexp, 0, len(patterns))
	for _, s := range patterns {
		re, err := regexp.Compile("(?i)" + s)
		if err != nil {
			return fmt.Errorf("白名单正则 %q 编译失败: %w", s, err)
		}
		compiled = append(compiled, re)
	}
	p.AllowedPatterns = compiled
	return nil
}

// AllowlistEnabled 是否处于白名单模式。
func (p *Policy) AllowlistEnabled() bool {
	return p != nil && len(p.AllowedPatterns) > 0
}

// Default 返回默认高危命令拦截策略(32 条)。
func Default() *Policy {
	rules := []struct {
		pattern string
		quick   string
		hint    string
	}{
		{`(?i)Format-Volume`, "format-volume", ""},
		{`(?i)Remove-Item\s+(?:-[A-Za-z]+\s+)*-?Recurse\s+(?:-[A-Za-z]+\s+)*-?Force`, "remove-item", ""},
		{`(?i)\b(?:Remove-Item|del|erase|rd|rmdir)\s+(?:-[A-Za-z]+\s+)*-?Recurse\s+(?:-[A-Za-z]+\s+)*-?Force\s+[A-Z]:\\(?:Windows|Users|Program)`, "remove-item", ""},
		{`(?i)Stop-Computer`, "stop-computer", ""},
		{`(?i)Restart-Computer\s+-Force`, "restart-computer", ""},
		{`(?i)diskpart`, "diskpart", ""},
		{`(?i)\breg\s+(?:delete|import|load)\s+HKLM\\SYSTEM`, "reg ", ""},
		{`(?i)bcdedit\s+/,`, "bcdedit", ""},
		{`(?i)\brm\s+-[a-zA-Z]*r[a-zA-Z]*f?\s+/(?:\s|$|\*|\.|\?)`, "rm ", ""},
		{`(?i)\brm\s+-[a-zA-Z]*f[a-zA-Z]*r?\s+/(?:\s|$|\*|\.|\?)`, "rm ", ""},
		{`(?i)\bmkfs\.[a-z0-9]+\s+/dev/`, "mkfs", ""},
		{`(?i)\bdd\s+if=.*of=/dev/(?:sd|nvme|hd|vd|xvd)`, "dd ", ""},
		{`(?i):\(\)\s*\{\s*:\s*\|\s*:&\s*\}\s*;:`, ":()", ""},
		{`(?i)\bchmod\s+-R\s+777\s+/`, "chmod", ""},
		{`(?i)\bchown\s+-R\s+\S+\s+/`, "chown", ""},
		{`(?i)Invoke-WebRequest.*\|\s*(?:Invoke-Expression|\biex\b)`, "invoke-webrequest", ""},
		{`(?i)Invoke-RestMethod.*\|\s*(?:Invoke-Expression|\biex\b)`, "invoke-restmethod", ""},
		{`(?i)\bIEX\s*\(\s*(?:New-Object\s+Net\.WebClient|Invoke-WebRequest)`, "iex", ""},
		{`(?i)\b(?:curl|wget)\b.*\|\s*(?:sh|bash)\b`, "curl", ""},
		{`(?i)Start-BitsTransfer`, "start-bitstransfer", ""},
		// EncodedCommand 拦截指引(使用会话实证:用户以手动 -EncodedCommand 规避
		// 内联转义被拦,但正解是 script 任务)——指向 -s 脚本文件路径。
		{`(?i)-EncodedCommand\s+[A-Za-z0-9+/=]{20,}`, "encodedcommand", "如需执行复杂命令请改用 script 任务(-s <本地脚本文件>,自动 BOM+UTF-8 头+远端临时文件执行,规避转义与编码问题)"},
		{`(?i)-Enc\s+[A-Za-z0-9+/=]{20,}`, "-enc ", "如需执行复杂命令请改用 script 任务(-s <本地脚本文件>)"},
		{`(?i)\breg\s+save\s+HKLM\\SECURITY`, "reg ", ""},
		{`(?i)\breg\s+save\s+HKLM\\SAM`, "reg ", ""},
		{`(?i)\breg\s+save\s+HKLM\\SYSTEM`, "reg ", ""},
		{`(?i)ntds\.util`, "ntds", ""},
		{`(?i)vssadmin\s+delete\s+shadows`, "vssadmin", ""},
		{`(?i)\bGet-Content\s+.*NTUSER\.DAT`, "ntuser.dat", ""},
		{`(?i)New-Item\s+.*HKLM:\\SOFTWARE\\Microsoft\\Windows\\CurrentVersion\\Run`, "currentversion\\run", ""},
		{`(?i)New-ItemProperty\s+.*HKLM:\\SOFTWARE\\Microsoft\\Windows\\CurrentVersion\\Run`, "currentversion\\run", ""},
		{`(?i)schtasks\s+/create`, "schtasks", ""},
		{`(?i)\bat\s+\d+(?::\d)?\s+\S+\\`, " at ", ""},
	}
	var compiled []*regexp.Regexp
	var quicks []string
	var hints []string
	for _, r := range rules {
		if re, err := regexp.Compile(r.pattern); err == nil {
			compiled = append(compiled, re)
			quicks = append(quicks, r.quick)
			hints = append(hints, r.hint)
		}
	}
	return &Policy{BlockedPatterns: compiled, quickChecks: quicks, hints: hints}
}

// CheckCommand 校验命令文本是否安全。返回 nil 放行,非 nil 命中拦截。
func (p *Policy) CheckCommand(command string) error {
	if p == nil {
		return nil
	}
	// 白名单模式:必须命中至少一条 allow 规则(交集语义,黑名单仍在下方兜底)。
	if len(p.AllowedPatterns) > 0 {
		allowed := false
		for _, re := range p.AllowedPatterns {
			if re.MatchString(command) {
				allowed = true
				break
			}
		}
		if !allowed {
			return fmt.Errorf("命令未命中白名单(allow-only,%d 条规则): %.60s", len(p.AllowedPatterns), command)
		}
	}
	lower := strings.ToLower(command)
	for i, re := range p.BlockedPatterns {
		if i < len(p.quickChecks) && p.quickChecks[i] != "" {
			if !strings.Contains(lower, p.quickChecks[i]) {
				continue
			}
		}
		if re.MatchString(command) {
			msg := fmt.Sprintf("命令命中安全策略拦截(高危模式: %s)", re.String())
			if i < len(p.hints) && p.hints[i] != "" {
				msg += "; " + p.hints[i]
			}
			return fmt.Errorf("%s", msg)
		}
	}
	return nil
}

// CheckCommandPayload 根据任务类型校验载荷中的命令字段(走 proto.ExtractCommand 注册表)。
func (p *Policy) CheckCommandPayload(taskType string, payload []byte) error {
	if p == nil {
		return nil
	}
	cmdText := proto.ExtractCommand(taskType, payload)
	if cmdText == "" {
		return nil
	}
	return p.CheckCommand(cmdText)
}
