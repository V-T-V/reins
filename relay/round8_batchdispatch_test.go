// Package main — round 8 批量任务分发系统测试。
//
// 验证一对多分发 + 结果聚合 + 失败重试的核心不变量:
//   - 并发投递所有目标。
//   - 结果聚合(成功/失败/输出)与成功率计算。
//   - 失败目标指数退避重试。
//   - 重试幂等(成功后不再重投)。
package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QiuShichang/go-rmm/proto"
)

// mockDispatch 构造可控行为的投递函数。
// failFirst 中的 agentID 在其首次调用时失败(模拟临时/永久故障),第二次起成功。
// 返回总调用计数便于断言重试次数。
func mockDispatch(failFirst []string) (func(context.Context, string, proto.TaskRequest) (string, error), *int32) {
	var calls int32
	failSet := make(map[string]bool, len(failFirst))
	for _, a := range failFirst {
		failSet[a] = true
	}
	var mu sync.Mutex
	// 记录每个 agent 已调用次数,首次失败后从 failSet 移除(下次成功)
	counts := make(map[string]int)
	fn := func(ctx context.Context, agentID string, task proto.TaskRequest) (string, error) {
		atomic.AddInt32(&calls, 1)
		mu.Lock()
		counts[agentID]++
		shouldFail := failSet[agentID]
		if shouldFail {
			delete(failSet, agentID) // 下次成功
		}
		mu.Unlock()
		if shouldFail {
			return "", errors.New("mock failure")
		}
		return task.TaskID, nil
	}
	return fn, &calls
}

// --- 一对多分发 + 结果聚合 ---

// TestBatchDispatchAllSucceed 验证所有目标成功的聚合。
func TestBatchDispatchAllSucceed(t *testing.T) {
	fn, _ := mockDispatch(nil) // 全部成功
	d := newBatchDispatcher(fn)
	req := BatchRequest{
		BatchID:  "b1",
		AgentIDs: []string{"a1", "a2", "a3"},
		Type:     proto.TaskPowerShell,
		Payload:  []byte("{}"),
	}
	br := d.Dispatch(context.Background(), req)
	if len(br.Targets) != 3 {
		t.Fatalf("应有 3 个目标结果, got %d", len(br.Targets))
	}
	if br.SuccessRate() != 1.0 {
		t.Fatalf("全成功 SuccessRate 应 1.0, got %f", br.SuccessRate())
	}
	if len(br.Succeeded()) != 3 || len(br.Failed()) != 0 {
		t.Fatalf("Succeeded/Failed 错误: %v / %v", br.Succeeded(), br.Failed())
	}
	if br.FinishedAt < br.StartedAt {
		t.Fatal("FinishedAt 不应早于 StartedAt")
	}
}

// TestBatchDispatchPartialFailure 验证部分失败的聚合。
func TestBatchDispatchPartialFailure(t *testing.T) {
	// a2 首次失败(不重试,本用例验证聚合)
	fn, _ := mockDispatch([]string{"a2"})
	d := newBatchDispatcher(fn)
	req := BatchRequest{
		BatchID:  "b2",
		AgentIDs: []string{"a1", "a2", "a3"},
		Type:     proto.TaskPowerShell,
	}
	br := d.Dispatch(context.Background(), req)
	if br.SuccessRate() < 0.6 || br.SuccessRate() > 0.7 {
		t.Fatalf("2/3 成功 SuccessRate 应 ~0.67, got %f", br.SuccessRate())
	}
	failed := br.Failed()
	if len(failed) != 1 || failed[0] != "a2" {
		t.Fatalf("失败应为 [a2], got %v", failed)
	}
}

// TestBatchDispatchConcurrentAllDispatched 验证并发投递(每个目标都收到)。
func TestBatchDispatchConcurrentAllDispatched(t *testing.T) {
	var dispatched int32
	fn := func(ctx context.Context, agentID string, task proto.TaskRequest) (string, error) {
		atomic.AddInt32(&dispatched, 1)
		return task.TaskID, nil
	}
	d := newBatchDispatcher(fn)
	req := BatchRequest{BatchID: "b3", AgentIDs: []string{"a1", "a2", "a3", "a4", "a5"}}
	d.Dispatch(context.Background(), req)
	if dispatched != 5 {
		t.Fatalf("应投递 5 个目标, got %d", dispatched)
	}
}

