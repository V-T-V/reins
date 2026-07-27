//go:build !windows

package executors

import "syscall"

func applyProcAttr(attr *syscall.SysProcAttr) {
	attr.Setpgid = true
}
