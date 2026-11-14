package taskstore

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/V-T-V/reins/proto"
)

// TestStoreTaskLifecycle 验证任务的保存/查询/状态流转。
func TestStoreTaskLifecycle(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("OpenStore 失败: %v", err)
	}
	defer s.Close()

	// 保存一个 pending 任务
	task := proto.TaskRequest{
		TaskID:  "t-lifecycle",
		Type:    proto.TaskPowerShell,
		AgentID: "agent-1",
	}
	if err := s.SaveTask(StoredTask{Request: task, Status: StatusPending}); err != nil {
		t.Fatalf("SaveTask 失败: %v", err)
	}

	// 读回
	got, err := s.GetTask("t-lifecycle")
	if err != nil || got == nil {
		t.Fatalf("GetTask 失败: %v", err)
	}
	if got.Status != StatusPending || got.Request.Type != proto.TaskPowerShell {
		t.Fatalf("任务数据不符: %+v", got)
	}

	// 更新为 dispatched
	got.Status = StatusDispatched
	got.Attempt = 1
	if err := s.SaveTask(*got); err != nil {
		t.Fatalf("更新失败: %v", err)
	}
	got2, _ := s.GetTask("t-lifecycle")
	if got2.Status != StatusDispatched || got2.Attempt != 1 {
		t.Fatalf("更新后数据不符: %+v", got2)
	}

	// 按 status 查询
	pending, _ := s.ListTasksByStatus(StatusPending)
	if len(pending) != 0 {
		t.Fatalf("应无 pending，有 %d", len(pending))
	}
	dispatched, _ := s.ListTasksByStatus(StatusDispatched)
	if len(dispatched) != 1 {
		t.Fatalf("应有 1 个 dispatched，有 %d", len(dispatched))
	}
}

// TestStoreResultSaveGet 验证结果保存与读取。
func TestStoreResultSaveGet(t *testing.T) {
	dir := t.TempDir()
	s, _ := OpenStore(filepath.Join(dir, "r.db"))
	defer s.Close()

	res := proto.TaskResult{TaskID: "t-r", Success: true, Stdout: "hello"}
	if err := s.SaveResult("t-r", res); err != nil {
		t.Fatalf("SaveResult 失败: %v", err)
	}
	got, err := s.GetResult("t-r")
	if err != nil || got == nil {
		t.Fatalf("GetResult 失败: %v", err)
	}
	if got.Stdout != "hello" || !got.Success {
		t.Fatalf("结果数据不符: %+v", got)
	}
}

// TestStoreSurvivesReopen 验证 R2 修复核心：重新打开同一个 db 文件，数据仍在。
func TestStoreSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "persist.db")

	// 第一次打开，写入任务
	s1, _ := OpenStore(dbPath)
	task := proto.TaskRequest{TaskID: "t-survive", Type: proto.TaskScript, AgentID: "a1"}
	s1.SaveTask(StoredTask{Request: task, Status: StatusPending})
	s1.Close()

	// 第二次打开（模拟 Relay 重启）
	s2, err := OpenStore(dbPath)
	if err != nil {
		t.Fatalf("重新打开失败: %v", err)
	}
	defer s2.Close()

	got, err := s2.GetTask("t-survive")
	if err != nil || got == nil {
		t.Fatalf("重启后任务应仍在: %v", err)
	}
	if got.Request.Type != proto.TaskScript {
		t.Fatalf("任务类型不符: %+v", got)
	}

	// pending 任务应能被查出来（供 recoverInFlightTasks/补投用）
	pending, _ := s2.ListTasksByStatus(StatusPending)
	if len(pending) != 1 {
		t.Fatalf("应能查出 1 个 pending，有 %d", len(pending))
	}
}

// TestStoreListTasksLimit 验证 ListTasks 的 limit 与顺序。
func TestStoreListTasksLimit(t *testing.T) {
	dir := t.TempDir()
	s, _ := OpenStore(filepath.Join(dir, "list.db"))
	defer s.Close()

	for i := 0; i < 10; i++ {
		s.SaveTask(StoredTask{
			Request: proto.TaskRequest{TaskID: "t-" + string(rune('a'+i))},
			Status:  StatusCompleted,
		})
	}

	all, _ := s.ListTasks(0)
	if len(all) != 10 {
		t.Fatalf("应返回 10 个，有 %d", len(all))
	}
	limited, _ := s.ListTasks(3)
	if len(limited) != 3 {
		t.Fatalf("limit=3 应返回 3 个，有 %d", len(limited))
	}
}

