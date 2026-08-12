package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
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
	ExternalURL string // 外部访问地址(经 nginx/反代/TLS 终端时用),如 https://203.0.113.99/rmm
	// P1 生产硬化:/metrics 与 /debug/pprof 的访问控制(防指标/内存侦察泄露)
	MetricsToken string // /metrics 独立 token(空则回退 Token;都空才真无鉴权,生产不推荐)
	DebugPprof   bool   // 启用 /debug/pprof 调试端点(默认关,生产关闭防 heap/goroutine 泄露)
	// Agent 编排(2026-08-06):Web 控制台自然语言任务 → LLM 规划 → 工具执行
	LLMBaseURL  string // OpenAI 兼容 /v1 地址,默认 http://127.0.0.1:11434/v1(Ollama);空=禁用 Agent 编排
	LLMAPIVKey  string // API key(可空,Ollama 不需要)
	LLMModel    string // 模型名,默认 qwen2.5:7b
	LLMAgentMax int    // 并发 Agent run 上限,默认 2(防打爆 LLM/Relay)

	// 控制台会话鉴权(2026-08-12):HttpOnly+Secure+SameSite Cookie
	// 浏览器登录后颁发有状态内存 session,替代把 Relay Token 放浏览器/localStorage。
	// ConsolePassword 空=控制台登录禁用(只能用 Bearer,即 CLI/Agent 路径)。
	ConsolePassword     string        // 控制台登录密码(独立于 Token);空=禁用控制台登录
	ConsoleSessionTTL   time.Duration // 会话有效期,默认 8h(滑动续期)
	ConsoleCookieName   string        // session cookie 名,默认 gormm_session
	ConsoleSecureCookie bool          // 强制 Secure 标志(反代 TLS 终端下默认自动识别 X-Forwarded-Proto)
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
	// Agent 编排 LLM 配置(Ollama 默认;空 base-url 禁用)
	flag.StringVar(&c.LLMBaseURL, "llm-base-url", getEnv("GO_RMM_LLM_BASE_URL", ""), "LLM OpenAI 兼容 /v1 地址,如 http://127.0.0.1:11434/v1(空=禁用 Agent 编排)")
	flag.StringVar(&c.LLMAPIVKey, "llm-api-key", getEnv("GO_RMM_LLM_API_KEY", ""), "LLM API key(可空,Ollama 不需要)")
	flag.StringVar(&c.LLMModel, "llm-model", getEnv("GO_RMM_LLM_MODEL", "qwen2.5:7b"), "LLM 模型名")
	flag.IntVar(&c.LLMAgentMax, "llm-agent-max", atoiOr(getEnv("GO_RMM_LLM_AGENT_MAX", "2"), 2), "并发 Agent run 上限(默认 2)")
	// 控制台会话鉴权(2026-08-12)
	flag.StringVar(&c.ConsolePassword, "console-password", getEnv("GO_RMM_CONSOLE_PASSWORD", ""), "控制台登录密码(独立于 Relay Token;空=禁用控制台登录,只能用 CLI Bearer)")
	flag.DurationVar(&c.ConsoleSessionTTL, "console-session-ttl", durationOr(getEnv("GO_RMM_CONSOLE_SESSION_TTL", "8h"), 8*time.Hour), "控制台会话有效期(滑动续期),如 8h / 30m")
	flag.StringVar(&c.ConsoleCookieName, "console-cookie-name", getEnv("GO_RMM_CONSOLE_COOKIE_NAME", "gormm_session"), "控制台 session cookie 名")
	flag.BoolVar(&c.ConsoleSecureCookie, "console-secure-cookie", getEnv("GO_RMM_CONSOLE_SECURE_COOKIE", "") != "", "强制 cookie Secure 标志(反代 TLS 终端下默认按 X-Forwarded-Proto 自动识别)")
	flag.Parse()
	return c
}

// durationOr 解析时长字符串(go duration 语法,如 8h/30m/600s),失败回退默认值。
func durationOr(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	if d, err := time.ParseDuration(s); err == nil {
		return d
	}
	return def
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// atoiOr 解析整数字符串,失败回退默认值(环境变量用)。
func atoiOr(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}

func (c Config) String() string {
	mask := "（未设置，不校验）"
	if c.Token != "" {
		mask = c.Token[:min(4, len(c.Token))] + "***"
	}
	console := "禁用"
	if c.ConsolePassword != "" {
		console = c.ConsolePassword[:min(4, len(c.ConsolePassword))] + "***"
	}
	return fmt.Sprintf("addr=%s token=%s dev=%v mtls=%v console=%s", c.Addr, mask, c.Dev, c.MTLS, console)
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
	// 控制台会话校验
	if c.ConsolePassword != "" {
		if len(c.ConsolePassword) < 6 {
			return fmt.Errorf("console-password 长度 %d 过短(建议 ≥6 字符)", len(c.ConsolePassword))
		}
		if c.ConsoleSessionTTL < time.Minute {
			return fmt.Errorf("console-session-ttl %s 过短(最小 1m)", c.ConsoleSessionTTL)
		}
		if c.ConsoleCookieName == "" {
			return fmt.Errorf("console-cookie-name 不能为空(启用控制台鉴权时)")
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
