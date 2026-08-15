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
//  3. P1-A:优先 WebSocket 全双工模式(GET /api/tunnel/ws,单连接承载
//     open 信令 + 双向数据 + 心跳,无首包竞态);老 Relay 不支持时
//     自动回退 HTTP 模式(P0-A:open 与 poll 并行启动,消除竞态窗口)
//  4. HTTP 模式:两个 goroutine:
//     a. 本地 conn → POST /api/tunnel/data (chunked body)
//     b. GET /api/tunnel/poll → 本地 conn (流式读 response)
//  5. 任一方 EOF → 通知关闭
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
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/QiuShichang/go-rmm/proto"
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
//
// P0-2 dual-stack:当本地绑定是默认回环 127.0.0.1 时,额外尝试监听 [::1](IPv6 回环),
// 让 IPv6 优先的客户端(如部分浏览器)也能连接;::1 监听失败(IPv6 不可用)则降级为仅 IPv4。
func runTunnel(cfg config, agentID, localAddr, remoteTarget string) error {
	ln, err := net.Listen("tcp", localAddr)
	if err != nil {
		return fmt.Errorf("本地监听失败 %s: %w", localAddr, err)
	}
	listeners := []net.Listener{ln}

	// dual-stack:127.0.0.1 回环时额外尝试 ::1(IPv6 回环)
	if host, port, splitErr := net.SplitHostPort(localAddr); splitErr == nil && host == "127.0.0.1" {
		if ln6, err6 := net.Listen("tcp", net.JoinHostPort("::1", port)); err6 == nil {
			listeners = append(listeners, ln6)
		}
		// ::1 监听失败(IPv6 不可用/被占)则静默降级,仅保留 IPv4
	}

	// P0-B:JSON 模式输出隧道建立摘要(长驻命令,输出一次后继续转发)
	if jsonMode {
		locals := make([]string, len(listeners))
		for i, l := range listeners {
			locals[i] = l.Addr().String()
		}
		outputJSON(map[string]any{
			"command": "tunnel", "ok": true,
			"local": locals, "relay": cfg.relay,
			"agent_id": agentID, "target": remoteTarget,
		})
	} else {
		fmt.Printf("🔌 隧道已建立\n")
		for _, l := range listeners {
			fmt.Printf("   本地: %s\n", l.Addr().String())
		}
		fmt.Printf("   经由: %s (agent=%s)\n", cfg.relay, agentID)
		fmt.Printf("   转发到: %s\n", remoteTarget)
		fmt.Printf("   Ctrl+C 关闭\n\n")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup
	// 每个 listener 一个 accept goroutine(支持 dual-stack),共享同一个 wg
	for _, l := range listeners {
		l := l
		go func() {
			<-ctx.Done()
			l.Close()
		}()
		go acceptTunnel(cfg, agentID, l, remoteTarget, &wg, ctx)
	}

	<-ctx.Done()
	fmt.Println("\n关闭隧道...")
	wg.Wait()
	return nil
}

// acceptTunnel 在单个 listener 上接受隧道连接(供 dual-stack 多 listener 复用)。
func acceptTunnel(cfg config, agentID string, ln net.Listener, remoteTarget string, wg *sync.WaitGroup, ctx context.Context) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				fmt.Fprintf(os.Stderr, "accept 失败: %v\n", err)
				return
			}
		}
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
	// P1-A:优先 WS 全双工模式(单连接 + 心跳,无竞态);握手失败自动回退 HTTP 模式。
	if tryTunnelWS(cfg, agentID, tunnelID, target, localConn) {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// P0-A 修复:poll 与 open 并行启动,消除"Agent 推数据 → poll 注册"竞态窗口。
	// Agent 在 TunnelReady 后立即推数据帧,Relay 在 open 阶段已提前注册 dataCh 缓冲;
	// poll 先于 open 返回启动(Relay 侧 getOrRegister 复用同一通道),
	// open 失败时经 ctx 取消退出,不悬挂。
	pollDone := make(chan struct{})
	go func() {
		defer close(pollDone)
		pollURL := fmt.Sprintf("%s/api/tunnel/poll?tunnel_id=%s&agent_id=%s",
			cfg.relay, url.QueryEscape(tunnelID), url.QueryEscape(agentID))
		req, _ := http.NewRequestWithContext(ctx, "GET", pollURL, nil)
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

	// 1. 开隧道
	openBody := map[string]string{
		"tunnel_id": tunnelID,
		"agent_id":  agentID,
		"target":    target,
	}
	data, _ := json.Marshal(openBody)
	openURL := cfg.relay + "/api/tunnel/open"
	req, _ := http.NewRequestWithContext(ctx, "POST", openURL, strings.NewReader(string(data)))
	req.Header.Set("Content-Type", "application/json")
	if cfg.token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.token)
	}
	openResp, err := controlClient.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ [%s] open 失败: %v\n", tunnelID, err)
		// Review:取消 poll,否则 poll goroutine 挂在 Relay 侧(30 分钟 ctx 超时)导致本函数阻塞
		cancel()
		<-pollDone
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
		cancel()
		<-pollDone
		return
	}
	fmt.Printf("✓ [%s] 隧道就绪 → %s\n", tunnelID, target)

	// 2. 主线程:本地→POST data 端点(写 Agent)
	dataURL := fmt.Sprintf("%s/api/tunnel/data?tunnel_id=%s&agent_id=%s",
		cfg.relay, url.QueryEscape(tunnelID), url.QueryEscape(agentID))
	req2, _ := http.NewRequestWithContext(ctx, "POST", dataURL, localConn)
	if cfg.token != "" {
		req2.Header.Set("Authorization", "Bearer "+cfg.token)
	}
	req2.Header.Set("Content-Type", "application/octet-stream")
	resp2, err := streamingClient.Do(req2)
	if err == nil {
		resp2.Body.Close()
	}

	// 3. 等 poll 结束 + 关闭
	<-pollDone
	closeBody := map[string]string{
		"tunnel_id": tunnelID,
		"agent_id":  agentID,
		"reason":    "local closed",
	}
	apiPost(cfg, "/api/tunnel/close", closeBody, nil)
	fmt.Printf("⏹ [%s] 连接关闭\n", tunnelID)
}

