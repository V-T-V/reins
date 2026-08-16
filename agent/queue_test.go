package main

import (
	"context"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/QiuShichang/go-rmm/proto"
)

// newQueue 构造一个独立 rQueue(不复用全局 resultQueue)。
func newQueue() *rQueue { return &rQueue{} }

func mkEnv(id string) proto.Envelope {
	env, _ := proto.NewEnvelope(proto.TypeTaskResult, "agent", "relay",
		proto.TaskResult{TaskID: id, Success: true})
	return env
}

// TestQueueFIFOOrder enqueue 顺序 = drain 顺序(FIFO)。
func TestQueueFIFOOrder(t *testing.T) {
	q := newQueue()
	for _, id := range []string{"t1", "t2", "t3"} {
		q.enqueue(mkEnv(id))
	}
	if q.len() != 3 {
		t.Fatalf("len=%d 期望 3", q.len())
	}
	out := q.drain()
	if len(out) != 3 {
		t.Fatalf("drain len=%d 期望 3", len(out))
	}
	for i, want := range []string{"t1", "t2", "t3"} {
		var tr proto.TaskResult
		if err := out[i].env.Decode(&tr); err != nil {
			t.Errorf("[%d] decode: %v", i, err)
			continue
		}
		if tr.TaskID != want {
			t.Errorf("[%d] TaskID=%q 期望 %q", i, tr.TaskID, want)
		}
	}
	// drain 后清空
	if q.len() != 0 {
		t.Fatalf("drain 后 len=%d 期望 0", q.len())
	}
}

// TestQueuePeekDoesNotMutate peek 不出队。
func TestQueuePeekDoesNotMutate(t *testing.T) {
	q := newQueue()
	q.enqueue(mkEnv("a"))
	q.enqueue(mkEnv("b"))
	before := q.len()
	out := q.peek()
	if q.len() != before {
		t.Fatalf("peek 改变了 len: %d→%d", before, q.len())
	}
	if len(out) != before {
		t.Fatalf("peek 返回 %d 期望 %d", len(out), before)
	}
}

// TestQueueRequeueIncrementsAttempt requeue 后 attempt 递增,且结果回到队列。
func TestQueueRequeueIncrementsAttempt(t *testing.T) {
	q := newQueue()
	q.enqueue(mkEnv("x"))
	items := q.drain()
	if items[0].attempt != 0 {
		t.Fatalf("初始 attempt=%d 期望 0", items[0].attempt)
	}
	q.requeue(items)
	if q.len() != 1 {
		t.Fatalf("requeue 后 len=%d 期望 1", q.len())
	}
	again := q.drain()
	if again[0].attempt != 1 {
		t.Fatalf("requeue 后 attempt=%d 期望 1", again[0].attempt)
	}
}

// TestQueuePopHeadEmptyNoPanic 空队 popHead 不 panic,返回零值。
func TestQueuePopHeadEmptyNoPanic(t *testing.T) {
	q := newQueue()
	item := q.popHead()
	if item.env.ID != "" {
		t.Fatalf("空队 popHead 应返回零值, got ID=%q", item.env.ID)
	}
}

// TestQueuePeekHeadEmptyFalse 空队 peekHead 返回 false。
func TestQueuePeekHeadEmptyFalse(t *testing.T) {
	q := newQueue()
	if _, ok := q.peekHead(); ok {
		t.Fatal("空队 peekHead 应 false")
	}
}

// TestQueuePopHeadOrder popHead 维持 FIFO。
func TestQueuePopHeadOrder(t *testing.T) {
	q := newQueue()
	for _, id := range []string{"h1", "h2"} {
		q.enqueue(mkEnv(id))
	}
	first := q.popHead()
	second := q.popHead()
	var tr1, tr2 proto.TaskResult
	if first.env.Decode(&tr1) != nil || tr1.TaskID != "h1" {
		t.Errorf("first TaskID=%q 期望 h1", tr1.TaskID)
	}
	if second.env.Decode(&tr2) != nil || tr2.TaskID != "h2" {
		t.Errorf("second TaskID=%q 期望 h2", tr2.TaskID)
	}
	if q.len() != 0 {
		t.Errorf("len=%d 期望 0", q.len())
	}
}

// TestQueueFlushToDrainsOnSuccess flushTo 在 WS 可用时逐条发送成功后出队。
func TestQueueFlushToDrainsOnSuccess(t *testing.T) {
	var received int64
	srv := startWSServer(t, func(env proto.Envelope) proto.Envelope {
		if env.Type == proto.TypeTaskResult {
			atomic.AddInt64(&received, 1)
		}
		return proto.Envelope{}
	})
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, wsURL(srv), nil) //nolint:bodyclose // coder/websocket:成功时 resp.Body 为 nil,无需关闭
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.CloseNow()
	wc := newWSConn(c)

	q := newQueue()
	for i := 0; i < 5; i++ {
		q.enqueue(mkEnv("flush-" + strconv.Itoa(i)))
	}
	q.flushTo(wc)

	// 等服务器收到全部
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt64(&received) == 5 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := atomic.LoadInt64(&received); got != 5 {
		t.Fatalf("服务器应收到 5 条, 得到 %d", got)
	}
	if q.len() != 0 {
		t.Fatalf("flush 成功后队应空, len=%d", q.len())
	}
}

// TestQueueFlushToRetainsOnWriteFailure 写失败时结果留在队列(可靠交付核心不变量)。
func TestQueueFlushToRetainsOnWriteFailure(t *testing.T) {
	// 用一个立即关闭的 WS,使 writeBytes 失败
	srv := startWSServer(t, func(env proto.Envelope) proto.Envelope {
		return proto.Envelope{}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, wsURL(srv), nil) //nolint:bodyclose // coder/websocket:成功时 resp.Body 为 nil,无需关闭
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	wc := newWSConn(c)
	c.CloseNow() // 立即关闭,后续写必失败

	q := newQueue()
	q.enqueue(mkEnv("will-stay"))
	q.flushTo(wc)

	// 写失败 → 结果应仍在队列(最多 attempt 变化)
	if q.len() == 0 {
		t.Fatal("写失败后结果不应出队(可靠交付要求保留)")
	}
}
