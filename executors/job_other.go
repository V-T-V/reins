//go:build !windows

// Package executors — 进程树生命周期管理(非 Windows:进程组击杀)。
package executors

import (
	"context"
	"os/exec"
	"syscall"
)

// runManaged 启动 cmd;ctx 取消/超时时向进程组发 SIGKILL 杀整棵树
// (setProcAttr 已 Setpgid,子进程 pid 即 pgid)。
func runManaged(ctx context.Context, cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	waitDone := make(chan struct{})
	defer close(waitDone)
	go func() {
		select {
		case <-ctx.Done():
			if cmd.Process != nil {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
		case <-waitDone:
		}
	}()
	return cmd.Wait()
}
