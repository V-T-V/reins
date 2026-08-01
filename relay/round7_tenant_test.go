// Package main — round 7 多租户隔离骨架测试。
//
// 验证组织/角色/权限模型(tenant.go)的核心不变量:
//   - 组织 CRUD + 启停 + 配额。
//   - 角色策略的权限矩阵(admin/operator/viewer)。
//   - FilterAgentsByTenant 数据隔离(admin 全量 / 租户只看自己的)。
//   - 与 rbac.go 向后兼容(旧 token=admin)。
package main

import (
	"testing"
)

// --- 组织管理 ---

// TestOrgCreateAndGet 验证创建 + 查询。
func TestOrgCreateAndGet(t *testing.T) {
	m := NewTenantManager()
	ok := m.CreateOrg(Organization{ID: "acme", Name: "ACME Corp", Quota: 10, Enabled: true})
	if !ok {
		t.Fatal("首次创建应成功")
	}
	// 重复创建应失败
	if m.CreateOrg(Organization{ID: "acme", Name: "dup"}) {
		t.Fatal("重复创建同一 ID 应失败")
	}
	o := m.GetOrg("acme")
	if o == nil || o.Name != "ACME Corp" {
		t.Fatalf("查询应返回 acme, got %+v", o)
	}
	if o.Quota != 10 || !o.Enabled {
		t.Fatalf("字段应保留, got quota=%d enabled=%v", o.Quota, o.Enabled)
	}
	if m.GetOrg("ghost") != nil {
		t.Fatal("不存在的组织应返回 nil")
	}
}

// TestOrgEmptyIDRejected 验证空组织 ID 被拒。
func TestOrgEmptyIDRejected(t *testing.T) {
	m := NewTenantManager()
	if m.CreateOrg(Organization{ID: ""}) {
		t.Fatal("空 ID 应被拒绝")
	}
}

// TestOrgSetEnabled 验证启停组织 + OrgEnabled 查询。
func TestOrgSetEnabled(t *testing.T) {
	m := NewTenantManager()
	m.CreateOrg(Organization{ID: "acme", Enabled: true})
	if !m.OrgEnabled("acme") {
		t.Fatal("创建时 Enabled 应为 true")
	}
	m.SetOrgEnabled("acme", false)
	if m.OrgEnabled("acme") {
		t.Fatal("禁用后 OrgEnabled 应返回 false")
	}
	m.SetOrgEnabled("acme", true)
	if !m.OrgEnabled("acme") {
		t.Fatal("重新启用后应返回 true")
	}
	// 不存在的组织 fail-closed
	if m.OrgEnabled("ghost") {
		t.Fatal("不存在的组织应 fail-closed(禁用)")
	}
	// 空 orgID(admin)不受约束
	if !m.OrgEnabled("") {
		t.Fatal("空 orgID 应视为不受组织约束(放行)")
	}
}

// TestOrgQuota 验证配额检查。
func TestOrgQuota(t *testing.T) {
	m := NewTenantManager()
	m.CreateOrg(Organization{ID: "acme", Quota: 5, Enabled: true})
	for i := 0; i < 5; i++ {
		if !m.CheckQuota("acme", i) {
			t.Fatalf("count=%d 未超配额 5 应放行", i)
		}
	}
	if m.CheckQuota("acme", 5) {
		t.Fatal("count=5 已达配额上限应拒绝")
	}
	// Quota=0 不限
	m.CreateOrg(Organization{ID: "unlimited", Quota: 0})
	if !m.CheckQuota("unlimited", 9999) {
		t.Fatal("Quota=0 应不限")
	}
}

// --- 角色权限矩阵 ---

// TestRolePolicyPermissionMatrix 验证默认角色策略的权限矩阵。
func TestRolePolicyPermissionMatrix(t *testing.T) {
	p := NewDefaultRolePolicy()
	cases := []struct {
		role    string
		action  string
		allowed bool
	}{
		{RoleAdmin, "agent.revoke", true},
		{RoleAdmin, "power.shutdown", true},
		{RoleAdmin, "anything.unknown", true}, // admin 全权限
		{RoleOperator, "task.list", true},
		{RoleOperator, "agent.revoke", false}, // operator 不能 revoke
		{RoleOperator, "power.shutdown", false},
		{RoleViewer, "task.list", true},
		{RoleViewer, "agent.list", true},
		{RoleViewer, "power.shutdown", false},
		{RoleViewer, "task.run", false},
		{"unknown_role", "task.list", false},
	}
	for _, c := range cases {
		if got := p.CanPerform(c.role, c.action); got != c.allowed {
			t.Errorf("CanPerform(%s,%s) = %v, 期望 %v", c.role, c.action, got, c.allowed)
		}
	}
}

