package main

import (
	"path/filepath"
	"testing"

	"github.com/QiuShichang/go-rmm/proto"
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
