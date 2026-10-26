package main

import (
	"sync"
	"testing"

	"github.com/QiuShichang/reins/proto"
)

// TestXferChannelConcurrentClose 并发 routeXferData 与 unregisterXferChannels,
// 验证 P0-#4 修复:不再 panic "send on closed channel"。
//
// 之前 bug:routeXferData 解锁后再 send,与 unregister 的 close(ch) 竞争。
// 现在持锁发送,且 safeClose 用 recover 兜底。
func TestXferChannelConcurrentClose(t *testing.T) {
	const transfers = 50
	const rounds = 10

	for round := 0; round < rounds; round++ {
		wc := &wsConn{}
		var wg sync.WaitGroup

		// 并发启动 N 个 transfer,每个独立 route + unregister
		for i := 0; i < transfers; i++ {
			tid := "xfer-" + string(rune('a'+i)) + string(rune('0'+round))
			wc.registerXferChannel(tid)
			wc.registerReadyChannel(tid)
			wc.registerCompleteChannel(tid)

			wg.Add(2)
			// goroutine A:持续 route 数据
			go func(id string) {
				defer wg.Done()
				defer func() {
					if r := recover(); r != nil {
						t.Errorf("routeXferData panic: %v", r)
					}
				}()
				for j := 0; j < 20; j++ {
					wc.routeXferData(id, []byte{byte(j)}, j == 19)
				}
			}(tid)
			// goroutine B:中途 unregister
			go func(id string) {
				defer wg.Done()
				wc.unregisterXferChannels(id)
			}(tid)
		}
		wg.Wait()
	}
}

// TestXferChannelCloseAll 验证 closeAllXferChannels 不漏不重(P1-#5)。
func TestXferChannelCloseAll(t *testing.T) {
	wc := &wsConn{}
	for i := 0; i < 10; i++ {
		tid := "xfer-all-" + string(rune('a'+i))
		wc.registerXferChannel(tid)
		wc.registerReadyChannel(tid)
		wc.registerCompleteChannel(tid)
	}
	wc.closeAllXferChannels()

	// 再次 close 不应 panic
	wc.closeAllXferChannels()

	// map 应清空
	if len(wc.xferChannels) != 0 || len(wc.xferReady) != 0 || len(wc.xferComplete) != 0 {
		t.Errorf("closeAll 后 map 未清空: channels=%d ready=%d complete=%d",
			len(wc.xferChannels), len(wc.xferReady), len(wc.xferComplete))
	}
}

// TestReadySignalDelivery 验证 ready 信号在持锁状态下能送达(P1-#6)。
func TestReadySignalDelivery(t *testing.T) {
	wc := &wsConn{}
	tid := "xfer-ready-test"
	readyCh := wc.registerReadyChannel(tid)

	// 模拟 Agent 发 ready
	ready := proto.FileTransferReady{TransferID: tid, Accepted: true, Size: 100}
	wc.xferMu.Lock()
	ch, ok := wc.xferReady[tid]
	if ok {
		ch <- &ready
	}
	wc.xferMu.Unlock()
	if !ok {
		t.Fatal("ready channel 未注册")
	}

	// handler 应能收到
	select {
	case got := <-readyCh:
		if got.Accepted != true || got.Size != 100 {
			t.Errorf("ready 信号内容错误: %+v", got)
		}
	default:
		t.Error("ready 信号丢失")
	}
}
