// Package main — 控制台会话鉴权端点(HttpOnly Cookie)。
//
// 三个端点(均不走 authorize,自行读 cookie / 校验密码):
//
//	GET  /api/console/session — 查询登录态(前端启动判断显示登录页还是主 UI)
//	POST /api/console/login   — 密码校验 → 颁发 session cookie
//	POST /api/console/logout  — 登出(清 session + 清 cookie)
//
// 安全:
//   - ConsolePassword 空 → 登录端点 404(控制台鉴权未启用)
//   - subtle.ConstantTimeCompare 防计时侧信道
//   - cookie: HttpOnly + Secure(自适应 X-Forwarded-Proto) + SameSite=Lax
//   - 审计 login_success / login_failed / logout(Operator = 用户名)
package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// consoleLoginReq 登录请求体。
type consoleLoginReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// cookieName 返回控制台 session cookie 名(默认 gormm_session)。
func (r *Relay) cookieName() string {
	if r.cfg.ConsoleCookieName != "" {
		return r.cfg.ConsoleCookieName
	}
	return "gormm_session"
}

// handleConsoleSession GET /api/console/session —— 查询当前登录态(公开,不鉴权)。
//
// 始终 200。前端据此决策:
//   - logged_in=true            → 已登录,直接进主 UI
//   - console_auth=true,未登录  → 显示登录页
//   - console_auth=false        → 控制台鉴权未启用(dev 或纯 Bearer),尝试无鉴权进
func (r *Relay) handleConsoleSession(w http.ResponseWriter, req *http.Request) {
	resp := map[string]any{
		"logged_in":    false,
		"console_auth": r.cfg.ConsolePassword != "",
	}
	if sess, ok := r.currentSession(req); ok {
		resp["logged_in"] = true
		resp["username"] = sess.Username
		resp["expires_at"] = sess.ExpiresAt.Unix()
	}
	writeJSON(w, http.StatusOK, resp)
}

// currentSession 从请求 cookie 读 session(滑动续期)。
func (r *Relay) currentSession(req *http.Request) (*Session, bool) {
	if r.sessions == nil {
		return nil, false
	}
	cookie, err := req.Cookie(r.cookieName())
	if err != nil {
		return nil, false
	}
	return r.sessions.Get(cookie.Value)
}

// handleConsoleLogin POST /api/console/login —— 密码校验 + 颁发 session cookie。
func (r *Relay) handleConsoleLogin(w http.ResponseWriter, req *http.Request) {
	if r.cfg.ConsolePassword == "" || r.sessions == nil {
		writeJSON(w, http.StatusNotFound, errMap(errConsoleAuthDisabled))
		return
	}
	var body consoleLoginReq
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, errMap(errConsoleBadBody))
		return
	}
	username := body.Username
	if username == "" {
		username = "admin"
	}
	// 常量时间比较密码(防计时侧信道)
	ok := subtle.ConstantTimeCompare([]byte(body.Password), []byte(r.cfg.ConsolePassword)) == 1
	if !ok {
		if r.audit != nil {
			r.audit.Log(AuditEntry{Event: "console_login_failed", Operator: username, Detail: "ip=" + clientIP(req)})
		}
		writeJSON(w, http.StatusUnauthorized, errMap(errConsoleBadCreds))
		return
	}
	sess, err := r.sessions.Create(username)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errMap(err))
		return
	}
	r.setSessionCookie(w, req, sess.ID, int(r.sessions.TTL().Seconds()))
	if r.audit != nil {
		r.audit.Log(AuditEntry{Event: "console_login_success", Operator: username, Detail: "ip=" + clientIP(req)})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"logged_in":  true,
		"username":   sess.Username,
		"expires_at": sess.ExpiresAt.Unix(),
	})
}

// handleConsoleLogout POST /api/console/logout —— 登出(清 session + 清 cookie)。幂等。
func (r *Relay) handleConsoleLogout(w http.ResponseWriter, req *http.Request) {
	if sess, ok := r.currentSession(req); ok {
		r.sessions.Delete(sess.ID)
		if r.audit != nil {
			r.audit.Log(AuditEntry{Event: "console_logout", Operator: sess.Username})
		}
	}
	r.clearSessionCookie(w, req)
	writeJSON(w, http.StatusOK, map[string]any{"logged_out": true})
}

// setSessionCookie 写入会话 cookie(HttpOnly + Secure 自适应 + SameSite=Lax + Path=/)。
func (r *Relay) setSessionCookie(w http.ResponseWriter, req *http.Request, sid string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     r.cookieName(),
		Value:    sid,
		Path:     "/",
		MaxAge:   maxAge,
		Expires:  time.Now().Add(time.Duration(maxAge) * time.Second),
		HttpOnly: true,
		Secure:   r.shouldSecureCookie(req),
		SameSite: http.SameSiteLaxMode,
	})
}

// clearSessionCookie 清除会话 cookie(同名覆盖,立即过期)。
func (r *Relay) clearSessionCookie(w http.ResponseWriter, req *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     r.cookieName(),
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
		HttpOnly: true,
		Secure:   r.shouldSecureCookie(req),
		SameSite: http.SameSiteLaxMode,
	})
}

// shouldSecureCookie 判断是否设 Secure 标志。
//
// 反代 TLS 终端场景:Nginx↔Relay 是 HTTP,但浏览器↔Nginx 是 HTTPS,
// 通过 X-Forwarded-Proto: https 识别;直连 HTTPS 用 req.TLS!=nil。
// cfg.ConsoleSecureCookie 可强制开启(纯 HTTP 调试时不应开)。
func (r *Relay) shouldSecureCookie(req *http.Request) bool {
	if r.cfg.ConsoleSecureCookie {
		return true
	}
	if req.TLS != nil {
		return true
	}
	for _, p := range req.Header.Values("X-Forwarded-Proto") {
		if p == "https" {
			return true
		}
	}
	return false
}

// clientIP 从请求提取客户端 IP(X-Forwarded-For 优先,否则 RemoteAddr)。
// 仅用于审计日志记录,不用于鉴权决策。
func clientIP(req *http.Request) string {
	if xff := req.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	host := req.RemoteAddr
	if i := strings.LastIndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	return host
}

// 控制台鉴权相关错误(集中定义,便于测试断言)。
var (
	errConsoleAuthDisabled = errors.New("控制台鉴权未启用(需配置 -console-password)")
	errConsoleBadBody      = errors.New("请求体解析失败")
	errConsoleBadCreds     = errors.New("密码错误")
)
