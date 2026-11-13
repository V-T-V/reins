// reins-gencert —— 自签 TLS 证书生成向导(打包分发配套)。
//
// 为 Relay 的 -tls-cert/-tls-key 生成自签证书对(PEM),零外部依赖(纯标准库)。
// 内网/隔离网络部署时无需 CA;面向公网请换正规 CA 签发(Let's Encrypt 等)。
//
// 用法:
//
//	reins-gencert -cn relay.corp.local -ip 10.0.0.5,127.0.0.1 -dns relay.corp.local,localhost -days 825 \
//	              -out-cert certs/relay.crt -out-key certs/relay.key
//
// 全部参数可选:默认 CN=reins-relay、SAN 含本机全部网卡 IP+localhost、有效期 825 天
// (Apple 对自签证书的最长信任期;到期重新生成即可,配合 tls_watcher 的过期监控)。
// 输出文件已存在时拒绝覆盖(-force 覆盖),key 文件权限 0600。
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"flag"
	"fmt"
	"math/big"
	"net"
	"os"
	"time"
)

// version 由 -ldflags 注入。
var version = "dev"

func main() {
	cn := flag.String("cn", "reins-relay", "证书 Common Name")
	sans := flag.String("dns", "", "DNS SAN,逗号分隔(默认自动含 localhost)")
	ips := flag.String("ip", "", "IP SAN,逗号分隔(默认自动含本机全部网卡 IP 与 127.0.0.1)")
	days := flag.Int("days", 825, "有效天数(自签证书建议 ≤825,配合 Relay TLS 过期监控)")
	outCert := flag.String("out-cert", "relay.crt", "证书输出路径(PEM)")
	outKey := flag.String("out-key", "relay.key", "私钥输出路径(PEM)")
	force := flag.Bool("force", false, "覆盖已存在的输出文件")
	showVer := flag.Bool("version", false, "打印版本")
	flag.Parse()

	if *showVer {
		fmt.Printf("reins-gencert %s\n", version)
		return
	}

	if err := run(*cn, *sans, *ips, *days, *outCert, *outKey, *force); err != nil {
		fmt.Fprintf(os.Stderr, "reins-gencert: %v\n", err)
		os.Exit(1)
	}
}

func run(cn, dnsStr, ipStr string, days int, outCert, outKey string, force bool) error {
	if days <= 0 {
		return fmt.Errorf("-days 须为正整数")
	}
	for _, p := range []string{outCert, outKey} {
		if !force {
			if _, err := os.Stat(p); err == nil {
				return fmt.Errorf("%s 已存在(用 -force 覆盖)", p)
			}
		}
	}

	// SAN 装配:显式值优先;未给 ip 时自动枚举本机网卡
	var dnsNames []string
	if dnsStr != "" {
		dnsNames = splitAndTrim(dnsStr)
	}
	var ipAddrs []net.IP
	if ipStr != "" {
		for _, s := range splitAndTrim(ipStr) {
			ip := net.ParseIP(s)
			if ip == nil {
				return fmt.Errorf("非法 IP: %q", s)
			}
			ipAddrs = append(ipAddrs, ip)
		}
	} else {
		ipAddrs = localIPs()
	}
	if len(dnsNames) == 0 {
		dnsNames = []string{"localhost"}
	}
	hasLoopback := false
	for _, ip := range ipAddrs {
		if ip.IsLoopback() {
			hasLoopback = true
		}
	}
	if !hasLoopback {
		ipAddrs = append(ipAddrs, net.ParseIP("127.0.0.1"))
	}

	// 生成密钥对 + 自签证书
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("生成私钥: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return fmt.Errorf("生成序列号: %w", err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cn, Organization: []string{"reins"}},
		NotBefore:             now.Add(-5 * time.Minute), // 时钟偏移容忍
		NotAfter:              now.AddDate(0, 0, days),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              dnsNames,
		IPAddresses:           ipAddrs,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return fmt.Errorf("签发证书: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return fmt.Errorf("编码私钥: %w", err)
	}

	// 落盘
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(outCert, certPEM, 0644); err != nil {
		return fmt.Errorf("写证书: %w", err)
	}
	if err := os.WriteFile(outKey, keyPEM, 0600); err != nil {
		return fmt.Errorf("写私钥: %w", err)
	}

	// 自检:刚生成的对必须能被 tls.LoadX509KeyPair 加载(把"生成成功"与"可用"分开)
	if _, err := tls.LoadX509KeyPair(outCert, outKey); err != nil {
		return fmt.Errorf("自检失败(生成的证书对无法加载): %w", err)
	}

	fmt.Printf("✅ 自签证书已生成\n  证书: %s\n  私钥: %s (0600)\n  CN=%s 有效期 %d 天\n  DNS SAN: %v\n  IP SAN: %v\n",
		outCert, outKey, cn, days, dnsNames, ipAddrs)
	fmt.Printf("启用: reins-relay -mtls -tls-cert %s -tls-key %s\n", outCert, outKey)
	return nil
}

// localIPs 枚举本机全部非 loopback 网卡 IPv4/IPv6(自签证书默认 SAN)。
func localIPs() []net.IP {
	var out []net.IP
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip != nil && !ip.IsLinkLocalUnicast() {
				out = append(out, ip)
			}
		}
	}
	return out
}

func splitAndTrim(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			part := trimSpaces(s[start:i])
			if part != "" {
				out = append(out, part)
			}
			start = i + 1
		}
	}
	return out
}

func trimSpaces(s string) string {
	a, b := 0, len(s)
	for a < b && (s[a] == ' ' || s[a] == '\t') {
		a++
	}
	for b > a && (s[b-1] == ' ' || s[b-1] == '\t') {
		b--
	}
	return s[a:b]
}
