package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/QiuShichang/reins/proto"
)

// AgentCerts 管理 Agent 的 mTLS 客户端证书。
//
// C8：Agent 首次启动若无证书，用 enrollment token + CSR 向 Relay 换取证书；
// 证书临近过期自动续签。证书落盘 data/agent.crt + data/agent.key。
type AgentCerts struct {
	relayURL string // Relay base URL（http(s)://host:port，不含路径）
	dataDir  string
	certPath string
	keyPath  string
	caPath   string

	cert   *tls.Certificate
	caPool *x509.CertPool
}

// NewAgentCerts 构造证书管理器。
func NewAgentCerts(relayURL, dataDir string) *AgentCerts {
	return &AgentCerts{
		relayURL: relayURL,
		dataDir:  dataDir,
		certPath: filepath.Join(dataDir, "agent.crt"),
		keyPath:  filepath.Join(dataDir, "agent.key"),
		caPath:   filepath.Join(dataDir, "ca.crt"),
	}
}

// LoadOrEnroll 加载已有证书，不存在则用 token enroll。
// 返回可用的 tls.Certificate 与 CA pool。
func (a *AgentCerts) LoadOrEnroll(ctx context.Context, agentID, token string) error {
	_ = os.MkdirAll(a.dataDir, 0755)

	// 尝试加载本地证书
	if err := a.load(); err == nil {
		// 已有证书，检查是否需要续签
		if a.needRenew() {
			log.Printf("[enroll] 证书即将过期，尝试续签")
			if err := a.enroll(ctx, agentID, token); err != nil {
				log.Printf("[enroll] 续签失败(用旧证书继续): %v", err)
			}
		}
		return nil
	}

	// 无证书，首次 enroll
	log.Printf("[enroll] 首次注册，用 token 换取证书")
	return a.enroll(ctx, agentID, token)
}

// load 从磁盘加载证书与 CA。
func (a *AgentCerts) load() error {
	cert, err := tls.LoadX509KeyPair(a.certPath, a.keyPath)
	if err != nil {
		return err
	}
	caPEM, err := os.ReadFile(a.caPath)
	if err != nil {
		return err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return fmt.Errorf("CA 证书解析失败")
	}
	a.cert = &cert
	a.caPool = pool
	return nil
}

// needRenew 检查证书是否在 2 小时内过期。
func (a *AgentCerts) needRenew() bool {
	if a.cert == nil || len(a.cert.Certificate) == 0 {
		return true
	}
	leaf, err := x509.ParseCertificate(a.cert.Certificate[0])
	if err != nil {
		return true
	}
	return time.Until(leaf.NotAfter) < 2*time.Hour
}

// enroll 生成密钥+CSR，向 Relay 换取证书并落盘。
func (a *AgentCerts) enroll(ctx context.Context, agentID, token string) error {
	// 1. 生成 ECDSA 密钥
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("生成密钥失败: %w", err)
	}

	// 2. 构造 CSR
	csrTmpl := &x509.CertificateRequest{
		Subject:  pkix.Name{CommonName: agentID},
		DNSNames: []string{agentID},
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, csrTmpl, key)
	if err != nil {
		return fmt.Errorf("创建 CSR 失败: %w", err)
	}
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})

	// 3. POST /api/enroll
	// A3 修复：relayURL 是 WS 端点（wss://host:port/agent），需转成 HTTP（https://host:port/api/enroll）
	enrollURL := wsToHTTP(a.relayURL) + "/api/enroll"
	enroll := proto.Enroll{AgentID: agentID, Token: token, CSRPEM: string(csrPEM)}
	body, _ := json.Marshal(enroll)
	req, _ := http.NewRequestWithContext(ctx, "POST", enrollURL, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("enroll 请求失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("enroll HTTP %d", resp.StatusCode)
	}

	var respBody proto.EnrollResp
	if err := json.NewDecoder(resp.Body).Decode(&respBody); err != nil {
		return fmt.Errorf("enroll 响应解析失败: %w", err)
	}
	if !respBody.Success {
		return fmt.Errorf("enroll 失败: %s", respBody.Error)
	}

	// 4. 落盘证书 + CA
	if err := os.WriteFile(a.certPath, []byte(respBody.CertPEM), 0644); err != nil {
		return err
	}
	keyDER, _ := x509.MarshalPKCS8PrivateKey(key)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(a.keyPath, keyPEM, 0600); err != nil {
		return err
	}
	if err := os.WriteFile(a.caPath, []byte(respBody.CAPEM), 0644); err != nil {
		return err
	}

	// 5. 加载到内存
	cert, err := tls.X509KeyPair([]byte(respBody.CertPEM), keyPEM)
	if err != nil {
		return err
	}
	a.cert = &cert
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM([]byte(respBody.CAPEM))
	a.caPool = pool

	log.Printf("[enroll] 证书获取成功，有效期至 %s",
		time.Unix(respBody.ExpiresAt, 0).Format("2006-01-02 15:04:05"))
	return nil
}

