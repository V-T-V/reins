// Package main — 进程属性设置(P1-A1 修复:防子进程孤儿)。
//
// 问题:exec.CommandContext 的 ctx cancel 只 kill 直接子进程,
// PowerShell 起的孙进程(ping/notepad 等)成为孤儿继续运行。
//
// 修复:Windows 用 CREATE_NEW_PROCESS_GROUP 建独立进程组,
//   cancel 时 kill 整组(配合 Windows Job Object 更彻底,但进程组已能覆盖多数场景)。
// Linux 用 Setpgid + kill -PGID 杀整个进程组。
package main

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
