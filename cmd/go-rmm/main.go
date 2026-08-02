// go-rmm CLI —— 统一命令行客户端,对接 Relay HTTP API。
//
// 用法:
//
//	go-rmm [global flags] <command> [command flags]
//
// 全局 flag:
//
//	--relay   Relay 地址(默认 http://127.0.0.1:7780,可用 GO_RMM_RELAY 环境变量)
//	--token   鉴权 token(可用 GO_RMM_TOKEN 环境变量)
//
// 子命令:
//
//	agent list                      列出在线 Agent
//	task run    -a <agent> -t <type> [-c command | -s script | -f payload]
//	task list   [-n 20]            列出最近任务
//	task result <task-id>           查看任务结果
//	task cancel <task-id> [-a agent] 取消任务
//	file upload <local> <remote> -a <agent>   上传文件
//	file download <remote> <local> -a <agent> 下载文件
//	shell open  -a <agent>          打开交互式终端
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/QiuShichang/go-rmm/proto"
)

// version 由 -ldflags 注入。
var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		// B6:区分退出码——exitError 携带语义化 code,否则默认 1
		if ee, ok := err.(*exitError); ok {
			fmt.Fprintf(os.Stderr, "go-rmm: %v\n", err)
			os.Exit(ee.code)
		}
		fmt.Fprintf(os.Stderr, "go-rmm: %v\n", err)
		os.Exit(exitGeneric)
	}
}

// === 配置 ===

type config struct {
	relay    string // Relay HTTP 地址
	token    string // Authorization Bearer token
	output   string // 输出格式: text(默认) / json(机器可读)
	insecure bool   // 跳过 TLS 证书校验(自签证书场景)
	cacert   string // 自定义 CA 证书路径
}

// === 命令路由 ===

func run(args []string) error {
	if len(args) == 0 {
		printUsage()
		return nil
	}

	// 解析全局 flag
	cfg := config{
		relay: getEnv("GO_RMM_RELAY", "http://127.0.0.1:7780"),
		token: getEnv("GO_RMM_TOKEN", ""),
	}
	global := flag.NewFlagSet("go-rmm", flag.ContinueOnError)
	global.StringVar(&cfg.relay, "relay", cfg.relay, "Relay 地址")
	global.StringVar(&cfg.token, "token", cfg.token, "鉴权 token")
	global.StringVar(&cfg.output, "output", getEnv("GO_RMM_OUTPUT", "text"), "输出格式: text/json")
	global.StringVar(&cfg.output, "o", getEnv("GO_RMM_OUTPUT", "text"), "输出格式简写: text/json")
	global.BoolVar(&cfg.insecure, "insecure", false, "跳过 TLS 证书校验(自签证书)")
	global.StringVar(&cfg.cacert, "cacert", "", "自定义 CA 证书路径(自签 mTLS)")
	global.SetOutput(os.Stderr)
	global.Usage = func() { printUsage() }
	if err := global.Parse(args); err != nil {
		return err // flag 解析失败现在友好打印(Usage 已触发)
	}
	// P1-S9:--insecure 或 --cacert 配置 TLS 客户端
	applyTLSConfig(cfg)
	// P2:设置 JSON 输出模式
	jsonMode = cfg.output == "json"

	posArgs := global.Args()
	if len(posArgs) == 0 {
		printUsage()
		return nil
	}

	cmd := posArgs[0]
	switch cmd {
	case "agent":
		return cmdAgent(cfg, posArgs[1:])
	case "task":
		return cmdTask(cfg, posArgs[1:])
	case "file":
		return cmdFile(cfg, posArgs[1:])
	case "shell":
		return cmdShell(cfg, posArgs[1:])
	case "cron":
		return cmdCron(cfg, posArgs[1:])
	case "deploy":
		return cmdDeploy(cfg, posArgs[1:])
	case "tunnel":
		return cmdTunnel(cfg, posArgs[1:])
	case "update":
		return cmdUpdate(cfg, posArgs[1:])
	case "sysinfo":
		return cmdSysinfo(cfg, posArgs[1:])
	case "service":
		return cmdService(cfg, posArgs[1:])
	case "proc":
		return cmdProc(cfg, posArgs[1:])
	case "power":
		return cmdPower(cfg, posArgs[1:])
	case "monitor":
		return cmdMonitor(cfg, posArgs[1:])
	case "net":
		return cmdNet(cfg, posArgs[1:])
	case "logs":
		return cmdLogs(cfg, posArgs[1:])
	case "sync":
		return cmdSync(cfg, posArgs[1:])
	case "watch":
		return cmdWatch(cfg, posArgs[1:])
	case "exec":
		return cmdExec(cfg, posArgs[1:])
	case "tag":
		return cmdTag(cfg, posArgs[1:])
	case "events":
		return cmdEvents(cfg, posArgs[1:])
	case "topology", "topo":
		return cmdTopology(cfg, posArgs[1:])
	case "diagnose", "diag":
		return cmdDiagnose(cfg, posArgs[1:])
	case "compare":
		return cmdCompare(cfg, posArgs[1:])
	case "group":
		return cmdGroup(cfg, posArgs[1:])
	case "inventory":
		return cmdInventory(cfg, posArgs[1:])
	case "history":
		return cmdHistory(cfg, posArgs[1:])
	case "health-check", "healthcheck":
		return cmdHealthCheck(cfg, posArgs[1:])
	case "workflow":
		return cmdWorkflow(cfg, posArgs[1:])
	case "alert":
		return cmdAlert(cfg, posArgs[1:])
	case "find":
		return cmdFind(cfg, posArgs[1:])
	case "config":
		return cmdConfigMgmt(cfg, posArgs[1:])
	case "snapshot":
		return cmdSnapshot(cfg, posArgs[1:])
	case "completion":
		return cmdCompletion(cfg, posArgs[1:])
	case "status":
		return cmdStatus(cfg, posArgs[1:])
	case "bootstrap":
		return cmdBootstrap(cfg, posArgs[1:])
	case "rescue":
		return cmdRescue(cfg, posArgs[1:])
	case "version":
		fmt.Println("go-rmm", version)
		return nil
	case "help", "-h", "--help":
		printUsage()
		return nil
	default:
		printUsage()
		return fmt.Errorf("未知命令: %s", cmd)
	}
}

// === agent 子命令 ===

func cmdAgent(cfg config, args []string) error {
	if len(args) == 0 {
		return errors.New("用法: go-rmm agent <list|revoke|enroll-token> [args]")
	}
	switch args[0] {
	case "list", "ls":
		return agentList(cfg, args[1:])
	case "revoke":
		return agentRevoke(cfg, args[1:])
	case "enroll-token", "enroll":
		return agentEnroll(cfg, args[1:])
	default:
		return fmt.Errorf("未知 agent 子命令: %s (可用: list revoke enroll-token)", args[0])
	}
}

