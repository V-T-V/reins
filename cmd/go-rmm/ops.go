// CLI 运维命令:sysinfo / service / proc / power / monitor。
//
// 即时查询类(sysinfo/service/proc)复用 powershell task 的语义化包装。
// 电源(power)走独立 /api/power 端点(标签白名单)。
// 监测(monitor)走独立 monitor task + SSE 流式。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/QiuShichang/go-rmm/proto"
)

// === sysinfo 一次性系统体检 ===

func cmdSysinfo(cfg config, args []string) error {
	agentID := extractArg(args, "-a")
	if agentID == "" {
		return fmt.Errorf("用法: go-rmm sysinfo -a <agent>")
	}
	// 组合 PowerShell 采集命令
	script := `$ErrorActionPreference='SilentlyContinue';
$cpu=(Get-Counter '\Processor(_Total)\% Processor Time').CounterSamples.CookedValue;
$os=Get-CimInstance Win32_OperatingSystem;
$disks=Get-CimInstance Win32_LogicalDisk -Filter 'DriveType=3';
$ub=(Get-Date)-$os.LastBootUpTime;
$r=@{
  hostname=$env:COMPUTERNAME;
  os=$os.Caption+' '+$os.Version;
  cpu_pct=[math]::Round($cpu,1);
  mem_total_gb=[math]::Round($os.TotalVisibleMemorySize/1MB,1);
  mem_free_gb=[math]::Round($os.FreePhysicalMemory/1MB,1);
  mem_used_pct=[math]::Round(($os.TotalVisibleMemorySize-$os.FreePhysicalMemory)/$os.TotalVisibleMemorySize*100,1);
  uptime_days=[math]::Round($ub.TotalDays,1);
  disks=@($disks|ForEach-Object{@{dev=$_.DeviceID;total_gb=[math]::Round($_.Size/1GB,1);free_gb=[math]::Round($_.FreeSpace/1GB,1);used_pct=[math]::Round(($_.Size-$_.FreeSpace)/$_.Size*100,1)}});
};
$r|ConvertTo-Json -Compress`
	stdout, err := runOpsTask(cfg, agentID, script, 15)
	if err != nil {
		return err
	}
	// JSON 输出模式
	if jsonMode {
		fmt.Println(stdout)
		return nil
	}
	// 解析格式化
	var info map[string]interface{}
	if err := json.Unmarshal([]byte(stdout), &info); err != nil {
		fmt.Println(stdout) // 解析失败直接输出原文
		return nil
	}
	fmt.Println("=== 系统体检 ===")
	fmt.Printf("主机名:    %v\n", info["hostname"])
	fmt.Printf("操作系统:  %v\n", info["os"])
	fmt.Printf("CPU 使用率: %.1f%%\n", getFloat(info, "cpu_pct"))
	fmt.Printf("内存:      %.1f / %.1f GB (%.1f%%)\n",
		getFloat(info, "mem_total_gb")-getFloat(info, "mem_free_gb"),
		getFloat(info, "mem_total_gb"), getFloat(info, "mem_used_pct"))
	fmt.Printf("运行时长:  %.1f 天\n", getFloat(info, "uptime_days"))
	if disks, ok := info["disks"].([]interface{}); ok {
		fmt.Println("\n磁盘:")
		for _, d := range disks {
			if dm, ok := d.(map[string]interface{}); ok {
				fmt.Printf("  %s  %.1f GB (可用 %.1f GB, 已用 %.1f%%)\n",
					dm["dev"], getFloat(dm, "total_gb"), getFloat(dm, "free_gb"), getFloat(dm, "used_pct"))
			}
		}
	}
	return nil
}

func getFloat(m map[string]interface{}, k string) float64 {
	if v, ok := m[k].(float64); ok {
		return v
	}
	return 0
}

// === service 服务管理 ===

