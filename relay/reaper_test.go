package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/QiuShichang/go-rmm/proto"
)

// TestReaperReapsStaleDispatched 验证 P1-2 修复：超时的 dispatched 任务被回退 pending。
func TestReaperReapsStaleDispatched(t *testing.T) {
	dir := t.TempDir()
	s, _ := OpenStore(filepath.Join(dir, "reaper.db"))
	defer s.Close()

	// 插入一个 dispatched 任务，UpdatedAt 设为很久以前（模拟超时）
	old := time.Now().Add(-10 * time.Minute).UnixMilli()
	task := proto.TaskRequest{TaskID: "stale-1", Type: proto.TaskPowerShell, AgentID: "a1", Timeout: 30}
	s.SaveTask(StoredTask{Request: task, Status: StatusDispatched, UpdatedAt: old})

	// 再插一个新鲜的 dispatched（不应被 reap）
	fresh := proto.TaskRequest{TaskID: "fresh-1", Type: proto.TaskPowerShell, AgentID: "a1", Timeout: 30}
	s.SaveTask(StoredTask{Request: fresh, Status: StatusDispatched, UpdatedAt: time.Now().UnixMilli()})

	r := NewRelay(Config{})
	r.WithStore(s)

	// 执行 reap
	r.reapStaleTasks()

	// stale-1 应回退 pending
	st, _ := s.GetTask("stale-1")
	if st.Status != StatusPending {
		t.Fatalf("超时的 dispatched 应回退 pending，得到 %s", st.Status)
	}
	// fresh-1 应保持 dispatched
	ft, _ := s.GetTask("fresh-1")
	if ft.Status != StatusDispatched {
		t.Fatalf("新鲜的 dispatched 不应被 reap，得到 %s", ft.Status)
	}
}

// TestReaperAcknowledgedNeedsLongerTimeout 验证 P1-5 修复：
// acknowledged 任务用更长超时阈值，不会与 dispatched 同等回退。
func TestReaperAcknowledgedNeedsLongerTimeout(t *testing.T) {
	dir := t.TempDir()
	s, _ := OpenStore(filepath.Join(dir, "ack.db"))
	defer s.Close()

	// acknowledged 任务，超时 30s，5 分钟前更新
	// dispatched 阈值=120s（会 reap），acknowledged 阈值=300s（5 分钟，边界值不会 reap）
	old := time.Now().Add(-4 * time.Minute).UnixMilli()
	task := proto.TaskRequest{TaskID: "ack-1", Type: proto.TaskPowerShell, AgentID: "a1", Timeout: 30}
	s.SaveTask(StoredTask{Request: task, Status: StatusAcknowledged, UpdatedAt: old})

	r := NewRelay(Config{})
	r.WithStore(s)
	r.reapStaleTasks()

	// 4 分钟 < acknowledged 阈值 5 分钟，不应被 reap
	st, _ := s.GetTask("ack-1")
	if st.Status != StatusAcknowledged {
		t.Fatalf("acknowledged 4分钟不应被 reap（阈值300s），得到 %s", st.Status)
	}
}

// TestRecoverOnlyDispatchedNotAcknowledged 验证 P1-5 修复：
// recoverInFlightTasks 只重置 dispatched，不动 acknowledged。
func TestRecoverOnlyDispatchedNotAcknowledged(t *testing.T) {
	dir := t.TempDir()
	s, _ := OpenStore(filepath.Join(dir, "recover.db"))
	defer s.Close()

	// dispatched 任务（应被恢复为 pending）
	s.SaveTask(StoredTask{
		Request: proto.TaskRequest{TaskID: "d-1", Type: proto.TaskPowerShell, AgentID: "a1"},
		Status:  StatusDispatched,
	})
	// acknowledged 任务（不应被恢复）
	s.SaveTask(StoredTask{
		Request: proto.TaskRequest{TaskID: "a-1", Type: proto.TaskPowerShell, AgentID: "a1"},
		Status:  StatusAcknowledged,
	})

	r := NewRelay(Config{})
	r.WithStore(s)
	r.recoverInFlightTasks()

	d, _ := s.GetTask("d-1")
	if d.Status != StatusPending {
		t.Fatalf("dispatched 应恢复为 pending，得到 %s", d.Status)
	}
	a, _ := s.GetTask("a-1")
	if a.Status != StatusAcknowledged {
		t.Fatalf("acknowledged 不应被动，得到 %s", a.Status)
	}
}

// TestMaxAttemptsPreventsPoisonTask 验证 E2/W1：达到 maxAttempts 的任务转 failed。
func TestMaxAttemptsPreventsPoisonTask(t *testing.T) {
	dir := t.TempDir()
	s, _ := OpenStore(filepath.Join(dir, "maxattempt.db"))
	defer s.Close()

	// 插入一个已投递 MaxTaskAttempts 次的任务
	task := proto.TaskRequest{TaskID: "poison", Type: proto.TaskPowerShell, AgentID: "ghost", Timeout: 5}
	s.SaveTask(StoredTask{Request: task, Status: StatusPending, Attempt: MaxTaskAttempts})

	r := NewRelay(Config{})
	r.WithStore(s)

	// dispatchTask 应因 maxAttempts 拒绝（ghost agent 不在线，conn 为 nil 也不影响——
	// 检查在写 WS 前）
	err := r.dispatchTask(&wsConn{id: "fake"}, task)
	if err == nil {
		t.Fatal("达到 maxAttempts 的任务应被拒绝投递")
	}

	// 任务应已转 failed
	t2, _ := s.GetTask("poison")
	if t2.Status != StatusFailed {
		t.Fatalf("应转 failed，得到 %s", t2.Status)
	}
}
