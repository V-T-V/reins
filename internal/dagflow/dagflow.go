// Package dagflow —— 批量任务编排的确定性 DAG 调度核心。
//
// 定位:纯逻辑、零依赖、无 IO——relay 的批量下发(P0-1 按 tag 扇出)只会
// "同时对一批 Agent 发同一个任务",而实际运维流程往往是多阶段的:
// "备份 → 升级 → 健康检查 → 失败回滚"。dagflow 把这类流程建模为
// 节点+依赖的 DAG,提供三件可测的核心能力:
//
//  1. Validate:图合法性(重复 ID/悬空依赖/自依赖/环)的提交期检查——
//     Kahn 拓扑排序判环,环上的节点会被点名报告;
//  2. Run:并发上限 + 就绪队列的确定性调度——就绪集合始终按节点 ID
//     字典序出队,派发结果只由图结构与 Action 成败决定;
//  3. 失败级联:节点失败后,所有(传递)下游默认整枝跳过,防止在
//     半升级状态下继续跑后续阶段;节点可标 ContinueOnError 把自身依赖
//     全部软化("告警/清理"类阶段无论上游成败都该跑)。
//
// 确定性语义:与 goroutine 调度/时钟/网络解耦——
//   - 串行模式(MaxConcurrency=1):启动序列完全可复现,"第 k 个启动的
//     是谁"只由图结构与各 Action 成败决定;
//   - 并发模式:每一轮调度事件内按 ID 字典序成批派发,哪些节点会
//     启动/跳过/失败同样只由结果决定(只有跨轮的批次间交错随完成
//     时序波动)。
package dagflow

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Status 节点终态。
type Status string

const (
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
	StatusSkipped   Status = "skipped" // 上游失败被级联跳过(从未启动)
)

// Node DAG 中的一个执行单元。Action 为 nil 视为立即成功的占位节点
// (用作纯汇聚/阶段分隔点)。
type Node struct {
	ID   string
	Deps []string // 依赖的节点 ID;全部成功后本节点才可启动
	// Action 执行体;返回 nil 记成功。ctx 在整个 Run 结束时取消,
	// 长 Action 应自行感知 ctx 提前退出。
	Action func(ctx context.Context) error
	// ContinueOnError:本节点的依赖全部视为软依赖——上游失败/跳过
	// 不级联跳过本节点,只要上游全部终态即可启动("告警/清理"类
	// 无论上游成败都该跑的阶段)。拓扑顺序保留:仍须等上游执行完。
	ContinueOnError bool
}

// Graph 一次编排流程的定义。
type Graph struct {
	Nodes []Node
	// MaxConcurrency 同时运行的最大节点数;<=0 视为 1(串行)。
	MaxConcurrency int
}

// NodeResult 单节点执行结果。
type NodeResult struct {
	ID     string
	Status Status
	Err    string // StatusFailed 时的失败原因文本
}

// Result 整次 Run 的结果快照。
type Result struct {
	Nodes []NodeResult // 按 ID 字典序
	// StartOrder 节点实际启动顺序(串行模式完全确定;并发模式每轮
	// 派发批内按 ID 序)。被跳过的节点不出现。
	StartOrder []string
	Succeeded  int
	Failed     int
	Skipped    int
}

// Success 全部节点成功(无失败且无跳过)。
func (r *Result) Success() bool { return r.Failed == 0 && r.Skipped == 0 }