func agentList(cfg config, args []string) error {
	// B4:支持位置参数作 tag 过滤(客户端过滤,因 Relay 无对应端点)
	tagFilter := ""
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			tagFilter = a
			break
		}
	}

	type agentInfo struct {
		AgentID      string            `json:"agent_id"`
		Hostname     string            `json:"hostname"`
		OS           string            `json:"os"`
		Version      string            `json:"version"`
		Capabilities []string          `json:"capabilities"`
		Tags         map[string]string `json:"tags"`
		OnlineSince  int64             `json:"online_since"`
	}
	var resp struct {
		Agents []agentInfo `json:"agents"`
		Count  int         `json:"count"`
	}
	if err := apiGet(cfg, "/api/agents", &resp); err != nil {
		return err
	}

	// 客户端 tag 过滤
	filtered := resp.Agents
	if tagFilter != "" {
		filtered = nil
		for _, a := range resp.Agents {
			if matchTag(a.Tags, tagFilter) {
				filtered = append(filtered, a)
			}
		}
	}

	// P2:JSON 输出模式(脚本可解析)
	if jsonMode {
		outputJSON(map[string]any{"agents": filtered, "count": len(filtered)})
		return nil
	}

	if len(filtered) == 0 {
		if tagFilter != "" {
			return errNotFound("无匹配 tag=" + tagFilter + " 的在线 Agent")
		}
		fmt.Println("暂无在线 Agent")
		return nil
	}
	fmt.Printf("%-24s %-20s %-12s %-12s %s\n", "AGENT ID", "HOSTNAME", "OS", "VERSION", "CAPS")
	fmt.Println(strings.Repeat("-", 100))
	for _, a := range filtered {
		onlineSince := "—"
		if a.OnlineSince > 0 {
			onlineSince = time.UnixMilli(a.OnlineSince).Format("15:04:05")
		}
		tagsStr := ""
		if len(a.Tags) > 0 {
			var parts []string
			for k, v := range a.Tags {
				parts = append(parts, k+"="+v)
			}
			tagsStr = " [" + strings.Join(parts, ",") + "]"
		}
		fmt.Printf("%-24s %-20s %-12s %-12s %s%s (在线自 %s)\n",
			a.AgentID, a.Hostname, a.OS, a.Version,
			strings.Join(a.Capabilities, ","), tagsStr, onlineSince)
	}
	fmt.Printf("\n共 %d 个在线\n", len(filtered))
	return nil
}

// matchTag 检查 agent tags 是否匹配过滤器。
// filter 格式:"key=value" 精确匹配,或 "value" 匹配任意 key 的 value。
func matchTag(tags map[string]string, filter string) bool {
	if idx := strings.Index(filter, "="); idx > 0 {
		k := filter[:idx]
		v := filter[idx+1:]
		return tags[k] == v
	}
	for _, v := range tags {
		if v == filter {
			return true
		}
	}
	return false
}

// === task 子命令 ===

func cmdTask(cfg config, args []string) error {
	if len(args) == 0 {
		return errors.New("用法: go-rmm task <run|list|result|cancel|batch> [flags]")
	}
	switch args[0] {
	case "run":
		return taskRun(cfg, args[1:])
	case "stream", "live", "follow":
		return taskStream(cfg, args[1:])
	case "list", "ls":
		return taskList(cfg, args[1:])
	case "result", "get":
		return taskResult(cfg, args[1:])
	case "cancel":
		return taskCancel(cfg, args[1:])
	case "batch":
		return taskBatch(cfg, args[1:])
	default:
		return fmt.Errorf("未知 task 子命令: %s (可用: run list result cancel batch)", args[0])
	}
}

func taskRun(cfg config, args []string) error {
	fs := flag.NewFlagSet("task run", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "用法: go-rmm task run -a <agent> [-t type] [-c command | -s script | -f payload] [--idem key] [--no-wait]")
		fs.PrintDefaults()
	}
	agentID := fs.String("a", "", "目标 Agent ID(必填)")
	taskType := fs.String("t", "powershell", "任务类型: powershell/script/file/screenshot/browse/guiclick")
	payloadFile := fs.String("f", "", "从文件读取 JSON payload(与 -c/-s 互斥)")
	command := fs.String("c", "", "PowerShell 命令(快捷方式)")
	script := fs.String("s", "", "脚本内容或路径(快捷方式)")
	timeout := fs.Int("timeout", 60, "任务超时秒数")
	idemKey := fs.String("idem", "", "幂等键(防重复执行,默认按内容自动生成)")
	noWait := fs.Bool("no-wait", false, "不等待结果,只返回 task_id")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *agentID == "" {
		return errors.New("-a <agent_id> 必填")
	}
	if !proto.IsValidTaskType(*taskType) {
		return fmt.Errorf("未知任务类型: %s (可用: powershell script file screenshot browse guiclick)", *taskType)
	}

	payload, err := buildPayload(*taskType, *command, *script, *payloadFile)
	if err != nil {
		return err
	}

	// P2-#10:幂等键默认按 (agent,type,payload-hash) 生成,保证重试不重复执行
	if *idemKey == "" {
		h := sha256.Sum256(append([]byte(*agentID+"|"+*taskType+"|"), payload...))
		*idemKey = fmt.Sprintf("cli-%x", h[:8])
	}

	taskID := "cli-" + *taskType + "-" + fmt.Sprint(time.Now().UnixMilli())
	body := proto.TaskRequest{
		TaskID:         taskID,
		Type:           *taskType,
		AgentID:        *agentID,
		Payload:        payload,
		Timeout:        *timeout,
		IdempotencyKey: *idemKey,
	}

	var resp struct {
		TaskID  string `json:"task_id"`
		Status  string `json:"status"`
		AgentID string `json:"agent_id"`
		Message string `json:"message,omitempty"`
	}
	// C3:任务提交带 idemKey,网络抖动可安全重试
	if err := apiPostRetry(cfg, "/api/tasks", body, &resp, 2); err != nil {
		return err
	}

	fmt.Printf("task_id:  %s\n", resp.TaskID)
	fmt.Printf("agent_id: %s\n", resp.AgentID)
	fmt.Printf("status:   %s\n", resp.Status)
	fmt.Printf("idem_key: %s\n", *idemKey)
	if resp.Message != "" {
		fmt.Printf("message:  %s\n", resp.Message)
	}

	if *noWait {
		return nil
	}
	if resp.Status == proto.StatusPending && resp.Message != "" {
		fmt.Println("\nAgent 离线,任务已缓存。上线后自动投递。")
		return nil
	}
	fmt.Printf("\n等待结果...")
	// P2-#11:waitResult 现在监听 SIGINT,可 Ctrl+C 中断
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	done, err := waitResult(ctx, cfg, taskID, *timeout)
	if err != nil {
		return err
	}
	if !done {
		// B7:超时/中断时任务可能仍在 Agent 上运行,用 exitTimeout(3) 与任务失败区分
		return errWaitTimeout("等待被中断或超时,任务可能仍在运行,可用 `go-rmm task result " + taskID + "` 稍后查询")
	}
	// B6:任务执行失败用 exitTaskFail(2) 与 CLI 自身错误区分
	if taskFailed {
		return errTaskFailed("任务执行失败")
	}
	return nil
}

