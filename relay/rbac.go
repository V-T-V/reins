// Package main — 多租户 RBAC:凭据绑定身份 + 对象级授权。
//
// 凭据格式(向后兼容):
//
//	旧:   "secret123"                          → 无租户,全权限(admin)
//	新:   "tenant:role:secret"                  → 租户隔离 + 角色权限
//	多凭据: ":admin:adminsecret,acme:operator:s1,globex:viewer:s2"(逗号分隔;列表里每条都必须写全角色)
//
// 安全模型(2026-11 租户隔离加固):
//   - 服务端维护「凭据 → 固定身份」索引:tenant/role 一律取服务端配置条目的值;
//     请求方声明的前缀只用于取出 Secret 段参与比较,绝不参与身份判定(防提权)。
//   - Agent 归属 = 注册连接所用凭据的租户(服务端绑定,Registry 维护);
//     Agent 自报的 Tags["tenant"] 仅作展示,不参与鉴权。
//   - 无租户的凭据(admin)不受对象过滤;租户凭据只能访问本租户对象,
//     未知(离线/不存在)的 Agent 对非 admin 一律拒绝(fail-closed)。
package main

import (
	"context"
	"crypto/subtle"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"unicode"
)

// RBACRole 角色常量。
const (
	RoleAdmin    = "admin"
	RoleOperator = "operator"
	RoleViewer   = "viewer"
)

// Identity 凭据绑定的身份。
type Identity struct {
	Tenant string // 租户(空 = 全局 admin)
	Role   string // 角色(admin/operator/viewer)
	Secret string // 仅服务端凭据索引内部使用;authenticate 返回值不携带(置空)
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

// validRole 角色合法性(未知角色 fail-closed)。
func validRole(role string) bool {
	switch role {
	case RoleAdmin, RoleOperator, RoleViewer:
		return true
	}
	return false
}

// credentialLabel 凭据的安全展示形式(绝不回显 secret)。
func credentialLabel(id Identity) string {
	if id.Tenant == "" {
		return "admin:***"
	}
	return id.Tenant + ":" + id.Role + ":***"
}

// validTenantID 租户 ID 字符集:字母数字与 _ -,1..64 字符。
// 租户 ID 会用作审计标注、组织键、脚本库子目录名——必须是安全的单一路径段。
func validTenantID(t string) bool {
	if t == "" || len(t) > 64 {
		return false
	}
	for i := 0; i < len(t); i++ {
		c := t[i]
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-'
		if !ok {
			return false
		}
	}
	return true
}

// parseCredentials 解析服务端配置的凭据。语法(无歧义):
//
//	不含逗号 → 单凭据:纯 secret(= 全局 admin,旧格式)或 tenant:role:secret
//	含逗号   → 多凭据列表,**每条都必须是 [:tenant:]role:secret 形式**
//
// 列表里为什么不许裸 secret:裸条目有歧义——既可能是 admin 凭据,也可能是某个
// **含逗号的旧式单 token** 被拆开的一半。"ab,cd" 若被接受,就凭空多出 ab / cd 两个
// admin secret,而原合法客户端反而被锁在门外(凭据被悄悄削弱)。所以列表里的全局 admin
// 一律写成 :admin:secret。出示时仍可用裸 secret(匹配只看 Secret 段),只是配置必须写全。
//
// role / tenant ID 非法、secret 复用、secret 含空白或控制字符 → error(启动即 Fatal,见 NewRelay)。
func parseCredentials(cfgToken string) ([]Identity, error) {
	parts := strings.Split(cfgToken, ",")
	multi := len(parts) > 1
	var entries []Identity
	seen := map[string]string{} // secret → label(用于重复报错,不回显 secret)
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if multi && strings.Count(part, ":") < 2 {
			return nil, fmt.Errorf(
				"多凭据列表中的 %q 不是 :tenant:role:secret 形式(列表里的全局 admin 请写 :admin:secret;若这其实是一个含逗号的旧式单 token,请更换它——逗号现在是凭据分隔符)", part)
		}
		id := ParseToken(part)
		if !validRole(id.Role) {
			return nil, fmt.Errorf("token 凭据 %q 的角色 %q 非法(须为 admin/operator/viewer)", credentialLabel(id), id.Role)
		}
		// 仅当声明了租户时才校验 ID(纯 secret / ":role:secret" 的 Tenant 为空 = 全局域,合法)。
		// 注意不能对空值报错:否则所有旧格式单凭据部署都会在 NewRelay 里 Fatal。
		if id.Tenant != "" && !validTenantID(id.Tenant) {
			return nil, fmt.Errorf("token 凭据 %s 的租户 ID 非法(仅允许字母数字与 _ -,最长 64)", credentialLabel(id))
		}
		// 空白/控制字符在 Bearer 头与 cookie 两个通道里行为不一致,拒绝。
		// ("tenant:role" 漏写 secret 会落到这里:ParseToken 把它当成 secret 为 "tenant:role" 的
		// 纯 secret——角色名里没有空白,故追加一条专门的守卫。)
		if strings.ContainsFunc(id.Secret, func(c rune) bool { return unicode.IsSpace(c) || unicode.IsControl(c) }) {
			return nil, fmt.Errorf("token 凭据 %s 的 secret 含空白/控制字符,请更换", credentialLabel(id))
		}
		// "tenant:role" 漏写 secret 的守卫:ParseToken 会把它当成 secret 为 "tenant:role" 的
		// 纯 secret,于是得到一个 secret 恰为 "acme:operator" 的 admin 凭据。恰好一个冒号且
		// 第二段是角色名 → 判定为漏写。
		if seg := strings.SplitN(part, ":", 2); len(seg) == 2 && validRole(seg[1]) {
			return nil, fmt.Errorf("token 凭据 %q 缺少 secret(应为 tenant:role:secret)", part)
		}
		if prev := seen[id.Secret]; prev != "" {
			return nil, fmt.Errorf("token 凭据 %s 与 %s 使用了同一 secret(禁止复用)", prev, credentialLabel(id))
		}
		seen[id.Secret] = credentialLabel(id)
		entries = append(entries, id)
	}
	return entries, nil
}

