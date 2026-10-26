// Package executors — PowerShell 执行器。
//
// P0-P3 修复:改用 -EncodedCommand(UTF16LE Base64)规避编码与注入问题。
// 原 -Command 拼接在 OEM 代码页系统上对特殊字符/中文路径解析可能失败。
package executors

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"os/exec"
	"runtime"
	"unicode/utf16"

	"github.com/QiuShichang/reins/proto"
)

// PowerShellExecutor 执行内联 PowerShell 命令。
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
	// P0-P3:强制 UTF-8 前缀 + 用 -EncodedCommand 传递
	// 编码为 UTF16LE 再 Base64,PowerShell 原生支持,彻底规避编码问题
	// try/catch 前缀:无 attached console 的子进程里 [Console]::OutputEncoding 可能抛异常,
	// 使后续 exit N 不执行、进程 exit 1(TestPowerShellExitCode 回归)。
	fullCmd := "try { [Console]::OutputEncoding=[Text.Encoding]::UTF8; $OutputEncoding=[Text.Encoding]::UTF8 } catch {}; " + task.Command
	encoded := encodePowerShellCommand(fullCmd)

	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive",
		"-ExecutionPolicy", policy, "-EncodedCommand", encoded)
	setProcAttr(cmd) // P1-A1:进程组,防孙进程孤儿
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := runManaged(ctx, cmd)
	result := proto.TaskResult{
		Stdout: stdout.String(),
		Stderr: stderr.String(),
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		result.ExitCode = exitErr.ExitCode()
		result.Success = false
		result.Error = "exit " + exitErr.Error()
	} else if err != nil {
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

// encodePowerShellCommand 把命令文本编码为 PowerShell -EncodedCommand 格式
// (UTF16LE + Base64)。
func encodePowerShellCommand(s string) string {
	// string → []rune → []uint16(UTF16LE)
	runes := []rune(s)
	u16 := utf16.Encode(runes)
	// []uint16 → []byte(little-endian)
	b := make([]byte, len(u16)*2)
	for i, v := range u16 {
		binary.LittleEndian.PutUint16(b[i*2:], v)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// decode 反序列化 JSON 载荷的辅助。
func decode(payload []byte, v any) error {
	return jsonUnmarshal(payload, v)
}
