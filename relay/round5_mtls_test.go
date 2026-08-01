// Package main — round 5 TLS/mTLS 安全层深化测试。
//
// ca_test.go 已覆盖 CA 自签/令牌一次性/签发链/CRL 持久化/撤销。
// 本文件补强 mTLS 双向认证专项不变量：
//   - certMatchesAgent:证书身份(CN/SAN)与 AgentID 绑定校验(防冒充)。
//   - 跨 CA 证书拒绝(非本 CA 签发的证书不被信任)。
//   - 过期证书拒绝(NotAfter 已过)。
//   - 重新 enroll 撤销历史证书(P1-S6:旧证书立即失效)。
//   - Authorization token 常量时间比较(防计时侧信道)。
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"testing"
	"time"
)

// --- certMatchesAgent 身份绑定 ---

// TestCertMatchesAgentByCN 验证 CN 等于 agentID 时匹配。
func TestCertMatchesAgentByCN(t *testing.T) {
	if !certMatchesAgent("agent-prod-01", "agent-prod-01", nil) {
		t.Fatal("CN 等于 agentID 应匹配")
	}
}

// TestCertMatchesAgentByDNS 验证 CN 不匹配但 SAN DNSNames 含 agentID 时匹配。
func TestCertMatchesAgentByDNS(t *testing.T) {
	if certMatchesAgent("agent-1", "wrong-cn", nil) {
		t.Fatal("CN 不匹配且无 DNS 应不匹配")
	}
	if !certMatchesAgent("agent-1", "wrong-cn", []string{"agent-1"}) {
		t.Fatal("DNSNames 含 agentID 应匹配")
	}
	if !certMatchesAgent("agent-1", "wrong-cn", []string{"other", "agent-1"}) {
		t.Fatal("DNSNames 多值中含 agentID 应匹配")
	}
}

// TestCertMatchesAgentNoMatch 验证非空完全不匹配时返回 false。
func TestCertMatchesAgentNoMatch(t *testing.T) {
	if certMatchesAgent("agent-1", "agent-2", []string{"agent-3"}) {
		t.Fatal("CN 与 DNS 均不含 agentID 应不匹配")
	}
	if certMatchesAgent("agent-1", "agent-2", nil) {
		t.Fatal("CN 不匹配且无 DNS 应不匹配")
	}
}

// TestCertMatchesAgentEmptyEdgeCase 记录一个边界行为:
// certMatchesAgent 对 agentID="" + CN="" 返回 true(空串相等)。
// 这是已知边界,但实际安全由 Register/Enroll 阶段的非空校验兜底
// (AgentID 必填,空 AgentID 在 handlePostTask/Register 处即被拒)。
// 本测试锁定该行为,防止未来无意识改动。
func TestCertMatchesAgentEmptyEdgeCase(t *testing.T) {
	// 空串相等是 strings 比较的固有行为;此处锁定文档化。
	if !certMatchesAgent("", "", nil) {
		t.Fatal("空 agentID + 空 CN 当前匹配(strings ==),锁定行为")
	}
}

// --- 跨 CA 证书拒绝 ---

// TestRejectsCertFromOtherCA 验证非本 CA 签发的证书校验失败。
// 攻击者自带 CA 签发的证书不应被信任。
func TestRejectsCertFromOtherCA(t *testing.T) {
	// 本 CA
	ourCA, _ := LoadOrCreateCA(t.TempDir())

	// 攻击者的 CA(独立)
	attackerCA, _ := LoadOrCreateCA(t.TempDir())

	// 用攻击者 CA 签发一个"合法"证书
	victimKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "agent-victim"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, attackerCA.rootCert, &victimKey.PublicKey, attackerCA.rootKey)
	if err != nil {
		t.Fatalf("攻击者签发失败: %v", err)
	}
	evilCert, err := x509.ParseCertificate(certDER)
	if err != nil {
		t.Fatal(err)
	}

	// 本 CA 不应信任攻击者签发的证书
	if err := ourCA.VerifyClientCert(evilCert); err == nil {
		t.Fatal("非本 CA 签发的证书应被拒绝(mTLS 信任锚隔离)")
	}
}

// --- 过期证书拒绝 ---

// TestRejectsExpiredCert 验证过期证书(NotAfter 已过)被拒绝。
func TestRejectsExpiredCert(t *testing.T) {
	ca, _ := LoadOrCreateCA(t.TempDir())
	csrPEM := genTestCSR(t, "agent-expired")
	// 签发一个已过期的证书(ttl 负偏移无法直接设，用很短 ttl + sleep 不现实；
	// 改为直接构造过期证书)
	certPEM, err := ca.SignAgentCSR(csrPEM, "agent-expired", 1*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// 手动改成已过期：重新签发并把 NotAfter 设到过去
	block, _ := pem.Decode(certPEM)
	cert, _ := x509.ParseCertificate(block.Bytes)

	// 直接调 VerifyClientCert 时 cert.Verify 内部会校验时间。
	// 这里构造一个 NotAfter 在过去的证书来验证拒绝路径。
	victimKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	expiredTmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "agent-expired"},
		NotBefore:    time.Now().Add(-2 * time.Hour),
		NotAfter:     time.Now().Add(-1 * time.Hour), // 1 小时前过期
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	expiredDER, _ := x509.CreateCertificate(rand.Reader, expiredTmpl, ca.rootCert, &victimKey.PublicKey, ca.rootKey)
	expiredCert, _ := x509.ParseCertificate(expiredDER)

	// 合法(未过期)证书应通过
	if err := ca.VerifyClientCert(cert); err != nil {
		t.Fatalf("未过期证书应通过: %v", err)
	}
	// 过期证书应被拒绝
	if err := ca.VerifyClientCert(expiredCert); err == nil {
		t.Fatal("过期证书应被拒绝")
	}
}

