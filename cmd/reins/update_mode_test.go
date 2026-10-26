package main

// v4.7.1 update push 模式测试:apply 请求体默认 mode=download(公网内联超时坑的修正),
// --path 显式引用时 mode=path。

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestUpdatePushModeBody(t *testing.T) {
	var mu sync.Mutex
	var applyBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/updates/upload":
			_, _ = io.Copy(io.Discard, r.Body)
			_ = json.NewEncoder(w).Encode(map[string]any{"version": "v9.9.9", "sha256": "ab", "message": "ok"})
		case "/api/updates/apply":
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			applyBody = map[string]any{}
			_ = json.Unmarshal(b, &applyBody)
			mu.Unlock()
			_, _ = w.Write([]byte(`{"agent_id":"a1","task_id":"t1","version":"v9.9.9","old_version":"v1.0.0","message":"ok"}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	bin := filepath.Join(t.TempDir(), "agent.exe")
	os.WriteFile(bin, []byte("fake-binary"), 0755)

	if err := updatePush(config{relay: srv.URL}, []string{"-a", "a1", "-f", bin, "--version", "v9.9.9"}); err != nil {
		t.Fatalf("push 应成功: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if applyBody["mode"] != "download" {
		t.Errorf("默认应请求 download 模式: %v", applyBody["mode"])
	}
	if _, has := applyBody["binary_path"]; has {
		t.Errorf("-f 模式不应带 binary_path: %v", applyBody)
	}
}

func TestUpdatePushPathModeBody(t *testing.T) {
	var mu sync.Mutex
	var applyBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/updates/apply" {
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			applyBody = map[string]any{}
			_ = json.Unmarshal(b, &applyBody)
			mu.Unlock()
			_, _ = w.Write([]byte(`{"agent_id":"a1","task_id":"t1","version":"v9.9.9","old_version":"v1.0.0","message":"ok"}`))
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()

	err := updatePush(config{relay: srv.URL}, []string{"-a", "a1", "--version", "v9.9.9", "--path", `C:\ws\agent.exe.new`})
	if err != nil {
		t.Fatalf("push 应成功: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if applyBody["mode"] != "path" {
		t.Errorf("--path 应请求 path 模式: %v", applyBody["mode"])
	}
	if applyBody["binary_path"] != `C:\ws\agent.exe.new` {
		t.Errorf("binary_path 应透传: %v", applyBody["binary_path"])
	}
}
