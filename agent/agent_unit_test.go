package main

import (
	"os"
	"testing"
	"time"

	"github.com/QiuShichang/go-rmm/proto"
)

// === 幂等缓存 ===

func TestIdemCacheGetPut(t *testing.T) {
	c := &idemCache{ttl: time.Minute, m: make(map[string]idemEntry)}
	if _, hit := c.get("missing"); hit {
		t.Error("空 cache 不应命中")
	}
	result := proto.TaskResult{TaskID: "t1", Success: true, Stdout: "done"}
	c.put("key1", result)
	got, hit := c.get("key1")
	if !hit {
		t.Error("应命中")
	}
	if got.TaskID != "t1" || !got.Success {
		t.Errorf("结果不匹配: %+v", got)
	}
}

func TestIdemCacheTTL(t *testing.T) {
	c := &idemCache{ttl: 50 * time.Millisecond, m: make(map[string]idemEntry)}
	c.put("key1", proto.TaskResult{TaskID: "t1"})
	if _, hit := c.get("key1"); !hit {
		t.Error("应命中")
	}
	time.Sleep(60 * time.Millisecond)
	if _, hit := c.get("key1"); hit {
		t.Error("过期后不应命中")
	}
}

func TestIdemCacheSweep(t *testing.T) {
	c := &idemCache{ttl: 50 * time.Millisecond, m: make(map[string]idemEntry)}
	c.put("k1", proto.TaskResult{})
	c.put("k2", proto.TaskResult{})
	time.Sleep(60 * time.Millisecond)
	c.sweep()
	if len(c.m) != 0 {
		t.Errorf("sweep 后应清空, 剩余 %d", len(c.m))
	}
}

func TestKeyFor(t *testing.T) {
	task1 := proto.TaskRequest{IdempotencyKey: "custom-key", TaskID: "t1"}
	if k := keyFor(task1); k != "custom-key" {
		t.Errorf("keyFor = %s", k)
	}
	task2 := proto.TaskRequest{TaskID: "fallback-id"}
	if k := keyFor(task2); k != "fallback-id" {
		t.Errorf("keyFor = %s", k)
	}
}

// === 队列 ===

func TestQueueFlushTo(t *testing.T) {
	// queue 不依赖真实 WS 连接(flushTo 在无 conn 时只 peek 不发送)
	q := &rQueue{}
	if q.len() != 0 {
		t.Error("初始应为空")
	}
}

func TestQueueEnqueueLen(t *testing.T) {
	q := &rQueue{}
	env, _ := proto.NewEnvelope("task_result", "agent-1", "relay", proto.TaskResult{TaskID: "t1"})
	q.enqueue(env)
	if q.len() != 1 {
		t.Errorf("len = %d, 期望 1", q.len())
	}
	q.enqueue(env)
	if q.len() != 2 {
		t.Errorf("len = %d, 期望 2", q.len())
	}
}

// === 版本比较 ===

func TestVersionGE(t *testing.T) {
	tests := []struct {
		version    string
		minVersion string
		expect     bool
	}{
		{"v1.2.0", "v1.0.0", true},  // 高版本
		{"v1.0.0", "v1.2.0", false}, // 低版本
		{"v2.0.0", "v1.9.9", true},  // major 高
		{"v1.2.3", "v1.2.3", true},  // 相等
		{"v1.2.0", "", true},        // 无约束
		{"v1.10.0", "v1.9.0", true}, // 两位数
	}
	for _, tt := range tests {
		got := versionGE(tt.version, tt.minVersion)
		if got != tt.expect {
			t.Errorf("versionGE(%s, %s) = %v, 期望 %v", tt.version, tt.minVersion, got, tt.expect)
		}
	}
}

func TestParseSemver(t *testing.T) {
	tests := []struct {
		input string
		major int
		minor int
		patch int
	}{
		{"v1.2.3", 1, 2, 3},
		{"2.0.10", 2, 0, 10},
		{"v0.1.0", 0, 1, 0},
	}
	for _, tt := range tests {
		got := parseSemver(tt.input)
		if len(got) < 3 {
			t.Errorf("parseSemver(%s) 返回 %d 段", tt.input, len(got))
			continue
		}
		if got[0] != tt.major || got[1] != tt.minor || got[2] != tt.patch {
			t.Errorf("parseSemver(%s) = %v, 期望 [%d %d %d]", tt.input, got, tt.major, tt.minor, tt.patch)
		}
	}
}

// === SHA256 ===

func TestComputeSHA256(t *testing.T) {
	// 创建临时文件测试
	tmpFile := "test_sha256.txt"
	content := []byte("hello world")
	if err := writeFile(tmpFile, content); err != nil {
		t.Fatal(err)
	}
	defer removeFile(tmpFile)

	hash, err := computeSHA256(tmpFile)
	if err != nil {
		t.Fatal(err)
	}
	// "hello world" 的 SHA256 是已知的
	expected := "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9"
	if hash != expected {
		t.Errorf("hash = %s, 期望 %s", hash, expected)
	}
}

// 辅助
func writeFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0644)
}
func removeFile(path string) error {
	_, err := os.Stat(path)
	if err != nil {
		return nil
	}
	return os.Remove(path)
}
