// Package wsx 测试——优先级写引擎(P0 control 恒先于 P1 data)。
//
// 用 httptest + coder/websocket 建立真实环回连接(loopback,无外部网络依赖),
// 逐项验证:
//   - 控制帧优先:data 队列先堆满后投 control,control 帧仍先到对端
//   - Done 回传:写成功返回 nil,连接关闭后返回错误
//   - ctx 取消:WriteControl/WriteData 立即返回 ctx.Err
//   - Start 幂等:重复 Start 不会启动第二个 writer(帧不重复)
package wsx

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// newLoopback 建立一条真实 WS 环回连接,server 侧包成 *Writer。
// 返回 writer(服务侧)、client(客户端直连)、cleanup。
func newLoopback(t *testing.T) (*Writer, *websocket.Conn, func()) {
	t.Helper()
	var srvW *Writer
	ready := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, req *http.Request) {
		c, err := websocket.Accept(w, req, nil)
		if err != nil {
			return
		}
		srvW = New(c)
		close(ready)
		// 阻塞持住 handler 直到测试结束(连接由 cleanup 关闭)。
		<-req.Context().Done()
	})
	srv := httptest.NewServer(mux)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	client, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws", nil) //nolint:bodyclose // 成功时 resp.Body 为 nil
	if err != nil {
		srv.Close()
		t.Fatalf("dial: %v", err)
	}
	client.SetReadLimit(1 << 20)
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("server side never accepted")
	}
	cleanup := func() {
		cancel()
		client.Close(websocket.StatusNormalClosure, "")
		srv.Close()
	}
	return srvW, client, cleanup
}

// recvMsg 收一帧并返回 (type, data)。
func recvMsg(t *testing.T, c *websocket.Conn) (websocket.MessageType, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	mt, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return mt, data
}

// TestControlPriorityOverData —— 核心语义:控制帧不被数据帧饿死。
//
// 构造:writer 阻塞在对端不读的 socket 写上(大帧打满 OS 缓冲),此时
// dataCh 积压若干数据帧,再投一个 control 帧;恢复读取后 control 帧
// 必须先于积压的数据帧到达(writer 每写完一帧回到 select 时,
// 外层非阻塞分支总是先消费 controlCh)。
func TestControlPriorityOverData(t *testing.T) {
	w, client, cleanup := newLoopback(t)
	defer cleanup()

	ctx := context.Background()
	w.Start(ctx)

	const frames = 8
	const frameSize = 256 * 1024
	payload := bytes.Repeat([]byte("D"), frameSize)

	// 生产者:连发 8 个 256KB 数据帧(客户端先不读 → writer 尽快阻塞在写)。
	done := make(chan error, frames)
	go func() {
		for i := 0; i < frames; i++ {
			done <- w.WriteData(ctx, websocket.MessageBinary, payload)
		}
	}()
	// 等 writer 真正阻塞在 socket 写:第一批 WriteData 完成数停滞即说明
	// OS 发送缓冲已满(完成后帧也不再被写出)。256KB×8 ≫ loopback 缓冲。
	time.Sleep(300 * time.Millisecond)

	// 数据积压时投 control 帧(它入 controlCh 缓冲,不被 dataCh 排队阻塞)。
	ctrlErr := make(chan error, 1)
	go func() { ctrlErr <- w.WriteControl(ctx, websocket.MessageText, []byte("CTRL")) }()

	// 恢复读取,记录到达顺序(帧类型序列)。
	order := make(chan websocket.MessageType, frames+1)
	go func() {
		for i := 0; i < frames+1; i++ {
			mt, data, err := client.Read(ctx)
			if err != nil {
				return
			}
			_ = data
			order <- mt
		}
	}()

	// CTRL(text) 的序号必须 < 最后一个数据帧的序号:
	// 即 control 帧在仍有积压数据时被优先写出(P0 抢占 P1 队列)。
	ctrlIdx, lastDataIdx := -1, -1
	for i := 0; i < frames+1; i++ {
		mt := <-order
		if mt == websocket.MessageText {
			ctrlIdx = i
		} else {
			lastDataIdx = i
		}
	}
	if ctrlIdx == -1 {
		t.Fatal("control frame never arrived")
	}
	if ctrlIdx > lastDataIdx {
		t.Fatalf("control frame starved behind data backlog: ctrl idx=%d, last data idx=%d", ctrlIdx, lastDataIdx)
	}
	// 所有 WriteData/WriteControl 都应成功完成。
	if err := <-ctrlErr; err != nil {
		t.Fatalf("WriteControl: %v", err)
	}
	for i := 0; i < frames; i++ {
		if err := <-done; err != nil {
			t.Fatalf("WriteData %d: %v", i, err)
		}
	}
}

