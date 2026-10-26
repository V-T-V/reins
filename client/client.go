// Package client 提供 reins Relay API 的 Go SDK(L3 神级提升)。
//
// 第三方程序(运维平台/CMDB/监控系统)可直接 import 调用,无需手抄 HTTP。
//
// 用法:
//
//	c := client.New("http://relay:7780", "your-token")
//	agents, _ := c.Agents.List()
//	result, _ := c.Tasks.Run("agent-1", "powershell", `{"command":"Get-Date"}`)
package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/V-T-V/reins/proto"
)

// Client reins Relay API 客户端。
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// New 构造客户端。
func New(baseURL, token string) *Client {
	return &Client{
		baseURL: baseURL,
		token:   token,
		http: &http.Client{
			Timeout: 60 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        10,
				MaxIdleConnsPerHost: 5,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

// === Agents API ===

// AgentsService Agent 相关操作。
type AgentsService struct{ c *Client }

// Agents 返回 Agents 服务。
func (c *Client) Agents() *AgentsService { return &AgentsService{c: c} }

// List 列出在线 Agent。
func (s *AgentsService) List() ([]proto.Register, error) {
	var resp struct {
		Agents []proto.Register `json:"agents"`
		Count  int              `json:"count"`
	}
	if err := s.c.get("/api/agents", &resp); err != nil {
		return nil, err
	}
	return resp.Agents, nil
}

// === Tasks API ===

// TasksService 任务相关操作。
type TasksService struct{ c *Client }

// Tasks 返回 Tasks 服务。
func (c *Client) Tasks() *TasksService { return &TasksService{c: c} }

// Run 下发任务并等待结果。
func (s *TasksService) Run(agentID, taskType string, payload json.RawMessage, timeout int) (*proto.TaskResult, error) {
	taskID := fmt.Sprintf("sdk-%d", time.Now().UnixNano())
	body := proto.TaskRequest{
		TaskID:  taskID,
		Type:    taskType,
		AgentID: agentID,
		Payload: payload,
		Timeout: timeout,
	}
	var resp struct {
		TaskID string `json:"task_id"`
		Status string `json:"status"`
	}
	if err := s.c.post("/api/tasks", body, &resp); err != nil {
		return nil, err
	}
	// 轮询等结果
	deadline := time.Now().Add(time.Duration(timeout+10) * time.Second)
	for time.Now().Before(deadline) {
		var r struct {
			Task *struct {
				Status string `json:"status"`
			} `json:"task"`
			Result *proto.TaskResult `json:"result"`
		}
		if err := s.c.get("/api/tasks/"+url.PathEscape(taskID), &r); err == nil && r.Task != nil {
			if r.Task.Status == "completed" || r.Task.Status == "failed" {
				return r.Result, nil
			}
		}
		time.Sleep(time.Second)
	}
	return nil, fmt.Errorf("任务超时: %s", taskID)
}

// === File API ===

// FileService 文件传输。
type FileService struct{ c *Client }

// Files 返回 Files 服务。
func (c *Client) Files() *FileService { return &FileService{c: c} }

// Upload 上传文件(reader → Agent 远端路径)。
func (s *FileService) Upload(agentID, remotePath string, reader io.Reader) error {
	q := url.Values{}
	q.Set("agent_id", agentID)
	q.Set("path", remotePath)
	q.Set("transfer_id", fmt.Sprintf("sdk-%d", time.Now().UnixNano()))
	u := s.c.baseURL + "/api/transfer/upload?" + q.Encode()
	req, _ := http.NewRequest("POST", u, reader)
	if s.c.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.c.token)
	}
	resp, err := s.c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("上传失败: HTTP %d", resp.StatusCode)
	}
	return nil
}

// === 底层 HTTP ===

func (c *Client) get(path string, v any) error {
	req, _ := http.NewRequest("GET", c.baseURL+path, nil)
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("GET %s: HTTP %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

func (c *Client) post(path string, body any, v any) error {
	data, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", c.baseURL+path, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("POST %s: HTTP %d", path, resp.StatusCode)
	}
	if v != nil {
		return json.NewDecoder(resp.Body).Decode(v)
	}
	return nil
}

// === Status ===

// Status Relay 状态摘要。
type Status struct {
	Relay   string `json:"relay"`
	Store   string `json:"store"`
	Agents  int    `json:"agents"`
	UptimeS int    `json:"uptime_s"`
}

// GetStatus 获取 Relay 状态(免 token)。
func (c *Client) GetStatus() (*Status, error) {
	var s Status
	if err := c.get("/healthz", &s); err != nil {
		return nil, err
	}
	return &s, nil
}
