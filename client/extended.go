// Package client — 扩展 API(AI Agent 接入所需的全部操作)。
//
// 让外部 AI Agent 通过此 SDK 完整控制 go-rmm 平台的所有能力。
// 覆盖:任务/文件/Shell/电源/Cron/Secret/系统信息/工具发现。
package client

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/QiuShichang/go-rmm/proto"
)

// === 批量任务 ===

// BatchService 批量任务操作。
type BatchService struct{ c *Client }

// Batch 返回批量任务服务。
func (c *Client) Batch() *BatchService { return &BatchService{c: c} }

// RunByTag 按标签批量下发任务,返回命中的 Agent 列表。
func (s *BatchService) RunByTag(tag, taskType string, payload json.RawMessage, timeout int) ([]string, error) {
	body := map[string]any{"tag": tag, "type": taskType, "payload": payload, "timeout": timeout}
	var resp struct {
		Dispatched []string `json:"dispatched"`
		Count      int      `json:"count"`
	}
	if err := s.c.post("/api/tasks/batch", body, &resp); err != nil {
		return nil, err
	}
	return resp.Dispatched, nil
}

// === 任务取消/查询/列表 ===

// Cancel 取消指定任务。
func (s *TasksService) Cancel(taskID, agentID string) error {
	body := map[string]string{"task_id": taskID, "agent_id": agentID}
	return s.c.post("/api/tasks/cancel", body, nil)
}

// Get 查询单个任务详情 + 结果。
func (s *TasksService) Get(taskID string) (*proto.TaskResult, string, error) {
	var resp struct {
		Task *struct {
			Status string `json:"status"`
		} `json:"task"`
		Result *proto.TaskResult `json:"result"`
	}
	if err := s.c.get("/api/tasks/"+url.PathEscape(taskID), &resp); err != nil {
		return nil, "", err
	}
	status := ""
	if resp.Task != nil {
		status = resp.Task.Status
	}
	return resp.Result, status, nil
}

// List 列出任务历史。
func (s *TasksService) List(limit int) ([]map[string]any, error) {
	if limit <= 0 {
		limit = 20
	}
	var resp struct {
		Tasks []map[string]any `json:"tasks"`
	}
	if err := s.c.get(fmt.Sprintf("/api/tasks?limit=%d", limit), &resp); err != nil {
		return nil, err
	}
	return resp.Tasks, nil
}

// === Shell 会话 ===

// ShellService 交互式终端。
type ShellService struct{ c *Client }

// Shell 返回 Shell 服务。
func (c *Client) Shell() *ShellService { return &ShellService{c: c} }

// ShellSession 表示一个打开的 shell 会话。
type ShellSession struct {
	SessionID string
	AgentID   string
	client    *Client
}

// Open 打开 shell 会话,返回会话句柄。
func (s *ShellService) Open(agentID string, cols, rows int) (*ShellSession, error) {
	body := map[string]any{"agent_id": agentID, "cols": cols, "rows": rows}
	var resp struct {
		SessionID string `json:"session_id"`
	}
	if err := s.c.post("/api/shell/open", body, &resp); err != nil {
		return nil, err
	}
	return &ShellSession{SessionID: resp.SessionID, AgentID: agentID, client: s.c}, nil
}

// Send 发送输入到 shell 会话。
func (s *ShellSession) Send(data string) error {
	body := map[string]string{
		"session_id": s.SessionID,
		"agent_id":   s.AgentID,
		"data_b64":   base64.StdEncoding.EncodeToString([]byte(data)),
	}
	return s.client.post("/api/shell/input", body, nil)
}

// Close 关闭 shell 会话。
func (s *ShellSession) Close() error {
	body := map[string]string{"session_id": s.SessionID, "agent_id": s.AgentID}
	return s.client.post("/api/shell/close", body, nil)
}

// === 电源操作 ===

// PowerService 电源管理。
type PowerService struct{ c *Client }

// Power 返回电源服务。
func (c *Client) Power() *PowerService { return &PowerService{c: c} }

// Shutdown 关机(Agent 需带 power_allowed=true 标签)。
func (s *PowerService) Shutdown(agentID string, delay int) error {
	return s.exec(agentID, "shutdown", delay)
}

// Restart 重启。
func (s *PowerService) Restart(agentID string, delay int) error {
	return s.exec(agentID, "restart", delay)
}

// Sleep 休眠。
func (s *PowerService) Sleep(agentID string) error {
	return s.exec(agentID, "sleep", 0)
}

func (s *PowerService) exec(agentID, op string, delay int) error {
	body := map[string]any{"agent_id": agentID, "op": op, "delay": delay}
	return s.c.post("/api/power", body, nil)
}

// === 定时任务 ===

// CronService 定时任务管理。
type CronService struct{ c *Client }

// Cron 返回定时任务服务。
func (c *Client) Cron() *CronService { return &CronService{c: c} }

// CronRule 定时规则。
type CronRule struct {
	ID       string          `json:"id"`
	CronExpr string          `json:"cron_expr"`
	TaskType string          `json:"task_type"`
	AgentID  string          `json:"agent_id"`
	Payload  json.RawMessage `json:"payload"`
	Timeout  int             `json:"timeout"`
}

// List 列出定时规则。
func (s *CronService) List() ([]CronRule, error) {
	var resp struct {
		Crons []CronRule `json:"crons"`
	}
	if err := s.c.get("/api/crons", &resp); err != nil {
		return nil, err
	}
	return resp.Crons, nil
}

