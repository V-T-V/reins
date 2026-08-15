// CLI 扩展命令:status / cron / agent revoke / agent enroll / task batch。
//
// 集中放扩展命令实现,main.go 只负责路由分发。
// 每个命令对接 Relay 对应的 HTTP API,见 relay/router.go。
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/QiuShichang/go-rmm/proto"
)

// === status 命令 ===

// cmdStatus 一键查看 Relay 状态:healthz + agents + metrics + 任务状态分布(P2-C 补全)。
// healthz 和 metrics 都不需要鉴权(even if token set),agents/tasks 需要。
func cmdStatus(cfg config, args []string) error {
	// 1. healthz(含文件传输指标)
	var health struct {
		Relay             string `json:"relay"`
		Store             string `json:"store"`
		Agents            int    `json:"agents"`
		UptimeS           int    `json:"uptime_s"`
		XferUploads       int64  `json:"xfer_uploads"`
		XferDownloads     int64  `json:"xfer_downloads"`
		XferUploadBytes   int64  `json:"xfer_upload_bytes"`
		XferDownloadBytes int64  `json:"xfer_download_bytes"`
		XferFailures      int64  `json:"xfer_failures"`
	}
	if err := apiGetNoAuth(cfg, "/healthz", &health); err != nil {
		// healthz 失败说明 Relay 不可达,直接返回
		return fmt.Errorf("Relay 不可达: %w", err)
	}

	// 2. metrics(无需鉴权)
	var metrics struct {
		AgentsOnline    int64 `json:"agents_online"`
		TasksSubmitted  int64 `json:"tasks_submitted"`
		TasksDispatched int64 `json:"tasks_dispatched"`
		TasksCompleted  int64 `json:"tasks_completed"`
		TasksFailed     int64 `json:"tasks_failed"`
		TasksBlocked    int64 `json:"tasks_blocked"`
		TasksCancelled  int64 `json:"tasks_cancelled"`
		TasksReaped     int64 `json:"tasks_reaped"`
		EventsDropped   int64 `json:"events_dropped"`
		UptimeSeconds   int64 `json:"uptime_seconds"`
		Goroutines      int64 `json:"goroutines"`
	}
	_ = apiGetNoAuth(cfg, "/api/metrics", &metrics) // 失败不致命

	// 3. agents(每-Agent 详情:capabilities/tags/xfer_stats)
	var agentsResp struct {
		Count  int `json:"count"`
		Agents []struct {
			AgentID      string   `json:"agent_id"`
			Hostname     string   `json:"hostname"`
			OS           string   `json:"os"`
			Version      string   `json:"version"`
			Capabilities []string `json:"capabilities"`
			Tags         []string `json:"tags"`
			XferStats    *struct {
				UploadBytes   int64 `json:"upload_bytes"`
				DownloadBytes int64 `json:"download_bytes"`
				UploadCount   int64 `json:"upload_count"`
				DownloadCount int64 `json:"download_count"`
				FailedCount   int64 `json:"failed_count"`
			} `json:"xfer_stats"`
		} `json:"agents"`
	}
	_ = apiGet(cfg, "/api/agents", &agentsResp) // 失败不致命

	// 4. 任务实时状态分布(P2-C:原本只显示累计指标,补当前 store 分布)
	statusCount := map[string]int{}
	var tasksResp struct {
		Tasks []struct {
			Status string `json:"status"`
		} `json:"tasks"`
	}
	_ = apiGet(cfg, "/api/tasks?limit=1000", &tasksResp) // 失败不致命
	for _, t := range tasksResp.Tasks {
		statusCount[t.Status]++
	}

	// P0-B:JSON 模式
	if jsonMode {
		outputJSON(map[string]any{
			"command":        "status",
			"ok":             true,
			"relay":          cfg.relay,
			"health":         health,
			"metrics":        metrics,
			"agents":         agentsResp.Agents,
			"tasks_by_status": statusCount,
		})
		return nil
	}

	fmt.Println("=== go-rmm Relay 状态 ===")
	fmt.Printf("Relay:    %s\n", cfg.relay)
	fmt.Printf("健康:     %s\n", health.Relay)
	fmt.Printf("存储:     %s\n", health.Store)
	fmt.Printf("运行时长: %s\n", formatDuration(int64(health.UptimeS)))
	fmt.Printf("Goroutine数: %d\n", metrics.Goroutines)
	fmt.Println()
	fmt.Println("--- 任务统计 ---")
	fmt.Printf("  提交: %d  分发: %d  完成: %d  失败: %d  取消: %d\n",
		metrics.TasksSubmitted, metrics.TasksDispatched,
		metrics.TasksCompleted, metrics.TasksFailed, metrics.TasksCancelled)
	fmt.Printf("  拦截(策略): %d  回收(超时): %d  事件丢弃: %d\n",
		metrics.TasksBlocked, metrics.TasksReaped, metrics.EventsDropped)
	if len(statusCount) > 0 {
		fmt.Println("  实时状态分布:")
		for _, s := range []string{proto.StatusPending, proto.StatusDispatched, proto.StatusAcknowledged, proto.StatusCompleted, proto.StatusFailed, proto.StatusCancelled} {
			if n := statusCount[s]; n > 0 {
				fmt.Printf("    %-14s %d\n", s, n)
			}
		}
	}
	fmt.Println()
	fmt.Println("--- 文件传输 ---")
	fmt.Printf("  上传: %d 次 (%s)  下载: %d 次 (%s)  失败: %d\n",
		health.XferUploads, formatSize(health.XferUploadBytes),
		health.XferDownloads, formatSize(health.XferDownloadBytes),
		health.XferFailures)
	fmt.Println()
	fmt.Printf("--- Agent: %d 在线 ---\n", agentsResp.Count)
	for _, a := range agentsResp.Agents {
		fmt.Printf("  %-40s %-20s %-12s v%s caps=%d tags=%v\n",
			a.AgentID, a.Hostname, a.OS, a.Version, len(a.Capabilities), a.Tags)
	}
	return nil
}

