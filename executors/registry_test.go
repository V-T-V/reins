package executors

import (
	"context"
	"sync"
	"testing"

	"github.com/V-T-V/reins/proto"
)

// noopExecutor 测试用执行器，立即返回成功。
type noopExecutor struct{ typ string }

func (e *noopExecutor) Type() string { return e.typ }
func (e *noopExecutor) Exec(_ context.Context, _ []byte, _ ProgressFunc) proto.TaskResult {
	return proto.TaskResult{TaskID: "", Success: true}
}

// TestDispatcherConcurrentRegisterDispatch 验证 Q1 修复：并发 Register + Dispatch 不触发 race。
// 需配合 `go test -race` 运行。
func TestDispatcherConcurrentRegisterDispatch(t *testing.T) {
	d := New()

	// 先注册一个基线类型
	d.Register(&noopExecutor{typ: "baseline"})

	var wg sync.WaitGroup
	// 并发写：动态注册新类型
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			d.Register(&noopExecutor{typ: "type-" + string(rune('a'+i%26))})
		}(i)
	}
	// 并发读：Dispatch + Types
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = d.Types()
			d.Dispatch(context.Background(), proto.TaskRequest{
				TaskID: "t",
				Type:   "baseline",
			}, nil)
		}()
	}
	wg.Wait()
	// 若 map 无锁，-race 会在此报 fatal
}

// TestDispatcherUnknownType 验证未知类型返回明确错误。
func TestDispatcherUnknownType(t *testing.T) {
	d := New()
	r := d.Dispatch(context.Background(), proto.TaskRequest{
		TaskID: "t1",
		Type:   "nonexistent",
	}, nil)
	if r.Success {
		t.Fatal("未知类型应失败")
	}
}