// TestBatchDispatchEmptyTargets 验证空目标列表安全。
func TestBatchDispatchEmptyTargets(t *testing.T) {
	fn, _ := mockDispatch(nil)
	d := newBatchDispatcher(fn)
	br := d.Dispatch(context.Background(), BatchRequest{BatchID: "b-empty", AgentIDs: nil})
	if len(br.Targets) != 0 {
		t.Fatalf("空目标应返回空结果, got %d", len(br.Targets))
	}
	if br.SuccessRate() != 0 {
		t.Fatal("空结果 SuccessRate 应 0")
	}
}

// --- 失败重试 ---

// TestBatchRetryRecoversTemporaryFailure 验证重试能恢复临时失败。
// a2 第一次失败,重试后成功。
func TestBatchRetryRecoversTemporaryFailure(t *testing.T) {
	// a2 在第 1 次调用失败,之后成功(用 attempt 计数模拟)
	var a2Calls int32
	fn := func(ctx context.Context, agentID string, task proto.TaskRequest) (string, error) {
		if agentID == "a2" {
			n := atomic.AddInt32(&a2Calls, 1)
			if n == 1 {
				return "", errors.New("temp failure")
			}
		}
		return task.TaskID, nil
	}
	d := newBatchDispatcher(fn)
	req := BatchRequest{
		BatchID:    "b-retry",
		AgentIDs:   []string{"a1", "a2", "a3"},
		MaxRetries: 3,
	}
	br := d.DispatchWithRetry(context.Background(), req)
	if br.SuccessRate() != 1.0 {
		t.Fatalf("重试后应全成功(1.0), got %f", br.SuccessRate())
	}
	// a2 应被调用至少 2 次(首次失败 + 重试成功)
	if a2Calls < 2 {
		t.Fatalf("a2 重试后应调用 ≥2 次, got %d", a2Calls)
	}
}

// TestBatchRetryStopsAfterMaxRetries 验证达到 MaxRetries 后停止重试。
func TestBatchRetryStopsAfterMaxRetries(t *testing.T) {
	// a2 永远失败
	fn := func(ctx context.Context, agentID string, task proto.TaskRequest) (string, error) {
		if agentID == "a2" {
			return "", errors.New("permanent failure")
		}
		return task.TaskID, nil
	}
	var calls int32
	wrapped := func(ctx context.Context, agentID string, task proto.TaskRequest) (string, error) {
		atomic.AddInt32(&calls, 1)
		return fn(ctx, agentID, task)
	}
	d := newBatchDispatcher(wrapped)
	req := BatchRequest{
		BatchID:    "b-max",
		AgentIDs:   []string{"a1", "a2"},
		MaxRetries: 2,
	}
	br := d.DispatchWithRetry(context.Background(), req)
	// a1 成功(1次) + a2 失败(1 + 2 重试 = 3 次) = 总 4 次
	if br.SuccessRate() < 0.4 || br.SuccessRate() > 0.6 {
		t.Fatalf("1/2 成功率应 ~0.5, got %f", br.SuccessRate())
	}
	// a2 的 Attempt 应 ≤ MaxRetries+1
	for _, t2 := range br.Targets {
		if t2.AgentID == "a2" {
			if t2.Attempt > req.MaxRetries+1 {
				t.Fatalf("a2 Attempt 应 ≤ %d, got %d", req.MaxRetries+1, t2.Attempt)
			}
			if t2.Success {
				t.Fatal("a2 永久失败不应最终成功")
			}
		}
	}
}

