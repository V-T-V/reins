package main

import (
	"context"
	"encoding/json"
	"time"

	"github.com/V-T-V/reins/proto"
)

// contextWithTimeout 封装 context.WithTimeout（便于测试替换 fake clock）。
func contextWithTimeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

// marshalEnvelopeBytes 把 payload 构造为 Envelope 并序列化字节（不发送）。
func marshalEnvelopeBytes(env proto.Envelope) ([]byte, error) {
	return json.Marshal(env)
}