// credentialIndex 惰性构建并缓存服务端凭据索引(配置启动后只读)。
// 正常启动路径已由 NewRelay 校验过;此处兜底处理直接构造 Relay 的场景
// (测试字面量构造):跳过非法条目并记日志,不 Fatal。
func (r *Relay) credentialIndex() []Identity {
	r.credMu.Lock()
	defer r.credMu.Unlock()
	if r.credBuilt {
		return r.credentials
	}
	r.credBuilt = true
	entries, err := parseCredentials(r.cfg.Token)
	if err != nil {
		log.Printf("[auth] %v(将忽略冲突/非法凭据条目)", err)
		entries = entries[:0]
		for _, part := range strings.Split(r.cfg.Token, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			id := ParseToken(part)
			if !validRole(id.Role) {
				continue
			}
			dup := false
			for _, e := range entries {
				if e.Secret == id.Secret {
					dup = true
					break
				}
			}
			if !dup {
				entries = append(entries, id)
			}
		}
	}
	r.credentials = entries
	return entries
}

// credentialFromRequest 提取请求凭据。
// H4:仅 Authorization 头 + 同源 reins_token cookie(query 会进日志/Referer,恒拒)。
// cookie 值由前端 encodeURIComponent 写入(:→%3A),Go 的 Cookie 解析不做 URL 解码,
// 此处补齐;解码失败回退原值。
func credentialFromRequest(req *http.Request) string {
	raw := req.Header.Get("Authorization")
	if raw != "" {
		return strings.TrimPrefix(raw, "Bearer ")
	}
	if c, err := req.Cookie("reins_token"); err == nil && c.Value != "" {
		v := c.Value
		if strings.Contains(v, "%") {
			if dec, derr := url.QueryUnescape(v); derr == nil {
				v = dec
			}
		}
		return v
	}
	return ""
}

// identityCtxKey 请求上下文中的身份键(仅经 authenticateCtx 写入)。
type identityCtxKey struct{}

// matchCredential 把出示的凭据串匹配到服务端凭据条目。
// 只拿出示串的 Secret 段参与常量时间比较;返回**服务端条目**(含其绑定的 tenant/role)。
// 全条目遍历比较(不提前返回,尽量抹平多凭据下的计时差异)。
func (r *Relay) matchCredential(cred string) (Identity, bool) {
	if cred == "" {
		return Identity{}, false
	}
	presented := ParseToken(cred).Secret
	var matched Identity
	found := false
	for _, srv := range r.credentialIndex() {
		if subtle.ConstantTimeCompare([]byte(presented), []byte(srv.Secret)) == 1 && !found {
			matched, found = srv, true
		}
	}
	return matched, found
}

