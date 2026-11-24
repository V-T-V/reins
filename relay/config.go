package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// version 由 -ldflags 注入，默认 dev。
var version = "dev"

// Config Relay 运行配置。
type Config struct {
	Addr         string // HTTP 监听地址，如 ":7780"
	Token        string // Agent/Console 连接鉴权 token（空则不校验）
	Dev          bool   // 开发模式：打印调试日志、放宽校验
	MTLS         bool   // C7/C8：启用 mTLS（需内置 CA + Agent 证书）
	CertFile     string // Relay 服务器 TLS 证书（mTLS 模式）
	KeyFile      string // Relay 服务器 TLS 私钥（mTLS 模式）
	ExternalURL  string // 外部访问地址(经 nginx/反代/TLS 终端时用),如 https://203.0.113.10/rmm
	InstallToken string // 部署专用 Token(烘焙进 /install.ps1;仅授权 Agent 注册与 GET /api/agents)
	// P1 生产硬化:/metrics 与 /debug/pprof 的访问控制(防指标/内存侦察泄露)
	MetricsToken string // /metrics 独立 token(空则回退 Token;都空才真无鉴权,生产不推荐)
	DebugPprof   bool   // 启用 /debug/pprof 调试端点(默认关,生产关闭防 heap/goroutine 泄露)
	// P1-6 命令策略白名单(逗号分隔正则)。非空时 Relay 切换 allow-only:
	// 命令必须命中至少一条白名单规则,且仍受 32 条黑名单兜底(交集语义)。
	// 例:-policy-allow '^Get-Service,^Get-Process,^node '
	PolicyAllow string
	// F11 webhook 告警(逗号分隔 URL)。触发:task_failed/agent_offline/policy_blocked/
	// power_executed/agent_revoked;flag 优先,空则回退 REINS_WEBHOOKS 环境变量。
	Webhooks string
}

// parseFlags 解析命令行参数。
//
// 优先级：命令行 flag > 环境变量 > 默认值。
func parseFlags() Config {
	c := Config{}
	flag.StringVar(&c.Addr, "addr", getEnv("REINS_RELAY_ADDR", ":7780"), "HTTP 监听地址")
	flag.StringVar(&c.Token, "token", getEnv("REINS_RELAY_TOKEN", ""), "鉴权凭据,逗号分隔多条:纯 secret=全局 admin;tenant:role:secret 绑定该租户与角色(admin/operator/viewer)。租户与角色取服务端配置,请求方声明无效。空则不校验")
	flag.BoolVar(&c.Dev, "dev", getEnv("REINS_RELAY_DEV", "") != "", "开发模式（详细日志）")
	flag.BoolVar(&c.MTLS, "mtls", getEnv("REINS_RELAY_MTLS", "") != "", "启用 mTLS（需内置 CA + Agent 客户端证书）")
	flag.StringVar(&c.CertFile, "tls-cert", getEnv("REINS_RELAY_TLS_CERT", ""), "Relay TLS 证书文件（mTLS/HTTPS 用）")
	flag.StringVar(&c.KeyFile, "tls-key", getEnv("REINS_RELAY_TLS_KEY", ""), "Relay TLS 私钥文件（mTLS/HTTPS 用）")
	flag.StringVar(&c.ExternalURL, "external-url", getEnv("REINS_EXTERNAL_URL", ""), "外部访问地址(经 nginx 反代/TLS 终端时用),如 https://host/rmm")
	flag.StringVar(&c.InstallToken, "install-token", getEnv("REINS_INSTALL_TOKEN", ""), "部署专用 Token:服务 /install.ps1 时自动烘焙,目标机器零参数部署;仅授权 Agent 注册与 GET /api/agents?agent_id= 单机探活,不授予其余 REST 权限;泄漏后果=可注册伪 Agent,建议部署窗口开启并定期轮换")
	// P1 生产硬化:metrics/pprof 访问控制
	flag.StringVar(&c.MetricsToken, "metrics-token", getEnv("REINS_METRICS_TOKEN", ""), "/metrics 独立 token(空则回退主 Token;生产推荐配独立低权 token 或 nginx IP 白名单)")
	flag.BoolVar(&c.DebugPprof, "debug-pprof", getEnv("REINS_DEBUG_PPROF", "") != "", "启用 /debug/pprof(默认关,生产关闭防内存侦察;dev 模式自动开)")
	// P1-6:命令策略白名单(黑名单 best-effort 可绕,白名单 allow-only 收紧)
	flag.StringVar(&c.PolicyAllow, "policy-allow", getEnv("REINS_POLICY_ALLOW", ""), "命令白名单(逗号分隔正则;非空则 allow-only:须命中白名单且不碰黑名单)")
	// F11:webhook 告警(空则回退 REINS_WEBHOOKS 环境变量)
	flag.StringVar(&c.Webhooks, "webhooks", getEnv("REINS_WEBHOOKS", ""), "webhook 告警 URL(逗号分隔;触发 task_failed/agent_offline/policy_blocked/power_executed/agent_revoked)")
	flag.Parse()
	return c
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// String 配置的安全展示形式:凭据只报「有无/条数/首 4 位」,绝不回显完整 secret。
func (c Config) String() string {
	mask := "（未设置，不校验）"
	if c.Token != "" {
		// 多凭据不回显任何片段(列表首部可能是某个租户凭据的前缀)
		if strings.Contains(c.Token, ",") {
			mask = fmt.Sprintf("(%d 条凭据)***", len(strings.Split(c.Token, ",")))
		} else {
			mask = c.Token[:min(4, len(c.Token))] + "***"
		}
	}
	return fmt.Sprintf("addr=%s token=%s dev=%v mtls=%v install_token=%t metrics_token=%t policy_allow=%t",
		c.Addr, mask, c.Dev, c.MTLS, c.InstallToken != "", c.MetricsToken != "", c.PolicyAllow != "")
}

// LogValue 让 Config 可以安全地进结构化日志(slog.LogValuer)。
//
// 2026 修复(既有泄漏):启动日志写的是 `slog.Info(..., "config", cfg)`——把**结构体**交给
// JSON handler,它把全部导出字段原样序列化:Token(多凭据下是所有租户的 secret)、
// InstallToken、MetricsToken 明文落盘。日志被收进 journald/容器日志/SIEM 即凭据外泄。
// 在类型层面封死:无论谁把 Config 交给 slog,拿到的都是上面的脱敏字符串。
func (c Config) LogValue() slog.Value {
	return slog.StringValue(c.String())
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
	if c.Token == "" {
		return nil
	}
	// 2026 多凭据:逐条校验结构(角色/租户 ID/secret 复用与非法字符),
	// 长度建议也按**每条 secret**计(原按整串长度,多凭据下会被列表总长掩盖)。
	creds, err := parseCredentials(c.Token)
	if err != nil {
		return err
	}
	for _, cred := range creds {
		if len(cred.Secret) < 8 {
			return fmt.Errorf("token 凭据 %s 的 secret 长度 %d 过短(建议 ≥8 字符)", credentialLabel(cred), len(cred.Secret))
		}
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
