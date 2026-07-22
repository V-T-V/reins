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
