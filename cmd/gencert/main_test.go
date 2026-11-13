package main

import (
	"crypto/tls"
	"crypto/x509"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// 生成 → 落盘 → tls.LoadX509KeyPair 自检闭环(与 relay 同一加载路径)。
func TestGencertRoundtrip(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "relay.crt")
	keyPath := filepath.Join(dir, "relay.key")

	if err := run("relay.test", "relay.test,localhost", "127.0.0.1,::1", 30, certPath, keyPath, false); err != nil {
		t.Fatalf("生成失败: %v", err)
	}

	// 1) 证书对可被标准库加载(relay tls_watcher 同路径)
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatalf("LoadX509KeyPair: %v", err)
	}

	// 2) 解析证书断言 SAN/CN/有效期
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	if cert.Subject.CommonName != "relay.test" {
		t.Errorf("CN = %s", cert.Subject.CommonName)
	}
	dnsSet := map[string]bool{}
	for _, d := range cert.DNSNames {
		dnsSet[d] = true
	}
	if !dnsSet["relay.test"] || !dnsSet["localhost"] {
		t.Errorf("DNS SAN = %v", cert.DNSNames)
	}
	ipSet := map[string]bool{}
	for _, ip := range cert.IPAddresses {
		ipSet[ip.String()] = true
	}
	if !ipSet["127.0.0.1"] || !ipSet["::1"] {
		t.Errorf("IP SAN = %v", cert.IPAddresses)
	}
	if !cert.NotAfter.After(cert.NotBefore) {
		t.Errorf("NotAfter 应晚于 NotBefore")
	}

	// 3) key 文件 0600(Windows/NTFS 不落实 chmod,跳过)
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(keyPath)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0600 {
			t.Errorf("key 权限 = %o, 期望 0600", fi.Mode().Perm())
		}
	}

	// 4) 已存在时拒绝覆盖;force 通过
	if err := run("x", "", "", 1, certPath, keyPath, false); err == nil {
		t.Errorf("已存在文件应拒绝覆盖")
	}
	if err := run("x", "", "", 1, certPath, keyPath, true); err != nil {
		t.Errorf("force 覆盖失败: %v", err)
	}

	// 5) 非法 IP 报错
	if err := run("x", "", "not-an-ip", 1, filepath.Join(dir, "a.crt"), filepath.Join(dir, "a.key"), false); err == nil {
		t.Errorf("非法 IP 应报错")
	}
	// 6) 非法天数报错
	if err := run("x", "", "", 0, filepath.Join(dir, "b.crt"), filepath.Join(dir, "b.key"), false); err == nil {
		t.Errorf("days=0 应报错")
	}
}

// splitAndTrim 纯函数边界(空段/空格)。
func TestSplitAndTrim(t *testing.T) {
	got := splitAndTrim(" a, b ,,c ")
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Errorf("splitAndTrim = %v", got)
	}
	if got := splitAndTrim(""); len(got) != 0 {
		t.Errorf("空串应返回空切片")
	}
}

// localIPs 枚举不应 panic(结果依赖网卡环境,不做内容断言)。
func TestLocalIPsNoPanic(t *testing.T) {
	_ = localIPs()
}
