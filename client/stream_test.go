package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSDKStreamExec(t *testing.T) {
	// Mock SSE server
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("flusher not supported")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)

		// 推送进度
		fmt.Fprintf(w, "event: task_progress\ndata: {\"task_id\":\"t1\",\"percent\":50}\n\n")
		flusher.Flush()

		// 推送结果
		fmt.Fprintf(w, "event: task_result\ndata: {\"data\":{\"task_id\":\"t1\",\"success\":true,\"stdout\":\"done\"}}\n\n")
		flusher.Flush()
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	events := []string{}
	result, err := c.StreamCommand(ctx, "agent-1", "test", 5, func(event string, data json.RawMessage) {
		events = append(events, event)
	})
	if err != nil {
		t.Fatalf("StreamCommand: %v", err)
	}
	if result == nil || !result.Success {
		t.Fatal("应返回成功结果")
	}
	if result.Stdout != "done" {
		t.Errorf("stdout = %s", result.Stdout)
	}
	// 应收到至少 1 个 progress 事件
	found := false
	for _, e := range events {
		if e == "task_progress" {
			found = true
		}
	}
	if !found {
		t.Error("应收到 task_progress 事件")
	}
}

func TestSDKStreamExecError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err := c.StreamCommand(ctx, "agent-1", "test", 5, nil)
	if err == nil {
		t.Error("应返回 error")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error 应含 500: %v", err)
	}
}