// TLSConfig 返回用于 mTLS 连接的 TLS 配置。
func (a *AgentCerts) TLSConfig() *tls.Config {
	if a.cert == nil {
		return nil
	}
	return &tls.Config{
		Certificates: []tls.Certificate{*a.cert},
		RootCAs:      a.caPool,
		ServerName:   "reins-relay", // Relay 证书的 CN/SAN
		MinVersion:   tls.VersionTLS12,
	}
}

// wsToHTTP 把 WS 端点 URL 转成 HTTP base URL（剥掉 /agent 路径，换 scheme）。
// "wss://host:7780/agent" → "https://host:7780"
// "ws://host:7780/agent"  → "http://host:7780"
func wsToHTTP(wsURL string) string {
	u := wsURL
	u = strings.Replace(u, "wss://", "https://", 1)
	u = strings.Replace(u, "ws://", "http://", 1)
	// 剥掉 /agent 后缀（enroll 是 HTTP REST，不在 WS 路径）
	u = strings.TrimSuffix(u, "/agent")
	return u
}

// renew 用现有 mTLS 证书向 Relay /api/renew 续签(W7:不依赖一次性 token)。
func (a *AgentCerts) renew(ctx context.Context, agentID string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("生成密钥失败: %w", err)
	}
	csrTmpl := &x509.CertificateRequest{
		Subject:  pkix.Name{CommonName: agentID},
		DNSNames: []string{agentID},
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, csrTmpl, key)
	if err != nil {
		return fmt.Errorf("创建 CSR 失败: %w", err)
	}
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})
	renewURL := wsToHTTP(a.relayURL) + "/api/renew"
	body, _ := json.Marshal(map[string]string{"csr_pem": string(csrPEM)})
	req, _ := http.NewRequestWithContext(ctx, "POST", renewURL, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: a.TLSConfig()}}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("renew 请求失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("renew HTTP %d", resp.StatusCode)
	}
	var respBody proto.EnrollResp
	if err := json.NewDecoder(resp.Body).Decode(&respBody); err != nil {
		return fmt.Errorf("renew 响应解析失败: %w", err)
	}
	if !respBody.Success {
		return fmt.Errorf("renew 失败: %s", respBody.Error)
	}
	if err := atomicWriteFile(a.certPath, []byte(respBody.CertPEM), 0644); err != nil {
		return err
	}
	keyDER, _ := x509.MarshalPKCS8PrivateKey(key)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	if err := atomicWriteFile(a.keyPath, keyPEM, 0600); err != nil {
		return err
	}
	if err := atomicWriteFile(a.caPath, []byte(respBody.CAPEM), 0644); err != nil {
		return err
	}
	cert, err := tls.X509KeyPair([]byte(respBody.CertPEM), keyPEM)
	if err != nil {
		return err
	}
	a.cert = &cert
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM([]byte(respBody.CAPEM))
	a.caPool = pool
	log.Printf("[enroll] 证书续签成功(经 /api/renew),有效期至 %s",
		time.Unix(respBody.ExpiresAt, 0).Format("2006-01-02 15:04:05"))
	return nil
}

// atomicWriteFile 原子写文件(temp+rename,防中途崩溃留截断文件)。
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
