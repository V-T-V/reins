// CLI deploy 命令 —— playbook 多步编排(本地驱动)。
//
// 阶段 2:CLI 侧解析 playbook JSON,串行下发 steps,每步复用已有 CLI 函数。
// 适合在线编排(CLI 与 Agent 保持连接);离线可靠编排见后续 Agent WorkflowExecutor。
//
// playbook 格式(JSON,易于程序生成 + 无新依赖):
//
//	{
//	  "name": "deploy-myapp",
//	  "target": "win-prod-01",
//	  "target_tag": "env=prod",     // 与 target 二选一,tag 走批量
//	  "workspace": "C:\\apps\\myapp",
//	  "vars": {"port": "8080"},
//	  "steps": [
//	    {"name":"upload", "action":"upload", "src":"./app.exe", "dst":"app.exe"},
//	    {"name":"stop",   "action":"exec",   "cmd":"Stop-Service myapp"},
//	    {"name":"start",  "action":"exec",   "cmd":"Start-Process app.exe"},
//	    {"name":"verify", "action":"exec",   "cmd":"curl localhost:${port}/health", "retry":3, "delay":"2s"}
//	  ]
//	}
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"
)

// PlayBook 部署编排描述。
type PlayBook struct {
	Name      string            `json:"name"`
	Target    string            `json:"target,omitempty"`     // 单 agent ID
	TargetTag string            `json:"target_tag,omitempty"` // 或按标签批量(与 target 互斥)
	Workspace string            `json:"workspace,omitempty"`  // 覆盖 agent workspace
	Vars      map[string]string `json:"vars,omitempty"`
	Steps     []PlayStep        `json:"steps"`
}

// PlayStep 单个步骤。
type PlayStep struct {
	Name   string `json:"name"`
	Action string `json:"action"`           // upload / exec / script
	Src    string `json:"src,omitempty"`    // upload: 本地路径
	Dst    string `json:"dst,omitempty"`    // upload: 远端路径(相对 workspace)
	Cmd    string `json:"cmd,omitempty"`    // exec: powershell 命令
	Script string `json:"script,omitempty"` // script: 脚本文件路径
	Lang   string `json:"lang,omitempty"`   // script: ps1/bat
	// Timeout 步骤超时秒数(exec 默认 60/script 默认 120)。
	// npm install / 大包解压等长步骤必须显式调大——默认值会中途杀掉
	// 进程树(2026-08-31 loan-kiosk 部署会话实证:npm install 常超 60s)。
	Timeout int `json:"timeout,omitempty"`
	// 容错
	Retry int    `json:"retry,omitempty"` // 失败重试次数(默认 0)
	Delay string `json:"delay,omitempty"` // 重试间隔(默认 1s)
	// 条件
	When string `json:"when,omitempty"` // 条件表达式(简化:仅支持 "success" 判断上一步)
}

// deployRulesAck 本次 deploy run 解析出的项目规则 ack(所有步骤统一携带;
// tag 批量模式经 /api/tasks/batch 服务端构造,暂不携带——见 README 限制说明)。
var deployRulesAck string

// cmdDeploy deploy 子命令入口。
func cmdDeploy(cfg config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("用法: reins deploy <run|validate|init> <playbook.json>")
	}
	switch args[0] {
	case "run":
		return deployRun(cfg, args[1:])
	case "validate", "check":
		return deployValidate(cfg, args[1:])
	case "init":
		return deployInit(args[1:])
	default:
		return fmt.Errorf("未知 deploy 子命令: %s (可用: run validate init)", args[0])
	}
}

// deployValidate 只校验 playbook 不执行。
func deployValidate(cfg config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("用法: reins deploy validate <playbook.json>")
	}
	pb, err := loadPlaybook(args[0])
	if err != nil {
		return err
	}
	fmt.Printf("✅ playbook 校验通过\n")
	fmt.Printf("   名称: %s\n", pb.Name)
	if pb.Target != "" {
		fmt.Printf("   目标: %s\n", pb.Target)
	} else if pb.TargetTag != "" {
		fmt.Printf("   目标标签: %s\n", pb.TargetTag)
	}
	if pb.Workspace != "" {
		fmt.Printf("   workspace: %s\n", pb.Workspace)
	}
	fmt.Printf("   步骤: %d 步\n", len(pb.Steps))
	for i, s := range pb.Steps {
		fmt.Printf("     %d. [%s] %s", i+1, s.Action, s.Name)
		if s.Timeout > 0 {
			fmt.Printf(" (timeout=%ds)", s.Timeout)
		}
		if s.Retry > 0 {
			fmt.Printf(" (retry=%d)", s.Retry)
		}
		fmt.Println()
	}
	return nil
}

