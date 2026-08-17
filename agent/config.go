package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/google/uuid"
)

// version 由 -ldflags 注入。
var version = "dev"

// Config Agent 运行配置。
//
// 服务模式下从配置文件加载；前台调试模式下从命令行参数覆盖。
type Config struct {
	AgentID  string `json:"agent_id"`  // Agent 唯一 ID（空则自动生成并持久化）
	RelayURL string `json:"relay_url"` // Relay 的 Agent WS 端点，如 ws://host:7780/agent
	// RelayURLs 备用 Relay 列表(L2 故障切换)。RelayURL 连接失败时按序尝试。
	// 优先级:RelayURL > RelayURLs[0] > RelayURLs[1] > ...
	// DNS SRV/域名轮询可用单个域名,N 个 Agent 分布到不同 IP。
	RelayURLs []string          `json:"relay_urls,omitempty"`
	Token     string            `json:"token,omitempty"`     // 鉴权 token（非 mTLS 模式）/ enrollment token（mTLS 首次注册）
	Tags      map[string]string `json:"tags,omitempty"`      // 业务标签
	Heartbeat time.Duration     `json:"heartbeat,omitempty"` // 心跳间隔，默认 15s
	// MTLS 启用 mTLS 模式（wss:// + 客户端证书）。为 true 时 Token 作为 enrollment token（首次），
	// 证书获取后用 mTLS 连接，不再用 token。为 false 时走 ws://+token 明文模式（向后兼容）。
	MTLS bool `json:"mtls,omitempty"`
	// AllowedRoots 文件操作允许的根目录(file 任务 + 大文件传输共用)。
	// 留空时按 AllowAllRoots 决定:AllowAllRoots=true 则整盘兜底(兼容),false 则严格拒绝。
	AllowedRoots []string `json:"allowed_roots,omitempty"`
	// AllowAllRoots 未配置 AllowedRoots 时是否放开整盘(向后兼容默认 true)。
	// 生产环境应设 false + 配置 AllowedRoots 做最小权限。
	AllowAllRoots bool `json:"allow_all_roots,omitempty"`
	// Workspace 工作空间目录(deploy/文件操作默认基准目录)。
	// 留空则默认 <exeDir>/workspace/。启动时自动 MkdirAll。
	// AllowedRoots 会自动追加此目录(部署目录始终可写)。
	Workspace string `json:"workspace,omitempty"`
	// PolicyAllow 命令白名单正则(P1-6,可选)。非空时 Agent 侧切换 allow-only:
	// 命令必须命中至少一条规则,且仍受黑名单兜底(交集语义)。
	// Agent 是强制底线——即使 Relay 被绕过(交互式 shell/cron)也生效。
	PolicyAllow []string `json:"policy_allow,omitempty"`
	// MinVersion 允许的最低版本(防降级)。
	// 自更新时若新版本 < MinVersion 则拒绝。留空则不强制。
	MinVersion string `json:"min_version,omitempty"`
	// MaxConcurrentTasks 最大并发执行任务数(C6 并发限流)。0 或负数表示不限。
	// 默认 4,防止被并发任务打爆。
	MaxConcurrentTasks int `json:"max_concurrent_tasks,omitempty"`
	Glm                struct {
		APIKey  string `json:"api_key,omitempty"`  // GLM 视觉模型 key（guiclick 执行器需要）
		BaseURL string `json:"base_url,omitempty"` // 默认 https://open.bigmodel.cn/api/paas/v4
		Model   string `json:"model,omitempty"`    // 默认 glm-5-turbo
	} `json:"glm,omitempty"`

	// === 主从双 Agent 架构(M1) ===
	//
	// Role 角色:"master"(主,稳定不更新,负责救援从)/ "slave"(从,可热更新,承担业务)。
	// 留空 = 向后兼容,等同 slave(单 Agent 模式)。
	//
	// master 行为差异:
	//   - 注册最小 executor 集(powershell/script/file/service/watchdog),不注册 screenshot/monitor/browse/guiclick
	//   - 拒绝 update 任务(主自己永不更新)
	//   - 启动本地看门狗 goroutine,周期检查 slave 服务状态并自动救援
	//
	// slave 行为差异:
	//   - 注册全 executor 集(现有行为不变)
	//   - 接受 update 任务(可被热更新)
	Role string `json:"role,omitempty"`

	// ServiceName Windows/Linux 服务名。
	// 留空则按 Role 推导:master→go-rmm-master, slave→go-rmm-slave, 空→go-rmm-agent(兼容)。
	// 用于 kardianos/service 注册 + 看门狗 sc.exe 操作。
	ServiceName string `json:"service_name,omitempty"`

	// === master 专属配置(看门狗 + 救援用) ===
	//
	// SlaveExeDir 从 Agent 的 exe 目录(如 C:\Program Files\go-rmm-slave)。
	// 看门狗用它定位从的 .bak 文件做本地回滚兜底。仅 master 需要。
	SlaveExeDir string `json:"slave_exe_dir,omitempty"`
	// SlaveAgentID 从 Agent 的 agent_id(如 agent-PC-abc123)。
	// master 经 Relay 救援时用此 ID 定位从(POST /api/updates/apply)。仅 master 需要。
	SlaveAgentID string `json:"slave_agent_id,omitempty"`
}

