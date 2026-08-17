//go:build !windows

package executors

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestRunManagedKillsProcessTree 实证进程组击杀:sh 起后台 sleep 孙进程,
// ctx 超时后向负 pgid 发 SIGKILL,整组应被终止。
func TestRunManagedKillsProcessTree(t *testing.T) {
	if testing.Short() {
		t.Skip("短模式跳过:进程组击杀实测约 2s")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1200*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", "sleep 30 & sleep 30")

	start := time.Now()
	if err := runManaged(ctx, cmd); err == nil {
		t.Fatal("超时上下文下 runManaged 应返回非 nil 错误")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Wait 耗时 %v——进程组未随超时终止", elapsed)
	}
	// pgrep -g 验证组内进程全灭;pgrep 不可用时跳过断言(Wait 及时返回已验证主路径)
	out, err := exec.Command("pgrep", "-g", strconv.Itoa(cmd.Process.Pid)).Output()
	if err != nil && len(out) == 0 {
		return // pgrep 无输出即组内无存活进程
	}
	if strings.TrimSpace(string(out)) != "" {
		t.Fatalf("进程组 %d 仍有存活进程: %s", cmd.Process.Pid, out)
	}
}

// TestRunManagedNormalExit 验证正常完成路径不受影响。
func TestRunManagedNormalExit(t *testing.T) {
	ctx := context.Background()
	cmd := exec.CommandContext(ctx, "sh", "-c", "echo ok")
	var buf strings.Builder
	cmd.Stdout = &buf
	if err := runManaged(ctx, cmd); err != nil {
		t.Fatalf("正常命令应成功: %v", err)
	}
	if strings.TrimSpace(buf.String()) != "ok" {
		t.Fatalf("stdout 应为 ok,实际 %q", buf.String())
	}
}
