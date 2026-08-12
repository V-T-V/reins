// Package main — 控制台登录限流(防暴力破解/字典攻击,2026-08-12 安全加固)。
//
// 按 IP 维度统计连续失败次数,达阈值则临时锁定该 IP(锁定期间直接拒绝,
// 不查密码 —— 避免计时侧信道 + 减速字典攻击)。成功登录或锁定过期自动清除。
//
// 存储:内存态(单 Relay 够用;多 Relay 时可迁 Redis,对齐 TaskStore 模式)。
// 重启清空(可接受:攻击者要重新累计;合法用户无感)。
package main

import (
	"sync"
	"time"
)

const (
	loginMaxFails      = 5                // 连续失败阈值,达到则锁定
	loginLockout       = 5 * time.Minute  // 锁定时长
	loginSweepInterval = 10 * time.Minute // 后台清扫间隔(清过期/陈旧条目)
	loginEntryTTL      = 15 * time.Minute // 条目最长保留(无活动)
)

// loginEntry 单个 IP 的登录尝试记录。
type loginEntry struct {
	fails       int       // 当前窗口连续失败次数
	lockedUntil time.Time // 锁定截止时间(零值=未锁)
	lastAttempt time.Time // 最近一次尝试(用于清扫)
}

// LoginLimiter 登录限流器(IP → 尝试记录)。
type LoginLimiter struct {
	mu   sync.Mutex
	m    map[string]*loginEntry
	stop chan struct{}
}

// NewLoginLimiter 构造,启动后台清扫 goroutine。
func NewLoginLimiter() *LoginLimiter {
	l := &LoginLimiter{
		m:    make(map[string]*loginEntry),
		stop: make(chan struct{}),
	}
	go l.sweepLoop()
	return l
}

// Allow 返回该 IP 是否允许尝试 + 锁定剩余时间(0=未锁)。
// 锁定期内拒绝(不查密码,防计时侧信道)。
func (l *LoginLimiter) Allow(ip string) (allowed bool, lockedRemaining time.Duration) {
	if ip == "" {
		return true, 0 // 无 IP(异常)不限流,避免误伤
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.m[ip]
	if !ok {
		return true, 0
	}
	if rem := time.Until(e.lockedUntil); rem > 0 {
		return false, rem
	}
	return true, 0
}

// RecordFail 记录一次失败。达阈值则锁定(返回锁定时长;0=未触发锁定)。
// 锁定后重置 fails 计数(解锁后重新累计)。
func (l *LoginLimiter) RecordFail(ip string) time.Duration {
	if ip == "" {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.m[ip]
	if !ok {
		e = &loginEntry{}
		l.m[ip] = e
	}
	e.fails++
	e.lastAttempt = time.Now()
	if e.fails >= loginMaxFails {
		e.lockedUntil = time.Now().Add(loginLockout)
		e.fails = 0
		return loginLockout
	}
	return 0
}

// RecordSuccess 成功登录,清除该 IP 的失败记录。
func (l *LoginLimiter) RecordSuccess(ip string) {
	if ip == "" {
		return
	}
	l.mu.Lock()
	delete(l.m, ip)
	l.mu.Unlock()
}

// Fails 返回当前失败计数(测试/调试用)。
func (l *LoginLimiter) Fails(ip string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e, ok := l.m[ip]; ok {
		return e.fails
	}
	return 0
}

// sweepLoop 定期清扫:清已过期且陈旧的条目(释放内存)。
func (l *LoginLimiter) sweepLoop() {
	t := time.NewTicker(loginSweepInterval)
	defer t.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-t.C:
			l.mu.Lock()
			now := time.Now()
			for ip, e := range l.m {
				// 锁定已过期 + 超过 TTL 无活动 → 清
				if now.After(e.lockedUntil) && now.Sub(e.lastAttempt) > loginEntryTTL {
					delete(l.m, ip)
				}
			}
			l.mu.Unlock()
		}
	}
}

// Close 停止 sweep(测试用,防 goroutine 泄漏)。幂等。
func (l *LoginLimiter) Close() {
	if l == nil {
		return
	}
	select {
	case <-l.stop:
	default:
		close(l.stop)
	}
}
