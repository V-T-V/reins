package client

// === 版本仓库与 Agent 更新(v4.7.1+ 端点的 SDK 覆盖) ===
//
// 此前 SDK 有 Batch/Shell/Power/Cron/Secrets 五服务,唯独没有 Update——
// 第三方消费者无法驱动自动更新链路(列表/最新/推送/下载)。

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// UpdateService 版本仓库与 Agent 更新。
type UpdateService struct{ c *Client }

// Updates 返回 Update 服务。
func (c *Client) Updates() *UpdateService { return &UpdateService{c: c} }

// UpdateVersion 仓库中的一个版本条目。
type UpdateVersion struct {
	Version   string `json:"version"`
	SHA256    string `json:"sha256"`
	Size      int64  `json:"size"`
	CreatedAt int64  `json:"created_at"`
}

// RelayVersion Relay 版本信息(/api/version,部署漂移观测)。
type RelayVersion struct {
	Version   string `json:"version"`
	Protocol  int    `json:"protocol"`
	Component string `json:"component"`
}

// Version 查询 Relay 版本(部署漂移一眼可辨)。
func (c *Client) Version() (*RelayVersion, error) {
	var v RelayVersion
	if err := c.get("/api/version", &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// List 列出仓库全部版本(降序,最新在前)。
func (s *UpdateService) List() ([]UpdateVersion, error) {
	var resp struct {
		Versions []UpdateVersion `json:"versions"`
		Enabled  bool            `json:"enabled"`
	}
	if err := s.c.get("/api/updates", &resp); err != nil {
		return nil, err
	}
	return resp.Versions, nil
}

// Latest 查询最新版本(Agent auto_update 同款轮询端点)。
// 空仓库返回 (nil, nil)。
func (s *UpdateService) Latest() (*UpdateVersion, error) {
	var resp struct {
		Enabled bool           `json:"enabled"`
		Latest  *UpdateVersion `json:"latest"`
	}
	if err := s.c.get("/api/updates/latest", &resp); err != nil {
		return nil, err
	}
	return resp.Latest, nil
}

// UpdateApplyResult push 的受理回执。
type UpdateApplyResult struct {
	AgentID    string `json:"agent_id"`
	TaskID     string `json:"task_id"`
	Version    string `json:"version"`
	SHA256     string `json:"sha256"`
	OldVersion string `json:"old_version"`
	BinarySize int64  `json:"binary_size"`
	Message    string `json:"message"`
}

// Push 触发指定 Agent 更新到目标版本。
// 模式与 CLI 一致:默认 download(Agent 自取,须有 update_dl 能力;
// 旧 Agent 由 Relay 自动降级内联)。binaryPath 非空时改走引用模式。
func (s *UpdateService) Push(agentID, version, binaryPath string) (*UpdateApplyResult, error) {
	body := map[string]any{
		"agent_id": agentID,
		"version":  version,
		"mode":     "download",
	}
	if binaryPath != "" {
		body["binary_path"] = binaryPath
		body["mode"] = "path"
	}
	var out UpdateApplyResult
	if err := s.c.post("/api/updates/apply", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DownloadStream 流式下载指定版本二进制(调用方负责 Close)。
// 响应头 X-Xfer-SHA256 可与 Latest().SHA256 对拍。
func (s *UpdateService) DownloadStream(version string) (io.ReadCloser, string, error) {
	u := s.c.baseURL + "/api/updates/download?version=" + url.QueryEscape(version)
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, "", err
	}
	if s.c.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.c.token)
	}
	resp, err := s.c.http.Do(req)
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, "", fmt.Errorf("download %s: HTTP %d", version, resp.StatusCode)
	}
	return resp.Body, resp.Header.Get("X-Xfer-SHA256"), nil
}