// taskFailed 由 waitResult 设置,供 taskRun 判断是否返回任务失败退出码。
var taskFailed bool

func buildPayload(taskType, command, script, payloadFile string) (json.RawMessage, error) {
	// -f 文件优先
	if payloadFile != "" {
		data, err := os.ReadFile(payloadFile)
		if err != nil {
			return nil, fmt.Errorf("读取 payload 文件失败: %w", err)
		}
		if !json.Valid(data) {
			return nil, errors.New("payload 文件不是合法 JSON")
		}
		return data, nil
	}

	switch taskType {
	case "powershell":
		var cmd string
		if command != "" {
			cmd = command
		}
		if cmd == "" {
			return nil, errors.New("powershell 任务需要 -c <command>")
		}
		return json.Marshal(proto.PowerShellTask{Command: cmd})

	case "script":
		if script == "" {
			return nil, errors.New("script 任务需要 -s <脚本内容或文件路径>")
		}
		content := script
		if fi, err := os.Stat(script); err == nil && !fi.IsDir() {
			data, err := os.ReadFile(script)
			if err != nil {
				return nil, fmt.Errorf("读取脚本文件失败: %w", err)
			}
			content = string(data)
		}
		lang := "ps1"
		if strings.HasSuffix(script, ".bat") || strings.HasSuffix(script, ".cmd") {
			lang = "bat"
		}
		return json.Marshal(proto.ScriptTask{Lang: lang, Content: content})

	default:
		return nil, fmt.Errorf("任务类型 %s 需要 -f <payload.json> 提供完整 JSON", taskType)
	}
}

// waitResult 轮询任务状态直到完成/失败/取消/超时/中断。
// 返回 (true, nil) 表示拿到终态; (false, nil) 表示被中断或超时(已在 stderr 提示)。
func waitResult(ctx context.Context, cfg config, taskID string, timeout int) (bool, error) {
	deadline := time.Now().Add(time.Duration(timeout+10) * time.Second)
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			fmt.Println("\n已中断")
			return false, nil
		case <-ticker.C:
			if time.Now().After(deadline) {
				fmt.Println("\n等待超时")
				return false, nil
			}
			var resp struct {
				Task *struct {
					Status string `json:"status"`
				} `json:"task"`
				Result *proto.TaskResult `json:"result"`
			}
			if err := apiGet(cfg, "/api/tasks/"+url.PathEscape(taskID), &resp); err != nil {
				fmt.Print(".")
				continue
			}
			if resp.Task == nil {
				fmt.Print(".")
				continue
			}
			switch resp.Task.Status {
			case proto.StatusCompleted:
				fmt.Println()
				printResult(resp.Result)
				return true, nil
			case proto.StatusFailed:
				fmt.Println()
				printResult(resp.Result)
				taskFailed = true // B6:标记任务失败,taskRun 据此返回 exitTaskFail
				return true, nil
			case proto.StatusCancelled:
				fmt.Println("\n任务已取消")
				return true, nil
			default:
				fmt.Print(".")
			}
		}
	}
}

func printResult(r *proto.TaskResult) {
	if r == nil {
		fmt.Println("结果为空")
		return
	}
	// P2: 过滤 PowerShell CLIXML 噪音(stderr 中的 #< CLIXML ... <Objs> 块)
	stdout := filterCLIXML(r.Stdout)
	stderr := filterCLIXML(r.Stderr)
	fmt.Printf("\n=== 任务结果 ===\n")
	fmt.Printf("成功:      %v\n", r.Success)
	fmt.Printf("退出码:    %d\n", r.ExitCode)
	fmt.Printf("耗时:      %d ms\n", r.Duration)
	if stdout != "" {
		fmt.Printf("--- stdout ---\n%s\n", stdout)
	}
	if stderr != "" {
		fmt.Printf("--- stderr ---\n%s\n", stderr)
	}
	if r.Error != "" {
		fmt.Printf("错误:      %s\n", r.Error)
	}
	if len(r.Data) > 0 && string(r.Data) != "null" {
		fmt.Printf("数据:      %s\n", string(r.Data))
	}
}