// tryTunnelWS 尝试 WebSocket 全双工隧道(P1-A)。成功(含 Agent 拒绝已处理)返回 true;
// WS 不可用(老 Relay 无此端点/握手失败)返回 false,调用方回退 HTTP 模式。
//
// 协议:
//   - 握手 query 携带 tunnel_id/agent_id/target,握手即 open(Relay 等 Agent ready)
//   - localConn → WS 二进制数据帧;WS 数据帧 → localConn
//   - Relay 发 Ping(0x02)时回 Pong(0x03);收到 RST(0x01)打印 Agent 拒绝原因
//   - 本地 EOF → 发 tunnel_close JSON 信令
func tryTunnelWS(cfg config, agentID, tunnelID, target string, localConn net.Conn) bool {
	base := strings.TrimRight(cfg.relay, "/")
	u := fmt.Sprintf("%s/api/tunnel/ws?tunnel_id=%s&agent_id=%s&target=%s",
		base, url.QueryEscape(tunnelID), url.QueryEscape(agentID), url.QueryEscape(target))
	wsURL := strings.Replace(u, "http://", "ws://", 1)
	wsURL = strings.Replace(wsURL, "https://", "wss://", 1)

	header := http.Header{}
	if cfg.token != "" {
		header.Set("Authorization", "Bearer "+cfg.token)
	}
	// 握手 15s 超时;复用共享 Transport(TLS/insecure/代理配置)。
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	c, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPClient: &http.Client{Transport: httpTransport},
		HTTPHeader: header,
	})
	cancel()
	if err != nil {
		return false // 回退 HTTP(老 Relay / 网络不可达)
	}
	defer c.CloseNow()
	if !jsonMode {
		fmt.Printf("✓ [%s] 隧道就绪(WS) → %s\n", tunnelID, target)
	}

	tctx, tstop := context.WithCancel(context.Background())
	defer tstop()

	// coder/websocket 允许单读者多写者,但为稳妥仍用互斥串行化写。
	var wsMu sync.Mutex
	wsWrite := func(mt websocket.MessageType, data []byte) error {
		wsMu.Lock()
		defer wsMu.Unlock()
		wctx, wcancel := context.WithTimeout(tctx, 10*time.Second)
		defer wcancel()
		return c.Write(wctx, mt, data)
	}

	// Agent→本地:WS 帧 → localConn(数据)/应答心跳/打印拒绝。
	wsDone := make(chan struct{})
	go func() {
		defer close(wsDone)
		for {
			msgType, data, err := c.Read(tctx)
			if err != nil {
				return
			}
			if msgType != websocket.MessageBinary || !proto.IsTunnelFrame(data) {
				continue
			}
			_, chanType, flags, payload, perr := proto.ParseTunnelFrame(data)
			if perr != nil {
				continue
			}
			if chanType == proto.ChanTypeTunnelCtrl {
				switch flags {
				case proto.TunnelCtrlPing:
					_ = wsWrite(websocket.MessageBinary,
						proto.BuildTunnelFrame(proto.ChanTypeTunnelCtrl, tunnelID, proto.TunnelCtrlPong, nil))
				case proto.TunnelCtrlRST:
					fmt.Fprintf(os.Stderr, "✗ [%s] Agent 拒绝: %s\n", tunnelID, string(payload))
					localConn.Close() // 触发本地读循环退出
					return
				}
				continue
			}
			if len(payload) > 0 {
				if _, werr := localConn.Write(payload); werr != nil {
					return
				}
			}
		}
	}()

	// 本地→Agent:localConn → WS 数据帧;EOF 后发 close 信令。
	buf := make([]byte, 32*1024)
	for {
		n, rerr := localConn.Read(buf)
		if n > 0 {
			if werr := wsWrite(websocket.MessageBinary,
				proto.BuildTunnelFrame(proto.ChanTypeTunnelData, tunnelID, 0, buf[:n])); werr != nil {
				break
			}
		}
		if rerr != nil {
			break
		}
	}
	// 本地关闭 → close 信令(不等 wsDone,连接生命周期由 defer 收尾)。
	closeEnv, _ := proto.NewEnvelope(proto.TypeTunnelClose, "console", agentID, proto.TunnelClose{
		TunnelID: tunnelID, Reason: "local closed",
	})
	closeJSON, _ := json.Marshal(closeEnv)
	_ = wsWrite(websocket.MessageText, closeJSON)
	if !jsonMode {
		fmt.Printf("⏹ [%s] 连接关闭(WS)\n", tunnelID)
	}
	return true
}
