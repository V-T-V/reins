package main

import (
	"strings"
	"testing"
)

// TestDefaultIdemKeyUnique 同一命令两次调用必须生成不同幂等键——
// 修复前纯内容 hash 使重跑命中 Agent 端幂等缓存(2h TTL)回放旧结果,
// 运维被迫手工给 --idem 加时间戳后缀(使用会话 2026-08 实证 4 例)。
func TestDefaultIdemKeyUnique(t *testing.T) {
	payload := []byte(`{"command":"Get-Process"}`)
	a := defaultIdemKey("agent-1", "powershell", payload)
	b := defaultIdemKey("agent-1", "powershell", payload)
	if a == b {
		t.Fatalf("相同命令两次调用应生成不同幂等键:\n%s\n%s", a, b)
	}
	if !strings.HasPrefix(a, "cli-") {
		t.Fatalf("应保留 cli- 前缀: %s", a)
	}
	// 内容指纹段:同 agent+payload 的 hash 前缀一致,不同 agent 应不同
	hashOf := func(k string) string { return strings.Split(k, "-")[1] }
	if hashOf(a) != hashOf(b) {
		t.Fatalf("相同内容应有一致 hash 指纹:\n%s\n%s", a, b)
	}
	c := defaultIdemKey("agent-2", "powershell", payload)
	if hashOf(a) == hashOf(c) {
		t.Fatalf("不同 agent 的 hash 指纹应不同: %s", a)
	}
}