func cmdService(cfg config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("用法: go-rmm service <list|start|stop|restart> -a <agent> [name]")
	}
	agentID := extractArg(args, "-a")
	if agentID == "" {
		return fmt.Errorf("-a <agent_id> 必填")
	}
	switch args[0] {
	case "list", "ls":
		statusFilter := extractArg(args, "--status")
		cmd := "Get-Service | Select Name,DisplayName,Status,StartType | ConvertTo-Csv -NoTypeInformation"
		if statusFilter != "" {
			cmd = fmt.Sprintf("Get-Service | Where Status -eq '%s' | Select Name,DisplayName,Status,StartType | ConvertTo-Csv -NoTypeInformation", statusFilter)
		}
		stdout, err := runOpsTask(cfg, agentID, cmd, 30)
		if err != nil {
			return err
		}
		fmt.Println(stdout)
		return nil
	case "start":
		name := posArg(args, 1)
		if name == "" {
			return fmt.Errorf("用法: go-rmm service start -a <agent> <name>")
		}
		return runSvcAction(cfg, agentID, "Start-Service", name)
	case "stop":
		name := posArg(args, 1)
		if name == "" {
			return fmt.Errorf("用法: go-rmm service stop -a <agent> <name>")
		}
		return runSvcAction(cfg, agentID, "Stop-Service", name)
	case "restart":
		name := posArg(args, 1)
		if name == "" {
			return fmt.Errorf("用法: go-rmm service restart -a <agent> <name>")
		}
		return runSvcAction(cfg, agentID, "Restart-Service", name)
	default:
		return fmt.Errorf("未知 service 子命令: %s", args[0])
	}
}

func runSvcAction(cfg config, agentID, action, svcName string) error {
	// 先查当前状态
	statusCmd := fmt.Sprintf("(Get-Service -Name '%s').Status", svcName)
	curStatus, _ := runOpsTask(cfg, agentID, statusCmd, 10)
	fmt.Printf("服务 %s 当前状态: %s\n", svcName, strings.TrimSpace(curStatus))
	// 执行操作
	cmd := fmt.Sprintf("%s -Name '%s'; Write-Output 'done'", action, svcName)
	_, err := runOpsTask(cfg, agentID, cmd, 20)
	if err != nil {
		return err
	}
	// 查新状态
	newStatus, _ := runOpsTask(cfg, agentID, statusCmd, 10)
	fmt.Printf("✅ 操作完成,新状态: %s\n", strings.TrimSpace(newStatus))
	return nil
}

// === proc 进程管理 ===

func cmdProc(cfg config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("用法: go-rmm proc <list|kill> -a <agent>")
	}
	agentID := extractArg(args, "-a")
	if agentID == "" {
		return fmt.Errorf("-a <agent_id> 必填")
	}
	switch args[0] {
	case "list", "ls":
		sortBy := extractArg(args, "--sort")
		if sortBy == "" {
			sortBy = "cpu"
		}
		n := 20
		nStr := extractArg(args, "-n")
		if nStr != "" {
			fmt.Sscanf(nStr, "%d", &n)
		}
		sortField := "CPU"
		if sortBy == "mem" {
			sortField = "WS"
		}
		cmd := fmt.Sprintf("Get-Process | Sort %s -Desc | Select -First %d Name,Id,CPU,@{n='Mem(MB)';e={[int]($_.WS/1MB)}} | Format-Table -AutoSize | Out-String", sortField, n)
		stdout, err := runOpsTask(cfg, agentID, cmd, 15)
		if err != nil {
			return err
		}
		fmt.Println(stdout)
		return nil
	case "kill":
		target := posArg(args, 1)
		if target == "" {
			return fmt.Errorf("用法: go-rmm proc kill -a <agent> <pid|name>")
		}
		var cmd string
		// 数字当 PID,否则当进程名
		if isAllDigit(target) {
			cmd = fmt.Sprintf("Stop-Process -Id %s -Force; Write-Output 'killed pid %s'", target, target)
		} else {
			cmd = fmt.Sprintf("Stop-Process -Name '%s' -Force; Write-Output 'killed %s'", target, target)
		}
		stdout, err := runOpsTask(cfg, agentID, cmd, 10)
		if err != nil {
			return err
		}
		fmt.Printf("✅ %s\n", strings.TrimSpace(stdout))
		return nil
	default:
		return fmt.Errorf("未知 proc 子命令: %s", args[0])
	}
}

