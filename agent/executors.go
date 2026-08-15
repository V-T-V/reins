package main

import (
	"log"
	"runtime"

	"github.com/QiuShichang/go-rmm/executors"
	"github.com/QiuShichang/go-rmm/vision"
)

// registerExecutors 构造任务分发器，注册当前可用的执行器。
//
// 能力探测由 executors.RegisterAll 处理：
//   - powershell/script/screenshot：仅 Windows
//   - guiclick：Windows + GLM API key
//   - file：跨平台
//   - browse：默认 stub（-tags browse 编译为真实 chromedp）
//
// M1 修复：同时为 transfers（大文件传输引擎）注入与 FileExecutor 一致的路径策略。
func registerExecutors(cfg Config) *executors.Dispatcher {
	d := executors.New()

	// 构造视觉客户端（仅当配置了 GLM key）
	var visionClient *vision.Client
	if cfg.Glm.APIKey != "" {
		visionClient = vision.New(cfg.Glm.APIKey)
		if cfg.Glm.BaseURL != "" {
			visionClient.BaseURL = cfg.Glm.BaseURL
		}
		if cfg.Glm.Model != "" {
			visionClient.Model = cfg.Glm.Model
		}
	}

	executors.RegisterAll(d, executors.Options{
		VisionClient: visionClient,
		Workspace:    cfg.Workspace, // D4:相对路径解析基准
		Role:         cfg.Role,      // M1:master 注册最小能力集
	})

	// 建议 C / P0-1:自更新执行器纳入统一 Dispatcher(修复能力协商断裂 + 架构例外)。
	// 仅 slave 注册:master 不上报 update 能力,Relay 不把 update 路由给 master(主永不更新);
	// 误路由时 Dispatcher fail-closed 拒绝。原实现 runTask 里 if task.Type==TaskUpdate 绕过 Dispatcher,
	// 导致 Agent 能力清单不含 update → Relay 拒绝所有 update 任务。
	if cfg.Role != "master" {
		d.Register(&updateExecutor{})
	}

	// M1 修复:大文件传输引擎复用与 FileExecutor 一致的路径校验策略。
	// 创建共享的 FilePolicy 实例,同时用于 file 任务和大文件传输。
	//
	// P0-S4 修复:默认 fail-closed(AllowedRoots 空 = 拒绝所有),
	// 强制用户显式配置工作目录。之前用 defaultRoots() 默认放开整盘是错误的
	// (与 DefaultFilePolicy "留空 fail-closed" 的设计意图相反)。
	// 为兼容已有部署(用户可能未配置 AllowedRoots),增加 config 开关:
	//   - 未配置 AllowedRoots 且 AllowAllRoots=true(默认):用整盘兜底,记录告警
	//   - 未配置 AllowedRoots 且 AllowAllRoots=false:严格 fail-closed
	sharedPolicy := executors.DefaultFilePolicy()
	// D4 修复:注入 Workspace 作为相对路径的解析基准。
	// 使用户传 "foo.txt" / "workspace\foo.txt" 可预期地落到允许根内,
	// 不受 Agent 启动 CWD 影响。
	sharedPolicy.Workspace = cfg.Workspace
	if len(cfg.AllowedRoots) > 0 {
		sharedPolicy.AllowedRoots = cfg.AllowedRoots
	} else if cfg.AllowAllRoots {
		// 向后兼容:默认整盘(带告警)
		sharedPolicy.AllowedRoots = defaultRoots()
		log.Printf("[agent] ⚠️ 未配置 allowed_roots,默认放开整盘(不安全)。建议显式配置工作目录或设 allow_all_roots=false")
	}
	// else:严格 fail-closed,所有文件操作拒绝直到配置 AllowedRoots
	transfers.SetPolicy(func(path string) error {
		return sharedPolicy.Check(path)
	})
	// FT 修复:注入 workspace 作为相对路径的写入基准。
	// 避免 SYSTEM 服务模式下相对路径落到 C:\Windows\System32。
	transfers.SetWorkspace(cfg.Workspace)

	return d
}

// defaultRoots 返回平台的默认允许根目录（当用户未显式配置 AllowedRoots 时使用）。
func defaultRoots() []string {
	if runtime.GOOS == "windows" {
		return []string{"C:\\"}
	}
	return []string{"/"}
}