// DefaultConfig 返回带默认值的配置。
func DefaultConfig() Config {
	c := Config{
		Heartbeat:          15 * time.Second,
		MaxConcurrentTasks: 4,
		AllowAllRoots:      true, // P0-S4:向后兼容默认放开整盘(带告警),生产应设 false
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

// Validate 校验 Agent 配置(M5 神级提升)。
func (c Config) Validate() error {
	if c.RelayURL == "" {
		return fmt.Errorf("relay_url 不能为空")
	}
	// URL 格式校验
	if !strings.HasPrefix(c.RelayURL, "ws://") && !strings.HasPrefix(c.RelayURL, "wss://") {
		return fmt.Errorf("relay_url 必须以 ws:// 或 wss:// 开头")
	}
	if c.AgentID == "" {
		return fmt.Errorf("agent_id 不能为空(留空则自动生成)")
	}
	if c.Heartbeat < 5*time.Second && c.Heartbeat != 0 {
		return fmt.Errorf("heartbeat %v 过短(建议 ≥5s)", c.Heartbeat)
	}
	if c.MaxConcurrentTasks < 0 {
		return fmt.Errorf("max_concurrent_tasks 不能为负数")
	}
	// 主从角色校验
	if c.Role != "" && c.Role != "master" && c.Role != "slave" {
		return fmt.Errorf("role 必须是 master/slave/空(兼容),得到 %q", c.Role)
	}
	// master 必须配 SlaveExeDir(看门狗需要)
	if c.Role == "master" && c.SlaveExeDir == "" {
		return fmt.Errorf("master 角色必须配置 slave_exe_dir(看门狗需要定位从的目录)")
	}
	return nil
}

// DerivedServiceName 按 Role 推导 Windows/Linux 服务名。
// 优先用显式配置的 ServiceName,否则按 Role 推导。
func (c Config) DerivedServiceName() string {
	if c.ServiceName != "" {
		return c.ServiceName
	}
	switch c.Role {
	case "master":
		return "go-rmm-master"
	case "slave":
		return "go-rmm-slave"
	default:
		return "go-rmm-agent" // 向后兼容
	}
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
	role := fs.String("role", cfg.Role, "角色: master(主,稳定)/ slave(从,可更新)。空=兼容单 Agent")
	if e := fs.Parse(args[1:]); e != nil {
		return cmd, cfg, e
	}
	cfg.RelayURL = normalizeRelayURL(*relayURL) // P1：裸 IP 自动补全
	cfg.Token = *token
	cfg.AgentID = *agentID
	cfg.MTLS = *mtls
	if *role != "" {
		cfg.Role = *role
	}
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

// normalizeRelayURL 把用户输入的简写地址补全为完整 WS URL。
// 支持以下输入格式：
//
//	"192.168.1.100"        → ws://192.168.1.100:7780/agent
//	"192.168.1.100:9999"   → ws://192.168.1.100:9999/agent
//	"relay.example.com"    → ws://relay.example.com:7780/agent
//	"ws://host:7780/agent" → 不变（已是完整 URL）
//	"wss://host/agent"     → 不变
func normalizeRelayURL(input string) string {
	if input == "" {
		return ""
	}
	// 已含 scheme，直接返回
	if strings.HasPrefix(input, "ws://") || strings.HasPrefix(input, "wss://") {
		return input
	}
	// 裸地址：自动补全
	if !strings.Contains(input, ":") {
		return "ws://" + input + ":7780/agent"
	}
	return "ws://" + input + "/agent"
}
func generateAgentID() string {
	host, _ := os.Hostname()
	if host == "" {
		host = runtime.GOOS
	}
	// 截短 UUID 前 8 位
	short := uuid.NewString()[:8]
	return fmt.Sprintf("agent-%s-%s", host, short)
}
