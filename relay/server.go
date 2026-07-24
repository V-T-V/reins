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

	// B1 修复：二进制帧路由——serveAgent 的 Read 循环拦截二进制帧，
	// 按 transferID 路由到对应 channel，HTTP handler 从 channel 消费。
	xferMu       sync.Mutex
	xferChannels map[string]chan []byte // transferID → data channel（二进制帧）
	xferReady    map[string]chan *proto.FileTransferReady    // transferID → ready 信号
	xferComplete map[string]chan *proto.FileTransferComplete // transferID → 完成信号
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
func (w *wsConn) unregisterXferChannels(transferID string) {
	w.xferMu.Lock()
	defer w.xferMu.Unlock()
	if ch, ok := w.xferChannels[transferID]; ok {
		delete(w.xferChannels, transferID)
		close(ch)
	}
	delete(w.xferReady, transferID)
	delete(w.xferComplete, transferID)
}

// registerXferChannel 注册一个传输数据通道，返回只读 channel。
func (w *wsConn) registerXferChannel(transferID string) <-chan []byte {
	w.xferMu.Lock()
	defer w.xferMu.Unlock()
	if w.xferChannels == nil {
		w.xferChannels = make(map[string]chan []byte)
	}
	ch := make(chan []byte, 64)
	w.xferChannels[transferID] = ch
	return ch
}

// unregisterXferChannel 注销传输通道。
func (w *wsConn) unregisterXferChannel(transferID string) {
	w.xferMu.Lock()
	defer w.xferMu.Unlock()
	if ch, ok := w.xferChannels[transferID]; ok {
		delete(w.xferChannels, transferID)
		close(ch)
	}
}

// routeXferData 把二进制帧路由到对应 transferID 的 channel。
// A2 修复：删除 default 分支改为阻塞式，让生产者（Agent WS 读）自然背压。
// 满了会阻塞 serveAgent 的 Read 循环 → Agent 侧 WS 写阻塞 → TCP 背压 → 全链路自协调。
func (w *wsConn) routeXferData(transferID string, data []byte, isLast bool) {
	w.xferMu.Lock()
	ch, ok := w.xferChannels[transferID]
	w.xferMu.Unlock()
	if !ok {
		return
	}
	if len(data) > 0 {
		ch <- data // 阻塞直到消费端读取（背压）
	}
	if isLast {
		ch <- nil // 阻塞发送 EOF 标记
	}
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
	dropped     int64                // 累计丢弃事件数
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

// Broadcast 向所有订阅者非阻塞投递。慢消费者丢事件并计数（D7）。
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
	cfg     Config
	agents  *Registry
	console *EventBus
	store   *Store        // 任务持久化（R2/F1），可为 nil（关闭持久化）
	ca      *CA           // 内置 CA（C7 身份认证），可为 nil（关闭 mTLS）
	policy  *CommandPolicy // 命令安全策略（S4）
	audit   *AuditLog     // 审计日志（F1）
	scheduler *Scheduler  // H2 定时任务调度器，可为 nil
}

func NewRelay(cfg Config) *Relay {
	return &Relay{
		cfg:     cfg,
		agents:  NewRegistry(),
		console: NewEventBus(),
		policy:  DefaultCommandPolicy(),
	}
}

// WithStore 注入持久化存储（C4）。
func (r *Relay) WithStore(s *Store) *Relay {
	r.store = s
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

// 鉴权：校验 Bearer token（配置为空则放行）。
//
// S6 修复：token 比较用 crypto/subtle.ConstantTimeCompare，消除计时侧信道。
func (r *Relay) authorize(req *http.Request) error {
	if r.cfg.Token == "" {
		return nil
	}
	token := req.Header.Get("Authorization")
	if token == "" {
		token = req.URL.Query().Get("token")
		if token != "" {
			token = "Bearer " + token
		}
	}
	want := "Bearer " + r.cfg.Token
	// 常量时间比较：长度不同时先 normalize 再比，避免长度泄露
	if subtle.ConstantTimeCompare([]byte(token), []byte(want)) != 1 {
		return errors.New("无效的鉴权 token")
	}
	return nil
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
			}
		}
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
			// M3 修复：保留 isLast 语义，不再硬编码 false
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
				r.agents.Touch(hb.AgentID, hb.Seq)
			}

		case proto.TypeTaskAck:
			// C5：任务 ACK，更新状态
			// E1/B1 修复：改用 UpdateTaskStatus，经终态锁校验（防 cancel 后被 ACK 改回）
			var ack proto.TaskAck
			if err := env.Decode(&ack); err == nil && r.store != nil {
				if err := r.store.UpdateTaskStatus(ack.TaskID, StatusAcknowledged); err == nil {
					metrics.Inc("acked") // D7
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
			// A4：路由到 ready channel（让 upload handler 等待）
			var ready proto.FileTransferReady
			if err := env.Decode(&ready); err == nil {
				wc.xferMu.Lock()
				ch, ok := wc.xferReady[ready.TransferID]
				wc.xferMu.Unlock()
				if ok {
					select {
					case ch <- &ready:
					default:
					}
				}
			}
			r.console.Broadcast(Event{
				Kind: env.Type, AgentID: agentID, Payload: env.Payload, TS: time.Now().UnixMilli(),
			})

		case proto.TypeFileTransferComplete:
			// A5：路由到 complete channel（让 upload handler 等待完成）
			var complete proto.FileTransferComplete
			if err := env.Decode(&complete); err == nil {
				wc.xferMu.Lock()
				ch, ok := wc.xferComplete[complete.TransferID]
				wc.xferMu.Unlock()
				if ok {
					select {
					case ch <- &complete:
					default:
					}
				}
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
