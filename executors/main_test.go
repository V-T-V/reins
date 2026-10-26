package executors

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"
)

// TestMain 在所有 executors 测试前预热 PowerShell 模块分析缓存。
//
// PowerShell 5.x 首次运行时会扫描模块生成缓存(伴随"正在准备首次使用模块"进度记录)。
// 在 go exec 启动的、无 attached console 的子进程里,这个首次准备过程不稳定,
// 可能让首个 PowerShell 测试拿到 exit 1 而非真实退出码(TestPowerShell* 偶发失败)。
// 预热一次让缓存落盘,后续测试进程复用,稳定 exit code 捕获。
func TestMain(m *testing.M) {
	if runtime.GOOS == "windows" {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive",
			"-Command", "Write-Output reins-preheat")
		_ = cmd.Run() // 结果不重要,只为触发并完成模块缓存准备
		cancel()
	}
	os.Exit(m.Run())
}
