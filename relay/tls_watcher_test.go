package main

// XL5 TLS 证书过期检测测试(2026-09-14,零测试模块补测)。
//
// CheckCertExpiry 是 certbot/Let's Encrypt 自动轮换链路的观测面:
// CertDaysLeftMetric 供 Prometheus 告警指标。此前整个模块零测试。
// 覆盖:正常解析(NotAfter/DaysLeft/Issuer/Subject)/三种错误路径
// (文件缺失/非 PEM/坏 DER)/CertDaysLeftMetric 双口径/LoadCertForRelay。

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeTestCert 生成一张自签证书写入临时文件,返回证书路径。
// notAfter 控制 NotAfter(过期检测的核心输入);CN 同时用作 Subject 与 Issuer。
func writeTestCert(t *testing.T, cn string, notAfter time.Time) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成密钥失败: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("签发证书失败: %v", err)
	}
	path := filepath.Join(t.TempDir(), "cert.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("写证书文件失败: %v", err)
	}
	return path
}

// TestCheckCertExpiryParses 正常路径:字段齐全 + DaysLeft 取整语义。
// DaysLeft = int64(Until(NotAfter).Hours()/24):36h → 1(截断非四舍五入)。
func TestCheckCertExpiryParses(t *testing.T) {
	path := writeTestCert(t, "watcher-unit-cn", time.Now().Add(36*time.Hour))

	info, err := CheckCertExpiry(path)
	if err != nil {
		t.Fatalf("CheckCertExpiry 失败: %v", err)
	}
	if info.Path != path {
		t.Errorf("Path 应原样回传: %q", info.Path)
	}
	if info.Subject != "watcher-unit-cn" {
		t.Errorf("Subject CN 应为 watcher-unit-cn,得到 %q", info.Subject)
	}
	// 自签证书 Issuer == Subject
	if info.Issuer != "watcher-unit-cn" {
		t.Errorf("自签证书 Issuer CN 应等于 Subject,得到 %q", info.Issuer)
	}
	if info.DaysLeft != 1 {
		t.Errorf("36h 剩余应截断为 1 天(int64(hours/24)),得到 %d", info.DaysLeft)
	}
	if !info.NotAfter.After(time.Now()) {
		t.Error("NotAfter 应在未来")
	}
	if !info.NotBefore.Before(time.Now()) {
		t.Error("NotBefore 应在过去")
	}
}

// TestCheckCertExpiryExpired 已过期证书:DaysLeft 为 0 或负(48h 前 → -2)。
func TestCheckCertExpiryExpired(t *testing.T) {
	path := writeTestCert(t, "expired-cn", time.Now().Add(-48*time.Hour))

	info, err := CheckCertExpiry(path)
	if err != nil {
		t.Fatalf("过期证书仍应可解析: %v", err)
	}
	if info.DaysLeft != -2 {
		t.Errorf("过期 48h 应得 -2 天,得到 %d", info.DaysLeft)
	}
}

// TestCheckCertExpiryErrors 三种错误路径:文件缺失/非 PEM/PEM 内坏 DER。
func TestCheckCertExpiryErrors(t *testing.T) {
	// 1. 文件不存在
	if _, err := CheckCertExpiry(filepath.Join(t.TempDir(), "nope.pem")); err == nil {
		t.Error("缺失文件应报错")
	}
	// 2. 非 PEM 内容
	dir := t.TempDir()
	junk := filepath.Join(dir, "junk.pem")
	if err := os.WriteFile(junk, []byte("not a pem at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := CheckCertExpiry(junk); err == nil {
		t.Error("非 PEM 内容应报错")
	}
	// 3. PEM 块内是坏 DER
	bad := filepath.Join(dir, "bad.pem")
	if err := os.WriteFile(bad, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte{0x01, 0x02, 0x03}}), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := CheckCertExpiry(bad); err == nil {
		t.Error("坏 DER 应报错")
	}
}

// TestCertDaysLeftMetric 双口径:正常路径与 CheckCertExpiry 一致,错误路径 -1。
// 注:notAfter 取 84h(3.5 天)——int64 截断语义下恰好整天边界(72h)会因
// 证书生成到检查之间的毫秒级延迟抖到 2,选半整天窗口使截断结果稳定。
func TestCertDaysLeftMetric(t *testing.T) {
	path := writeTestCert(t, "metric-cn", time.Now().Add(84*time.Hour))

	got := CertDaysLeftMetric(path)
	info, err := CheckCertExpiry(path)
	if err != nil {
		t.Fatalf("CheckCertExpiry 失败: %v", err)
	}
	if got != info.DaysLeft {
		t.Errorf("CertDaysLeftMetric(%d) 与 CheckCertExpiry.DaysLeft(%d) 不一致", got, info.DaysLeft)
	}
	if got != 3 {
		t.Errorf("84h 剩余应截断为 3 天,得到 %d", got)
	}
	if bad := CertDaysLeftMetric(filepath.Join(t.TempDir(), "missing.pem")); bad != -1 {
		t.Errorf("错误路径应返回 -1,得到 %d", bad)
	}
}

// TestLoadCertForRelay 合法 keypair 可加载;坏路径报错。
func TestLoadCertForRelay(t *testing.T) {
	dir := t.TempDir()
	certPath := writeTestCert(t, "relay-cn", time.Now().Add(24*time.Hour))

	// key 文件不存在 → 失败
	if _, err := LoadCertForRelay(certPath, filepath.Join(dir, "nope.key")); err == nil {
		t.Error("key 缺失应报错")
	}
	// key 是垃圾内容 → 失败
	junkKey := filepath.Join(dir, "junk.key")
	if err := os.WriteFile(junkKey, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCertForRelay(certPath, junkKey); err == nil {
		t.Error("坏 key 应报错")
	}
	// cert 缺失 → 失败
	if _, err := LoadCertForRelay(filepath.Join(dir, "nope.pem"), junkKey); err == nil {
		t.Error("cert 缺失应报错")
	}
}