// === power 电源操作 ===

func cmdPower(cfg config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("用法: go-rmm power <shutdown|restart|sleep|lock> -a <agent>")
	}
	agentID := extractArg(args, "-a")
	if agentID == "" {
		return fmt.Errorf("-a <agent_id> 必填")
	}
	force := false
	yes := false
	delay := 0
	for _, a := range args {
		if a == "--force" {
			force = true
		}
		if a == "--yes" || a == "-y" {
			yes = true
		}
		if strings.HasPrefix(a, "--delay=") {
			fmt.Sscanf(strings.TrimPrefix(a, "--delay="), "%d", &delay)
		}
	}
	op := args[0]
	switch op {
	case "shutdown", "restart", "sleep", "lock":
	default:
		return fmt.Errorf("未知 power 操作: %s (可用: shutdown restart sleep lock)", op)
	}

	// 二次确认(--yes 跳过)
	if !yes {
		fmt.Printf("⚠️  即将对 Agent %s 执行 [%s] 操作", agentID, op)
		if delay > 0 {
			fmt.Printf("(延迟 %ds)", delay)
		}
		fmt.Println()
		fmt.Print("确认? 输入 yes 继续: ")
		var input string
		fmt.Scanln(&input)
		if input != "yes" {
			fmt.Println("已取消")
			return nil
		}
	}

	// 调 /api/power(标签校验)
	q := url.Values{}
	body := map[string]any{
		"agent_id": agentID,
		"op":       op,
		"force":    force,
		"delay":    delay,
	}
	var resp struct {
		AgentID string `json:"agent_id"`
		TaskID  string `json:"task_id"`
		Op      string `json:"op"`
		Message string `json:"message"`
	}
	if err := apiPost(cfg, "/api/power?"+q.Encode(), body, &resp); err != nil {
		return err
	}
	fmt.Printf("✅ %s (agent=%s, task=%s)\n", resp.Message, resp.AgentID, resp.TaskID)
	return nil
}

// === monitor 流式监测 ===

func cmdMonitor(cfg config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("用法: go-rmm monitor <top|cpu|mem|disk|net|gpu|svc> -a <agent> [--interval 3] [--duration 60]")
	}
	subCmd := args[0]
	agentID := extractArg(args, "-a")
	if agentID == "" {
		return fmt.Errorf("-a <agent_id> 必填")
	}
	interval := 3
	duration := 60
	if v := extractArg(args, "--interval"); v != "" {
		fmt.Sscanf(v, "%d", &interval)
	}
	if v := extractArg(args, "--duration"); v != "" {
		fmt.Sscanf(v, "%d", &duration)
	}

	// 子命令映射到采集 metrics
	// 注:单次 PowerShell 脚本输出过长会触发 CLIXML 编码导致解析失败,
	// 因此 top 只采集基础(cpu/mem/disk/proc),扩展指标用 monitor disk/net/gpu 单独查
	var metrics []string
	switch subCmd {
	case "top", "full":
		metrics = []string{"cpu", "mem", "disk", "proc"} // 基础四项(稳定)
	case "cpu":
		metrics = []string{"cpu", "proc"}
	case "mem":
		metrics = []string{"mem"}
	case "disk":
		metrics = []string{"disk", "diskio"}
	case "net":
		metrics = []string{"netio", "net"}
	case "gpu":
		metrics = []string{"gpu"}
	case "svc", "service":
		metrics = []string{"svc"}
	default:
		return fmt.Errorf("未知 monitor 子命令: %s (可用: top cpu mem disk net gpu svc)", subCmd)
	}

	// 下发 monitor 任务(--no-wait,不等终态)
	taskID := "monitor-" + fmt.Sprint(time.Now().UnixNano())
	payload, _ := json.Marshal(proto.MonitorTask{
		Interval: interval,
		Duration: duration,
		Metrics:  metrics,
	})
	body := struct {
		TaskID  string          `json:"task_id"`
		Type    string          `json:"type"`
		AgentID string          `json:"agent_id"`
		Payload json.RawMessage `json:"payload"`
		Timeout int             `json:"timeout"`
	}{
		TaskID: taskID, Type: proto.TaskMonitor, AgentID: agentID,
		Payload: payload, Timeout: duration + 10,
	}
	var resp struct {
		TaskID string `json:"task_id"`
		Status string `json:"status"`
	}
	if err := apiPost(cfg, "/api/tasks", body, &resp); err != nil {
		return err
	}
	fmt.Printf("🔍 监测中 (agent=%s, mode=%s, interval=%ds, duration=%ds)\n", agentID, subCmd, interval, duration)
	fmt.Printf("   Ctrl+C 停止\n\n")

	// 订阅 SSE 接收 task_progress
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	sseMonitorSubscribe(ctx, cfg, taskID, agentID)
	return nil
}

