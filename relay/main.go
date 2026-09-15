package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/coder/websocket"

	"github.com/QiuShichang/go-rmm/internal/ca"

	"github.com/QiuShichang/go-rmm/proto"
)

// 心跳超时清理：Agent 心跳间隔的 3 倍。默认心跳 15s → 阈值 45s。
const heartbeatTimeout = 45 * time.Second

func main() {
	cfg := parseFlags()
	// S2:结构化日志(JSON 生产 / Text 开发)
	initLogger(cfg.Dev)
	initWebhooks(cfg.Webhooks) // F11:webhook 告警(flag > GO_RMM_WEBHOOKS env)

	// 零配置体验:非 dev 模式下自动生成并保存 token(dev 模式不校验)
	if cfg.Token == "" && !cfg.Dev {
		cfg.Token = loadOrGenToken()
	}

	slog.Info("relay 启动", "version", version, "config", cfg)
	log.Printf("[relay] go-rmm-relay %s 启动: %s", version, cfg)

	relay := NewRelay(cfg)

	// P1-6:命令策略白名单(非空则 allow-only,编译失败 fail-fast)
	if cfg.PolicyAllow != "" {
		patterns := strings.Split(cfg.PolicyAllow, ",")
		if err := relay.policy.SetAllowlist(patterns); err != nil {
			log.Fatalf("[relay] %v", err)
		}
		log.Printf("[relay] 命令白名单已启用(allow-only,%d 条规则,黑名单仍兜底)", len(patterns))
	}

	// C4：打开持久化存储（data/tasks.db）
	store, err := OpenStore(filepath.Join(dataDir(), "tasks.db"))
	if err != nil {
		log.Printf("[relay] 持久化存储打开失败(降级为内存模式): %v", err)
	} else {
		relay.WithStore(store)
		defer store.Close()
		// 恢复在途任务（dispatched 但未完成的）
		relay.recoverInFlightTasks()
		// P0-1:多租户组织持久化(与任务/cron 共用 tasks.db 的 tenants bucket)
		relay.WithTenants(NewTenantManagerWithStore(store))
		log.Printf("[relay] 持久化已启用: %s", filepath.Join(dataDir(), "tasks.db"))
	}

	// C7：加载/创建内置 CA（启用 mTLS 时必需）
	if cfg.MTLS {
		authority, err := ca.LoadOrCreateCA(dataDir())
		if err != nil {
			log.Printf("[relay] CA 初始化失败(mTLS 不可用): %v", err)
		} else {
			relay.WithCA(authority)
			log.Printf("[relay] 内置 CA 已启用: %s", filepath.Join(dataDir(), "ca-root.crt"))
		}
	}

	// C9:打开审计日志(data/audit.log)
	audit, err := OpenAuditLog(filepath.Join(dataDir(), "audit.log"))
	if err != nil {
		log.Printf("[relay] 审计日志打开失败(降级): %v", err)
	} else {
		relay.WithAudit(audit)
		defer audit.Close()
		log.Printf("[relay] 审计日志已启用: %s", filepath.Join(dataDir(), "audit.log"))
	}

	// 自更新版本仓库(data/agent-binaries/)
	updateRepo, err := NewUpdateRepo(dataDir())
	if err != nil {
		log.Printf("[relay] 版本仓库初始化失败(降级): %v", err)
	} else {
		relay.WithUpdateRepo(updateRepo)
		log.Printf("[relay] Agent 版本仓库已启用: %s", filepath.Join(dataDir(), "agent-binaries"))
	}

	// F8:脚本库(data/scripts/)
	scriptLibInstance, err := NewScriptLibrary(dataDir())
	if err != nil {
		log.Printf("[relay] 脚本库初始化失败(降级): %v", err)
	} else {
		scriptLib = scriptLibInstance
		log.Printf("[relay] 脚本库已启用: %s", filepath.Join(dataDir(), "scripts"))
	}

	// H2：启动定时任务调度器
	scheduler := NewScheduler(store, relay)
	if err := scheduler.Start(); err != nil {
		log.Printf("[relay] 调度器启动失败: %v", err)
	} else {
		relay.WithScheduler(scheduler)
		defer scheduler.Stop()
		log.Printf("[relay] 定时任务调度器已启用")
	}

	mux := http.NewServeMux()
	relay.registerHTTP(mux)
	mux.HandleFunc("/agent", relay.handleAgentWS) // Agent 反向连接
	// P2-S6 清理:移除 /console 死路由(原 handler 立即 Close,无用且增加攻击面)
	relay.registerWebUI(mux) // N1:内嵌 Web 控制台

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// C8/G2：mTLS 模式配置 TLSConfig（要求客户端证书 + CRL 校验）
	if cfg.MTLS && relay.ca != nil {
		authority := relay.ca
		srv.TLSConfig = &tls.Config{
			ClientCAs:  caCertPool(authority),
			ClientAuth: tls.RequireAndVerifyClientCert,
			MinVersion: tls.VersionTLS12,
			// G2 修复（A2）：标准库只验链与有效期，不查 CRL。
			// 加 VerifyPeerCertificate 钩子，握手时调 CA.VerifyClientCert（含 IsRevoked）。
			VerifyPeerCertificate: func(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error {
				if len(rawCerts) == 0 {
					return fmt.Errorf("无客户端证书")
				}
				cert, err := x509.ParseCertificate(rawCerts[0])
				if err != nil {
					return fmt.Errorf("客户端证书解析失败: %w", err)
				}
				return authority.VerifyClientCert(cert) // 含链校验 + CRL 撤销检查
			},
		}
		log.Printf("[relay] mTLS 已启用：要求 Agent 客户端证书 + CRL 撤销校验")
	}

	// 优雅关闭 ctx(P2:上移,供 sweepLoop/reaperLoop 共用)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 零交互:启动成功后打印连接信息
	port := strings.TrimPrefix(cfg.Addr, ":")
	if port == "" {
		port = "7780"
	}
	localIP := getLocalIP()
	tokenHint := cfg.Token
	if len(tokenHint) > 8 {
		tokenHint = tokenHint[:8] + "..."
	}

	fmt.Println()
	fmt.Println("╔════════════════════════════════════════════════════╗")
	fmt.Println("║         go-rmm 服务端已启动!                       ║")
	fmt.Printf("║  Web UI:  http://localhost:%s                     ║\n", port)
	fmt.Printf("║  外网IP:  http://%s:%s                          \n", localIP, port)
	fmt.Printf("║  Token:   %s              \n", tokenHint)
	fmt.Println("║                                                    ║")
	fmt.Println("║  ─── 在被控端执行 ───                              ║")
	fmt.Printf("║  go-rmm-agent run -relay ws://%s:%s/agent -token <token>\n", localIP, port)
	fmt.Println("║                                                    ║")
	fmt.Println("║  ─── 在操作端执行 ───                              ║")
	fmt.Printf("║  go-rmm --relay http://%s:%s --token <token> agent list\n", localIP, port)
	fmt.Println("╚════════════════════════════════════════════════════╝")
	fmt.Println()

	// 心跳超时清理 goroutine(P2:支持优雅退出)
	go sweepLoop(ctx, relay)
	// D2:任务状态机 reaper(回退超时未 ACK 的任务,P2 支持优雅退出)
	go reaperLoop(ctx, relay)

	go func() {
		if cfg.MTLS && cfg.CertFile != "" && cfg.KeyFile != "" {
			log.Printf("[relay] HTTPS(mTLS) 监听 %s", cfg.Addr)
			if err := srv.ListenAndServeTLS(cfg.CertFile, cfg.KeyFile); err != nil && err != http.ErrServerClosed {
				log.Fatalf("[relay] mTLS 监听失败: %v", err)
			}
		} else {
			log.Printf("[relay] HTTP 监听 %s", cfg.Addr)
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Fatalf("[relay] 监听失败: %v", err)
			}
		}
	}()

	<-ctx.Done()
	slog.Info("收到退出信号,开始优雅关闭...")
	gracefulShutdown(srv, relay, cfg)
	slog.Info("Relay 已关闭")
}