// TestWriteErrorPropagation —— 连接关闭后写必须报错(Done 语义:错误不吞)。
func TestWriteErrorPropagation(t *testing.T) {
	w, client, cleanup := newLoopback(t)
	defer cleanup()

	ctx := context.Background()
	w.Start(ctx)
	if err := w.WriteControl(ctx, websocket.MessageText, []byte("ok")); err != nil {
		t.Fatalf("first write should succeed: %v", err)
	}

	// 关闭客户端读侧 → 服务端写失败。coder/websocket 在对端关闭后
	// 后续 Write 返回错误;等待传播完成后断言非 nil。
	client.Close(websocket.StatusGoingAway, "")
	if err := w.WriteData(ctx, websocket.MessageText, []byte("boom")); err == nil {
		// 可能存在写缓冲尚未 flush 的窗口,重试一次确保传播。
		time.Sleep(100 * time.Millisecond)
		if err = w.WriteData(ctx, websocket.MessageText, []byte("boom2")); err == nil {
			t.Fatal("write after peer close must return error, got nil")
		}
	}
}

// TestContextCancel —— ctx 取消后 WriteControl/WriteData 立即返回 ctx.Err,
// 不阻塞在队列或等待 Done 上。
func TestContextCancel(t *testing.T) {
	w, _, cleanup := newLoopback(t)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	cancel()

	wctx, wcancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer wcancel()
	if err := w.WriteControl(wctx, websocket.MessageText, []byte("x")); err == nil {
		t.Fatal("WriteControl after writer ctx cancelled should fail, got nil")
	}
	if err := w.WriteData(wctx, websocket.MessageText, []byte("x")); err == nil {
		t.Fatal("WriteData after writer ctx cancelled should fail, got nil")
	}
}

// TestCallerCtxExpiry —— 调用方 ctx 到期时投递等待立即放弃并返回 DeadlineExceeded。
// 构造:不 Start writer(无人消费,Done 永不回填),WriteControl 应由调用方
// 超时兜底返回,而不是永久阻塞。这是对"生产者必须传可取消 ctx"契约的锚定。
func TestCallerCtxExpiry(t *testing.T) {
	w, _, cleanup := newLoopback(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := w.WriteControl(ctx, websocket.MessageText, []byte("never-written"))
	if err == nil {
		t.Fatal("WriteControl without started writer should time out, got nil")
	}
	if err != context.DeadlineExceeded {
		t.Fatalf("want context.DeadlineExceeded, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("caller ctx expiry should return promptly, took %v", elapsed)
	}
}

// TestStartIdempotent —— Start 调两次不产生双 writer(帧不重复)。
func TestStartIdempotent(t *testing.T) {
	w, client, cleanup := newLoopback(t)
	defer cleanup()

	ctx := context.Background()
	w.Start(ctx)
	w.Start(ctx) // 第二次必须是 no-op

	if err := w.WriteControl(ctx, websocket.MessageText, []byte("once")); err != nil {
		t.Fatalf("write: %v", err)
	}

	rctx, rcancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer rcancel()
	mt, data, err := client.Read(rctx)
	if err != nil {
		t.Fatalf("read first frame: %v", err)
	}
	if mt != websocket.MessageText || string(data) != "once" {
		t.Fatalf("frame corrupted: type=%v data=%q", mt, data)
	}
	// 若 Start 不幂等(双 writer 各自从队列消费同一 Item?不可能重复消费,
	// 但双 writer 并发写会触发 coder/websocket 的并发写断言/帧交错)。
	// 此处再写一批帧验证顺序完好。
	for i := 0; i < 4; i++ {
		if err := w.WriteData(ctx, websocket.MessageBinary, []byte{byte(i)}); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	for i := 0; i < 4; i++ {
		_, data := recvMsg(t, client)
		if len(data) != 1 || data[0] != byte(i) {
			t.Fatalf("frame %d corrupted: %v", i, data)
		}
	}
}