// Validate 检查图合法性:节点非空、ID 唯一、无悬空依赖、无自依赖、
// 无环。环检测用 Kahn 拓扑排序:入度剥洋葱后剥不完的节点即环上节点。
func (g *Graph) Validate() error {
	if len(g.Nodes) == 0 {
		return fmt.Errorf("dagflow: 空图")
	}
	byID := make(map[string]*Node, len(g.Nodes))
	for i := range g.Nodes {
		n := &g.Nodes[i]
		if n.ID == "" {
			return fmt.Errorf("dagflow: 节点[%d]缺少 ID", i)
		}
		if _, dup := byID[n.ID]; dup {
			return fmt.Errorf("dagflow: 重复节点 ID %q", n.ID)
		}
		byID[n.ID] = n
	}
	// 入度表(同时校验悬空依赖/自依赖/重复依赖)。
	indeg := make(map[string]int, len(g.Nodes))
	for _, n := range g.Nodes {
		seen := make(map[string]bool, len(n.Deps))
		for _, d := range n.Deps {
			if d == n.ID {
				return fmt.Errorf("dagflow: 节点 %q 自依赖", n.ID)
			}
			if _, ok := byID[d]; !ok {
				return fmt.Errorf("dagflow: 节点 %q 依赖不存在的 %q", n.ID, d)
			}
			if seen[d] {
				return fmt.Errorf("dagflow: 节点 %q 重复依赖 %q", n.ID, d)
			}
			seen[d] = true
			indeg[n.ID]++
		}
	}
	// Kahn:入度 0 入队,逐个剥除;同层按 ID 序剥除保证报错信息稳定。
	children := g.childrenMap()
	queue := make([]string, 0, len(g.Nodes))
	for _, n := range g.Nodes {
		if indeg[n.ID] == 0 {
			queue = append(queue, n.ID)
		}
	}
	sort.Strings(queue)
	peeled := 0
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		peeled++
		kids := children[id]
		sort.Strings(kids)
		for _, c := range kids {
			indeg[c]--
			if indeg[c] == 0 {
				queue = append(queue, c)
			}
		}
	}
	if peeled != len(g.Nodes) {
		cyclic := make([]string, 0)
		for _, n := range g.Nodes {
			if indeg[n.ID] > 0 {
				cyclic = append(cyclic, n.ID)
			}
		}
		sort.Strings(cyclic)
		return fmt.Errorf("dagflow: 存在依赖环,环上/环后节点: %v", cyclic)
	}
	return nil
}

// childrenMap 依赖反查:ID → 直接依赖它的节点列表(共享底层数组,只读使用)。
func (g *Graph) childrenMap() map[string][]string {
	children := make(map[string][]string, len(g.Nodes))
	for _, n := range g.Nodes {
		for _, d := range n.Deps {
			children[d] = append(children[d], n.ID)
		}
	}
	return children
}

