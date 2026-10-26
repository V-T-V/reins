package main

import (
	"log"
	"runtime"

	"github.com/V-T-V/reins/executors"
	"github.com/V-T-V/reins/vision"
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
		// file 任务与流式传输共用同一份允许根(config.allowed_roots 的约定语义)。
		AllowedRoots: resolveAllowedRoots(cfg),
		Role:         cfg.Role, // M1:master 注册最小能力集
	})

	// update 自更新执行器纳入 Dispatcher(仅 slave;master 不上报 update 能力——
	// Relay agentSupports 据此不会把 update 路由给 master,主永不更新)。
	// 2026-08-18 生产实证:该注册缺失(灾难重建时丢失)导致 Agent caps 不含
	// update,Relay 能力校验拒绝 task run -t update,自更新只能靠专用端点绕行。
	if cfg.Role == "" || cfg.Role == "slave" {
		d.Register(&updateExecutor{})
	}

	// P1-6:命令白名单(可选)。非空时 Agent 侧切换 allow-only(交集语义,黑名单兜底)。
	// Agent 是强制底线——即使 Relay 被绕过(交互式 shell/cron)也生效;
	// 正则编译失败 fail-fast,启动即报错。
	if len(cfg.PolicyAllow) > 0 {
		if err := d.SetAllowlist(cfg.PolicyAllow); err != nil {
			log.Fatalf("[agent] %v", err)
		}
		log.Printf("[agent] 命令白名单已启用(allow-only,%d 条规则,黑名单仍兜底)", len(cfg.PolicyAllow))
	}

	// M1 修复:大文件传输引擎复用与 FileExecutor 一致的路径校验策略。
	// 创建共享的 FilePolicy 实例,同时用于 file 任务和大文件传输。
	//
	// P0-S4 修复:默认 fail-closed(AllowedRoots 空 = 拒绝所有),
	// 强制用户显式配置工作目录。为兼容已有部署增加 config 开关:
	//   - 未配置 AllowedRoots 且 AllowAllRoots=true(默认):用整盘兜底,记录告警
	//   - 未配置 AllowedRoots 且 AllowAllRoots=false:严格 fail-closed
	//
	// 根目录解析统一走 resolveAllowedRoots(纯函数):与 RegisterAll 注入
	// FileExecutor 的根同源,file 任务与流式传输边界一致(此前只接线了
	// 传输引擎,file 任务恒 fail-closed 是装配断线)。
	if len(cfg.AllowedRoots) == 0 && cfg.AllowAllRoots {
		log.Printf("[agent] ⚠️ 未配置 allowed_roots,默认放开整盘(不安全)。建议显式配置工作目录或设 allow_all_roots=false")
	}
	sharedPolicy := executors.DefaultFilePolicy()
	// D4 修复:注入 Workspace 作为相对路径的解析基准。
	// 使用户传 "foo.txt" / "workspace\foo.txt" 可预期地落到允许根内,
	// 不受 Agent 启动 CWD 影响。
	sharedPolicy.Workspace = cfg.Workspace
	sharedPolicy.AllowedRoots = resolveAllowedRoots(cfg)
	transfers.SetPolicy(func(path string) error {
		return sharedPolicy.Check(path)
	})
	// FT 修复:注入 workspace 作为相对路径的写入基准。
	// 避免 SYSTEM 服务模式下相对路径落到 C:\Windows\System32。
	transfers.SetWorkspace(cfg.Workspace)
	// P2-12:发送限速(下载方向)——并发下载吃满上行会挤死心跳 WS。
	if cfg.TransferRateLimitMB > 0 {
		transfers.SetSendRateLimit(cfg.TransferRateLimitMB * 1024 * 1024)
		log.Printf("[agent] 下载发送限速已启用: %.1f MB/s", cfg.TransferRateLimitMB)
	}

	// 项目规则门禁:变更类任务须携带当前 .reins/rules.md 的 ack。
	// 发现范围与文件策略同源(allowed_roots+workspace),规则文件与
	// 项目部署位置天然一致。默认关闭,config rules_gate 开启。
	d.SetRulesGate(executors.NewRulesGate(cfg.RulesGate, resolveAllowedRoots(cfg), cfg.Workspace))
	if cfg.RulesGate {
		log.Printf("[agent] 项目规则门禁已启用: 变更类任务须携带 rules_ack(先 reins rules read 加载规则)")
	}

	return d
}

// resolveAllowedRoots 解析文件操作允许根(纯函数,无副作用):
//   - 配置了 allowed_roots → 原样返回(caller 已追加 workspace/exeDir)
//   - 未配置且 allow_all_roots=true(默认) → 平台整盘兜底(带告警由调用方打)
//   - 未配置且 allow_all_roots=false → nil(严格 fail-closed)
func resolveAllowedRoots(cfg Config) []string {
	if len(cfg.AllowedRoots) > 0 {
		return cfg.AllowedRoots
	}
	if cfg.AllowAllRoots {
		return defaultRoots()
	}
	return nil
}

// defaultRoots 返回平台的默认允许根目录（当用户未显式配置 AllowedRoots 时使用）。
func defaultRoots() []string {
	if runtime.GOOS == "windows" {
		return []string{"C:\\"}
	}
	return []string{"/"}
}
