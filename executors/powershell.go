package executors

import (
	"bytes"
	"context"
	"os/exec"
	"runtime"

	"github.com/QiuShichang/go-rmm/proto"
)

// PowerShellExecutor 执行内联 PowerShell 命令。
//
// 关键：前置 [Console]::OutputEncoding=[Text.Encoding]::UTF8 强制 UTF-8 输出，
// 避免 PowerShell 被管道接管时回退到 OEM 代码页（CP936）导致中文乱码。
type PowerShellExecutor struct{}

func NewPowerShell() *PowerShellExecutor { return &PowerShellExecutor{} }

func (e *PowerShellExecutor) Type() string { return proto.TaskPowerShell }

func (e *PowerShellExecutor) Exec(ctx context.Context, payload []byte, _ ProgressFunc) proto.TaskResult {
	if runtime.GOOS != "windows" {
		return fail("", "powershell 仅在 Windows 上可用")
	}
	var task proto.PowerShellTask
	if err := decode(payload, &task); err != nil {
		return fail("", err.Error())
	}
	if task.Command == "" {
		return fail("", "command 不能为空")
	}

	policy := task.ExecutionPolicy
	if policy == "" {
		policy = "Bypass"
	}
	// 强制 UTF-8：避免管道接管 stdout 时中文乱码
	fullCmd := "[Console]::OutputEncoding=[Text.Encoding]::UTF8; $OutputEncoding=[Text.Encoding]::UTF8; " + task.Command

	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile",
		"-ExecutionPolicy", policy, "-Command", fullCmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	result := proto.TaskResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		result.ExitCode = exitErr.ExitCode()
		result.Success = false
		result.Error = "exit " + exitErr.Error()
	} else if err != nil {
		// 启动失败或 ctx 超时
		result.Success = false
		if ctx.Err() != nil {
			result.Error = "超时或取消"
		} else {
			result.Error = err.Error()
		}
	} else {
		result.ExitCode = 0
		result.Success = true
	}
	return result
}

// decode 反序列化 JSON 载荷的辅助。
func decode(payload []byte, v any) error {
	return jsonUnmarshal(payload, v)
}