// sseMonitorSubscribe 订阅 SSE,过滤出 monitor 的 task_progress,实时打印。
func sseMonitorSubscribe(ctx context.Context, cfg config, taskID, agentID string) {
	u := cfg.relay + "/api/events"
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return
	}
	if cfg.token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.token)
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := streamingClient.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "SSE 连接失败: %v\n", err)
		return
	}
	defer resp.Body.Close()
	scanner := newSSEScanner(resp.Body)
	for {
		ev, err := scanner.Next()
		if err != nil {
			return
		}
		if ctx.Err() != nil {
			return
		}
		if ev.event != "task_progress" {
			continue
		}
		var outer struct {
			Data json.RawMessage `json:"data"`
		}
		if json.Unmarshal(ev.data, &outer) != nil {
			continue
		}
		var prog proto.TaskProgress
		if json.Unmarshal(outer.Data, &prog) != nil {
			continue
		}
		if prog.TaskID != taskID {
			continue
		}
		// prog.Message 格式: "采样 #N | <json sample>"
		idx := strings.Index(prog.Message, "{")
		if idx < 0 {
			continue
		}
		var sample proto.MonitorSample
		if json.Unmarshal([]byte(prog.Message[idx:]), &sample) == nil {
			printMonitorSample(sample)
		}
	}
}

// renderDashboard 渲染多面板 TUI(glances 风格)。
// 用 ANSI 转义实现多行刷新:每次计算行数 → 回首行 → 逐行重绘。
// 首次调用打印面板,后续调用刷新(靠全局 prevDashLines 记录行数)。
var prevDashLines int

func printMonitorSample(s proto.MonitorSample) {
	// JSON 模式直接 dump
	if jsonMode {
		data, _ := json.Marshal(s)
		fmt.Println(string(data))
		return
	}

	// 渲染面板
	lines := renderDashboardLines(s)

	// 回到面板首行(首次 prevDashLines=0 不回退)
	if prevDashLines > 0 {
		fmt.Printf("\033[%dA", prevDashLines) // 光标上移
	}
	for _, line := range lines {
		fmt.Printf("\033[2K\r%s\n", line) // 清行 + 打印
	}
	prevDashLines = len(lines)
}

