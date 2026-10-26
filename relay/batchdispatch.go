// Package main — 批量任务分发系统(round 8)。
//
// 在 handleBatchTask(按标签一对多下发)之上,补全:
//   - BatchDispatcher:一对多分发 + 结果聚合 + 失败重试。
//   - BatchResult:聚合每个目标的成功/失败/输出,计算整体成功率。
//   - 指数退避重试:对失败/超时的目标自动重投(可配 maxRetries)。
//
// 设计要点:
//   - 复用现有 dispatchTask(WS 转发),不重复实现投递路径。
//   - 结果聚合:监听 EventBus 的 task_result 事件,按 batchID 关联。
//   - 幂等:依赖 TaskStore 的 CAS 机制,重试不会重复执行已成功的任务。
//   - 与 handleBatchTask 共存:本模块提供更丰富的聚合 API,batch handler 可选用。
package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/QiuShichang/reins/proto"
)

// BatchRequest 批量任务请求(一对多)。
type BatchRequest struct {
	BatchID    string   // 批次 ID(为空则自动生成)
	AgentIDs   []string // 目标 Agent 列表(一对多)
	Type       string   // 任务类型
	Payload    []byte   // 任务载荷
	Timeout    int      // 单任务超时秒
	MaxRetries int      // 失败重试次数(0=不重试)
}

// TargetResult 单个目标的执行结果。
type TargetResult struct {
	AgentID string
	TaskID  string
	Success bool
	Output  string
	Error   string
	Attempt int // 实际投递次数
}

// BatchResult 批量任务聚合结果。
type BatchResult struct {
	BatchID    string
	StartedAt  int64
	FinishedAt int64
	Targets    []TargetResult
}

// SuccessRate 返回成功率(0-1)。
func (br *BatchResult) SuccessRate() float64 {
	if len(br.Targets) == 0 {
		return 0
	}
	ok := 0
	for _, t := range br.Targets {
		if t.Success {
			ok++
		}
	}
	return float64(ok) / float64(len(br.Targets))
}

// Succeeded/Failed 返回成功/失败的目标 ID 列表。
func (br *BatchResult) Succeeded() []string {
	var out []string
	for _, t := range br.Targets {
		if t.Success {
			out = append(out, t.AgentID)
		}
	}
	return out
}
func (br *BatchResult) Failed() []string {
	var out []string
	for _, t := range br.Targets {
		if !t.Success {
			out = append(out, t.AgentID)
		}
	}
	return out
}

// batchDispatcher 批量分发器(对 Relay 的增强,独立可测)。
// 用 dispatchFn 抽象投递动作,便于测试注入 mock(不依赖真实 WS)。
type batchDispatcher struct {
	dispatchFn func(ctx context.Context, agentID string, task proto.TaskRequest) (taskID string, err error)
}

// newBatchDispatcher 构造分发器。dispatchFn 为单个目标的投递函数。
func newBatchDispatcher(dispatchFn func(ctx context.Context, agentID string, task proto.TaskRequest) (string, error)) *batchDispatcher {
	return &batchDispatcher{dispatchFn: dispatchFn}
}

// Dispatch 执行一对多分发,并发投递所有目标,聚合结果。
// 不含重试(MaxRetries 在 DispatchWithRetry 中处理)。
func (d *batchDispatcher) Dispatch(ctx context.Context, req BatchRequest) BatchResult {
	br := BatchResult{
		BatchID:   req.BatchID,
		StartedAt: time.Now().UnixMilli(),
		Targets:   make([]TargetResult, len(req.AgentIDs)),
	}
	var wg sync.WaitGroup
	for i, aid := range req.AgentIDs {
		wg.Add(1)
		go func(idx int, agentID string) {
			defer wg.Done()
			taskID, err := d.dispatchOne(ctx, req, agentID, 1)
			tr := TargetResult{AgentID: agentID, TaskID: taskID, Attempt: 1}
			if err != nil {
				tr.Error = err.Error()
			} else {
				tr.Success = true
			}
			br.Targets[idx] = tr
		}(i, aid)
	}
	wg.Wait()
	br.FinishedAt = time.Now().UnixMilli()
	return br
}

