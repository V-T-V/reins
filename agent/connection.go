package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/QiuShichang/go-rmm/executors"
	"github.com/QiuShichang/go-rmm/proto"
)

// agentBizID 是当前 Agent 的业务身份（用于 Envelope.From）。
// 进程级变量，在 Run 启动时设置。
var agentBizID string

// currentAgent 进程级 Agent 实例(update 任务 download 模式取 token/mTLS 凭证用)。
var currentAgent *Agent

// Agent 主体：持有配置与 dispatcher，管理到 Relay 的反向连接。
type Agent struct {
	cfg   Config
	disp  *executors.Dispatcher
	pool  *WorkerPool       // C6 并发限流
	certs *AgentCerts       // C8 mTLS 证书（nil=非 mTLS 模式）
	xfer  *xferDirectServer // Phase1 直连监听(nil=未开启)
}

func NewAgent(cfg Config, disp *executors.Dispatcher) *Agent {
	a := &Agent{
		cfg:  cfg,
		disp: disp,
		pool: NewWorkerPool(cfg.MaxConcurrentTasks), // C6
	}
	currentAgent = a
	return a
}

// WithCerts 注入 mTLS 证书（C8）。
func (a *Agent) WithCerts(c *AgentCerts) *Agent { a.certs = c; return a }

// mtlsHTTPClient 构造用 AgentCerts 的 http.Client（跳过 Relay 证书 ServerName 校验兜底）。
func mtlsHTTPClient(certs *AgentCerts) *http.Client {
	tlsCfg := certs.TLSConfig()
	if tlsCfg == nil {
		return http.DefaultClient
	}
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: tlsCfg,
		},
	}
}