// renderDashboardLines 渲染所有面板行,返回行数组。
func renderDashboardLines(s proto.MonitorSample) []string {
	var lines []string
	w := 40 // 面板宽度

	// 时间戳行
	lines = append(lines, fmt.Sprintf(" go-rmm monitor  %s  up %dd%dh%dm",
		time.Unix(s.Timestamp, 0).Format("15:04:05"),
		s.Uptime/86400, (s.Uptime%86400)/3600, (s.Uptime%3600)/60))
	lines = append(lines, "")

	// CPU 面板
	lines = append(lines, panelHeader("CPU", w))
	lines = append(lines, fmt.Sprintf(" Total %5.1f%% %s", s.CPUUsage, progressBar(s.CPUUsage, 28)))
	// 每核(最多显示 8 核)
	cores := s.CPUPerCore
	if len(cores) > 8 {
		cores = cores[:8]
	}
	if len(cores) > 0 {
		coreLine := " "
		for i, c := range cores {
			coreLine += fmt.Sprintf("C%d:%3.0f%% ", i, c)
		}
		lines = append(lines, coreLine)
	}
	if s.CPUQueueLen > 0 || s.CPUCtxSwitch > 0 {
		lines = append(lines, fmt.Sprintf(" Queue:%.0f  CtxSwitch:%s/s", s.CPUQueueLen, formatInt(s.CPUCtxSwitch)))
	}
	lines = append(lines, panelFooter(w))

	// MEM 面板
	lines = append(lines, panelHeader("MEMORY", w))
	memUsed := float64(s.MemTotalMB-s.MemFreeMB) / 1024
	memTotal := float64(s.MemTotalMB) / 1024
	lines = append(lines, fmt.Sprintf(" %.1f/%.1fGB %5.1f%% %s", memUsed, memTotal, s.MemUsedPct, progressBar(s.MemUsedPct, 24)))
	if s.MemCommittedMB > 0 {
		lines = append(lines, fmt.Sprintf(" Committed: %.1fGB  Cache: %.1fGB", float64(s.MemCommittedMB)/1024, float64(s.MemCacheMB)/1024))
	}
	lines = append(lines, panelFooter(w))

	// 磁盘容量 + IO
	if len(s.Disks) > 0 || len(s.DiskIO) > 0 {
		lines = append(lines, panelHeader("DISK", w))
		for _, d := range s.Disks {
			lines = append(lines, fmt.Sprintf(" %s %5.1f/%5.1fGB %5.1f%% %s", d.Device, d.TotalGB-d.FreeGB, d.TotalGB, d.UsedPct, progressBar(d.UsedPct, 16)))
		}
		for _, d := range s.DiskIO {
			lines = append(lines, fmt.Sprintf(" %s R:%5.1fMB/s W:%5.1fMB/s Q:%.0f", truncate(d.Device, 8), d.ReadMBps, d.WriteMBps, d.QueueLength))
		}
		lines = append(lines, panelFooter(w))
	}

	// 网络
	if len(s.NetIO) > 0 || s.TCPEstablished > 0 {
		lines = append(lines, panelHeader("NETWORK", w))
		for _, n := range s.NetIO {
			lines = append(lines, fmt.Sprintf(" %-16s ↓%5.0fKB/s ↑%5.0fKB/s", truncate(n.Interface, 16), n.RxKBps, n.TxKBps))
		}
		if s.TCPEstablished > 0 || s.TCPListen > 0 {
			lines = append(lines, fmt.Sprintf(" TCP  ESTAB:%d  LISTEN:%d", s.TCPEstablished, s.TCPListen))
		}
		lines = append(lines, panelFooter(w))
	}

	// GPU
	if len(s.GPUs) > 0 {
		lines = append(lines, panelHeader("GPU", w))
		for _, g := range s.GPUs {
			line := fmt.Sprintf(" %s", truncate(g.Name, 28))
			if g.UsagePct > 0 {
				line += fmt.Sprintf(" %4.0f%%", g.UsagePct)
			}
			if g.TempC > 0 {
				line += fmt.Sprintf(" %2.0f°C", g.TempC)
			}
			lines = append(lines, line)
		}
		lines = append(lines, panelFooter(w))
	}

	// Top 进程
	if len(s.TopProcesses) > 0 {
		lines = append(lines, panelHeader("TOP PROCESSES", w))
		n := 5
		if len(s.TopProcesses) < n {
			n = len(s.TopProcesses)
		}
		for i := 0; i < n; i++ {
			p := s.TopProcesses[i]
			lines = append(lines, fmt.Sprintf(" %-16s %6d  %5.1f%%  %4.0fMB", truncate(p.Name, 16), p.PID, p.CPU, p.MemMB))
		}
		lines = append(lines, panelFooter(w))
	}

	// 异常服务
	if len(s.UnhealthyServices) > 0 {
		lines = append(lines, panelHeader("UNHEALTHY SERVICES", w))
		for _, sv := range s.UnhealthyServices {
			lines = append(lines, fmt.Sprintf(" ⚠ %-24s %s", truncate(sv.Name, 24), sv.Status))
		}
		lines = append(lines, panelFooter(w))
	}

	return lines
}