// DispatchWithRetry 执行带重试的一对多分发。
// 对失败的目标按指数退避(初始 100ms,2x,封顶 1s)重投,最多 MaxRetries 次。
func (d *batchDispatcher) DispatchWithRetry(ctx context.Context, req BatchRequest) BatchResult {
	base := d.Dispatch(ctx, req)
	if req.MaxRetries <= 0 {
		return base
	}
	// 对失败目标重试
	for attempt := 1; attempt <= req.MaxRetries; attempt++ {
		var failedIdx []int
		for i, t := range base.Targets {
			if !t.Success {
				failedIdx = append(failedIdx, i)
			}
		}
		if len(failedIdx) == 0 {
			break // 全成功
		}
		// 指数退避(100ms * 2^(attempt-1),封顶 1s)
		backoff := time.Duration(100*(1<<(attempt-1))) * time.Millisecond
		if backoff > time.Second {
			backoff = time.Second
		}
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return base
		}
		var wg sync.WaitGroup
		for _, idx := range failedIdx {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				aid := base.Targets[i].AgentID
				taskID, err := d.dispatchOne(ctx, req, aid, attempt+1)
				if err == nil {
					base.Targets[i].Success = true
					base.Targets[i].Error = ""
					base.Targets[i].TaskID = taskID
				}
				base.Targets[i].Attempt = attempt + 1
			}(idx)
		}
		wg.Wait()
	}
	base.FinishedAt = time.Now().UnixMilli()
	return base
}

// dispatchOne 投递单个目标。taskID 用 batchID+agentID+attempt 去重。
func (d *batchDispatcher) dispatchOne(ctx context.Context, req BatchRequest, agentID string, attempt int) (string, error) {
	if d.dispatchFn == nil {
		return "", fmt.Errorf("无投递函数")
	}
	taskID := fmt.Sprintf("batch-%s-%s-a%d-%d", req.BatchID, agentID, attempt, time.Now().UnixNano())
	task := proto.TaskRequest{
		TaskID:  taskID,
		Type:    req.Type,
		AgentID: agentID,
		Payload: req.Payload,
		Timeout: req.Timeout,
	}
	return d.dispatchFn(ctx, agentID, task)
}

// --- 进度跟踪(Progress Tracking) ---
//
// BatchDispatcher 在 Dispatch / DispatchWithRetry 中是同步阻塞的——调用方要等所有目标结束。
// 但批量任务往往需要实时观察「已完成几个 / 失败几个 / 预计多久跑完」。
// BatchProgress 提供线程安全的实时进度快照,供:
//   - 控制台 SSE /api/batch/:id/progress 实时拉取。
//   - CLI `batch status <id>` 轮询。
//
// 状态机:pending → running → (succeeded | failed)。重试时 failed 可回退 running。

// 目标状态。
const (
	TargetPending   = "pending"   // 未投递
	TargetRunning   = "running"   // 投递中(含重试)
	TargetSucceeded = "succeeded" // 成功
	TargetFailed    = "failed"    // 失败(已耗尽重试)
)

// TargetState 单个目标的实时状态。
type TargetState struct {
	AgentID string
	Status  string // TargetPending/Running/Succeeded/Failed
	Attempt int    // 当前/最终投递次数
	TaskID  string
	Error   string
}

// BatchProgress 批次实时进度(线程安全快照)。
type BatchProgress struct {
	mu         sync.RWMutex
	batchID    string
	total      int
	startedAt  int64
	states     map[string]*TargetState // agentID → state
	succeeded  int
	failed     int
	dispatched int // 已投递(running+succeeded+failed)
}

// newBatchProgress 构造进度跟踪器,total 为目标数。
func newBatchProgress(batchID string, total int) *BatchProgress {
	states := make(map[string]*TargetState, total)
	return &BatchProgress{
		batchID:   batchID,
		total:     total,
		startedAt: time.Now().UnixMilli(),
		states:    states,
	}
}

