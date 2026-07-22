package main

import (
	"context"
	"encoding/json"
	"runtime"

	"github.com/QiuShichang/go-rmm/proto"
)

// registerExecutors 注册当前可用的执行器。
//
// P0 完整实现 6 个（见 executors/ 包）。此处在 agent 包内做编译时的能力探测：
//   - powershell/script：仅 Windows 可用
//   - screenshot/guiclick：仅 Windows 可用（依赖 Win32）
//   - browse：需 chromedp，P0 先跳过（build tag 隔离）
//   - file：跨平台
//
// Step 4 会把具体执行器从 executors/ 包引入替换占位实现。
func registerExecutors(d *dispatcher, cfg Config) {
	// --- 占位：echo 执行器（联调用，Step 4 替换） ---
	d.register(&echoExecutor{})

	if runtime.GOOS == "windows" {
		// Step 4 填充：
		// d.register(powershell.New())
		// d.register(script.New())
		// d.register(screenshot.New())
		// d.register(guiclick.New(cfg.Glm))
	}
	// file 执行器跨平台
	// d.register(file.New())
	// browse 执行器（build tag 隔离）
}

// echoExecutor 占位执行器，回显任务载荷，用于联调验证 Agent→Relay→Console 闭环。
type echoExecutor struct{}

func (e *echoExecutor) Type() string { return "echo" }

func (e *echoExecutor) Exec(ctx context.Context, payload []byte, progress ProgressFunc) proto.TaskResult {
	var info map[string]any
	_ = json.Unmarshal(payload, &info)
	data, _ := json.Marshal(map[string]any{
		"echo":   string(payload),
		"host":   hostname(),
		"os":     runtimeGOOS() + "/" + runtimeGOARCH(),
		"parsed": info,
	})
	return proto.TaskResult{
		Success:  true,
		ExitCode: 0,
		Stdout:   "echo: " + string(payload),
		Data:     data,
	}
}
