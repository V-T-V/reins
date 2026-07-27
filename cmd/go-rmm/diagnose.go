// CLI topology / diagnose / group / compare 命令 — 多 Agent 分析(M1-M4)。
//
// 用法:
//
//	go-rmm topology                        多 Agent 拓扑视图(按 tag/region 分组)
//	go-rmm diagnose --tag env=prod         批量诊断(一键全量 Agent 体检)
//	go-rmm diagnose -a agent-1,agent-2     指定 Agent 诊断
//	go-rmm compare agent-1 agent-2         差异分析(软件/服务/端口对比)
//	go-rmm group list                       列出 Agent 分组(按 tag 聚合)
//	go-rmm group run <group> -c <cmd>       按组批量执行
package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// === M1: topology 拓扑视图 ===

func cmdTopology(cfg config, args []string) error {
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
	if resp.Count == 0 {
		fmt.Println("无在线 Agent")
		return nil
	}

	// 按 tag 分组聚合
	groups := make(map[string][]string) // groupKey → []agentID
	noTag := []string{}
	for _, a := range resp.Agents {
		if len(a.Tags) == 0 {
			noTag = append(noTag, a.AgentID)
			continue
		}
		// 用 env+role 组合作为分组 key
		env := a.Tags["env"]
		role := a.Tags["role"]
		region := a.Tags["region"]
		parts := []string{}
		if env != "" {
			parts = append(parts, env)
		}
		if role != "" {
			parts = append(parts, role)
		}
		if region != "" {
			parts = append(parts, region)
		}
		key := strings.Join(parts, "/")
		if key == "" {
			key = "other"
		}
		groups[key] = append(groups[key], a.AgentID)
	}

	// 排序输出
	fmt.Printf("🌐 Agent 拓扑 (%d 个在线)\n\n", resp.Count)
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		agents := groups[k]
		fmt.Printf("📁 %s (%d)\n", k, len(agents))
		for _, a := range agents {
			fmt.Printf("   └─ %s\n", a)
		}
	}
	if len(noTag) > 0 {
		fmt.Printf("📁 未分组 (%d)\n", len(noTag))
		for _, a := range noTag {
			fmt.Printf("   └─ %s\n", a)
		}
	}
	return nil
}

// === M2: diagnose 批量诊断 ===

func cmdDiagnose(cfg config, args []string) error {
	tag := extractArg(args, "--tag")
	agentList := extractArg(args, "-a")

	var agentIDs []string
	if agentList != "" {
		agentIDs = strings.Split(agentList, ",")
		for i := range agentIDs {
			agentIDs[i] = strings.TrimSpace(agentIDs[i])
		}
	} else if tag != "" {
		var resp struct {
			Agents []struct {
				AgentID string            `json:"agent_id"`
				Tags    map[string]string `json:"tags"`
			} `json:"agents"`
		}
		if err := apiGet(cfg, "/api/agents", &resp); err != nil {
			return err
		}
		for _, a := range resp.Agents {
			if matchTag(a.Tags, tag) {
				agentIDs = append(agentIDs, a.AgentID)
			}
		}
	}
	if len(agentIDs) == 0 {
		return fmt.Errorf("无匹配 Agent(用 --tag 或 -a 指定)")
	}

	fmt.Printf("🏥 批量诊断: %d 个 Agent\n\n", len(agentIDs))

	// 并行诊断
	results := make(map[string]*diagResult)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, aid := range agentIDs {
		wg.Add(1)
		go func(agentID string) {
			defer wg.Done()
			r := diagnoseAgent(cfg, agentID)
			mu.Lock()
			results[agentID] = r
			mu.Unlock()
		}(aid)
	}
	wg.Wait()

	// 汇总输出
	printDiagResults(results)
	return nil
}

type diagResult struct {
	AgentID  string
	Hostname string
	Online   bool
	CPU      string
	Mem      string
	Disk     string
	Errors   []string
	Warnings []string
	Healthy  bool
}

