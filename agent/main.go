package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/QiuShichang/reins/executors"
)

// usage 打印用法。
const usage = `reins-agent %s —— 远程自动化执行 Agent

用法:
  reins-agent <command> [flags]

命令:
  install      注册为系统服务（Windows 服务 / Linux systemd），需先配置 -relay
  uninstall    卸载系统服务
  start        启动已注册的系统服务
  stop         停止系统服务
  status       查询服务状态
  run          前台运行（调试用，Ctrl+C 退出）
  version      打印版本

Flags（install/run 适用）:
  -relay string   Relay 的 Agent WS 端点，如 ws://host:7780/agent
  -token string   鉴权 token
  -id string      Agent ID（留空自动生成）

配置文件: 与可执行文件同目录的 config.json（服务模式从该文件加载）
`

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	args := os.Args[1:]
	if len(args) == 0 {
		// 无参数 = 被 SCM 调起，走服务模式
		runServiceFromConfig()
		return
	}

	// 处理无需配置文件的命令
	switch args[0] {
	case "version", "-v", "--version":
		fmt.Printf("reins-agent %s\n", version)
		return
	case "uninstall":
		if err := uninstallService(); err != nil {
			log.Fatalf("卸载失败: %v", err)
		}
		log.Printf("已卸载")
		return
	case "stop":
		if err := stopService(); err != nil {
			log.Fatalf("停止失败: %v", err)
		}
		log.Printf("已停止")
		return
	case "status":
		serviceStatus()
		return
	case "-h", "--help", "help":
		fmt.Printf(usage, version)
		return
	}

	// 需要配置的命令：install / run / start
	cmd, cfg, err := loadConfig(args)
	if err != nil {
		log.Fatalf("配置加载失败: %v", err)
	}

	// M5:统一配置校验(install/run 必须有效)
	if cmd == "install" || cmd == "run" {
		if err := cfg.Validate(); err != nil {
			log.Fatalf("配置无效: %v", err)
		}
	}

	disp := buildDispatcher(cfg)

	switch cmd {
	case "install":
		// 持久化配置供服务模式读取
		if err := saveConfig(cfg); err != nil {
			log.Fatalf("配置持久化失败: %v", err)
		}
		if err := installService(cfg); err != nil {
			log.Fatalf("安装失败: %v", err)
		}
	case "start":
		if err := startService(); err != nil {
			log.Fatalf("启动失败: %v", err)
		}
		log.Printf("已启动")
	case "run":
		runForeground(cfg, disp)
	default:
		log.Fatalf("未知命令 %q。用法见 --help", cmd)
	}
}

// runServiceFromConfig 服务模式入口（无参数，从配置文件加载）。
func runServiceFromConfig() {
	_, cfg, err := loadConfig(nil)
	if err != nil {
		log.Fatalf("配置加载失败: %v", err)
	}
	disp := buildDispatcher(cfg)
	if err := runServiceMode(cfg, disp); err != nil {
		log.Fatalf("服务运行失败: %v", err)
	}
}

// runForeground 前台调试模式。
func runForeground(cfg Config, disp *executors.Dispatcher) {
	// 自更新健康自检
	StartHealthCheck()
	// M1 主从架构:master 角色启动本地看门狗
	StartWatchdog(cfg)
	log.Printf("[agent] 前台模式启动: agent_id=%s relay=%s caps=%v mtls=%v role=%s",
		cfg.AgentID, cfg.RelayURL, disp.Types(), cfg.MTLS, cfg.Role)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	a := NewAgent(cfg, disp)
	// G1 修复：mTLS 模式下加载/换取证书（A1 接线）
	if err := setupAgentCerts(ctx, a, cfg); err != nil {
		log.Fatalf("[agent] mTLS 证书初始化失败: %v", err)
	}
	if err := a.Run(ctx); err != nil && ctx.Err() == nil {
		log.Fatalf("[agent] 退出: %v", err)
	}
}

// setupAgentCerts 在 mTLS 模式下加载或换取客户端证书，注入到 Agent。
// 非 mTLS 模式直接返回（向后兼容 ws://+token）。
// G1 修复（A1）：原实现 enroll 整条链是死代码，从不调用。
func setupAgentCerts(ctx context.Context, a *Agent, cfg Config) error {
	if !cfg.MTLS {
		return nil // 非 mTLS 模式，跳过
	}
	certs := NewAgentCerts(cfg.RelayURL, agentDataDir())
	if err := certs.LoadOrEnroll(ctx, cfg.AgentID, cfg.Token); err != nil {
		return fmt.Errorf("证书加载/注册失败: %w", err)
	}
	a.WithCerts(certs)
	log.Printf("[agent] mTLS 证书已就绪")
	return nil
}

// buildDispatcher 构造任务分发器,注册当前可用的执行器。
// 同时初始化 workspace 目录并注入 AllowedRoots。
func buildDispatcher(cfg Config) *executors.Dispatcher {
	ensureWorkspace(&cfg)
	return registerExecutors(cfg)
}

// ensureWorkspace 初始化工作空间目录。
// 阶段 1.1:deploy/文件操作的默认基准目录,AllowedRoots 自动追加。
// 自更新:exeDir 也加入 AllowedRoots(Agent 二进制所在目录,自更新需要写 .new/.bak)。
func ensureWorkspace(cfg *Config) {
	if cfg.Workspace == "" {
		cfg.Workspace = filepath.Join(exeDir(), "workspace")
	}
	if err := os.MkdirAll(cfg.Workspace, 0755); err != nil {
		log.Printf("[agent] workspace 创建失败 %s: %v(继续)", cfg.Workspace, err)
	} else {
		log.Printf("[agent] workspace: %s", cfg.Workspace)
	}
	// workspace 始终加入 AllowedRoots(部署目录可写)
	cfg.AllowedRoots = append(cfg.AllowedRoots, cfg.Workspace)
	// 自更新:exeDir 加入 AllowedRoots(写 .new/.bak 文件)
	// 注:FilePolicy 黑名单仍生效(Program Files 等系统目录会挡,需部署到非系统目录)
	cfg.AllowedRoots = append(cfg.AllowedRoots, exeDir())
}