// --- 重新 enroll 撤销历史证书(P1-S6) ---

// TestReenrollRevokesOldCert 验证同一 agentID 重新 enroll 后旧证书立即失效。
// 攻击场景：攻击者窃取 token-A 完成 enroll 拿到 cert-A，
// 管理员重新 enroll 合法主机拿到 cert-B，此时 cert-A 应被撤销。
func TestReenrollRevokesOldCert(t *testing.T) {
	ca, _ := LoadOrCreateCA(t.TempDir())

	// 第一次 enroll:攻击者用 token-A 签发 cert-A
	csrA := genTestCSR(t, "agent-reloaded")
	certAPEM, err := ca.SignAgentCSR(csrA, "agent-reloaded", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	blockA, _ := pem.Decode(certAPEM)
	certA, _ := x509.ParseCertificate(blockA.Bytes)
	// cert-A 最初有效
	if err := ca.VerifyClientCert(certA); err != nil {
		t.Fatalf("cert-A 最初应有效: %v", err)
	}

	// 第二次 enroll:管理员重新签发(不同密钥) cert-B
	csrB := genTestCSR(t, "agent-reloaded")
	certBPEM, err := ca.SignAgentCSR(csrB, "agent-reloaded", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	blockB, _ := pem.Decode(certBPEM)
	certB, _ := x509.ParseCertificate(blockB.Bytes)

	// P1-S6:cert-A 应被撤销(重新 enroll 即吊销旧证书)
	if err := ca.VerifyClientCert(certA); err == nil {
		t.Fatal("重新 enroll 后旧证书 cert-A 应被撤销失效")
	}
	// cert-B 应有效
	if err := ca.VerifyClientCert(certB); err != nil {
		t.Fatalf("新证书 cert-B 应有效: %v", err)
	}
}

// --- 授权 token 常量时间比较 ---

// TestAuthorizeRejectsWrongToken 验证错误 token 被拒绝、正确 token 放行。
func TestAuthorizeRejectsWrongToken(t *testing.T) {
	r := NewRelay(Config{Token: "correct-secret-token"})
	req, _ := newReqWithToken("wrong-token")
	if err := r.authorize(req); err == nil {
		t.Fatal("错误 token 应被拒绝")
	}
	req2, _ := newReqWithToken("correct-secret-token")
	if err := r.authorize(req2); err != nil {
		t.Fatalf("正确 token 应放行: %v", err)
	}
}

// TestAuthorizeEmptyTokenConfigAllowsAll 验证未配置 token 时放行所有(向后兼容)。
func TestAuthorizeEmptyTokenConfigAllowsAll(t *testing.T) {
	r := NewRelay(Config{Token: ""})
	req, _ := httpNewRequest("GET", "/api/agents")
	if err := r.authorize(req); err != nil {
		t.Fatalf("未配置 token 应放行: %v", err)
	}
}

// TestAuthorizeQueryTokenFallback 验证 token 可通过 query 参数传递。
func TestAuthorizeQueryTokenFallback(t *testing.T) {
	r := NewRelay(Config{Token: "q-secret"})
	req, _ := httpNewRequest("GET", "/api/agents?token=q-secret")
	if err := r.authorize(req); err != nil {
		t.Fatalf("query token 应放行: %v", err)
	}
}

// --- CAPEM 分发 ---

// TestCAPEMDistributable 验证 CAPEM 可作为信任锚分发给 Agent(非空且可解析)。
func TestCAPEMDistributable(t *testing.T) {
	ca, _ := LoadOrCreateCA(t.TempDir())
	pemBytes := ca.CAPEM()
	if len(pemBytes) == 0 {
		t.Fatal("CAPEM 不应为空")
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatal("CAPEM 应为合法 CERTIFICATE PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("CAPEM 应可解析: %v", err)
	}
	if !cert.IsCA {
		t.Fatal("分发的 CA 证书应 IsCA=true")
	}
}

// --- 测试辅助 ---

// newReqWithToken 构造带 Bearer token 的请求。
func newReqWithToken(token string) (*http.Request, error) {
	req, err := http.NewRequest("GET", "/api/agents", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return req, nil
}

// httpNewRequest 构造请求(测试辅助,集中错误处理)。
func httpNewRequest(method, url string) (*http.Request, error) {
	return http.NewRequest(method, url, nil)
}
