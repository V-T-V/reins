package main

import (
	"net/http/httptest"
	"testing"

	"github.com/V-T-V/reins/proto"
)

// === RBAC authenticate 集成(2026 租户加固:原 authorizeRBAC 用例迁到 authenticate,
// 断言口径升级为「服务端绑定身份」——请求方声明的前缀不参与判定) ===

func TestAuthenticateNoTokenConfig(t *testing.T) {
	r := &Relay{cfg: Config{}} // Token="" 不校验
	req := httptest.NewRequest("GET", "/api/agents", nil)
	id, err := r.authenticate(req)
	if err != nil {
		t.Fatalf("无 token 配置应放行: %v", err)
	}
	if id.Role != RoleAdmin {
		t.Errorf("Role = %s, 期望 admin", id.Role)
	}
}

func TestAuthenticateValidLegacyToken(t *testing.T) {
	r := &Relay{cfg: Config{Token: "secret123"}}
	req := httptest.NewRequest("GET", "/api/agents", nil)
	req.Header.Set("Authorization", "Bearer secret123")
	id, err := r.authenticate(req)
	if err != nil {
		t.Fatalf("有效 token 应放行: %v", err)
	}
	// 旧格式 token = admin
	if id.Role != RoleAdmin || id.Tenant != "" {
		t.Errorf("期望 admin/无租户,得到 %s/%s", id.Tenant, id.Role)
	}
	// Secret 不外带
	if id.Secret != "" {
		t.Errorf("authenticate 返回值不应携带 Secret")
	}
}

func TestAuthenticateRBACToken(t *testing.T) {
	// 服务端配 tenant:role:secret,客户端发同样格式
	r := &Relay{cfg: Config{Token: "acme:operator:shared-secret"}}
	req := httptest.NewRequest("GET", "/api/agents", nil)
	req.Header.Set("Authorization", "Bearer acme:operator:shared-secret")
	id, err := r.authenticate(req)
	if err != nil {
		t.Fatalf("RBAC token 应放行: %v", err)
	}
	if id.Tenant != "acme" {
		t.Errorf("Tenant = %s", id.Tenant)
	}
	if id.Role != RoleOperator {
		t.Errorf("Role = %s", id.Role)
	}
}

// TestAuthenticateIgnoresClientDeclaredIdentity 红锚:知道 secret 的一方
// 自称任意租户/admin,身份仍必须是服务端条目的绑定值(提权路径消灭)。
func TestAuthenticateIgnoresClientDeclaredIdentity(t *testing.T) {
	r := &Relay{cfg: Config{Token: ":admin:adminsecret,acme:operator:s1,globex:viewer:s2"}}

	cases := []struct {
		name, presented, wantTenant, wantRole string
	}{
		{"acme secret 自称 victim 租户 admin", "victim:admin:s1", "acme", RoleOperator},
		{"acme secret 自称本租户 admin", "acme:admin:s1", "acme", RoleOperator},
		{"acme secret 以纯 secret 形式出现(旧客户端)", "s1", "acme", RoleOperator},
		{"globex secret 自称 acme admin", "acme:admin:s2", "globex", RoleViewer},
		{"admin 纯 secret 自称租户", "acme:admin:adminsecret", "", RoleAdmin},
		{"多凭据下未知 role 前缀不影响绑定", "x:nonsense:s2", "globex", RoleViewer},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/api/agents", nil)
			req.Header.Set("Authorization", "Bearer "+c.presented)
			id, err := r.authenticate(req)
			if err != nil {
				t.Fatalf("应放行: %v", err)
			}
			if id.Tenant != c.wantTenant || id.Role != c.wantRole {
				t.Errorf("身份 = %s/%s, 期望服务端绑定值 %s/%s", id.Tenant, id.Role, c.wantTenant, c.wantRole)
			}
		})
	}
}

