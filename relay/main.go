package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/QiuShichang/go-rmm/proto"
)

// 心跳超时清理：Agent 心跳间隔的 3 倍。默认心跳 15s → 阈值 45s。
const heartbeatTimeout = 45 * time.Second

func main() {
	cfg := parseFlags()
	log.Printf("[relay] go-rmm-relay %s 启动: %s", version, cfg)

	relay := NewRelay(cfg)

	// C4：打开持久化存储（data/tasks.db）
	store, err := OpenStore(filepath.Join(dataDir(), "tasks.db"))
	if err != nil {
		log.Printf("[relay] 持久化存储打开失败(降级为内存模式): %v", err)
	} else {
		relay.WithStore(store)
		defer store.Close()
		// 恢复在途任务（dispatched 但未完成的）
		relay.recoverInFlightTasks()
		log.Printf("[relay] 持久化已启用: %s", filepath.Join(dataDir(), "tasks.db"))
	}

	// C7：加载/创建内置 CA（启用 mTLS 时必需）
	if cfg.MTLS {
		ca, err := LoadOrCreateCA(dataDir())
		if err != nil {
			log.Printf("[relay] CA 初始化失败(mTLS 不可用): %v", err)
		} else {
			relay.WithCA(ca)
			log.Printf("[relay] 内置 CA 已启用: %s", filepath.Join(dataDir(), "ca-root.crt"))
		}
	}

	// C9：打开审计日志（data/audit.log）
	audit, err := OpenAuditLog(filepath.Join(dataDir(), "audit.log"))
	if err != nil {
		log.Printf("[relay] 审计日志打开失败(降级): %v", err)
	} else {
		relay.WithAudit(audit)
		defer audit.Close()
		log.Printf("[relay] 审计日志已启用: %s", filepath.Join(dataDir(), "audit.log"))
	}

	mux := http.NewServeMux()
	relay.registerHTTP(mux)
	mux.HandleFunc("/agent", relay.handleAgentWS)     // Agent 反向连接
	mux.HandleFunc("/console", relay.handleConsoleWS) // 控制台 WS（预留）

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// C8：mTLS 模式配置 TLSConfig（要求客户端证书）
	if cfg.MTLS && relay.ca != nil {
		srv.TLSConfig = &tls.Config{
			// 要求并验证客户端证书：用内置 CA 校验
			ClientCAs:  caCertPool(relay.ca),
			ClientAuth: tls.RequireAndVerifyClientCert,
			MinVersion: tls.VersionTLS12,
		}
		log.Printf("[relay] mTLS 已启用：要求 Agent 客户端证书")
	}

	// 心跳超时清理 goroutine
	go sweepLoop(relay)
	// D2：任务状态机 reaper（回退超时未 ACK 的任务）
	go reaperLoop(relay)

	// 优雅关闭
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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
	log.Printf("[relay] 收到退出信号，开始关闭...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	log.Printf("[relay] 已关闭")
}

// sweepLoop 周期清理超时未心跳的 Agent。
func sweepLoop(r *Relay) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
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

// caCertPool 用内置 CA 的根证书构造一个 CertPool，用于 mTLS 客户端证书校验。
func caCertPool(ca *CA) *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(ca.rootCert)
	return pool
}
