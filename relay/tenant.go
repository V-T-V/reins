// Package main — 多租户隔离骨架(L8 神级提升深化)。
//
// 在 rbac.go 的 Identity(租户:角色:密钥)之上,补全组织/角色/权限的正式模型:
//   - Organization:组织(tenant)实体,含名称、配额、启停。
//   - Role/Permission:角色→权限集合映射(替代 rbac.go 的硬编码 map,可配置)。
//   - TenantScope:把 Agent/任务列表按 Identity 的 tenant 过滤,实现数据隔离。
//
// 设计要点:
//   - 与现有 rbac.go 完全向后兼容(旧 token=admin 全权限)。
//   - 不引入 DB:用内存 store(可后续替换为持久化)。骨架优先,接口先行。
//   - 权限用细粒度 action 字符串(agent.list/task.run/power.shutdown...),
//     复用 rbac.go 的 CanPerform,通过 RolePolicy 解耦。
package main

import (
	"sync"
)

// Organization 一个租户(组织)。
type Organization struct {
	ID      string // 组织 ID(= tenant 名)
	Name    string // 显示名
	Quota   int    // 允许的最大 Agent 数(0=不限)
	Enabled bool   // 是否启用(禁用后该租户所有操作被拒)
}

// RolePolicy 角色策略:角色→允许的 action 集合。
// 可在启动时从配置注入,默认用内置策略(admin 全权限/operator/viewer)。
type RolePolicy struct {
	mu    sync.RWMutex
	roles map[string]map[string]bool // role → {action: true}
}

// TenantManager 多租户管理器(内存版骨架)。
type TenantManager struct {
	mu    sync.RWMutex
	orgs  map[string]*Organization // orgID → org
}

// NewTenantManager 构造空的多租户管理器。
func NewTenantManager() *TenantManager {
	return &TenantManager{orgs: make(map[string]*Organization)}
}

// NewDefaultRolePolicy 返回内置默认角色策略(与 rbac.go 的硬编码 map 一致)。
func NewDefaultRolePolicy() *RolePolicy {
	p := &RolePolicy{roles: make(map[string]map[string]bool)}
	// admin:全权限(用通配标记,实际 CanPerform 直接 true)
	p.roles[RoleAdmin] = nil // nil 表示全权限
	// operator:除高危外
	op := make(map[string]bool)
	p.roles[RoleOperator] = op // operator 复用 rbac 的 operatorBlocked 判定
	// viewer:只读
	p.roles[RoleViewer] = viewerAllowed
	return p
}

// CanPerform 用 RolePolicy 判定权限(可配置版本)。
// admin 始终 true;其它角色查 role→action 集合。
func (p *RolePolicy) CanPerform(role, action string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	allowed, ok := p.roles[role]
	if !ok {
		return false
	}
	if role == RoleAdmin {
		return true // admin 全权限
	}
	if role == RoleOperator {
		// operator:不在 blocked 列表即允许
		_, blocked := operatorBlocked[action]
		return !blocked
	}
	return allowed[action]
}

// GrantRole 给角色授予 action(运行期动态加权限)。
func (p *RolePolicy) GrantRole(role string, actions ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.roles[role] == nil {
		p.roles[role] = make(map[string]bool)
	}
	for _, a := range actions {
		p.roles[role][a] = true
	}
}

// --- 组织管理 ---

// CreateOrg 创建组织。已存在则返回 false。
func (m *TenantManager) CreateOrg(org Organization) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.orgs[org.ID]; exists {
		return false
	}
	if org.ID == "" {
		return false
	}
	m.orgs[org.ID] = &org
	return true
}

// GetOrg 查询组织。不存在返回 nil。
func (m *TenantManager) GetOrg(orgID string) *Organization {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if o, ok := m.orgs[orgID]; ok {
		cp := *o
		return &cp
	}
	return nil
}

// SetOrgEnabled 启用/禁用组织。禁用后该租户所有操作被拒。
func (m *TenantManager) SetOrgEnabled(orgID string, enabled bool) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if o, ok := m.orgs[orgID]; ok {
		o.Enabled = enabled
		return true
	}
	return false
}

// OrgEnabled 检查组织是否启用(不存在视为禁用,fail-closed)。
func (m *TenantManager) OrgEnabled(orgID string) bool {
	if orgID == "" {
		return true // 无租户(admin)不受组织约束
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if o, ok := m.orgs[orgID]; ok {
		return o.Enabled
	}
	return false // 组织不存在 = 禁用
}

// --- 租户隔离过滤 ---

// FilterAgentsByTenant 按身份的 tenant 过滤 Agent 列表,实现数据隔离。
// admin(无 tenant)看全部;租户用户只看 tags["tenant"]==自己 tenant 的。
func FilterAgentsByTenant(id Identity, agents []tenantAgentView) []tenantAgentView {
	if id.Tenant == "" {
		return agents // admin 全量
	}
	out := make([]tenantAgentView, 0, len(agents))
	for _, a := range agents {
		if a.Tags["tenant"] == id.Tenant {
			out = append(out, a)
		}
	}
	return out
}

// tenantAgentView Agent 的租户视图(只暴露隔离判定需要的字段)。
type tenantAgentView struct {
	AgentID string
	Tags    map[string]string
}

// CheckQuota 检查组织是否还能加 Agent(配额)。
// 返回 true 表示未超配额可加。
func (m *TenantManager) CheckQuota(orgID string, currentCount int) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	o, ok := m.orgs[orgID]
	if !ok {
		return true // 组织不存在不强制配额
	}
	if o.Quota <= 0 {
		return true // 0 = 不限
	}
	return currentCount < o.Quota
}
