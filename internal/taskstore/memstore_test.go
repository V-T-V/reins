package taskstore

import (
	"testing"

	"github.com/V-T-V/reins/proto"
)

func TestMemoryStoreSaveGet(t *testing.T) {
	s := NewMemoryStore()
	task := StoredTask{
		Request: proto.TaskRequest{TaskID: "t1", Type: "powershell", AgentID: "a1"},
		Status:  StatusPending,
	}
	if err := s.SaveTask(task); err != nil {
		t.Fatalf("SaveTask: %v", err)
	}
	got, err := s.GetTask("t1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != StatusPending {
		t.Errorf("Status = %s", got.Status)
	}
}

func TestMemoryStoreTerminalLock(t *testing.T) {
	s := NewMemoryStore()
	s.SaveTask(StoredTask{
		Request: proto.TaskRequest{TaskID: "t2"},
		Status:  StatusCompleted,
	})
	// 终态不应能回退
	if err := s.ReapToPending("t2"); err == nil {
		t.Error("终态 ReapToPending 应失败")
	}
	if err := s.UpdateTaskStatus("t2", StatusPending); err == nil {
		t.Error("终态 UpdateTaskStatus 应失败")
	}
}

func TestMemoryStoreCAS(t *testing.T) {
	s := NewMemoryStore()
	s.SaveTask(StoredTask{
		Request: proto.TaskRequest{TaskID: "t3"},
		Status:  StatusPending,
	})
	// pending → dispatched CAS 应通过
	if err := s.CASDispatchToPending("t3"); err != nil {
		t.Errorf("pending CAS 应通过: %v", err)
	}
	// 改为 dispatched
	s.UpdateTaskStatus("t3", StatusDispatched)
	// 再 CAS 应失败(已 dispatched)
	if err := s.CASDispatchToPending("t3"); err == nil {
		t.Error("dispatched CAS 应失败")
	}
}

func TestMemoryStoreReapToPending(t *testing.T) {
	s := NewMemoryStore()
	s.SaveTask(StoredTask{
		Request: proto.TaskRequest{TaskID: "t4"},
		Status:  StatusDispatched,
	})
	// dispatched → pending 应通过
	if err := s.ReapToPending("t4"); err != nil {
		t.Errorf("dispatched ReapToPending 应通过: %v", err)
	}
	got, _ := s.GetTask("t4")
	if got.Status != StatusPending {
		t.Errorf("Status = %s, 期望 pending", got.Status)
	}
}

func TestMemoryStoreResult(t *testing.T) {
	s := NewMemoryStore()
	r := proto.TaskResult{TaskID: "t5", Success: true, Stdout: "hello"}
	s.SaveResult("t5", r)
	got, err := s.GetResult("t5")
	if err != nil {
		t.Fatalf("GetResult: %v", err)
	}
	if got.Stdout != "hello" {
		t.Errorf("Stdout = %s", got.Stdout)
	}
}

func TestMemoryStoreListByStatus(t *testing.T) {
	s := NewMemoryStore()
	s.SaveTask(StoredTask{Request: proto.TaskRequest{TaskID: "p1"}, Status: StatusPending})
	s.SaveTask(StoredTask{Request: proto.TaskRequest{TaskID: "d1"}, Status: StatusDispatched})
	s.SaveTask(StoredTask{Request: proto.TaskRequest{TaskID: "c1"}, Status: StatusCompleted})

	pending, _ := s.ListTasksByStatus(StatusPending)
	if len(pending) != 1 || pending[0].Request.TaskID != "p1" {
		t.Errorf("ListTasksByStatus(pending) 期望 1 条 p1, 得到 %d 条", len(pending))
	}
	dispatched, _ := s.ListTasksByStatus(StatusDispatched)
	if len(dispatched) != 1 {
		t.Errorf("ListTasksByStatus(dispatched) 期望 1 条")
	}
	completed, _ := s.ListTasksByStatus(StatusCompleted)
	if len(completed) != 1 {
		t.Errorf("ListTasksByStatus(completed) 期望 1 条")
	}
}
