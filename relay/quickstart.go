// Package main — 零配置辅助(S2:极简安装体验)。
package main

import (
	"crypto/rand"
	"encoding/hex"
	"net"
	"os"
	"path/filepath"
)

// loadOrGenToken 读取已保存的 token,不存在则生成并保存。
func loadOrGenToken() string {
	tokenPath := filepath.Join(dataDir(), ".token")
	if data, err := os.ReadFile(tokenPath); err == nil && len(data) >= 16 {
		return string(data)
	}
	// 生成 32 字符 hex token
	b := make([]byte, 16)
	rand.Read(b)
	token := hex.EncodeToString(b)
	os.MkdirAll(dataDir(), 0755)
	os.WriteFile(tokenPath, []byte(token), 0600)
	return token
}

// getLocalIP 返回本机第一个非 loopback IPv4。
func getLocalIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "127.0.0.1"
	}
	for _, addr := range addrs {
		if ipNet, ok := addr.(*net.IPNet); ok && !ipNet.IP.IsLoopback() {
			if ipNet.IP.To4() != nil {
				return ipNet.IP.String()
			}
		}
	}
	return "127.0.0.1"
}