// Run 启动 Agent 主循环：连接 Relay → 注册 → 心跳 + 接收任务。
//
// 阻塞直到 ctx 取消。断线自动指数退避重连（1s→2s→…→30s 封顶）。
func (a *Agent) Run(ctx context.Context) error {
	agentBizID = a.cfg.AgentID
	cleanupOldRunning() // 自更新两步 rename 的残留清理(旧进程死后才可删)
	// P2-10:本地 Prometheus 指标端点(127.0.0.1:7781,metrics_addr 可改/off 禁用)
	SetActiveSessionsSource(transfers.ActiveSessions)
	if addr := a.cfg.MetricsAddr; addr != "off" {
		if addr == "" {
			addr = defaultMetricsAddr
		}
		go startMetricsServer(ctx, addr)
	}
	// W7:mTLS 证书后台续签(进程级;原只在启动时检查→24h 必过期→永久离线)
	if a.certs != nil {
		go a.certRenewLoop(ctx)
	}
	// 自动更新轮询(可选,默认关;进程级,独立于 WS 连接起伏)
	if a.cfg.AutoUpdateInterval > 0 {
		go a.autoUpdateLoop(ctx)
	}
	// Phase1 内网直连监听(进程级,不随 WS 连接起伏;失败降级中继不致命)
	if a.cfg.XferListen != "" {
		if xd, err := startXferDirect(ctx, a.cfg); err != nil {
			log.Printf("[xfer-direct] 启动失败,直连不可用(继续中继模式): %v", err)
		} else {
			a.xfer = xd
		}
	}
	// W1:候选 Relay 列表 = 主 URL 优先 + 备用(去重)
	candidates := buildRelayCandidates(a.cfg.RelayURL, a.cfg.RelayURLs)
	if len(candidates) == 0 {
		return fmt.Errorf("无 relay_url 可用(主 URL + 备用列表均空)")
	}
	relayIdx := 0
	backoff := initialBackoff
	rng := rand.New(rand.NewSource(time.Now().UnixNano())) // S1: jitter 随机源
	for {
		// 自更新:pendingRestart 标记触发优雅退出,SCM 自动拉新版本
		if pendingRestart {
			log.Printf("[agent] 自更新完成,退出以重启服务...")
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// W1:轮询候选 = candidates[relayIdx],失败后 relayIdx++ 切下一个(主→备用→…→回主)
		chosen := candidates[relayIdx%len(candidates)]
		a.cfg.RelayURL = chosen
		connectStart := time.Now()
		err := a.connectAndServe(ctx)
		if pendingRestart {
			log.Printf("[agent] 自更新完成,退出以重启服务...")
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// W5:曾健康保持较久(≥60s)的连接断开,重置退避(避免一次抖动后每次断线都等满 30s)
		if time.Since(connectStart) >= 60*time.Second {
			backoff = initialBackoff
		}
		relayIdx++
		nextChosen := candidates[relayIdx%len(candidates)]
		if len(candidates) > 1 {
			log.Printf("[agent] 连接 %s 断开: %v,下次将尝试 %s", chosen, err, nextChosen)
		} else {
			log.Printf("[agent] 连接 %s 断开: %v", chosen, err)
		}
		// S1:full jitter 退避,防 thundering herd
		reconnectsTotal.Add(1) // P2-10:重连计数(含首次连接失败的重试)
		jittered := time.Duration(rng.Int63n(int64(backoff)))/10*9 + backoff/10
		log.Printf("[agent] %v 后重连", jittered)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(jittered):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// buildRelayCandidates 构建去重的候选 Relay URL 列表:主 URL 优先,后接备用列表(W1)。
func buildRelayCandidates(primary string, backups []string) []string {
	seen := make(map[string]bool)
	var out []string
	add := func(u string) {
		u = strings.TrimSpace(u)
		if u != "" && !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	add(primary)
	for _, u := range backups {
		add(u)
	}
	return out
}

// certRenewLoop 后台定期检查证书是否需要续签,经 /api/renew 续签(W7)。
func (a *Agent) certRenewLoop(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if a.certs == nil || !a.certs.needRenew() {
				continue
			}
			log.Printf("[agent] 证书即将过期,后台续签(经 /api/renew)")
			renewCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			if err := a.certs.renew(renewCtx, a.cfg.AgentID); err != nil {
				log.Printf("[agent] 证书续签失败: %v(用旧证书继续,下次再试)", err)
			}
			cancel()
		}
	}
}

// connectAndServe 建立一次连接的完整生命周期。
//
// C8：mTLS 模式下用客户端证书 dial（wss://）。
func (a *Agent) connectAndServe(ctx context.Context) error {
	header := http.Header{}
	dialOpts := &websocket.DialOptions{HTTPHeader: header}

	// 非 mTLS 模式：用 token
	if a.certs == nil {
		if a.cfg.Token != "" {
			header.Set("Authorization", "Bearer "+a.cfg.Token)
		}
	} else {
		// mTLS 模式：注入 TLS 配置
		dialOpts.HTTPClient = mtlsHTTPClient(a.certs)
	}

	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	c, _, err := websocket.Dial(dialCtx, a.cfg.RelayURL, dialOpts) //nolint:bodyclose // coder/websocket:成功时 resp.Body 为 nil,无需关闭
	if err != nil {
		return err
	}
	defer c.CloseNow()
	c.SetReadLimit(32 << 20) // 32MiB，支持大截图

	wc := newWSConn(c)
	log.Printf("[agent] 已连接到 Relay %s", a.cfg.RelayURL)

	// 子 ctx：连接级取消（断线时让心跳 goroutine 退出）
	connCtx, connCancel := context.WithCancel(ctx)
	defer connCancel()
	wc.startWriter(connCtx) // 弱网 A1:写优先级 writer(P0 心跳不被 P1 数据饿死)
	// H3:连接断开时关闭所有 shell 会话(防资源泄漏)
	defer sessions.CloseAll()
	// J1:连接断开时关闭所有文件传输会话
	defer transfers.CloseAll()
	// 阶段 3.1:连接断开时关闭所有隧道
	defer tunnelMgr.CloseAll()
	// 注入隧道 ready 回调(发 TunnelReady 给 Relay)
	tunnelReadyCb = func(ready proto.TunnelReady) {
		_ = wc.writeEnvelope(ctx, proto.TypeTunnelReady, "relay", ready)
	}

	// 1. 发送 register
	if err := a.sendRegister(connCtx, wc); err != nil {
		return err
	}
	// 自更新健康确认:注册成功 = 新版本健康,取消回滚倒计时
	ConfirmUpdateHealthy()
	// 2. 连接建立后补发队列中积压的结果（R3 修复核心）
	if resultQueue.len() > 0 {
		log.Printf("[agent] 连接恢复，补发 %d 个积压结果", resultQueue.len())
		resultQueue.flushTo(wc)
	}
	// 3. 启动心跳 goroutine
	go a.heartbeatLoop(connCtx, wc)
	// S1:启动死连接检测 watchdog。半开连接(NAT 超时/防火墙变更)时 recvLoop 永久阻塞,
	// 心跳写在 TCP 缓冲未满时可能"成功",要等 TCP 重传超时(~15 分钟)才报错。
	// watchdog 检查 lastRecv:2×Heartbeat 无收包则 closeNow 强制断开重连。
	go a.deadConnWatchdog(connCtx, wc)
	// 4. 启动进程级后台 sweep(幂等缓存清理 + shell 回收)
	// P1-A4 修复:用 sync.Once 保证只启动一次,绑定进程级 ctx(不随连接起伏)
	startBackgroundSweeps(ctx)
	// 5. 接收循环(阻塞直到断开)
	return a.recvLoop(connCtx, wc)
}

// sweepsOnce 保证后台 sweep goroutine 只启动一次(进程级)。
var sweepsOnce sync.Once

// startBackgroundSweeps 启动进程级后台任务(幂等缓存清理 + 空闲 shell 回收)。
// P1-A2/P1-A4 修复:绑定进程级 ctx,不随连接断开停止。
func startBackgroundSweeps(ctx context.Context) {
	sweepsOnce.Do(func() {
		go idempotencySweepLoop(ctx)
		go sessionsReapLoop(ctx)
		go transfers.SweepOrphanPartsLoop(ctx) // S3:孤儿分片清理
	})
}

// sessionsReapLoop 周期回收空闲 shell 会话(P1-A2 修复)。
// 原 ReapIdle 定义但从未调用,空闲 shell 永久泄漏(powershell 进程 + 2 goroutine)。
func sessionsReapLoop(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sessions.ReapIdle(30 * time.Minute) // 30 分钟无活动的会话回收
		}
	}
}

// sendRegister 发送注册消息。
func (a *Agent) sendRegister(ctx context.Context, wc *wsConn) error {
	caps := a.disp.Types()
	// I1：终端能力声明（shell 不是 executor，但 Agent 支持交互式终端）
	if sessions.available() {
		caps = append(caps, "shell")
	}
	// S4:文件传输能力协商。新版 Agent 支持分片下载(offset/limit),
	// 注册 xfer_offset 让 Relay/CLI 据此决定是否走分片(旧 Agent 无此能力 → 显式拒绝)。
	caps = append(caps, "xfer_offset")
	// update_dl:update 任务支持 download_url 模式(Agent 自己 HTTP 流式拉取)。
	// Relay 据此决定 apply 模式:无此能力的旧 Agent 自动降级 base64 内联(legacy)。
	caps = append(caps, "update_dl")
	// M1 主从架构:Tags 自动加 role 标识,便于 Relay/CLI 按 role 筛选。
	// 不修改原 cfg.Tags(避免污染配置),构造副本。
	tags := make(map[string]string, len(a.cfg.Tags)+1)
	for k, v := range a.cfg.Tags {
		tags[k] = v
	}
	role := a.cfg.Role
	if role == "" {
		role = "slave" // 兼容旧版:未显式配 role 的视为 slave
	}
	tags["role"] = role
	reg := proto.Register{
		AgentID:      a.cfg.AgentID,
		Hostname:     hostname(),
		OS:           runtimeGOOS() + "/" + runtimeGOARCH(),
		Version:      version,
		Capabilities: caps,
		Tags:         tags,
	}
	// Phase1:直连监听在跑才上报地址(未开启/启动失败均不上报,CLI 据此回落中继)
	if a.xfer != nil {
		reg.XferAddrs = a.xfer.Addrs()
	}
	return wc.writeEnvelope(ctx, proto.TypeRegister, "relay", reg)
}

// heartbeatLoop 周期发送心跳。所有 Write 经 wc.writeJSON 串行化。
func (a *Agent) heartbeatLoop(ctx context.Context, wc *wsConn) {
	interval := a.cfg.Heartbeat
	if interval <= 0 {
		// 直接构造 Config(不经 loadConfig 填默认)时的兜底——零值会让
		// NewTicker(0) panic(故障切换 E2E 测试实证)。
		interval = 15 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var seq int64
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			seq++
			hb := proto.Heartbeat{AgentID: a.cfg.AgentID, Seq: seq}
			// V1:心跳携带 Agent 传输计数器(供 Relay 聚合,打通 per-agent 可观测性)
			hb.UploadBytes, hb.DownloadBytes, hb.UploadCount, hb.DownloadCount, hb.FailedCount = transfers.SnapshotStats()
			wctx, wcancel := context.WithTimeout(ctx, 5*time.Second)
			if err := wc.writeControlEnvelope(wctx, proto.TypeHeartbeat, "relay", hb); err != nil {
				log.Printf("[agent] 心跳发送失败: %v", err)
				wcancel()
				// C2 修复：主动关闭连接，强制打断 recvLoop 的 Read，
				// 否则 TCP 半开时 recvLoop 可能阻塞数分钟才超时
				wc.closeNow()
				return
			}
			wcancel()
		}
	}
}