// canonical 凭据的规范形式(服务端配置里的写法;含 secret,只用于把它交还给
// 持有者本人——例如 bootstrap 回显调用者自己的凭据)。
func (id Identity) canonical() string {
	if id.Tenant == "" {
		return id.Secret
	}
	return id.Tenant + ":" + id.Role + ":" + id.Secret
}

// authenticate 鉴权并返回**服务端绑定**的身份。
//
//   - cfg.Token 为空:单信任域模式,返回 admin(所有接入方共属一个信任域)。
//   - 凭据匹配:客户端凭据的 Secret 段与服务端任一凭据条目常量时间相等时,
//     身份取**该服务端条目**的 tenant/role——请求方声明的前缀被完全忽略。
//   - 组织禁用/删除:该条目身份的租户立即失效(fail-closed,P0-1)。
//   - 错误消息是 e2e/verify 的契约面,逐字保留。
func (r *Relay) authenticate(req *http.Request) (Identity, error) {
	if r.cfg.Token == "" {
		return Identity{Role: RoleAdmin}, nil
	}
	matched, ok := r.matchCredential(credentialFromRequest(req))
	if !ok {
		return Identity{}, fmt.Errorf("无效的鉴权 token")
	}
	id := Identity{Tenant: matched.Tenant, Role: matched.Role} // Secret 不外带
	if r.tenants != nil && !r.tenants.OrgEnabled(id.Tenant) {
		return Identity{}, fmt.Errorf("所属组织已被禁用或删除")
	}
	return id, nil
}

// authenticateCtx 鉴权并把身份写入请求上下文,供深层代码 identityOf 取用。
// 返回的 req 已携带上下文,调用方必须使用返回值继续传递。
func (r *Relay) authenticateCtx(req *http.Request) (*http.Request, Identity, error) {
	id, err := r.authenticate(req)
	if err != nil {
		return req, id, err
	}
	return req.WithContext(context.WithValue(req.Context(), identityCtxKey{}, id)), id, nil
}

