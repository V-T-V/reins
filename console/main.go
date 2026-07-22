// Package main 是 go-rmm Web 控制台。
//
// 控制台自身无状态：所有 Agent/任务数据都从 Relay 取。
// 它做两件事：
//  1. 反向代理 Relay 的 /api/* 端点（避免浏览器跨域）
//  2. 提供内嵌的静态前端（embed.FS）
//
// 这样控制台可单独部署，也可与 Relay 合并（指向 localhost）。
package main

import (
	"context"
	"embed"
	"flag"
	"io/fs"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"syscall"
)

// version 由 -ldflags 注入。
var version = "dev"

//go:embed web/*.html web/*.js
var webFS embed.FS

func main() {
	addr := flag.String("addr", getEnv("GO_RMM_CONSOLE_ADDR", ":7781"), "控制台 HTTP 监听地址")
	relayURL := flag.String("relay", getEnv("GO_RMM_RELAY_URL", "http://127.0.0.1:7780"), "Relay 地址（反向代理目标）")
	flag.Parse()

	mux := buildMux(*relayURL)
	srv := &http.Server{Addr: *addr, Handler: mux}

	// 优雅关闭
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		log.Printf("[console] go-rmm-console %s", version)
		log.Printf("[console] 监听 %s（反向代理 Relay %s）", *addr, *relayURL)
		log.Printf("[console] 浏览器打开 http://localhost%s", portOrDefault(*addr))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[console] 监听失败: %v", err)
		}
	}()

	<-ctx.Done()
	log.Printf("[console] 关闭中...")
	srv.Shutdown(ctx)
}

// buildMux 构造 HTTP 路由：反向代理 /api/* 到 Relay + 静态前端。
// 抽成独立函数便于测试（main_test.go 直接复用）。
func buildMux(relayURL string) *http.ServeMux {
	relay, err := url.Parse(relayURL)
	if err != nil {
		log.Fatalf("relay URL 解析失败: %v", err)
	}

	mux := http.NewServeMux()

	// 1. 反向代理 /api/* 和 /agent 到 Relay
	proxy := httputil.NewSingleHostReverseProxy(relay)
	// 自定义 Director：保留原 path，透传 SSE 必要头
	origDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		origDirector(req)
		req.Host = relay.Host // 避免 Host 头导致 Relay 拒绝
	}
	mux.Handle("/api/", proxy)
	// SSE 需要禁用缓冲
	mux.HandleFunc("/api/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Accel-Buffering", "no")
		proxy.ServeHTTP(w, r)
	})

	// 2. 静态前端
	staticFS, _ := fs.Sub(webFS, "web")
	mux.Handle("/", http.FileServer(http.FS(staticFS)))

	return mux
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func portOrDefault(addr string) string {
	// ":7781" → ":7781"；"0.0.0.0:7781" → ":7781"
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[i:]
		}
	}
	return addr
}
