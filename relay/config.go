package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

// version 由 -ldflags 注入，默认 dev。
var version = "dev"

// Config Relay 运行配置。
type Config struct {
	Addr     string // HTTP 监听地址，如 ":7780"
	Token    string // Agent/Console 连接鉴权 token（空则不校验）
	Dev      bool   // 开发模式：打印调试日志、放宽校验
	MTLS     bool   // C7/C8：启用 mTLS（需内置 CA + Agent 证书）
	CertFile string // Relay 服务器 TLS 证书（mTLS 模式）
	KeyFile  string // Relay 服务器 TLS 私钥（mTLS 模式）
}

// parseFlags 解析命令行参数。
//
// 优先级：命令行 flag > 环境变量 > 默认值。
func parseFlags() Config {
	c := Config{}
	flag.StringVar(&c.Addr, "addr", getEnv("GO_RMM_RELAY_ADDR", ":7780"), "HTTP 监听地址")
	flag.StringVar(&c.Token, "token", getEnv("GO_RMM_RELAY_TOKEN", ""), "Agent/Console 鉴权 token（空则不校验）")
	flag.BoolVar(&c.Dev, "dev", getEnv("GO_RMM_RELAY_DEV", "") != "", "开发模式（详细日志）")
	flag.BoolVar(&c.MTLS, "mtls", getEnv("GO_RMM_RELAY_MTLS", "") != "", "启用 mTLS（需内置 CA + Agent 客户端证书）")
	flag.StringVar(&c.CertFile, "tls-cert", getEnv("GO_RMM_RELAY_TLS_CERT", ""), "Relay TLS 证书文件（mTLS/HTTPS 用）")
	flag.StringVar(&c.KeyFile, "tls-key", getEnv("GO_RMM_RELAY_TLS_KEY", ""), "Relay TLS 私钥文件（mTLS/HTTPS 用）")
	flag.Parse()
	return c
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func (c Config) String() string {
	mask := "（未设置，不校验）"
	if c.Token != "" {
		mask = c.Token[:min(4, len(c.Token))] + "***"
	}
	return fmt.Sprintf("addr=%s token=%s dev=%v", c.Addr, mask, c.Dev)
}

// dataDir 返回数据目录（与可执行文件同目录的 data/）。
func dataDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "data"
	}
	return filepath.Join(filepath.Dir(exe), "data")
}