// deployRun 执行 playbook。
func deployRun(cfg config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("用法: reins deploy run <playbook.json> [--dry-run]")
	}
	path := args[0]
	dryRun := false
	for _, a := range args[1:] {
		if a == "--dry-run" || a == "-n" {
			dryRun = true
		}
	}
	pb, err := loadPlaybook(path)
	if err != nil {
		return err
	}
	if pb.Target == "" && pb.TargetTag == "" {
		return fmt.Errorf("playbook 缺少 target 或 target_tag")
	}

	fmt.Printf("🚀 部署: %s (dry-run=%v)\n", pb.Name, dryRun)
	target := pb.Target
	if target == "" {
		target = "tag:" + pb.TargetTag
	}
	fmt.Printf("   目标: %s\n", target)

	// 部署是最高风险的变更操作:开始前加载项目规则(本地 .reins/rules.md
	// 打印全文进上下文,或 rules read 缓存 ack),后续所有步骤统一携带。
	deployRulesAck = ""
	if !dryRun {
		deployRulesAck = resolveRulesAck(pb.Target, "", true)
	}

	// 变量合并:CLI 默认 vars + playbook vars
	vars := map[string]string{}
	for k, v := range pb.Vars {
		vars[k] = v
	}
	if pb.Workspace != "" {
		vars["workspace"] = pb.Workspace
	}

	// 串行执行步骤
	startTime := time.Now()
	for i, step := range pb.Steps {
		stepNum := i + 1
		// 变量替换
		cmd := substituteVars(step.Cmd, vars)
		script := substituteVars(step.Script, vars)
		dst := substituteVars(step.Dst, vars)

		fmt.Printf("\n[%d/%d] %s (%s)\n", stepNum, len(pb.Steps), step.Name, step.Action)
		if dryRun {
			switch step.Action {
			case "upload", "file":
				fmt.Printf("  (dry-run) upload %s → %s\n", step.Src, dst)
			case "exec", "run":
				fmt.Printf("  (dry-run) exec: %s\n", cmd)
			case "script":
				fmt.Printf("  (dry-run) script %s (%s)\n", script, step.Lang)
			}
			continue
		}

		// 真实执行
		var stepErr error
		maxAttempt := step.Retry + 1
		delay := parseDelay(step.Delay)
		for attempt := 1; attempt <= maxAttempt; attempt++ {
			if attempt > 1 {
				fmt.Printf("  重试 %d/%d (等待 %v)...\n", attempt-1, step.Retry, delay)
				time.Sleep(delay)
			}
			stepErr = execPlayStep(cfg, *pb, step, cmd, script, dst)
			if stepErr == nil {
				break
			}
			if attempt < maxAttempt {
				fmt.Fprintf(os.Stderr, "  ⚠️ 步骤失败: %v\n", stepErr)
			}
		}
		if stepErr != nil {
			fmt.Fprintf(os.Stderr, "\n❌ 步骤 [%s] 失败,部署中止: %v\n", step.Name, stepErr)
			return errTaskFailed(fmt.Sprintf("步骤 %s 失败: %v", step.Name, stepErr))
		}
		fmt.Printf("  ✅ 完成\n")
	}

	fmt.Printf("\n🎉 部署完成: %s (耗时 %v)\n", pb.Name, time.Since(startTime).Round(time.Second))
	return nil
}

// execPlayStep 执行单个步骤(复用已有 CLI 函数)。
func execPlayStep(cfg config, pb PlayBook, step PlayStep, cmd, script, dst string) error {
	target := pb.Target
	switch step.Action {
	case "upload", "file":
		if step.Src == "" || dst == "" {
			return fmt.Errorf("upload 步骤缺少 src/dst")
		}
		// dst 相对 workspace 时,拼绝对路径(Agent 侧 FilePolicy 校验)
		remotePath := dst
		if pb.Workspace != "" && !filepathIsAbs(dst) {
			remotePath = joinPath(pb.Workspace, dst)
		}
		return runFileUpload(cfg, target, step.Src, remotePath)
	case "exec", "run":
		if cmd == "" {
			return fmt.Errorf("exec 步骤缺少 cmd")
		}
		return runTaskSync(cfg, target, "powershell", cmd, stepTimeout(step, 60))
	case "script":
		if script == "" {
			return fmt.Errorf("script 步骤缺少 script")
		}
		lang := step.Lang
		if lang == "" {
			lang = "ps1"
		}
		return runTaskSyncScript(cfg, target, script, lang, stepTimeout(step, 120))
	default:
		return fmt.Errorf("未知 action: %s", step.Action)
	}
}

