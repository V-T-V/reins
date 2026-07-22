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
	return d
}
