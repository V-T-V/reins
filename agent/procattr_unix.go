//go:build !windows

package main

import "syscall"

// applyProcAttr Unix:用 Setpgid 建独立进程组。
// cancel 时 kill -PGID 杀整个进程组,防孙进程孤儿。
func applyProcAttr(attr *syscall.SysProcAttr) {
	attr.Setpgid = true
}