func TestAuthenticateInvalidToken(t *testing.T) {
	r := &Relay{cfg: Config{Token: "correct"}}
	req := httptest.NewRequest("GET", "/api/agents", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	_, err := r.authenticate(req)
	if err == nil {
		t.Error("错误 token 应拒绝")
	}
	// 错误消息是 e2e/verify 契约面,逐字保留
	req.Header.Set("Authorization", "Bearer wrong")
	_, err = r.authenticate(req)
	if err == nil || err.Error() != "无效的鉴权 token" {
		t.Errorf("错误消息漂移: %v", err)
	}
}

func TestAuthenticateNoHeader(t *testing.T) {
	r := &Relay{cfg: Config{Token: "secret"}}
	req := httptest.NewRequest("GET", "/api/agents", nil)
	_, err := r.authenticate(req)
	if err == nil {
		t.Error("无 Authorization header 应拒绝")
	}
}

// === SelectBestAgent 智能调度 ===

func TestSelectBestAgentEmpty(t *testing.T) {
	r := &Relay{
		agents: NewRegistry(),
	}
	_, err := r.SelectBestAgent("", "")
	if err == nil {
		t.Error("无 Agent 应返回 error")
	}
}

func TestSelectBestAgentWithMockAgents(t *testing.T) {
	r := &Relay{agents: NewRegistry()}
	// 注册两个 Agent
	r.agents.Register(proto.Register{
		AgentID: "agent-a", Hostname: "host-a",
		Capabilities: []string{"powershell"},
	}, &wsConn{})
	r.agents.Register(proto.Register{
		AgentID: "agent-b", Hostname: "host-b",
		Capabilities: []string{"powershell", "file"},
	}, &wsConn{})

	// 无 tag 过滤,选最优(两者负载相同,返回其中一个)
	best, err := r.SelectBestAgent("", "powershell")
	if err != nil {
		t.Fatalf("SelectBestAgent 失败: %v", err)
	}
	if best != "agent-a" && best != "agent-b" {
		t.Errorf("best = %s, 期望 agent-a 或 agent-b", best)
	}
}

func TestSelectBestAgentCapabilityFilter(t *testing.T) {
	r := &Relay{agents: NewRegistry()}
	r.agents.Register(proto.Register{
		AgentID: "agent-a", Capabilities: []string{"powershell"},
	}, &wsConn{})
	r.agents.Register(proto.Register{
		AgentID: "agent-b", Capabilities: []string{"file"},
	}, &wsConn{})

	// 只有 agent-a 支持 powershell
	best, err := r.SelectBestAgent("", "powershell")
	if err != nil {
		t.Fatalf("失败: %v", err)
	}
	if best != "agent-a" {
		t.Errorf("best = %s, 期望 agent-a", best)
	}

	// file 类型只有 agent-b
	best, _ = r.SelectBestAgent("", "file")
	if best != "agent-b" {
		t.Errorf("best = %s, 期望 agent-b", best)
	}

	// browse 都不支持 → error
	_, err = r.SelectBestAgent("", "browse")
	if err == nil {
		t.Error("无 Agent 支持 browse 应返回 error")
	}
}

// === SIEM 格式 ===

func TestFormatCEF(t *testing.T) {
	rec := auditRecord{
		AuditEntry: AuditEntry{TS: 1700000000000, Event: "task_submitted", AgentID: "a1"},
		PrevHash:   "abc",
		ThisHash:   "def",
	}
	cef := formatCEF(rec)
	if cef == "" {
		t.Error("CEF 不应为空")
	}
	if !contains(cef, "CEF:0|reins") {
		t.Error("应含 CEF header")
	}
}

func TestFormatSyslog(t *testing.T) {
	rec := auditRecord{
		AuditEntry: AuditEntry{TS: 1700000000000, Event: "task_blocked", AgentID: "a1"},
	}
	sys := formatSyslog(rec)
	if sys == "" {
		t.Error("syslog 不应为空")
	}
	if !contains(sys, "<") {
		t.Error("应含 syslog priority")
	}
}

func TestEscapeCEF(t *testing.T) {
	tests := []struct {
		input  string
		expect string
	}{
		{"normal", "normal"},
		{"a|b", "a\\|b"},
		{"a=b", "a\\=b"},
		{"a\\b", "a\\\\b"},
	}
	for _, tt := range tests {
		got := escapeCEF(tt.input)
		if got != tt.expect {
			t.Errorf("escapeCEF(%q) = %q, 期望 %q", tt.input, got, tt.expect)
		}
	}
}

// === 辅助 ===

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsStr(s, substr))
}

func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
