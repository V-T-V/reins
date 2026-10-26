// CLI deploy batch / health-check / inventory / history — 多 Agent 运维扩展(F1-F4)。
//
// 用法:
//
//	reins deploy batch <playbook.json> --tag env=prod       多 Agent 并行部署
//	reins health-check [--tag env=prod] [--schedule "*/5 * * * *"]  健康巡检
//	reins inventory [--format csv|json]                      Agent 清单导出
//	reins history [--agent <id>] [--status failed] [-n 100]  历史趋势统计
package main

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// === F1: deploy batch 多 Agent 并行部署 ===

func cmdDeployBatch(cfg config, args []string) error {
	playbookPath := posArg(args, 0)
	tag := extractArg(args, "--tag")
	if playbookPath == "" || tag == "" {
		return fmt.Errorf("用法: reins deploy batch <playbook.json> --tag <tag>")
	}

	// 加载 playbook
	pb, err := loadPlaybook(playbookPath)
	if err != nil {
		return err
	}

	// 查找匹配 Agent
	var resp struct {
		Agents []struct {
			AgentID string            `json:"agent_id"`
			Tags    map[string]string `json:"tags"`
		} `json:"agents"`
	}
	if err := apiGet(cfg, "/api/agents", &resp); err != nil {
		return err
	}
	var agentIDs []string
	for _, a := range resp.Agents {
		if matchTag(a.Tags, tag) {
			agentIDs = append(agentIDs, a.AgentID)
		}
	}
	if len(agentIDs) == 0 {
		return fmt.Errorf("无匹配 tag=%s 的在线 Agent", tag)
	}

	fmt.Printf("🚀 批量部署: %s → %d 个 Agent\n\n", pb.Name, len(agentIDs))

	// 并行部署
	type deployResult struct {
		AgentID string
		Success bool
		Error   string
		Steps   int
		Time    time.Duration
	}
	results := make([]deployResult, len(agentIDs))
	var wg sync.WaitGroup
	for i, aid := range agentIDs {
		wg.Add(1)
		go func(idx int, agentID string) {
			defer wg.Done()
			start := time.Now()
			// 修改 playbook 的 target 为当前 Agent
			pbCopy := *pb
			pbCopy.Target = agentID
			// 串行执行 steps
			stepOK := 0
			for _, step := range pbCopy.Steps {
				cmd := substituteVars(step.Cmd, map[string]string{})
				_, err := runOpsTask(cfg, agentID, cmd, 60)
				if err != nil {
					results[idx] = deployResult{AgentID: agentID, Success: false, Error: err.Error(), Steps: stepOK, Time: time.Since(start)}
					return
				}
				stepOK++
			}
			results[idx] = deployResult{AgentID: agentID, Success: true, Steps: stepOK, Time: time.Since(start)}
		}(i, aid)
	}
	wg.Wait()

	// 汇总
	success := 0
	failed := 0
	for _, r := range results {
		status := "✅"
		if !r.Success {
			status = "❌"
			failed++
		} else {
			success++
		}
		fmt.Printf("%s %s (%d 步, %v)\n", status, r.AgentID, r.Steps, r.Time.Round(time.Second))
		if r.Error != "" {
			fmt.Printf("   错误: %s\n", r.Error)
		}
	}
	fmt.Printf("\n汇总: ✅ %d 成功 | ❌ %d 失败\n", success, failed)
	return nil
}

// === F2: health-check 健康巡检 ===

func cmdHealthCheck(cfg config, args []string) error {
	tag := extractArg(args, "--tag")
	schedule := extractArg(args, "--schedule")

	// 如果指定了 --schedule,创建 cron 规则
	if schedule != "" {
		// 用 diagnose 命令作为巡检脚本
		// 简化:创建一个 cron 规则,定期检查 CPU/内存/磁盘
		var resp struct {
			Agents []struct {
				AgentID string            `json:"agent_id"`
				Tags    map[string]string `json:"tags"`
			} `json:"agents"`
		}
		if err := apiGet(cfg, "/api/agents", &resp); err != nil {
			return err
		}
		count := 0
		for _, a := range resp.Agents {
			if tag != "" && !matchTag(a.Tags, tag) {
				continue
			}
			// 创建巡检 cron
			checkCmd := `$r=@{cpu=[math]::Round((Get-Counter '\Processor(_Total)\% Processor Time').CounterSamples.CookedValue,1);mem=[math]::Round((Get-CimInstance Win32_OperatingSystem).TotalVisibleMemorySize/1MB,1);memFree=[math]::Round((Get-CimInstance Win32_OperatingSystem).FreePhysicalMemory/1MB,1);diskC=[math]::Round((Get-PSDrive C).Used/((Get-PSDrive C).Used+(Get-PSDrive C).Free)*100,1)};if($r.cpu -gt 90 -or $r.diskC -gt 90){Write-Output ('ALERT: CPU='+$r.cpu+'% Disk='+$r.diskC+'%')}else{Write-Output 'OK'};$r|ConvertTo-Json -Compress`
			payload, _ := json.Marshal(map[string]string{"command": checkCmd})
			cronBody := cronRuleCLI{
				CronExpr: schedule,
				TaskType: "powershell",
				AgentID:  a.AgentID,
				Payload:  payload,
				Timeout:  30,
			}
			var created cronRuleCLI
			if err := apiPost(cfg, "/api/crons", cronBody, &created); err != nil {
				fmt.Fprintf(os.Stderr, "⚠️ %s 创建巡检规则失败: %v\n", a.AgentID, err)
				continue
			}
			fmt.Printf("✅ %s 巡检已创建 (cron=%s, id=%s)\n", a.AgentID, schedule, created.ID)
			count++
		}
		fmt.Printf("\n共创建 %d 个巡检规则\n", count)
		return nil
	}

	// 一次性巡检(复用 diagnose)
	fmt.Println("🏥 健康巡检(一次性)")
	return cmdDiagnose(cfg, args)
}

