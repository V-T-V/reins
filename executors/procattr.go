// Package executors — 进程属性设置(P1-A1 修复:防子进程孤儿)。
package executors

import (
	"os/exec"
	"syscall"
)

// setProcAttr 为 cmd 设置进程组属性(平台相关)。
// P1-A1 修复:确保超时 cancel 能 kill 整个进程树,不留孤儿。
func setProcAttr(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	applyProcAttr(cmd.SysProcAttr)
}
