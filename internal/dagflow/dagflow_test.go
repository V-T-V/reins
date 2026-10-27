package dagflow

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestValidateLegit 合法 DAG(钻石 + 菱形汇聚)应通过校验。
func TestValidateLegit(t *testing.T) {
	g := &Graph{Nodes: []Node{
		{ID: "backup"},
		{ID: "upgrade", Deps: []string{"backup"}},
		{ID: "notify", Deps: []string{"upgrade"}, ContinueOnError: true},
		{ID: "health", Deps: []string{"upgrade"}},
	}}
	if err := g.Validate(); err != nil {
		t.Fatalf("合法图不应报错: %v", err)
	}
}

// TestValidateErrors 各类非法图逐一拒绝,且报错点名肇事节点。
func TestValidateErrors(t *testing.T) {
	cases := []struct {
		name  string
		graph Graph
		want  string // 报错应包含的子串
	}{
		{"空图", Graph{}, "空图"},
		{"缺ID", Graph{Nodes: []Node{{ID: "a"}, {}}}, "缺少 ID"},
		{"重复ID", Graph{Nodes: []Node{{ID: "a"}, {ID: "a"}}}, `重复节点 ID "a"`},
		{"悬空依赖", Graph{Nodes: []Node{{ID: "a", Deps: []string{"ghost"}}}}, `依赖不存在的 "ghost"`},
		{"自依赖", Graph{Nodes: []Node{{ID: "a", Deps: []string{"a"}}}}, `自依赖`},
		{"重复依赖", Graph{Nodes: []Node{{ID: "a"}, {ID: "b", Deps: []string{"a", "a"}}}}, `重复依赖`},
		{"两节点环", Graph{Nodes: []Node{{ID: "a", Deps: []string{"b"}}, {ID: "b", Deps: []string{"a"}}}}, "依赖环"},
		{"三角环", Graph{Nodes: []Node{
			{ID: "a", Deps: []string{"c"}},
			{ID: "b", Deps: []string{"a"}},
			{ID: "c", Deps: []string{"b"}},
		}}, "依赖环"},
	}
	for _, c := range cases {
		g := c.graph
		err := g.Validate()
		if err == nil {
			t.Fatalf("[%s] 应报错却通过", c.name)
		}
		if !contains(err.Error(), c.want) {
			t.Fatalf("[%s] 报错 %q 应包含 %q", c.name, err.Error(), c.want)
		}
	}
}

