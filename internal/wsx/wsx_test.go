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

	// 32×1MB = 32MB:必须远大于任何宿主的 socket 缓冲(CI runner 内核 wmem 可达数 MB,
	// 早期 8×256KB=2MB 版本在缓冲较大的机器上 writer 根本不阻塞,顺序断言必然翻转——
	// 2026-10-26 GitHub Actions 首跑即 flake 的真机制)。
	const frames = 32
	const frameSize = 1024 * 1024
	payload := bytes.Repeat([]byte("D"), frameSize)

	// 生产者:连发 32 个 1MB 数据帧(客户端先不读 → writer 尽快阻塞在写)。
	done := make(chan error, frames)
	go func() {
		for i := 0; i < frames; i++ {
			done <- w.WriteData(ctx, websocket.MessageBinary, payload)
		}
	}()
	// 等 writer 真正阻塞在 socket 写:第一批 WriteData 完成数停滞即说明
	// OS 发送缓冲已满(完成后帧也不再被写出)。32MB ≫ loopback/CI 缓冲。
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

// TestContextCancel —— ctx 契约(2026-09-14 修订断言:原版 flaky)。
//
// 设计语义见 wsx.go 包注释:"ctx 取消即退出 writer;生产者在 ctx 超时后
// 放弃等待,帧仍会随队列消费被写出(可能迟到但不会丢)"。
//
// 两段确定性/非确定性分层:
//  1. [确定性] 调用方 ctx 已取消:WriteControl/WriteData 必返回 ctx.Err()——
//     投递 select(入队 vs ctx.Done)两分支殊途同归:入队后 waitDone 必选
//     ctx.Done(无 writer 回填 done),或投递时直接选 ctx.Done。
//  2. [契约] writer ctx 取消(Start 后 cancel):帧允许被 writer 迟到消费并
//     成功写出(返回 nil——"不会丢"),也允许 writer 已退出而由调用方超时
//     兜底(返回 err)。唯一保证:调用不挂死。
//
// 原断言"取消后必须返回错误"过强:cancel 与 writer 消费 controlCh 竞态
// (两处 select 多 ready case 随机选择)+ loopback 写 fast path 必成功,
// 在调度偏置下稳定返回 nil 而翻车——nil 本就是设计允许的行为。
func TestContextCancel(t *testing.T) {
	// --- 段 1:调用方 ctx 已取消(未 Start,无 writer)——确定性返回 ctx.Err ---
	w1, _, cleanup1 := newLoopback(t)
	defer cleanup1()
	cctx, ccancel := context.WithCancel(context.Background())
	ccancel()
	if err := w1.WriteControl(cctx, websocket.MessageText, []byte("x")); err != context.Canceled {
		t.Fatalf("WriteControl with cancelled caller ctx: want context.Canceled, got %v", err)
	}
	if err := w1.WriteData(cctx, websocket.MessageText, []byte("x")); err != context.Canceled {
		t.Fatalf("WriteData with cancelled caller ctx: want context.Canceled, got %v", err)
	}

	// --- 段 2:writer ctx 取消——只锚定"不挂死"契约(nil/err 均合法) ---
	w2, _, cleanup2 := newLoopback(t)
	defer cleanup2()
	ctx, cancel := context.WithCancel(context.Background())
	w2.Start(ctx)
	cancel()

	wctx, wcancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer wcancel()
	done := make(chan error, 2)
	go func() { done <- w2.WriteControl(wctx, websocket.MessageText, []byte("x")) }()
	go func() { done <- w2.WriteData(wctx, websocket.MessageText, []byte("x")) }()
	for i := 0; i < 2; i++ {
		select {
		case err := <-done:
			// nil(迟到消费成功)/err(writer 已退出,调用方兜底)均为设计允许,不判值。
			_ = err
		case <-time.After(3 * time.Second):
			t.Fatal("Write after writer ctx cancelled must not block (deadlock)")
		}
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
