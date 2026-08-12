// Package main — /api/tasks 与 /api/crons 过滤参数测试(2026-08-12 控制台档案面板)。
//
// 覆盖:
//   - handleListTasks 的 ?agent_id / ?status / 组合 / 无参回归
//   - handleListCrons 的 ?agent_id 过滤 + next_run 运行时计算
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/QiuShichang/go-rmm/proto"
)

// saveTaskForTest 往 store 存一个任务。
func saveTaskForTest(r *Relay, taskID, agentID, status string, createdAgoMs int64) {
	now := time.Now().UnixMilli()
	_ = r.store.SaveTask(StoredTask{
		Request:   proto.TaskRequest{TaskID: taskID, AgentID: agentID, Type: "powershell"},
		Status:    status,
		Attempt:   1,
		CreatedAt: now - createdAgoMs,
		UpdatedAt: now,
	})
}

// getTasks 调 /api/tasks 并返回解析后的任务数组(每条为 map,含 request/status 等)。
func getTasks(t *testing.T, url string) []map[string]any {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s 应 200, got %d", url, resp.StatusCode)
	}
	var data struct {
		Tasks []map[string]any `json:"tasks"`
		Count int              `json:"count"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		t.Fatal(err)
	}
	return data.Tasks
}

func taskAgentID(t *testing.T, task map[string]any) string {
	req, ok := task["request"].(map[string]any)
	if !ok {
		t.Fatalf("task 缺 request 字段: %v", task)
	}
	a, _ := req["agent_id"].(string)
	return a
}

// --- /api/tasks 过滤 ---

func TestListTasksFilterByAgent(t *testing.T) {
	r, srv := newTestRelay(t)
	defer srv.Close()
	saveTaskForTest(r, "t1", "agentA", "completed", 3000)
	saveTaskForTest(r, "t2", "agentA", "completed", 2000)
	saveTaskForTest(r, "t3", "agentB", "completed", 1000)

	tasks := getTasks(t, srv.URL+"/api/tasks?agent_id=agentA")
	if len(tasks) != 2 {
		t.Fatalf("agentA 应 2 个任务, got %d", len(tasks))
	}
	for _, tk := range tasks {
		if taskAgentID(t, tk) != "agentA" {
			t.Errorf("过滤后应全是 agentA, got %s", taskAgentID(t, tk))
		}
	}
}

func TestListTasksFilterByStatus(t *testing.T) {
	r, srv := newTestRelay(t)
	defer srv.Close()
	saveTaskForTest(r, "t1", "agentA", "completed", 3000)
	saveTaskForTest(r, "t2", "agentA", "failed", 2000)
	saveTaskForTest(r, "t3", "agentA", "pending", 1000)

	tasks := getTasks(t, srv.URL+"/api/tasks?status=completed")
	if len(tasks) != 1 {
		t.Fatalf("completed 应 1 个, got %d", len(tasks))
	}
	if tasks[0]["status"] != "completed" {
		t.Errorf("应 completed, got %v", tasks[0]["status"])
	}
}

func TestListTasksFilterAgentAndStatus(t *testing.T) {
	r, srv := newTestRelay(t)
	defer srv.Close()
	saveTaskForTest(r, "t1", "agentA", "completed", 5000)
	saveTaskForTest(r, "t2", "agentA", "failed", 4000)
	saveTaskForTest(r, "t3", "agentA", "completed", 3000)
	saveTaskForTest(r, "t4", "agentB", "completed", 2000)

	tasks := getTasks(t, srv.URL+"/api/tasks?agent_id=agentA&status=completed")
	if len(tasks) != 2 {
		t.Fatalf("agentA+completed 应 2 个, got %d", len(tasks))
	}
	for _, tk := range tasks {
		if taskAgentID(t, tk) != "agentA" || tk["status"] != "completed" {
			t.Errorf("组合过滤不符: agent=%s status=%v", taskAgentID(t, tk), tk["status"])
		}
	}
}

// TestListTasksNoFilter 回归:无参仍返回全部(不破坏 CLI/现有前端)。
func TestListTasksNoFilter(t *testing.T) {
	r, srv := newTestRelay(t)
	defer srv.Close()
	saveTaskForTest(r, "t1", "agentA", "completed", 3000)
	saveTaskForTest(r, "t2", "agentB", "failed", 2000)
	saveTaskForTest(r, "t3", "agentC", "pending", 1000)

	tasks := getTasks(t, srv.URL+"/api/tasks")
	if len(tasks) != 3 {
		t.Fatalf("无过滤应 3 个, got %d(回归破坏?)", len(tasks))
	}
}

// --- /api/crons 过滤 + next_run ---

// newCronTestRelay 用 bbolt Store 构造(支持 SaveCronRule),并挂 Scheduler(不 Start)。
func newCronTestRelay(t *testing.T) (*Relay, *httptest.Server, *Store) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "tasks.db")
	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() }) // Windows 下不关 bbolt 则 TempDir 清理失败
	r := &Relay{
		cfg:       Config{Addr: ":0"},
		agents:    NewRegistry(),
		console:   NewEventBus(),
		policy:    DefaultCommandPolicy(),
		store:     store,
		scheduler: NewScheduler(store, nil), // 不 Start,relay=nil(executeRule 不触发)
	}
	mux := http.NewServeMux()
	r.registerHTTP(mux)
	srv := httptest.NewServer(mux)
	return r, srv, store
}

func TestListCronsFilterByAgent(t *testing.T) {
	_, srv, store := newCronTestRelay(t)
	defer srv.Close()
	_ = store.SaveCronRule(CronRule{ID: "c1", AgentID: "agentA", CronExpr: "0 3 * * *", TaskType: "powershell", Enabled: true})
	_ = store.SaveCronRule(CronRule{ID: "c2", AgentID: "agentB", CronExpr: "0 4 * * *", TaskType: "script", Enabled: true})

	resp, err := http.Get(srv.URL + "/api/crons?agent_id=agentA")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var data struct {
		Crons []map[string]any `json:"crons"`
		Count int              `json:"count"`
	}
	json.NewDecoder(resp.Body).Decode(&data)
	if data.Count != 1 {
		t.Fatalf("agentA 应 1 条 cron, got %d", data.Count)
	}
	if data.Crons[0]["agent_id"] != "agentA" {
		t.Errorf("应 agentA, got %v", data.Crons[0]["agent_id"])
	}
}

func TestListCronsNextRunComputed(t *testing.T) {
	_, srv, store := newCronTestRelay(t)
	defer srv.Close()
	now := time.Now().UnixMilli()
	_ = store.SaveCronRule(CronRule{ID: "c1", AgentID: "agentA", CronExpr: "0 3 * * *", TaskType: "powershell", Enabled: true})
	_ = store.SaveCronRule(CronRule{ID: "c2", AgentID: "agentA", CronExpr: "0 4 * * *", TaskType: "script", Enabled: false})

	resp, err := http.Get(srv.URL + "/api/crons")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var data struct {
		Crons []map[string]any `json:"crons"`
	}
	json.NewDecoder(resp.Body).Decode(&data)

	var enabled, disabled map[string]any
	for _, c := range data.Crons {
		if c["id"] == "c1" {
			enabled = c
		}
		if c["id"] == "c2" {
			disabled = c
		}
	}
	if enabled == nil {
		t.Fatal("c1 未返回")
	}
	nr, ok := enabled["next_run"].(float64)
	if !ok {
		t.Fatalf("enabled 规则应有 next_run, got %T %v", enabled["next_run"], enabled["next_run"])
	}
	if int64(nr) <= now {
		t.Errorf("next_run 应在未来, got %d (now %d)", int64(nr), now)
	}
	if _, has := disabled["next_run"]; has {
		t.Errorf("disabled 规则不应有 next_run, got %v", disabled["next_run"])
	}
}