// TestValidateCycleNamesNodes 环检测应把环上节点点名(而非只说"有环")。
func TestValidateCycleNamesNodes(t *testing.T) {
	g := &Graph{Nodes: []Node{
		{ID: "root"},
		{ID: "x", Deps: []string{"y", "root"}},
		{ID: "y", Deps: []string{"z"}},
		{ID: "z", Deps: []string{"x"}},
		{ID: "after", Deps: []string{"z"}}, // 环后节点同样永不可达
	}}
	err := g.Validate()
	if err == nil {
		t.Fatal("应检出环")
	}
	for _, id := range []string{"x", "y", "z", "after"} {
		if !contains(err.Error(), id) {
			t.Fatalf("报错 %q 应点名不可达节点 %q", err.Error(), id)
		}
	}
	if contains(err.Error(), "root") {
		t.Fatalf("root 可达,不应被误报: %v", err)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

// TestRunSerialDeterministic 串行模式启动序列完全确定:
// 深度 4 的升级流水线,50 次重跑 StartOrder 逐位一致。
func TestRunSerialDeterministic(t *testing.T) {
	g := func() *Graph {
		return &Graph{Nodes: []Node{
			{ID: "backup", Action: ok()},
			{ID: "upload", Deps: []string{"backup"}, Action: ok()},
			{ID: "verify", Deps: []string{"upload"}, Action: ok()},
			{ID: "switch", Deps: []string{"verify"}, Action: ok()},
			{ID: "watch", Deps: []string{"switch"}, Action: ok()},
		}}
	}
	want := []string{"backup", "upload", "verify", "switch", "watch"}
	for i := 0; i < 50; i++ {
		res := g().Run(context.Background())
		if !res.Success() {
			t.Fatalf("第 %d 次全成功失败: %+v", i, res)
		}
		assertSeq(t, res.StartOrder, want)
	}
}

// TestRunDependencyOrder 同层多候选时按 ID 字典序启动(串行)。
func TestRunDependencyOrder(t *testing.T) {
	g := &Graph{Nodes: []Node{
		{ID: "root", Action: ok()},
		{ID: "z-branch", Deps: []string{"root"}, Action: ok()},
		{ID: "a-branch", Deps: []string{"root"}, Action: ok()},
		{ID: "m-branch", Deps: []string{"root"}, Action: ok()},
		{ID: "join", Deps: []string{"z-branch", "a-branch", "m-branch"}, Action: ok()},
	}}
	res := g.Run(context.Background())
	assertSeq(t, res.StartOrder, []string{"root", "a-branch", "m-branch", "z-branch", "join"})
}

// TestRunConcurrencyCap 并发上限:10 个并行节点 + MaxConcurrency=3,
// 任一时刻在跑数 ≤3,且全部成功。
func TestRunConcurrencyCap(t *testing.T) {
	var cur, peak int32
	var mu sync.Mutex // 保护 peak 更新的读-改-写
	g := &Graph{MaxConcurrency: 3}
	for i := 0; i < 10; i++ {
		g.Nodes = append(g.Nodes, Node{
			ID: fmt.Sprintf("n%02d", i),
			Action: func(ctx context.Context) error {
				c := atomic.AddInt32(&cur, 1)
				mu.Lock()
				if c > peak {
					peak = c
				}
				mu.Unlock()
				time.Sleep(5 * time.Millisecond)
				atomic.AddInt32(&cur, -1)
				return nil
			},
		})
	}
	res := g.Run(context.Background())
	if res.Succeeded != 10 || !res.Success() {
		t.Fatalf("应全部成功: %+v", res)
	}
	if peak > 3 {
		t.Fatalf("并发峰值 %d 超上限 3", peak)
	}
	if peak < 2 {
		t.Fatalf("并发峰值 %d <2,疑似退化为串行", peak)
	}
}

// TestRunFailureCascade 升级失败 → 健康检查/观察整枝跳过,告警
// (ContinueOnError)照常执行;跳过节点从未启动。
func TestRunFailureCascade(t *testing.T) {
	var started sync.Map
	startedFn := func(id string) func(context.Context) error {
		return func(context.Context) error {
			started.Store(id, true)
			return nil
		}
	}
	g := &Graph{MaxConcurrency: 2, Nodes: []Node{
		{ID: "backup", Action: startedFn("backup")},
		{ID: "upgrade", Deps: []string{"backup"}, Action: func(context.Context) error {
			started.Store("upgrade", true)
			return fmt.Errorf("disk full")
		}},
		{ID: "health", Deps: []string{"upgrade"}, Action: startedFn("health")},
		{ID: "watch", Deps: []string{"health"}, Action: startedFn("watch")},
		{ID: "alert", Deps: []string{"upgrade"}, ContinueOnError: true, Action: startedFn("alert")},
	}}
	res := g.Run(context.Background())

	if res.Success() {
		t.Fatal("有失败节点,整体不应 Success")
	}
	if res.Failed != 1 || res.Skipped != 2 || res.Succeeded != 2 {
		t.Fatalf("统计应 1 失败/2 跳过/2 成功(backup+alert): %+v", res)
	}
	if res.Nodes[findRes(t, res, "upgrade")].Err != "disk full" {
		t.Fatalf("失败原因应透传: %+v", res.Nodes[findRes(t, res, "upgrade")])
	}
	for _, id := range []string{"health", "watch"} {
		if _, ran := started.Load(id); ran {
			t.Fatalf("被级联跳过的 %q 不应被启动", id)
		}
		if res.Nodes[findRes(t, res, id)].Status != StatusSkipped {
			t.Fatalf("%q 应为 skipped: %+v", id, res.Nodes)
		}
	}
	if _, ran := started.Load("alert"); !ran {
		t.Fatal("ContinueOnError 下游 alert 应照常执行")
	}
}

// TestRunSoftDepMustFinish 软依赖虽不拦截成败,但仍要等它终态——
// join 在 slow-alert 结束前不得启动。
func TestRunSoftDepMustFinish(t *testing.T) {
	var alertDone int32
	var joinSawAlertDone int32
	g := &Graph{Nodes: []Node{
		{ID: "root", Action: ok()},
		{ID: "slow-alert", Deps: []string{"root"}, Action: func(context.Context) error {
			time.Sleep(30 * time.Millisecond)
			atomic.StoreInt32(&alertDone, 1)
			return fmt.Errorf("alert channel broken") // 软依赖还失败
		}},
		{ID: "join", Deps: []string{"slow-alert"}, ContinueOnError: true, Action: func(context.Context) error {
			if atomic.LoadInt32(&alertDone) == 1 {
				atomic.StoreInt32(&joinSawAlertDone, 1)
			}
			return nil
		}},
	}}
	res := g.Run(context.Background())
	if res.Nodes[findRes(t, res, "join")].Status != StatusSucceeded {
		t.Fatalf("软依赖失败不应拦下游: %+v", res.Nodes)
	}
	if joinSawAlertDone != 1 {
		t.Fatal("join 启动时软依赖尚未终态(顺序被破坏)")
	}
}

// TestRunPlaceholderNode Action 为 nil 的占位节点立即成功(纯汇聚点)。
func TestRunPlaceholderNode(t *testing.T) {
	g := &Graph{Nodes: []Node{
		{ID: "fanout"}, // nil Action
		{ID: "a", Deps: []string{"fanout"}, Action: ok()},
	}}
	res := g.Run(context.Background())
	if !res.Success() {
		t.Fatalf("占位节点应立即成功: %+v", res)
	}
}

// TestRunContextCancel ctx 取消:未启动节点收尾为 skipped,已启动的
// Action 感知 ctx 提前退出。
func TestRunContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	g := &Graph{MaxConcurrency: 1, Nodes: []Node{
		{ID: "a", Action: func(c context.Context) error {
			cancel() // 第一个节点执行中取消全局 ctx
			return nil
		}},
		{ID: "b", Deps: []string{"a"}, Action: func(context.Context) error {
			t.Fatal("取消后 b 不应执行")
			return nil
		}},
	}}
	res := g.Run(ctx)
	if res.Nodes[findRes(t, res, "a")].Status != StatusSucceeded {
		t.Fatalf("a 已在跑且成功: %+v", res.Nodes)
	}
	if res.Nodes[findRes(t, res, "b")].Status != StatusSkipped {
		t.Fatalf("取消后 b 应 skipped 收尾: %+v", res.Nodes)
	}
}

// TestRunContextCancelDeterministic 取消竞态回归锚:Action 内 cancel() 先于
// finish() 落账的窗口里,在跑节点的成功终态不得被取消清扫误标 skipped
// (2026-10 CI 两次 flake 的根因,修复=清扫只埋未启动节点)。50 次全绿为过。
func TestRunContextCancelDeterministic(t *testing.T) {
	for i := 0; i < 50; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		g := &Graph{MaxConcurrency: 1, Nodes: []Node{
			{ID: "a", Action: func(c context.Context) error {
				cancel()
				return nil
			}},
			{ID: "b", Deps: []string{"a"}, Action: func(context.Context) error {
				t.Fatal("取消后 b 不应执行")
				return nil
			}},
		}}
		res := g.Run(ctx)
		if res.Nodes[findRes(t, res, "a")].Status != StatusSucceeded {
			t.Fatalf("round %d: a 取消前已成功,不得被清扫误标: %+v", i, res.Nodes)
		}
		if res.Nodes[findRes(t, res, "b")].Status != StatusSkipped {
			t.Fatalf("round %d: b 应 skipped: %+v", i, res.Nodes)
		}
	}
}

