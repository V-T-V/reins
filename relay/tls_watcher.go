// Package main — TLS 证书过期检测(XL5 神级提升)。
//
// 定期检查 Relay 的 TLS 证书过期时间,提前告警。
// 配合 certbot/Let's Encrypt 实现自动轮换:
//  1. certbot renew 每周检查 + 续签
//  2. Relay 重载证书(无需重启,信号触发)
//  3. 此模块监控证书状态 + Prometheus 告警指标
package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log/slog"
	"os"
	"time"
)

// TLSCertInfo 证书信息。
type TLSCertInfo struct {
	Path      string
	NotBefore time.Time
	NotAfter  time.Time
	DaysLeft  int64
	Issuer    string
	Subject   string
	AutoRenew bool // 是否启用自动轮换(certbot)
}

// CheckCertExpiry 检查证书过期状态。
func CheckCertExpiry(certPath string) (*TLSCertInfo, error) {
	data, err := os.ReadFile(certPath)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("证书 PEM 解码失败")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, err
	}
	daysLeft := int64(time.Until(cert.NotAfter).Hours() / 24)
	return &TLSCertInfo{
		Path:      certPath,
		NotBefore: cert.NotBefore,
		NotAfter:  cert.NotAfter,
		DaysLeft:  daysLeft,
		Issuer:    cert.Issuer.CommonName,
		Subject:   cert.Subject.CommonName,
	}, nil
}

// certExpiryLoop 定期检查证书过期(每小时)。
// 证书 < 14 天时告警,< 7 天时 ERROR 级别。
func certExpiryLoop(certPath string) {
	if certPath == "" {
		return
	}
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()
	checkOnce := func() {
		info, err := CheckCertExpiry(certPath)
		if err != nil {
			slog.Error("证书检查失败", "path", certPath, "error", err)
			return
		}
		if info.DaysLeft < 7 {
			slog.Error("⚠️ 证书即将过期", "path", certPath, "days_left", info.DaysLeft,
				"expires", info.NotAfter.Format("2006-01-02"), "subject", info.Subject)
		} else if info.DaysLeft < 30 {
			slog.Warn("证书即将过期", "path", certPath, "days_left", info.DaysLeft,
				"expires", info.NotAfter.Format("2006-01-02"))
		}
	}
	checkOnce() // 启动时立即检查
	for range ticker.C {
		checkOnce()
	}
}

// LoadCertForRelay 加载 Relay 证书(支持运行时 reload)。
func LoadCertForRelay(certFile, keyFile string) (*tls.Certificate, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	return &cert, nil
}

// CertDaysLeftMetric 返回证书剩余天数(供 Prometheus 指标用)。
func CertDaysLeftMetric(certPath string) int64 {
	info, err := CheckCertExpiry(certPath)
	if err != nil {
		return -1
	}
	return info.DaysLeft
}