// Add 添加定时规则。
func (s *CronService) Add(agentID, cronExpr, taskType string, payload json.RawMessage, timeout int) (*CronRule, error) {
	rule := CronRule{CronExpr: cronExpr, TaskType: taskType, AgentID: agentID, Payload: payload, Timeout: timeout}
	var created CronRule
	if err := s.c.post("/api/crons", rule, &created); err != nil {
		return nil, err
	}
	return &created, nil
}

// Delete 删除定时规则。
func (s *CronService) Delete(ruleID string) error {
	u := s.c.baseURL + "/api/crons/" + url.PathEscape(ruleID)
	req, _ := http.NewRequest("DELETE", u, nil)
	if s.c.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.c.token)
	}
	resp, err := s.c.http.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("DELETE cron: HTTP %d", resp.StatusCode)
	}
	return nil
}

// === Secret 管理 ===

// SecretService 凭据管理。
type SecretService struct{ c *Client }

// Secrets 返回 Secret 服务。
func (c *Client) Secrets() *SecretService { return &SecretService{c: c} }

// Put 存储凭据(TTL+一次性),返回 ref。
func (s *SecretService) Put(value string, ttlSec int, oneTime bool) (string, error) {
	body := map[string]any{"value": value, "ttl": ttlSec, "one_time": oneTime}
	var resp struct {
		Ref string `json:"ref"`
	}
	if err := s.c.post("/api/secrets", body, &resp); err != nil {
		return "", err
	}
	return resp.Ref, nil
}

// Get 拉取凭据值。
func (s *SecretService) Get(ref string) (string, error) {
	var resp struct {
		Value string `json:"value"`
	}
	if err := s.c.get("/api/secrets/"+url.PathEscape(ref), &resp); err != nil {
		return "", err
	}
	return resp.Value, nil
}

// Delete 删除凭据。
func (s *SecretService) Delete(ref string) error {
	u := s.c.baseURL + "/api/secrets/" + url.PathEscape(ref)
	req, _ := http.NewRequest("DELETE", u, nil)
	if s.c.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.c.token)
	}
	resp, err := s.c.http.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// === 系统信息(AI Agent 最常用)===

// Sysinfo 获取远端系统体检(结构化 JSON)。
func (c *Client) Sysinfo(agentID string) (map[string]any, error) {
	cmd := `$ErrorActionPreference='SilentlyContinue';$r=@{hostname=$env:COMPUTERNAME;os=(Get-CimInstance Win32_OperatingSystem).Caption;cpu=[math]::Round((Get-Counter '\Processor(_Total)\% Processor Time').CounterSamples.CookedValue,1);memTotal=[int]((Get-CimInstance Win32_OperatingSystem).TotalVisibleMemorySize/1024);memFree=[int]((Get-CimInstance Win32_OperatingSystem).FreePhysicalMemory/1024)};$r|ConvertTo-Json -Compress`
	payload, _ := json.Marshal(map[string]string{"command": cmd})
	result, err := c.Tasks().Run(agentID, "powershell", payload, 15)
	if err != nil {
		return nil, err
	}
	if result == nil || !result.Success || result.Stdout == "" {
		return nil, fmt.Errorf("sysinfo 失败")
	}
	var info map[string]any
	if json.Unmarshal([]byte(result.Stdout), &info) != nil {
		return map[string]any{"raw": result.Stdout}, nil
	}
	return info, nil
}

// RunCommand 在指定 Agent 上执行命令并返回 stdout(快捷方式)。
func (c *Client) RunCommand(agentID, command string, timeout int) (string, error) {
	payload, _ := json.Marshal(map[string]string{"command": command})
	result, err := c.Tasks().Run(agentID, "powershell", payload, timeout)
	if err != nil {
		return "", err
	}
	if result == nil {
		return "", fmt.Errorf("无结果")
	}
	if !result.Success {
		return result.Stderr, fmt.Errorf("exit %d: %s", result.ExitCode, result.Error)
	}
	return result.Stdout, nil
}

// === 工具发现(供 AI Agent 自动生成 tool 定义)===

// ToolInfo 工具描述(AI Agent 可读)。
type ToolInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Method      string `json:"method"`
	Path        string `json:"path"`
	Category    string `json:"category"`
}

// ListTools 获取可用工具清单(GET /api/tools)。
// AI Agent 用此发现 go-rmm 的全部能力并生成 function calling tool 定义。
func (c *Client) ListTools() ([]ToolInfo, error) {
	var resp struct {
		Tools []ToolInfo `json:"tools"`
		Count int        `json:"count"`
	}
	if err := c.get("/api/tools", &resp); err != nil {
		return nil, err
	}
	return resp.Tools, nil
}

// === Agent 管理 ===

// RevokeAgent 撤销 Agent 证书。
func (s *AgentsService) Revoke(agentID string) error {
	body := map[string]string{"agent_id": agentID}
	return s.c.post("/api/agents/revoke", body, nil)
}

// EnrollToken 生成一次性注册令牌。
func (s *AgentsService) EnrollToken(agentID string) (string, error) {
	body := map[string]string{"agent_id": agentID}
	var resp struct {
		Token string `json:"token"`
	}
	if err := s.c.post("/api/enroll-token", body, &resp); err != nil {
		return "", err
	}
	return resp.Token, nil
}
