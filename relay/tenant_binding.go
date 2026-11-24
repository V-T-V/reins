package main

// Agent 租户归属绑定(2026 租户加固)。
//
// 归属的权威来源是**服务端在注册时按连接凭据写入的绑定**,不是 Agent 自报的 Tags:
//
//   - 携带租户凭据的连接首次注册某 agentID 时,该 ID 被钉到这个租户并持久化
//     (TaskStore 的 agent_tenants;未启用持久化时仅进程内)。
//   - 此后其它租户、以及**无租户**的连接(部署专用 Token / 不带凭据的 mTLS)
//     都不得注册该 ID —— install.ps1 未鉴权下发且烘焙了部署 Token,任何人都能拿到它;
//     若放任顶替,顶替者就能接走该 Agent 的缓存任务(pending 补投)。
//   - 全局 admin 凭据不受此约束(admin 本就可见全部)。
//   - 无租户连接不为未绑定的 ID 建立绑定(单 admin 部署零行为变化)。
//
// 离线归属:绑定离线后仍可查,租户可对自己离线的 Agent 缓存任务;重新上线的连接
// 必须仍是同一租户(见上),所以缓存任务不会被别的租户接走。投递层另有一道中央闸
// (dispatchTask):租户任务只经同租户连接投递。

import (
	"fmt"
	"log"
)

// agentPins / pinMu 进程内绑定缓存(Relay 结构体字段)。
// 未启用持久化(store == nil)时它就是唯一存储;启用时作为 store 的读缓存。

// agentBoundTenant 返回 Agent 的服务端归属:在线取注册表(当前连接),离线回落绑定。
// ok=false 表示未知 Agent(不在线且从未绑定)——调用方对非 admin 必须 fail-closed。
func (r *Relay) agentBoundTenant(agentID string) (string, bool) {
	if agentID == "" || r.agents == nil {
		return "", false
	}
	if t, online := r.agents.TenantOf(agentID); online {
		return t, true
	}
	r.pinMu.Lock()
	defer r.pinMu.Unlock()
	if t, ok := r.agentPins[agentID]; ok {
		return t, true
	}
	if r.store == nil {
		return "", false
	}
	t, ok := r.store.AgentTenant(agentID)
	if !ok {
		return "", false
	}
	r.cachePinLocked(agentID, t)
	return t, true
}

// cachePinLocked 写入进程缓存(须持 pinMu;容忍结构体字面量构造的 nil map)。
func (r *Relay) cachePinLocked(agentID, tenant string) {
	if r.agentPins == nil {
		r.agentPins = make(map[string]string)
	}
	r.agentPins[agentID] = tenant
}

// errAgentTenantBound 归属冲突(日志用;不回给连接方,避免泄漏持有者租户名)。
func errAgentTenantBound(agentID, holder, presented string) error {
	return fmt.Errorf("agent %s 已归属租户 %q,租户 %q 的连接不得顶替", agentID, holder, presented)
}

// admitAgentRegistration 注册准入:连接的租户必须等于该 agentID 的既有绑定。
// adminCred 的连接(全局 admin 凭据/未启用鉴权)直接放行且不改写绑定。
// 首次以租户凭据注册时建立绑定;持久化失败则拒绝(fail-closed:绑不住就不放进来)。
func (r *Relay) admitAgentRegistration(wc *wsConn, agentID string) error {
	if wc.adminCred {
		return nil
	}
	r.pinMu.Lock()
	defer r.pinMu.Unlock()
	if t, ok := r.agentPins[agentID]; ok && t != wc.tenant {
		return errAgentTenantBound(agentID, t, wc.tenant)
	}
	if r.store != nil {
		if cur, ok := r.store.AgentTenant(agentID); ok {
			r.cachePinLocked(agentID, cur)
			if cur != wc.tenant {
				return errAgentTenantBound(agentID, cur, wc.tenant)
			}
			return nil // 同租户重连
		}
	}
	if wc.tenant == "" {
		return nil // 无租户连接:只能注册未绑定的 ID,且不建立绑定
	}
	bound := wc.tenant
	if r.store != nil {
		var err error
		if bound, err = r.store.BindAgentTenant(agentID, wc.tenant); err != nil {
			return fmt.Errorf("绑定 agent %s 的租户失败: %w", agentID, err)
		}
	}
	r.cachePinLocked(agentID, bound)
	if bound != wc.tenant { // 并发首注册:insert-if-absent 的后到者
		return errAgentTenantBound(agentID, bound, wc.tenant)
	}
	log.Printf("[tenant] agent %s 绑定到租户 %q", agentID, bound)
	return nil
}

// provisionTenantOrgs 凭据列表即租户清单:为配置里的每个租户凭据确保组织存在
// (启用、不限配额)。组织此前只能在测试里创建——生产没有建组织的接口,
// 而 OrgEnabled 对不存在的组织 fail-closed,租户凭据会永远 401。
// 已存在的组织(含被禁用的)一律不动:禁用是运行期闸门,跨重启保持;
// 要彻底收回一个租户,从配置里删掉它的凭据。
func (r *Relay) provisionTenantOrgs(quiet bool) {
	if r.tenants == nil {
		return
	}
	seen := make(map[string]bool)
	for _, c := range r.credentialIndex() {
		if c.Tenant == "" || seen[c.Tenant] {
			continue
		}
		seen[c.Tenant] = true
		if r.tenants.GetOrg(c.Tenant) == nil {
			if r.tenants.CreateOrg(Organization{ID: c.Tenant, Name: c.Tenant, Enabled: true}) {
				if !quiet {
					log.Printf("[auth] 已为租户凭据 %q 建立组织", c.Tenant)
				}
			}
		}
	}
}

// mayClaimAgent 「能否把某个 agentID 纳入自己的租户」——注册令牌签发用。
// 与 canAccessAgent 的区别:那边针对**已接入**的对象(未知 Agent 对租户 fail-closed),
// 这里的目标恰恰是尚未接入的新 Agent,所以规则与注册准入同款:
// 未绑定(谁都能认领),或已绑定在本租户;绑在别家的一律不行。
func (r *Relay) mayClaimAgent(id Identity, agentID string) bool {
	if id.Tenant == "" {
		return true
	}
	tenant, bound := r.agentBoundTenant(agentID)
	return !bound || tenant == id.Tenant
}
