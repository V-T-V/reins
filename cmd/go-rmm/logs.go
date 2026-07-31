// CLI logs 命令 — 远端日志文件实时 tail(类似 kubectl logs -f)。
//
// 用法:
//
//	go-rmm logs tail -a <agent> <file> [--lines 100] [--filter "ERROR"]
//	go-rmm logs read -a <agent> <file> [--offset 0] [--limit 1000]
//	go-rmm logs list -a <agent> <dir>      (列出目录下日志文件)
//	go-rmm logs search -a <agent> <file> <pattern> [--max 50]
//
// 场景:排查 Agent 侧应用的实时日志,无需 SSH 登录。
package main

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/QiuShichang/go-rmm/proto"
)

// cmdLogs logs 子命令入口。
func cmdLogs(cfg config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("用法: go-rmm logs <tail|read|list|search> -a <agent> [file]")
	}
	switch args[0] {
	case "tail", "follow", "f":
		return logsTail(cfg, args[1:])
	case "read", "cat":
		return logsRead(cfg, args[1:])
	case "list", "ls":
		return logsList(cfg, args[1:])
	case "search", "grep":
		return logsSearch(cfg, args[1:])
	default:
		return fmt.Errorf("未知 logs 子命令: %s (可用: tail read list search)", args[0])
	}
}

// logsRead 读取日志文件(一次性)。
func logsRead(cfg config, args []string) error {
	agentID := extractArg(args, "-a")
	filePath := posArg(args, 1)
	if agentID == "" || filePath == "" {
		return fmt.Errorf("用法: go-rmm logs read -a <agent> <file> [--limit 1000]")
	}
	limit := 1000
	if l := extractArg(args, "--limit"); l != "" {
		fmt.Sscanf(l, "%d", &limit)
	}
	// 用 Get-Content -Tail 读最后 N 行
	cmd := fmt.Sprintf("Get-Content '%s' -Tail %d -ErrorAction SilentlyContinue", filePath, limit)
	stdout, err := runOpsTask(cfg, agentID, cmd, 15)
	if err != nil {
		// Linux fallback
		cmd = fmt.Sprintf("tail -n %d '%s' 2>/dev/null", limit, filePath)
		stdout, err = runOpsTask(cfg, agentID, cmd, 15)
		if err != nil {
			return err
		}
	}
	fmt.Println(stdout)
	return nil
}

// logsTail 实时 tail 日志(用 shell 会话 + Get-Content -Wait)。
func logsTail(cfg config, args []string) error {
	agentID := extractArg(args, "-a")
	filePath := posArg(args, 1)
	if agentID == "" || filePath == "" {
		return fmt.Errorf("用法: go-rmm logs tail -a <agent> <file> [--lines 50] [--filter ERROR]")
	}
	lines := 50
	if l := extractArg(args, "--lines"); l != "" {
		fmt.Sscanf(l, "%d", &lines)
	}
	filter := extractArg(args, "--filter")

	// 启动一个 shell 会话,执行 Get-Content -Wait(或 tail -f)
	var cmd string
	if filter != "" {
		cmd = fmt.Sprintf("Get-Content '%s' -Tail %d -Wait | Where-Object { $_ -match '%s' }", filePath, lines, filter)
	} else {
		cmd = fmt.Sprintf("Get-Content '%s' -Tail %d -Wait", filePath, lines)
	}
	// Linux fallback
	linuxCmd := fmt.Sprintf("tail -n %d -f '%s'", lines, filePath)
	if filter != "" {
		linuxCmd = fmt.Sprintf("tail -n %d -f '%s' | grep --line-buffered '%s'", lines, filePath, filter)
	}

	// 用 shell open 会话执行(持久输出)
	fmt.Printf("📡 实时 tail: %s (agent=%s, lines=%d", filePath, agentID, lines)
	if filter != "" {
		fmt.Printf(", filter=%s", filter)
	}
	fmt.Println(")")
	fmt.Println("   Ctrl+C 停止")

	// 简化:用 task run 下发命令(超时控制而非真正持久)
	// 真正持久需要 shell 会话,这里用长超时 task 模拟
	timeout := 300 // 5 分钟
	cmdFull := fmt.Sprintf("%s 2>$null; if ($LASTEXITCODE -ne 0) { %s }", cmd, linuxCmd)
	payload, _ := json.Marshal(struct {
		Command string `json:"command"`
	}{Command: cmdFull})
	taskID := "logs-" + fmt.Sprint(time.Now().UnixNano())
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
	}
	if err := apiPost(cfg, "/api/tasks", body, &resp); err != nil {
		return err
	}
	// 等结果(持续输出)
	return waitAndPrint(cfg, taskID, timeout)
}

// waitAndPrint 轮询任务结果并打印。
func waitAndPrint(cfg config, taskID string, timeout int) error {
	for i := 0; i < timeout+10; i++ {
		var r struct {
			Task *struct {
				Status string `json:"status"`
			} `json:"task"`
			Result *struct {
				Stdout  string `json:"stdout"`
				Success bool   `json:"success"`
			} `json:"result"`
		}
		if err := apiGet(cfg, "/api/tasks/"+taskID, &r); err == nil && r.Task != nil {
			if r.Task.Status == proto.StatusCompleted || r.Task.Status == proto.StatusFailed {
				if r.Result != nil {
					fmt.Print(r.Result.Stdout)
				}
				return nil
			}
		}
		sleepDuration(1)
	}
	return fmt.Errorf("超时")
}

// logsList 列出目录下的日志文件。
func logsList(cfg config, args []string) error {
	agentID := extractArg(args, "-a")
	dir := posArg(args, 1)
	if agentID == "" {
		return fmt.Errorf("用法: go-rmm logs list -a <agent> [dir]")
	}
	if dir == "" {
		dir = "."
	}
	cmd := fmt.Sprintf("Get-ChildItem '%s' -Filter *.log | Select Name,Length,LastWriteTime | Format-Table -AutoSize | Out-String", dir)
	stdout, err := runOpsTask(cfg, agentID, cmd, 15)
	if err != nil {
		cmd = fmt.Sprintf("ls -lh %s/*.log 2>/dev/null", dir)
		stdout, err = runOpsTask(cfg, agentID, cmd, 15)
		if err != nil {
			return err
		}
	}
	fmt.Println(stdout)
	return nil
}

// logsSearch 在日志文件中搜索模式。
func logsSearch(cfg config, args []string) error {
	agentID := extractArg(args, "-a")
	filePath := posArg(args, 1)
	pattern := posArg(args, 2)
	if agentID == "" || filePath == "" || pattern == "" {
		return fmt.Errorf("用法: go-rmm logs search -a <agent> <file> <pattern> [--max 50]")
	}
	max := 50
	if m := extractArg(args, "--max"); m != "" {
		fmt.Sscanf(m, "%d", &max)
	}
	cmd := fmt.Sprintf("Select-String -Path '%s' -Pattern '%s' | Select -First %d | ForEach-Object { $_.Line }", filePath, pattern, max)
	stdout, err := runOpsTask(cfg, agentID, cmd, 30)
	if err != nil {
		cmd = fmt.Sprintf("grep -m %d '%s' '%s' 2>/dev/null", max, pattern, filePath)
		stdout, err = runOpsTask(cfg, agentID, cmd, 30)
		if err != nil {
			return err
		}
	}
	fmt.Println(stdout)
	return nil
}

// sleepDuration 辅助。
func sleepDuration(secs int) { time.Sleep(time.Duration(secs) * time.Second) }