// P2-#9:task list 直接 decode 到结构化类型,去除 marshal/unmarshal 往返
// B1:支持 --status / --agent / --type 客户端过滤(Relay 无服务端过滤)
func taskList(cfg config, args []string) error {
	fs := flag.NewFlagSet("task list", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	limit := fs.Int("n", 20, "返回条数")
	statusFilter := fs.String("status", "", "按状态过滤: pending/dispatched/acknowledged/completed/failed/cancelled")
	agentFilter := fs.String("agent", "", "按 Agent ID 过滤")
	typeFilter := fs.String("type", "", "按任务类型过滤")
	if err := fs.Parse(args); err != nil {
		return err
	}

	var resp struct {
		Tasks []taskListItem `json:"tasks"`
		Count int            `json:"count"`
		Enabled bool         `json:"enabled"`
	}
	// 扩大查询以补偿客户端过滤的样本损失
	queryLimit := *limit
	if *statusFilter != "" || *agentFilter != "" || *typeFilter != "" {
		queryLimit *= 5 // 多取 5 倍样本再过滤
	}
	if err := apiGet(cfg, "/api/tasks?limit="+strconv.Itoa(queryLimit), &resp); err != nil {
		return err
	}
	if !resp.Enabled {
		fmt.Println("任务持久化未启用(Relay 未配置 store)")
		return nil
	}

	// 客户端过滤
	filtered := resp.Tasks[:0:0]
	for _, t := range resp.Tasks {
		if *statusFilter != "" && t.Status != *statusFilter {
			continue
		}
		if *agentFilter != "" && t.Request.AgentID != *agentFilter {
			continue
		}
		if *typeFilter != "" && t.Request.Type != *typeFilter {
			continue
		}
		filtered = append(filtered, t)
	}
	// 截断到用户请求的 limit
	if len(filtered) > *limit {
		filtered = filtered[:*limit]
	}

	filterDesc := ""
	if *statusFilter != "" || *agentFilter != "" || *typeFilter != "" {
		var parts []string
		if *statusFilter != "" {
			parts = append(parts, "status="+*statusFilter)
		}
		if *agentFilter != "" {
			parts = append(parts, "agent="+*agentFilter)
		}
		if *typeFilter != "" {
			parts = append(parts, "type="+*typeFilter)
		}
		filterDesc = " (过滤: " + strings.Join(parts, ",") + ")"
	}
	fmt.Printf("最近 %d 条任务(共 %d)%s:\n", len(filtered), resp.Count, filterDesc)
	fmt.Printf("%-40s %-14s %-24s %-12s %s\n", "TASK ID", "TYPE", "AGENT", "STATUS", "CREATED")
	fmt.Println(strings.Repeat("-", 110))
	for _, t := range filtered {
		ts := time.UnixMilli(t.CreatedAt).Format("01-02 15:04:05")
		fmt.Printf("%-40s %-14s %-24s %-12s %s\n",
			t.Request.TaskID, t.Request.Type, t.Request.AgentID, t.Status, ts)
	}
	return nil
}

// taskListItem 是 task list 的响应项(也用于客户端过滤)。
type taskListItem struct {
	Request struct {
		TaskID  string `json:"task_id"`
		Type    string `json:"type"`
		AgentID string `json:"agent_id"`
	} `json:"request"`
	Status    string `json:"status"`
	CreatedAt int64  `json:"created_at"`
}

func taskResult(cfg config, args []string) error {
	if len(args) == 0 {
		return errors.New("用法: go-rmm task result <task-id> [--watch]")
	}
	taskID := args[0]
	fs := flag.NewFlagSet("task result", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	watch := fs.Bool("watch", false, "持续轮询直到任务终态(长任务跟踪)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	if *watch {
		// B2:轮询直到终态
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		fmt.Printf("跟踪 task=%s (Ctrl+C 中止)...\n", taskID)
		done, _ := waitResult(ctx, cfg, taskID, 3600) // 默认跟 1 小时
		if !done {
			fmt.Println("\n跟踪被中断")
		}
		return nil
	}

	// 单次查询
	var resp struct {
		Task *struct {
			Status string `json:"status"`
		} `json:"task"`
		Result *proto.TaskResult `json:"result"`
	}
	if err := apiGet(cfg, "/api/tasks/"+url.PathEscape(taskID), &resp); err != nil {
		return err
	}
	if resp.Task == nil {
		return errNotFound("任务不存在: " + taskID)
	}
	// P2:JSON 输出
	if jsonMode {
		outputJSON(map[string]any{"task": resp.Task, "result": resp.Result})
		return nil
	}
	fmt.Printf("状态: %s\n", resp.Task.Status)
	printResult(resp.Result)
	return nil
}

func taskCancel(cfg config, args []string) error {
	if len(args) == 0 {
		return errors.New("用法: go-rmm task cancel <task-id> [-a agent_id]")
	}
	taskID := args[0]
	fs := flag.NewFlagSet("task cancel", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	agentID := fs.String("a", "", "Agent ID(可选,从 store 推断)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	body := map[string]string{
		"task_id":  taskID,
		"agent_id": *agentID,
		"reason":   "cli cancelled",
	}
	var resp struct {
		TaskID string `json:"task_id"`
		Status string `json:"status"`
	}
	if err := apiPost(cfg, "/api/tasks/cancel", body, &resp); err != nil {
		return err
	}
	fmt.Printf("已取消: %s (status=%s)\n", resp.TaskID, resp.Status)
	return nil
}

// === file 子命令 ===

func cmdFile(cfg config, args []string) error {
	if len(args) == 0 {
		return errors.New("用法: go-rmm file <upload|download> [args]")
	}
	switch args[0] {
	case "upload", "up":
		return fileUpload(cfg, args[1:])
	case "download", "down", "dl":
		return fileDownload(cfg, args[1:])
	default:
		return fmt.Errorf("未知 file 子命令: %s (可用: upload download)", args[0])
	}
}

// P0-#1:上传用 TeeReader 边读边算 hash,只读一次文件
// P0-#3:URL 参数用 QueryEscape 编码
// A7:支持 - 作为 localPath,从 stdin 读(管道: cat f | go-rmm file upload -a x - remote)
//
// FT 分片上传:文件 > chunkThreshold(50MB)时自动走分片模式:
//   - 切成 chunkSize(10MB)的独立分片
//   - 每片用现有 upload 端点传到 <name>.ftpart<NN>
//   - 已传的分片跳过(断点续传,查目标机文件是否存在)
//   - 全部传完后发 powershell 合并 + 校验 SHA256
// 解决单请求传 1GB 经 nginx 504 超时的问题。
func fileUpload(cfg config, args []string) error {
	// O4:先剥离分片参数 flag(--concurrency/--chunk-size/--chunk-retry 及其值),
	// 避免 extractTwoPos 把 flag 的值误当位置参数。
	args = stripValueFlags(args, "--concurrency", "--chunk-size", "--chunk-retry")

	agentID := extractArg(args, "-a")
	localPath, remotePath, rest := extractTwoPos(args, "-a", agentID)

	// O4:分片参数 flag(可选,在 -a 前后均可)
	if v := flagVal(args, "--concurrency"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			concurrencyArg = n
		}
	}
	if v := flagVal(args, "--chunk-size"); v != "" {
		if n, err := parseSize(v); err == nil && n > 0 {
			// R3:上限 128MB,防 OOM(流式上传后内存占用小,但仍需限制单片大小)
			if n > 128*1024*1024 {
				return fmt.Errorf("--chunk-size 超过上限 128M: %s", v)
			}
			chunkSizeArg = n
		}
	}
	if v := flagVal(args, "--chunk-retry"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			chunkMaxRetryArg = n
		}
	}

	if len(rest) > 0 {
		_ = rest
	}
	if localPath == "" || remotePath == "" {
		return errors.New("用法: go-rmm file upload -a <agent> [--concurrency N] [--chunk-size 10M] [--chunk-retry 3] <local-path|-> <remote-path>")
	}

	// 管道模式不支持分片(stdin 无大小)
	if localPath != "-" {
		if fi, err := os.Stat(localPath); err == nil && fi.Size() > chunkThreshold {
			return fileUploadChunked(cfg, agentID, localPath, remotePath, fi.Size())
		}
	}
	if agentID == "" {
		return errors.New("-a <agent_id> 必填")
	}

	// 管道模式:localPath == "-" 从 stdin 读
	var (
		f        io.ReadCloser
		totalSize int64
		err      error
	)
	if localPath == "-" {
		f = io.NopCloser(os.Stdin)
		totalSize = -1 // 未知大小,进度条降级
		fmt.Println("(从 stdin 读取)")
	} else {
		fi, err := os.Stat(localPath)
		if err != nil {
			return fmt.Errorf("本地文件不存在: %w", err)
		}
		totalSize = fi.Size()
		f, err = os.Open(localPath)
		if err != nil {
			return err
		}
	}
	defer f.Close()

	// 边读边算 hash + 进度:TeeReader 同时喂给 hasher 和 progressReader
	hasher := sha256.New()
	var tee io.Reader
	if totalSize > 0 {
		pr := &progressReader{r: f, total: totalSize, label: "上传"}
		tee = io.TeeReader(pr, hasher)
	} else {
		// 管道模式无 totalSize,progressReader 会降级显示已读字节
		pr := &progressReader{r: f, total: -1, label: "上传"}
		tee = io.TeeReader(pr, hasher)
	}

	transferID := "xfer-" + fmt.Sprint(time.Now().UnixNano())
	q := url.Values{}
	q.Set("agent_id", agentID)
	q.Set("path", remotePath)
	q.Set("transfer_id", transferID)
	// FT 修复:大文件经 nginx 反代时 Content-Length 会卡死(nginx 对超大 CL 处理异常)。
	// 改用 chunked encoding(req.ContentLength=-1)+ 通过 query 显式传 size,
	// Relay 端优先读 query.size 作为文件大小。
	if totalSize > 0 {
		q.Set("size", fmt.Sprintf("%d", totalSize))
	}
	u := cfg.relay + "/api/transfer/upload?" + q.Encode()

	fmt.Printf("上传 %s → %s:%s\n", localPath, agentID, remotePath)

	req, err := http.NewRequest("POST", u, tee)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.token)
	// FT 修复:强制 chunked(不设 ContentLength),避免大 CL 经 nginx 卡死。
	// Go http client 对 NewRequest 带 reader 的,ContentLength=0 时自动用 chunked。
	req.ContentLength = -1

	resp, err := streamingClient.Do(req) // C1:共享连接池
	if err != nil {
		return fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()

	// P1-#8:严格检查 JSON 解码
	var result struct {
		TransferID string `json:"transfer_id"`
		Bytes      int64  `json:"bytes"`
		SHA256     string `json:"sha256"`
		Status     string `json:"status"`
		Error      string `json:"error"`
	}
	dec := json.NewDecoder(resp.Body)
	if err := dec.Decode(&result); err != nil {
		return fmt.Errorf("解析响应失败 (HTTP %d): %w", resp.StatusCode, err)
	}

	if resp.StatusCode >= 400 {
		msg := result.Error
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		if resp.StatusCode == http.StatusForbidden {
			return errBlocked("上传被拒绝: " + msg) // B6:策略拦截专用退出码
		}
		return fmt.Errorf("上传失败: %s", msg)
	}

	// P0-#1:本地 hasher 此时已完成,与 Agent 回传的 sha256 比对
	localSHA := fmt.Sprintf("%x", hasher.Sum(nil))
	fmt.Println()
	fmt.Printf("✅ 上传完成: %s\n", formatSize(result.Bytes))
	fmt.Printf("   Agent SHA256: %s\n", result.SHA256)
	if result.SHA256 != "" && localSHA != result.SHA256 {
		fmt.Fprintf(os.Stderr, "   ⚠️  校验不一致!本地=%s\n", localSHA)
		return errors.New("SHA256 校验失败,文件可能损坏")
	}
	if result.SHA256 == localSHA {
		fmt.Printf("   ✅ 本地 SHA256 一致\n")
	}
	return nil
}

// P0-#2:下载 ContentLength=-1 时降级显示已写字节数 + 速率
// P0-#3:URL 参数用 QueryEscape 编码
// A7:支持 - 作为 localPath,写 stdout(管道: go-rmm file download -a x remote - | gzip > f.gz)
// O3:大文件(>50MB)自动走分片下载(并发 + 断点续传),经 nginx 不再依赖单连接超时
func fileDownload(cfg config, args []string) error {
	// O4:剥离分片参数 flag,避免 extractTwoPos 把值误当位置参数
	args = stripValueFlags(args, "--concurrency", "--chunk-size", "--chunk-retry")
	if v := flagVal(args, "--concurrency"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			concurrencyArg = n
		}
	}
	if v := flagVal(args, "--chunk-size"); v != "" {
		if n, err := parseSize(v); err == nil && n > 0 {
			// R3:上限 128MB,防 OOM(流式上传后内存占用小,但仍需限制单片大小)
			if n > 128*1024*1024 {
				return fmt.Errorf("--chunk-size 超过上限 128M: %s", v)
			}
			chunkSizeArg = n
		}
	}
	if v := flagVal(args, "--chunk-retry"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			chunkMaxRetryArg = n
		}
	}

	agentID := extractArg(args, "-a")
	remotePath, localPath, _ := extractTwoPos(args, "-a", agentID)
	if remotePath == "" || localPath == "" {
		return errors.New("用法: go-rmm file download -a <agent> [--concurrency N] [--chunk-size 10M] <remote-path> <local-path|->")
	}
	if agentID == "" {
		return errors.New("-a <agent_id> 必填")
	}

	// O3:管道模式(stdout)不分片(无法切片写 stdout);非管道大文件走分片
	if localPath != "-" {
		// 先查远端文件大小(powershell 任务)
		if sz, err := remoteFileSize(cfg, agentID, remotePath); err == nil && sz > chunkThreshold {
			return fileDownloadChunked(cfg, agentID, remotePath, localPath, sz)
		}
	}

	transferID := "xfer-" + fmt.Sprint(time.Now().UnixNano())
	q := url.Values{}
	q.Set("agent_id", agentID)
	q.Set("path", remotePath)
	q.Set("transfer_id", transferID)
	u := cfg.relay + "/api/transfer/download?" + q.Encode()

	fmt.Printf("下载 %s:%s → %s\n", agentID, remotePath, localPath)

	req, err := http.NewRequest("POST", u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.token)

	resp, err := streamingClient.Do(req) // C1:共享连接池
	if err != nil {
		return fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		var errBody struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&errBody)
		errMsg := fmt.Sprintf("HTTP %d", resp.StatusCode)
		if errBody.Error != "" {
			errMsg = errBody.Error
		}
		if resp.StatusCode == http.StatusForbidden {
			return errBlocked("下载被拒绝: " + errMsg)
		}
		if resp.StatusCode == http.StatusNotFound {
			return errNotFound("下载失败: " + errMsg)
		}
		return fmt.Errorf("下载失败: %s", errMsg)
	}

	// A7:管道模式或普通模式选 writer
	var out io.Writer
	pipeMode := localPath == "-"
	if pipeMode {
		out = os.Stdout
		fmt.Fprintln(os.Stderr, "(写入 stdout)")
	} else {
		if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
			return fmt.Errorf("创建目录失败: %w", err)
		}
		f, err := os.Create(localPath)
		if err != nil {
			return fmt.Errorf("创建文件失败: %w", err)
		}
		defer f.Close()
		out = f
	}

	totalSize := resp.ContentLength // 可能 -1(流式)
	// P0-P1 修复:管道模式禁止 progressWriter(会污染 stdout 数据流)
	if pipeMode {
		written, err := io.Copy(out, resp.Body)
		if err != nil {
			return fmt.Errorf("写入 stdout 失败: %w", err)
		}
		fmt.Fprintf(os.Stderr, "✅ 下载完成: %s\n", formatSize(written))
		return nil
	}
	pw := &progressWriter{w: out, total: totalSize, label: "下载"}
	written, err := io.Copy(pw, resp.Body)
	if err != nil {
		return fmt.Errorf("写入文件失败: %w", err)
	}
	pw.finish()
	fmt.Println()
	fmt.Fprintf(os.Stderr, "✅ 下载完成: %s\n", formatSize(written))
	return nil
}