func formatDuration(secs int64) string {
	if secs < 60 {
		return fmt.Sprintf("%ds", secs)
	}
	if secs < 3600 {
		return fmt.Sprintf("%dm%ds", secs/60, secs%60)
	}
	if secs < 86400 {
		return fmt.Sprintf("%dh%dm", secs/3600, (secs%3600)/60)
	}
	return fmt.Sprintf("%dd%dh", secs/86400, (secs%86400)/3600)
}

// apiGetNoAuth 不带鉴权 header 的 GET(用于 healthz/metrics 公开端点)。
func apiGetNoAuth(cfg config, path string, v any) error {
	u := cfg.relay + path
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return err
	}
	resp, err := controlClient.Do(req)
	if err != nil {
		return fmt.Errorf("请求 %s 失败: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("%s: HTTP %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// === cron 子命令 ===

// 对接 Relay 的 CronRule(relay/scheduler.go):
//   ID / CronExpr / TaskType / AgentID / Payload / Timeout / Enabled

type cronRuleCLI struct {
	ID        string          `json:"id"`
	CronExpr  string          `json:"cron_expr"`
	TaskType  string          `json:"task_type"`
	AgentID   string          `json:"agent_id"`
	Payload   json.RawMessage `json:"payload"`
	Timeout   int             `json:"timeout"`
	Enabled   *bool           `json:"enabled,omitempty"`
	CreatedAt int64           `json:"created_at,omitempty"` // P1-C6:-1 哨兵表示禁用
}

func cmdCron(cfg config, args []string) error {
	if len(args) == 0 {
		return errors.New("用法: go-rmm cron <list|add|delete> [args]")
	}
	switch args[0] {
	case "list", "ls":
		return cronList(cfg, args[1:])
	case "add", "new":
		return cronAdd(cfg, args[1:])
	case "delete", "rm", "del":
		return cronDelete(cfg, args[1:])
	default:
		return fmt.Errorf("未知 cron 子命令: %s (可用: list add delete)", args[0])
	}
}

func cronList(cfg config, args []string) error {
	var resp struct {
		Crons   []cronRuleCLI `json:"crons"`
		Count   int           `json:"count"`
		Enabled bool          `json:"enabled"`
	}
	if err := apiGet(cfg, "/api/crons", &resp); err != nil {
		return err
	}
	if !resp.Enabled {
		fmt.Println("调度器未启用(Relay 未配置 scheduler)")
		return nil
	}
	if resp.Count == 0 {
		fmt.Println("暂无定时任务")
		return nil
	}
	fmt.Printf("%-26s %-18s %-14s %-24s %s\n", "ID", "CRON", "TYPE", "AGENT", "ENABLED")
	fmt.Println(strings.Repeat("-", 95))
	for _, c := range resp.Crons {
		enabled := "true"
		if c.Enabled != nil && !*c.Enabled {
			enabled = "false"
		}
		fmt.Printf("%-26s %-18s %-14s %-24s %s\n", c.ID, c.CronExpr, c.TaskType, c.AgentID, enabled)
	}
	fmt.Printf("\n共 %d 条规则\n", resp.Count)
	return nil
}

func cronAdd(cfg config, args []string) error {
	fs := flag.NewFlagSet("cron add", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	agentID := fs.String("a", "", "目标 Agent ID(必填)")
	taskType := fs.String("t", "powershell", "任务类型")
	cronExpr := fs.String("expr", "", "cron 表达式(必填,如 '0 */5 * * * *' 每5分钟 / '@every 30s')")
	command := fs.String("c", "", "PowerShell 命令(快捷方式)")
	payloadFile := fs.String("f", "", "从 JSON 文件读取 payload(高级)")
	timeout := fs.Int("timeout", 60, "任务超时秒数")
	disabled := fs.Bool("disabled", false, "创建为禁用状态")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *agentID == "" {
		return errors.New("-a <agent_id> 必填")
	}
	if *cronExpr == "" {
		return errors.New("-expr <cron表达式> 必填,如 '-expr \"@every 30s\"' 或 '-expr \"0 */5 * * * *\"'")
	}
	if !proto.IsValidTaskType(*taskType) {
		return fmt.Errorf("未知任务类型: %s", *taskType)
	}
	payload, err := buildPayload(*taskType, *command, "", *payloadFile)
	if err != nil {
		return err
	}

	rule := cronRuleCLI{
		CronExpr: *cronExpr,
		TaskType: *taskType,
		AgentID:  *agentID,
		Payload:  payload,
		Timeout:  *timeout,
	}
	if *disabled {
		f := false
		rule.Enabled = &f
		// P1-C6:用 CreatedAt=-1 哨兵告诉 scheduler 这是禁用规则
		// (bool 零值无法区分"未设"和"显式 false",用 CreatedAt 作标记)
		rule.CreatedAt = -1
	}
	var created cronRuleCLI
	if err := apiPost(cfg, "/api/crons", rule, &created); err != nil {
		return err
	}
	fmt.Printf("✅ 已创建定时任务\n")
	fmt.Printf("   ID: %s\n", created.ID)
	fmt.Printf("   Cron: %s\n", created.CronExpr)
	fmt.Printf("   Agent: %s  Type: %s\n", created.AgentID, created.TaskType)
	return nil
}

func cronDelete(cfg config, args []string) error {
	if len(args) == 0 {
		return errors.New("用法: go-rmm cron delete <rule-id>")
	}
	ruleID := args[0]
	req, err := http.NewRequest("DELETE", cfg.relay+"/api/crons/"+ruleID, nil)
	if err != nil {
		return err
	}
	if cfg.token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.token)
	}
	resp, err := controlClient.Do(req)
	if err != nil {
		return fmt.Errorf("删除失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("删除失败: %s", readError(resp.Body))
	}
	fmt.Printf("✅ 已删除: %s\n", ruleID)
	return nil
}

// === agent revoke / enroll ===

func agentRevoke(cfg config, args []string) error {
	if len(args) == 0 {
		return errors.New("用法: go-rmm agent revoke <agent-id>")
	}
	agentID := args[0]
	body := map[string]string{"agent_id": agentID}
	var resp struct {
		AgentID string `json:"agent_id"`
		Status  string `json:"status"`
	}
	if err := apiPost(cfg, "/api/agents/revoke", body, &resp); err != nil {
		return err
	}
	fmt.Printf("✅ 已撤销 %s 的证书(status=%s)\n", resp.AgentID, resp.Status)
	fmt.Printf("   该 Agent 的 mTLS 连接会被立即断开,重连被拒绝(CRL)\n")
	return nil
}

func agentEnroll(cfg config, args []string) error {
	if len(args) == 0 {
		return errors.New("用法: go-rmm agent enroll-token <agent-id>")
	}
	agentID := args[0]
	body := map[string]string{"agent_id": agentID}
	var resp struct {
		AgentID  string `json:"agent_id"`
		Token    string `json:"token"`
		ExpiresIn int   `json:"expires_in"`
	}
	if err := apiPost(cfg, "/api/enroll-token", body, &resp); err != nil {
		return err
	}
	fmt.Printf("✅ 已生成一次性注册令牌\n")
	fmt.Printf("   Agent ID: %s\n", resp.AgentID)
	fmt.Printf("   Token:    %s\n", resp.Token)
	fmt.Printf("   有效期:   %d 秒\n", resp.ExpiresIn)
	fmt.Printf("\n使用方法(在目标 Agent 上):\n")
	fmt.Printf("   go-rmm-agent enroll -relay wss://relay-host/agent -token %s -id %s\n", resp.Token, resp.AgentID)
	return nil
}

// === task batch ===

func taskBatch(cfg config, args []string) error {
	fs := flag.NewFlagSet("task batch", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	tag := fs.String("tag", "", "目标 Agent 标签(必填,如 'env=prod')")
	taskType := fs.String("t", "powershell", "任务类型")
	command := fs.String("c", "", "PowerShell 命令")
	payloadFile := fs.String("f", "", "从 JSON 文件读取 payload")
	timeout := fs.Int("timeout", 60, "任务超时秒数")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *tag == "" {
		return errors.New("-tag <标签> 必填,如 -tag env=prod")
	}
	if !proto.IsValidTaskType(*taskType) {
		return fmt.Errorf("未知任务类型: %s", *taskType)
	}
	payload, err := buildPayload(*taskType, *command, "", *payloadFile)
	if err != nil {
		return err
	}
	body := map[string]any{
		"tag":     *tag,
		"type":    *taskType,
		"payload": payload,
		"timeout": *timeout,
	}
	var resp struct {
		Tag        string            `json:"tag"`
		Dispatched []string          `json:"dispatched"`
		Count      int               `json:"count"`
		TaskIDs    map[string]string `json:"task_ids"`
	}
	if err := apiPost(cfg, "/api/tasks/batch", body, &resp); err != nil {
		// Relay 对 403(策略拦截)/404(无匹配 agent)有明确语义
		return err
	}
	fmt.Printf("✅ 批量下发完成\n")
	fmt.Printf("   标签: %s\n", resp.Tag)
	fmt.Printf("   命中 Agent: %d 个\n", resp.Count)
	for _, aid := range resp.Dispatched {
		fmt.Printf("   • %s (task=%s)\n", aid, resp.TaskIDs[aid])
	}
	return nil
}