// deadConnWatchdog 死连接检测(S1 修复)。
// 半开连接(TCP 黑洞)时,recvLoop 的 Read 永久阻塞,心跳 Write 在缓冲未满时可能成功,
// 要等 TCP 重传超时(~15 分钟)才报错。watchdog 按 Heartbeat 周期检查 lastRecv 时间戳,
// 超过 2×Heartbeat 无收包则 closeNow 强制断开,让 Run 循环重连。
func (a *Agent) deadConnWatchdog(ctx context.Context, wc *wsConn) {
	// 检查间隔 = Heartbeat,容忍度 = 2×Heartbeat(给一次心跳往返的余量)
	interval := a.cfg.Heartbeat
	if interval < 5*time.Second {
		interval = 5 * time.Second
	}
	threshold := 3 * interval // B1:弱网容忍突发丢包(2×→3×)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			last := time.UnixMilli(wc.lastRecvMs())
			if time.Since(last) > threshold {
				log.Printf("[agent] 死连接检测: %s 无收包,强制断开重连",
					time.Since(last).Truncate(time.Second))
				wc.closeNow()
				return
			}
		}
	}
}

// recvLoop 接收并处理消息（task/shell/file_transfer 控制信令 + 二进制数据帧）。
func (a *Agent) recvLoop(ctx context.Context, wc *wsConn) error {
	for {
		// U2 修复:自更新完成后置 pendingRestart,这里主动退出让 SCM 重启服务。
		// 之前只在 connectAndServe 返回后检查——连接不断开就永远不退出,
		// 新 exe 替换了却无法重启生效(实测 v4.2.1 更新后进程仍跑旧代码)。
		if pendingRestart {
			log.Printf("[agent] 自更新完成,recvLoop 退出以重启服务")
			return nil
		}
		msgType, data, err := wc.read(ctx)
		if err != nil {
			return err
		}
		// 二进制帧:文件上传数据 OR 隧道数据(阶段 1.2 帧扩展区分)
		if msgType == websocket.MessageBinary {
			if proto.IsTunnelFrame(data) {
				// 隧道帧:同步路由到 TunnelManager(快,只是 conn.Write)
				tunnelID, _, _, payload, err := proto.ParseTunnelFrame(data)
				if err == nil {
					tunnelMgr.HandleData(tunnelID, payload)
				}
			} else {
				// 文件帧:顺序处理(必须!异步 go 会导致帧乱序 + isLast 提前到达 → 文件损坏)
				// F1 修复:之前每个帧 `go handleUploadData`,goroutine 调度不保证顺序,
				// 最后几帧可能乱序或 isLast 先执行 → file 提前 close → "file already closed"
				// + 数据丢失(实测 1GB 文件丢失 5832 字节)。
				// 现在 recvLoop 顺序处理:同一 transfer 的帧严格按到达顺序写入。
				// panic recovery 防止执行器内部 panic 崩溃整个 Agent。
				func() {
					defer func() {
						if r := recover(); r != nil {
							// S5:输出 stack trace 便于排查(之前只有错误信息无堆栈)
							log.Printf("[agent] handleUploadData panic(已恢复): %v\n%s", r, debug.Stack())
						}
					}()
					a.handleUploadData(ctx, wc, data)
				}()
			}
			continue
		}
		// JSON 帧 = 控制信令
		var env proto.Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			log.Printf("[agent] 消息解析失败: %v", err)
			continue
		}
		// P0-P2 修复:协议版本校验
		if !env.IsValidVersion() {
			log.Printf("[agent] 收到不兼容的协议版本 v=%d (期望 %d),忽略", env.Version, proto.Version)
			continue
		}
		switch env.Type {
		case proto.TypeHeartbeatAck:
			// S7:Relay 对心跳的确认帧。仅用于证明链路双向可用
			// (read 已刷新 lastRecv,watchdog 不会误判死连接),无业务动作。
		case proto.TypeTaskRequest:
			safeCall("task", func() { a.handleTaskWithPool(ctx, wc, env, a.pool) })
		case proto.TypeTaskCancel:
			safeCall("cancel", func() { a.handleCancel(env) })
		case proto.TypeShellOpen:
			safeCall("shell_open", func() { a.handleShellOpen(ctx, wc, env) })
		case proto.TypeShellInput:
			safeCall("shell_input", func() { a.handleShellInput(env) })
		case proto.TypeShellClose:
			safeCall("shell_close", func() { a.handleShellClose(env) })
		case proto.TypeFileTransferInit:
			safeCall("xfer_init", func() { a.handleTransferInit(ctx, wc, env) })
		case proto.TypeFileTransferCancel:
			var cancel proto.FileTransferCancel
			if err := env.Decode(&cancel); err == nil {
				transfers.Cancel(cancel.TransferID)
			}
		case proto.TypeXferTicket:
			// Phase1:Relay 推来的直连票据授权(存内存,一次性消费,60s TTL 懒清理)
			var grant proto.XferTicketGrant
			if err := env.Decode(&grant); err == nil {
				xferTickets.Add(grant)
				log.Printf("[xfer-direct] 收到票据 %s (%s %s,过期 %d)",
					grant.TicketID[:8], grant.Direction, grant.Path, grant.ExpiryUnix)
			}
		case proto.TypeTunnelOpen:
			safeCall("tunnel_open", func() { a.handleTunnelOpen(ctx, wc, env) })
		case proto.TypeTunnelClose:
			var tc proto.TunnelClose
			if err := env.Decode(&tc); err == nil {
				tunnelMgr.Close(tc.TunnelID, "remote closed: "+tc.Reason)
			}
		default:
			log.Printf("[agent] 忽略消息类型 %s", env.Type)
		}
	}
}