// TestRunInvalidGraphSkipped 未过 Validate 的带环图:Run 不死锁不
// panic,环上节点 skipped 收尾。
func TestRunInvalidGraphSkipped(t *testing.T) {
	g := &Graph{Nodes: []Node{
		{ID: "ok"},
		{ID: "x", Deps: []string{"y"}},
		{ID: "y", Deps: []string{"x"}},
	}}
	done := make(chan *Result, 1)
	go func() { done <- g.Run(context.Background()) }()
	select {
	case res := <-done:
		if res.Nodes[findRes(t, res, "ok")].Status != StatusSucceeded {
			t.Fatalf("环外节点应正常执行: %+v", res.Nodes)
		}
		for _, id := range []string{"x", "y"} {
			if res.Nodes[findRes(t, res, id)].Status != StatusSkipped {
				t.Fatalf("环上节点 %q 应 skipped 收尾: %+v", id, res.Nodes)
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("带环图 Run 死锁")
	}
}

// TestRunDiamondRepeat 并发钻石图 30 次重跑:结果集(谁成功)完全一致,
// StartOrder 首元素恒为源点——并发交错不影响结果确定性。
func TestRunDiamondRepeat(t *testing.T) {
	for i := 0; i < 30; i++ {
		g := &Graph{MaxConcurrency: 2, Nodes: []Node{
			{ID: "src", Action: ok()},
			{ID: "left", Deps: []string{"src"}, Action: ok()},
			{ID: "right", Deps: []string{"src"}, Action: ok()},
			{ID: "sink", Deps: []string{"left", "right"}, Action: ok()},
		}}
		res := g.Run(context.Background())
		if !res.Success() {
			t.Fatalf("第 %d 次应全成功: %+v", i, res)
		}
		if res.StartOrder[0] != "src" {
			t.Fatalf("源点必须最先启动: %v", res.StartOrder)
		}
		if res.StartOrder[3] != "sink" {
			t.Fatalf("汇聚点必须最后启动: %v", res.StartOrder)
		}
	}
}

// TestRunMaxConcurrencyDefault 零/负并发上限按 1(串行)处理。
func TestRunMaxConcurrencyDefault(t *testing.T) {
	for _, mc := range []int{0, -3} {
		var peak int32
		g := &Graph{MaxConcurrency: mc}
		for i := 0; i < 3; i++ {
			g.Nodes = append(g.Nodes, Node{ID: fmt.Sprintf("p%d", i), Action: func(context.Context) error {
				c := atomic.AddInt32(&peak, 1)
				time.Sleep(2 * time.Millisecond)
				atomic.AddInt32(&peak, -1)
				if c > 1 {
					return fmt.Errorf("并发 %d 超串行上限", c)
				}
				return nil
			}})
		}
		if !g.Run(context.Background()).Success() {
			t.Fatalf("MaxConcurrency=%d 应串行执行", mc)
		}
	}
}

func ok() func(context.Context) error { return func(context.Context) error { return nil } }

func assertSeq(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("序列长度 %d ≠ %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 个启动 %q ≠ %q (全序列 %v)", i, got[i], want[i], got)
		}
	}
}

func findRes(t *testing.T, res *Result, id string) int {
	t.Helper()
	for i := range res.Nodes {
		if res.Nodes[i].ID == id {
			return i
		}
	}
	t.Fatalf("结果里找不到节点 %q", id)
	return -1
}
