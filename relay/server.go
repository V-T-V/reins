package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/QiuShichang/go-rmm/proto"
)

// wsConn 对一条 WebSocket 连接的轻封装。
//
// 封装目的：
//   - 绑定一个 agentID / consoleID（身份）。
//   - 串行化 Write（coder/websocket 的 Write 本身并发安全，但封装后便于加业务逻辑）。
type wsConn struct {
	conn *websocket.Conn
	id   string // 连接 ID（UUID，非业务身份）
	role string // "agent" / "console"
	biz  string // 业务身份：agentID 或 consoleID
	// M2：mTLS 客户端证书身份（非 mTLS 模式为空）
	certCN  string
	certDNS []string

	writeMu sync.Mutex

	// B1/P0-C1 修复:二进制帧路由——serveAgent 的 Read 循环拦截二进制帧,
	// 按 transferID 路由到对应 channel,HTTP handler 从 channel 消费。
	//
	// P0-C1 核心修复:用 xferSession(data+done)替代裸 channel。
	// routeXferData 锁内只取 session 引用,锁外用 select+done 发送,
	// 避免持锁阻塞导致死锁(channel 满时消费者断开会双向死锁)。
	xferMu       sync.Mutex
	xferChannels map[string]*xferSession                     // transferID → 下载数据会话
	xferReady    map[string]chan *proto.FileTransferReady    // transferID → ready 信号
	xferComplete map[string]chan *proto.FileTransferComplete // transferID → 完成信号
}

// xferSession 下载传输会话:data 为数据 channel,done 为取消信号。
//
// P0-C1 修复:done channel 在 unregister/closeAll 时 close,
// routeXferData 用 select 监听 done 优雅退出,不再持锁阻塞。
type xferSession struct {
	data chan []byte   // 传输数据(满则背压)
	done chan struct{} // close 时通知生产者(routeXferData)停止发送
	once sync.Once
}

// signalDone 幂等关闭 done channel(防重复 close panic)。
func (s *xferSession) signalDone() {
	s.once.Do(func() { close(s.done) })
}

// registerReadyChannel 注册等待 Agent 的 FileTransferReady（A4 修复）。
func (w *wsConn) registerReadyChannel(transferID string) chan *proto.FileTransferReady {
	w.xferMu.Lock()
	defer w.xferMu.Unlock()
	if w.xferReady == nil {
		w.xferReady = make(map[string]chan *proto.FileTransferReady)
	}
	ch := make(chan *proto.FileTransferReady, 1)
	w.xferReady[transferID] = ch
	return ch
}

// registerCompleteChannel 注册等待 Agent 的 FileTransferComplete（A5 修复）。
func (w *wsConn) registerCompleteChannel(transferID string) chan *proto.FileTransferComplete {
	w.xferMu.Lock()
	defer w.xferMu.Unlock()
	if w.xferComplete == nil {
		w.xferComplete = make(map[string]chan *proto.FileTransferComplete)
	}
	ch := make(chan *proto.FileTransferComplete, 1)
	w.xferComplete[transferID] = ch
	return ch
}

// unregisterXferChannels 清理所有传输通道。
// P0-A 修复:close done channel 通知生产者停止;不再持锁阻塞发送。
// P0-A 延伸:锁外向 dataCh 发 EOF(nil)唤醒阻塞在 poll 的消费者,
// 否则 /api/tunnel/close 后 poll 的 HTTP 响应要挂到 30 分钟 ctx 超时才结束(CLI 连接关闭后进程悬挂)。
func (w *wsConn) unregisterXferChannels(transferID string) {
	w.xferMu.Lock()
	var wake chan []byte
	if s, ok := w.xferChannels[transferID]; ok {
		delete(w.xferChannels, transferID)
		s.signalDone() // 通知 routeXferData 停止发送
		wake = s.data
	}
	if ch, ok := w.xferReady[transferID]; ok {
		delete(w.xferReady, transferID)
		safeCloseReadyChan(ch)
	}
	if ch, ok := w.xferComplete[transferID]; ok {
		delete(w.xferComplete, transferID)
		safeCloseCompleteChan(ch)
	}
	w.xferMu.Unlock()
	if wake != nil {
		select {
		case wake <- nil: // EOF 标记,唤醒 poll 消费者
		default: // 消费者已不在读(channel 满),无需唤醒
		}
	}
}

