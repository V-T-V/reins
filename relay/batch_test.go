package main

import (
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestBatchWriterSend(t *testing.T) {
	bw := &batchWriter{
		ch:   make(chan writeItem, 4),
		done: make(chan struct{}),
	}
	for i := 0; i < 4; i++ {
		if !bw.send(websocket.MessageText, []byte("msg")) {
			t.Error("send 应成功")
		}
	}
	if bw.send(websocket.MessageText, []byte("overflow")) {
		t.Error("队列满应返回 false")
	}
}

func TestBatchWriterClose(t *testing.T) {
	bw := newBatchWriter(nil, 4)
	close(bw.ch)
}

func TestBufferPoolRoundTrip(t *testing.T) {
	buf := Get256K()
	if cap(buf) != 256*1024 {
		t.Errorf("cap = %d", cap(buf))
	}
	Put256K(buf)

	buf32 := Get32K()
	if cap(buf32) != 32*1024 {
		t.Errorf("cap = %d", cap(buf32))
	}
	Put32K(buf32)
}

func TestBufferPool4K(t *testing.T) {
	buf := Get4K()
	if cap(buf) != 4096 {
		t.Errorf("cap = %d", cap(buf))
	}
	Put4K(buf)
}

var _ = time.Second
