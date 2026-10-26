package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// captureStdout 劫持 stdout 执行 fn,返回期间写入的内容(用于断言 CLI 输出)。
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("创建管道失败: %v", err)
	}
	os.Stdout = w
	fn()
	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	return string(out)
}

// withJSONMode 临时开启 JSON 输出模式。
func withJSONMode(t *testing.T) {
	t.Helper()
	jsonMode = true
	t.Cleanup(func() { jsonMode = false })
}

// TestWaitResultJSONMode 实证 task run 等待终态时 stdout 为纯 JSON
// (无 "等待结果..."/轮询点污染)——修复前 printResult 输出人类可读文本块,
// 脚本消费者无法解析(使用会话 2026-08-06 "-o json 无效"实证)。
func TestWaitResultJSONMode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/api/tasks/t1") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"task":{"status":"completed"},"result":{"task_id":"t1","success":true,"exit_code":0,"stdout":"ok"}}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	withJSONMode(t)
	var out string
	var done bool
	out = captureStdout(t, func() {
		done, _ = waitResult(context.Background(), config{relay: srv.URL}, "t1", 5)
	})
	if !done {
		t.Fatal("任务应等待到终态")
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &v); err != nil {
		t.Fatalf("jsonMode 下 stdout 应为纯 JSON,实际: %q (%v)", out, err)
	}
	if v["status"] != "completed" {
		t.Fatalf("status 应为 completed,实际 %v", v["status"])
	}
	res, _ := v["result"].(map[string]any)
	if res == nil || res["success"] != true {
		t.Fatalf("result.success 应为 true,实际 %v", v["result"])
	}
	if v["task_id"] != "t1" {
		t.Fatalf("task_id 应为 t1,实际 %v", v["task_id"])
	}
}

// TestWaitResultTextMode 文本模式回归:仍输出人类可读结果块。
func TestWaitResultTextMode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/api/tasks/t2") {
			_, _ = w.Write([]byte(`{"task":{"status":"completed"},"result":{"task_id":"t2","success":true,"exit_code":0,"stdout":"hello"}}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	var done bool
	out := captureStdout(t, func() {
		done, _ = waitResult(context.Background(), config{relay: srv.URL}, "t2", 5)
	})
	if !done {
		t.Fatal("任务应等待到终态")
	}
	if !strings.Contains(out, "=== 任务结果 ===") || !strings.Contains(out, "hello") {
		t.Fatalf("文本模式应输出结果块,实际: %q", out)
	}
}

// TestTaskListJSONMode 实证 task list -o json 输出任务数组(修复前仅表格文本)。
func TestTaskListJSONMode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/tasks") {
			_, _ = w.Write([]byte(`{"tasks":[{"request":{"task_id":"t1","type":"powershell","agent_id":"a1"},"status":"completed","created_at":1786900000000}],"count":1,"enabled":true}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	withJSONMode(t)
	out := captureStdout(t, func() {
		if err := taskList(config{relay: srv.URL}, []string{"-n", "5"}); err != nil {
			t.Errorf("taskList 失败: %v", err)
		}
	})
	var v struct {
		Tasks []taskListItem `json:"tasks"`
		Count int            `json:"count"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &v); err != nil {
		t.Fatalf("jsonMode 下 task list 应输出纯 JSON,实际: %q (%v)", out, err)
	}
	if v.Count != 1 || len(v.Tasks) != 1 || v.Tasks[0].Request.TaskID != "t1" {
		t.Fatalf("tasks 数据不符: %+v", v)
	}
}
