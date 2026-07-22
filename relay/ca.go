package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// CA 是 Relay 内置的证书颁发机构。
//
// 修复 S1(P0)：原实现 Agent 自报 agent-id 即可注册，任何人都能冒充任意 Agent。
// 现在引入 PKI：
//  1. Relay 首次启动自签根 CA（ECDSA P-256，10 年）
//  2. 管理员为每台主机生成一次性 enrollment_token
//  3. Agent 用 token + CSR 换取短期客户端证书（24h，自动续签）
//  4. mTLS 握手时校验证书（C8 实现）
//
// 根 CA 私钥落盘 data/ca-root.key（权限 0600），签发的 Agent 证书序列号记录在
// revoked 列表（CRL）以支持撤销。
type CA struct {
	mu       sync.Mutex
	rootCert *x509.Certificate
	rootKey  *ecdsa.PrivateKey
	caPEM    []byte
	certPath string // 根证书落盘路径
	keyPath  string // 根私钥落盘路径

	// 注册令牌：token → 绑定的 agentID（一次性，签发后删除）
	tokens map[string]string
	// 已签发证书：agentID → 序列号（用于撤销/查询）
	issued map[string]*big.Int
	// 已撤销序列号
	revoked map[string]bool
}

// LoadOrCreateCA 加载现有 CA，不存在则自签创建。
func LoadOrCreateCA(dataDir string) (*CA, error) {
	ca := &CA{
		certPath: filepath.Join(dataDir, "ca-root.crt"),
		keyPath:  filepath.Join(dataDir, "ca-root.key"),
		tokens:   make(map[string]string),
		issued:   make(map[string]*big.Int),
		revoked:  make(map[string]bool),
	}

	// 尝试加载现有 CA
	if err := ca.load(); err == nil {
		return ca, nil
	}

	// 不存在则创建
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return nil, err
	}
	if err := ca.createRoot(); err != nil {
		return nil, err
	}
	return ca, nil
}

// load 从磁盘加载根 CA。
func (c *CA) load() error {
	certPEM, err := os.ReadFile(c.certPath)
	if err != nil {
		return err
	}
	keyPEM, err := os.ReadFile(c.keyPath)
	if err != nil {
		return err
	}
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil {
		return errors.New("CA 证书 PEM 解码失败")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return fmt.Errorf("CA 证书解析失败: %w", err)
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return errors.New("CA 私钥 PEM 解码失败")
	}
	key, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		return fmt.Errorf("CA 私钥解析失败: %w", err)
	}
	ecdsaKey, ok := key.(*ecdsa.PrivateKey)
	if !ok {
		return errors.New("CA 私钥非 ECDSA")
	}
	c.rootCert = cert
	c.rootKey = ecdsaKey
	c.caPEM = certPEM
	return nil
}

// createRoot 自签根 CA 并落盘。
func (c *CA) createRoot() error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("生成 CA 密钥失败: %w", err)
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			Organization: []string{"go-rmm"},
			CommonName:   "go-rmm-root-ca",
		},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0), // 10 年
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            1,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return fmt.Errorf("自签 CA 失败: %w", err)
	}
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	// 落盘（私钥 0600）
	if err := os.WriteFile(c.certPath, certPEM, 0644); err != nil {
		return err
	}
	if err := os.WriteFile(c.keyPath, keyPEM, 0600); err != nil {
		return err
	}
	c.rootCert = cert
	c.rootKey = key
	c.caPEM = certPEM
	return nil
}

// CAPEM 返回根 CA 证书的 PEM（发给 Agent 用于校验 Relay）。
func (c *CA) CAPEM() []byte { return c.caPEM }

// GenerateEnrollmentToken 为指定 agentID 生成一次性注册令牌。
// 同一 agentID 重复生成会覆盖旧令牌。
func (c *CA) GenerateEnrollmentToken(agentID string) (string, error) {
	tokBytes := make([]byte, 32)
	if _, err := rand.Read(tokBytes); err != nil {
		return "", err
	}
	token := fmt.Sprintf("%x", tokBytes)
	c.mu.Lock()
	c.tokens[token] = agentID
	c.mu.Unlock()
	return token, nil
}

// ValidateEnrollmentToken 校验令牌是否绑定到指定 agentID。校验成功后令牌作废（一次性）。
func (c *CA) ValidateEnrollmentToken(token, agentID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	bound, ok := c.tokens[token]
	if !ok || bound != agentID {
		return false
	}
	delete(c.tokens, token) // 一次性
	return true
}

// SignAgentCert 解析 Agent 提交的 CSR，用根 CA 签发短期客户端证书。
// 返回签发后的证书 PEM。
func (c *CA) SignAgentCSR(csrPEM []byte, agentID string, ttl time.Duration) ([]byte, error) {
	block, _ := pem.Decode(csrPEM)
	if block == nil {
		return nil, errors.New("CSR PEM 解码失败")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("CSR 解析失败: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("CSR 签名无效: %w", err)
	}

	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      csr.Subject,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(ttl),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		// 用 agentID 作为证书的 URI/SAN 项便于校验身份
		DNSNames: []string{agentID},
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, c.rootCert, csr.PublicKey, c.rootKey)
	if err != nil {
		return nil, fmt.Errorf("签发证书失败: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	c.issued[agentID] = serial
	return certPEM, nil
}

// RevokeAgent 撤销指定 Agent 的证书（加入 CRL）。
func (c *CA) RevokeAgent(agentID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if serial, ok := c.issued[agentID]; ok {
		c.revoked[serial.String()] = true
		delete(c.issued, agentID)
	}
}

// IsRevoked 检查证书序列号是否已撤销。
func (c *CA) IsRevoked(serial *big.Int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.revoked[serial.String()]
}

// VerifyClientCert 校验客户端证书是否由本 CA 签发且未撤销。
func (c *CA) VerifyClientCert(cert *x509.Certificate) error {
	roots := x509.NewCertPool()
	roots.AddCert(c.rootCert)
	_, err := cert.Verify(x509.VerifyOptions{
		Roots:   roots,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
	if err != nil {
		return fmt.Errorf("证书校验失败: %w", err)
	}
	if c.IsRevoked(cert.SerialNumber) {
		return errors.New("证书已撤销")
	}
	return nil
}

// readFileOS os.ReadFile 的薄封装（测试辅助）。
func readFileOS(path string) ([]byte, error) { return os.ReadFile(path) }
