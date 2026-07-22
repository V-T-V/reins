package main

import (
	"context"
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

	mux := http.NewServeMux()
	relay.registerHTTP(mux)
	mux.HandleFunc("/agent", relay.handleAgentWS)     // Agent 反向连接
	mux.HandleFunc("/console", relay.handleConsoleWS) // 控制台 WS（预留）

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// 心跳超时清理 goroutine
	go sweepLoop(relay)

	// 优雅关闭
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("[relay] HTTP 监听 %s", cfg.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[relay] 监听失败: %v", err)
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
