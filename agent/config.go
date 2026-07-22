package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/google/uuid"
)

// version 由 -ldflags 注入。
var version = "dev"

// Config Agent 运行配置。
//
// 服务模式下从配置文件加载；前台调试模式下从命令行参数覆盖。
type Config struct {
	AgentID   string            `json:"agent_id"`            // Agent 唯一 ID（空则自动生成并持久化）
	RelayURL  string            `json:"relay_url"`           // Relay 的 Agent WS 端点，如 ws://host:7780/agent
	Token     string            `json:"token,omitempty"`     // 鉴权 token（非 mTLS 模式）/ enrollment token（mTLS 首次注册）
	Tags      map[string]string `json:"tags,omitempty"`      // 业务标签
	Heartbeat time.Duration     `json:"heartbeat,omitempty"` // 心跳间隔，默认 15s
	// MTLS 启用 mTLS 模式（wss:// + 客户端证书）。为 true 时 Token 作为 enrollment token（首次），
	// 证书获取后用 mTLS 连接，不再用 token。为 false 时走 ws://+token 明文模式（向后兼容）。
	MTLS      bool              `json:"mtls,omitempty"`
	// MaxConcurrentTasks 最大并发执行任务数（C6 并发限流）。0 或负数表示不限。
	// 默认 4，防止被并发任务打爆。
	MaxConcurrentTasks int `json:"max_concurrent_tasks,omitempty"`
	Glm       struct {
		APIKey  string `json:"api_key,omitempty"` // GLM 视觉模型 key（guiclick 执行器需要）
		BaseURL string `json:"base_url,omitempty"` // 默认 https://open.bigmodel.cn/api/paas/v4
		Model   string `json:"model,omitempty"`    // 默认 glm-5-turbo
	} `json:"glm,omitempty"`
}

// DefaultConfig 返回带默认值的配置。
func DefaultConfig() Config {
	c := Config{
		Heartbeat:          15 * time.Second,
		MaxConcurrentTasks: 4,
	}
	c.Glm.BaseURL = "https://open.bigmodel.cn/api/paas/v4"
	c.Glm.Model = "glm-5-turbo"
	return c
}

// configPath 返回配置文件路径：与可执行文件同目录的 config.json。
func configPath() string {
	return filepath.Join(exeDir(), "config.json")
}

// agentDataDir 返回 Agent 数据目录（与可执行文件同目录的 data/）。
// 存放 mTLS 证书（agent.crt/agent.key/ca.crt）。
func agentDataDir() string {
	return filepath.Join(exeDir(), "data")
}

// exeDir 返回可执行文件所在目录。
func exeDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(exe)
}

// loadConfig 从配置文件加载（不存在则用默认值），并用 flag 覆盖。
//
// 子命令模式：
//
//	go-rmm-agent install -relay <url> [-token <t>]   注册为服务
//	go-rmm-agent uninstall
//	go-rmm-agent start
//	go-rmm-agent stop
//	go-rmm-agent run -relay <url> [-token <t>]       前台调试
//	go-rmm-agent status
func loadConfig(args []string) (cmd string, cfg Config, err error) {
	cfg = DefaultConfig()

	// 先从配置文件加载（服务模式依赖此文件）
	if b, e := os.ReadFile(configPath()); e == nil {
		if e := json.Unmarshal(b, &cfg); e != nil {
			return "", cfg, fmt.Errorf("配置文件 %s 解析失败: %w", configPath(), e)
		}
	}

	// 默认 AgentID 持久化
	if cfg.AgentID == "" {
		cfg.AgentID = generateAgentID()
		_ = saveConfig(cfg) // 持久化生成的 ID
	}

	if len(args) == 0 {
		return "run", cfg, nil // 服务模式由 SCM 调起，无子命令
	}
	cmd = args[0]
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	relayURL := fs.String("relay", cfg.RelayURL, "Relay 的 Agent WS 端点 URL")
	token := fs.String("token", cfg.Token, "鉴权 token / enrollment token（mTLS）")
	agentID := fs.String("id", cfg.AgentID, "Agent ID")
	mtls := fs.Bool("mtls", cfg.MTLS, "启用 mTLS 模式（wss://+客户端证书）")
	if e := fs.Parse(args[1:]); e != nil {
		return cmd, cfg, e
	}
	cfg.RelayURL = *relayURL
	cfg.Token = *token
	cfg.AgentID = *agentID
	cfg.MTLS = *mtls
	return cmd, cfg, nil
}

// saveConfig 持久化配置到文件（用于服务模式重启后恢复）。
// Q3 修复：配置含 token/glm.api_key 等敏感信息，权限改 0600。
func saveConfig(cfg Config) error {
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath(), b, 0600)
}

// generateAgentID 生成形如 agent-<hostname>-<shortuuid> 的 ID。
func generateAgentID() string {
	host, _ := os.Hostname()
	if host == "" {
		host = runtime.GOOS
	}
	// 截短 UUID 前 8 位
	short := uuid.NewString()[:8]
	return fmt.Sprintf("agent-%s-%s", host, short)
}
