package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
)

// usage 打印用法。
const usage = `go-rmm-agent %s —— 远程自动化执行 Agent

用法:
  go-rmm-agent <command> [flags]

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
		fmt.Printf("go-rmm-agent %s\n", version)
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

	// 校验 Relay URL（install/run 必填）
	if (cmd == "install" || cmd == "run") && cfg.RelayURL == "" {
		log.Fatalf("缺少 -relay 参数。用法见 --help")
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
func runForeground(cfg Config, disp *dispatcher) {
	log.Printf("[agent] 前台模式启动: agent_id=%s relay=%s caps=%v",
		cfg.AgentID, cfg.RelayURL, disp.types())
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	a := NewAgent(cfg, disp)
	if err := a.Run(ctx); err != nil && ctx.Err() == nil {
		log.Fatalf("[agent] 退出: %v", err)
	}
}

// buildDispatcher 构造任务分发器，注册当前可用的执行器。
//
// 执行器在 Step 4 实现。此处先注册一个 echo 执行器用于联调，
// 后续会替换为真实的 6 个执行器。
func buildDispatcher(cfg Config) *dispatcher {
	d := newDispatcher()
	registerExecutors(d, cfg)
	return d
}