func diagnoseAgent(cfg config, agentID string) *diagResult {
	r := &diagResult{AgentID: agentID, Healthy: true}
	// 下发 sysinfo 命令
	stdout, err := runOpsTask(cfg, agentID, `$r=@{hostname=$env:COMPUTERNAME;cpu=[math]::Round((Get-Counter '\Processor(_Total)\% Processor Time').CounterSamples.CookedValue,1);memTotal=[int]((Get-CimInstance Win32_OperatingSystem).TotalVisibleMemorySize/1024);memFree=[int]((Get-CimInstance Win32_OperatingSystem).FreePhysicalMemory/1024);diskC=[math]::Round((Get-PSDrive C).Used/((Get-PSDrive C).Used+(Get-PSDrive C).Free)*100,1)};$r|ConvertTo-Json -Compress`, 15)
	if err != nil {
		r.Online = false
		r.Healthy = false
		r.Errors = append(r.Errors, fmt.Sprintf("不可达: %v", err))
		return r
	}
	r.Online = true
	// 解析 JSON
	var info map[string]interface{}
	if json.Unmarshal([]byte(stdout), &info) != nil {
		r.Errors = append(r.Errors, "JSON 解析失败")
		r.Healthy = false
		return r
	}
	if h, ok := info["hostname"].(string); ok {
		r.Hostname = h
	}
	if cpu, ok := info["cpu"].(float64); ok {
		r.CPU = fmt.Sprintf("%.1f%%", cpu)
		if cpu > 90 {
			r.Warnings = append(r.Warnings, fmt.Sprintf("CPU %.0f%% 过高", cpu))
		}
		if cpu > 95 {
			r.Healthy = false
		}
	}
	memTotal, _ := info["memTotal"].(float64)
	memFree, _ := info["memFree"].(float64)
	if memTotal > 0 {
		pct := (memTotal - memFree) / memTotal * 100
		r.Mem = fmt.Sprintf("%.1f%% (%.0fMB free)", pct, memFree)
		if pct > 90 {
			r.Warnings = append(r.Warnings, fmt.Sprintf("内存 %.0f%% 过高", pct))
		}
	}
	if disk, ok := info["diskC"].(float64); ok {
		r.Disk = fmt.Sprintf("%.1f%%", disk)
		if disk > 90 {
			r.Warnings = append(r.Warnings, fmt.Sprintf("磁盘 C: %.0f%% 过高", disk))
			r.Healthy = false
		}
	}
	return r
}

func printDiagResults(results map[string]*diagResult) {
	if jsonMode {
		data, _ := json.Marshal(results)
		fmt.Println(string(data))
		return
	}
	healthy := 0
	warning := 0
	critical := 0
	for _, r := range results {
		if !r.Online || !r.Healthy {
			critical++
		} else if len(r.Warnings) > 0 {
			warning++
		} else {
			healthy++
		}
	}
	fmt.Printf("汇总: ✅ %d 健康 | ⚠️ %d 告警 | ❌ %d 异常\n\n", healthy, warning, critical)
	for _, r := range results {
		status := "✅"
		if !r.Online || !r.Healthy {
			status = "❌"
		} else if len(r.Warnings) > 0 {
			status = "⚠️"
		}
		fmt.Printf("%s %s (%s)\n", status, r.AgentID, r.Hostname)
		if r.CPU != "" {
			fmt.Printf("   CPU: %s  Mem: %s  Disk C: %s\n", r.CPU, r.Mem, r.Disk)
		}
		for _, w := range r.Warnings {
			fmt.Printf("   ⚠️ %s\n", w)
		}
		for _, e := range r.Errors {
			fmt.Printf("   ❌ %s\n", e)
		}
		fmt.Println()
	}
}

// === M3: group 分组 ===

func cmdGroup(cfg config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("用法: go-rmm group <list|run> [args]")
	}
	switch args[0] {
	case "list", "ls":
		return groupList(cfg, args[1:])
	case "run", "exec":
		return groupRun(cfg, args[1:])
	default:
		return fmt.Errorf("未知 group 子命令: %s (可用: list run)", args[0])
	}
}

func groupList(cfg config, args []string) error {
	// 复用 topology 逻辑(按 tag 聚合)
	return cmdTopology(cfg, args)
}

func groupRun(cfg config, args []string) error {
	tag := extractArg(args, "--tag")
	command := extractArg(args, "-c")
	if tag == "" || command == "" {
		return fmt.Errorf("用法: go-rmm group run --tag <group> -c <command>")
	}
	// 复用 exec 批量并行
	return cmdExec(cfg, []string{"--tag", tag, "-c", command})
}

