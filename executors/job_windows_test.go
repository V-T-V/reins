//go:build windows

package executors

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestRunManagedKillsProcessTree 实证 Job Object 超时杀整棵进程树:
// powershell 用 Start-Process 起 ping 孙进程(30s),ctx 超时后孙进程应随
// Job 终止——修复前只杀 powershell 本身,ping 存活为僵尸占住端口。
func TestRunManagedKillsProcessTree(t *testing.T) {
	if testing.Short() {
		t.Skip("短模式跳过:进程树击杀实测约 2~7s")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command",
		"Start-Process ping -WindowStyle Hidden -ArgumentList '-n','30','127.0.0.1'; Start-Sleep -Seconds 30")

	start := time.Now()
	if err := runManaged(ctx, cmd); err == nil {
		t.Fatal("超时上下文下 runManaged 应返回非 nil 错误")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("Wait 耗时 %v——Job Object 未随超时终止进程", elapsed)
	}
	// 轮询 tasklist 确认 ping 孙进程已被杀(容忍 kill 传播延迟)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		out, _ := exec.Command("tasklist", "/FI", "IMAGENAME eq PING.EXE", "/FO", "CSV", "/NH").Output()
		if !strings.Contains(string(out), "PING.EXE") {
			return // 孙进程已死,通过
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatal("超时后 5s 内 PING.EXE 孙进程仍未终止——Job Object 未杀进程树")
}

// TestRunManagedNormalExit 验证正常完成路径不受 Job 影响。
func TestRunManagedNormalExit(t *testing.T) {
	ctx := context.Background()
	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", "Write-Output ok")
	var buf strings.Builder
	cmd.Stdout = &buf
	if err := runManaged(ctx, cmd); err != nil {
		t.Fatalf("正常命令应成功: %v", err)
	}
	if strings.TrimSpace(buf.String()) != "ok" {
		t.Fatalf("stdout 应为 ok,实际 %q", buf.String())
	}
}
