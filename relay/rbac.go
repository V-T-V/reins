// Package main — 多租户 RBAC 基础(L8 神级提升)。
//
// token 格式扩展(向后兼容):
//
//	旧: "secret123"           → 无租户,全权限(admin)
//	新: "tenant:role:secret"   → 租户隔离 + 角色权限
//
// 角色:
//
//	admin   — 全权限(默认,旧 token 等效)
//	operator — 可下发任务/管理 Agent,不可 revoke/电源
//	viewer  — 只读(查看 agents/tasks/metrics,不能下发)
//
// 租户隔离:
//
//	tenant=acme 的 token 只能看到 Tags["tenant"]="acme" 的 Agent
//	旧 token(无 tenant)看所有 Agent(admin 等效)
package main

import (
	"crypto/subtle"
	"fmt"
	"log"
	"net/http"
	"strings"
)

// RBACRole 角色常量。
const (
	RoleAdmin    = "admin"
	RoleOperator = "operator"
	RoleViewer   = "viewer"
)

// Identity 从 token 解析的身份。
type Identity struct {
	Tenant string // 租户(空 = 全局 admin)
	Role   string // 角色(admin/operator/viewer)
	Secret string // 实际密钥
}

// ParseToken 解析 token。
// 格式: "tenant:role:secret" 或纯 secret(向后兼容,= admin 无 tenant)。
func ParseToken(token string) Identity {
	parts := strings.SplitN(token, ":", 3)
	if len(parts) == 3 {
		return Identity{Tenant: parts[0], Role: parts[1], Secret: parts[2]}
	}
	// 旧格式:纯 secret = admin
	return Identity{Tenant: "", Role: RoleAdmin, Secret: token}
}

// authorizeRBAC RBAC 增强版鉴权。
// 返回 Identity + nil(放行)或空 + error(拒绝)。
func (r *Relay) authorizeRBAC(req *http.Request) (Identity, error) {
	if r.cfg.Token == "" {
		return Identity{Role: RoleAdmin}, nil // 无 token 配置 = 全权限
	}
	// 从 header 提取 token
	raw := req.Header.Get("Authorization")
	if raw == "" {
		raw = "Bearer " + req.URL.Query().Get("token")
	}
	raw = strings.TrimPrefix(raw, "Bearer ")
	// 解析请求方的 token
	clientIdent := ParseToken(raw)
	// 解析服务端配置的 token(支持多 token:逗号分隔)
	// 简化:单一配置 token,解析其 secret 部分
	serverIdent := ParseToken(r.cfg.Token)
	// 常量时间比较 secret
	want := "Bearer " + clientIdent.Secret
	got := "Bearer " + serverIdent.Secret
	if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		return Identity{}, fmt.Errorf("无效的鉴权 token")
	}
	// 返回客户端身份(tenant + role)
	return clientIdent, nil
}

// CanPerform 检查身份是否有权执行操作。
func (id Identity) CanPerform(action string) bool {
	switch id.Role {
	case RoleAdmin:
		return true
	case RoleOperator:
		// operator 不能 revoke/电源(高危)
		_, blocked := operatorBlocked[action]
		return !blocked
	case RoleViewer:
		_, allowed := viewerAllowed[action]
		return allowed
	}
	return false
}

// operatorBlocked operator 被禁止的操作。
var operatorBlocked = map[string]bool{
	"agent.revoke":       true,
	"power.shutdown":     true,
	"power.restart":      true,
	"update.push":        true,
	"agent.enroll-token": true,
	"secret.put":         true,
	"secret.delete":      true,
}

// viewerAllowed viewer 允许的操作。
var viewerAllowed = map[string]bool{
	"agent.list":   true,
	"task.list":    true,
	"task.result":  true,
	"metrics":      true,
	"healthz":      true,
	"status":       true,
	"monitor.top":  true,
	"sysinfo":      true,
	"service.list": true,
	"proc.list":    true,
}

// CanAccessAgent 检查身份是否可访问指定 Agent(租户隔离)。
func (id Identity) CanAccessAgent(tags map[string]string) bool {
	if id.Tenant == "" {
		return true // admin 无租户限制
	}
	return tags["tenant"] == id.Tenant
}

// identityFromAuth 从已通过 authorize 的请求提取 Identity。
func identityFromAuth(req *http.Request) Identity {
	raw := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
	if raw == "" {
		return Identity{Role: RoleAdmin} // 控制台会话/无 Bearer = 管理员
	}
	return ParseToken(raw)
}

// RequireRole RBAC 端点守卫(架构短1:让 CanPerform 生效)。
// 自含鉴权(authorize + 角色检查),包装高危端点注册。
func (r *Relay) RequireRole(action string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if err := r.authorize(req); err != nil {
			writeJSON(w, http.StatusUnauthorized, errMap(err))
			return
		}
		id := identityFromAuth(req)
		if !id.CanPerform(action) {
			log.Printf("[rbac] 拒绝 %s 执行 %s (role=%s tenant=%s)", req.RemoteAddr, action, id.Role, id.Tenant)
			writeJSON(w, http.StatusForbidden, errMap(fmt.Errorf("角色 %s 无权执行 %s", id.Role, action)))
			return
		}
		h(w, req)
	}
}