// Run 调度执行整张图,阻塞至全部节点终态。非法图(未过 Validate)不
// panic:环上/环后节点会作为 skipped 收尾。ctx 取消时,未启动节点按
// skipped 收尾,运行中的 Action 自行感知 ctx(框架不强杀)。
//
// 调度循环:就绪集合(所有硬依赖 succeeded、软依赖已终态)按 ID 字典序
// 取前 MaxConcurrency 个启动;任一节点终态后重新计算。就绪序与完成
// 时序解耦,保证 StartOrder 可复现。
func (g *Graph) Run(ctx context.Context) *Result {
	byID := make(map[string]*Node, len(g.Nodes))
	for i := range g.Nodes {
		byID[g.Nodes[i].ID] = &g.Nodes[i]
	}
	children := g.childrenMap()

	var (
		mu       sync.Mutex
		status   = make(map[string]Status, len(g.Nodes)) // 已终态节点
		errs     = make(map[string]string)               // failed 节点的错误文本
		running  = make(map[string]bool)                 // 已启动未终态
		startOrd []string
	)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// cascadeLocked:根节点已 failed/skipped,递归标记被硬依赖破坏的
	// 下游为 skipped。调用方须持 mu;BFS 队列实现,与图深度无关。
	cascadeLocked := func(root string) {
		queue := append([]string{}, children[root]...)
		for len(queue) > 0 {
			c := queue[0]
			queue = queue[1:]
			if _, done := status[c]; done {
				continue
			}
			broken := false
			if !byID[c].ContinueOnError { // 软节点不级联跳过
				for _, d := range byID[c].Deps {
					if status[d] == StatusFailed || status[d] == StatusSkipped {
						broken = true
						break
					}
				}
			}
			if broken {
				status[c] = StatusSkipped
				queue = append(queue, children[c]...)
			}
		}
	}

	// finish:记录终态并级联下游。
	finish := func(id string, st Status, errText string) {
		mu.Lock()
		defer mu.Unlock()
		delete(running, id)
		status[id] = st
		if errText != "" {
			errs[id] = errText
		}
		if st == StatusFailed || st == StatusSkipped {
			cascadeLocked(id)
		}
	}

	maxC := g.MaxConcurrency
	if maxC < 1 {
		maxC = 1
	}

	for {
		mu.Lock()
		if len(status) == len(g.Nodes) {
			mu.Unlock()
			break
		}
		if ctx.Err() != nil {
			// 取消收尾:未启动节点标 skipped;**在跑节点等它自己的终态落账**。
			// 此前的清扫把 running 中、Action 已返回但 finish() 尚未执行的节点
			// 一并埋成 skipped——取消前已成功的动作被误标(CI 两次 flake 的根因,
			// cancel() 在 Action 内先于 finish 调用时,轮询循环立刻在 ctx.Done 醒来
			// 抢先清扫)。Action 收到已取消的 runCtx 会尽快返回,等待有界。
			for _, n := range g.Nodes {
				if _, done := status[n.ID]; !done && !running[n.ID] {
					status[n.ID] = StatusSkipped
				}
			}
			if len(running) == 0 {
				mu.Unlock()
				break
			}
			mu.Unlock()
			select {
			case <-ctx.Done():
			case <-time.After(50 * time.Microsecond):
			}
			continue
		}
		// 就绪 = 未终态、未运行、所有硬依赖 succeeded 且软依赖已终态。
		ready := make([]string, 0)
		for _, n := range g.Nodes {
			if _, done := status[n.ID]; done || running[n.ID] {
				continue
			}
			ok := true
			soft := n.ContinueOnError // 软节点:依赖只须终态,成败不限
			for _, d := range n.Deps {
				ds, depDone := status[d]
				if !depDone { // 依赖还没跑完——等它终态
					ok = false
					break
				}
				if !soft && ds != StatusSucceeded {
					ok = false
					break
				}
			}
			if ok {
				ready = append(ready, n.ID)
			}
		}
		sort.Strings(ready)
		if len(ready) == 0 && len(running) == 0 {
			// 无可启动且无在跑:剩余节点被环/断链卡死(非法图),收尾。
			for _, n := range g.Nodes {
				if _, done := status[n.ID]; !done {
					status[n.ID] = StatusSkipped
				}
			}
			mu.Unlock()
			break
		}
		slots := maxC - len(running)
		launch := ready
		if len(launch) > slots {
			launch = launch[:slots]
		}
		for _, id := range launch {
			running[id] = true
			startOrd = append(startOrd, id)
		}
		mu.Unlock()

		for _, id := range launch {
			go func(n *Node) {
				var err error
				if n.Action != nil {
					err = n.Action(runCtx)
				}
				if err != nil {
					finish(n.ID, StatusFailed, err.Error())
				} else {
					finish(n.ID, StatusSucceeded, "")
				}
			}(byID[id])
		}
		if len(launch) == 0 {
			// 全在跑,等下一个终态事件:轻量轮询(核心库不引 channel
			// 广播,保持实现最小;50µs 粒度对调度语义无影响)。
			select {
			case <-ctx.Done():
			case <-time.After(50 * time.Microsecond):
			}
		}
	}

	res := &Result{StartOrder: startOrd}
	for _, n := range g.Nodes {
		st := status[n.ID] // 收尾循环保证全部终态
		nr := NodeResult{ID: n.ID, Status: st, Err: errs[n.ID]}
		res.Nodes = append(res.Nodes, nr)
		switch st {
		case StatusSucceeded:
			res.Succeeded++
		case StatusFailed:
			res.Failed++
		case StatusSkipped:
			res.Skipped++
		}
	}
	return res
}