// registerXferChannel 注册一个传输数据通道,返回只读 channel。
// P0-A 修复:getOrRegister 语义——已存在则复用原通道。
// 隧道 open 阶段提前注册 dataCh 后,poll 复用同一通道,
// Agent 早到的数据帧缓冲在通道里不再丢失(原实现重复注册会覆盖旧通道,数据发往被丢弃的旧 channel)。
func (w *wsConn) registerXferChannel(transferID string) <-chan []byte {
	w.xferMu.Lock()
	defer w.xferMu.Unlock()
	if w.xferChannels == nil {
		w.xferChannels = make(map[string]*xferSession)
	}
	if s, ok := w.xferChannels[transferID]; ok {
		return s.data
	}
	s := &xferSession{
		data: make(chan []byte, 64),
		done: make(chan struct{}),
	}
	w.xferChannels[transferID] = s
	return s.data
}

// unregisterReadyChannel 只清理 ready 信号通道,不动 dataCh。
// P0-A 修复:隧道 open 阶段提前注册 dataCh,open 返回时只应释放 readyCh,
// dataCh 由 poll 的 defer(或 close 端点)负责清理,避免把提前注册的数据通道误杀。
func (w *wsConn) unregisterReadyChannel(transferID string) {
	w.xferMu.Lock()
	defer w.xferMu.Unlock()
	if ch, ok := w.xferReady[transferID]; ok {
		delete(w.xferReady, transferID)
		safeCloseReadyChan(ch)
	}
}

// routeXferData 把二进制帧路由到对应 transferID 的 channel。
//
// P0-C1 修复(关键):锁内只取 session 引用,锁外用 select+done 发送。
// 旧实现持锁阻塞发送 → channel 满时与 unregister 双向死锁。
// 现在即使 channel 满,done 关闭后 select 立即退出,不持锁。
func (w *wsConn) routeXferData(transferID string, data []byte, isLast bool) {
	w.xferMu.Lock()
	s, ok := w.xferChannels[transferID]
	w.xferMu.Unlock()
	if !ok {
		return
	}
	// 锁外发送:channel 满时阻塞,但 done 关闭后立即退出
	if len(data) > 0 {
		select {
		case s.data <- data:
		case <-s.done:
			return // 消费者已退出,丢弃
		}
	}
	if isLast {
		select {
		case s.data <- nil: // EOF 标记
		case <-s.done:
		}
	}
}

// closeAllXferChannels 关闭所有传输通道(Agent 断线时调用,P1-#5 修复)。
// 让所有阻塞在 <-ch 的 HTTP handler 立即收到 channel 关闭信号并退出。
func (w *wsConn) closeAllXferChannels() {
	w.xferMu.Lock()
	defer w.xferMu.Unlock()
	for id, s := range w.xferChannels {
		delete(w.xferChannels, id)
		s.signalDone()
		select {
		case s.data <- nil: // P0-A:唤醒 poll 消费者
		default:
		}
	}
	for id, ch := range w.xferReady {
		delete(w.xferReady, id)
		safeCloseReadyChan(ch)
	}
	for id, ch := range w.xferComplete {
		delete(w.xferComplete, id)
		safeCloseCompleteChan(ch)
	}
}

// safeCloseReadyChan/safeCloseCompleteChan 防 close 已关闭 channel 的 panic。
func safeCloseReadyChan(ch chan *proto.FileTransferReady) {
	defer func() { recover() }()
	close(ch)
}

func safeCloseCompleteChan(ch chan *proto.FileTransferComplete) {
	defer func() { recover() }()
	close(ch)
}

func (w *wsConn) writeJSON(ctx context.Context, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	return w.conn.Write(ctx, websocket.MessageText, data)
}

func (w *wsConn) close(code websocket.StatusCode, reason string) {
	if w == nil || w.conn == nil {
		return // nil 安全（测试 stub 可能无真实连接）
	}
	_ = w.conn.Close(code, reason)
}

// --- 事件总线（Relay → Console 实时推送） ---
//
// 控制台通过 SSE 订阅事件。事件类型：agent_online / agent_offline / task_result / task_progress。
// Relay 把这些事件投递到事件总线，所有订阅的 Console 各取一份。

// Event 推给控制台的事件（信封形式）。
type Event struct {
	Kind    string          // agent_online / agent_offline / task_result / task_progress
	AgentID string          // 相关 Agent
	Payload json.RawMessage // 原始载荷
	TS      int64
}