// stepTimeout 解析步骤超时:显式配置优先,否则用默认值。
func stepTimeout(step PlayStep, def int) int {
	if step.Timeout > 0 {
		return step.Timeout
	}
	return def
}

// runTaskSync 下发 powershell 命令并等待完成,失败返回 error。
func runTaskSync(cfg config, agentID, taskType, cmd string, timeout int) error {
	if agentID == "" {
		return fmt.Errorf("target_tag 批量部署暂不支持 exec 步骤(需单 target)")
	}
	payload, err := json.Marshal(struct {
		Command string `json:"command"`
	}{Command: cmd})
	if err != nil {
		return err
	}
	taskID := "deploy-" + fmt.Sprint(time.Now().UnixNano())
	body := taskSubmitBody{
		TaskID: taskID, Type: taskType, AgentID: agentID,
		Payload: payload, Timeout: timeout,
		RulesAck: deployRulesAck,
	}
	var resp struct {
		TaskID string `json:"task_id"`
		Status string `json:"status"`
	}
	if err := apiPost(cfg, "/api/tasks", body, &resp); err != nil {
		return err
	}
	// 等结果
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	done, _ := waitResult(ctx, cfg, taskID, timeout)
	if !done {
		return fmt.Errorf("任务超时: %s", taskID)
	}
	if taskFailed {
		return fmt.Errorf("任务失败: %s", taskID)
	}
	return nil
}

// runTaskSyncScript 下发脚本并等待。
func runTaskSyncScript(cfg config, agentID, scriptPath, lang string, timeout int) error {
	content, err := os.ReadFile(scriptPath)
	if err != nil {
		return fmt.Errorf("读取脚本失败: %w", err)
	}
	payload, err := json.Marshal(struct {
		Lang    string `json:"lang"`
		Content string `json:"content"`
	}{Lang: lang, Content: string(content)})
	if err != nil {
		return err
	}
	taskID := "deploy-script-" + fmt.Sprint(time.Now().UnixNano())
	body := taskSubmitBody{
		TaskID: taskID, Type: "script", AgentID: agentID,
		Payload: payload, Timeout: timeout,
		RulesAck: deployRulesAck,
	}
	var resp struct {
		TaskID string `json:"task_id"`
		Status string `json:"status"`
	}
	if err := apiPost(cfg, "/api/tasks", body, &resp); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	done, _ := waitResult(ctx, cfg, taskID, timeout)
	if !done {
		return fmt.Errorf("脚本任务超时: %s", taskID)
	}
	if taskFailed {
		return fmt.Errorf("脚本任务失败: %s", taskID)
	}
	return nil
}

// runFileUpload 复用 fileUpload 逻辑(构造 args 调用)。
func runFileUpload(cfg config, agentID, localPath, remotePath string) error {
	// 复用 fileUpload 的完整逻辑(进度/hash/校验),构造等效 args
	return fileUpload(cfg, []string{"-a", agentID, localPath, remotePath})
}

// loadPlaybook 从 JSON 文件加载。
func loadPlaybook(path string) (*PlayBook, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取 playbook 失败: %w", err)
	}
	var pb PlayBook
	if err := json.Unmarshal(data, &pb); err != nil {
		return nil, fmt.Errorf("解析 playbook JSON 失败: %w", err)
	}
	if pb.Name == "" {
		pb.Name = path
	}
	if len(pb.Steps) == 0 {
		return nil, fmt.Errorf("playbook 无步骤")
	}
	return &pb, nil
}

// substituteVars 替换 ${var} 变量。
func substituteVars(s string, vars map[string]string) string {
	if s == "" || len(vars) == 0 {
		return s
	}
	for k, v := range vars {
		s = strings.ReplaceAll(s, "${"+k+"}", v)
	}
	return s
}

// parseDelay 解析延迟字符串("2s"/"500ms")。
func parseDelay(s string) time.Duration {
	if s == "" {
		return time.Second
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return time.Second
	}
	return d
}

// filepathIsAbs 跨平台绝对路径判断(避免 import 路径冲突)。
func filepathIsAbs(s string) bool {
	if len(s) >= 1 && (s[0] == '/' || s[0] == '\\') {
		return true
	}
	// Windows C:\
	if len(s) >= 3 && s[1] == ':' && (s[2] == '\\' || s[2] == '/') {
		return true
	}
	return false
}

// joinPath 拼接路径(用 / 或 \ 由 Agent 侧解析)。
func joinPath(base, rel string) string {
	if base == "" {
		return rel
	}
	sep := "/"
	if strings.Contains(base, "\\") {
		sep = "\\"
	}
	return strings.TrimRight(base, "/\\") + sep + strings.TrimLeft(rel, "/\\")
}
