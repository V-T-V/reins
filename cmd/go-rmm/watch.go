// CLI watch 命令 — 进程/服务守护(F4 扩展)。
//
// 用途:监控远端关键进程或服务,崩溃时自动重启。
// 通过 cron 实现:定期检查进程/服务状态,不运行则启动。
//
// 用法:
//
//	go-rmm watch add -a <agent> --proc <name> [--restart "Start-Process name"]
//	go-rmm watch add -a <agent> --svc <service-name>
//	go-rmm watch list                         (列出所有 watch 规则)
//	go-rmm watch remove <rule-id>
//	go-rmm watch check -a <agent>             (手动检查一次)
package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// cmdWatch watch 子命令入口。
func cmdWatch(cfg config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("用法: go-rmm watch <add|list|remove|check> [args]")
	}
	switch args[0] {
	case "add", "create":
		return watchAdd(cfg, args[1:])
	case "list", "ls":
		return watchList(cfg, args[1:])
	case "remove", "rm", "delete":
		return watchRemove(cfg, args[1:])
	case "check", "status":
		return watchCheck(cfg, args[1:])
	default:
		return fmt.Errorf("未知 watch 子命令: %s (可用: add list remove check)", args[0])
	}
}

// watchAdd 添加守护规则(转换为 cron + 条件命令)。
func watchAdd(cfg config, args []string) error {
	agentID := extractArg(args, "-a")
	procName := extractArg(args, "--proc")
	svcName := extractArg(args, "--svc")
	interval := extractArg(args, "--interval")
	if interval == "" {
		interval = "*/1 * * * *" // 每分钟检查
	}

	if agentID == "" {
		return fmt.Errorf("-a <agent> 必填")
	}
	if procName == "" && svcName == "" {
		return fmt.Errorf("需要 --proc <进程名> 或 --svc <服务名>")
	}

	var checkCmd, restartCmd, desc string
	if svcName != "" {
		// 服务守护:检查服务状态,stopped 则 Start-Service
		checkCmd = fmt.Sprintf("$s=Get-Service -Name '%s' -ErrorAction SilentlyContinue; if($s.Status -ne 'Running'){Start-Service -Name '%s'; Write-Output 'restarted'}", svcName, svcName)
		restartCmd = fmt.Sprintf("Start-Service -Name '%s'", svcName)
		desc = fmt.Sprintf("守护服务 %s", svcName)
	} else {
		// 进程守护:检查进程是否存在,不存在则启动
		checkCmd = fmt.Sprintf("$p=Get-Process -Name '%s' -ErrorAction SilentlyContinue; if(-not $p){Start-Process '%s'; Write-Output 'restarted'}", procName, procName)
		restartCmd = fmt.Sprintf("Start-Process '%s'", procName)
		desc = fmt.Sprintf("守护进程 %s", procName)
	}

	// 创建 cron 规则(每分钟检查 + 自动重启)
	rule := cronRuleCLI{
		CronExpr: interval,
		TaskType: "powershell",
		AgentID:  agentID,
	}
	payload, _ := json.Marshal(struct {
		Command string `json:"command"`
	}{Command: checkCmd})
	rule.Payload = payload
	rule.Timeout = 30

	var created cronRuleCLI
	if err := apiPost(cfg, "/api/crons", rule, &created); err != nil {
		return err
	}

	fmt.Printf("✅ 守护规则已创建\n")
	fmt.Printf("   %s (agent=%s)\n", desc, agentID)
	fmt.Printf("   检查间隔: %s\n", interval)
	fmt.Printf("   检查命令: %s\n", checkCmd)
	fmt.Printf("   重启命令: %s\n", restartCmd)
	fmt.Printf("   cron ID: %s\n", created.ID)
	return nil
}

// watchList 列出守护规则(过滤含 '守护' 或 Get-Service/Get-Process 的 cron)。
func watchList(cfg config, args []string) error {
	var resp struct {
		Crons   []cronRuleCLI `json:"crons"`
		Count   int           `json:"count"`
		Enabled bool          `json:"enabled"`
	}
	if err := apiGet(cfg, "/api/crons", &resp); err != nil {
		return err
	}
	if !resp.Enabled || resp.Count == 0 {
		fmt.Println("无定时规则")
		return nil
	}

	// 过滤守护类规则(payload 含 Get-Service 或 Get-Process)
	var watchRules []cronRuleCLI
	for _, c := range resp.Crons {
		payloadStr := string(c.Payload)
		if strings.Contains(payloadStr, "Get-Service") || strings.Contains(payloadStr, "Get-Process") ||
			strings.Contains(payloadStr, "Start-Service") || strings.Contains(payloadStr, "Start-Process") {
			watchRules = append(watchRules, c)
		}
	}

	if len(watchRules) == 0 {
		fmt.Println("无守护规则(用 watch add 创建)")
		return nil
	}

	fmt.Printf("守护规则 (%d):\n", len(watchRules))
	for _, r := range watchRules {
		enabled := "true"
		if r.Enabled != nil && !*r.Enabled {
			enabled = "false"
		}
		fmt.Printf("  %s  cron=%s  agent=%s  enabled=%s\n", r.ID, r.CronExpr, r.AgentID, enabled)
	}
	return nil
}

// watchRemove 删除守护规则(复用 cron delete)。
func watchRemove(cfg config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("用法: go-rmm watch remove <rule-id>")
	}
	ruleID := args[0]
	return cronDelete(cfg, []string{ruleID})
}

// watchCheck 手动检查一次(不发 cron,直接执行检查命令)。
func watchCheck(cfg config, args []string) error {
	agentID := extractArg(args, "-a")
	procName := extractArg(args, "--proc")
	svcName := extractArg(args, "--svc")
	if agentID == "" {
		return fmt.Errorf("-a <agent> 必填")
	}

	if svcName != "" {
		// 检查服务状态(双方言)
		var stdout string
		var err error
		if isLinuxAgent(cfg, agentID) {
			stdout, err = runOpsScript(cfg, agentID, fmt.Sprintf("systemctl status '%s' --no-pager 2>/dev/null | head -5; echo enabled:$(systemctl is-enabled '%s' 2>/dev/null)", svcName, svcName), 10)
		} else {
			stdout, err = runOpsTask(cfg, agentID, fmt.Sprintf("Get-Service -Name '%s' | Select Name,Status,StartType | Format-Table -AutoSize | Out-String", svcName), 10)
		}
		if err != nil {
			return err
		}
		fmt.Println(stdout)
	} else if procName != "" {
		// 检查进程状态(双方言)
		var stdout string
		var err error
		if isLinuxAgent(cfg, agentID) {
			stdout, err = runOpsScript(cfg, agentID, fmt.Sprintf("pgrep -a -x '%s'; true", procName), 10)
		} else {
			stdout, err = runOpsTask(cfg, agentID, fmt.Sprintf("Get-Process -Name '%s' -ErrorAction SilentlyContinue | Select Name,Id,CPU,WS | Format-Table -AutoSize | Out-String", procName), 10)
		}
		if err != nil {
			return err
		}
		if strings.TrimSpace(stdout) == "" {
			fmt.Printf("⚠️ 进程 %s 未运行\n", procName)
		} else {
			fmt.Println(stdout)
		}
	} else {
		return fmt.Errorf("需要 --proc 或 --svc 指定检查目标")
	}
	return nil
}
