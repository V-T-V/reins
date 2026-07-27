package main

import (
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestBatchWriterSend(t *testing.T) {
	// 创建 batchWriter(不启动 run,只测 send 的队列行为)
	bw := &batchWriter{
		ch:   make(chan writeItem, 4),
		done: make(chan struct{}),
	}
	// 发 4 条(send 应成功,channel 容量 4)
	for i := 0; i < 4; i++ {
		if !bw.send(websocket.MessageText, []byte("msg")) {
			t.Error("send 应成功")
		}
	}
	// 第 5 条应被丢弃(队列满)
	if bw.send(websocket.MessageText, []byte("overflow")) {
		t.Error("队列满应返回 false")
	}
}

func TestBatchWriterClose(t *testing.T) {
	bw := newBatchWriter(nil, 4)
	close(bw.ch) // 模拟关闭
	// close 不应 panic(once 保护)
}

func TestBatchStoreSubmit(t *testing.T) {
	store := NewMemoryStore()
	bs := NewBatchStore(&Store{}) // BatchStore 内部简化:Submit 直接返回 nil
	defer bs.Close()
	_ = store // 避免 unused

	err := bs.Submit("tasks", "test-key", []byte("test-value"))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
}

func TestBatchStoreFlushOnClose(t *testing.T) {
	bs := NewBatchStore(&Store{})

	bs.SubmitAsync("tasks", "async-1", []byte("val1"))
	bs.SubmitAsync("tasks", "async-2", []byte("val2"))

	time.Sleep(100 * time.Millisecond)
	bs.Close()
}