// markRunning 标记某目标开始投递(第 attempt 次)。
// 状态语义:succeeded/failed 计数只反映「当前」处于该终态的目标数。
// markRunning 把目标移出终态(若之前是 Failed/Succeeded 则扣减对应计数),进入 Running。
func (p *BatchProgress) markRunning(agentID, taskID string, attempt int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st, ok := p.states[agentID]
	if !ok {
		st = &TargetState{AgentID: agentID, Status: TargetPending}
		p.states[agentID] = st
		p.dispatched++
	}
	// 离开旧终态前扣减计数
	switch st.Status {
	case TargetFailed:
		p.failed--
	case TargetSucceeded:
		p.succeeded--
	}
	st.Status = TargetRunning
	st.Attempt = attempt
	st.TaskID = taskID
}

// markSucceeded 标记某目标成功。
func (p *BatchProgress) markSucceeded(agentID, taskID string, attempt int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st, ok := p.states[agentID]
	if !ok {
		st = &TargetState{AgentID: agentID}
		p.states[agentID] = st
		p.dispatched++
	}
	// 离开旧终态前扣减计数
	switch st.Status {
	case TargetFailed:
		p.failed--
	case TargetSucceeded:
		// 已是成功,幂等:仅刷新字段,不重复计数
		st.Attempt = attempt
		st.TaskID = taskID
		st.Error = ""
		return
	}
	p.succeeded++
	st.Status = TargetSucceeded
	st.Attempt = attempt
	st.TaskID = taskID
	st.Error = ""
}

// markFailed 标记某目标失败(本次投递失败,不一定是终态——还会重试)。
func (p *BatchProgress) markFailed(agentID string, attempt int, errStr string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st, ok := p.states[agentID]
	if !ok {
		st = &TargetState{AgentID: agentID}
		p.states[agentID] = st
		p.dispatched++
	}
	// 离开旧终态前扣减计数
	switch st.Status {
	case TargetFailed:
		// 已是失败,幂等:仅刷新字段,不重复计数
		st.Attempt = attempt
		st.Error = errStr
		return
	case TargetSucceeded:
		p.succeeded--
	}
	p.failed++
	st.Status = TargetFailed
	st.Attempt = attempt
	st.Error = errStr
}

// ProgressSnapshot 进度只读快照。
type ProgressSnapshot struct {
	BatchID    string
	Total      int
	Dispatched int
	Succeeded  int
	Failed     int
	Pending    int // total - dispatched
	Done       int // Succeeded + Failed
	Percent    float64
	Elapsed    int64 // ms
	ETA        int64 // ms,按当前吞吐估算(0=无法估算)
	States     []TargetState
}

// Snapshot 返回当前进度快照(并发安全)。
func (p *BatchProgress) Snapshot() ProgressSnapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()
	states := make([]TargetState, 0, len(p.states))
	for _, st := range p.states {
		states = append(states, *st)
	}
	done := p.succeeded + p.failed
	pending := p.total - p.dispatched
	if pending < 0 {
		pending = 0
	}
	elapsed := time.Now().UnixMilli() - p.startedAt
	var percent float64
	if p.total > 0 {
		percent = float64(done) / float64(p.total) * 100
	}
	// ETA:按已完成平均耗时估算剩余
	var eta int64
	if done > 0 {
		avg := elapsed / int64(done)
		eta = avg * int64(pending)
	}
	return ProgressSnapshot{
		BatchID:    p.batchID,
		Total:      p.total,
		Dispatched: p.dispatched,
		Succeeded:  p.succeeded,
		Failed:     p.failed,
		Pending:    pending,
		Done:       done,
		Percent:    percent,
		Elapsed:    elapsed,
		ETA:        eta,
		States:     states,
	}
}

// IsDone 是否全部结束(没有 pending/running)。
func (p *BatchProgress) IsDone() bool {
	s := p.Snapshot()
	return s.Done >= s.Total && s.Total > 0
}