// TestBatchRetryIdempotentSuccess 验证已成功目标不被重投。
func TestBatchRetryIdempotentSuccess(t *testing.T) {
	var a1Calls int32
	fn := func(ctx context.Context, agentID string, task proto.TaskRequest) (string, error) {
		if agentID == "a1" {
			atomic.AddInt32(&a1Calls, 1)
		}
		return task.TaskID, nil // 全成功
	}
	d := newBatchDispatcher(fn)
	req := BatchRequest{
		BatchID:    "b-idem",
		AgentIDs:   []string{"a1", "a2"},
		MaxRetries: 3,
	}
	d.DispatchWithRetry(context.Background(), req)
	if a1Calls != 1 {
		t.Fatalf("已成功的 a1 不应被重投, got %d 次调用", a1Calls)
	}
}

// TestBatchRetryContextCancel 验证 ctx 取消时及时退出。
func TestBatchRetryContextCancel(t *testing.T) {
	fn := func(ctx context.Context, agentID string, task proto.TaskRequest) (string, error) {
		return "", errors.New("fail") // 一直失败触发重试
	}
	d := newBatchDispatcher(fn)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	req := BatchRequest{
		BatchID:    "b-cancel",
		AgentIDs:   []string{"a1"},
		MaxRetries: 100, // 很多重试,但 ctx 会在 200ms 超时
	}
	// 不应因重试过多而 hang
	done := make(chan struct{})
	go func() {
		d.DispatchWithRetry(ctx, req)
		close(done)
	}()
	select {
	case <-done:
		// ok
	case <-time.After(3 * time.Second):
		t.Fatal("ctx 取消后应退出,不应 hang 3s")
	}
}

// TestBatchResultTaskIDUnique 验证每个目标的 taskID 唯一(含 attempt)。
func TestBatchResultTaskIDUnique(t *testing.T) {
	fn, _ := mockDispatch(nil)
	d := newBatchDispatcher(fn)
	req := BatchRequest{BatchID: "b-unique", AgentIDs: []string{"a1", "a2"}}
	br := d.Dispatch(context.Background(), req)
	seen := map[string]bool{}
	for _, t2 := range br.Targets {
		if t2.TaskID == "" {
			t.Fatal("TaskID 不应为空")
		}
		if seen[t2.TaskID] {
			t.Fatalf("TaskID 重复: %s", t2.TaskID)
		}
		seen[t2.TaskID] = true
	}
}

// --- 进度跟踪(Progress Tracking) ---

// TestBatchProgressAllSucceed 验证全成功时进度终态 100%。
func TestBatchProgressAllSucceed(t *testing.T) {
	fn, _ := mockDispatch(nil)
	d := newBatchDispatcher(fn)
	req := BatchRequest{BatchID: "p1", AgentIDs: []string{"a1", "a2", "a3"}}
	br, progress := d.DispatchTracked(context.Background(), req)
	if br.SuccessRate() != 1.0 {
		t.Fatalf("应全成功, got %f", br.SuccessRate())
	}
	s := progress.Snapshot()
	if s.Total != 3 || s.Done != 3 || s.Succeeded != 3 || s.Failed != 0 {
		t.Fatalf("进度终态错误: %+v", s)
	}
	if s.Percent != 100.0 {
		t.Fatalf("Percent 应 100, got %f", s.Percent)
	}
	if s.Pending != 0 {
		t.Fatalf("Pending 应 0, got %d", s.Pending)
	}
	if !progress.IsDone() {
		t.Fatal("全成功后 IsDone 应 true")
	}
}

// TestBatchProgressPartialFailure 验证部分失败进度统计。
func TestBatchProgressPartialFailure(t *testing.T) {
	fn, _ := mockDispatch([]string{"a2"}) // a2 失败
	d := newBatchDispatcher(fn)
	req := BatchRequest{BatchID: "p2", AgentIDs: []string{"a1", "a2", "a3"}}
	_, progress := d.DispatchTracked(context.Background(), req)
	s := progress.Snapshot()
	if s.Succeeded != 2 || s.Failed != 1 {
		t.Fatalf("应 2 成功 1 失败, got Succeeded=%d Failed=%d", s.Succeeded, s.Failed)
	}
	if s.Percent < 99.9 || s.Percent > 100.1 {
		t.Fatalf("Percent 应 100(全部结束), got %f", s.Percent)
	}
	// a2 状态应为 failed
	var a2 *TargetState
	for i := range s.States {
		if s.States[i].AgentID == "a2" {
			a2 = &s.States[i]
		}
	}
	if a2 == nil || a2.Status != TargetFailed {
		t.Fatalf("a2 状态应为 failed, got %+v", a2)
	}
}