// TestRolePolicyGrantRole 验证运行期动态授权。
func TestRolePolicyGrantRole(t *testing.T) {
	p := NewDefaultRolePolicy()
	// 自定义角色 + 动态加权限
	p.GrantRole("auditor", "task.list", "agent.list", "metrics")
	if !p.CanPerform("auditor", "task.list") {
		t.Fatal("auditor 应有 task.list")
	}
	if !p.CanPerform("auditor", "agent.list") {
		t.Fatal("auditor 应有 agent.list")
	}
	if p.CanPerform("auditor", "power.shutdown") {
		t.Fatal("auditor 不应有 power.shutdown(未授权)")
	}
}

// --- 数据隔离过滤 ---

// TestFilterAgentsByTenantIsolation 验证按租户隔离过滤 Agent 列表。
func TestFilterAgentsByTenantIsolation(t *testing.T) {
	agents := []tenantAgentView{
		{AgentID: "web-1", Tags: map[string]string{"tenant": "acme"}},
		{AgentID: "web-2", Tags: map[string]string{"tenant": "acme"}},
		{AgentID: "db-1", Tags: map[string]string{"tenant": "globex"}},
		{AgentID: "shared-1", Tags: map[string]string{}}, // 无 tenant 标签
	}

	// admin(无 tenant)看全部
	admin := Identity{Tenant: "", Role: RoleAdmin}
	if got := FilterAgentsByTenant(admin, agents); len(got) != 4 {
		t.Fatalf("admin 应看全部 4 个, got %d", len(got))
	}

	// acme operator 只看 acme 的
	acme := Identity{Tenant: "acme", Role: RoleOperator}
	got := FilterAgentsByTenant(acme, agents)
	if len(got) != 2 {
		t.Fatalf("acme 应看 2 个, got %d", len(got))
	}
	for _, a := range got {
		if a.Tags["tenant"] != "acme" {
			t.Fatalf("acme 过滤泄漏了非 acme 的 Agent: %s", a.AgentID)
		}
	}

	// globex 只看 1 个
	globex := Identity{Tenant: "globex", Role: RoleViewer}
	if got := FilterAgentsByTenant(globex, agents); len(got) != 1 || got[0].AgentID != "db-1" {
		t.Fatalf("globex 应只看 db-1, got %+v", got)
	}
}

// TestFilterAgentsByTenantNoLeakage 验证租户隔离不泄漏跨租户 Agent ID。
func TestFilterAgentsByTenantNoLeakage(t *testing.T) {
	agents := []tenantAgentView{
		{AgentID: "secret-acme-1", Tags: map[string]string{"tenant": "acme"}},
		{AgentID: "secret-globex-1", Tags: map[string]string{"tenant": "globex"}},
	}
	acme := Identity{Tenant: "acme", Role: RoleOperator}
	got := FilterAgentsByTenant(acme, agents)
	for _, a := range got {
		if a.AgentID == "secret-globex-1" {
			t.Fatal("acme 租户不应看到 globex 的 Agent(隔离泄漏)")
		}
	}
}

// TestFilterAgentsByTenantEmpty 验证空列表安全。
func TestFilterAgentsByTenantEmpty(t *testing.T) {
	acme := Identity{Tenant: "acme", Role: RoleOperator}
	got := FilterAgentsByTenant(acme, nil)
	if len(got) != 0 {
		t.Fatalf("空列表应返回空, got %d", len(got))
	}
}

// --- 与 rbac.go 向后兼容 ---

// TestTenantBackwardCompatWithRBAC 验证 tenant.go 与 rbac.go 的 Identity 兼容。
func TestTenantBackwardCompatWithRBAC(t *testing.T) {
	// 旧格式 token(纯 secret)解析为 admin 无 tenant
	id := ParseToken("legacy-secret")
	if id.Role != RoleAdmin || id.Tenant != "" {
		t.Fatal("旧 token 应为 admin 无 tenant")
	}
	// 新格式 token
	id = ParseToken("acme:operator:pass")
	if id.Tenant != "acme" || id.Role != RoleOperator {
		t.Fatalf("新 token 解析错误: %+v", id)
	}
	// 两者都可用于 FilterAgentsByTenant
	agents := []tenantAgentView{{AgentID: "a", Tags: map[string]string{"tenant": "acme"}}}
	if len(FilterAgentsByTenant(id, agents)) != 1 {
		t.Fatal("acme operator 应看到 acme agent")
	}
}
