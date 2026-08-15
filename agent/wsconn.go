package main

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/QiuShichang/go-rmm/internal/wsx"
	"github.com/QiuShichang/go-rmm/proto"
)

// wsConn 对 WebSocket 连接的封装:优先级写引擎(共享 internal/wsx,架构短3)+
// Agent 侧业务字段(lastRecv 供 watchdog 半开检测)。
//
// 弱网 A1:P0(心跳)不被 P1(文件/隧道/结果)饿死;语义见 internal/wsx 包注释。
type wsConn struct {
	*wsx.Writer
	lastRecv atomic.Int64 // monotonic 毫秒时间戳(read 成功时更新)
}

func newWSConn(c *websocket.Conn) *wsConn {
	w := &wsConn{Writer: wsx.New(c)}
	w.lastRecv.Store(time.Now().UnixMilli())
	return w
}

// startWriter 启动优先级 writer(兼容名,转发共享引擎)。
func (w *wsConn) startWriter(ctx context.Context) { w.Writer.Start(ctx) }

// writeControlEnvelope 构造 Envelope + 投 P0(心跳/ack 高优先)。
func (w *wsConn) writeControlEnvelope(ctx context.Context, typ, to string, payload any) error {
	data, err := marshalEnvelope(typ, to, payload)
	if err != nil {
		return err
	}
	return w.WriteControl(ctx, websocket.MessageText, data)
}

func (w *wsConn) writeJSON(ctx context.Context, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return w.WriteData(ctx, websocket.MessageText, data)
}

func (w *wsConn) writeEnvelope(ctx context.Context, typ, to string, payload any) error {
	data, err := marshalEnvelope(typ, to, payload)
	if err != nil {
		return err
	}
	return w.WriteData(ctx, websocket.MessageText, data)
}

func (w *wsConn) writeBytes(ctx context.Context, data []byte) error {
	return w.WriteData(ctx, websocket.MessageText, data)
}

func (w *wsConn) writeBinaryData(ctx context.Context, data []byte) error {
	return w.WriteData(ctx, websocket.MessageBinary, data)
}

// marshalEnvelope 静态构造 Envelope 字节(不写连接)。
func marshalEnvelope(typ, to string, payload any) ([]byte, error) {
	env, err := proto.NewEnvelope(typ, agentBizID, to, payload)
	if err != nil {
		return nil, err
	}
	return json.Marshal(env)
}

// read 透传,成功时更新 lastRecv。
func (w *wsConn) read(ctx context.Context) (websocket.MessageType, []byte, error) {
	mt, data, err := w.Conn().Read(ctx)
	if err == nil {
		w.lastRecv.Store(time.Now().UnixMilli())
	}
	return mt, data, err
}

func (w *wsConn) lastRecvMs() int64 { return w.lastRecv.Load() }
func (w *wsConn) closeNow() error   { return w.Conn().CloseNow() }
func (w *wsConn) close(code websocket.StatusCode, reason string) error {
	return w.Conn().Close(code, reason)
}