// === shell 子命令 ===

func cmdShell(cfg config, args []string) error {
	if len(args) == 0 {
		return errors.New("用法: go-rmm shell open -a <agent> [--cols 80] [--rows 24] [--shell powershell]")
	}
	switch args[0] {
	case "open":
		return shellOpen(cfg, args[1:])
	default:
		return fmt.Errorf("未知 shell 子命令: %s (可用: open)", args[0])
	}
}

// shellOpen 打开交互式终端。
//
// 实现方式:通过 /api/shell/open 开会话 → 读本地 stdin 发 /api/shell/input →
// 订阅 SSE /api/events 接收 shell_output → Ctrl+C 或 exit 后调 /api/shell/close。
//
// 局限:Relay 当前 shell_output 经 EventBus 广播给所有订阅者,CLI 需按 sessionID 过滤。
// 这里采用轻量 SSE 订阅 + 按 session 过滤 + raw 模式读取本地输入。
func shellOpen(cfg config, args []string) error {
	fs := flag.NewFlagSet("shell open", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	agentID := fs.String("a", "", "目标 Agent ID(必填)")
	cols := fs.Int("cols", 80, "终端列数")
	rows := fs.Int("rows", 24, "终端行数")
	shellName := fs.String("shell", "powershell", "shell 类型: powershell/cmd/bash")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *agentID == "" {
		return errors.New("-a <agent_id> 必填")
	}

	// 1. 开会话
	var openResp struct {
		SessionID string `json:"session_id"`
		AgentID   string `json:"agent_id"`
	}
	body := map[string]any{
		"agent_id": *agentID,
		"cols":     *cols,
		"rows":     *rows,
		"shell":    *shellName,
	}
	if err := apiPost(cfg, "/api/shell/open", body, &openResp); err != nil {
		return err
	}
	sessionID := openResp.SessionID
	fmt.Printf("已开启 shell 会话: %s (agent=%s shell=%s)\n", sessionID, *agentID, *shellName)
	fmt.Printf("输入命令回车发送,输入 :q 或 Ctrl+C 退出\n\n")

	// 2. 启动 SSE 订阅 goroutine 接收输出
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go sseSubscribe(ctx, cfg, sessionID, *agentID)

	// 3. 读本地 stdin → 发 input
	defer func() {
		// 关闭会话
		closeBody := map[string]string{"session_id": sessionID, "agent_id": *agentID}
		_ = apiPost(cfg, "/api/shell/close", closeBody, nil)
		fmt.Println("\n已关闭 shell 会话")
	}()

	// 监听 Ctrl+C
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	// 主循环:读 stdin
	buf := make([]byte, 4096)
	for {
		// 简化:按行读取
		line, err := readLine()
		if err != nil {
			return nil
		}
		if line == ":q" || line == "exit" {
			return nil
		}
		select {
		case <-sigCh:
			return nil
		default:
		}
		// 发送命令 + 换行
		input := line + "\n"
		dataB64 := base64Encode([]byte(input))
		inputBody := map[string]string{
			"session_id": sessionID,
			"agent_id":   *agentID,
			"data_b64":   dataB64,
		}
		if err := apiPost(cfg, "/api/shell/input", inputBody, nil); err != nil {
			fmt.Fprintf(os.Stderr, "发送失败: %v\n", err)
		}
		_ = buf
	}
}

// sseSubscribe 订阅 SSE 事件流,过滤出当前 sessionID 的 shell_output。
func sseSubscribe(ctx context.Context, cfg config, sessionID, agentID string) {
	u := cfg.relay + "/api/events"
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return
	}
	if cfg.token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.token)
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := streamingClient.Do(req) // C1:共享连接池
	if err != nil {
		return
	}
	defer resp.Body.Close()

	// 简单 SSE 解析:按行读,event: / data: 配对
	scanner := newSSEScanner(resp.Body)
	for {
		ev, err := scanner.Next()
		if err != nil {
			return
		}
		if ev.event != "shell_output" {
			continue
		}
		var outer struct {
			AgentID string          `json:"agent_id"`
			Data    json.RawMessage `json:"data"`
		}
		if json.Unmarshal(ev.data, &outer) != nil {
			continue
		}
		if outer.AgentID != agentID {
			continue
		}
		var out proto.ShellOutput
		if json.Unmarshal(outer.Data, &out) != nil {
			continue
		}
		if out.SessionID != sessionID {
			continue
		}
		if out.DataB64 != "" {
			if decoded, err := base64Decode(out.DataB64); err == nil {
				os.Stdout.Write(decoded)
				os.Stdout.Sync()
			}
		}
		if out.Exit {
			fmt.Printf("\n[进程退出 code=%d]\n", out.ExitCode)
			return
		}
	}
}

