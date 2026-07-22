package main

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/coder/websocket"

	"github.com/QiuShichang/go-rmm/proto"
)

// wsConn 对 WebSocket 连接的封装，串行化所有 Write 调用。
//
// 存在原因（修复 R1 P0 bug）：coder/websocket 的 Write 虽然内部对帧做了保护，
// 但业务层从多个 goroutine（心跳、任务进度、任务结果）并发调用 Write 时，
// 帧仍可能交错损坏。此封装用 writeMu 串行化业务写入。
//
// 同时封装 Envelope 的构造与序列化，避免业务代码重复样板。
type wsConn struct {
	conn    *websocket.Conn
	writeMu sync.Mutex
}

func newWSConn(c *websocket.Conn) *wsConn { return &wsConn{conn: c} }

// writeJSON 把 v 序列化为 JSON 并串行化写入。
func (w *wsConn) writeJSON(ctx context.Context, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	return w.conn.Write(ctx, websocket.MessageText, data)
}

// writeEnvelope 构造 Envelope 并写入（参数同 proto.NewEnvelope）。
func (w *wsConn) writeEnvelope(ctx context.Context, typ, to string, payload any) error {
	env, err := proto.NewEnvelope(typ, agentBizID, to, payload)
	if err != nil {
		return err
	}
	return w.writeJSON(ctx, env)
}

// writeBytes 串行化写入原始字节（已序列化的消息，避免重复 marshal）。
func (w *wsConn) writeBytes(ctx context.Context, data []byte) error {
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	return w.conn.Write(ctx, websocket.MessageText, data)
}

// marshalEnvelope 静态构造 Envelope 字节（不写连接），供调用方按需批量处理。
func marshalEnvelope(typ, to string, payload any) ([]byte, error) {
	env, err := proto.NewEnvelope(typ, agentBizID, to, payload)
	if err != nil {
		return nil, err
	}
	return json.Marshal(env)
}

// read 透传到底层 conn。
func (w *wsConn) read(ctx context.Context) (websocket.MessageType, []byte, error) {
	return w.conn.Read(ctx)
}

// closeNow 透传。
func (w *wsConn) closeNow() error { return w.conn.CloseNow() }

// close 透传。
func (w *wsConn) close(code websocket.StatusCode, reason string) error {
	return w.conn.Close(code, reason)
}
