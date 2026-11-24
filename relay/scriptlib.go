// Package main — 脚本库管理(F8 扩展)。
//
// 在 Relay 侧存储常用运维脚本,CLI 按名称引用执行。
// 避免每次都要 -s ./script.ps1 手传文件。
//
// 存储:data/scripts/<name>.ps1 (或 .sh/.bat)
// 管理通过 SecretStore 模式(文件系统 + metadata)。
//
// 2026 租户加固:脚本按**命名空间**隔离——
//
//	data/scripts/<name>            admin 域(既有布局不变,旧脚本原地可用)
//	data/scripts/<tenant>/<name>   租户 <tenant> 的私有脚本
//
// 脚本常内嵌口令与部署参数,原来是全局共享的:任一租户都能读、能删、
// 还能在自己的 Agent 上运行别的租户上传的脚本(等于拿到对方的脚本内容)。
// 租户身份被固定在本租户命名空间;admin 默认在根命名空间,可用 ?tenant=<id>
// 进入某租户的命名空间查看/管理。脚本名禁止含路径分隔符,租户无法借名字越出自己的目录。
//
// API:
//
//	POST   /api/scripts          上传脚本(name + content)
//	GET    /api/scripts          列出脚本
//	GET    /api/scripts/{name}   读取脚本内容
//	DELETE /api/scripts/{name}   删除脚本
//	POST   /api/scripts/{name}/run  在指定 Agent 上运行脚本
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/V-T-V/reins/proto"
)

// ScriptLibrary 脚本库(文件系统态)。
type ScriptLibrary struct {
	dir string // data/scripts/
}

// scopeDir 返回命名空间对应的目录。scope 为空 = 根目录(admin 域);
// 否则必须是合法租户 ID(单一路径段),目录为 data/scripts/<scope>/。
func (lib *ScriptLibrary) scopeDir(scope string) (string, bool) {
	if scope == "" {
		return lib.dir, true
	}
	if !validTenantID(scope) {
		return "", false
	}
	return filepath.Join(lib.dir, scope), true
}

