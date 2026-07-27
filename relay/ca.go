package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
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
//
// D3 修复 P1-3：CRL（revoked/issued）持久化到 data/ca-state.json，
// Relay 重启后撤销状态不丢失。
type CA struct {
	mu       sync.Mutex
	rootCert *x509.Certificate
	rootKey  *ecdsa.PrivateKey
	caPEM    []byte
	certPath string // 根证书落盘路径
	keyPath  string // 根私钥落盘路径
	statePath string // CRL 状态落盘路径（ca-state.json）

	// 注册令牌：token → 绑定信息（agentID + 创建时间，一次性，签发后删除，不持久化）
	// E3/W4：加 createdAt 用于过期校验，对齐前端返回的 expires_in:3600
	tokens map[string]tokenEntry
	// 已签发证书:agentID → 历史序列号列表(P1-S6:支持撤销历史证书)
	// 持久化到 ca-state.json
	issued map[string][]string
	// 已撤销序列号(持久化)
	revoked map[string]bool
}

// tokenEntry 注册令牌绑定信息。
type tokenEntry struct {
	agentID   string
	createdAt time.Time
}

// EnrollmentTokenTTL 注册令牌有效期（E3：对齐前端 expires_in:3600）。
const EnrollmentTokenTTL = time.Hour

// LoadOrCreateCA 加载现有 CA，不存在则自签创建。
func LoadOrCreateCA(dataDir string) (*CA, error) {
	ca := &CA{
		certPath:  filepath.Join(dataDir, "ca-root.crt"),
		keyPath:   filepath.Join(dataDir, "ca-root.key"),
		statePath: filepath.Join(dataDir, "ca-state.json"),
		tokens:    make(map[string]tokenEntry),
		issued:    make(map[string][]string),
		revoked:   make(map[string]bool),
	}

	// 尝试加载现有 CA
	if err := ca.load(); err == nil {
		// 加载 CRL 状态（即使根证书加载成功，状态文件可能不存在——不致命）
		_ = ca.loadState()
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
// E3：记录 createdAt，校验时用于过期判断。
func (c *CA) GenerateEnrollmentToken(agentID string) (string, error) {
	tokBytes := make([]byte, 32)
	if _, err := rand.Read(tokBytes); err != nil {
		return "", err
	}
	token := fmt.Sprintf("%x", tokBytes)
	c.mu.Lock()
	c.tokens[token] = tokenEntry{agentID: agentID, createdAt: time.Now()}
	c.mu.Unlock()
	return token, nil
}

// ValidateEnrollmentToken 校验令牌是否绑定到指定 agentID 且未过期。
// 校验成功后令牌作废（一次性）。
// E3/W4：加过期校验，对齐前端 expires_in:3600。
func (c *CA) ValidateEnrollmentToken(token, agentID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.tokens[token]
	if !ok || entry.agentID != agentID {
		return false
	}
	// 过期校验
	if time.Since(entry.createdAt) > EnrollmentTokenTTL {
		delete(c.tokens, token) // 清理过期令牌
		return false
	}
	delete(c.tokens, token) // 一次性
	return true
}

// SignAgentCSR 解析 Agent 提交的 CSR,用根 CA 签发短期客户端证书。
// 返回签发后的证书 PEM。
//
// P1-S8 修复:强制覆盖 CSR 的 Subject.CommonName 为传入的 agentID,
// 而非信任 CSR 自报的 CN。这样即使攻击者拿到 token-A,也无法在 CSR 里
// 伪造 CN=agent-B 来冒名注册。
//
// P1-S6 修复:重新 enroll 前撤销该 agentID 的所有旧证书(把旧 serial 加入 revoked)。
// 这样攻击者窃取旧 token 完成 enroll 后,管理员重新 enroll 合法主机时,
// 攻击者的旧证书会被立即作废。
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
	// P1-S8:强制覆盖 Subject 和 DNSNames,忽略 CSR 自报值
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   agentID, // 强制 CN = enroll 请求的 agentID
			Organization: []string{"go-rmm"},
		},
		NotBefore:   time.Now().Add(-time.Hour),
		NotAfter:    time.Now().Add(ttl),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		DNSNames:    []string{agentID}, // 强制 SAN = agentID
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	// P1-S6:重新 enroll 时撤销该 agentID 的所有旧证书
	// (旧实现只存最新 serial,旧证书 24h 内仍可用且无法 revoke)
	for _, oldSerial := range c.issued[agentID] {
		c.revoked[oldSerial] = true
	}

	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, c.rootCert, csr.PublicKey, c.rootKey)
	if err != nil {
		return nil, fmt.Errorf("签发证书失败: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	// P1-S6:issued 改为 map[string][]string(多序列号),支持撤销历史证书
	c.issued[agentID] = append(c.issued[agentID], serial.String())
	c.saveState() // D3:持久化签发记录
	return certPEM, nil
}

// RevokeAgent 撤销指定 Agent 的所有证书(含历史签发的)。
//
// P1-S6 修复:旧实现只撤销最新证书,旧证书在有效期内仍可用。
// 现在遍历 issued[agentID] 的所有历史 serial 全部加入 revoked。
func (c *CA) RevokeAgent(agentID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, serial := range c.issued[agentID] {
		c.revoked[serial] = true
	}
	delete(c.issued, agentID)
	c.saveState() // D3:CRL 持久化,重启不丢
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

// caState 持久化的 CRL 状态(JSON)。
// P1-S6:Issued 改为 map[string][]string 支持多历史证书。
type caState struct {
	Issued  map[string][]string `json:"issued"`  // agentID → serial hex 列表(历史所有签发)
	Revoked []string            `json:"revoked"` // serial hex 列表
}

// loadState 从 statePath 加载 issued/revoked。文件不存在视为空状态(不报错)。
//
// 兼容旧格式:旧 caState.Issued 是 map[string]string(单 serial),
// 新格式是 map[string][]string。用 json.RawMessage 兼容两种。
func (c *CA) loadState() error {
	data, err := os.ReadFile(c.statePath)
	if err != nil {
		return nil // 文件不存在不报错
	}
	// 兼容旧格式:先尝试解析为新格式,失败则尝试旧格式
	var newSt struct {
		Issued  map[string][]string `json:"issued"`
		Revoked []string            `json:"revoked"`
	}
	if err := json.Unmarshal(data, &newSt); err == nil && newSt.Issued != nil {
		for agentID, serials := range newSt.Issued {
			c.issued[agentID] = serials
		}
		for _, serialHex := range newSt.Revoked {
			c.revoked[serialHex] = true
		}
		return nil
	}
	// 旧格式:map[string]string
	var oldSt struct {
		Issued  map[string]string `json:"issued"`
		Revoked []string          `json:"revoked"`
	}
	if err := json.Unmarshal(data, &oldSt); err != nil {
		return fmt.Errorf("CA 状态文件解析失败: %w", err)
	}
	for agentID, serialHex := range oldSt.Issued {
		c.issued[agentID] = []string{serialHex}
	}
	for _, serialHex := range oldSt.Revoked {
		c.revoked[serialHex] = true
	}
	return nil
}

// saveState 把 issued/revoked 持久化到 statePath。
func (c *CA) saveState() {
	st := caState{
		Issued:  make(map[string][]string, len(c.issued)),
		Revoked: make([]string, 0, len(c.revoked)),
	}
	for aid, serials := range c.issued {
		st.Issued[aid] = serials
	}
	for serial := range c.revoked {
		st.Revoked = append(st.Revoked, serial)
	}
	data, err := json.Marshal(st)
	if err != nil {
		return
	}
	_ = os.WriteFile(c.statePath, data, 0600)
}