// panelHeader 面板头。
func panelHeader(title string, w int) string {
	return fmt.Sprintf("┌─ %s %s", title, strings.Repeat("─", w-len(title)-4))
}

// panelFooter 面板尾。
func panelFooter(w int) string {
	return "└" + strings.Repeat("─", w-1)
}

// progressBar 文本进度条 [████░░░░]。
func progressBar(pct float64, width int) string {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	filled := int(pct / 100 * float64(width))
	bar := strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
	return "[" + bar + "]"
}

// truncate 截断字符串到 maxLen。
func truncate(s string, maxLen int) string {
	// 简单按 rune 截断(中文占 2 宽度,这里简化)
	r := []rune(s)
	if len(r) <= maxLen {
		return s
	}
	return string(r[:maxLen])
}

// formatInt 格式化大数字(k/M)。
func formatInt(n int64) string {
	if n > 1000000 {
		return fmt.Sprintf("%.1fM", float64(n)/1000000)
	}
	if n > 1000 {
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprintf("%d", n)
}

// === 辅助 ===

// runOpsTask 下发 powershell 命令并等待结果,返回 stdout。
func runOpsTask(cfg config, agentID, cmd string, timeout int) (string, error) {
	if agentID == "" {
		return "", fmt.Errorf("-a <agent_id> 必填")
	}
	payload, _ := json.Marshal(struct {
		Command string `json:"command"`
	}{Command: cmd})
	taskID := "ops-" + fmt.Sprint(time.Now().UnixNano())
	body := struct {
		TaskID  string          `json:"task_id"`
		Type    string          `json:"type"`
		AgentID string          `json:"agent_id"`
		Payload json.RawMessage `json:"payload"`
		Timeout int             `json:"timeout"`
	}{
		TaskID: taskID, Type: "powershell", AgentID: agentID,
		Payload: payload, Timeout: timeout,
	}
	var resp struct {
		TaskID string `json:"task_id"`
		Status string `json:"status"`
	}
	if err := apiPost(cfg, "/api/tasks", body, &resp); err != nil {
		return "", err
	}
	// 轮询等结果
	for i := 0; i < timeout+10; i++ {
		var r struct {
			Task *struct {
				Status string `json:"status"`
			} `json:"task"`
			Result *struct {
				Stdout  string `json:"stdout"`
				Success bool   `json:"success"`
				Error   string `json:"error"`
			} `json:"result"`
		}
		if err := apiGet(cfg, "/api/tasks/"+taskID, &r); err == nil && r.Task != nil {
			if r.Task.Status == "completed" || r.Task.Status == "failed" {
				if r.Result != nil {
					if !r.Result.Success {
						return "", fmt.Errorf("%s", r.Result.Error)
					}
					return r.Result.Stdout, nil
				}
				return "", nil
			}
		}
		time.Sleep(time.Second)
	}
	return "", fmt.Errorf("任务超时")
}

// posArg 取位置参数(args[n],跳过 - 开头的)。
func posArg(args []string, n int) string {
	count := 0
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			continue
		}
		if count == n {
			return a
		}
		count++
	}
	return ""
}

// isAllDigit 判断字符串是否全数字。
func isAllDigit(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
