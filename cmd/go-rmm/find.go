// CLI find 命令 — 远端文件查找/搜索(F6 扩展)。
//
// 用途:在远端 Agent 上查找文件、搜索文件内容、检查文件属性。
//
// 用法:
//
//	go-rmm find name -a <agent> <pattern> [--dir C:\] [--max 50]
//	go-rmm find grep -a <agent> <pattern> [--dir C:\logs] [--ext .log]
//	go-rmm find stat -a <agent> <file>              (文件属性)
//	go-rmm find du -a <agent> <dir>                  (目录大小)
package main

import (
	"fmt"
)

// cmdFind find 子命令入口。
func cmdFind(cfg config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("用法: go-rmm find <name|grep|stat|du> -a <agent> [args]")
	}
	switch args[0] {
	case "name", "search":
		return findName(cfg, args[1:])
	case "grep", "content":
		return findGrep(cfg, args[1:])
	case "stat", "info":
		return findStat(cfg, args[1:])
	case "du", "size":
		return findDu(cfg, args[1:])
	default:
		return fmt.Errorf("未知 find 子命令: %s (可用: name grep stat du)", args[0])
	}
}

// findName 按文件名查找。
func findName(cfg config, args []string) error {
	agentID := extractArg(args, "-a")
	pattern := posArg(args, 1)
	if agentID == "" || pattern == "" {
		return fmt.Errorf("用法: go-rmm find name -a <agent> <pattern> [--dir C:\\] [--max 50]")
	}
	dir := extractArg(args, "--dir")
	if dir == "" {
		dir = "C:\\"
	}
	max := 50
	if m := extractArg(args, "--max"); m != "" {
		fmt.Sscanf(m, "%d", &max)
	}
	// Windows: Get-ChildItem -Recurse -Filter; Linux: find
	cmd := fmt.Sprintf("Get-ChildItem -Path '%s' -Recurse -Filter '*%s*' -File -ErrorAction SilentlyContinue | Select -First %d FullName,Length,LastWriteTime | Format-Table -AutoSize | Out-String", dir, pattern, max)
	stdout, err := runOpsTask(cfg, agentID, cmd, 30)
	if err != nil {
		cmd = fmt.Sprintf("find '%s' -name '*%s*' -type f 2>/dev/null | head -%d", dir, pattern, max)
		stdout, err = runOpsTask(cfg, agentID, cmd, 30)
		if err != nil {
			return err
		}
	}
	fmt.Println(stdout)
	return nil
}

// findGrep 在文件内容中搜索。
func findGrep(cfg config, args []string) error {
	agentID := extractArg(args, "-a")
	pattern := posArg(args, 1)
	if agentID == "" || pattern == "" {
		return fmt.Errorf("用法: go-rmm find grep -a <agent> <pattern> [--dir C:\\logs] [--ext .log]")
	}
	dir := extractArg(args, "--dir")
	if dir == "" {
		dir = "."
	}
	ext := extractArg(args, "--ext")
	if ext == "" {
		ext = "*.log"
	} else {
		ext = "*" + ext
	}
	// Windows: Select-String; Linux: grep -r
	cmd := fmt.Sprintf("Get-ChildItem -Path '%s' -Filter '%s' -Recurse -File | Select-String -Pattern '%s' | Select Filename,LineNumber,Line | Format-Table -AutoSize -Wrap | Out-String", dir, ext, pattern)
	stdout, err := runOpsTask(cfg, agentID, cmd, 60)
	if err != nil {
		cmd = fmt.Sprintf("grep -rn '%s' %s/%s 2>/dev/null | head -50", pattern, dir, ext)
		stdout, err = runOpsTask(cfg, agentID, cmd, 60)
		if err != nil {
			return err
		}
	}
	fmt.Println(stdout)
	return nil
}

// findStat 文件属性。
func findStat(cfg config, args []string) error {
	agentID := extractArg(args, "-a")
	filePath := posArg(args, 1)
	if agentID == "" || filePath == "" {
		return fmt.Errorf("用法: go-rmm find stat -a <agent> <file>")
	}
	cmd := fmt.Sprintf("Get-Item '%s' | Select Name,FullName,Length,CreationTime,LastWriteTime,Attributes | Format-List | Out-String", filePath)
	stdout, err := runOpsTask(cfg, agentID, cmd, 10)
	if err != nil {
		cmd = fmt.Sprintf("stat '%s' 2>/dev/null || ls -la '%s'", filePath, filePath)
		stdout, err = runOpsTask(cfg, agentID, cmd, 10)
		if err != nil {
			return err
		}
	}
	fmt.Println(stdout)
	return nil
}

// findDu 目录大小。
func findDu(cfg config, args []string) error {
	agentID := extractArg(args, "-a")
	dir := posArg(args, 1)
	if agentID == "" || dir == "" {
		return fmt.Errorf("用法: go-rmm find du -a <agent> <dir>")
	}
	cmd := fmt.Sprintf("@{Name='%s'; SizeMB=[math]::Round((Get-ChildItem '%s' -Recurse -File -ErrorAction SilentlyContinue | Measure-Object Length -Sum).Sum/1MB,1)} | Format-Table -AutoSize | Out-String", dir, dir)
	stdout, err := runOpsTask(cfg, agentID, cmd, 30)
	if err != nil {
		cmd = fmt.Sprintf("du -sh '%s' 2>/dev/null", dir)
		stdout, err = runOpsTask(cfg, agentID, cmd, 30)
		if err != nil {
			return err
		}
	}
	fmt.Println(stdout)
	return nil
}

// containsStr 辅助:检查字符串是否在切片中。
func containsStr(slice []string, s string) bool {
	for _, x := range slice {
		if x == s {
			return true
		}
	}
	return false
}
