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

// Agent 主体：持有配置与 dispatcher，管理到 Relay 的反向连接。
type Agent struct {
	cfg     Config
	disp    *executors.Dispatcher
}

func NewAgent(cfg Config, disp *executors.Dispatcher) *Agent {
	return &Agent{cfg: cfg, disp: disp}
}

// Run 启动 Agent 主循环：连接 Relay → 注册 → 心跳 + 接收任务。
//
// 阻塞直到 ctx 取消。断线自动指数退避重连（1s→2s→…→30s 封顶）。
func (a *Agent) Run(ctx context.Context) error {
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

	log.Printf("[agent] 已连接到 Relay %s", a.cfg.RelayURL)

	// 子 ctx：连接级取消（断线时让心跳 goroutine 退出）
	connCtx, connCancel := context.WithCancel(ctx)
	defer connCancel()

	// 1. 发送 register
	if err := a.sendRegister(connCtx, c); err != nil {
		return err
	}
	// 2. 启动心跳 goroutine
	go a.heartbeatLoop(connCtx, c)
	// 3. 接收循环（阻塞直到断开）
	return a.recvLoop(connCtx, c)
}

// sendRegister 发送注册消息。
func (a *Agent) sendRegister(ctx context.Context, c *websocket.Conn) error {
	reg := proto.Register{
		AgentID:      a.cfg.AgentID,
		Hostname:     hostname(),
		OS:           runtimeGOOS() + "/" + runtimeGOARCH(),
		Version:      version,
		Capabilities: a.disp.Types(),
		Tags:         a.cfg.Tags,
	}
	env, err := proto.NewEnvelope(proto.TypeRegister, a.cfg.AgentID, "relay", reg)
	if err != nil {
		return err
	}
	data, _ := json.Marshal(env)
	return c.Write(ctx, websocket.MessageText, data)
}

// heartbeatLoop 周期发送心跳。
func (a *Agent) heartbeatLoop(ctx context.Context, c *websocket.Conn) {
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
			env, _ := proto.NewEnvelope(proto.TypeHeartbeat, a.cfg.AgentID, "relay", hb)
			data, _ := json.Marshal(env)
			wctx, wcancel := context.WithTimeout(ctx, 5*time.Second)
			if err := c.Write(wctx, websocket.MessageText, data); err != nil {
				log.Printf("[agent] 心跳发送失败: %v", err)
				wcancel()
				return // 心跳失败视为断线，退出触发重连
			}
			wcancel()
		}
	}
}

// recvLoop 接收并处理消息（task_request）。
func (a *Agent) recvLoop(ctx context.Context, c *websocket.Conn) error {
	for {
		_, data, err := c.Read(ctx)
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
			a.handleTask(ctx, c, env)
		default:
			log.Printf("[agent] 忽略消息类型 %s", env.Type)
		}
	}
}

// handleTask 在独立 goroutine 中执行任务，结果回流给 Relay。
func (a *Agent) handleTask(parent context.Context, c *websocket.Conn, env proto.Envelope) {
	var task proto.TaskRequest
	if err := env.Decode(&task); err != nil {
		log.Printf("[agent] 任务解析失败: %v", err)
		return
	}
	go func() {
		log.Printf("[agent] 执行任务 %s type=%s", task.TaskID, task.Type)
		start := time.Now()

		// 任务超时 ctx
		timeout := proto.DefaultTaskTimeout
		if task.Timeout > 0 {
			timeout = task.Timeout
		}
		ctx, cancel := context.WithTimeout(parent, time.Duration(timeout)*time.Second)
		defer cancel()

		// 进度上报回调：通过 WS 发 task_progress
		progress := func(step, message string, percent int) {
			p := proto.TaskProgress{TaskID: task.TaskID, Step: step, Message: message, Percent: percent}
			pEnv, _ := proto.NewEnvelope(proto.TypeTaskProgress, a.cfg.AgentID, "relay", p)
			pData, _ := json.Marshal(pEnv)
			wctx, wcancel := context.WithTimeout(parent, 5*time.Second)
			defer wcancel()
			_ = c.Write(wctx, websocket.MessageText, pData)
		}

		result := a.disp.Dispatch(ctx, task, progress)
		result.Duration = int(time.Since(start) / time.Millisecond)
		result.TaskID = task.TaskID

		// 回流结果
		resEnv, _ := proto.NewEnvelope(proto.TypeTaskResult, a.cfg.AgentID, "relay", result)
		resData, _ := json.Marshal(resEnv)
		wctx, wcancel := context.WithTimeout(parent, 10*time.Second)
		defer wcancel()
		if err := c.Write(wctx, websocket.MessageText, resData); err != nil {
			log.Printf("[agent] 任务结果回流失败: %v", err)
		} else {
			log.Printf("[agent] 任务 %s 完成 success=%v duration=%dms", task.TaskID, result.Success, result.Duration)
		}
	}()
}

const (
	initialBackoff = 1 * time.Second
	maxBackoff     = 30 * time.Second
)
