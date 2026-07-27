package main

import "testing"

func TestParseTokenLegacy(t *testing.T) {
	id := ParseToken("secret123")
	if id.Role != RoleAdmin {
		t.Errorf("旧 token Role = %s, 期望 admin", id.Role)
	}
	if id.Tenant != "" {
		t.Errorf("旧 token Tenant = %s, 期望空", id.Tenant)
	}
	if id.Secret != "secret123" {
		t.Errorf("Secret = %s", id.Secret)
	}
}

func TestParseTokenRBAC(t *testing.T) {
	id := ParseToken("acme:operator:mypass")
	if id.Tenant != "acme" {
		t.Errorf("Tenant = %s", id.Tenant)
	}
	if id.Role != RoleOperator {
		t.Errorf("Role = %s", id.Role)
	}
	if id.Secret != "mypass" {
		t.Errorf("Secret = %s", id.Secret)
	}
}

func TestCanPerform(t *testing.T) {
	tests := []struct {
		role    string
		action  string
		allowed bool
	}{
		{RoleAdmin, "agent.revoke", true},
		{RoleAdmin, "power.shutdown", true},
		{RoleAdmin, "task.list", true},
		{RoleOperator, "task.list", true},
		{RoleOperator, "agent.revoke", false},
		{RoleOperator, "power.shutdown", false},
		{RoleViewer, "task.list", true},
		{RoleViewer, "agent.list", true},
		{RoleViewer, "task.run", false}, // 不在 viewerAllowed
		{RoleViewer, "power.shutdown", false},
	}
	for _, tt := range tests {
		t.Run(tt.role+"/"+tt.action, func(t *testing.T) {
			id := Identity{Role: tt.role}
			if id.CanPerform(tt.action) != tt.allowed {
				t.Errorf("CanPerform(%s) for %s = %v, 期望 %v",
					tt.action, tt.role, !tt.allowed, tt.allowed)
			}
		})
	}
}

func TestTenantIsolation(t *testing.T) {
	// admin(无 tenant)可访问所有
	admin := Identity{Tenant: "", Role: RoleAdmin}
	if !admin.CanAccessAgent(map[string]string{"tenant": "acme"}) {
		t.Error("admin 应能访问所有 Agent")
	}
	// tenant=acme 可访问 acme 的 Agent
	acme := Identity{Tenant: "acme", Role: RoleOperator}
	if !acme.CanAccessAgent(map[string]string{"tenant": "acme"}) {
		t.Error("acme operator 应能访问 acme 的 Agent")
	}
	// tenant=acme 不能访问 other 的 Agent
	if acme.CanAccessAgent(map[string]string{"tenant": "other"}) {
		t.Error("acme operator 不应访问 other 的 Agent")
	}
	// 无 tenant 标签的 Agent,只有 admin 能访问
	if acme.CanAccessAgent(map[string]string{}) {
		t.Error("acme operator 不应访问无 tenant 标签的 Agent")
	}
	if !admin.CanAccessAgent(map[string]string{}) {
		t.Error("admin 应能访问无标签 Agent")
	}
}
