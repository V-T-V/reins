// CLI rules 命令 —— 项目规则(.reins/rules.md)加载与 ack 管理。
//
// 被部署项目在项目根放 .reins/rules.md 写定制运维规则。AI/自动化运维
// 用 reins 操作该项目前必须先 `rules read` 加载规则:
//
//	reins rules read -a <agent>              # 自动发现并读全部规则
//	reins rules read -a <agent> -p D:\proj   # 只读指定项目的规则
//
// 读取成功后 ack(规则内容指纹)自动缓存,后续 task run/exec/deploy 提交
// 任务时自动携带 rules_ack;Agent 侧 rules_gate 门禁校验一致才放行,
// 规则文件变更后旧 ack 失效(强制重新加载)。
//
// 本地模式:若 CLI 当前目录就是项目仓库(存在 .reins/rules.md),变更类
// 命令会把本地规则全文打印到 stderr(AI 上下文可见)并以其指纹作为 ack
// ——规则随仓库走,部署后本地与远端内容一致即通过门禁。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/QiuShichang/reins/executors"
	"github.com/QiuShichang/reins/proto"
)

// localRulesRelPath 本地项目规则的相对位置(相对 CLI 当前目录)。
const localRulesRelPath = ".reins/rules.md"

// taskSubmitBody 提交任务的统一请求体。原先 taskRun/execOnAgent/
// runTaskSync 各持匿名结构,现收敛一处,并统一携带 rules_ack
// (Agent 侧 rules_gate 门禁的凭据)。
type taskSubmitBody struct {
	TaskID         string          `json:"task_id"`
	Type           string          `json:"type"`
	AgentID        string          `json:"agent_id"`
	Payload        json.RawMessage `json:"payload"`
	Timeout        int             `json:"timeout"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
	RulesAck       string          `json:"rules_ack,omitempty"`
}

// cmdRules rules 子命令入口。
func cmdRules(cfg config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("用法: reins rules <read> -a <agent> [-p <项目目录>]  (操作被部署项目前必须先 read 加载规则)")
	}
	switch args[0] {
	case "read", "load":
		return rulesRead(cfg, args[1:])
	default:
		return fmt.Errorf("未知 rules 子命令: %s (可用: read)", args[0])
	}
}

// rulesRead 读取远端项目规则并缓存 ack。
func rulesRead(cfg config, args []string) error {
	fs := flag.NewFlagSet("rules read", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "用法: reins rules read -a <agent> [-p <远端项目目录>]")
		fmt.Fprintln(os.Stderr, "  读取被部署项目的 .reins/rules.md 并缓存 ack;操作前必须加载并遵守规则")
		fs.PrintDefaults()
	}
	agentID := fs.String("a", "", "目标 Agent ID(必填)")
	projectDir := fs.String("p", "", "远端项目目录(空=自动发现全部规则文件)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *agentID == "" {
		return fmt.Errorf("-a <agent_id> 必填")
	}

	payload, err := json.Marshal(proto.FileTask{Op: "rules", Path: *projectDir})
	if err != nil {
		return err
	}
	taskID := "cli-rules-" + fmt.Sprint(uniqueNano())
	body := taskSubmitBody{
		TaskID: taskID, Type: proto.TaskFile, AgentID: *agentID,
		Payload: payload, Timeout: 60,
	}
	var resp struct {
		TaskID string `json:"task_id"`
		Status string `json:"status"`
	}
	if err := apiPostRetry(cfg, "/api/tasks", body, &resp, 2); err != nil {
		return fmt.Errorf("提交规则读取任务失败(旧版 Agent 不支持 rules op,请升级): %w", err)
	}
	result, err := pollTaskResult(cfg, taskID, 60)
	if err != nil {
		return err
	}
	if result == nil || !result.Success {
		msg := "无结果"
		if result != nil && result.Error != "" {
			msg = result.Error
		}
		return errTaskFailed("读取规则失败: " + msg)
	}

	// 解析 Data: {files:[...], ack}
	var data struct {
		Files []struct {
			Path    string `json:"path"`
			Bytes   int    `json:"bytes"`
			SHA16   string `json:"sha256_16"`
			Exists  bool   `json:"exists"`
			Error   string `json:"error"`
			Content string `json:"content"`
		} `json:"files"`
		Ack string `json:"ack"`
	}
	if len(result.Data) > 0 {
		if err := json.Unmarshal(result.Data, &data); err != nil {
			return fmt.Errorf("解析规则结果失败: %w", err)
		}
	}

	if jsonMode {
		outputJSON(map[string]any{"agent_id": *agentID, "files": data.Files, "ack": data.Ack})
	} else {
		printRulesBanner(*agentID, data.Files, data.Ack)
	}

	// 缓存 ack(空 ack 也缓存:表示该机无规则,门禁本就不拦,携带无意义)
	if data.Ack != "" {
		if err := saveAckCache(*agentID, data.Ack); err != nil {
			fmt.Fprintf(os.Stderr, "⚠️ ack 缓存写入失败(不影响本次读取): %v\n", err)
		}
	}
	return nil
}

// printRulesBanner 打印规则全文(text 模式)。
func printRulesBanner(agentID string, files []struct {
	Path    string `json:"path"`
	Bytes   int    `json:"bytes"`
	SHA16   string `json:"sha256_16"`
	Exists  bool   `json:"exists"`
	Error   string `json:"error"`
	Content string `json:"content"`
}, ack string) {
	line := strings.Repeat("═", 56)
	fmt.Println(line)
	fmt.Printf(" 📋 项目规则 (agent: %s) — 操作前必须加载并遵守\n", agentID)
	fmt.Println(line)
	if len(files) == 0 {
		fmt.Println(" 未发现任何项目规则文件(.reins/rules.md)")
		fmt.Println(" → 该机器未定义项目规则;如需定制,在项目根创建 .reins/rules.md")
		fmt.Println(line)
		return
	}
	for i, f := range files {
		fmt.Printf("\n [%d] %s", i+1, f.Path)
		if !f.Exists {
			fmt.Printf("  ✗ %s\n", f.Error)
			continue
		}
		fmt.Printf("  (%d B, sha256_16=%s)\n %s\n", f.Bytes, f.SHA16, strings.Repeat("─", 54))
		fmt.Println(f.Content)
		fmt.Println(strings.Repeat("─", 54))
	}
	if ack != "" {
		fmt.Printf("\n RULES_ACK: %s  (已缓存,后续 task run/exec/deploy 自动携带;\n", ack)
		fmt.Println("            规则文件变更后失效,需重新 rules read)")
	}
	fmt.Println(line)
}

// --- ack 缓存(按 agent 维度,存用户缓存目录) ---

// ackCachePath 返回 agent 的 ack 缓存文件路径。
func ackCachePath(agentID string) (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	safe := strings.NewReplacer("/", "_", "\\", "_", ":", "_").Replace(agentID)
	return filepath.Join(dir, "reins", "ack-"+safe+".txt"), nil
}

// saveAckCache 写入 ack 缓存。
func saveAckCache(agentID, ack string) error {
	path, err := ackCachePath(agentID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(ack), 0644)
}

// loadAckCache 读取 ack 缓存(无缓存返回空串)。
func loadAckCache(agentID string) string {
	path, err := ackCachePath(agentID)
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// resolveRulesAck 为发往 agentID 的任务解析 rules_ack。
//
// 优先级: 显式 ack > 本地项目规则(CWD/.reins/rules.md) > ack 缓存。
//
// printRules=true 时(单目标命令 task run/deploy run),本地规则全文打印到
// stderr——这是"AI 先加载规则再操作"的主通道:规则内容进入 AI 上下文,
// 无法只重放指纹不读内容。多目标命令(exec)静默携带。
func resolveRulesAck(agentID, explicit string, printRules bool) string {
	if explicit != "" {
		return explicit
	}
	if ack, content := localRules(); ack != "" {
		if printRules {
			fmt.Fprintf(os.Stderr, "\n⚠️ 已加载本地项目规则 %s (ack=%s) — 本命令及其后续操作必须遵守:\n%s\n%s\n%s\n",
				localRulesRelPath, ack, strings.Repeat("─", 56), content, strings.Repeat("─", 56))
		}
		return ack
	}
	return loadAckCache(agentID)
}

// localRules 读取 CLI 当前目录的项目规则(CWD 是项目仓库时)。
// 返回 (ack, content);无规则文件返回空串。
func localRules() (string, string) {
	b, err := os.ReadFile(localRulesRelPath)
	if err != nil || len(b) == 0 || len(b) > 1024*1024 {
		return "", ""
	}
	return executors.RulesAckForContent(b), string(b)
}

// pollTaskResult 轮询任务直到终态,返回结果(与 waitResult 语义一致但
// 返回结构化结果供调用方解析 Data)。
func pollTaskResult(cfg config, taskID string, timeoutSec int) (*proto.TaskResult, error) {
	deadline := time.Now().Add(time.Duration(timeoutSec+10) * time.Second)
	for {
		if time.Now().After(deadline) {
			return nil, errWaitTimeout("读取规则超时: " + taskID)
		}
		var resp struct {
			Task *struct {
				Status string `json:"status"`
			} `json:"task"`
			Result *proto.TaskResult `json:"result"`
		}
		if err := apiGet(cfg, "/api/tasks/"+url.PathEscape(taskID), &resp); err == nil && resp.Task != nil {
			switch resp.Task.Status {
			case proto.StatusCompleted, proto.StatusFailed, proto.StatusCancelled:
				return resp.Result, nil
			}
		}
		time.Sleep(1 * time.Second)
	}
}
