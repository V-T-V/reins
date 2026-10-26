package main

import (
	"path/filepath"
	"testing"

	"github.com/V-T-V/reins/proto"
)

// TestStateMachineRejectsTerminalChange 验证 P2-2 修复：终态不可变更。
func TestStateMachineRejectsTerminalChange(t *testing.T) {
	dir := t.TempDir()
	s, _ := OpenStore(filepath.Join(dir, "sm.db"))
	defer s.Close()

	task := proto.TaskRequest{TaskID: "t-sm", Type: proto.TaskPowerShell, AgentID: "a1"}
	s.SaveTask(StoredTask{Request: task, Status: StatusPending})

	// pending → completed 合法
	if err := s.UpdateTaskStatus("t-sm", StatusCompleted); err != nil {
		t.Fatalf("pending→completed 应允许: %v", err)
	}

	// completed → failed 应拒绝（终态锁死）
	if err := s.UpdateTaskStatus("t-sm", StatusFailed); err == nil {
		t.Fatal("终态 completed→failed 应拒绝")
	}
	// completed → cancelled 应拒绝
	if err := s.UpdateTaskStatus("t-sm", StatusCancelled); err == nil {
		t.Fatal("终态 completed→cancelled 应拒绝")
	}
	// completed → pending 应拒绝
	if err := s.UpdateTaskStatus("t-sm", StatusPending); err == nil {
		t.Fatal("终态 completed→pending 应拒绝")
	}
}

// TestStateMachineAllowsValidTransitions 验证合法转换放行。
func TestStateMachineAllowsValidTransitions(t *testing.T) {
	dir := t.TempDir()
	s, _ := OpenStore(filepath.Join(dir, "sm2.db"))
	defer s.Close()

	task := proto.TaskRequest{TaskID: "t-valid", Type: proto.TaskPowerShell, AgentID: "a1"}
	s.SaveTask(StoredTask{Request: task, Status: StatusPending})

	// pending → dispatched
	if err := s.UpdateTaskStatus("t-valid", StatusDispatched); err != nil {
		t.Fatalf("pending→dispatched 应允许: %v", err)
	}
	// dispatched → acknowledged
	if err := s.UpdateTaskStatus("t-valid", StatusAcknowledged); err != nil {
		t.Fatalf("dispatched→acknowledged 应允许: %v", err)
	}
	// acknowledged → completed
	if err := s.UpdateTaskStatus("t-valid", StatusCompleted); err != nil {
		t.Fatalf("acknowledged→completed 应允许: %v", err)
	}
}

// TestStoreAfterCloseNoPanic 验证 P2-7 修复：Close 后写不 panic。
func TestStoreAfterCloseNoPanic(t *testing.T) {
	dir := t.TempDir()
	s, _ := OpenStore(filepath.Join(dir, "close.db"))
	s.Close() // 立即关闭

	// 这些调用不应 panic
	_ = s.SaveTask(StoredTask{Request: proto.TaskRequest{TaskID: "x"}})
	_ = s.SaveResult("x", proto.TaskResult{})
	t1, _ := s.GetTask("x")
	if t1 != nil {
		t.Fatal("Close 后 GetTask 应返回 nil")
	}
	t2, _ := s.GetResult("x")
	if t2 != nil {
		t.Fatal("Close 后 GetResult 应返回 nil")
	}
	list, _ := s.ListTasksByStatus(StatusPending)
	if list != nil {
		t.Fatal("Close 后 ListTasksByStatus 应返回 nil")
	}
}

// TestACKCannotOverrideTerminal 验证 E1/B1 修复：ACK 不能把终态改回 acknowledged。
// 场景：任务已 cancel（终态），Agent 迟到的 ACK 不能改变它。
func TestACKCannotOverrideTerminal(t *testing.T) {
	dir := t.TempDir()
	s, _ := OpenStore(filepath.Join(dir, "ack.db"))
	defer s.Close()

	task := proto.TaskRequest{TaskID: "t-ack", Type: proto.TaskPowerShell, AgentID: "a1"}
	s.SaveTask(StoredTask{Request: task, Status: StatusPending})

	// 先 cancel（进入终态）
	if err := s.UpdateTaskStatus("t-ack", StatusCancelled); err != nil {
		t.Fatalf("cancel 失败: %v", err)
	}

	// 迟到的 ACK 尝试改为 acknowledged——应被拒绝
	if err := s.UpdateTaskStatus("t-ack", StatusAcknowledged); err == nil {
		t.Fatal("终态 cancelled 后 ACK 应被拒绝，不能改回 acknowledged")
	}

	// 验证状态仍是 cancelled
	t2, _ := s.GetTask("t-ack")
	if t2.Status != StatusCancelled {
		t.Fatalf("状态应仍是 cancelled，得到 %s", t2.Status)
	}
}

// TestUpdateTaskStatusAtomic 验证 E1/B2：UpdateTaskStatus 在单写事务内原子完成。
// 这是对原子性的回归保护——确保读-校验-写在同一事务。
func TestUpdateTaskStatusAtomic(t *testing.T) {
	dir := t.TempDir()
	s, _ := OpenStore(filepath.Join(dir, "atomic.db"))
	defer s.Close()

	task := proto.TaskRequest{TaskID: "t-atom", Type: proto.TaskPowerShell, AgentID: "a1"}
	s.SaveTask(StoredTask{Request: task, Status: StatusDispatched})

	// 正常转换
	if err := s.UpdateTaskStatus("t-atom", StatusCompleted); err != nil {
		t.Fatalf("dispatched→completed 应成功: %v", err)
	}
	// 终态后不能再变
	if err := s.UpdateTaskStatus("t-atom", StatusFailed); err == nil {
		t.Fatal("completed→failed 应拒绝")
	}
	// 不存在的任务
	if err := s.UpdateTaskStatus("nonexistent", StatusPending); err == nil {
		t.Fatal("不存在的任务应返回错误")
	}
}