// identityOf 从请求上下文取身份(须经 authenticateCtx 写入)。
// ok=false 表示未写入——调用方应视为未鉴权并拒绝,不得默认提权。
func identityOf(req *http.Request) (Identity, bool) {
	v, ok := req.Context().Value(identityCtxKey{}).(Identity)
	return v, ok
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

// CanAccessAgent 检查身份是否可访问指定 Agent(对象级租户隔离)。
// agentTenant 为服务端绑定的归属值(Register 时从连接凭据取);空 = 仅 admin 可见。
func (id Identity) CanAccessAgent(agentTenant string) bool {
	if id.Tenant == "" {
		return true // admin 无租户限制
	}
	return agentTenant == id.Tenant
}

// canAccessAgent 对象级授权:admin 全量;租户凭据仅本租户 Agent。
// 归属取服务端绑定(在线取注册表,离线回落持久绑定)——租户可对自己**离线**的
// Agent 缓存任务(上线补投时仍只经同租户连接,见 dispatchTask)。
// 未知 Agent(不在线且从未绑定)对非 admin 一律拒绝(fail-closed,防跨租户探测)。
func (r *Relay) canAccessAgent(id Identity, agentID string) bool {
	if id.Tenant == "" {
		return true
	}
	tenant, ok := r.agentBoundTenant(agentID)
	return ok && tenant == id.Tenant
}

// requireAgentAccess 对象级授权守卫:失败写 403(动作类拒绝码)并返回 false。
func (r *Relay) requireAgentAccess(w http.ResponseWriter, id Identity, agentID string) bool {
	if r.canAccessAgent(id, agentID) {
		return true
	}
	log.Printf("[tenant] 拒绝访问 Agent %s (调用者租户 %q 不匹配)", agentID, id.Tenant)
	writeJSON(w, http.StatusForbidden, errMap(fmt.Errorf("无权访问该 Agent")))
	return false
}

// auditLabel 审计身份标注("tenant/role";admin 域记 "admin")。
func (id Identity) auditLabel() string {
	if id.Tenant == "" {
		return id.Role
	}
	return id.Tenant + "/" + id.Role
}

// --- viewer 只读落地(2026) ---
//
// 角色矩阵一直声明「viewer — 只读」,但此前只有 6 个端点调用了 CanPerform,
// 其余端点仅验凭据:viewer 可以开 shell、传文件、建 cron、触发自更新。
// 这里改为**默认拒绝**:viewer 只能访问显式列出的只读端点(新增端点对 viewer
// 自动不可见,直到被显式加入白名单),以及提交只读型任务(monitor)。

// viewerReadOnlyExact viewer 可访问的只读端点(精确路径;GET/HEAD)。
var viewerReadOnlyExact = map[string]bool{
	"/api/agents":           true,
	"/api/tasks":            true, // 任务列表
	"/api/dag":              true,
	"/api/events":           true, // SSE
	"/api/metrics":          true,
	"/api/crons":            true,
	"/api/scheduler/best":   true,
	"/api/tools":            true,
	"/api/updates":          true,
	"/api/updates/latest":   true,
	"/api/updates/download": true, // Agent 二进制(与 /dl/ 同一文件,本就公开下发)
	"/api/version":          true,
	"/api/audit/query":      true, // 自身租户范围(服务端按身份过滤)
	"/api/audit/export":     true,
	"/api/scripts":          true, // 自身租户命名空间
	// 未列入 = 对 viewer 默认拒绝:/api/bootstrap(部署助手,operator 及以上)、
	// /api/secrets(RequireRole secret.get 另行拒绝)、/api/tunnel/*(数据面)等。
}

// viewerReadOnlyPrefix viewer 可访问的只读端点(路径前缀;GET/HEAD)。
// 这些前缀下的写操作均为 POST/DELETE,方法检查已把它们挡在外面。
var viewerReadOnlyPrefix = []string{
	"/api/tasks/",   // 任务详情
	"/api/batch/",   // 批次状态/进度
	"/api/dag/",     // 编排状态
	"/api/scripts/", // 脚本内容(读)
}

// viewerTaskTypes viewer 可提交的任务类型——必须天然只读。
// 注意:CLI 的 sysinfo/service/proc 是 powershell 任务的语义化包装,Relay 无法区分
// 「查服务状态」与任意命令,所以 viewer 不能提交 powershell;只放行 monitor。
var viewerTaskTypes = map[string]bool{
	"monitor": true,
}

// viewerMayCall 判断只读请求是否对 viewer 开放。
func viewerMayCall(req *http.Request) bool {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		return false
	}
	path := req.URL.Path
	if viewerReadOnlyExact[path] {
		return true
	}
	for _, p := range viewerReadOnlyPrefix {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// enforceRolePolicy /api/ 路由的角色落地门(默认拒绝 viewer 的非只读操作)。
// 只对**已鉴权为 viewer** 的请求设限;鉴权失败的原样交给下游 handler——
// 由它返回契约的 401,或处理部署专用 Token(/api/agents?agent_id= 单机查询)。
// POST /api/tasks 放行到 handler,由 handler 按 viewerTaskTypes 限定任务类型。
func (r *Relay) enforceRolePolicy(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if r.cfg.Token != "" {
			if id, err := r.authenticate(req); err == nil && id.Role == RoleViewer {
				allow := viewerMayCall(req) || (req.Method == http.MethodPost && req.URL.Path == "/api/tasks") // 后者放行到 handler 按 viewerTaskTypes 限定
				if !allow {
					log.Printf("[rbac] 拒绝 viewer 执行非只读操作 %s %s", req.Method, req.URL.Path)
					writeJSON(w, http.StatusForbidden, errMap(fmt.Errorf("角色 viewer 为只读,无权执行该操作")))
					return
				}
			}
		}
		next.ServeHTTP(w, req)
	})
}

// RequireRole RBAC 端点守卫(角色→action 检查)。
// 身份取 authenticate 的服务端绑定结果并写入上下文(cookie/Bearer 同一道闸)。
func (r *Relay) RequireRole(action string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		req, id, err := r.authenticateCtx(req)
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, errMap(err))
			return
		}
		if !id.CanPerform(action) {
			log.Printf("[rbac] 拒绝 %s 执行 %s (role=%s tenant=%s)", req.RemoteAddr, action, id.Role, id.Tenant)
			writeJSON(w, http.StatusForbidden, errMap(fmt.Errorf("角色 %s 无权执行 %s", id.Role, action)))
			return
		}
		h(w, req)
	}
}

// identityOf 取调用者身份:优先用请求上下文(经 RequireRole / authenticateCtx 写入),
// 没有就现场鉴权。handler 因此不必假定自己一定被 RequireRole 包装过——
// 直接挂载(部分既有测试、以及将来新增的注册点)同样成立,不会退化成 401。
// 注意:这里只解析身份,不做角色检查;角色门仍由 RequireRole 负责。
func (r *Relay) identityOf(req *http.Request) (Identity, bool) {
	if id, ok := identityOf(req); ok {
		return id, true
	}
	id, err := r.authenticate(req)
	return id, err == nil
}