// TestStatusIndexConsistency 验证 C2/F1：多次状态翻动后索引与主表一致。
// 这是 F1 状态索引优化的关键回归保护——确保 SaveTask/UpdateTaskStatus
// 维护的 bucketTasksByStatus 索引在多次变更后不残留旧状态、不丢失新状态。
func TestStatusIndexConsistency(t *testing.T) {
	dir := t.TempDir()
	s, _ := OpenStore(filepath.Join(dir, "idx.db"))
	defer s.Close()

	task := proto.TaskRequest{TaskID: "idx-task", Type: proto.TaskPowerShell, AgentID: "a"}
	s.SaveTask(StoredTask{Request: task, Status: StatusPending})

	// 多次状态翻动
	transitions := []string{
		StatusDispatched, StatusAcknowledged, StatusCompleted,
	}
	for _, target := range transitions {
		if err := s.UpdateTaskStatus("idx-task", target); err != nil {
			t.Fatalf("转换到 %s 失败: %v", target, err)
		}
		// 每次翻动后验证：旧状态索引不含此任务，新状态索引含此任务
		for _, st := range []string{StatusPending, StatusDispatched, StatusAcknowledged, StatusCompleted, StatusFailed} {
			got, _ := s.ListTasksByStatus(st)
			found := false
			for _, g := range got {
				if g.Request.TaskID == "idx-task" {
					found = true
				}
			}
			if st == target && !found {
				t.Fatalf("状态 %s 索引应含 idx-task（当前态=%s）", st, target)
			}
			if st != target && found {
				t.Fatalf("状态 %s 索引不应含 idx-task（当前态=%s，残留旧索引）", st, target)
			}
		}
	}

	// 最终态 completed 后，UpdateTaskStatus 应拒绝再变更（终态锁）
	if err := s.UpdateTaskStatus("idx-task", StatusPending); err == nil {
		t.Fatal("终态 completed 后不应允许变更")
	}
}

// TestStatusIndexMultipleTasks 验证多任务场景下索引正确隔离。
func TestStatusIndexMultipleTasks(t *testing.T) {
	dir := t.TempDir()
	s, _ := OpenStore(filepath.Join(dir, "multi.db"))
	defer s.Close()

	// 3 个任务不同状态
	s.SaveTask(StoredTask{Request: proto.TaskRequest{TaskID: "t1"}, Status: StatusPending})
	s.SaveTask(StoredTask{Request: proto.TaskRequest{TaskID: "t2"}, Status: StatusDispatched})
	s.SaveTask(StoredTask{Request: proto.TaskRequest{TaskID: "t3"}, Status: StatusCompleted})

	pending, _ := s.ListTasksByStatus(StatusPending)
	dispatched, _ := s.ListTasksByStatus(StatusDispatched)
	completed, _ := s.ListTasksByStatus(StatusCompleted)

	if len(pending) != 1 || pending[0].Request.TaskID != "t1" {
		t.Fatalf("pending 应只有 t1，得到 %+v", pending)
	}
	if len(dispatched) != 1 || dispatched[0].Request.TaskID != "t2" {
		t.Fatalf("dispatched 应只有 t2，得到 %+v", dispatched)
	}
	if len(completed) != 1 || completed[0].Request.TaskID != "t3" {
		t.Fatalf("completed 应只有 t3，得到 %+v", completed)
	}
}

