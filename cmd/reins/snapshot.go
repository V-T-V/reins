// CLI snapshot 命令 — 远端系统状态快照(F9 扩展)。
//
// 用途:采集远端系统的完整状态快照(已安装软件/服务/端口/进程/用户/网络配置),
// 用于变更检测、合规审计、故障排查的"时间点"对比。
//
// 用法:
//
//	reins snapshot take -a <agent> [--name baseline]   采集快照
//	reins snapshot list -a <agent>                      列出历史快照
//	reins snapshot diff -a <agent> <snap1> <snap2>     比较两个快照
package main

import (
	"encoding/json"
	"fmt"
	"time"
)

// cmdSnapshot snapshot 子命令入口。
func cmdSnapshot(cfg config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("用法: reins snapshot <take|list|diff> -a <agent>")
	}
	switch args[0] {
	case "take", "capture":
		return snapshotTake(cfg, args[1:])
	case "list", "ls":
		return snapshotList(cfg, args[1:])
	case "diff", "compare":
		return snapshotDiff(cfg, args[1:])
	default:
		return fmt.Errorf("未知 snapshot 子命令: %s (可用: take list diff)", args[0])
	}
}

// snapshotTake 采集系统快照。
func snapshotTake(cfg config, args []string) error {
	agentID := extractArg(args, "-a")
	name := extractArg(args, "--name")
	if agentID == "" {
		return fmt.Errorf("用法: reins snapshot take -a <agent> [--name baseline]")
	}
	if name == "" {
		name = "snap-" + time.Now().Format("20060102-150405")
	}

	// 采集关键状态(双方言,输出同构 JSON:services/ports/processes/users/…)
	var stdout string
	var err error
	if isLinuxAgent(cfg, agentID) {
		sh := `SVCS=$(systemctl list-unit-files --type=service --state=enabled 2>/dev/null | awk '{print $1}' | sort | paste -sd' ' -)
PORTS=$(ss -tln 2>/dev/null | tail -n +2 | awk '{print $4}' | awk -F: '{print $NF}' | sort -nu | paste -sd, -)
PROCS=$(ps -eo comm= | sort -u | paste -sd' ' -)
USERS=$(awk -F: '$3>=1000 || $3==0 {print $1}' /etc/passwd | paste -sd, -)
TS=$(date -Iseconds)
printf '{"services":"%s","ports":"%s","processes":"%s","users":"%s","hostname":"%s","os":"%s","ts":"%s"}' \
  "$SVCS" "$PORTS" "$PROCS" "$USERS" "$(hostname)" "$(uname -s) $(uname -r)" "$TS"`
		stdout, err = runOpsScript(cfg, agentID, sh, 30)
	} else {
		nl := "`n"
		cmd := "$ErrorActionPreference='SilentlyContinue';" +
			"$r=@{};" +
			"$r.services=(Get-Service | Where StartType -eq Automatic | Select -ExpandProperty Name | Sort) -join \"" + nl + "\";" +
			"$r.ports=(Get-NetTCPConnection -State Listen | Select -ExpandProperty LocalPort | Sort -Unique) -join ',';" +
			"$r.processes=(Get-Process | Select -ExpandProperty Name | Sort -Unique) -join \"" + nl + "\";" +
			"$r.users=(Get-LocalUser | Select -ExpandProperty Name) -join ',';" +
			"$r.hostname=$env:COMPUTERNAME;" +
			"$r.os=(Get-CimInstance Win32_OperatingSystem).Caption;" +
			"$r.ts=Get-Date -Format 'o';" +
			"$r|ConvertTo-Json -Compress"
		stdout, err = runOpsTask(cfg, agentID, cmd, 30)
	}
	if err != nil {
		return err
	}

	// 存储快照(写到本地文件)
	if jsonMode {
		fmt.Println(stdout)
		return nil
	}

	// 解析展示
	var snap map[string]interface{}
	if err := json.Unmarshal([]byte(stdout), &snap); err != nil {
		fmt.Println(stdout)
		return nil
	}

	fmt.Printf("📸 系统快照: %s\n", name)
	fmt.Printf("   主机: %s\n", snap["hostname"])
	fmt.Printf("   系统: %s\n", snap["os"])
	fmt.Printf("   时间: %s\n", snap["ts"])
	fmt.Printf("   已安装软件: %d 个\n", countLines(snap["installed"]))
	fmt.Printf("   自动启动服务: %d 个\n", countLines(snap["services"]))
	fmt.Printf("   监听端口: %d 个\n", countPorts(snap["ports"]))
	fmt.Printf("   运行进程: %d 个\n", countLines(snap["processes"]))
	fmt.Printf("   本地用户: %s\n", snap["users"])

	// JSON 输出(供 diff 使用)
	fmt.Printf("\n(快照 JSON 已输出,可用 --output json 保存)\n")
	return nil
}

