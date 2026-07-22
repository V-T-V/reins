package main

import (
	"path/filepath"
	"testing"

	"github.com/QiuShichang/go-rmm/proto"
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
