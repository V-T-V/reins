// CLI workflow / alert 命令 — 任务编排 + 告警规则(F5-F6)。
//
// 用法:
//
//	go-rmm workflow run <workflow.json>          执行多步编排(条件+变量传递)
//	go-rmm workflow validate <workflow.json>     校验
//	go-rmm alert add --agent <id> --metric cpu --gt 90 --for 300 --action webhook
//	go-rmm alert list
//	go-rmm alert delete <rule-id>
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
)

// === F5: workflow 任务编排 ===

// WorkflowDef 工作流定义(JSON)。
type WorkflowDef struct {
	Name      string            `json:"name"`
	Target    string            `json:"target"` // 单 agent 或留空用 steps 里的
	Variables map[string]string `json:"vars,omitempty"`
	Steps     []WorkflowStep    `json:"steps"`
	OnFailure string            `json:"on_failure,omitempty"` // abort(默认) / continue
}

// WorkflowStep 单步骤。
type WorkflowStep struct {
	Name    string `json:"name"`
	Command string `json:"command"`           // PowerShell 命令
	Timeout int    `json:"timeout,omitempty"` // 默认 60
	// 条件:上一步 stdout 匹配此正则才执行(否则 skip)
	Condition string `json:"condition,omitempty"`
	// 提取变量:从 stdout 提取命名捕获组,供后续步骤 ${var} 引用
	Extract map[string]string `json:"extract,omitempty"` // varName → 正则
}

// cmdWorkflow workflow 子命令入口。
func cmdWorkflow(cfg config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("用法: go-rmm workflow <run|validate> <workflow.json>")
	}
	switch args[0] {
	case "run":
		return workflowRun(cfg, args[1:])
	case "validate", "check":
		return workflowValidate(cfg, args[1:])
	default:
		return fmt.Errorf("未知 workflow 子命令: %s", args[0])
	}
}

func workflowValidate(cfg config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("用法: go-rmm workflow validate <workflow.json>")
	}
	data, err := os.ReadFile(args[0])
	if err != nil {
		return fmt.Errorf("读取失败: %w", err)
	}
	var wf WorkflowDef
	if err := json.Unmarshal(data, &wf); err != nil {
		return fmt.Errorf("JSON 解析失败: %w", err)
	}
	if wf.Name == "" {
		return fmt.Errorf("name 不能为空")
	}
	if len(wf.Steps) == 0 {
		return fmt.Errorf("steps 不能为空")
	}
	for i, s := range wf.Steps {
		if s.Command == "" {
			return fmt.Errorf("step[%d] command 不能为空", i)
		}
	}
	fmt.Printf("✅ workflow 校验通过\n")
	fmt.Printf("   名称: %s\n", wf.Name)
	fmt.Printf("   步骤: %d\n", len(wf.Steps))
	for i, s := range wf.Steps {
		fmt.Printf("     %d. %s", i+1, s.Name)
		if s.Condition != "" {
			fmt.Printf(" (条件: %s)", s.Condition)
		}
		if len(s.Extract) > 0 {
			fmt.Printf(" (提取: %d 变量)", len(s.Extract))
		}
		fmt.Println()
	}
	return nil
}

func workflowRun(cfg config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("用法: go-rmm workflow run <workflow.json>")
	}
	data, err := os.ReadFile(args[0])
	if err != nil {
		return fmt.Errorf("读取失败: %w", err)
	}
	var wf WorkflowDef
	if err := json.Unmarshal(data, &wf); err != nil {
		return fmt.Errorf("JSON 解析失败: %w", err)
	}
	if wf.Target == "" {
		return fmt.Errorf("target 不能为空")
	}

	// 变量上下文(初始用 wf.Variables,逐步追加 Extract 结果)
	vars := make(map[string]string)
	for k, v := range wf.Variables {
		vars[k] = v
	}

	fmt.Printf("🔄 工作流: %s (%d 步)\n\n", wf.Name, len(wf.Steps))

	for i, step := range wf.Steps {
		// 条件检查
		if step.Condition != "" {
			re, err := regexp.Compile(step.Condition)
			if err != nil {
				return fmt.Errorf("step[%d] 条件正则错误: %w", i, err)
			}
			prevVar := vars["__last_stdout__"]
			if !re.MatchString(prevVar) {
				fmt.Printf("[%d/%d] %s ⏭️ skip (条件不匹配)\n", i+1, len(wf.Steps), step.Name)
				continue
			}
		}

		// 变量替换 ${var}
		cmd := step.Command
		for k, v := range vars {
			cmd = strings.ReplaceAll(cmd, "${"+k+"}", v)
		}

		fmt.Printf("[%d/%d] %s\n", i+1, len(wf.Steps), step.Name)

		// 执行
		timeout := step.Timeout
		if timeout <= 0 {
			timeout = 60
		}
		stdout, err := runOpsTask(cfg, wf.Target, cmd, timeout)
		if err != nil {
			fmt.Printf("  ❌ 失败: %v\n", err)
			if wf.OnFailure != "continue" {
				fmt.Println("\n🛑 工作流中止(on_failure=abort)")
				return errTaskFailed(fmt.Sprintf("step %s 失败", step.Name))
			}
			fmt.Println("  ⚠️ continue(配置了 on_failure=continue)")
			continue
		}

		// 提取变量
		for varName, pattern := range step.Extract {
			re, err := regexp.Compile(pattern)
			if err != nil {
				continue
			}
			matches := re.FindStringSubmatch(stdout)
			if len(matches) > 1 {
				vars[varName] = matches[1]
				fmt.Printf("  📎 提取 %s = %s\n", varName, matches[1])
			}
		}

		// 保存 stdout 供下一步条件判断
		vars["__last_stdout__"] = stdout
		fmt.Printf("  ✅ 完成\n")
	}

	fmt.Printf("\n🎉 工作流完成: %s\n", wf.Name)
	return nil
}