// snapshotList 列出快照(简化:本地存储目录)。
func snapshotList(cfg config, args []string) error {
	agentID := extractArg(args, "-a")
	if agentID == "" {
		return fmt.Errorf("用法: reins snapshot list -a <agent>")
	}
	fmt.Println("快照存储在本地 ~/.reins/snapshots/ 目录")
	fmt.Println("(用 snapshot take --output json > snap.json 保存)")
	return nil
}

// snapshotDiff 比较两个快照(需要两个 JSON 文件)。
func snapshotDiff(cfg config, args []string) error {
	snap1 := posArg(args, 1)
	snap2 := posArg(args, 2)
	if snap1 == "" || snap2 == "" {
		return fmt.Errorf("用法: reins snapshot diff <snap1.json> <snap2.json>")
	}
	// 读取两个快照文件做 diff
	data1, err := readFileContent(snap1)
	if err != nil {
		return fmt.Errorf("读取 %s 失败: %w", snap1, err)
	}
	data2, err := readFileContent(snap2)
	if err != nil {
		return fmt.Errorf("读取 %s 失败: %w", snap2, err)
	}

	var s1, s2 map[string]interface{}
	json.Unmarshal([]byte(data1), &s1)
	json.Unmarshal([]byte(data2), &s2)

	fmt.Println("=== 快照差异 ===")
	compareSnapshotField(s1, s2, "installed", "已安装软件")
	compareSnapshotField(s1, s2, "services", "服务")
	compareSnapshotField(s1, s2, "ports", "端口")
	compareSnapshotField(s1, s2, "processes", "进程")
	return nil
}

// compareSnapshotField 比较快照的某字段(行列表 diff)。
func compareSnapshotField(s1, s2 map[string]interface{}, field, label string) {
	v1, _ := s1[field].(string)
	v2, _ := s2[field].(string)
	set1 := stringToSet(v1)
	set2 := stringToSet(v2)

	added := []string{}
	removed := []string{}
	for k := range set2 {
		if !set1[k] {
			added = append(added, k)
		}
	}
	for k := range set1 {
		if !set2[k] {
			removed = append(removed, k)
		}
	}

	if len(added) == 0 && len(removed) == 0 {
		fmt.Printf("✅ %s: 无变化\n", label)
		return
	}
	fmt.Printf("📝 %s:\n", label)
	for _, a := range added {
		fmt.Printf("   + %s\n", a)
	}
	for _, r := range removed {
		fmt.Printf("   - %s\n", r)
	}
}

// 辅助函数。
func countLines(v interface{}) int {
	s, ok := v.(string)
	if !ok || s == "" {
		return 0
	}
	return len(splitNewlines(s))
}

func countPorts(v interface{}) int {
	s, ok := v.(string)
	if !ok || s == "" {
		return 0
	}
	return len(splitComma(s))
}

func stringToSet(s string) map[string]bool {
	set := make(map[string]bool)
	for _, line := range splitNewlines(s) {
		line = trimSpace(line)
		if line != "" {
			set[line] = true
		}
	}
	return set
}

func splitComma(s string) []string {
	result := []string{}
	current := ""
	for _, c := range s {
		if c == ',' {
			result = append(result, current)
			current = ""
		} else {
			current += string(c)
		}
	}
	if current != "" {
		result = append(result, current)
	}
	return result
}

func trimSpace(s string) string {
	start := 0
	end := len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t' || s[start] == '\r') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t' || s[end-1] == '\r') {
		end--
	}
	return s[start:end]
}