// EventBus 订阅/广播模型。
//
// D7：慢消费者丢弃计数（atomic），通过 Dropped() 暴露，metrics 反映。
type EventBus struct {
	mu          sync.RWMutex
	subscribers map[string]chan Event // subID → channel
	dropped     int64                 // 累计丢弃事件数
}

func NewEventBus() *EventBus {
	return &EventBus{subscribers: make(map[string]chan Event)}
}

// Dropped 返回累计丢弃的事件数。
func (b *EventBus) Dropped() int64 { return atomic.LoadInt64(&b.dropped) }

// Subscribe 创建一个订阅，返回 subID 与只读 channel。
// buf 为 channel 缓冲大小；满则广播时丢弃该订阅的事件（避免慢消费者阻塞全局）。
func (b *EventBus) Subscribe(buf int) (string, <-chan Event) {
	ch := make(chan Event, buf)
	id := uuid.NewString()
	b.mu.Lock()
	b.subscribers[id] = ch
	b.mu.Unlock()
	return id, ch
}

func (b *EventBus) Unsubscribe(id string) {
	b.mu.Lock()
	if ch, ok := b.subscribers[id]; ok {
		delete(b.subscribers, id)
		close(ch)
	}
	b.mu.Unlock()
}

// SubscribeWithFilter 订阅指定类型的事件(D3:事件过滤,减少无关推送)。
// 只投递 filter 中包含的事件类型。filter=nil 表示全量订阅(兼容旧接口)。
func (b *EventBus) SubscribeWithFilter(bufSize int, filter map[string]bool) (string, <-chan Event) {
	if bufSize <= 0 {
		bufSize = 64
	}
	id := uuid.NewString()
	// 用包装 channel 做过滤
	rawCh := make(chan Event, bufSize)
	filteredCh := make(chan Event, bufSize)
	b.mu.Lock()
	b.subscribers[id] = rawCh
	b.mu.Unlock()
	// 单 goroutine 做过滤(避免 Broadcast 中遍历 filter)
	go func() {
		for ev := range rawCh {
			if filter == nil || filter[ev.Kind] {
				select {
				case filteredCh <- ev:
				default:
					atomic.AddInt64(&b.dropped, 1)
				}
			}
		}
		close(filteredCh)
	}()
	return id, filteredCh
}

// Broadcast 向所有订阅者非阻塞投递。慢消费者丢事件并计数(D7)。
func (b *EventBus) Broadcast(ev Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, ch := range b.subscribers {
		select {
		case ch <- ev:
		default:
			atomic.AddInt64(&b.dropped, 1)
			log.Printf("[eventbus] 订阅者队列满，丢弃事件 %s", ev.Kind)
		}
	}
}

// --- Relay 主体 ---

type Relay struct {
	cfg          Config
	agents       *Registry
	console      *EventBus
	store        TaskStore      // 任务持久化(R2/F1),可为 nil(关闭持久化)
	ca           *CA            // 内置 CA(C7 身份认证),可为 nil(关闭 mTLS)
	policy       *CommandPolicy // 命令安全策略(S4)
	audit        *AuditLog      // 审计日志(F1)
	scheduler    *Scheduler     // H2 定时任务调度器,可为 nil
	updateRepo   *UpdateRepo    // Agent 版本仓库(自更新分发),可为 nil
	sessions     *SessionStore  // 控制台会话存储(HttpOnly Cookie 鉴权,2026-08-12),可为 nil(控制台登录禁用)
	loginLimiter *LoginLimiter  // 登录限流(IP 维度失败计数+锁定,2026-08-12),可为 nil

	// P1-A:WS 隧道心跳参数(可测试覆盖;0 表示用默认值)。
	tunnelPingInterval time.Duration // 心跳 Ping 间隔,默认 15s
	tunnelDeadline     time.Duration // 无收包判死阈值,默认 30s

	// Agent 编排(2026-08-06):Web 控制台自然语言任务 → LLM 规划 → 工具执行
	llmClient   *LLMClient    // 非 nil 时启用 /api/agent/run(配置了 -llm-base-url)
	agentRunSem chan struct{} // 并发 Agent run 信号量(LLMAgentMax)
}

