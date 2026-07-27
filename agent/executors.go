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

	executors.RegisterAll(d, executors.Options{VisionClient: visionClient})

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

	return d
}

// defaultRoots 返回平台的默认允许根目录（当用户未显式配置 AllowedRoots 时使用）。
func defaultRoots() []string {
	if runtime.GOOS == "windows" {
		return []string{"C:\\"}
	}
	return []string{"/"}
}
