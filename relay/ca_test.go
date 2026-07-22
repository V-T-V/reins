package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"path/filepath"
	"testing"
	"time"
)

// TestCACreateAndPersist 验证 CA 自签根 + 持久化 + 重新加载。
func TestCACreateAndPersist(t *testing.T) {
	dir := t.TempDir()

	// 创建
	ca1, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatalf("创建 CA 失败: %v", err)
	}
	if ca1.rootCert == nil || ca1.rootKey == nil {
		t.Fatal("根证书/密钥不应为空")
	}
	// 验证是 CA 证书
	if !ca1.rootCert.IsCA {
		t.Fatal("应为 CA 证书")
	}
	if !ca1.rootCert.BasicConstraintsValid {
		t.Fatal("BasicConstraints 应有效")
	}

	// 落盘文件应存在
	if _, err := readFile(filepath.Join(dir, "ca-root.crt")); err != nil {
		t.Fatalf("根证书应落盘: %v", err)
	}

	// 重新加载（应拿到同一根证书）
	ca2, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatalf("重新加载失败: %v", err)
	}
	if ca1.rootCert.SerialNumber.Cmp(ca2.rootCert.SerialNumber) != 0 {
		t.Fatal("重新加载应得到同一根证书（序列号相同）")
	}
}

// TestCAEnrollmentTokenOneTime 验证注册令牌一次性。
func TestCAEnrollmentTokenOneTime(t *testing.T) {
	ca, _ := LoadOrCreateCA(t.TempDir())

	token, err := ca.GenerateEnrollmentToken("agent-1")
	if err != nil {
		t.Fatal(err)
	}

	// 第一次校验成功
	if !ca.ValidateEnrollmentToken(token, "agent-1") {
		t.Fatal("令牌应校验成功")
	}
	// 第二次应失败（已使用）
	if ca.ValidateEnrollmentToken(token, "agent-1") {
		t.Fatal("令牌应一次性，第二次应失败")
	}
	// 错误的 agentID 应失败
	token2, _ := ca.GenerateEnrollmentToken("agent-2")
	if ca.ValidateEnrollmentToken(token2, "agent-1") {
		t.Fatal("令牌绑定 agent-2，校验 agent-1 应失败")
	}
}

// TestCASignAndVerifyCSR 验证完整签发链：生成 CSR → CA 签发 → 校验证书。
func TestCASignAndVerifyCSR(t *testing.T) {
	ca, _ := LoadOrCreateCA(t.TempDir())

	// Agent 侧：生成密钥 + CSR
	agentKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	csrTmpl := &x509.CertificateRequest{
		Subject:  pkix.Name{CommonName: "agent-test"},
		DNSNames: []string{"agent-test"},
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, csrTmpl, agentKey)
	if err != nil {
		t.Fatalf("创建 CSR 失败: %v", err)
	}
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})

	// CA 签发
	certPEM, err := ca.SignAgentCSR(csrPEM, "agent-test", 1*time.Hour)
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}

	// 解析签发的证书
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("签发证书 PEM 解码失败")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("签发证书解析失败: %v", err)
	}

	// 校验：应通过
	if err := ca.VerifyClientCert(cert); err != nil {
		t.Fatalf("证书校验应通过: %v", err)
	}

	// 撤销后再校验应失败
	ca.RevokeAgent("agent-test")
	if err := ca.VerifyClientCert(cert); err == nil {
		t.Fatal("撤销后校验应失败")
	}
}

// readFile 测试辅助（避免引入 os/ioutil）。
func readFile(path string) ([]byte, error) {
	return readFileOS(path)
}

// TestCRLPersistsAcrossRestart 验证 P1-3 修复：撤销状态跨 Relay 重启保留。
func TestCRLPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()

	// 第一次：创建 CA，签发证书，撤销
	ca1, _ := LoadOrCreateCA(dir)
	csrPEM := genTestCSR(t, "agent-revoked")
	certPEM, err := ca1.SignAgentCSR(csrPEM, "agent-revoked", time.Hour)
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	ca1.RevokeAgent("agent-revoked")

	// 验证撤销生效
	block, _ := pem.Decode(certPEM)
	cert, _ := x509.ParseCertificate(block.Bytes)
	if err := ca1.VerifyClientCert(cert); err == nil {
		t.Fatal("撤销后应校验失败")
	}

	// 第二次：重新加载 CA（模拟 Relay 重启）
	ca2, _ := LoadOrCreateCA(dir)

	// P1-3 验证：撤销状态应保留
	if err := ca2.VerifyClientCert(cert); err == nil {
		t.Fatal("重启后撤销状态应保留，证书校验应失败")
	}
}

// genTestCSR 生成测试用 CSR PEM。
func genTestCSR(t *testing.T, agentID string) []byte {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	csrTmpl := &x509.CertificateRequest{
		Subject:  pkix.Name{CommonName: agentID},
		DNSNames: []string{agentID},
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, csrTmpl, key)
	if err != nil {
		t.Fatalf("创建 CSR 失败: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})
}

// TestEnrollmentTokenExpires 验证 E3/W4：注册令牌过期后失效。
func TestEnrollmentTokenExpires(t *testing.T) {
	ca, _ := LoadOrCreateCA(t.TempDir())

	token, _ := ca.GenerateEnrollmentToken("agent-expire")

	// 立即校验应成功
	if !ca.ValidateEnrollmentToken(token, "agent-expire") {
		t.Fatal("新生成的令牌应有效")
	}

	// 再生成一个，手动改成 2 小时前（模拟过期）
	token2, _ := ca.GenerateEnrollmentToken("agent-expire2")
	ca.mu.Lock()
	e := ca.tokens[token2]
	e.createdAt = time.Now().Add(-2 * time.Hour) // 2h 前，超过 1h TTL
	ca.tokens[token2] = e
	ca.mu.Unlock()

	// 过期令牌应校验失败
	if ca.ValidateEnrollmentToken(token2, "agent-expire2") {
		t.Fatal("过期的令牌应校验失败")
	}
}
