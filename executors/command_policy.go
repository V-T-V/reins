// Package executors — 命令策略(命令黑名单)。
//
// P0-S3 修复:之前命令策略只存在于 relay/policy.go,且只在 REST 入口校验。
// 现在下沉到 executors 包,既给 Relay 用(快速拒绝+审计),也给 Agent 用(强制底线)。
// Agent 侧在 Dispatcher.Dispatch 调用前强制过 CheckCommand,杜绝 Relay 被绕过的场景
// (如交互式 shell、cron、未来新增 task 类型)。
//
// 这是纵深防御:Relay 策略是"快速过滤+审计",Agent 策略是"强制底线"。
// 黑名单本质可绕过(见 P2-S1),但能拦常见破坏性命令,且配合路径策略/审计形成多层防线。
package executors

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// CommandPolicy 命令安全策略。
//
// 使用方式:
//   - Relay 侧:checkRelay = true 时,对 powershell/script 命令做高危模式匹配
//   - Agent 侧:所有命令强制过 Check(Agent 是最后防线)
type CommandPolicy struct {
	// BlockedPatterns 高危命令正则(大小写不敏感)。
	BlockedPatterns []*regexp.Regexp
	// quickChecks 每条正则对应的快速检查子串(小写),命中才跑正则。
	quickChecks []string
}

// DefaultCommandPolicy 返回默认高危命令拦截策略。
//
// 规则覆盖:
//   - Windows 破坏性命令(Format-Volume/Stop-Computer/diskpart/reg delete HKLM 等)
//   - Unix 破坏性命令(rm -rf //mkfs/dd/fork bomb)
//   - 外联下载执行(IEX+WebClient/curl|sh 等)
//   - 凭据窃取(新增,覆盖常见凭据读取位置)
//   - 持久化后门(新增,覆盖注册表启动项/计划任务创建)
//
// 注意:黑名单 best-effort,可被别名/换序/Base64/拼接绕过。
// 根本防线是默认 fail-closed + 网络隔离 + 审计。
func DefaultCommandPolicy() *CommandPolicy {
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
	return &CommandPolicy{BlockedPatterns: compiled, quickChecks: quicks}
}

// CheckCommand 校验命令文本是否安全。返回 nil 放行,非 nil 说明命中拦截。
//
// 优化:先对命令做小写化,然后用 strings.Contains 做 quickCheck 预过滤,
// 只有包含危险关键词的命令才跑对应正则。安全命令 O(1) 跳过全部正则。
func (p *CommandPolicy) CheckCommand(command string) error {
	if p == nil {
		return nil
	}
	lower := strings.ToLower(command)
	for i, re := range p.BlockedPatterns {
		// 快速预过滤:不含关键词的规则直接跳过
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

// CheckPayload 根据任务类型校验载荷中的命令字段。
// powershell 检查 command,script 检查 content,其他类型不校验。
func (p *CommandPolicy) CheckPayload(taskType string, payload []byte) error {
	if p == nil {
		return nil
	}
	var cmdText string
	switch taskType {
	case protoTaskPowerShell:
		var t struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal(payload, &t)
		cmdText = t.Command
	case protoTaskScript:
		var t struct {
			Content string `json:"content"`
		}
		_ = json.Unmarshal(payload, &t)
		cmdText = t.Content
	default:
		return nil // 非命令类任务不校验
	}
	if cmdText == "" {
		return nil
	}
	return p.CheckCommand(cmdText)
}

// protoTaskPowerShell/protoTaskScript 与 proto.TaskPowerShell/TaskScript 一致,
// 这里本地常量避免 executors → proto 的循环依赖风险(proto 无依赖,实际可直引,
// 但为保持策略模块自包含,用本地常量)。
const (
	protoTaskPowerShell = "powershell"
	protoTaskScript     = "script"
)