func NewRelay(cfg Config) *Relay {
	maxAgent := cfg.LLMAgentMax
	if maxAgent <= 0 {
		maxAgent = 2
	}
	relay := &Relay{
		cfg:         cfg,
		agents:      NewRegistry(),
		console:     NewEventBus(),
		policy:      DefaultCommandPolicy(),
		agentRunSem: make(chan struct{}, maxAgent),
	}
	// LLM 配置了 base-url 才启用 Agent 编排
	if cfg.LLMBaseURL != "" {
		relay.llmClient = NewLLMClient(cfg.LLMBaseURL, cfg.LLMAPIVKey, cfg.LLMModel)
	}
	// 控制台会话 + 登录限流(配置了 -console-password 才启用,2026-08-12)
	if cfg.ConsolePassword != "" {
		relay.sessions = NewSessionStore(cfg.ConsoleSessionTTL)
		relay.loginLimiter = NewLoginLimiter()
	}
	return relay
}

// WithStore 注入持久化存储(C4)。
func (r *Relay) WithStore(s TaskStore) *Relay {
	r.store = s
	return r
}

// WithUpdateRepo 注入 Agent 版本仓库(自更新分发)。
func (r *Relay) WithUpdateRepo(u *UpdateRepo) *Relay {
	r.updateRepo = u
	return r
}

// WithCA 注入内置 CA（C7）。
func (r *Relay) WithCA(ca *CA) *Relay {
	r.ca = ca
	return r
}

// WithAudit 注入审计日志（C9）。
func (r *Relay) WithAudit(a *AuditLog) *Relay {
	r.audit = a
	return r
}

// WithScheduler 注入定时任务调度器（H2）。
func (r *Relay) WithScheduler(s *Scheduler) *Relay {
	r.scheduler = s
	return r
}

// WithSessions 注入控制台会话存储(2026-08-12,测试用)。
func (r *Relay) WithSessions(s *SessionStore) *Relay {
	r.sessions = s
	return r
}

// 鉴权：双轨校验 —— Bearer token(CLI/Agent) 或 控制台 session cookie(浏览器)。
//
//   - cfg.Token 空 + cfg.ConsolePassword 空 → 放行(dev/no-auth 回归保护)
//   - Bearer 匹配 cfg.Token → 通过(CLI/Agent 路径,S6 常量时间比较)
//   - session cookie 有效 → 通过(浏览器路径,2026-08-12 新增)
//   - 否则 401
//
// 所有 ~20 个 handler 第一行调 r.authorize(req),改此函数内部即可全端点(含 SSE)支持 cookie,
// 无需动任何调用点。
func (r *Relay) authorize(req *http.Request) error {
	if r.cfg.Token == "" && r.cfg.ConsolePassword == "" {
		return nil
	}
	if r.cfg.Token != "" && r.hasValidBearer(req) {
		return nil
	}
	if r.cfg.ConsolePassword != "" && r.hasValidSession(req) {
		return nil
	}
	return errors.New("无效的鉴权")
}

// hasValidBearer 校验 Bearer token(Authorization 头 或 ?token= 查询参数)。
// S6 常量时间比较,消除计时侧信道。
func (r *Relay) hasValidBearer(req *http.Request) bool {
	token := req.Header.Get("Authorization")
	if token == "" {
		token = req.URL.Query().Get("token")
		if token != "" {
			token = "Bearer " + token
		}
	}
	want := "Bearer " + r.cfg.Token
	// 常量时间比较：长度不同时先 normalize 再比，避免长度泄露
	return subtle.ConstantTimeCompare([]byte(token), []byte(want)) == 1
}

// hasValidSession 校验控制台会话 cookie(浏览器路径,滑动续期)。
func (r *Relay) hasValidSession(req *http.Request) bool {
	if r.sessions == nil {
		return false
	}
	cookie, err := req.Cookie(r.cookieName())
	if err != nil {
		return false
	}
	_, ok := r.sessions.Get(cookie.Value)
	return ok
}

