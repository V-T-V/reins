package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	svc "github.com/kardianos/service"

	"github.com/QiuShichang/go-rmm/executors"
)

// program 实现 svc.Interface，由 kardianos 在服务模式下驱动。
type program struct {
	cfg  Config
	disp *executors.Dispatcher

	ctx    context.Context
	cancel context.CancelFunc
}

func (p *program) Start(s svc.Service) error {
	// 在独立 goroutine 中跑，避免阻塞 SCM 的启动调用
	p.ctx, p.cancel = context.WithCancel(context.Background())
	go p.run()
	return nil
}

func (p *program) Stop(s svc.Service) error {
	if p.cancel != nil {
		p.cancel()
	}
	return nil
}

func (p *program) run() {
	a := NewAgent(p.cfg, p.disp)
	// G1 修复：服务模式也接线 mTLS（与 runForeground 一致）
	if err := setupAgentCerts(p.ctx, a, p.cfg); err != nil {
		log.Printf("[service] mTLS 证书初始化失败: %v", err)
	}
	if err := a.Run(p.ctx); err != nil && p.ctx.Err() == nil {
		log.Printf("[service] Agent 异常退出: %v", err)
	}
}

// serviceConfig 构造 kardianos 服务描述。
func serviceConfig(cfg Config) *svc.Config {
	return &svc.Config{
		Name:        "go-rmm-agent",
		DisplayName: "go-rmm Remote Agent",
		Description: "go-rmm 远程自动化执行 Agent——经反向 WebSocket 连接中继服务器，执行下发的自动化任务。",
		// Arguments：服务启动时调用的参数。kardianos 会自动加上可执行文件路径。
		// 这里不带子命令（空参 = run 模式），从配置文件读 Relay 地址。
		Arguments: []string{},
		Option:    map[string]interface{}{},
	}
}

// runServiceMode 服务模式：由 SCM 调起，无子命令，从配置文件加载。
func runServiceMode(cfg Config, disp *executors.Dispatcher) error {
	prog := &program{cfg: cfg, disp: disp}
	s, err := svc.New(prog, serviceConfig(cfg))
	if err != nil {
		return err
	}
	// 捕获信号也优雅停止
	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-sigCtx.Done()
		_ = s.Stop()
	}()

	log.Printf("[agent] 服务模式启动 (pid=%d)", os.Getpid())
	return s.Run()
}

// installService 注册并可选立即启动。
func installService(cfg Config) error {
	s, err := svc.New(&program{cfg: cfg}, serviceConfig(cfg))
	if err != nil {
		return err
	}
	if err := s.Install(); err != nil {
		return err
	}
	log.Printf("[agent] 服务已安装。启动前请确认 %s 配置正确（relay_url 等）", configPath())
	if err := s.Start(); err != nil {
		log.Printf("[agent] 自动启动失败（服务已安装）: %v", err)
	} else {
		log.Printf("[agent] 服务已启动")
	}
	return nil
}

func uninstallService() error {
	s, err := svc.New(&program{}, serviceConfig(Config{}))
	if err != nil {
		return err
	}
	_ = s.Stop() // 尝试停止
	return s.Uninstall()
}

func startService() error {
	s, err := svc.New(&program{}, serviceConfig(Config{}))
	if err != nil {
		return err
	}
	return s.Start()
}

func stopService() error {
	s, err := svc.New(&program{}, serviceConfig(Config{}))
	if err != nil {
		return err
	}
	return s.Stop()
}

func serviceStatus() {
	s, err := svc.New(&program{}, serviceConfig(Config{}))
	if err != nil {
		log.Printf("[agent] %v", err)
		return
	}
	st, err := s.Status()
	if err != nil {
		log.Printf("[agent] 状态查询失败: %v", err)
		return
	}
	var desc string
	switch st {
	case svc.StatusUnknown:
		desc = "未知"
	case svc.StatusRunning:
		desc = "运行中"
	case svc.StatusStopped:
		desc = "已停止"
	}
	log.Printf("[agent] 服务状态: %s", desc)
}

// 跨平台辅助（connection.go 用）。在非 Windows 平台退化为合理默认。
func hostname() string {
	h, _ := os.Hostname()
	return h
}
func runtimeGOOS() string   { return runtime.GOOS }
func runtimeGOARCH() string { return runtime.GOARCH }