// === M4: compare 差异分析 ===

func cmdCompare(cfg config, args []string) error {
	a1 := posArg(args, 0)
	a2 := posArg(args, 1)
	if a1 == "" || a2 == "" {
		return fmt.Errorf("用法: go-rmm compare <agent-1> <agent-2>")
	}
	fmt.Printf("🔍 对比 %s vs %s\n\n", a1, a2)

	// 并行采集两个 Agent 的信息
	var wg sync.WaitGroup
	var s1, s2 string
	wg.Add(2)
	go func() { defer wg.Done(); s1, _ = runOpsTask(cfg, a1, compareCmd(), 20) }()
	go func() { defer wg.Done(); s2, _ = runOpsTask(cfg, a2, compareCmd(), 20) }()
	wg.Wait()

	// 解析
	var i1, i2 map[string]interface{}
	json.Unmarshal([]byte(s1), &i1)
	json.Unmarshal([]byte(s2), &i2)
	if i1 == nil || i2 == nil {
		return fmt.Errorf("采集失败")
	}

	fmt.Printf("%-20s %-30s %-30s\n", "项目", a1, a2)
	fmt.Println(strings.Repeat("-", 85))
	compareField(i1, i2, "hostname", "主机名")
	compareField(i1, i2, "os", "操作系统")
	compareFieldNum(i1, i2, "cpu", "CPU%")
	compareFieldNum(i1, i2, "memTotal", "内存(MB)")
	// 服务/进程差异
	svc1 := splitNewlines(getStr(i1, "services"))
	svc2 := splitNewlines(getStr(i2, "services"))
	set1 := toSet(svc1)
	set2 := toSet(svc2)
	only1 := setDiff(set1, set2)
	only2 := setDiff(set2, set1)
	fmt.Printf("%-20s %d 个服务(%d 独有)        %d 个服务(%d 独有)\n",
		"自动启动服务", len(svc1), len(only1), len(svc2), len(only2))
	if len(only1) > 0 {
		fmt.Printf("  %s 独有: %s\n", a1, strings.Join(only1[:min(5, len(only1))], ", "))
	}
	if len(only2) > 0 {
		fmt.Printf("  %s 独有: %s\n", a2, strings.Join(only2[:min(5, len(only2))], ", "))
	}
	return nil
}

func compareCmd() string {
	return "$ErrorActionPreference='SilentlyContinue';" +
		"$r=@{};" +
		"$r.hostname=$env:COMPUTERNAME;" +
		"$r.os=[string](Get-CimInstance Win32_OperatingSystem).Caption;" +
		"$r.memTotal=[int]((Get-CimInstance Win32_OperatingSystem).TotalVisibleMemorySize/1024);" +
		"$r.services=(Get-Service|Where StartType -eq Automatic|Select -ExpandProperty Name|Sort) -join ',';" +
		"$r|ConvertTo-Json -Compress"
}

func compareField(i1, i2 map[string]interface{}, key, label string) {
	v1 := fmt.Sprintf("%v", i1[key])
	v2 := fmt.Sprintf("%v", i2[key])
	mark := ""
	if v1 != v2 {
		mark = " ⚠️"
	}
	fmt.Printf("%-20s %-30s %-30s%s\n", label, v1, v2, mark)
}

func compareFieldNum(i1, i2 map[string]interface{}, key, label string) {
	v1, _ := i1[key].(float64)
	v2, _ := i2[key].(float64)
	mark := ""
	if v1 != v2 {
		mark = " ⚠️"
	}
	fmt.Printf("%-20s %-30v %-30v%s\n", label, v1, v2, mark)
}

func toSet(items []string) map[string]bool {
	s := make(map[string]bool)
	for _, i := range items {
		i = trimSpace(i)
		if i != "" {
			s[i] = true
		}
	}
	return s
}

func setDiff(a, b map[string]bool) []string {
	var diff []string
	for k := range a {
		if !b[k] {
			diff = append(diff, k)
		}
	}
	sort.Strings(diff)
	return diff
}


// 确保 time 被引用(diagnose 中的超时控制)。
var _ = time.Second

func getStr(m map[string]interface{}, k string) string { if v,ok := m[k].(string); ok { return v }; return "" }
func splitNewlines(s string) []string { return strings.Split(s, "\n") }