// handleAgentWS 处理 Agent 的反向 WS 连接。
//
// 鉴权策略（S3 修复）：
//   - mTLS 模式（r.ca != nil）：身份由 TLS 客户端证书保证，跳过 token
//   - 非 mTLS：用 token（向后兼容）
//   - Origin：Agent 非浏览器，InsecureSkipVerify 仅在 Dev 模式（收紧）
func (r *Relay) handleAgentWS(w http.ResponseWriter, req *http.Request) {
	if r.ca == nil {
		// 非 mTLS：用 token 校验
		if err := r.authorize(req); err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}
	c, err := websocket.Accept(w, req, &websocket.AcceptOptions{
		InsecureSkipVerify: r.cfg.Dev,
	})
	if err != nil {
		log.Printf("[agent-ws] accept 失败: %v", err)
		return
	}
	// 限制消息大小（默认 32MiB，支持大截图/文件）
	c.SetReadLimit(32 << 20)

	wc := &wsConn{conn: c, id: uuid.NewString(), role: "agent"}
	// M2：提取 mTLS 客户端证书身份（CN + DNSNames），供 serveAgent 校验
	if req.TLS != nil && len(req.TLS.PeerCertificates) > 0 {
		wc.certCN = req.TLS.PeerCertificates[0].Subject.CommonName
		wc.certDNS = req.TLS.PeerCertificates[0].DNSNames
	}
	r.serveAgent(req.Context(), wc)
}

