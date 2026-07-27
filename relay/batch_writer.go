// Package main — WS 写队列优化(O5:减少锁竞争 + 批量写)。
//
// 问题:writeMu 是粗粒度全局锁,多 goroutine(心跳+任务结果+进度+shell输出)
// 并发写同一 WS 连接时,每次都 Lock/Unlock,锁竞争严重。
//
// 优化:改为单写 goroutine + channel 队列模式。
//   - 所有写操作投递到 channel(无锁)
//   - 单 goroutine 串行消费(天然串行化,无锁竞争)
//   - coder/websocket 的 Write 在单 goroutine 内调用,无需 writeMu
//
// 兼容:旧的 writeJSON/writeBytes 方法保留(writeMu 兜底),
// 新增 BatchWriter 供高性能路径(文件传输/隧道/shell)使用。
package main

import (
	"context"
	"sync"

	"github.com/coder/websocket"
)

// batchWriter 单 goroutine 写队列(减少锁竞争)。
//
// 用法:
//
//	bw := newBatchWriter(conn)
//	go bw.run(ctx)
//	bw.send(data)  // 非阻塞投递
type batchWriter struct {
	conn *websocket.Conn
	ch   chan writeItem
	done chan struct{}
	once sync.Once
}

type writeItem struct {
	msgType websocket.MessageType
	data    []byte
}

// newBatchWriter 构造批量写器。
// bufSize:channel 缓冲(默认 64)。
func newBatchWriter(conn *websocket.Conn, bufSize int) *batchWriter {
	if bufSize <= 0 {
		bufSize = 64
	}
	return &batchWriter{
		conn: conn,
		ch:   make(chan writeItem, bufSize),
		done: make(chan struct{}),
	}
}

// run 启动写 goroutine(阻塞直到 ctx 取消或 channel 关闭)。
func (bw *batchWriter) run(ctx context.Context) {
	defer close(bw.done)
	for {
		select {
		case <-ctx.Done():
			return
		case item, ok := <-bw.ch:
			if !ok {
				return
			}
			// 单 goroutine 写,无需 writeMu
			writeCtx, cancel := context.WithTimeout(ctx, 10*1000*1000*1000) // 10s
			bw.conn.Write(writeCtx, item.msgType, item.data)
			cancel()
		}
	}
}

// send 非阻塞投递(channel 满时丢弃 + 返回 false)。
func (bw *batchWriter) send(msgType websocket.MessageType, data []byte) bool {
	select {
	case bw.ch <- writeItem{msgType: msgType, data: data}:
		return true
	default:
		return false // 队列满,丢弃(背压)
	}
}

// sendBlocking 阻塞投递(等队列有空间)。
func (bw *batchWriter) sendBlocking(ctx context.Context, msgType websocket.MessageType, data []byte) error {
	select {
	case bw.ch <- writeItem{msgType: msgType, data: data}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// close 关闭写队列(等 run 退出)。
func (bw *batchWriter) close() {
	bw.once.Do(func() {
		close(bw.ch)
	})
	<-bw.done
}