// TestBatchProgressRetryRecovers 验证重试恢复后进度反映最终成功。
func TestBatchProgressRetryRecovers(t *testing.T) {
	// a2 首次失败,重试成功
	var a2Calls int32
	fn := func(ctx context.Context, agentID string, task proto.TaskRequest) (string, error) {
		if agentID == "a2" {
			if atomic.AddInt32(&a2Calls, 1) == 1 {
				return "", errors.New("temp")
			}
		}
		return task.TaskID, nil
	}
	d := newBatchDispatcher(fn)
	req := BatchRequest{BatchID: "p3", AgentIDs: []string{"a1", "a2"}, MaxRetries: 3}
	br, progress := d.DispatchWithRetryTracked(context.Background(), req)
	if br.SuccessRate() != 1.0 {
		t.Fatalf("重试后应全成功, got %f", br.SuccessRate())
	}
	s := progress.Snapshot()
	if s.Succeeded != 2 || s.Failed != 0 {
		t.Fatalf("最终进度应 2 成功 0 失败, got Succeeded=%d Failed=%d", s.Succeeded, s.Failed)
	}
}

// TestBatchProgressConcurrentSnapshotSafe 验证并发快照线程安全(不 panic/data race)。
func TestBatchProgressConcurrentSnapshotSafe(t *testing.T) {
	p := newBatchProgress("p4-concurrent", 3)
	var wg sync.WaitGroup
	// 并发写:多个 goroutine 同时 markRunning/markSucceeded/markFailed
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			aid := "a" + string(rune('1'+n%3))
			p.markRunning(aid, "t", 1)
			if n%2 == 0 {
				p.markSucceeded(aid, "t", 1)
			} else {
				p.markFailed(aid, 1, "err")
			}
		}(i)
	}
	// 并发读:同时 Snapshot
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = p.Snapshot()
		}()
	}
	wg.Wait()
	// 最终能拿到一个合法快照即通过(不 panic 即并发安全)
	s := p.Snapshot()
	if s.Total != 3 {
		t.Fatalf("Total 应 3, got %d", s.Total)
	}
}

// TestBatchProgressFinalAttemptCount 验证进度中 Attempt 反映最终投递次数。
func TestBatchProgressFinalAttemptCount(t *testing.T) {
	// a2 永久失败,MaxRetries=2 → Attempt=3
	fn := func(ctx context.Context, agentID string, task proto.TaskRequest) (string, error) {
		if agentID == "a2" {
			return "", errors.New("perm")
		}
		return task.TaskID, nil
	}
	d := newBatchDispatcher(fn)
	req := BatchRequest{BatchID: "p5", AgentIDs: []string{"a1", "a2"}, MaxRetries: 2}
	_, progress := d.DispatchWithRetryTracked(context.Background(), req)
	s := progress.Snapshot()
	for _, st := range s.States {
		if st.AgentID == "a2" {
			if st.Attempt != 3 {
				t.Fatalf("a2 永久失败 Attempt 应 3(1+2重试), got %d", st.Attempt)
			}
			if st.Status != TargetFailed {
				t.Fatalf("a2 应 failed, got %s", st.Status)
			}
		}
	}
}

// TestBatchProgressETANonNeg 验证 ETA/Elapsed 非负且 ETA=0 当无吞吐时可接受。
func TestBatchProgressETANonNeg(t *testing.T) {
	fn, _ := mockDispatch(nil)
	d := newBatchDispatcher(fn)
	req := BatchRequest{BatchID: "p6", AgentIDs: []string{"a1", "a2"}}
	_, progress := d.DispatchTracked(context.Background(), req)
	s := progress.Snapshot()
	if s.Elapsed < 0 {
		t.Fatalf("Elapsed 不应负, got %d", s.Elapsed)
	}
	if s.ETA < 0 {
		t.Fatalf("ETA 不应负, got %d", s.ETA)
	}
}