// apiPostRetry 对幂等请求做有限重试。
// C3:网络抖动不再直接失败,带退避重试 2 次。
// 仅用于已知幂等的请求(带 idempotency_key 的 task 提交、GET 请求)。
func apiPostRetry(cfg config, path string, body any, v any, maxRetry int) error {
	var lastErr error
	for attempt := 0; attempt <= maxRetry; attempt++ {
		err := apiPost(cfg, path, body, v)
		if err == nil {
			return nil
		}
		lastErr = err
		// 只对网络类错误重试(不是 4xx 业务错误)
		if !isRetryableErr(err) {
			return err
		}
		if attempt < maxRetry {
			backoff := time.Duration(1<<attempt) * 500 * time.Millisecond // 0.5s, 1s, 2s...
			fmt.Fprintf(os.Stderr, "请求失败(%v),%v 后重试 %d/%d...\n", err, backoff, attempt+1, maxRetry)
			time.Sleep(backoff)
		}
	}
	return lastErr
}

// isRetryableErr 判断错误是否值得重试(网络抖动、超时、连接重置)。
// P1 修复:优先用类型化判断(errors.As + net.Error),文本匹配作为兜底。
// 避免单纯依赖错误文案(Go 版本/库更新会失效)。
func isRetryableErr(err error) bool {
	// 1. 类型化判断:net.Error 的 Timeout()
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	// 2. url.Error 包装的网络错误
	var urlErr *url.Error
	if errors.As(err, &urlErr) && (urlErr.Timeout() || urlErr.Temporary()) {
		return true
	}
	// 3. 兜底:错误消息匹配(覆盖库版本差异)
	msg := err.Error()
	for _, s := range []string{
		"EOF", "connection reset", "broken pipe",
		"no such host", "connection refused",
		"network is unreachable", "TLS handshake",
		"i/o timeout", "context deadline exceeded",
	} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// apiGetRetry 对 GET 做有限重试(GET 天然幂等)。
func apiGetRetry(cfg config, path string, v any, maxRetry int) error {
	var lastErr error
	for attempt := 0; attempt <= maxRetry; attempt++ {
		err := apiGet(cfg, path, v)
		if err == nil {
			return nil
		}
		lastErr = err
		if !isRetryableErr(err) {
			return err
		}
		if attempt < maxRetry {
			backoff := time.Duration(1<<attempt) * 500 * time.Millisecond
			time.Sleep(backoff)
		}
	}
	return lastErr
}

// === HTTP 客户端辅助 ===

func apiGet(cfg config, path string, v any) error {
	u := cfg.relay + path
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return err
	}
	if cfg.token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.token)
	}
	resp, err := controlClient.Do(req)
	if err != nil {
		return fmt.Errorf("请求 %s 失败: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("%s: %s", path, readError(resp.Body))
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		return fmt.Errorf("%s: 解析响应失败: %w", path, err)
	}
	return nil
}