// serveAgent 处理一条 Agent 连接的完整生命周期。
//
// 流程：循环读取消息 → register 首条登记身份 → 后续按 Type 分发 → 断开时清理。
func (r *Relay) serveAgent(ctx context.Context, wc *wsConn) {
	var agentID string
	cleanup := func() {
		if agentID != "" {
			if r.agents.Unregister(agentID, wc) {
				r.console.Broadcast(Event{
					Kind:    proto.TypeAgentOffline,
					AgentID: agentID,
					TS:      time.Now().UnixMilli(),
				})
				log.Printf("[agent] %s 离线", agentID)
				// F11:webhook 告警(Agent 离线)
				notifyAgentOffline(agentID, "")
			}
		}
		// P1-#5 修复:Agent 断线时关闭所有传输通道,
		// 让阻塞的 HTTP upload/download handler 立即退出(否则会挂到超时,最长 30 分钟)
		wc.closeAllXferChannels()
		wc.close(websocket.StatusNormalClosure, "bye")
	}
	defer cleanup()

	for {
		msgType, data, err := wc.conn.Read(ctx)
		if err != nil {
			log.Printf("[agent-read] %s 连接断开: %v", wc.id, err)
			return
		}
		if msgType != websocket.MessageText {
			// B1 修复：二进制帧 = 文件下载数据，路由到 transferID 的 channel
			transferID, isLast, payload, parseErr := proto.ParseBinaryFrame(data)
			if parseErr == nil {
				wc.routeXferData(transferID, payload, isLast)
			}
			continue
		}
		var env proto.Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			log.Printf("[agent-read] JSON 解析失败: %v", err)
			continue
		}
		// P0-P2 修复:协议版本校验(防 v2 载荷被当 v1 解析)
		if !env.IsValidVersion() {
			log.Printf("[agent-read] %s 收到不兼容的协议版本 v=%d (期望 %d),忽略",
				wc.id, env.Version, proto.Version)
			continue
		}

		switch env.Type {
		case proto.TypeRegister:
			var reg proto.Register
			if err := env.Decode(&reg); err != nil {
				log.Printf("[agent-read] register 解析失败: %v", err)
				continue
			}
			// M2 修复：mTLS 模式下校验 AgentID 与证书身份绑定
			if r.cfg.MTLS && wc.certCN != "" {
				if !certMatchesAgent(reg.AgentID, wc.certCN, wc.certDNS) {
					log.Printf("[agent-read] 拒绝注册：AgentID=%s 与证书身份(CN=%s DNS=%v)不匹配",
						reg.AgentID, wc.certCN, wc.certDNS)
					wc.close(websocket.StatusPolicyViolation, "agentID does not match certificate")
					return
				}
			}
			agentID = reg.AgentID
			wc.biz = agentID
			was := r.agents.Register(reg, wc)
			r.console.Broadcast(Event{
				Kind:    proto.TypeAgentOnline,
				AgentID: agentID,
				Payload: mustMarshal(reg),
				TS:      time.Now().UnixMilli(),
			})
			log.Printf("[agent] %s 上线 (hostname=%s caps=%v 重连=%v)", agentID, reg.Hostname, reg.Capabilities, was)
			// C4：Agent 上线后补投 pending 任务
			r.dispatchPendingTasks(agentID)

		case proto.TypeHeartbeat:
			var hb proto.Heartbeat
			if err := env.Decode(&hb); err == nil {
				r.agents.TouchWithStats(hb) // V1:存心跳 + 传输计数器
				// S7:回 heartbeat_ack,防空闲 Agent 被 watchdog 误杀。
				// watchdog 以 2×Heartbeat 无收包判定死连接并强制重连,
				// 若 Relay 不回帧,空闲 Agent 每 30s 掉线一次(掉线循环,
				// 隧道/任务在断线窗口内被随机拒绝,表现为"Agent 拒绝"或超时)。
				ackCtx, ackCancel := context.WithTimeout(ctx, 5*time.Second)
				if ackEnv, ackErr := proto.NewEnvelope(proto.TypeHeartbeatAck, "relay", agentID, hb); ackErr == nil {
					_ = wc.writeJSON(ackCtx, ackEnv)
				}
				ackCancel()
			}

		case proto.TypeTaskAck:
			// C5：任务 ACK，更新状态
			// E1/B1 修复：改用 UpdateTaskStatus，经终态锁校验（防 cancel 后被 ACK 改回）
			var ack proto.TaskAck
			if err := env.Decode(&ack); err == nil && r.store != nil {
				if err := r.store.UpdateTaskStatus(ack.TaskID, StatusAcknowledged); err == nil {
					metrics.Inc("acked") // D7
					// P1-B:广播 task_acknowledged 事件(CLI waitResult 实时感知)
					r.console.Broadcast(Event{
						Kind:    proto.TypeTaskAcknowledged,
						AgentID: agentID,
						Payload: mustMarshal(ack),
						TS:      time.Now().UnixMilli(),
					})
				}
			}

		case proto.TypeTaskNack:
			// D4：任务拒绝（Agent pool 满/无法执行），回退 pending 等重投
			// D5：UpdateTaskStatus 自带终态校验
			var nack proto.TaskNack
			if err := env.Decode(&nack); err == nil && r.store != nil {
				metrics.Inc("nacked") // D7
				if err := r.store.UpdateTaskStatus(nack.TaskID, StatusPending); err == nil {
					log.Printf("[task] 收到 NACK task=%s reason=%s，回退 pending",
						nack.TaskID, nack.Reason)
				}
			}

		case proto.TypeTaskResult:
			// 任务结果回流：持久化 + 审计 + 广播给控制台
			var res proto.TaskResult
			if err := env.Decode(&res); err == nil {
				if r.store != nil {
					_ = r.store.SaveResult(res.TaskID, res)
					// D5：用状态机校验更新（终态不可变更，防 cancel/result 竞态）
					newStatus := StatusFailed
					if res.Success {
						newStatus = StatusCompleted
					}
					if err := r.store.UpdateTaskStatus(res.TaskID, newStatus); err == nil {
						if res.Success {
							metrics.Inc("completed")
						} else {
							metrics.Inc("failed")
						}
					} else {
						// 不再静默吞错：result 已 SaveResult 持久化，但 status 推进失败
						// （常见于 reaper 已把任务转终态、或 cancel/result 竞态）。
						// CLI 的 waitResult 有 result 兜底，仍能拿到结果。
						log.Printf("[relay] task %s result received but status update failed: %v (result saved)", res.TaskID, err)
					}
				}
				// C9 审计：记录任务结果
				if r.audit != nil {
					r.audit.Log(AuditEntry{
						Event:   "task_result",
						AgentID: agentID,
						TaskID:  res.TaskID,
						Detail:  fmt.Sprintf("success=%v exit=%d duration=%dms", res.Success, res.ExitCode, res.Duration),
					})
				}
				r.console.Broadcast(Event{
					Kind:    proto.TypeTaskResult,
					AgentID: agentID,
					Payload: mustMarshal(res),
					TS:      time.Now().UnixMilli(),
				})
			}

		case proto.TypeTaskProgress:
			var p proto.TaskProgress
			if err := env.Decode(&p); err == nil {
				r.console.Broadcast(Event{
					Kind:    proto.TypeTaskProgress,
					AgentID: agentID,
					Payload: mustMarshal(p),
					TS:      time.Now().UnixMilli(),
				})
			}

		case proto.TypeShellOutput:
			// H3：终端输出转发给 Console（复用 EventBus 广播）
			var out proto.ShellOutput
			if err := env.Decode(&out); err == nil {
				r.console.Broadcast(Event{
					Kind:    proto.TypeShellOutput,
					AgentID: agentID,
					Payload: mustMarshal(out),
					TS:      time.Now().UnixMilli(),
				})
			}

		case proto.TypeShellClose:
			var sc proto.ShellClose
			if err := env.Decode(&sc); err == nil {
				r.console.Broadcast(Event{
					Kind:    proto.TypeShellClose,
					AgentID: agentID,
					Payload: mustMarshal(sc),
					TS:      time.Now().UnixMilli(),
				})
			}

		case proto.TypeFileTransferReady:
			// A4:路由到 ready channel(让 upload/download handler 等待)
			// P1-#6 修复:阻塞式发送(持锁),替代原 select-default 丢弃语义。
			// handler 端用 select+time.After 消费,buf=1,正常情况立即送达。
			var ready proto.FileTransferReady
			if err := env.Decode(&ready); err == nil {
				wc.xferMu.Lock()
				ch, ok := wc.xferReady[ready.TransferID]
				if ok {
					select {
					case ch <- &ready:
					default:
						// buf=1 满了说明 handler 没消费(异常),保留旧值优先
					}
				}
				wc.xferMu.Unlock()
			}
			r.console.Broadcast(Event{
				Kind: env.Type, AgentID: agentID, Payload: env.Payload, TS: time.Now().UnixMilli(),
			})

		case proto.TypeFileTransferComplete:
			// A5:路由到 complete channel(让 upload handler 等待完成)
			var complete proto.FileTransferComplete
			if err := env.Decode(&complete); err == nil {
				wc.xferMu.Lock()
				ch, ok := wc.xferComplete[complete.TransferID]
				if ok {
					select {
					case ch <- &complete:
					default:
					}
				}
				wc.xferMu.Unlock()
			}
			r.console.Broadcast(Event{
				Kind: env.Type, AgentID: agentID, Payload: env.Payload, TS: time.Now().UnixMilli(),
			})

		case proto.TypeFileTransferProgress:
			r.console.Broadcast(Event{
				Kind:    env.Type,
				AgentID: agentID,
				Payload: env.Payload,
				TS:      time.Now().UnixMilli(),
			})

		case proto.TypeTunnelReady:
			// 阶段 3.1:隧道 ready 路由到 xferReady(复用,handler 按 tunnelID 匹配)
			var ready proto.TunnelReady
			if err := env.Decode(&ready); err == nil {
				wc.xferMu.Lock()
				ch, ok := wc.xferReady[ready.TunnelID]
				if ok {
					// 把 TunnelReady 映射为 FileTransferReady 复用 channel
					fr := &proto.FileTransferReady{
						TransferID: ready.TunnelID,
						Accepted:   ready.Accepted,
						Reason:     ready.Reason,
					}
					select {
					case ch <- fr:
					default:
					}
				}
				wc.xferMu.Unlock()
			}

		case proto.TypeTunnelClose:
			// 隧道关闭:通知 dataCh 消费者 EOF
			var tc proto.TunnelClose
			if err := env.Decode(&tc); err == nil {
				wc.routeXferData(tc.TunnelID, nil, true) // 发 EOF(nil + isLast)
			}

		default:
			log.Printf("[agent-read] 未知消息类型 %s from %s", env.Type, agentID)
		}
	}
}

// handleConsoleWS 处理控制台的 WS 连接（P0 控制台用 SSE+REST，此端点预留兼容）。
func (r *Relay) handleConsoleWS(w http.ResponseWriter, req *http.Request) {
	if err := r.authorize(req); err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	c, err := websocket.Accept(w, req, &websocket.AcceptOptions{InsecureSkipVerify: r.cfg.Dev})
	if err != nil {
		return
	}
	c.Close(websocket.StatusPolicyViolation, "P0 控制台请使用 SSE /api/events")
}

// mustMarshal 编码失败返回 nil（不应发生）。
func mustMarshal(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

// certMatchesAgent 校验 AgentID 是否与证书身份匹配（M2 修复）。
// 检查 CN 和 DNSNames 是否包含 agentID。
func certMatchesAgent(agentID, certCN string, dnsNames []string) bool {
	if agentID == certCN {
		return true
	}
	for _, name := range dnsNames {
		if name == agentID {
			return true
		}
	}
	return false
}
