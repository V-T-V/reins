// Package main — Secret Management(XL1 神级提升)。
//
// 安全凭据管理:任务引用 secret 引用而非明文传递,凭据不下落 audit/bbolt。
//
// 用途:下发含数据库密码/API Key 的命令时,凭据存 Relay 侧 secret store,
// 任务 payload 只引用 secret_ref。Agent 执行前用短期 token 拉取 secret 值,
// 审计日志只记录 ref 不记录值。
//
// API:
//
//	POST /api/secrets        — 存储凭据(返回 ref)
//	GET  /api/secrets/{ref}  — Agent 拉取凭据(短期 token 鉴权)
//	DELETE /api/secrets/{ref} — 删除凭据
//
// 安全:
//   - 凭据存内存(不落盘,Relay 重启清空)
//   - TTL 自动过期(默认 1 小时)
//   - 一次性(被 Agent 拉取后自动删除,防重放)
//   - 审计只记 ref 不记值
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/V-T-V/reins/proto"
)

// SecretStore 凭据存储(内存态,TTL + 一次性)。
type SecretStore struct {
	mu      sync.RWMutex
	secrets map[string]*secretEntry
}

type secretEntry struct {
	value     string
	createdAt time.Time
	ttl       time.Duration
	oneTime   bool
	tenant    string // 2026 租户加固:创建者凭据绑定的租户;空=admin 域(仅 admin 可读)
}

// NewSecretStore 构造。
func NewSecretStore() *SecretStore {
	ss := &SecretStore{secrets: make(map[string]*secretEntry)}
	go ss.sweepLoop()
	return ss
}

// Put 存储凭据,返回 ref(2026 租户加固:记录创建者租户)。
func (ss *SecretStore) Put(value string, ttl time.Duration, oneTime bool, tenant string) (string, error) {
	ref, err := generateSecretRef()
	if err != nil {
		return "", err
	}
	if ttl == 0 {
		ttl = time.Hour
	}
	ss.mu.Lock()
	ss.secrets[ref] = &secretEntry{
		value:     value,
		createdAt: time.Now(),
		ttl:       ttl,
		oneTime:   oneTime,
		tenant:    tenant,
	}
	ss.mu.Unlock()
	return ref, nil
}

// Get 拉取凭据(admin 语义:不设租户限制;保留给既有调用方与测试)。
// 归属受限的读取请走 GetForTenant。
func (ss *SecretStore) Get(ref string) (string, bool) {
	return ss.GetForTenant(ref, "")
}

// GetForTenant 拉取凭据(一次性则拉取后删除)。
// 2026 租户加固:tenant 非空时只允许同租户读取;归属校验在消费一次性凭据**之前**——
// 被拒的读取不烧掉 one-time。
func (ss *SecretStore) GetForTenant(ref, tenant string) (string, bool) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	entry, ok := ss.secrets[ref]
	if !ok {
		return "", false
	}
	if tenant != "" && entry.tenant != tenant {
		return "", false
	}
	// TTL 检查
	if time.Since(entry.createdAt) > entry.ttl {
		delete(ss.secrets, ref)
		return "", false
	}
	value := entry.value
	// 一次性:拉取后删除
	if entry.oneTime {
		delete(ss.secrets, ref)
	}
	return value, true
}

// Delete 删除凭据。
func (ss *SecretStore) Delete(ref string) {
	ss.mu.Lock()
	delete(ss.secrets, ref)
	ss.mu.Unlock()
}

// sweepLoop 定期清理过期凭据。
func (ss *SecretStore) sweepLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		ss.mu.Lock()
		now := time.Now()
		for ref, entry := range ss.secrets {
			if now.Sub(entry.createdAt) > entry.ttl {
				delete(ss.secrets, ref)
			}
		}
		ss.mu.Unlock()
	}
}

// generateSecretRef 生成 32 字节随机 ref。
func generateSecretRef() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "sec_" + hex.EncodeToString(b), nil
}

