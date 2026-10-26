package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/V-T-V/reins/internal/taskstore"
)

// TestTenantPersistenceRoundTrip 实证组织持久化往返:
// 创建组织 → 用同一 store 重建管理器 → 组织(含配额/启停)完整恢复。
func TestTenantPersistenceRoundTrip(t *testing.T) {
	store, err := taskstore.OpenStore(filepath.Join(t.TempDir(), "tenants.db"))
	if err != nil {
		t.Fatalf("打开 store 失败: %v", err)
	}
	defer store.Close()

	m1 := NewTenantManagerWithStore(store)
	if !m1.CreateOrg(Organization{ID: "org-a", Name: "Alpha", Quota: 5, Enabled: true}) {
		t.Fatal("创建组织应成功")
	}
	if !m1.CreateOrg(Organization{ID: "org-b", Name: "Beta", Quota: 0, Enabled: false}) {
		t.Fatal("创建第二个组织应成功")
	}
	if !m1.SetOrgEnabled("org-b", true) {
		t.Fatal("启用组织应成功")
	}

	// 模拟重启:同一 DB 重建管理器
	m2 := NewTenantManagerWithStore(store)
	a := m2.GetOrg("org-a")
	if a == nil || a.Name != "Alpha" || a.Quota != 5 || !a.Enabled {
		t.Fatalf("org-a 未正确恢复: %+v", a)
	}
	b := m2.GetOrg("org-b")
	if b == nil || !b.Enabled {
		t.Fatalf("org-b 的启停变更未持久化: %+v", b)
	}

	// 删除持久化
	if !m2.DeleteOrg("org-a") {
		t.Fatal("删除组织应成功")
	}
	m3 := NewTenantManagerWithStore(store)
	if m3.GetOrg("org-a") != nil {
		t.Fatal("删除后的组织不应在重启后复活")
	}
	if m3.GetOrg("org-b") == nil {
		t.Fatal("未删除的组织应保留")
	}
}

// TestTenantDisabledBlocksAuthorize 实证租户禁用后 token 立即失效:
// authorize 对禁用组织的租户身份返回错误,admin(空 tenant)不受影响。
func TestTenantDisabledBlocksAuthorize(t *testing.T) {
	relay := NewRelay(Config{Token: "tenant:operator:s3cret"})
	if !relay.tenants.CreateOrg(Organization{ID: "tenant", Enabled: true}) {
		t.Fatal("创建组织应成功")
	}

	req := httptest.NewRequest(http.MethodGet, "/api/agents", nil)
	req.Header.Set("Authorization", "Bearer tenant:operator:s3cret")
	if err := relay.authorize(req); err != nil {
		t.Fatalf("组织启用时租户 token 应通过: %v", err)
	}

	relay.tenants.SetOrgEnabled("tenant", false)
	if err := relay.authorize(req); err == nil {
		t.Fatal("组织禁用后租户 token 应被拒绝")
	}

	relay.tenants.DeleteOrg("tenant")
	if err := relay.authorize(req); err == nil {
		t.Fatal("组织删除后租户 token 应被拒绝(fail-closed)")
	}

	// admin(纯 secret,无租户语法)不受组织约束
	adminReq := httptest.NewRequest(http.MethodGet, "/api/agents", nil)
	adminReq.Header.Set("Authorization", "Bearer s3cret")
	if err := relay.authorize(adminReq); err != nil {
		t.Fatalf("admin token 不应受组织状态影响: %v", err)
	}
}

// TestTenantConcurrentMutations 并发变更组织不崩(锁纪律回归)。
func TestTenantConcurrentMutations(t *testing.T) {
	m := NewTenantManager()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := string(rune('a' + i%26))
			m.CreateOrg(Organization{ID: id, Enabled: true})
			m.SetOrgEnabled(id, false)
			m.OrgEnabled(id)
			m.CheckQuota(id, 3)
			m.GetOrg(id)
			m.DeleteOrg(id)
		}(i)
	}
	wg.Wait()
}
