//go:build windows

package main

import "syscall"

// applyProcAttr Windows:用 CREATE_NEW_PROCESS_GROUP 建独立进程组。
// cancel 时可 kill 整组,防 PowerShell 的孙进程成为孤儿。
//
// 注:更彻底的方案是 Job Object(子进程结束自动回收所有后代),
// 但需要 win32 包支持,进程组已覆盖多数场景。
func applyProcAttr(attr *syscall.SysProcAttr) {
	attr.CreationFlags = syscall.CREATE_NEW_PROCESS_GROUP
}