// runTask 执行单个任务的核心逻辑(C5 增强版:ACK + 幂等 + 队列发送)。
// 由 handleTaskWithPool 经 worker pool 限流后调用。
func (a *Agent) runTask(parent context.Context, wc *wsConn, env proto.Envelope) {
	var task proto.TaskRequest
	if err := env.Decode(&task); err != nil {
		log.Printf("[agent] 任务解析失败: %v", err)
		return
	}

	// 1. 立即发 ACK（received）
	ackCtx, ackCancel := contextWithTimeout(5 * time.Second)
	_ = wc.writeEnvelope(ackCtx, proto.TypeTaskAck, "relay", proto.TaskAck{
		TaskID: task.TaskID, Status: "received",
	})
	ackCancel()

	// 2. 幂等检查
	idemKey := keyFor(task)
	if cached, hit := idempotencyCache.get(idemKey); hit {
		log.Printf("[agent] 任务 %s 命中幂等缓存，回放结果（不重复执行）", task.TaskID)
		cached.TaskID = task.TaskID
		a.sendResult(parent, wc, cached)
		return
	}

	log.Printf("[agent] 执行任务 %s type=%s (attempt=%d)", task.TaskID, task.Type, task.Attempt)
	start := time.Now()

	// 3. 任务超时 ctx
	timeout := proto.DefaultTaskTimeout
	if task.Timeout > 0 {
		timeout = task.Timeout
	}
	ctx, cancel := context.WithTimeout(parent, time.Duration(timeout)*time.Second)
	defer cancel()
	activeTasks.store(task.TaskID, cancel)
	defer activeTasks.delete(task.TaskID)

	// 4. 进度上报回调
	progress := func(step, message string, percent int) {
		p := proto.TaskProgress{TaskID: task.TaskID, Step: step, Message: message, Percent: percent}
		pctx, pcancel := contextWithTimeout(5 * time.Second)
		defer pcancel()
		if err := wc.writeEnvelope(pctx, proto.TypeTaskProgress, "relay", p); err != nil {
			log.Printf("[agent] 进度上报失败: %v", err)
		}
	}

	// 5. 执行
	// update 任务特殊处理:不走标准 Executor(强依赖 agent 包的 os.Executable/version)
	//
	// 统一经 Dispatcher 分发(update 已作为标准 Executor 注册;master 不注册→fail-closed 拒绝)
	result := a.disp.Dispatch(ctx, task, progress)
	// 如果是被取消的,标注
	if ctx.Err() == context.Canceled {
		result.Error = "任务被取消"
	}
	result.Duration = int(time.Since(start) / time.Millisecond)
	result.TaskID = task.TaskID

	// 6. 存入幂等缓存
	idempotencyCache.put(idemKey, result)

	// 7. 发送结果（经队列，失败重投）
	a.sendResult(parent, wc, result)
}