// validateScriptNameIn 校验脚本名相对目录 dir 是否安全:禁止路径穿越(../, 绝对路径,
// 分隔符),并二次确认解析后路径仍在该目录内(纵深防御,防清理绕过)。
// 返回安全时 (cleanedName, true),危险时 ("", false)。
//
// 背景:GET/DELETE /api/scripts/{name} 原先未校验 name,
// /api/scripts/../../etc/passwd 经 filepath.Join 清理后逃逸出脚本目录,
// 可读取/删除脚本目录外任意文件(CVE 级路径穿越)。
func validateScriptNameIn(dir, name string) (string, bool) {
	if dir == "" || name == "" {
		return "", false
	}
	// 第一道:字符级黑名单(快速拒绝明显的穿越尝试)。
	if strings.Contains(name, "..") || strings.ContainsAny(name, `/\`) {
		return "", false
	}
	// 第二道:解析后路径必须严格位于脚本目录内(防清理绕过,如 URL 编码/符号链接)。
	cleaned := filepath.Clean(filepath.Join(dir, name))
	absDir, err1 := filepath.Abs(dir)
	resolvedDir, err2 := filepath.Abs(cleaned)
	if err1 != nil || err2 != nil {
		return "", false
	}
	if !strings.HasPrefix(resolvedDir, absDir+string(filepath.Separator)) && resolvedDir != absDir {
		return "", false
	}
	return name, true
}

// validateScriptName 根命名空间(admin 域)的名字校验(既有调用方与测试沿用)。
func (lib *ScriptLibrary) validateScriptName(name string) (string, bool) {
	if lib == nil {
		return "", false
	}
	return validateScriptNameIn(lib.dir, name)
}

// NewScriptLibrary 构造。
func NewScriptLibrary(dataDir string) (*ScriptLibrary, error) {
	dir := filepath.Join(dataDir, "scripts")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	return &ScriptLibrary{dir: dir}, nil
}

// ScriptInfo 脚本元信息。
type ScriptInfo struct {
	Name       string `json:"name"`
	Lang       string `json:"lang"` // ps1/sh/bat
	Size       int64  `json:"size"`
	CreatedAt  int64  `json:"created_at"`
	ModifiedAt int64  `json:"modified_at"`
}

// List 列出根命名空间(admin 域)的全部脚本。
func (lib *ScriptLibrary) List() []ScriptInfo {
	return lib.ListIn("")
}

// ListIn 列出指定命名空间的全部脚本(子目录即其它命名空间,不计入)。
func (lib *ScriptLibrary) ListIn(scope string) []ScriptInfo {
	dir, ok := lib.scopeDir(scope)
	if !ok {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var infos []ScriptInfo
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, _ := e.Info()
		name := e.Name()
		lang := "ps1"
		if strings.HasSuffix(name, ".sh") {
			lang = "sh"
		} else if strings.HasSuffix(name, ".bat") {
			lang = "bat"
		}
		infos = append(infos, ScriptInfo{
			Name: name, Lang: lang, Size: info.Size(),
			CreatedAt:  info.ModTime().UnixMilli(),
			ModifiedAt: info.ModTime().UnixMilli(),
		})
	}
	return infos
}

// Save 保存脚本到根命名空间(admin 域)。
func (lib *ScriptLibrary) Save(name, content string) error {
	return lib.SaveIn("", name, content)
}

// SaveIn 保存脚本到指定命名空间。
// 安全校验:name 经 validateScriptNameIn,禁止路径穿越(纵深防御,防绕过 handler)。
func (lib *ScriptLibrary) SaveIn(scope, name, content string) error {
	dir, ok := lib.scopeDir(scope)
	if !ok {
		return fmt.Errorf("脚本命名空间非法")
	}
	if _, ok := validateScriptNameIn(dir, name); !ok {
		return fmt.Errorf("name 非法(路径穿越或为空)")
	}
	if scope != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
	}
	path := filepath.Join(dir, name)
	return os.WriteFile(path, []byte(content), 0644)
}

// Read 读取根命名空间(admin 域)的脚本内容。
func (lib *ScriptLibrary) Read(name string) (string, error) {
	return lib.ReadIn("", name)
}

// ReadIn 读取指定命名空间的脚本内容。
// 安全校验:name 经 validateScriptNameIn,禁止路径穿越(防读到脚本目录外文件)。
func (lib *ScriptLibrary) ReadIn(scope, name string) (string, error) {
	dir, ok := lib.scopeDir(scope)
	if !ok {
		return "", fmt.Errorf("脚本命名空间非法")
	}
	if _, ok := validateScriptNameIn(dir, name); !ok {
		return "", fmt.Errorf("name 非法(路径穿越或为空)")
	}
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// Delete 删除根命名空间(admin 域)的脚本。
func (lib *ScriptLibrary) Delete(name string) error {
	return lib.DeleteIn("", name)
}

// DeleteIn 删除指定命名空间的脚本。
// 安全校验:name 经 validateScriptNameIn,禁止路径穿越(防删脚本目录外文件)。
func (lib *ScriptLibrary) DeleteIn(scope, name string) error {
	dir, ok := lib.scopeDir(scope)
	if !ok {
		return fmt.Errorf("脚本命名空间非法")
	}
	if _, ok := validateScriptNameIn(dir, name); !ok {
		return fmt.Errorf("name 非法(路径穿越或为空)")
	}
	return os.Remove(filepath.Join(dir, name))
}

// 全局实例(在 main.go 初始化)。
var scriptLib *ScriptLibrary

// scriptScope 解析本次请求的脚本库命名空间(2026 租户加固)。
// 租户身份固定在本租户命名空间——?tenant= 参数对它无效,不能进入别人的目录;
// admin 默认根命名空间(admin 域),可用 ?tenant=<id> 进入某租户的命名空间查看/管理。
func (r *Relay) scriptScope(req *http.Request, id Identity) (string, bool) {
	scope := id.Tenant
	if scope == "" {
		scope = req.URL.Query().Get("tenant")
	}
	return scope, scope == "" || validTenantID(scope)
}

// handleScriptList GET /api/scripts — 列出脚本库(调用者可见的命名空间)。
func (r *Relay) handleScriptList(w http.ResponseWriter, req *http.Request) {
	_, id, err := r.authenticateCtx(req)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, errMap(err))
		return
	}
	if scriptLib == nil {
		writeJSON(w, http.StatusOK, map[string]any{"scripts": []any{}, "enabled": false})
		return
	}
	scope, ok := r.scriptScope(req, id)
	if !ok {
		writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("tenant 参数非法")))
		return
	}
	scripts := scriptLib.ListIn(scope)
	writeJSON(w, http.StatusOK, map[string]any{"scripts": scripts, "count": len(scripts), "enabled": true})
}

// handleScriptUpload POST /api/scripts — 上传脚本(落到调用者的命名空间)。
func (r *Relay) handleScriptUpload(w http.ResponseWriter, req *http.Request) {
	req, id, err := r.authenticateCtx(req)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, errMap(err))
		return
	}
	if scriptLib == nil {
		writeJSON(w, http.StatusServiceUnavailable, errMap(fmt.Errorf("脚本库未启用")))
		return
	}
	var body struct {
		Name    string `json:"name"`
		Content string `json:"content"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, errMap(err))
		return
	}
	if body.Name == "" || body.Content == "" {
		writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("name 和 content 不能为空")))
		return
	}
	scope, ok := r.scriptScope(req, id)
	if !ok {
		writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("tenant 参数非法")))
		return
	}
	// 安全:禁止路径穿越(与 get/delete/run 共用校验器)
	if _, ok := scriptLib.validateScriptName(body.Name); !ok {
		writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("name 含非法字符")))
		return
	}
	if err := scriptLib.SaveIn(scope, body.Name, body.Content); err != nil {
		writeJSON(w, http.StatusInternalServerError, errMap(err))
		return
	}
	if r.audit != nil {
		r.audit.Log(AuditEntry{Event: "script_uploaded", Operator: id.auditLabel(), Detail: body.Name, Tenant: id.Tenant})
	}
	writeJSON(w, http.StatusCreated, map[string]any{"name": body.Name, "size": len(body.Content)})
}

