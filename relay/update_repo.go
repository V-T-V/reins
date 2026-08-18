// Package main — Relay 版本仓库(Agent 二进制分发)。
//
// 管理 Agent 二进制版本:
//   - data/agent-binaries/<version>.exe + <version>.sha256
//   - GET /api/updates 列出可用版本
//   - POST /api/updates/upload 上传新版本
//   - POST /api/updates/apply 触发指定 Agent 更新(下发 update 任务)
//
// 仓库是文件系统态(单 Relay 部署),不持久化到 bbolt(版本文件本身就是持久化)。
package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/QiuShichang/go-rmm/proto"
)

// UpdateRepo Agent 二进制版本仓库。
type UpdateRepo struct {
	dir string // data/agent-binaries/
}

// NewUpdateRepo 构造仓库,确保目录存在。
func NewUpdateRepo(dataDir string) (*UpdateRepo, error) {
	dir := filepath.Join(dataDir, "agent-binaries")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	return &UpdateRepo{dir: dir}, nil
}

// VersionInfo 版本元信息。
type VersionInfo struct {
	Version   string `json:"version"`
	SHA256    string `json:"sha256"`
	Size      int64  `json:"size"`
	CreatedAt int64  `json:"created_at"`
}

// List 列出所有可用版本(按版本号降序)。
func (r *UpdateRepo) List() []VersionInfo {
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return nil
	}
	var infos []VersionInfo
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".exe") && !strings.HasSuffix(e.Name(), ".agent") {
			continue
		}
		version := strings.TrimSuffix(strings.TrimSuffix(e.Name(), ".exe"), ".agent")
		info, _ := e.Info()
		sha := r.readSHA(version)
		infos = append(infos, VersionInfo{
			Version:   version,
			SHA256:    sha,
			Size:      info.Size(),
			CreatedAt: info.ModTime().UnixMilli(),
		})
	}
	// 按版本降序(最新在前)
	sort.Slice(infos, func(i, j int) bool {
		return infos[i].Version > infos[j].Version
	})
	return infos
}

