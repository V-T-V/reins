// CLI events 命令 — 实时事件流订阅(F1)。
//
// 订阅 Relay SSE 事件流,实时显示 agent 上线/离线、任务进度/结果、shell 输出。
// 类似 kubectl get events -f / docker events。
//
// 用法:
//
//	reins events                          # 订阅所有事件
//	reins events --filter task_result     # 只看任务结果
//	reins events --filter agent_online    # 只看 Agent 上线
//	reins events --json                   # JSON 输出(管道友好)
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// cmdEvents events 子命令入口。
func cmdEvents(cfg config, args []string) error {
	filter := ""
	jsonOut := false
	for _, a := range args {
		if a == "--json" || a == "-j" {
			jsonOut = true
		}
		if len(a) > 8 && a[:8] == "--filter" {
			if len(a) > 9 && a[8] == '=' {
				filter = a[9:]
			}
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	u := cfg.relay + "/api/events"
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return err
	}
	if cfg.token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.token)
	}
	req.Header.Set("Accept", "text/event-stream")

	fmt.Fprintf(os.Stderr, "📡 实时事件流 (filter=%s, Ctrl+C 退出)\n\n", filter)

	resp, err := streamingClient.Do(req)
	if err != nil {
		return fmt.Errorf("SSE 连接失败: %w", err)
	}
	defer resp.Body.Close()

	scanner := newSSEScanner(resp.Body)
	for {
		ev, err := scanner.Next()
		if err != nil {
			return nil
		}
		if ctx.Err() != nil {
			return nil
		}
		// hello 事件跳过
		if ev.event == "hello" {
			continue
		}
		// 过滤
		if filter != "" && ev.event != filter {
			continue
		}
		// 输出
		if jsonOut {
			fmt.Printf(`{"event":"%s","data":%s,"ts":"%s"}`+"\n", ev.event, ev.data, time.Now().Format(time.RFC3339))
		} else {
			printEvent(ev.event, ev.data)
		}
	}
}

// printEvent 彩色打印事件。
func printEvent(event string, data []byte) {
	ts := time.Now().Format("15:04:05")
	// 解析外层结构
	var outer struct {
		AgentID string          `json:"agent_id"`
		Data    json.RawMessage `json:"data"`
	}
	json.Unmarshal(data, &outer)

	switch event {
	case "agent_online":
		var reg struct {
			Hostname string   `json:"hostname"`
			Caps     []string `json:"capabilities"`
		}
		json.Unmarshal(outer.Data, &reg)
		fmt.Printf("%s 🟢 %s 上线 (%s)\n", ts, outer.AgentID, reg.Hostname)

	case "agent_offline":
		fmt.Printf("%s 🔴 %s 离线\n", ts, outer.AgentID)

	case "task_progress":
		var p struct {
			TaskID  string `json:"task_id"`
			Step    string `json:"step"`
			Message string `json:"message"`
			Percent int    `json:"percent"`
		}
		json.Unmarshal(outer.Data, &p)
		fmt.Printf("%s ⏳ [%s] %s %d%%\n", ts, p.TaskID[:12], p.Message, p.Percent)

	case "task_result":
		var r struct {
			TaskID  string `json:"task_id"`
			Success bool   `json:"success"`
			Stdout  string `json:"stdout"`
		}
		json.Unmarshal(outer.Data, &r)
		status := "✅"
		if !r.Success {
			status = "❌"
		}
		stdout := r.Stdout
		if len(stdout) > 200 {
			stdout = stdout[:200] + "..."
		}
		fmt.Printf("%s %s [%s] %s\n", ts, status, r.TaskID[:12], stdout)

	case "shell_output":
		fmt.Printf("%s 💬 %s: shell 输出\n", ts, outer.AgentID)

	case "task_blocked":
		fmt.Printf("%s 🚫 策略拦截: %s\n", ts, string(outer.Data))

	default:
		// 其他事件类型
		preview := string(data)
		if len(preview) > 120 {
			preview = preview[:120] + "..."
		}
		fmt.Printf("%s 📋 %s: %s\n", ts, event, preview)
	}
}