func apiPost(cfg config, path string, body any, v any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("序列化请求体失败: %w", err)
	}
	u := cfg.relay + path
	req, err := http.NewRequest("POST", u, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.token)
	}
	resp, err := controlClient.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s 失败: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("%s: %s", path, readError(resp.Body))
	}
	if v != nil {
		if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
			return fmt.Errorf("%s: 解析响应失败: %w", path, err)
		}
	}
	return nil
}

// readError 读取错误响应体并尝试提取 error 字段。
func readError(r io.Reader) string {
	var errBody struct {
		Error string `json:"error"`
	}
	raw, _ := io.ReadAll(r)
	if json.Unmarshal(raw, &errBody) == nil && errBody.Error != "" {
		return errBody.Error
	}
	if len(raw) > 0 {
		return strings.TrimSpace(string(raw))
	}
	return "未知错误"
}

// === 工具函数 ===

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func formatSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for nn := n / unit; nn >= unit; nn /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// extractArg 从 args 中提取指定 flag 的值(支持 -flag value 和 -flag=value)。
func extractArg(args []string, flagName string) string {
	for i, a := range args {
		if a == flagName && i+1 < len(args) {
			return args[i+1]
		}
		if strings.HasPrefix(a, flagName+"=") {
			return strings.TrimPrefix(a, flagName+"=")
		}
	}
	return ""
}

// extractTwoPos 从 args 中提取两个位置参数(跳过 flag 及其值)。
// A7 修复:单独的 "-" 视为位置参数(stdin/stdout 管道),不当作 flag 跳过。
func extractTwoPos(args []string, skipFlag string, skipVal string) (string, string, []string) {
	var pos []string
	skipNext := false
	for _, a := range args {
		if skipNext {
			skipNext = false
			continue
		}
		if a == skipFlag || (skipVal != "" && a == skipVal) {
			skipNext = true
			continue
		}
		if strings.HasPrefix(a, skipFlag+"=") || (skipVal != "" && strings.HasPrefix(a, skipVal+"=")) {
			continue
		}
		// "-" 单独是位置参数(管道); 其它 - 开头的是 flag
		if a != "-" && strings.HasPrefix(a, "-") {
			continue
		}
		pos = append(pos, a)
	}
	if len(pos) >= 2 {
		return pos[0], pos[1], pos[2:]
	}
	if len(pos) == 1 {
		return pos[0], "", nil
	}
	return "", "", nil
}

// stripValueFlags 从 args 中移除指定的"带值 flag"及其值(如 --concurrency 4 → 都移除)。
// 用于 fileUpload 在 extractTwoPos 之前清理,避免 flag 的值被误当位置参数。
// 同时支持 --flag value 和 --flag=value 两种写法。
func stripValueFlags(args []string, flags ...string) []string {
	flagSet := make(map[string]bool, len(flags))
	for _, f := range flags {
		flagSet[f] = true
	}
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		// --flag=value 形式
		if eq := strings.IndexByte(a, '='); eq > 0 && flagSet[a[:eq]] {
			continue
		}
		// --flag value 形式
		if flagSet[a] {
			i++ // 跳过值
			continue
		}
		out = append(out, a)
	}
	return out
}

// flagVal 取 flag 的值(支持 --flag value 和 --flag=value),不存在返回空。
func flagVal(args []string, name string) string {
	for i := 0; i < len(args); i++ {
		if args[i] == name && i+1 < len(args) {
			return args[i+1]
		}
		if strings.HasPrefix(args[i], name+"=") {
			return args[i][len(name)+1:]
		}
	}
	return ""
}

// parseSize 解析带单位的大小字符串("10M"/"1G"/"5242880"),返回字节数。
func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("空")
	}
	mult := int64(1)
	switch s[len(s)-1] {
	case 'k', 'K':
		mult, s = 1024, s[:len(s)-1]
	case 'm', 'M':
		mult, s = 1024*1024, s[:len(s)-1]
	case 'g', 'G':
		mult, s = 1024*1024*1024, s[:len(s)-1]
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, err
	}
	return n * mult, nil
}

// === 进度跟踪(上传/下载共用,P0-#2 修复 ContentLength=-1 降级)===

type progressReader struct {
	r        io.Reader
	total    int64
	label    string
	read     int64
	lastTime time.Time
	lastRate int64
}

func (pr *progressReader) Read(p []byte) (int, error) {
	n, err := pr.r.Read(p)
	pr.read += int64(n)
	pr.maybePrint()
	return n, err
}

func (pr *progressReader) maybePrint() {
	now := time.Now()
	if now.Sub(pr.lastTime) < 200*time.Millisecond && pr.total > 0 && pr.read < pr.total {
		return // 百分比模式 200ms 限流
	}
	pr.lastTime = now
	if pr.total > 0 {
		pct := pr.read * 100 / pr.total
		fmt.Printf("\r%s: %d%% (%s / %s)   ", pr.label, pct, formatSize(pr.read), formatSize(pr.total))
	} else {
		// 降级:仅显示已读字节 + 估算速率
		fmt.Printf("\r%s: %s   ", pr.label, formatSize(pr.read))
	}
}

type progressWriter struct {
	w        io.Writer
	total    int64
	label    string
	written  int64
	start    time.Time
	lastTime time.Time
}

func (pw *progressWriter) Write(p []byte) (int, error) {
	n, err := pw.w.Write(p)
	pw.written += int64(n)
	if pw.start.IsZero() {
		pw.start = time.Now()
	}
	pw.maybePrint()
	return n, err
}

func (pw *progressWriter) maybePrint() {
	now := time.Now()
	if now.Sub(pw.lastTime) < 200*time.Millisecond && pw.total > 0 && pw.written < pw.total {
		return
	}
	pw.lastTime = now
	if pw.total > 0 {
		pct := pw.written * 100 / pw.total
		fmt.Printf("\r%s: %d%% (%s / %s)   ", pw.label, pct, formatSize(pw.written), formatSize(pw.total))
	} else {
		// P0-#2:ContentLength=-1 降级显示 已写字节数 + 速率
		elapsed := now.Sub(pw.start)
		rate := int64(0)
		if elapsed > 0 {
			rate = int64(float64(pw.written) / elapsed.Seconds())
		}
		fmt.Printf("\r%s: %s (%s/s)   ", pw.label, formatSize(pw.written), formatSize(rate))
	}
}

// finish 在所有数据写完后强制刷新一次最终状态。
func (pw *progressWriter) finish() {
	if pw.total > 0 {
		fmt.Printf("\r%s: 100%% (%s / %s)   ", pw.label, formatSize(pw.written), formatSize(pw.total))
	} else {
		elapsed := time.Since(pw.start)
		rate := int64(0)
		if elapsed > 0 {
			rate = int64(float64(pw.written) / elapsed.Seconds())
		}
		fmt.Printf("\r%s: %s (%s/s)   ", pw.label, formatSize(pw.written), formatSize(rate))
	}
}