// sendResult 把结果经队列发送，并补发队列中积压的结果。
func (a *Agent) sendResult(parent context.Context, wc *wsConn, result proto.TaskResult) {
	// 构造 Envelope 入队
	env, err := proto.NewEnvelope(proto.TypeTaskResult, a.cfg.AgentID, "relay", result)
	if err != nil {
		log.Printf("[agent] 结果序列化失败: %v", err)
		return
	}
	resultQueue.enqueue(env)
	// 尝试 flush（发送成功出队，失败留队列）
	resultQueue.flushTo(wc)
	if resultQueue.len() > 0 {
		log.Printf("[agent] 结果 %s 入队待发（队列剩 %d），将下次重连补发",
			result.TaskID, resultQueue.len())
	} else {
		log.Printf("[agent] 任务 %s 完成 success=%v duration=%dms",
			result.TaskID, result.Success, result.Duration)
	}
}

// handleCancel 处理任务取消（C6 完善前先实现基础版）。
func (a *Agent) handleCancel(env proto.Envelope) {
	var cancel proto.TaskCancel
	if err := env.Decode(&cancel); err != nil {
		return
	}
	if cancelFn, ok := activeTasks.load(cancel.TaskID); ok {
		cancelFn()
		log.Printf("[agent] 任务 %s 已取消", cancel.TaskID)
	}
}