// secrets 全局实例。
var secretStore = NewSecretStore()

// handleSecretPut POST /api/secrets —— 存储凭据(返回 ref)。
// RequireRole("secret.put") 已完成鉴权并把身份写入上下文。
func (r *Relay) handleSecretPut(w http.ResponseWriter, req *http.Request) {
	id, ok := r.identityOf(req)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, errMap(fmt.Errorf("无效的鉴权 token")))
		return
	}
	var body struct {
		Value   string `json:"value"`    // 凭据明文(不记入 audit)
		TTL     int    `json:"ttl"`      // 秒,默认 3600
		OneTime bool   `json:"one_time"` // 一次性(默认 true)
		// Tenant 把凭据放进指定租户的作用域。只有全局 admin 可以指定
		// (否则 admin 创建的凭据全落在 admin 域,租户永远用不上 Secret);
		// 租户身份的凭据固定在本租户,此字段被忽略。
		Tenant string `json:"tenant"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("解析失败: %w", err)))
		return
	}
	if body.Value == "" {
		writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("value 不能为空")))
		return
	}
	scope := id.Tenant // 凭据归属的作用域 = 创建者租户
	if id.Tenant == "" && body.Tenant != "" {
		if !validTenantID(body.Tenant) {
			writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("tenant 非法")))
			return
		}
		scope = body.Tenant
	}
	ttl := time.Duration(body.TTL) * time.Second
	oneTime := body.OneTime
	if body.TTL == 0 {
		ttl = time.Hour
		oneTime = true // 默认一次性
	}
	ref, err := secretStore.Put(body.Value, ttl, oneTime, scope)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errMap(err))
		return
	}
	// 审计只记 ref 与作用域,不记 value
	if r.audit != nil {
		r.audit.Log(AuditEntry{Event: "secret_stored", Operator: id.auditLabel(), Detail: fmt.Sprintf("%s scope=%q", ref, scope), Tenant: id.Tenant})
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"ref":      ref,
		"ttl":      int(ttl.Seconds()),
		"one_time": oneTime,
		"message":  "凭据已存储,任务 payload 中用 secret_ref 引用",
	})
}

// handleSecretGet GET /api/secrets/{ref} —— 拉取凭据。
// 2026 租户加固:RequireRole("secret.get") 守卫(viewer 拒)+ 同租户绑定
// (admin 域创建的凭据仅 admin 可读;归属校验在消费 one-time 之前,被拒不烧)。
func (r *Relay) handleSecretGet(w http.ResponseWriter, req *http.Request) {
	id, ok := r.identityOf(req)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, errMap(fmt.Errorf("无效的鉴权 token")))
		return
	}
	ref := req.URL.Path[len("/api/secrets/"):]
	if ref == "" {
		writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("缺少 ref")))
		return
	}
	value, ok := secretStore.GetForTenant(ref, id.Tenant)
	if !ok {
		writeJSON(w, http.StatusNotFound, errMap(fmt.Errorf("凭据不存在或已过期")))
		return
	}
	// 注:不审计凭据值(只记 ref)
	if r.audit != nil {
		r.audit.Log(AuditEntry{Event: "secret_accessed", Operator: id.auditLabel(), Detail: ref, Tenant: id.Tenant})
	}
	writeJSON(w, http.StatusOK, map[string]any{"value": value})
}

// handleSecretDelete DELETE /api/secrets/{ref} —— 删除凭据(RequireRole secret.delete=admin)。
func (r *Relay) handleSecretDelete(w http.ResponseWriter, req *http.Request) {
	if err := r.authorize(req); err != nil {
		writeJSON(w, http.StatusUnauthorized, errMap(err))
		return
	}
	ref := req.URL.Path[len("/api/secrets/"):]
	secretStore.Delete(ref)
	writeJSON(w, http.StatusOK, map[string]any{"ref": ref, "deleted": true})
}

// placeholder 避免 proto 未用 import(实际 secret 走 HTTP 不经 WS)。
var _ proto.Envelope
