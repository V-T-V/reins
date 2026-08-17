// Package wsx WebSocket 优先级写引擎(单一真相源)。
//
// 架构短3:原 agent/wsconn.go 与 relay/server.go 各持一份逐字节相同的实现
// (writeItem/startWriter/writeControl/writeData),且已开始漂移。写路径的下一轮
// 修复必须记得改两处,漏一处就是只在某一端复现的幽灵 bug。现收敛为本包一份。
//
// 设计(弱网 A1/A2):
//   - P0(controlCh:心跳/ack)总先于 P1(dataCh:文件/隧道/结果)被消费 → 控制帧不被数据饿死
//   - 单 writer goroutine 写 → 无并发帧交错
//   - done channel 同步返回 Write error(保留"写失败检测"语义)
//   - ctx 取消即退出 writer;生产者在 ctx 超时后放弃等待,帧仍会随队列消费被写出
//     ("可能迟到但不会丢")
package wsx

import (
	"context"
	"sync"

	"github.com/coder/websocket"
)

// Item 一次写请求。
type Item struct {
	MsgType websocket.MessageType
	Data    []byte
	Done    chan error // 同步:writer goroutine 写完后回传 error
}

// Writer 优先级写引擎(P0 控制 / P1 数据)。
type Writer struct {
	conn      *websocket.Conn
	controlCh chan Item
	dataCh    chan Item
	once      sync.Once
}

// New 构造写引擎(control 缓冲 8 / data 缓冲 64)。必须在首次写之前 Start。
func New(conn *websocket.Conn) *Writer {
	return &Writer{
		conn:      conn,
		controlCh: make(chan Item, 8),
		dataCh:    make(chan Item, 64),
	}
}

// Conn 返回底层连接(调用方 read/close 用;写必须经 WriteControl/WriteData)。
func (w *Writer) Conn() *websocket.Conn { return w.conn }

// Start 启动单 writer goroutine(P0 优先;幂等)。
func (w *Writer) Start(ctx context.Context) {
	w.once.Do(func() {
		go func() {
			doWrite := func(item Item) {
				err := w.conn.Write(ctx, item.MsgType, item.Data)
				if item.Done != nil {
					item.Done <- err
				}
			}
			for {
				select {
				case item := <-w.controlCh:
					doWrite(item)
				case <-ctx.Done():
					return
				default:
					select {
					case item := <-w.controlCh:
						doWrite(item)
					case item := <-w.dataCh:
						doWrite(item)
					case <-ctx.Done():
						return
					}
				}
			}
		}()
	})
}

// WriteControl 投 P0(高优先:心跳/ack)+ 同步等 Write 结果。
func (w *Writer) WriteControl(ctx context.Context, msgType websocket.MessageType, data []byte) error {
	done := make(chan error, 1)
	select {
	case w.controlCh <- Item{msgType, data, done}:
		return waitDone(ctx, done)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// WriteData 投 P1(低优先:数据帧)+ 同步等 Write 结果。
func (w *Writer) WriteData(ctx context.Context, msgType websocket.MessageType, data []byte) error {
	done := make(chan error, 1)
	select {
	case w.dataCh <- Item{msgType, data, done}:
		return waitDone(ctx, done)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func waitDone(ctx context.Context, done chan error) error {
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
