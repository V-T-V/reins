package main

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// TestSecretStorePutGetRoundTrip 验证存取往返与默认 TTL。
func TestSecretStorePutGetRoundTrip(t *testing.T) {
	ss := NewSecretStore()
	ref, err := ss.Put("db-password-123", 0, false)
	if err != nil {
		t.Fatalf("Put 失败: %v", err)
	}
	if !strings.HasPrefix(ref, "sec_") {
		t.Fatalf("ref 应以 sec_ 开头, 得到 %q", ref)
	}
	// 默认 ttl=0 → 内部 1 小时
	v, ok := ss.Get(ref)
	if !ok {
		t.Fatal("Get 应命中")
	}
	if v != "db-password-123" {
		t.Fatalf("值不匹配: %q", v)
	}
	// 非 one-time:可多次取
	if _, ok := ss.Get(ref); !ok {
		t.Fatal("非一次性凭据应可重复读取")
	}
}

// TestSecretStoreOneTimeConsumed 验证一次性凭据取后即删(防重放)。
func TestSecretStoreOneTimeConsumed(t *testing.T) {
	ss := NewSecretStore()
	ref, _ := ss.Put("api-key-xyz", time.Minute, true)

	v, ok := ss.Get(ref)
	if !ok || v != "api-key-xyz" {
		t.Fatalf("首次拉取应成功, got v=%q ok=%v", v, ok)
	}
	// 第二次应失败(已删除)
	if _, ok := ss.Get(ref); ok {
		t.Fatal("一次性凭据二次拉取必须失败(防重放)")
	}
}

// TestSecretStoreTTLExpiry 验证过期凭据不可读且被清理。
func TestSecretStoreTTLExpiry(t *testing.T) {
	ss := NewSecretStore()
	ref, _ := ss.Put("short-lived", 50*time.Millisecond, false)

	// 未过期:命中
	if _, ok := ss.Get(ref); !ok {
		t.Fatal("未过期应命中")
	}
	time.Sleep(80 * time.Millisecond)
	// 过期:miss 且被删除
	if _, ok := ss.Get(ref); ok {
		t.Fatal("过期凭据应 miss")
	}
}

// TestSecretStoreDelete 显式删除后不可读。
func TestSecretStoreDelete(t *testing.T) {
	ss := NewSecretStore()
	ref, _ := ss.Put("v", 0, false)
	ss.Delete(ref)
	if _, ok := ss.Get(ref); ok {
		t.Fatal("删除后应 miss")
	}
	// 删除不存在的 ref 不 panic
	ss.Delete("sec_nonexistent")
}

// TestSecretStoreGetMissingRef 不存在 ref 返回 miss。
func TestSecretStoreGetMissingRef(t *testing.T) {
	ss := NewSecretStore()
	if _, ok := ss.Get("sec_does_not_exist"); ok {
		t.Fatal("不存在 ref 应 miss")
	}
}

// TestSecretStoreConcurrent 并发 Put/Get 不竞争崩溃,且一次性语义在并发下不重复返回值。
func TestSecretStoreConcurrent(t *testing.T) {
	ss := NewSecretStore()
	const N = 50
	refs := make([]string, N)
	for i := 0; i < N; i++ {
		refs[i], _ = ss.Put("val", time.Hour, true) // 全部一次性
	}
	var wg sync.WaitGroup
	consumed := int32(0)
	var mu sync.Mutex
	// 并发拉取所有 ref,每个 ref 只应被成功消费一次
	for _, ref := range refs {
		wg.Add(1)
		go func(r string) {
			defer wg.Done()
			if _, ok := ss.Get(r); ok {
				mu.Lock()
				consumed++
				mu.Unlock()
			}
		}(ref)
	}
	wg.Wait()
	if consumed != int32(N) {
		t.Fatalf("一次性凭据并发拉取,每个应恰好被消费一次,期望 %d 得到 %d", N, consumed)
	}
}

// TestSecretStoreRefUniqueness 多次 Put 生成不同 ref(随机性回归)。
func TestSecretStoreRefUniqueness(t *testing.T) {
	ss := NewSecretStore()
	seen := make(map[string]bool, 100)
	for i := 0; i < 100; i++ {
		ref, err := ss.Put("v", 0, false)
		if err != nil {
			t.Fatalf("Put %d 失败: %v", i, err)
		}
		if seen[ref] {
			t.Fatalf("ref 重复: %s", ref)
		}
		seen[ref] = true
	}
}

// --- 智能调度 matchTagAny 单元测试 ---

func TestMatchTagAny(t *testing.T) {
	tags := map[string]string{"env": "prod", "region": "us-east", "role": "web"}
	cases := []struct {
		name   string
		filter string
		want   bool
	}{
		{"key=value 命中", "env=prod", true},
		{"value 命中", "prod", true},
		{"另一个 value 命中", "us-east", true},
		{"key=value 不命中", "env=staging", false},
		{"未知 value", "unknown", false},
		{"空 filter", "", false}, // k+"="+v 不会等于 ""
	}
	for _, c := range cases {
		if got := matchTagAny(tags, c.filter); got != c.want {
			t.Errorf("%s: filter=%q want %v got %v", c.name, c.filter, c.want, got)
		}
	}
}

// TestMatchTagAnyNilMap nil tags 一律不匹配(不 panic)。
func TestMatchTagAnyNilMap(t *testing.T) {
	if matchTagAny(nil, "env=prod") {
		t.Fatal("nil tags 应不匹配")
	}
	if matchTagAny(nil, "") {
		t.Fatal("nil tags + 空 filter 应不匹配")
	}
}
