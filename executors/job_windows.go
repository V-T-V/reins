//go:build windows

// Package executors — 进程树生命周期管理(Windows:Job Object)。
//
// 使用会话实证(2026-08 passport-service/voice-assistant 部署):任务超时后
// 只杀直接子进程 powershell.exe,其启动的孙进程(uvicorn/python 等)存活为
// 僵尸并占住端口,导致后续部署绑定失败、健康检查打到旧进程"假通过"。
// Job Object + KILL_ON_JOB_CLOSE 保证 ctx 取消时整棵进程树终止。
package executors

import (
	"context"
	"os/exec"
	"unsafe"

	"golang.org/x/sys/windows"
)

// jobObjectLimitKillOnJobClose:job 最后一个 handle 关闭时终止 job 内全部进程。
// x/sys/windows 未导出该常量,值来自 winnt.h 的 JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE。
const jobObjectLimitKillOnJobClose = 0x2000

// runManaged 启动 cmd 并纳入 Job Object;ctx 取消/超时时 TerminateJobObject
// 杀掉整棵进程树。返回值语义与 cmd.Run() 一致。
//
// Job 的子进程自动归属(孙进程无需逐个 Assign);Agent 自身异常退出时
// KILL_ON_JOB_CLOSE 也会随 handle 关闭终止残留进程。
func runManaged(ctx context.Context, cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err == nil {
		var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
		info.BasicLimitInformation.LimitFlags = jobObjectLimitKillOnJobClose
		_, err = windows.SetInformationJobObject(
			job,
			windows.JobObjectExtendedLimitInformation,
			uintptr(unsafe.Pointer(&info)),
			uint32(unsafe.Sizeof(info)))
	}
	var proc windows.Handle
	if err == nil {
		// os.Process 不暴露底层 handle,按 pid 重开一个(SetQuota 是 Assign 所需最小权限)。
		proc, err = windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	}
	if err == nil {
		err = windows.AssignProcessToJobObject(job, proc)
	}
	if proc != 0 {
		defer windows.CloseHandle(proc)
	}
	if err != nil {
		// Job 关联失败不阻塞执行:退化为 CommandContext 自身的 kill 语义。
		if job != 0 {
			windows.CloseHandle(job)
		}
		return cmd.Wait()
	}

	waitDone := make(chan struct{})
	defer close(waitDone)
	go func() {
		select {
		case <-ctx.Done():
			_ = windows.TerminateJobObject(job, 1)
		case <-waitDone:
		}
	}()
	err = cmd.Wait()
	windows.CloseHandle(job)
	return err
}