// === F6: alert 告警规则 ===

// AlertRule 告警规则定义。
type AlertRule struct {
	ID         string `json:"id"`
	AgentID    string `json:"agent_id"`
	Metric     string `json:"metric"`      // cpu / mem / disk
	Operator   string `json:"operator"`    // gt / lt
	Threshold  int    `json:"threshold"`   // 90
	ForSeconds int    `json:"for_seconds"` // 持续秒数(简化:单次触发)
	CronExpr   string `json:"cron_expr"`   // 检查频率(默认 */1 * * * *)
}

func cmdAlert(cfg config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("用法: go-rmm alert <add|list|delete> [args]")
	}
	switch args[0] {
	case "add", "create":
		return alertAdd(cfg, args[1:])
	case "list", "ls":
		return alertList(cfg, args[1:])
	case "delete", "rm":
		return alertDelete(cfg, args[1:])
	default:
		return fmt.Errorf("未知 alert 子命令: %s", args[0])
	}
}

func alertAdd(cfg config, args []string) error {
	agentID := extractArg(args, "--agent")
	metric := extractArg(args, "--metric")
	operator := extractArg(args, "--op")
	thresholdStr := extractArg(args, "--gt")
	if thresholdStr == "" {
		thresholdStr = extractArg(args, "--lt")
		operator = "lt"
	}
	if operator == "" {
		operator = "gt"
	}
	cronExpr := extractArg(args, "--cron")
	if cronExpr == "" {
		cronExpr = "*/1 * * * *"
	}

	if agentID == "" || metric == "" || thresholdStr == "" {
		return fmt.Errorf("用法: go-rmm alert add --agent <id> --metric cpu --gt 90 [--cron '*/1 * * * *']")
	}
	if !containsStr([]string{"cpu", "mem", "disk"}, metric) {
		return fmt.Errorf("metric 只支持: cpu / mem / disk")
	}

	// 构造检查命令(根据 metric + operator + threshold)
	var checkCmd string
	switch metric {
	case "cpu":
		checkCmd = `$cpu=[math]::Round((Get-Counter '\Processor(_Total)\% Processor Time').CounterSamples.CookedValue,1);` +
			`if($cpu ` + psOperator(operator) + ` ` + thresholdStr + `){Write-Output ('ALERT: CPU='+$cpu+'%')};$cpu`
	case "mem":
		checkCmd = `$os=Get-CimInstance Win32_OperatingSystem;$mem=[math]::Round(($os.TotalVisibleMemorySize-$os.FreePhysicalMemory)/$os.TotalVisibleMemorySize*100,1);` +
			`if($mem ` + psOperator(operator) + ` ` + thresholdStr + `){Write-Output ('ALERT: MEM='+$mem+'%')};$mem`
	case "disk":
		checkCmd = `$d=Get-PSDrive C;$disk=[math]::Round($d.Used/($d.Used+$d.Free)*100,1);` +
			`if($disk ` + psOperator(operator) + ` ` + thresholdStr + `){Write-Output ('ALERT: DISK='+$disk+'%')};$disk`
	}

	// 创建 cron 规则
	payload, _ := json.Marshal(map[string]string{"command": checkCmd})
	rule := cronRuleCLI{
		CronExpr: cronExpr,
		TaskType: "powershell",
		AgentID:  agentID,
		Payload:  payload,
		Timeout:  30,
	}
	var created cronRuleCLI
	if err := apiPost(cfg, "/api/crons", rule, &created); err != nil {
		return err
	}

	fmt.Printf("✅ 告警规则已创建\n")
	fmt.Printf("   Agent: %s\n", agentID)
	fmt.Printf("   条件: %s %s %s%%\n", metric, operatorStr(operator), thresholdStr)
	fmt.Printf("   频率: %s\n", cronExpr)
	fmt.Printf("   cron ID: %s\n", created.ID)
	fmt.Printf("   (配 GO_RMM_WEBHOOKS 环境变量可自动推送告警到 Slack/钉钉)\n")
	return nil
}

func alertList(cfg config, args []string) error {
	// 复用 cron list,过滤含 ALERT 的规则
	var resp struct {
		Crons []cronRuleCLI `json:"crons"`
	}
	if err := apiGet(cfg, "/api/crons", &resp); err != nil {
		return err
	}
	var alerts []cronRuleCLI
	for _, c := range resp.Crons {
		if strings.Contains(string(c.Payload), "ALERT") {
			alerts = append(alerts, c)
		}
	}
	if len(alerts) == 0 {
		fmt.Println("无告警规则(用 alert add 创建)")
		return nil
	}
	fmt.Printf("告警规则 (%d):\n", len(alerts))
	for _, a := range alerts {
		fmt.Printf("  %s  agent=%s  cron=%s\n", a.ID, a.AgentID, a.CronExpr)
	}
	return nil
}

func alertDelete(cfg config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("用法: go-rmm alert delete <rule-id>")
	}
	return cronDelete(cfg, args)
}

func psOperator(op string) string {
	switch op {
	case "gt":
		return "-gt"
	case "lt":
		return "-lt"
	case "ge":
		return "-ge"
	case "le":
		return "-le"
	default:
		return "-gt"
	}
}

func operatorStr(op string) string {
	switch op {
	case "gt":
		return ">"
	case "lt":
		return "<"
	case "ge":
		return ">="
	case "le":
		return "<="
	default:
		return ">"
	}
}

// 确保 time 引用。
var _ = time.Second
