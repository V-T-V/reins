// CLI batch 子命令 —— 异步批量分发批次的查询与跟踪(P0-1)。
//
//	go-rmm batch start --tag env=prod -c "Get-Date"   # 提交异步批次(带重试+进度)
//	go-rmm batch status <batch_id>                     # 进度快照
//	go-rmm batch watch <batch_id>                      # 实时跟踪到终态
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func cmdBatch(cfg config, args []string) error {
	if len(args) == 0 {
		return errors.New("用法: go-rmm batch <start|status|watch> [args]")
	}
	switch args[0] {
	case "start":
		return batchStart(cfg, args[1:])
	case "status":
		return batchStatus(cfg, args[1:])
	case "watch":
		return batchWatch(cfg, args[1:])
	default:
		return fmt.Errorf("未知子命令: %s (可用: start status watch)", args[0])
	}
}

// batchStart POST /api/batch —— 提交异步批次。
func batchStart(cfg config, args []string) error {
	fs := flag.NewFlagSet("batch start", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	tag := fs.String("tag", "", "目标 Agent 标签(必填,如 env=prod)")
	taskType := fs.String("t", "powershell", "任务类型")
	command := fs.String("c", "", "PowerShell 命令")
	payloadFile := fs.String("f", "", "从 JSON 文件读取 payload")
	timeout := fs.Int("timeout", 60, "单任务超时秒数")
	maxRetries := fs.Int("retries", 2, "投递失败重试次数(指数退避)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *tag == "" {
		return errors.New("-tag <标签> 必填,如 -tag env=prod")
	}
	payload, err := buildPayload(*taskType, *command, "", *payloadFile)
	if err != nil {
		return err
	}
	body := map[string]any{
		"tag":         *tag,
		"type":        *taskType,
		"payload":     payload,
		"timeout":     *timeout,
		"max_retries": *maxRetries,
	}
	var resp struct {
		BatchID    string `json:"batch_id"`
		Total      int    `json:"total"`
		Progress   string `json:"progress"`
		MaxRetries int    `json:"max_retries"`
	}
	if err := apiPost(cfg, "/api/batch", body, &resp); err != nil {
		return err
	}
	if jsonMode {
		outputJSON(resp)
		return nil
	}
	fmt.Printf("✅ 批次已提交(异步)\n")
	fmt.Printf("   批次 ID: %s\n", resp.BatchID)
	fmt.Printf("   目标数: %d\n", resp.Total)
	fmt.Printf("   跟踪:   go-rmm batch watch %s\n", resp.BatchID)
	return nil
}

// batchStatus GET /api/batch/{id} —— 单次快照。
func batchStatus(cfg config, args []string) error {
	if len(args) == 0 {
		return errors.New("用法: go-rmm batch status <batch_id>")
	}
	snap, err := fetchBatch(cfg, args[0])
	if err != nil {
		return err
	}
	if jsonMode {
		outputJSON(snap)
		return nil
	}
	printBatchProgress(snap)
	return nil
}

// batchWatch 轮询跟踪到终态(比 SSE 简单可靠,输出同一条进度行刷新)。
func batchWatch(cfg config, args []string) error {
	if len(args) == 0 {
		return errors.New("用法: go-rmm batch watch <batch_id>")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if !jsonMode {
		fmt.Printf("跟踪批次 %s (Ctrl+C 中止)...\n", args[0])
	}
	for {
		snap, err := fetchBatch(cfg, args[0])
		if err != nil {
			return err
		}
		if jsonMode {
			if status, _ := snap["status"].(string); status == "finished" {
				outputJSON(snap)
				return nil
			}
		} else {
			printBatchProgress(snap)
		}
		if status, _ := snap["status"].(string); status == "finished" {
			return nil
		}
		select {
		case <-ctx.Done():
			if !jsonMode {
				fmt.Println("\n已中断(可用 batch status 稍后查询)")
			}
			return nil
		case <-time.After(1 * time.Second):
		}
	}
}

// fetchBatch 拉取批次快照(宽松 map,字段由 Relay 定义)。
func fetchBatch(cfg config, batchID string) (map[string]any, error) {
	var snap map[string]any
	if err := apiGet(cfg, "/api/batch/"+url.PathEscape(batchID), &snap); err != nil {
		return nil, err
	}
	return snap, nil
}

// printBatchProgress 人类可读进度行。
func printBatchProgress(snap map[string]any) {
	status, _ := snap["status"].(string)
	total, _ := snap["total"].(float64)
	ok, _ := snap["succeeded"].(float64)
	fail, _ := snap["failed"].(float64)
	pending, _ := snap["pending"].(float64)
	pct, _ := snap["percent"].(float64)
	eta, _ := snap["eta_ms"].(float64)
	fmt.Printf("\r[%s] %v/%v 成功 %v | 失败 %v | 待投 %v | %.0f%% | ETA %vms   ",
		statusLabel(status), ok+fail, total, ok, fail, pending, pct, eta)
	if status == "finished" {
		fmt.Println()
		if sr, okv := snap["success_rate"].(float64); okv {
			fmt.Printf("完成: 成功率 %.0f%%\n", sr*100)
		}
	}
}

func statusLabel(s string) string {
	if s == "finished" {
		return "✅ 完成"
	}
	return "⏳ 运行"
}
