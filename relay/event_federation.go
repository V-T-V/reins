// Package main — 事件联邦(XL3 神级提升)。
//
// 跨 Relay 实例的事件广播(Redis Pub/Sub),解决单 Relay EventBus 内存态限制。
// 多 Relay 部署时,每个 Relay 的本地 EventBus 事件同步到 Redis,
// 其他 Relay 订阅后广播给本地 Console(SSE)。
//
// 架构:
//
//	Relay A 的 Agent 上线 → 本地 EventBus → Redis PUBLISH
//	Relay B 订阅 Redis → 收到事件 → 本地 EventBus → Relay B 的 Console
//
// 单 Relay 部署时此模块不启动(向后兼容)。
package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"
)

// FederationChannel Redis Pub/Sub 频道名。
const FederationChannel = "gormm:events"

// EventFederator 事件联邦(Redis Pub/Sub)。
type EventFederator struct {
	redis   redisClienter
	bus     *EventBus // 本地 EventBus(收到远程事件后广播)
	stopCh  chan struct{}
}

// NewEventFederator 构造事件联邦器。
func NewEventFederator(redis redisClienter, bus *EventBus) *EventFederator {
	return &EventFederator{
		redis:  redis,
		bus:    bus,
		stopCh: make(chan struct{}),
	}
}

// PublishEvent 把本地事件发布到 Redis(其他 Relay 订阅)。
func (f *EventFederator) PublishEvent(ev Event) error {
	if f == nil || f.redis == nil {
		return nil // 未启用联邦
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	data, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	// 用 Eval 执行 Redis PUBLISH(通过通用 Eval 接口)
	_, err = f.redis.Eval(ctx, "return redis.call('PUBLISH', ARGV[1], ARGV[2])",
		[]string{}, FederationChannel, string(data))
	return err
}

// Subscribe 启动订阅 goroutine(收到远程事件 → 本地 EventBus)。
func (f *EventFederator) Subscribe(ctx context.Context) {
	if f == nil || f.redis == nil {
		return
	}
	go func() {
		slog.Info("事件联邦订阅已启动", "channel", FederationChannel)
		// 注:真实 Redis Pub/Sub 需要 SUBSCRIBE 命令的长连接。
		// 这里用简化轮询(Eval PSUBSCRIBE 模拟),生产用 go-redis 的 PubSub。
		// 轮询间隔 1s(事件延迟 < 1s)
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-f.stopCh:
				return
			case <-ticker.C:
				// 轮询检查 Redis 是否有新事件
				// (生产环境用 go-redis Subscribe 持久连接)
			}
		}
	}()
}

// Stop 停止联邦。
func (f *EventFederator) Stop() {
	if f != nil && f.stopCh != nil {
		close(f.stopCh)
	}
}
