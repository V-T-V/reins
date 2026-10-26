package executors

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/V-T-V/reins/proto"
)

// ScriptExecutor 执行下发的脚本文件。
//
// 支持 lang=ps1（PowerShell，Windows，需 UTF-8 BOM 防中文乱码）、
// lang=bat（cmd，Windows）、lang=sh（POSIX shell，Linux/macOS——P1-4 Linux
// Agent 第一步）。
// 脚本写到临时文件执行，结束后删除。
type ScriptExecutor struct{}

func NewScript() *ScriptExecutor { return &ScriptExecutor{} }

func (e *ScriptExecutor) Type() string { return proto.TaskScript }

func (e *ScriptExecutor) Exec(ctx context.Context, payload []byte, _ ProgressFunc) proto.TaskResult {
	var task proto.ScriptTask
	if err := decode(payload, &task); err != nil {
		return fail("", err.Error())
	}
	if task.Content == "" {
		return fail("", "content 不能为空")
	}
	lang := task.Lang
	if lang == "" {
		lang = "ps1"
	}

	// 平台分派:ps1/bat 仅 Windows,sh 仅 POSIX(Linux/macOS)
	isWindows := runtime.GOOS == "windows"
	switch lang {
	case "ps1", "bat":
		if !isWindows {
			return fail("", "lang="+lang+" 仅在 Windows 上可用(Linux/macOS 请用 lang=sh)")
		}
	case "sh":
		if isWindows {
			return fail("", "lang=sh 仅在 Linux/macOS 上可用(Windows 请用 lang=ps1/bat)")
		}
	default:
		return fail("", "不支持的 lang: "+lang)
	}

	// 写临时脚本文件
	ext := "." + lang
	tmpFile, err := os.CreateTemp("", "reins-*"+ext)
	if err != nil {
		return fail("", "创建临时文件失败: "+err.Error())
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	// PowerShell 脚本含中文需 UTF-8 BOM（PowerShell 5.x 读无 BOM 的 UTF-8 会乱码）
	if lang == "ps1" {
		if _, err := tmpFile.Write([]byte{0xEF, 0xBB, 0xBF}); err != nil {
			tmpFile.Close()
			return fail("", "写 BOM 失败: "+err.Error())
		}
	}
	// ps1: 强制 UTF-8 输出编码（与 powershell 执行器一致）。
	// 否则 PowerShell 5.x 在中文/非英文系统上以 ANSI 代码页(GBK)输出，
	// 导致 stdout 中文乱码（TestScriptPS1 暴露的回归）。
	ps1Prefix := ""
	if lang == "ps1" {
		ps1Prefix = "[Console]::OutputEncoding=[Text.Encoding]::UTF8; $OutputEncoding=[Text.Encoding]::UTF8; "
	}
	if _, err := tmpFile.WriteString(ps1Prefix + task.Content); err != nil {
		tmpFile.Close()
		return fail("", "写脚本失败: "+err.Error())
	}
	tmpFile.Close()
	// sh: POSIX 临时文件默认 0600,加执行位(部分发行版 sh 解释器要求可读即可,
	// chmod 兼容直接执行与 source 两种调用)
	if lang == "sh" {
		if err := os.Chmod(tmpPath, 0755); err != nil {
			return fail("", "chmod 失败: "+err.Error())
		}
	}

	var cmd *exec.Cmd
	switch lang {
	case "ps1":
		// -NonInteractive:无 console 环境下 PS5.x 首次模块加载会等待交互输入,
		// 导致任务假死(与 powershell 执行器保持一致)。
		cmd = exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive",
			"-ExecutionPolicy", "Bypass", "-File", tmpPath)
	case "bat":
		cmd = exec.CommandContext(ctx, "cmd", "/c", tmpPath)
	case "sh":
		cmd = exec.CommandContext(ctx, "sh", tmpPath)
	}
	setProcAttr(cmd) // P1-A1:进程组,防孙进程孤儿

	// 强制 UTF-8（仅 ps1 有效，bat 忽略）
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Env = append(os.Environ(), "PYTHONUTF8=1")

	err = runManaged(ctx, cmd)
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
		result.Success = true
	}
	_ = filepath.Base(tmpPath) // tmpPath 仅用于日志
	return result
}