// gracefulShutdown 优雅停机流程(XL7 神级提升)。
//
// 顺序:
//  1. HTTP Server Shutdown(停止接收新请求,等待在途 handler 完成)
//  2. 等待 Agent WS 连接的 serveAgent goroutine 完成(最多 grace 秒)
//  3. flush 审计日志(异步 channel 排空)
//  4. 关闭 store / audit / scheduler / CA
//  5. 关闭所有 Agent WS 连接
func gracefulShutdown(srv *http.Server, relay *Relay, cfg Config) {
	// Phase 1: HTTP Server 优雅关闭(等待在途 HTTP handler)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	slog.Info("关闭 HTTP Server(等待在途请求)...")
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Warn("HTTP Server 关闭超时", "error", err)
	}

	// Phase 2: flush 审计日志(hash chain 需要排空 channel)
	slog.Info("flush 审计日志...")
	if relay.audit != nil {
		relay.audit.Close() // Close 内部会排空 channel 后关闭文件
	}

	// Phase 3: 关闭 Agent 连接(H-1:提前到 store 前,消除关停窗口写已关 store)
	if relay.agents != nil {
		for _, agent := range relay.agents.Snapshot() {
			if conn := relay.agents.Conn(agent.AgentID); conn != nil {
				conn.close(websocket.StatusNormalClosure, "relay shutting down")
			}
		}
	}
	// Phase 5: 关闭存储
	slog.Info("关闭持久化存储...")
	if relay.store != nil {
		relay.store.Close()
	}

	// Phase 4: 关闭调度器
	slog.Info("关闭定时任务调度器...")
	if relay.scheduler != nil {
		relay.scheduler.Stop()
	}

	// Phase 5: 关闭所有 Agent WS 连言(通知 Agent 重连到其他 Relay)
	slog.Info("关闭 Agent 连接...")
	if relay.agents != nil {
		for _, agent := range relay.agents.Snapshot() {
			if conn := relay.agents.Conn(agent.AgentID); conn != nil {
				conn.close(websocket.StatusNormalClosure, "relay shutting down")
			}
		}
	}

	slog.Info("优雅关闭完成")
}

// sweepLoop 周期清理超时未心跳的 Agent。
// P2 修复:支持优雅退出(响应 shutdown ctx)。
func sweepLoop(ctx context.Context, r *Relay) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			removed := r.agents.Sweep(heartbeatTimeout)
			for _, id := range removed {
				log.Printf("[registry] 清理超时 Agent %s", id)
				r.console.Broadcast(Event{
					Kind:    proto.TypeAgentOffline,
					AgentID: id,
					TS:      time.Now().UnixMilli(),
				})
			}
		}
	}
}

// caCertPool 用内置 CA 的根证书构造一个 CertPool，用于 mTLS 客户端证书校验。
func caCertPool(authority *ca.CA) *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(authority.RootCert())
	return pool
}