// handleScriptGet GET /api/scripts/{name} — 读取脚本。
func (r *Relay) handleScriptGet(w http.ResponseWriter, req *http.Request) {
	_, id, err := r.authenticateCtx(req)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, errMap(err))
		return
	}
	name := strings.TrimPrefix(req.URL.Path, "/api/scripts/")
	if scriptLib == nil {
		writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("脚本库未启用")))
		return
	}
	scope, ok := r.scriptScope(req, id)
	if !ok {
		writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("tenant 参数非法")))
		return
	}
	dir, _ := scriptLib.scopeDir(scope)
	if _, ok := validateScriptNameIn(dir, name); !ok {
		writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("缺少 name 或含非法字符")))
		return
	}
	content, err := scriptLib.ReadIn(scope, name)
	if err != nil {
		writeJSON(w, http.StatusNotFound, errMap(fmt.Errorf("脚本不存在: %s", name)))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "content": content})
}

// handleScriptDelete DELETE /api/scripts/{name} — 删除脚本。
func (r *Relay) handleScriptDelete(w http.ResponseWriter, req *http.Request) {
	_, id, err := r.authenticateCtx(req)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, errMap(err))
		return
	}
	name := strings.TrimPrefix(req.URL.Path, "/api/scripts/")
	if scriptLib == nil {
		writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("脚本库未启用")))
		return
	}
	scope, ok := r.scriptScope(req, id)
	if !ok {
		writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("tenant 参数非法")))
		return
	}
	dir, _ := scriptLib.scopeDir(scope)
	if _, ok := validateScriptNameIn(dir, name); !ok {
		writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("缺少 name 或含非法字符")))
		return
	}
	if err := scriptLib.DeleteIn(scope, name); err != nil {
		writeJSON(w, http.StatusNotFound, errMap(fmt.Errorf("脚本不存在")))
		return
	}
	if r.audit != nil {
		r.audit.Log(AuditEntry{Event: "script_deleted", Operator: id.auditLabel(), Detail: name, Tenant: id.Tenant})
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "deleted": true})
}