// === SSE scanner + base64 辅助 ===

type sseEvent struct {
	event string
	data  []byte
}

type sseScanner struct {
	r     *bufioReader
	cur   sseEvent
	pendingEvent string
}

func newSSEScanner(r io.Reader) *sseScanner {
	return &sseScanner{r: newBufioReader(r)}
}

// Next 返回下一个完整 SSE 事件(event/data 配对,空行分隔)。
func (s *sseScanner) Next() (*sseEvent, error) {
	var event string
	var dataBuf bytes.Buffer
	for {
		line, err := s.r.ReadString('\n')
		if err != nil && line == "" {
			if event != "" || dataBuf.Len() > 0 {
				s.cur = sseEvent{event: event, data: dataBuf.Bytes()}
				return &s.cur, nil
			}
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			// 空行 = 事件边界
			if event != "" || dataBuf.Len() > 0 {
				s.cur = sseEvent{event: event, data: dataBuf.Bytes()}
				return &s.cur, nil
			}
			continue
		}
		if strings.HasPrefix(line, "event:") {
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		} else if strings.HasPrefix(line, "data:") {
			if dataBuf.Len() > 0 {
				dataBuf.WriteByte('\n')
			}
			dataBuf.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
		if err != nil {
			break
		}
	}
	if event != "" || dataBuf.Len() > 0 {
		s.cur = sseEvent{event: event, data: dataBuf.Bytes()}
		return &s.cur, nil
	}
	return nil, io.EOF
}

// readLine 从共享 stdin 读一行(去除换行符)。
// C4 修复:必须用 sharedStdinReader 复用 bufio buffer,否则管道输入丢数据。
func readLine() (string, error) {
	r := sharedStdinReader()
	line, err := r.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func printUsage() {
	fmt.Print(`go-rmm v3.9 —— 企业级零信任远程访问平台 CLI

用法:
  go-rmm [全局 flags] <command> [args]

全局 flags:
  --relay <URL>    Relay 地址(默认 http://127.0.0.1:7780,环境变量 GO_RMM_RELAY)
  --token <TOKEN>  鉴权 token(环境变量 GO_RMM_TOKEN)
  -o json          JSON 输出模式(脚本可解析)
  --insecure       跳过 TLS 校验(自签证书)
  --cacert <path>  自定义 CA 证书

退出码: 0=成功 | 1=错误 | 2=任务失败 | 3=超时 | 4=未找到 | 5=策略拦截

 ══════════════════════════════════════════════
 诊断 (Diagnostics)
══════════════════════════════════════════════
  status                         Relay 状态(healthz+metrics+agents)
  bootstrap [--host IP]          生成一行远程部署命令(复制到目标机执行)
  sysinfo -a <agent>             远端系统体检(CPU/内存/磁盘/网络)
  topology                       多 Agent 拓扑视图(按 tag 分组)
  diagnose --tag <tag>           批量诊断(并行体检+告警汇总)
  compare <a1> <a2>              差异分析(OS/内存/服务对比)
  net ping/dns/trace/ports/check 远端网络诊断

══════════════════════════════════════════════
 执行 (Execution)
══════════════════════════════════════════════
  task run -a <agent> -c <cmd>   下发命令并等待结果
  task stream -a <agent> -c <cmd> 流式执行(SSE 实时 stdout)
  exec -a <a1,a2> -c <cmd>       多 Agent 并行执行
  deploy batch <pb.json> --tag <t> 多 Agent 并行部署
  workflow run <wf.json>         多步编排(条件+变量提取+on_failure)
  shell open -a <agent>          交互式终端(录屏归档)

══════════════════════════════════════════════
 传输 (Transfer)
══════════════════════════════════════════════
  file upload -a <agent> <l> <r> 上传文件(SHA256 双边校验)
  file download -a <agent> <r> <l> 下载文件
  sync push -a <agent> <dir> <r> 目录级同步(本地→远端)
  sync pull -a <agent> <r> <dir> 目录级同步(远端→本地)

══════════════════════════════════════════════
 部署 (Deploy)
══════════════════════════════════════════════
  deploy run <playbook.json>     多步编排(upload→exec→verify)
  deploy validate <playbook>     校验 playbook
  tunnel -L <port>:<h>:<p> -a <a> 端口转发(SSH -L 体验)
  update push -a <agent> --ver <v> Agent 自更新(rename+copy+重启)

══════════════════════════════════════════════
 主从救援 (Master-Slave Rescue)
══════════════════════════════════════════════
  rescue status -a <master>      查主监控的从的状态
  rescue restart -a <master>     经主重启从服务
  rescue rollback -a <master>    经主 .bak 回滚从
  rescue update -a <slave> -v V  经 Relay 推版本救援从

══════════════════════════════════════════════
 配置与搜索 (Config & Search)
══════════════════════════════════════════════
  config get -a <agent> <file>   读取远端配置
  config set -a <agent> <f> <k=v> 修改配置(自动备份)
  find name -a <agent> <pattern> 按文件名查找
  find grep -a <agent> <pattern> 文件内容搜索
  logs tail -a <agent> <file>    实时 tail 远端日志

══════════════════════════════════════════════
 监控 (Monitoring)
══════════════════════════════════════════════
  monitor top -a <agent>         全维度资源监控(glances TUI)
  monitor cpu/disk/net/gpu       单维度监控
  snapshot take -a <agent>       系统状态快照
  snapshot diff <s1.json> <s2>   快照差异对比
  events                        实时事件流(agent上线/任务结果/策略拦截)
  task stream -a <agent> -c <cmd> 流式任务(SSE 实时 stdout,非轮询)
  health-check [--tag] [--schedule] 健康巡检(一次性/定时 cron)
  alert add --agent <id> --metric cpu --gt 90  告警规则(CPU/内存/磁盘阈值)
  alert list                     列出告警规则
  inventory [--format csv|json]  Agent 清单导出
  history [-n 100]              历史趋势统计(成功率+分布)

══════════════════════════════════════════════
 管理 (Management)
══════════════════════════════════════════════
  agent list [tag]               列出 Agent(tag 过滤)
  agent revoke/enroll-token      Agent 证书管理
  service list/start/stop -a <a> Windows 服务管理
  proc list/kill -a <agent>      进程管理
  power shutdown/restart -a <a>  电源操作(需标签授权)
  watch add -a <agent> --svc <n> 进程守护(自动重启)
  cron list/add/delete           定时任务

══════════════════════════════════════════════
 工具 (Tools)
══════════════════════════════════════════════
  events                        实时事件流(agent上线/任务结果/策略拦截)
  completion <bash|zsh|pwsh|fish> 生成 shell 补全
  version                        版本信息

示例:
  go-rmm status
  go-rmm task run -a win-pc -c "Get-Date"
  go-rmm exec --tag env=prod -c "Get-Service MyApp"
  go-rmm monitor top -a win-pc
  go-rmm tunnel -L 3306:db:3306 -a agent-01
  go-rmm deploy run deploy.json
  go-rmm snapshot take -a win-pc --output json > baseline.json
`)
}
