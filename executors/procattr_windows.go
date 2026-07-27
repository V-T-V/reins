//go:build windows

package executors

import "syscall"

func applyProcAttr(attr *syscall.SysProcAttr) {
	attr.CreationFlags = syscall.CREATE_NEW_PROCESS_GROUP
}
