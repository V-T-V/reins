package executors

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/QiuShichang/go-rmm/proto"
)

// ScriptExecutor 执行下发的脚本文件。
//
// 支持 lang=ps1（PowerShell，需 UTF-8 BOM 防中文乱码）和 lang=bat（cmd）。
// 脚本写到临时文件执行，结束后删除。
type ScriptExecutor struct{}

func NewScript() *ScriptExecutor { return &ScriptExecutor{} }

func (e *ScriptExecutor) Type() string { return proto.TaskScript }

func (e *ScriptExecutor) Exec(ctx context.Context, payload []byte, _ ProgressFunc) proto.TaskResult {
	if runtime.GOOS != "windows" {
		return fail("", "script 仅在 Windows 上可用")
	}
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

	// 写临时脚本文件
	ext := "." + lang
	tmpFile, err := os.CreateTemp("", "go-rmm-*"+ext)
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
	if _, err := tmpFile.WriteString(task.Content); err != nil {
		tmpFile.Close()
		return fail("", "写脚本失败: "+err.Error())
	}
	tmpFile.Close()

	var cmd *exec.Cmd
	switch lang {
	case "ps1":
		cmd = exec.CommandContext(ctx, "powershell", "-NoProfile",
			"-ExecutionPolicy", "Bypass", "-File", tmpPath)
	case "bat":
		cmd = exec.CommandContext(ctx, "cmd", "/c", tmpPath)
	default:
		return fail("", "不支持的 lang: "+lang)
	}
	setProcAttr(cmd) // P1-A1:进程组,防孙进程孤儿

	// 强制 UTF-8（仅 ps1 有效，bat 忽略）
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Env = append(os.Environ(), "PYTHONUTF8=1")

	err = cmd.Run()
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
