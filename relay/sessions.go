// Package main — 控制台会话管理（HttpOnly Cookie 鉴权）。
//
// 用途：Web 控制台登录后颁发有状态内存 session，替代把 Relay Token 放浏览器。
// session_id (crypto/rand 32 字节) → Session{Username, ExpiresAt}，滑动续期，
// 后台 sweep goroutine 清过期。
//
// 与 Relay Token 的关系：
//   - Relay Token：CLI/Agent 的 Bearer 鉴权（长期共享密钥，驱动 Agent）
//   - Console Session：浏览器登录后的短期会话（独立密码校验通过才颁发）
//
// authorize() 两者皆接受，互不影响；CLI Bearer 链路零改动。
package main

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// Session 单个控制台会话。
type Session struct {
	ID        string
	Username  string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// SessionStore 控制台会话存储（内存态，滑动续期 + 后台 sweep）。
//
// 设计权衡：不复用 SecretStore——它无 caller-supplied-key 的 Set、无 TTL 续期、
// 且 Get 会删除一次性条目。会话需要稳定 key（cookie 值）+ 滑动续期 + 显式 Delete，
// 故独立实现。对齐 secrets.go 的 sweepLoop 模式。
type SessionStore struct {
	mu      sync.Mutex
	m       map[string]*Session
	ttl     time.Duration
	stop    chan struct{}
	stopped bool
}

// NewSessionStore 构造，启动后台过期清理 goroutine。
// ttl 为会话有效期；每次 Get 命中后滑动续期 ttl。ttl≤0 时取默认 8h。
func NewSessionStore(ttl time.Duration) *SessionStore {
	if ttl <= 0 {
		ttl = 8 * time.Hour
	}
	s := &SessionStore{
		m:    make(map[string]*Session),
		ttl:  ttl,
		stop: make(chan struct{}),
	}
	go s.sweepLoop()
	return s
}

// Create 颁发新会话，返回会话快照（含 ID）。
func (s *SessionStore) Create(username string) (*Session, error) {
	id, err := generateSessionID()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	sess := &Session{
		ID:        id,
		Username:  username,
		CreatedAt: now,
		ExpiresAt: now.Add(s.ttl),
	}
	s.mu.Lock()
	s.m[id] = sess
	s.mu.Unlock()
	cp := *sess // 返回快照，调用方不应持有内部指针
	return &cp, nil
}

// Get 查询会话；命中且未过期则滑动续期，返回快照。
func (s *SessionStore) Get(id string) (*Session, bool) {
	if id == "" {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.m[id]
	if !ok {
		return nil, false
	}
	if time.Now().After(sess.ExpiresAt) {
		delete(s.m, id)
		return nil, false
	}
	// 滑动续期：每次访问刷新过期时间
	sess.ExpiresAt = time.Now().Add(s.ttl)
	cp := *sess
	return &cp, true
}

// Delete 删除会话（登出）。幂等。
func (s *SessionStore) Delete(id string) {
	if id == "" {
		return
	}
	s.mu.Lock()
	delete(s.m, id)
	s.mu.Unlock()
}

// Len 返回当前会话数（近似，含尚未扫到的过期项）。
func (s *SessionStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m)
}

// TTL 返回会话有效期（设置 Cookie MaxAge 用）。
func (s *SessionStore) TTL() time.Duration { return s.ttl }

// Close 停止后台 sweep（测试用，防 goroutine 泄漏）。幂等。
func (s *SessionStore) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}
	s.stopped = true
	close(s.stop)
}

// sweepLoop 定期清理过期会话。间隔 = min(ttl/4, 5min)，下限 1min。
func (s *SessionStore) sweepLoop() {
	interval := s.ttl / 4
	if interval > 5*time.Minute {
		interval = 5 * time.Minute
	}
	if interval < time.Minute {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			s.mu.Lock()
			now := time.Now()
			for id, sess := range s.m {
				if now.After(sess.ExpiresAt) {
					delete(s.m, id)
				}
			}
			s.mu.Unlock()
		}
	}
}

// generateSessionID 生成 32 字节随机 session ID（sess_ 前缀，对齐 sec_ 风格）。
func generateSessionID() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "sess_" + hex.EncodeToString(b), nil
}