// --- H3 交互式终端 handler ---

// handleShellOpen 开启 shell 会话。
func (a *Agent) handleShellOpen(ctx context.Context, wc *wsConn, env proto.Envelope) {
	var open proto.ShellOpen
	if err := env.Decode(&open); err != nil {
		return
	}
	// output 回调：经 WS 发 shell_output
	outputCb := func(out proto.ShellOutput) {
		wctx, wcancel := contextWithTimeout(5 * time.Second)
		defer wcancel()
		_ = wc.writeEnvelope(wctx, proto.TypeShellOutput, "relay", out)
	}
	if err := sessions.OpenSession(open.SessionID, open.Cols, open.Rows, open.Shell, outputCb); err != nil {
		log.Printf("[shell] 开启会话 %s 失败: %v", open.SessionID, err)
		_ = wc.writeEnvelope(ctx, proto.TypeShellOutput, "relay", proto.ShellOutput{
			SessionID: open.SessionID, Exit: true, ExitCode: -1,
		})
	}
}

// handleShellInput 向会话写入 stdin。
func (a *Agent) handleShellInput(env proto.Envelope) {
	var input proto.ShellInput
	if err := env.Decode(&input); err != nil {
		return
	}
	data, err := base64.StdEncoding.DecodeString(input.DataB64)
	if err != nil {
		return
	}
	if err := sessions.WriteInput(input.SessionID, data); err != nil {
		log.Printf("[shell] 写入会话 %s 失败: %v", input.SessionID, err)
	}
}

// handleShellClose 关闭会话。
func (a *Agent) handleShellClose(env proto.Envelope) {
	var close proto.ShellClose
	if err := env.Decode(&close); err != nil {
		return
	}
	sessions.CloseSession(close.SessionID, close.Reason)
}

// --- J1 大文件传输 handler ---

// handleTunnelOpen 处理 tunnel_open(阶段 3.1:端口转发)。
// 解析 TunnelOpen,委托 TunnelManager.Dial 远端并启动双向转发。
func (a *Agent) handleTunnelOpen(ctx context.Context, wc *wsConn, env proto.Envelope) {
	var open proto.TunnelOpen
	if err := env.Decode(&open); err != nil {
		log.Printf("[tunnel] open 解析失败: %v", err)
		return
	}
	// writeBin:串行化 WS 写(复用 writeMu)
	writeBin := func(frame []byte) error {
		wctx, wcancel := contextWithTimeout(10 * time.Second)
		defer wcancel()
		return wc.writeBinaryData(wctx, frame)
	}
	tunnelMgr.HandleOpen(open, writeBin)
}

