// CLI task stream 命令 — 流式任务输出(F2)。
//
// 下发任务并通过 SSE 实时接收 stdout/进度/结果,无需轮询。
// 类似 kubectl logs -f。
//
// 用法:
//
//	go-rmm task stream -a <agent> -c "Get-Process | Sort CPU -Desc | Select -First 10"
//	go-rmm task stream -a <agent> -c "ping -t 8.8.8.8" --timeout 60
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/QiuShichang/go-rmm/proto"
)

// taskStream 下发流式任务(POST /api/tasks/stream + SSE 实时输出)。
func taskStream(cfg config, args []string) error {
	fs := flag.NewFlagSet("task stream", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	agentID := fs.String("a", "", "目标 Agent ID(必填)")
	command := fs.String("c", "", "PowerShell 命令")
	timeout := fs.Int("timeout", 60, "超时秒数")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *agentID == "" || *command == "" {
		return fmt.Errorf("用法: go-rmm task stream -a <agent> -c <command>")
	}

	taskID := "stream-" + fmt.Sprint(time.Now().UnixNano())
	payload, _ := json.Marshal(proto.PowerShellTask{Command: *command})

	// POST /api/tasks/stream(SSE 响应)
	body := proto.TaskRequest{
		TaskID:  taskID,
		Type:    "powershell",
		AgentID: *agentID,
		Payload: payload,
		Timeout: *timeout,
	}
	bodyJSON, _ := json.Marshal(body)

	u := cfg.relay + "/api/tasks/stream"
	req, err := http.NewRequest("POST", u, bytes.NewReader(bodyJSON))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.token)
	}
	req.Header.Set("Accept", "text/event-stream")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	req = req.WithContext(ctx)

	resp, err := streamingClient.Do(req)
	if err != nil {
		return fmt.Errorf("连接失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, readError(resp.Body))
	}

	fmt.Fprintf(os.Stderr, "📡 流式任务 (task=%s, Ctrl+C 中止)\n\n", taskID)

	scanner := newSSEScanner(resp.Body)
	for {
		ev, err := scanner.Next()
		if err != nil {
			return nil
		}
		if ctx.Err() != nil {
			return nil
		}
		switch ev.event {
		case "task_submitted":
			fmt.Fprintf(os.Stderr, "✅ 任务已提交\n")
		case "task_dispatched":
			fmt.Fprintf(os.Stderr, "📤 已投递到 Agent\n\n")
		case "task_progress":
			var outer struct {
				Data json.RawMessage `json:"data"`
			}
			json.Unmarshal(ev.data, &outer)
			var p proto.TaskProgress
			json.Unmarshal(outer.Data, &p)
			if p.Message != "" {
				fmt.Printf("[进度] %s (%d%%)\n", p.Message, p.Percent)
			}
		case "task_result":
			var outer struct {
				Data json.RawMessage `json:"data"`
			}
			json.Unmarshal(ev.data, &outer)
			var r proto.TaskResult
			json.Unmarshal(outer.Data, &r)
			fmt.Printf("\n=== 结果 ===\n")
			fmt.Printf("成功: %v  退出码: %d  耗时: %dms\n", r.Success, r.ExitCode, r.Duration)
			if r.Stdout != "" {
				fmt.Printf("--- stdout ---\n%s\n", filterCLIXML(r.Stdout))
			}
			if r.Stderr != "" {
				fmt.Printf("--- stderr ---\n%s\n", filterCLIXML(r.Stderr))
			}
			return nil
		case "error":
			return fmt.Errorf("错误: %s", string(ev.data))
		case "timeout":
			return fmt.Errorf("任务超时")
		}
	}
}
