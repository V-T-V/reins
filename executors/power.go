// Package executors — 电源操作执行器。
//
// 独立 task 类型(不走 powershell 命令黑名单)。
// Relay 侧强制校验 Agent 带 power_allowed=true 标签后才下发。
// Windows: shutdown /s|/r|/h ; Linux: shutdown/poweroff/reboot/hibernate
package executors

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/QiuShichang/go-rmm/proto"
)

// PowerExecutor 电源操作。
type PowerExecutor struct{}

// NewPower 构造。
func NewPower() *PowerExecutor { return &PowerExecutor{} }

func (e *PowerExecutor) Type() string { return proto.TaskPower }

// Exec 执行电源操作。
// 不走命令策略(非 powershell/script 类型,CheckPayload default 放行)。
// 安全由 Relay 的标签白名单 + 审计保证。
func (e *PowerExecutor) Exec(ctx context.Context, payload []byte, _ ProgressFunc) proto.TaskResult {
	var task proto.PowerTask
	if err := decode(payload, &task); err != nil {
		return fail("", "解析 power 任务失败: "+err.Error())
	}
	if task.Op == "" {
		return fail("", "op 不能为空(shutdown/restart/sleep/lock)")
	}

	// 构造命令
	args, desc := buildPowerCommand(task.Op, task.Force, task.Delay, task.Message)
	if args == nil {
		return fail("", "不支持的 op: "+task.Op)
	}

	// 执行(异步,shutdown 本身不阻塞)
	var binName string
	if runtime.GOOS == "windows" {
		binName = "shutdown"
	} else {
		binName = args[0]
		args = args[1:]
	}
	// 电源操作给 30s 超时(shutdown 通常立即返回)
	pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(pctx, binName, args...)
	setProcAttr(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fail("", fmt.Sprintf("%s 执行失败: %v\n输出: %s", desc, err, string(out)))
	}
	return proto.TaskResult{
		Success: true,
		Stdout:  desc + " 指令已执行" + appendOutput(out),
	}
}

// buildPowerCommand 按 op + 平台构造命令参数。
// 返回 (args, 描述)。args[0] 是 Linux 的命令名(Windows 统一用 shutdown.exe)。
func buildPowerCommand(op string, force bool, delay int, message string) ([]string, string) {
	delayStr := fmt.Sprintf("%d", delay)
	if delay == 0 {
		delayStr = "0"
	}
	msgFlag := ""
	if message != "" {
		msgFlag = message
	}
	_ = msgFlag

	if runtime.GOOS == "windows" {
		// Windows: shutdown.exe /s|/r|/h /t <delay> /f /c "msg"
		args := []string{}
		var desc string
		switch op {
		case "shutdown":
			args = append(args, "/s")
			desc = "关机"
		case "restart":
			args = append(args, "/r")
			desc = "重启"
		case "sleep":
			args = append(args, "/h") // 休眠(Windows 语义)
			desc = "休眠"
		case "lock":
			// 锁屏用 rundll32
			return []string{"rundll32.exe", "user32.dll,LockWorkStation"}, "锁屏"
		default:
			return nil, ""
		}
		args = append(args, "/t", delayStr)
		if force {
			args = append(args, "/f")
		}
		if message != "" {
			args = append(args, "/c", message)
		}
		return args, desc
	}

	// Linux
	switch op {
	case "shutdown":
		return []string{"shutdown", "-h", "now"}, "关机"
	case "restart":
		return []string{"reboot"}, "重启"
	case "sleep":
		return []string{"systemctl", "suspend"}, "挂起"
	default:
		return nil, ""
	}
}

// appendOutput 辅助拼接命令输出。
func appendOutput(out []byte) string {
	s := strings.TrimSpace(string(out))
	if s == "" {
		return ""
	}
	return "\n" + s
}