// DispatchTracked 执行带实时进度跟踪的一对多分发(不含重试)。
// 返回聚合结果与进度跟踪器;调用方可在分发完成后用 Snapshot() 取最终快照,
// 或在分发 goroutine 外并发地周期性 Snapshot() 拿实时进度。
func (d *batchDispatcher) DispatchTracked(ctx context.Context, req BatchRequest) (BatchResult, *BatchProgress) {
	progress := newBatchProgress(req.BatchID, len(req.AgentIDs))
	// 包装 dispatchFn,在投递前后更新进度
	orig := d.dispatchFn
	br := BatchResult{
		BatchID:   req.BatchID,
		StartedAt: time.Now().UnixMilli(),
		Targets:   make([]TargetResult, len(req.AgentIDs)),
	}
	var wg sync.WaitGroup
	for i, aid := range req.AgentIDs {
		wg.Add(1)
		go func(idx int, agentID string) {
			defer wg.Done()
			progress.markRunning(agentID, "", 1)
			taskID, err := d.dispatchOne(ctx, req, agentID, 1)
			tr := TargetResult{AgentID: agentID, TaskID: taskID, Attempt: 1}
			if err != nil {
				tr.Error = err.Error()
				progress.markFailed(agentID, 1, err.Error())
			} else {
				tr.Success = true
				progress.markSucceeded(agentID, taskID, 1)
			}
			br.Targets[idx] = tr
		}(i, aid)
	}
	wg.Wait()
	br.FinishedAt = time.Now().UnixMilli()
	_ = orig
	return br, progress
}

// DispatchWithRetryTracked 带重试 + 进度跟踪的版本。
func (d *batchDispatcher) DispatchWithRetryTracked(ctx context.Context, req BatchRequest) (BatchResult, *BatchProgress) {
	progress := newBatchProgress(req.BatchID, len(req.AgentIDs))
	return d.dispatchWithRetryTracked(ctx, req, progress), progress
}

// dispatchWithRetryTracked 主体:progress 由调用方传入——异步端点(P0-1 批量
// 分发端点化)需要在 goroutine 启动前就持有跟踪器,否则查询端点拿不到进度。
func (d *batchDispatcher) dispatchWithRetryTracked(ctx context.Context, req BatchRequest, progress *BatchProgress) BatchResult {
	base := BatchResult{
		BatchID:   req.BatchID,
		StartedAt: time.Now().UnixMilli(),
		Targets:   make([]TargetResult, len(req.AgentIDs)),
	}

	// 初始并发投递
	dispatchRound := func(attempt int, idxs []int) {
		var wg sync.WaitGroup
		for _, i := range idxs {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				aid := base.Targets[idx].AgentID
				if aid == "" {
					aid = req.AgentIDs[idx]
				}
				progress.markRunning(aid, "", attempt)
				taskID, err := d.dispatchOne(ctx, req, aid, attempt)
				if err == nil {
					base.Targets[idx].Success = true
					base.Targets[idx].Error = ""
					base.Targets[idx].TaskID = taskID
					progress.markSucceeded(aid, taskID, attempt)
				} else {
					base.Targets[idx].Error = err.Error()
					progress.markFailed(aid, attempt, err.Error())
				}
				base.Targets[idx].Attempt = attempt
			}(i)
		}
		wg.Wait()
	}

	// 首轮:全部目标
	all := make([]int, len(req.AgentIDs))
	for i := range all {
		all[i] = i
		base.Targets[i] = TargetResult{AgentID: req.AgentIDs[i], Attempt: 0}
	}
	dispatchRound(1, all)

	if req.MaxRetries <= 0 {
		base.FinishedAt = time.Now().UnixMilli()
		return base
	}
	// 重试:只对失败目标
	for attempt := 1; attempt <= req.MaxRetries; attempt++ {
		var failedIdx []int
		for i, t := range base.Targets {
			if !t.Success {
				failedIdx = append(failedIdx, i)
			}
		}
		if len(failedIdx) == 0 {
			break
		}
		backoff := time.Duration(100*(1<<(attempt-1))) * time.Millisecond
		if backoff > time.Second {
			backoff = time.Second
		}
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			base.FinishedAt = time.Now().UnixMilli()
			return base
		}
		dispatchRound(attempt+1, failedIdx)
	}
	base.FinishedAt = time.Now().UnixMilli()
	return base
}
