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
	Addr        string // HTTP 监听地址，如 ":7780"
	Token       string // Agent/Console 连接鉴权 token（空则不校验）
	Dev         bool   // 开发模式：打印调试日志、放宽校验
	MTLS        bool   // C7/C8：启用 mTLS（需内置 CA + Agent 证书）
	CertFile    string // Relay 服务器 TLS 证书（mTLS 模式）
	KeyFile     string // Relay 服务器 TLS 私钥（mTLS 模式）
	ExternalURL string // 外部访问地址(经 nginx/反代/TLS 终端时用),如 https://203.0.113.10/rmm
	// P1 生产硬化:/metrics 与 /debug/pprof 的访问控制(防指标/内存侦察泄露)
	MetricsToken string // /metrics 独立 token(空则回退 Token;都空才真无鉴权,生产不推荐)
	DebugPprof   bool   // 启用 /debug/pprof 调试端点(默认关,生产关闭防 heap/goroutine 泄露)
	// P1-6 命令策略白名单(逗号分隔正则)。非空时 Relay 切换 allow-only:
	// 命令必须命中至少一条白名单规则,且仍受 32 条黑名单兜底(交集语义)。
	// 例:-policy-allow '^Get-Service,^Get-Process,^node '
	PolicyAllow string
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
	flag.StringVar(&c.ExternalURL, "external-url", getEnv("GO_RMM_EXTERNAL_URL", ""), "外部访问地址(经 nginx 反代/TLS 终端时用),如 https://host/rmm")
	// P1 生产硬化:metrics/pprof 访问控制
	flag.StringVar(&c.MetricsToken, "metrics-token", getEnv("GO_RMM_METRICS_TOKEN", ""), "/metrics 独立 token(空则回退主 Token;生产推荐配独立低权 token 或 nginx IP 白名单)")
	flag.BoolVar(&c.DebugPprof, "debug-pprof", getEnv("GO_RMM_DEBUG_PPROF", "") != "", "启用 /debug/pprof(默认关,生产关闭防内存侦察;dev 模式自动开)")
	// P1-6:命令策略白名单(黑名单 best-effort 可绕,白名单 allow-only 收紧)
	flag.StringVar(&c.PolicyAllow, "policy-allow", getEnv("GO_RMM_POLICY_ALLOW", ""), "命令白名单(逗号分隔正则;非空则 allow-only:须命中白名单且不碰黑名单)")
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
	return fmt.Sprintf("addr=%s token=%s dev=%v mtls=%v", c.Addr, mask, c.Dev, c.MTLS)
}

// Validate 校验 Relay 配置(M5 神级提升)。
// 返回 nil 表示配置有效,非 nil 含可读的错误说明。
func (c Config) Validate() error {
	if c.Addr == "" {
		return fmt.Errorf("addr 不能为空")
	}
	// mTLS 模式必须配证书
	if c.MTLS {
		if c.CertFile == "" || c.KeyFile == "" {
			return fmt.Errorf("mtls 模式需要 -tls-cert 和 -tls-key")
		}
	}
	// token 长度建议(非强制)
	if c.Token != "" && len(c.Token) < 8 {
		return fmt.Errorf("token 长度 %d 过短(建议 ≥8 字符)", len(c.Token))
	}
	return nil
}

// dataDir 返回数据目录（与可执行文件同目录的 data/）。
func dataDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "data"
	}
	return filepath.Join(filepath.Dir(exe), "data")
}

// dlDir 返回 Agent 二进制下载目录（与可执行文件同目录的 dl/）。
func dlDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "dl"
	}
	return filepath.Join(filepath.Dir(exe), "dl")
}