// handleTransferInit 处理 file_xfer_init。
func (a *Agent) handleTransferInit(ctx context.Context, wc *wsConn, env proto.Envelope) {
	var init proto.FileTransferInit
	if err := env.Decode(&init); err != nil {
		return
	}
	ready := transfers.HandleInit(init)
	// 发 ready 回 Relay
	_ = wc.writeEnvelope(ctx, proto.TypeFileTransferReady, "relay", ready)

	// R2 已满快路径:.part 已满且校验通过时,HandleInit 在 Reason 里带 "ALREADY_COMPLETE:<sha>"
	// 约定前缀。此时不建 session,直接发 complete 让 Relay 立即返回成功(不等 sender 发数据)。
	if ready.Accepted && strings.HasPrefix(ready.Reason, "ALREADY_COMPLETE:") {
		actualSHA := strings.TrimPrefix(ready.Reason, "ALREADY_COMPLETE:")
		comp := proto.FileTransferComplete{
			TransferID: init.TransferID, Success: true,
			SHA256Actual: actualSHA, Bytes: ready.Size,
		}
		sendCompleteReliably(wc, comp)
		return
	}

	// 如果是下载，Agent 主动开始推数据
	if ready.Accepted && init.Direction == "download" {
		go func() {
			progressCb := func(p proto.FileTransferProgress) {
				pctx, pcancel := contextWithTimeout(5 * time.Second)
				defer pcancel()
				_ = wc.writeEnvelope(pctx, proto.TypeFileTransferProgress, "relay", p)
			}
			completeCb := func(c proto.FileTransferComplete) {
				sendCompleteReliably(wc, c)
			}
			transfers.SendDownloadData(ctx, wc, init.TransferID, progressCb, completeCb)
		}()
	}
}

// sendCompleteReliably 发送传输 Complete 终态帧(P1-5:at-least-once)。
// 原实现丢弃写错误(best-effort),断线瞬间的 complete 丢失会让 Console 侧
// 会话挂到 30 分钟 ctx 超时。现与任务结果同款:先入 resultQueue 再 flush,
// 发送成功出队、失败留队,重连后 flushTo 补发。
// progress/ready 中间帧保持 best-effort(丢失无碍,下一帧自然刷新)。
func sendCompleteReliably(wc *wsConn, c proto.FileTransferComplete) {
	env, err := proto.NewEnvelope(proto.TypeFileTransferComplete, agentBizID, "relay", c)
	if err != nil {
		log.Printf("[xfer] complete 帧序列化失败: %v", err)
		return
	}
	resultQueue.enqueue(env)
	resultQueue.flushTo(wc)
	if left := resultQueue.len(); left > 0 {
		log.Printf("[xfer] transfer=%s complete 入队待发(队列剩 %d),重连补发", c.TransferID, left)
	}
}

// handleUploadData 处理上传二进制数据帧。
func (a *Agent) handleUploadData(ctx context.Context, wc *wsConn, data []byte) {
	transferID, isLast, payload, err := proto.ParseBinaryFrame(data)
	if err != nil {
		log.Printf("[xfer] 解析二进制帧失败: %v", err)
		return
	}
	progressCb := func(p proto.FileTransferProgress) {
		pctx, pcancel := contextWithTimeout(5 * time.Second)
		defer pcancel()
		_ = wc.writeEnvelope(pctx, proto.TypeFileTransferProgress, "relay", p)
	}
	completeCb := func(c proto.FileTransferComplete) {
		sendCompleteReliably(wc, c)
	}
	transfers.ReceiveUploadData(transferID, payload, isLast, progressCb, completeCb)
}

// 退避参数用 var（非 const），便于测试临时覆盖加速。
var (
	initialBackoff = 1 * time.Second
	maxBackoff     = 30 * time.Second
)

// idempotencySweepLoop 周期清理过期的幂等缓存项。
func idempotencySweepLoop(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			idempotencyCache.sweep()
		}
	}
}

// safeCall 包装 handler 调用,防止单个 handler/任务 panic 崩溃整个 Agent 进程(W3)。
func safeCall(name string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[agent] %s 处理 panic(已恢复): %v\n%s", name, r, debug.Stack())
		}
	}()
	fn()
}
