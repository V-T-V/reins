package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"github.com/QiuShichang/go-rmm/executors"
	"github.com/QiuShichang/go-rmm/proto"
)

// agentBizID 是当前 Agent 的业务身份（用于 Envelope.From）。
// 进程级变量，在 Run 启动时设置。
var agentBizID string

// Agent 主体：持有配置与 dispatcher，管理到 Relay 的反向连接。
type Agent struct {
	cfg  Config
	disp *executors.Dispatcher
}

func NewAgent(cfg Config, disp *executors.Dispatcher) *Agent {
	return &Agent{cfg: cfg, disp: disp}
}

// Run 启动 Agent 主循环：连接 Relay → 注册 → 心跳 + 接收任务。
//
// 阻塞直到 ctx 取消。断线自动指数退避重连（1s→2s→…→30s 封顶）。
func (a *Agent) Run(ctx context.Context) error {
	agentBizID = a.cfg.AgentID
	backoff := initialBackoff
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err := a.connectAndServe(ctx)
		if ctx.Err() != nil {
			return ctx.Err() // 主动退出，不重连
		}
		log.Printf("[agent] 连接断开: %v，%v 后重连", err, backoff)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		// 指数退避，封顶 30s
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// connectAndServe 建立一次连接的完整生命周期。
func (a *Agent) connectAndServe(ctx context.Context) error {
	header := http.Header{}
	if a.cfg.Token != "" {
		header.Set("Authorization", "Bearer "+a.cfg.Token)
	}

	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	c, _, err := websocket.Dial(dialCtx, a.cfg.RelayURL, &websocket.DialOptions{
		HTTPHeader: header,
	})
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

	// 1. 发送 register
	if err := a.sendRegister(connCtx, wc); err != nil {
		return err
	}
	// 2. 连接建立后补发队列中积压的结果（R3 修复核心）
	if resultQueue.len() > 0 {
		log.Printf("[agent] 连接恢复，补发 %d 个积压结果", resultQueue.len())
		resultQueue.flushTo(wc)
	}
	// 3. 启动心跳 goroutine
	go a.heartbeatLoop(connCtx, wc)
	// 4. 启动幂等缓存清理
	go idempotencySweepLoop(connCtx)
	// 5. 接收循环（阻塞直到断开）
	return a.recvLoop(connCtx, wc)
}

// sendRegister 发送注册消息。
func (a *Agent) sendRegister(ctx context.Context, wc *wsConn) error {
	reg := proto.Register{
		AgentID:      a.cfg.AgentID,
		Hostname:     hostname(),
		OS:           runtimeGOOS() + "/" + runtimeGOARCH(),
		Version:      version,
		Capabilities: a.disp.Types(),
		Tags:         a.cfg.Tags,
	}
	return wc.writeEnvelope(ctx, proto.TypeRegister, "relay", reg)
}

// heartbeatLoop 周期发送心跳。所有 Write 经 wc.writeJSON 串行化。
func (a *Agent) heartbeatLoop(ctx context.Context, wc *wsConn) {
	ticker := time.NewTicker(a.cfg.Heartbeat)
	defer ticker.Stop()
	var seq int64
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			seq++
			hb := proto.Heartbeat{AgentID: a.cfg.AgentID, Seq: seq}
			wctx, wcancel := context.WithTimeout(ctx, 5*time.Second)
			if err := wc.writeEnvelope(wctx, proto.TypeHeartbeat, "relay", hb); err != nil {
				log.Printf("[agent] 心跳发送失败: %v", err)
				wcancel()
				return // 心跳失败视为断线，退出触发重连
			}
			wcancel()
		}
	}
}

// recvLoop 接收并处理消息（task_request/task_cancel）。
func (a *Agent) recvLoop(ctx context.Context, wc *wsConn) error {
	for {
		_, data, err := wc.read(ctx)
		if err != nil {
			return err
		}
		var env proto.Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			log.Printf("[agent] 消息解析失败: %v", err)
			continue
		}
		switch env.Type {
		case proto.TypeTaskRequest:
			a.handleTask(ctx, wc, env)
		case proto.TypeTaskCancel:
			a.handleCancel(env)
		default:
			log.Printf("[agent] 忽略消息类型 %s", env.Type)
		}
	}
}

// handleTask 在独立 goroutine 中执行任务，结果回流给 Relay。
// 所有 WS 写入经 wc 串行化，消除 R1 并发损坏 bug。
//
// C5 增强：
//   - 收到任务立即发 task_ack（让 Relay 知道任务已抵达）
//   - 幂等检查（同 IdempotencyKey/TaskID 不重复执行，直接回放结果）
//   - 结果经 resultQueue 缓存，发送失败不丢失，重连后补发
func (a *Agent) handleTask(parent context.Context, wc *wsConn, env proto.Envelope) {
	var task proto.TaskRequest
	if err := env.Decode(&task); err != nil {
		log.Printf("[agent] 任务解析失败: %v", err)
		return
	}
	go func() {
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
		result := a.disp.Dispatch(ctx, task, progress)
		result.Duration = int(time.Since(start) / time.Millisecond)
		result.TaskID = task.TaskID

		// 6. 存入幂等缓存
		idempotencyCache.put(idemKey, result)

		// 7. 发送结果（经队列，失败重投）
		a.sendResult(parent, wc, result)
	}()
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
