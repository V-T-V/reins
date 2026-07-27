// CLI tunnel 命令 —— 本地端口转发(ssh -L 体验)。
//
// 阶段 3.1:
//
//	go-rmm tunnel -L 8080:db.internal:3306 -a agent-01
//	go-rmm tunnel -L 8080:db.internal:3306 -a agent-01 --local 127.0.0.1
//
// 流程:
//  1. 本地起 TCP listener(默认 0.0.0.0:8080)
//  2. 每个 accept 的连接 → 生成 tunnelID
//  3. POST /api/tunnel/open {tunnelID, agentID, target} → 等 accepted
//  4. 两个 goroutine:
//     a. 本地 conn → POST /api/tunnel/data (chunked body)
//     b. GET /api/tunnel/poll → 本地 conn (流式读 response)
//  5. 任一方 EOF → POST /api/tunnel/close
//
// 注:当前实现每个 accept 起两个 HTTP 长连接(一读一写)。
// 生产可升级为 WebSocket(Console↔Relay 全双工),但当前 stream-through 够用。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/google/uuid"
)

// cmdTunnel tunnel 子命令入口。
func cmdTunnel(cfg config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("用法: go-rmm tunnel -L <local>:<remote_host>:<remote_port> -a <agent>")
	}

	// 解析 -L 参数(支持 -L 8080:host:3306 或 -L 127.0.0.1:8080:host:3306)
	var localAddr, remoteTarget, agentID string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-L" && i+1 < len(args) {
			localAddr, remoteTarget = parseForwardSpec(args[i+1])
			i++
		} else if strings.HasPrefix(a, "-L=") {
			localAddr, remoteTarget = parseForwardSpec(strings.TrimPrefix(a, "-L="))
		} else if a == "-a" && i+1 < len(args) {
			agentID = args[i+1]
			i++
		} else if strings.HasPrefix(a, "-a=") {
			agentID = strings.TrimPrefix(a, "-a=")
		}
	}

	if localAddr == "" || remoteTarget == "" {
		return fmt.Errorf("-L 参数格式错误,应为 <local_port>:<remote_host>:<remote_port> 或 <local_addr>:<local_port>:<remote_host>:<remote_port>")
	}
	if agentID == "" {
		return fmt.Errorf("-a <agent_id> 必填")
	}

	return runTunnel(cfg, agentID, localAddr, remoteTarget)
}

// parseForwardSpec 解析转发规格。
// "8080:host:3306" → ("127.0.0.1:8080", "host:3306")
// "0.0.0.0:8080:host:3306" → ("0.0.0.0:8080", "host:3306")
func parseForwardSpec(spec string) (localAddr, remoteTarget string) {
	parts := strings.Split(spec, ":")
	if len(parts) == 3 {
		// port:host:remotePort
		return "127.0.0.1:" + parts[0], parts[1] + ":" + parts[2]
	}
	if len(parts) == 4 {
		// addr:port:host:remotePort
		return parts[0] + ":" + parts[1], parts[2] + ":" + parts[3]
	}
	return "", ""
}

// runTunnel 启动本地 listener + 转发。
func runTunnel(cfg config, agentID, localAddr, remoteTarget string) error {
	ln, err := net.Listen("tcp", localAddr)
	if err != nil {
		return fmt.Errorf("本地监听失败 %s: %w", localAddr, err)
	}

	fmt.Printf("🔌 隧道已建立\n")
	fmt.Printf("   本地: %s\n", ln.Addr().String())
	fmt.Printf("   经由: %s (agent=%s)\n", cfg.relay, agentID)
	fmt.Printf("   转发到: %s\n", remoteTarget)
	fmt.Printf("   Ctrl+C 关闭\n\n")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup
	go func() {
		<-ctx.Done()
		fmt.Println("\n关闭隧道...")
		ln.Close()
	}()

	connID := 0
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				wg.Wait()
				return nil
			default:
				return fmt.Errorf("accept 失败: %w", err)
			}
		}
		connID++
		tunnelID := "tun-" + uuid.NewString()[:8]
		fmt.Printf("→ [%s] 新连接 from %s\n", tunnelID, conn.RemoteAddr())

		wg.Add(1)
		go func(c net.Conn, tid string) {
			defer wg.Done()
			defer c.Close()
			handleTunnelConn(cfg, agentID, tid, remoteTarget, c)
		}(conn, tunnelID)
	}
}

// handleTunnelConn 处理单个隧道连接。
func handleTunnelConn(cfg config, agentID, tunnelID, target string, localConn net.Conn) {
	// 1. 开隧道
	openBody := map[string]string{
		"tunnel_id": tunnelID,
		"agent_id":  agentID,
		"target":    target,
	}
	data, _ := json.Marshal(openBody)
	openURL := cfg.relay + "/api/tunnel/open"
	req, _ := http.NewRequest("POST", openURL, strings.NewReader(string(data)))
	req.Header.Set("Content-Type", "application/json")
	if cfg.token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.token)
	}
	openResp, err := controlClient.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ [%s] open 失败: %v\n", tunnelID, err)
		return
	}
	var openResult struct {
		Accepted bool   `json:"accepted"`
		Reason   string `json:"reason"`
	}
	json.NewDecoder(openResp.Body).Decode(&openResult)
	openResp.Body.Close()

	if !openResult.Accepted {
		fmt.Fprintf(os.Stderr, "✗ [%s] Agent 拒绝: %s\n", tunnelID, openResult.Reason)
		return
	}
	fmt.Printf("✓ [%s] 隧道就绪 → %s\n", tunnelID, target)

	// 2. 启动读 goroutine:从 poll 端点读 Agent→本地
	pollDone := make(chan struct{})
	go func() {
		defer close(pollDone)
		pollURL := fmt.Sprintf("%s/api/tunnel/poll?tunnel_id=%s&agent_id=%s",
			cfg.relay, url.QueryEscape(tunnelID), url.QueryEscape(agentID))
		req, _ := http.NewRequest("GET", pollURL, nil)
		if cfg.token != "" {
			req.Header.Set("Authorization", "Bearer "+cfg.token)
		}
		resp, err := streamingClient.Do(req)
		if err != nil {
			return
		}
		defer resp.Body.Close()
		io.Copy(localConn, resp.Body)
	}()

	// 3. 主线程:本地→POST data 端点(写 Agent)
	dataURL := fmt.Sprintf("%s/api/tunnel/data?tunnel_id=%s&agent_id=%s",
		cfg.relay, url.QueryEscape(tunnelID), url.QueryEscape(agentID))
	req2, _ := http.NewRequest("POST", dataURL, localConn)
	if cfg.token != "" {
		req2.Header.Set("Authorization", "Bearer "+cfg.token)
	}
	req2.Header.Set("Content-Type", "application/octet-stream")
	resp2, err := streamingClient.Do(req2)
	if err == nil {
		resp2.Body.Close()
	}

	// 4. 等 poll 结束 + 关闭
	<-pollDone
	closeBody := map[string]string{
		"tunnel_id": tunnelID,
		"agent_id":  agentID,
		"reason":    "local closed",
	}
	apiPost(cfg, "/api/tunnel/close", closeBody, nil)
	fmt.Printf("⏹ [%s] 连接关闭\n", tunnelID)
}