// readSHA 读版本对应的 .sha256 文件。
func (r *UpdateRepo) readSHA(version string) string {
	data, err := os.ReadFile(filepath.Join(r.dir, version+".sha256"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// Save 保存上传的二进制 + 计算 SHA256。
// 返回 (version, sha256, error)。
func (r *UpdateRepo) Save(version string, reader io.Reader) (string, string, error) {
	if version == "" {
		version = "v" + time.Now().Format("20060102-150405")
	}
	ext := ".exe"
	binPath := filepath.Join(r.dir, version+ext)
	shaPath := filepath.Join(r.dir, version+".sha256")

	// 流式写文件 + 算 hash
	f, err := os.Create(binPath)
	if err != nil {
		return "", "", err
	}
	defer f.Close()
	h := sha256.New()
	w := io.MultiWriter(f, h)
	if _, err := io.Copy(w, reader); err != nil {
		os.Remove(binPath)
		return "", "", err
	}
	sha := hex.EncodeToString(h.Sum(nil))
	os.WriteFile(shaPath, []byte(sha), 0644)
	return version, sha, nil
}

// BinPath 返回指定版本二进制的完整路径。
func (r *UpdateRepo) BinPath(version string) string {
	// 尝试 .exe 和 .agent
	for _, ext := range []string{".exe", ".agent"} {
		p := filepath.Join(r.dir, version+ext)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// handleUpdateList GET /api/updates —— 列出可用版本。
func (r *Relay) handleUpdateList(w http.ResponseWriter, req *http.Request) {
	if err := r.authorize(req); err != nil {
		writeJSON(w, http.StatusUnauthorized, errMap(err))
		return
	}
	if r.updateRepo == nil {
		writeJSON(w, http.StatusOK, map[string]any{"versions": []any{}, "enabled": false})
		return
	}
	versions := r.updateRepo.List()
	writeJSON(w, http.StatusOK, map[string]any{"versions": versions, "count": len(versions), "enabled": true})
}

// handleUpdateUpload POST /api/updates/upload?version=v1.2.0 —— 上传新版本二进制。
// body = 原始二进制(stream-through 落盘 + SHA256)。
func (r *Relay) handleUpdateUpload(w http.ResponseWriter, req *http.Request) {
	if err := r.authorize(req); err != nil {
		writeJSON(w, http.StatusUnauthorized, errMap(err))
		return
	}
	if r.updateRepo == nil {
		writeJSON(w, http.StatusServiceUnavailable, errMap(fmt.Errorf("版本仓库未启用")))
		return
	}
	version := req.URL.Query().Get("version")
	savedVersion, sha, err := r.updateRepo.Save(version, req.Body)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errMap(fmt.Errorf("保存失败: %w", err)))
		return
	}
	if r.audit != nil {
		r.audit.Log(AuditEntry{Event: "agent_version_uploaded", Detail: savedVersion + " sha=" + sha[:16]})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version": savedVersion,
		"sha256":  sha,
		"message": "上传成功,用 update push 触发更新",
	})
}

// protoUpdateTaskInline update 任务载荷(与 proto.UpdateTask 字段对齐)。
// BinaryPath 引用模式适合任意大小(经 filetransfer 分片预上传);
// BinaryB64 内联仅适合中等大小(<20MB,WS 32MB 限制内)且链路低延迟。
type protoUpdateTaskInline struct {
	BinaryB64  string `json:"binary_b64,omitempty"` // base64 编码的二进制
	BinaryPath string `json:"binary_path,omitempty"` // Agent 侧已上传的新二进制路径
	SHA256     string `json:"sha256"`
	Version    string `json:"version"`
	MinVersion string `json:"min_version"`
	Force      bool   `json:"force"`
}

// handleUpdateApply POST /api/updates/apply —— 触发 Agent 更新。
// 请求体: {"agent_id":"...", "version":"v1.2.0", "force":false, "binary_path":"可选"}
// 二进制来源两种模式:
//   - binary_path(推荐): Agent 侧已预上传的新二进制路径(经 file upload 分片传输),
//     任务 payload 只带路径引用——内联 7MB+ 二进制单帧 WS 写在公网链路必然超时
//     (2026-08-18 实测:v4.5.1 推送连续 context deadline exceeded)。
//   - 默认: 从仓库读二进制 base64 内联(仅适合小体积/低延迟链路)。
func (r *Relay) handleUpdateApply(w http.ResponseWriter, req *http.Request) {
	if err := r.authorize(req); err != nil {
		writeJSON(w, http.StatusUnauthorized, errMap(err))
		return
	}
	if r.updateRepo == nil {
		writeJSON(w, http.StatusServiceUnavailable, errMap(fmt.Errorf("版本仓库未启用")))
		return
	}
	var body struct {
		AgentID string `json:"agent_id"`
		Version string `json:"version"`
		Force   bool   `json:"force"`
		// BinaryPath Agent 侧已预上传的新二进制绝对路径,与版本仓库 SHA 校验配套。
		BinaryPath string `json:"binary_path,omitempty"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, errMap(err))
		return
	}
	if body.AgentID == "" || body.Version == "" {
		writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("agent_id/version 不能为空")))
		return
	}

	binPath := r.updateRepo.BinPath(body.Version)
	if binPath == "" {
		writeJSON(w, http.StatusNotFound, errMap(fmt.Errorf("版本 %s 不在仓库", body.Version)))
		return
	}
	sha := r.updateRepo.readSHA(body.Version)

	// 找 Agent 连接
	conn := r.agents.Conn(body.AgentID)
	if conn == nil {
		writeJSON(w, http.StatusNotFound, errMap(fmt.Errorf("agent %s 不在线", body.AgentID)))
		return
	}

	agentVersion := r.getAgentVersion(body.AgentID)

	// 下发 update 任务:优先 BinaryPath 引用模式(零内联),否则回退 base64 内联。
	taskID := "update-" + body.AgentID + "-" + fmt.Sprint(time.Now().UnixNano())
	var updatePayload []byte
	binarySize := 0
	if body.BinaryPath != "" {
		updatePayload, _ = json.Marshal(protoUpdateTaskInline{
			BinaryPath: body.BinaryPath,
			SHA256:     sha,
			Version:    body.Version,
			MinVersion: agentVersion, // 防降级
			Force:      body.Force,
		})
	} else {
		binData, err := os.ReadFile(binPath)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, errMap(fmt.Errorf("读仓库二进制失败: %w", err)))
			return
		}
		binarySize = len(binData)
		updatePayload, _ = json.Marshal(protoUpdateTaskInline{
			BinaryB64:  base64.StdEncoding.EncodeToString(binData),
			SHA256:     sha,
			Version:    body.Version,
			MinVersion: agentVersion, // 防降级
			Force:      body.Force,
		})
	}
	task := proto.TaskRequest{
		TaskID:  taskID,
		Type:    proto.TaskUpdate,
		AgentID: body.AgentID,
		Payload: updatePayload,
		Timeout: 120, // 给 base64 解码 + 写文件留时间
	}
	if r.store != nil {
		now := time.Now().UnixMilli()
		r.store.SaveTask(StoredTask{Request: task, Status: StatusPending, CreatedAt: now, UpdatedAt: now})
	}
	if err := r.dispatchTask(conn, task); err != nil {
		writeJSON(w, http.StatusBadGateway, errMap(fmt.Errorf("下发 update 任务失败: %w", err)))
		return
	}
	if r.audit != nil {
		r.audit.Log(AuditEntry{Event: "agent_update_triggered", AgentID: body.AgentID, TaskID: taskID, Detail: body.Version})
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"agent_id":    body.AgentID,
		"task_id":     taskID,
		"version":     body.Version,
		"sha256":      sha,
		"old_version": agentVersion,
		"binary_size": binarySize,
		"message":     "更新任务已下发,Agent 将自更新并重启",
	})
}

// getAgentVersion 从 registry 取 Agent 当前版本。
func (r *Relay) getAgentVersion(agentID string) string {
	for _, a := range r.agents.Snapshot() {
		if a.AgentID == agentID {
			return a.Version
		}
	}
	return ""
}
