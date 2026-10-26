// CLI dag 子命令 —— DAG 编排的提交与查询(v4.6)。
//
//	reins dag apply -f flow.json [-name 升级流程] [-concurrency 2]  # 提交编排(异步)
//	reins dag list                                                  # 编排列表
//	reins dag status <dag_id> [-w]                                  # 状态快照(-w 轮询到终态)
//
// flow.json 结构(与 POST /api/dag 请求体同款):
//
//	{"name":"upgrade","max_concurrency":2,"nodes":[
//	  {"id":"backup","tag":"env=prod","type":"powershell","payload":{"command":"..."},"timeout":60},
//	  {"id":"upgrade","deps":["backup"],"tag":"env=prod","type":"powershell","payload":{"command":"..."}},
//	  {"id":"alert","deps":["upgrade"],"agent_id":"ops-1","type":"http","payload":{},"continue_on_error":true}
//	]}
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"time"
)

func cmdDAG(cfg config, args []string) error {
	if len(args) == 0 {
		return errors.New("用法: reins dag <apply|list|status> [args]")
	}
	switch args[0] {
	case "apply":
		return dagApply(cfg, args[1:])
	case "list":
		return dagList(cfg, args[1:])
	case "status":
		return dagStatusCmd(cfg, args[1:])
	default:
		return fmt.Errorf("未知子命令: %s (可用: apply list status)", args[0])
	}
}

// dagApply POST /api/dag —— 从 JSON 文件提交 DAG 编排。
func dagApply(cfg config, args []string) error {
	fs := flag.NewFlagSet("dag apply", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	file := fs.String("f", "", "DAG 定义 JSON 文件(必填)")
	name := fs.String("name", "", "编排名(覆盖文件内 name)")
	concurrency := fs.Int("concurrency", 0, "并发上限(覆盖文件内 max_concurrency)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *file == "" {
		return errors.New("-f <DAG 定义文件> 必填")
	}
	raw, err := os.ReadFile(*file)
	if err != nil {
		return fmt.Errorf("读取 DAG 定义失败: %w", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return fmt.Errorf("解析 DAG 定义失败: %w", err)
	}
	if _, ok := body["nodes"]; !ok {
		return errors.New("DAG 定义缺少 nodes 字段")
	}
	if *name != "" {
		body["name"] = *name
	}
	if *concurrency > 0 {
		body["max_concurrency"] = *concurrency
	}
	var resp struct {
		DagID     string `json:"dag_id"`
		Name      string `json:"name"`
		NodeCount int    `json:"node_count"`
		Status    string `json:"status"`
	}
	if err := apiPost(cfg, "/api/dag", body, &resp); err != nil {
		return err
	}
	if jsonMode {
		outputJSON(resp)
		return nil
	}
	fmt.Printf("✅ 编排已提交(异步)\n")
	fmt.Printf("   编排 ID: %s\n", resp.DagID)
	fmt.Printf("   名称:   %s(%d 节点)\n", resp.Name, resp.NodeCount)
	fmt.Printf("   跟踪:   reins dag status %s -w\n", resp.DagID)
	return nil
}

// dagList GET /api/dag —— 编排列表。
func dagList(cfg config, args []string) error {
	_ = args
	var resp struct {
		Dags []struct {
			ID    string `json:"dag_id"`
			Name  string `json:"name"`
			State string `json:"state"`
		} `json:"dags"`
		Count int `json:"count"`
	}
	if err := apiGet(cfg, "/api/dag", &resp); err != nil {
		return err
	}
	if jsonMode {
		outputJSON(resp)
		return nil
	}
	if resp.Count == 0 {
		fmt.Println("(无编排记录)")
		return nil
	}
	fmt.Printf("%-32s %-10s %s\n", "DAG_ID", "STATE", "NAME")
	for _, d := range resp.Dags {
		fmt.Printf("%-32s %-10s %s\n", d.ID, d.State, d.Name)
	}
	return nil
}

// dagStatusCmd GET /api/dag/{id} —— 状态快照;-w 轮询到终态。
func dagStatusCmd(cfg config, args []string) error {
	fs := flag.NewFlagSet("dag status", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	watch := fs.Bool("w", false, "轮询直到终态")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) == 0 {
		return errors.New("用法: reins dag status <dag_id> [-w]")
	}
	dagID := rest[0]
	for {
		snap, err := fetchDAG(cfg, dagID)
		if err != nil {
			return err
		}
		if jsonMode {
			if status, _ := snap["status"].(string); status == "finished" || !*watch {
				outputJSON(snap)
				return nil
			}
		} else {
			printDAGStatus(snap)
		}
		if status, _ := snap["status"].(string); status == "finished" || !*watch {
			return nil
		}
		time.Sleep(1 * time.Second)
	}
}

// fetchDAG 拉取编排快照(宽松 map,字段由 Relay 定义)。
func fetchDAG(cfg config, dagID string) (map[string]any, error) {
	var snap map[string]any
	if err := apiGet(cfg, "/api/dag/"+url.PathEscape(dagID), &snap); err != nil {
		return nil, err
	}
	return snap, nil
}

// printDAGStatus 人类可读的编排状态(逐节点)。
func printDAGStatus(snap map[string]any) {
	status, _ := snap["status"].(string)
	name, _ := snap["name"].(string)
	fmt.Printf("编排 %s [%s] %s\n", name, status, snap["dag_id"])
	if status == "finished" {
		fmt.Printf("汇总: 成功 %.0f | 失败 %.0f | 跳过 %.0f | start_order=%v\n",
			snap["succeeded"], snap["failed"], snap["skipped"], snap["start_order"])
	}
	nodes, _ := snap["nodes"].([]any)
	for _, n := range nodes {
		m, _ := n.(map[string]any)
		id, _ := m["id"].(string)
		st, _ := m["status"].(string)
		line := fmt.Sprintf("  %-16s %s", id, st)
		if targets, _ := m["targets"].([]any); len(targets) > 0 {
			line += fmt.Sprintf(" targets=%v", targets)
		}
		if errText, _ := m["err"].(string); errText != "" {
			line += " err=" + errText
		}
		fmt.Println(line)
	}
}