// TestListTasksQueryEquivalence ListTasksQuery(子串预筛)与
// "全量取出+调用方过滤"逐位等价(性能轮 2026-11-14)。
// 含 JSON 非安全过滤值回退路径(含转义字符的值必须走全量解析)。
func TestListTasksQueryEquivalence(t *testing.T) {
	// JSON 安全与不安全的 agent/type 各一组
	agents := []string{"agent-1", "agent-2", `ag"quote\`}
	types := []string{proto.TaskPowerShell, proto.TaskScript, `ty"pe\`}
	statuses := []string{StatusCompleted, StatusDispatched}

	seed := func(s TaskStore) {
		for i, ag := range agents {
			for j, ty := range types {
				st := StatusCompleted
				if (i+j)%2 == 1 {
					st = StatusDispatched
				}
				if err := s.SaveTask(StoredTask{
					Request: proto.TaskRequest{
						TaskID:  fmt.Sprintf("t-%d-%d", i, j),
						Type:    ty,
						AgentID: ag,
					},
					Status: st, CreatedAt: int64(1000 + i*10 + j), UpdatedAt: int64(1000 + i*10 + j),
				}); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	refFilter := func(all []StoredTask, q TaskQuery) []StoredTask {
		statusSet := map[string]bool{}
		for _, st := range q.Statuses {
			statusSet[st] = true
		}
		var out []StoredTask
		for _, t := range all {
			if len(statusSet) > 0 && !statusSet[t.Status] {
				continue
			}
			if q.AgentID != "" && t.Request.AgentID != q.AgentID {
				continue
			}
			if q.Type != "" && t.Request.Type != q.Type {
				continue
			}
			out = append(out, t)
		}
		return out
	}

	impls := map[string]TaskStore{}
	if s, err := OpenStore(filepath.Join(t.TempDir(), "q.db")); err != nil {
		t.Fatal(err)
	} else {
		defer s.Close()
		impls["bolt"] = s
	}
	impls["mem"] = NewMemoryStore()

	queries := []TaskQuery{
		{},
		{AgentID: "agent-1"},
		{Type: proto.TaskScript},
		{AgentID: "agent-2", Type: proto.TaskPowerShell},
		{Statuses: statuses[:1]},
		{Statuses: statuses, AgentID: "agent-1"},
		{AgentID: `ag"quote\`}, // JSON 不安全:预筛须回退
		{Type: `ty"pe\`},       // JSON 不安全:预筛须回退
		{Statuses: statuses, Type: proto.TaskScript, AgentID: `ag"quote\`},
	}
	for name, s := range impls {
		seed(s)
		all, err := s.ListTasks(0)
		if err != nil {
			t.Fatalf("%s: ListTasks: %v", name, err)
		}
		for i, q := range queries {
			got, err := s.ListTasksQuery(q)
			if err != nil {
				t.Fatalf("%s query#%d: %v", name, i, err)
			}
			want := refFilter(all, q)
			if len(got) != len(want) {
				t.Fatalf("%s query#%d: got=%d want=%d (q=%+v)", name, i, len(got), len(want), q)
			}
			gotSet := map[string]StoredTask{}
			for _, tk := range got {
				gotSet[tk.Request.TaskID] = tk
			}
			for _, tk := range want {
				g, ok := gotSet[tk.Request.TaskID]
				if !ok || g.Status != tk.Status || g.Request.AgentID != tk.Request.AgentID || g.Request.Type != tk.Request.Type {
					t.Fatalf("%s query#%d: 记录 %s 不等价: got=%+v want=%+v", name, i, tk.Request.TaskID, g, tk)
				}
			}
		}
	}
}

// TestListTasksQueryLimit Limit 按已收录(匹配)数计数,两实现口径一致。
func TestListTasksQueryLimit(t *testing.T) {
	newSeeded := func(s TaskStore) {
		for i := 0; i < 10; i++ {
			ag := "agent-1"
			if i >= 5 {
				ag = "agent-2"
			}
			if err := s.SaveTask(StoredTask{
				Request: proto.TaskRequest{TaskID: fmt.Sprintf("lt-%02d", i), AgentID: ag},
				Status:  StatusCompleted,
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	boltS, err := OpenStore(filepath.Join(t.TempDir(), "limit.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer boltS.Close()
	memS := NewMemoryStore()
	for _, s := range []TaskStore{boltS, memS} {
		newSeeded(s)
		// 无过滤 Limit=3
		got, err := s.ListTasksQuery(TaskQuery{Limit: 3})
		if err != nil || len(got) != 3 {
			t.Fatalf("Limit=3 got=%d err=%v", len(got), err)
		}
		// agent 过滤下 Limit=4:匹配 5 条,取 4(全量扫 10 条不止步于第 4 条记录)
		got, err = s.ListTasksQuery(TaskQuery{AgentID: "agent-2", Limit: 4})
		if err != nil || len(got) != 4 {
			t.Fatalf("agent 过滤 Limit=4 got=%d err=%v", len(got), err)
		}
		for _, tk := range got {
			if tk.Request.AgentID != "agent-2" {
				t.Fatalf("过滤泄漏: %+v", tk)
			}
		}
		// Statuses+Limit
		got, err = s.ListTasksQuery(TaskQuery{Statuses: []string{StatusCompleted}, Limit: 2})
		if err != nil || len(got) != 2 {
			t.Fatalf("status Limit=2 got=%d err=%v", len(got), err)
		}
	}
}
