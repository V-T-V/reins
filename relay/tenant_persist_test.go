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
	// 2026:组织由凭据自动供给——此前生产没有任何建组织的途径,而 OrgEnabled 对
	// 不存在的组织 fail-closed,租户凭据会永远 401(旧代码里没人发现:裸 secret 当 admin 绕过去了)。
	org := relay.tenants.GetOrg("tenant")
	if org == nil || !org.Enabled {
		t.Fatalf("租户凭据应在启动时自动建立启用的组织,得到 %+v", org)
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

	// 2026 租户加固·语义翻转(红锚):服务端只配了 tenant:operator:s3cret 这一条凭据时,
	// 以纯 secret 形式出示(s3cret,省略前缀)仍是**同一条凭据** → 绑定身份 tenant/operator,
	// 照样受组织状态约束。旧实现把「去掉前缀」解析成 admin —— 这正是被修复的提权路径
	// (知道 secret 的租户用户省略前缀即得全局 admin),不得复活。
	bareReq := httptest.NewRequest(http.MethodGet, "/api/agents", nil)
	bareReq.Header.Set("Authorization", "Bearer s3cret")
	if err := relay.authorize(bareReq); err == nil {
		t.Fatal("纯 secret 形式出示租户凭据,组织删除后同样应被拒绝(省略前缀不得成为 admin)")
	}
	// 自称 admin 前缀同样无效
	forgedReq := httptest.NewRequest(http.MethodGet, "/api/agents", nil)
	forgedReq.Header.Set("Authorization", "Bearer :admin:s3cret")
	if err := relay.authorize(forgedReq); err == nil {
		t.Fatal("租户凭据自称 admin,组织删除后应被拒绝")
	}

	// admin 不受组织约束——前提是服务端**单独配置了** admin 凭据(无租户条目)
	adminRelay := NewRelay(Config{Token: ":admin:adminsecret,tenant:operator:s3cret"})
	if adminRelay.tenants.GetOrg("tenant") == nil {
		t.Fatal("多凭据配置下租户凭据应已自动供给组织")
	}
	adminRelay.tenants.SetOrgEnabled("tenant", false)
	adminReq := httptest.NewRequest(http.MethodGet, "/api/agents", nil)
	adminReq.Header.Set("Authorization", "Bearer adminsecret")
	id, err := adminRelay.authenticate(adminReq)
	if err != nil {
		t.Fatalf("admin token 不应受组织状态影响: %v", err)
	}
	if id.Role != RoleAdmin || id.Tenant != "" {
		t.Fatalf("admin 凭据身份 = %s/%s, 期望 /admin", id.Tenant, id.Role)
	}
	// 同一 Relay 上租户凭据仍被禁用组织拦下
	if _, err := adminRelay.authenticate(req); err == nil {
		t.Fatal("多凭据下禁用组织的租户凭据应被拒绝")
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
