package main

import (
	"net/http/httptest"
	"testing"

	"github.com/QiuShichang/go-rmm/proto"
)

// === RBAC authorizeRBAC 集成 ===

func TestAuthorizeRBACNoTokenConfig(t *testing.T) {
	r := &Relay{cfg: Config{}} // Token="" 不校验
	req := httptest.NewRequest("GET", "/api/agents", nil)
	id, err := r.authorizeRBAC(req)
	if err != nil {
		t.Fatalf("无 token 配置应放行: %v", err)
	}
	if id.Role != RoleAdmin {
		t.Errorf("Role = %s, 期望 admin", id.Role)
	}
}

func TestAuthorizeRBACValidToken(t *testing.T) {
	r := &Relay{cfg: Config{Token: "secret123"}}
	req := httptest.NewRequest("GET", "/api/agents", nil)
	req.Header.Set("Authorization", "Bearer secret123")
	id, err := r.authorizeRBAC(req)
	if err != nil {
		t.Fatalf("有效 token 应放行: %v", err)
	}
	// 旧格式 token = admin
	if id.Role != RoleAdmin {
		t.Errorf("Role = %s", id.Role)
	}
}

func TestAuthorizeRBACRBACToken(t *testing.T) {
	// 服务端配 tenant:role:secret,客户端发同样格式
	r := &Relay{cfg: Config{Token: "acme:operator:shared-secret"}}
	req := httptest.NewRequest("GET", "/api/agents", nil)
	req.Header.Set("Authorization", "Bearer acme:operator:shared-secret")
	id, err := r.authorizeRBAC(req)
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

func TestAuthorizeRBACInvalidToken(t *testing.T) {
	r := &Relay{cfg: Config{Token: "correct"}}
	req := httptest.NewRequest("GET", "/api/agents", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	_, err := r.authorizeRBAC(req)
	if err == nil {
		t.Error("错误 token 应拒绝")
	}
}

func TestAuthorizeRBACNoHeader(t *testing.T) {
	r := &Relay{cfg: Config{Token: "secret"}}
	req := httptest.NewRequest("GET", "/api/agents", nil)
	_, err := r.authorizeRBAC(req)
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
	if !contains(cef, "CEF:0|go-rmm") {
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