// handleScriptRun POST /api/scripts/{name}/run — 在指定 Agent 上运行库中脚本。
// 脚本取自调用者的命名空间;目标 Agent 必须归属调用者租户;派生任务继承租户。
func (r *Relay) handleScriptRun(w http.ResponseWriter, req *http.Request) {
	req, id, err := r.authenticateCtx(req)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, errMap(err))
		return
	}
	// 解析 path: /api/scripts/{name}/run
	path := strings.TrimPrefix(req.URL.Path, "/api/scripts/")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) != 2 || parts[1] != "run" {
		writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("路径格式错误")))
		return
	}
	name := parts[0]
	if _, ok := scriptLib.validateScriptName(name); !ok {
		writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("脚本名非法或脚本库未启用")))
		return
	}
	scope, ok := r.scriptScope(req, id)
	if !ok {
		writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("tenant 参数非法")))
		return
	}
	content, err := scriptLib.ReadIn(scope, name)
	if err != nil {
		writeJSON(w, http.StatusNotFound, errMap(fmt.Errorf("脚本不存在: %s", name)))
		return
	}
	var body struct {
		AgentID string `json:"agent_id"`
		Timeout int    `json:"timeout"`
	}
	json.NewDecoder(req.Body).Decode(&body)
	if body.AgentID == "" {
		writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("agent_id 不能为空")))
		return
	}
	if !r.requireAgentAccess(w, id, body.AgentID) { // 2026 租户加固:脚本执行是动作
		return
	}
	if body.Timeout == 0 {
		body.Timeout = 60
	}

	// 确定语言
	lang := "ps1"
	if strings.HasSuffix(name, ".sh") {
		lang = "bash"
	} else if strings.HasSuffix(name, ".bat") {
		lang = "bat"
	}

	// 下发 script 任务
	conn := r.agents.Conn(body.AgentID)
	if conn == nil {
		writeJSON(w, http.StatusNotFound, errMap(fmt.Errorf("agent %s 不在线", body.AgentID)))
		return
	}
	taskID := "scriptrun-" + fmt.Sprint(time.Now().UnixNano())
	payload, _ := json.Marshal(proto.ScriptTask{Lang: lang, Content: content})
	task := proto.TaskRequest{
		TaskID: taskID, Type: proto.TaskScript, AgentID: body.AgentID,
		Payload: payload, Timeout: body.Timeout,
	}
	if r.store != nil {
		now := time.Now().UnixMilli()
		r.store.SaveTask(StoredTask{Request: task, Status: StatusPending, CreatedAt: now, UpdatedAt: now, Submitted: id.auditLabel(), Tenant: id.Tenant})
	}
	if err := r.dispatchTask(conn, task); err != nil {
		writeJSON(w, http.StatusBadGateway, errMap(fmt.Errorf("下发失败: %w", err)))
		return
	}
	if r.audit != nil {
		r.audit.Log(AuditEntry{Event: "script_run", AgentID: body.AgentID, TaskID: taskID, Operator: id.auditLabel(), Detail: name, Tenant: id.Tenant})
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"script":   name,
		"task_id":  taskID,
		"agent_id": body.AgentID,
	})
}