// === F3: inventory 清单导出 ===

func cmdInventory(cfg config, args []string) error {
	format := extractArg(args, "--format")
	if format == "" {
		format = "json"
	}

	var resp struct {
		Agents []struct {
			AgentID      string            `json:"agent_id"`
			Hostname     string            `json:"hostname"`
			OS           string            `json:"os"`
			Version      string            `json:"version"`
			Capabilities []string          `json:"capabilities"`
			Tags         map[string]string `json:"tags"`
		} `json:"agents"`
		Count int `json:"count"`
	}
	if err := apiGet(cfg, "/api/agents", &resp); err != nil {
		return err
	}

	switch format {
	case "csv":
		w := csv.NewWriter(os.Stdout)
		defer w.Flush()
		w.Write([]string{"agent_id", "hostname", "os", "version", "capabilities", "tags"})
		for _, a := range resp.Agents {
			var tagParts []string
			for k, v := range a.Tags {
				tagParts = append(tagParts, k+"="+v)
			}
			w.Write([]string{
				a.AgentID, a.Hostname, a.OS, a.Version,
				strings.Join(a.Capabilities, ";"),
				strings.Join(tagParts, ";"),
			})
		}
	default:
		data, _ := json.MarshalIndent(resp.Agents, "", "  ")
		fmt.Println(string(data))
	}
	return nil
}

// === F4: history 历史趋势统计 ===

func cmdHistory(cfg config, args []string) error {
	agentFilter := extractArg(args, "--agent")
	statusFilter := extractArg(args, "--status")
	limit := 100
	if l := extractArg(args, "-n"); l != "" {
		fmt.Sscanf(l, "%d", &limit)
	}

	var resp struct {
		Tasks []struct {
			Request struct {
				TaskID  string `json:"task_id"`
				Type    string `json:"type"`
				AgentID string `json:"agent_id"`
			} `json:"request"`
			Status    string `json:"status"`
			CreatedAt int64  `json:"created_at"`
		} `json:"tasks"`
		Count int `json:"count"`
	}
	queryLimit := limit
	if agentFilter != "" || statusFilter != "" {
		queryLimit *= 5
	}
	if err := apiGet(cfg, fmt.Sprintf("/api/tasks?limit=%d", queryLimit), &resp); err != nil {
		return err
	}

	// 过滤
	var filtered []struct {
		Type      string
		AgentID   string
		Status    string
		CreatedAt int64
	}
	for _, t := range resp.Tasks {
		if agentFilter != "" && t.Request.AgentID != agentFilter {
			continue
		}
		if statusFilter != "" && t.Status != statusFilter {
			continue
		}
		filtered = append(filtered, struct {
			Type      string
			AgentID   string
			Status    string
			CreatedAt int64
		}{t.Request.Type, t.Request.AgentID, t.Status, t.CreatedAt})
	}
	if len(filtered) > limit {
		filtered = filtered[:limit]
	}

	// 统计
	total := len(filtered)
	statusCount := map[string]int{}
	typeCount := map[string]int{}
	agentCount := map[string]int{}
	for _, t := range filtered {
		statusCount[t.Status]++
		typeCount[t.Type]++
		agentCount[t.AgentID]++
	}

	fmt.Printf("📊 历史统计 (%d 条任务)\n\n", total)

	// 按状态
	fmt.Println("按状态:")
	for s, c := range statusCount {
		pct := float64(c) / float64(total) * 100
		bar := strings.Repeat("█", c*20/total)
		fmt.Printf("  %-14s %4d (%5.1f%%) %s\n", s, c, pct, bar)
	}

	// 按类型
	fmt.Println("\n按类型:")
	for t, c := range typeCount {
		fmt.Printf("  %-14s %4d\n", t, c)
	}

	// 按 Agent
	fmt.Println("\n按 Agent:")
	for a, c := range agentCount {
		fmt.Printf("  %-30s %4d\n", a, c)
	}

	// 成功率
	if completed := statusCount["completed"] + statusCount["failed"]; completed > 0 {
		rate := float64(statusCount["completed"]) / float64(completed) * 100
		fmt.Printf("\n成功率: %.1f%% (%d/%d)\n", rate, statusCount["completed"], completed)
	}
	return nil
}
