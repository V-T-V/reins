package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/QiuShichang/go-rmm/executors"
	"github.com/QiuShichang/go-rmm/proto"
)

// === Agent executors.go 测试 ===

func TestDefaultRoots(t *testing.T) {
	roots := defaultRoots()
	if len(roots) == 0 {
		t.Error("defaultRoots 不应返回空")
	}
}

func TestEnsureWorkspaceCreatesDir(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := &Config{
		Workspace: filepath.Join(tmpDir, "ws"),
	}
	ensureWorkspace(cfg)
	if fi, err := os.Stat(cfg.Workspace); err != nil || !fi.IsDir() {
		t.Errorf("workspace 目录未创建: %s", cfg.Workspace)
	}
	// AllowedRoots 应包含 workspace
	found := false
	for _, r := range cfg.AllowedRoots {
		if r == cfg.Workspace {
			found = true
			break
		}
	}
	if !found {
		t.Error("AllowedRoots 应包含 workspace")
	}
	// AllowedRoots 应包含 exeDir
	foundExe := false
	for _, r := range cfg.AllowedRoots {
		if r == exeDir() {
			foundExe = true
			break
		}
	}
	if !foundExe {
		t.Error("AllowedRoots 应包含 exeDir")
	}
}

// === Agent updater 测试 ===

func TestUpdaterVersionGESemver(t *testing.T) {
	tests := []struct {
		v, min string
		expect bool
	}{
		{"v1.2.0", "v1.0.0", true},
		{"v2.0.0", "v1.9.9", true},
		{"v1.0.0", "v1.2.0", false},
		{"v1.2.3", "v1.2.3", true},
		{"v1.0.0", "", true}, // 无约束
	}
	for _, tt := range tests {
		if got := versionGE(tt.v, tt.min); got != tt.expect {
			t.Errorf("versionGE(%q, %q) = %v", tt.v, tt.min, got)
		}
	}
}

func TestUpdaterParseSemver(t *testing.T) {
	v := parseSemver("v1.2.3")
	if len(v) < 3 || v[0] != 1 || v[1] != 2 || v[2] != 3 {
		t.Errorf("parseSemver(v1.2.3) = %v", v)
	}
}

func TestUpdaterComputeSHA256(t *testing.T) {
	f, err := os.CreateTemp("", "test-sha-*.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	f.Write([]byte("test"))
	f.Close()

	hash, err := computeSHA256(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	if len(hash) != 64 { // SHA256 hex = 64 chars
		t.Errorf("hash len = %d, 期望 64", len(hash))
	}
}

func TestUpdaterMarkerPath(t *testing.T) {
	p := updatePendingMarkerPath()
	if p == "" {
		t.Error("marker path 不应为空")
	}
	// 应在 exeDir 下
	if filepath.Dir(p) != exeDir() {
		t.Errorf("marker path 应在 exeDir: %s", p)
	}
}

// === Agent recording 测试 ===

func TestSessionRecorderWrite(t *testing.T) {
	rec, err := NewSessionRecorder("test-session-1", "agent-test", 80, 24)
	if err != nil {
		t.Fatalf("NewSessionRecorder: %v", err)
	}
	defer rec.Close()

	// 写入输出
	rec.WriteOutput([]byte("hello world\n"))
	rec.WriteOutput([]byte("second line\n"))

	// 验证文件存在且有内容
	info, err := os.Stat(filepath.Join(recordingDir(), "test-session-1.cast"))
	if err != nil {
		t.Fatalf("录屏文件不存在: %v", err)
	}
	if info.Size() < 50 {
		t.Errorf("录屏文件太小: %d bytes", info.Size())
	}
}

func TestSessionRecorderHeader(t *testing.T) {
	rec, err := NewSessionRecorder("test-session-2", "agent-x", 120, 40)
	if err != nil {
		t.Fatalf("NewSessionRecorder: %v", err)
	}
	defer rec.Close()

	// 读录屏文件验证 header(asciinema v2)
	data, _ := os.ReadFile(filepath.Join(recordingDir(), "test-session-2.cast"))
	if len(data) == 0 {
		t.Fatal("录屏文件为空")
	}
	// 应含 "version":2
	if !containsBytes(data, []byte("version")) {
		t.Error("录屏 header 应含 version")
	}
	// 应含 width 120
	if !containsBytes(data, []byte("120")) {
		t.Error("录屏 header 应含 width 120")
	}
}

// === Agent tunnel 测试(不连真实网络) ===

func TestTunnelManagerCloseAll(t *testing.T) {
	tm := &TunnelManager{tunnels: make(map[string]*tunnelSession)}
	tm.CloseAll() // 不应 panic
}

func TestTunnelManagerCancelNonExist(t *testing.T) {
	tm := &TunnelManager{tunnels: make(map[string]*tunnelSession)}
	tm.Close("nonexistent", "test") // 不应 panic
}

// === Agent queue 测试深化 ===

func TestQueuePeekHeadEmpty(t *testing.T) {
	q := &rQueue{}
	_, ok := q.peekHead()
	if ok {
		t.Error("空 queue peekHead 应返回 false")
	}
}

func TestQueuePopHeadEmpty(t *testing.T) {
	q := &rQueue{}
	item := q.popHead()
	if item.env.ID != "" {
		t.Error("空 queue popHead 应返回零值")
	}
}

// === 辅助 ===

func containsBytes(haystack, needle []byte) bool {
	if len(needle) > len(haystack) {
		return false
	}
	for i := 0; i <= len(haystack)-len(needle); i++ {
		match := true
		for j := 0; j < len(needle); j++ {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// 确保 executors 包被引用(避免 unused import)
var _ = executors.New
var _ = proto.TaskUpdate
var _ = json.Marshal
