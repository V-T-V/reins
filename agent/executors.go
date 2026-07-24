package main

import (
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

	// M1 修复：大文件传输引擎复用与 FileExecutor 一致的路径校验策略。
	// 创建共享的 FilePolicy 实例，同时用于 file 任务和大文件传输。
	sharedPolicy := executors.DefaultFilePolicy()
	if len(cfg.AllowedRoots) > 0 {
		sharedPolicy.AllowedRoots = cfg.AllowedRoots
	}
	transfers.SetPolicy(func(path string) error {
		return sharedPolicy.Check(path)
	})

	return d
}
