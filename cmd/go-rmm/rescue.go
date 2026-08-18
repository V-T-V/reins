// CLI rescue 命令(M1 主从架构):主从模式下的故障救援。
//
// 用法:
//
//	go-rmm rescue status -a <master-agent-id>       查主监控的从的状态
//	go-rmm rescue restart -a <master-agent-id>      经主重启从服务
//	go-rmm rescue rollback -a <master-agent-id>     经主 .bak 回滚从
//	go-rmm rescue update -a <slave-agent-id> -v V   经 Relay 推指定版本救援
//
// 这些命令实际上都是通过主 Agent 或直接调 Relay API 来操作从 Agent。
// rescue restart/rollback 通过下发 powershell 给主 Agent,主在本地操作从服务。
// rescue update 直接调 Relay 的 /api/updates/apply(复用现有机制)。
package main

import (
	"context"
	"fmt"
	"time"
)

// cmdRescue rescue 子命令入口。
func cmdRescue(cfg config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("用法: go-rmm rescue <status|restart|rollback|update> -a <agent-id>")
	}
	switch args[0] {
	case "status":
		return rescueStatus(cfg, args[1:])
	case "restart":
		return rescueRestart(cfg, args[1:])
	case "rollback":
		return rescueRollback(cfg, args[1:])
	case "update":
		return rescueUpdate(cfg, args[1:])
	default:
		return fmt.Errorf("未知 rescue 子命令: %s (可用: status/restart/rollback/update)", args[0])
	}
}

// runRescuePS 向主 Agent 下发救援 powershell 并等待结果展示。
// 2026-08-18 修复:原实现手工构造 body 缺 task_id,被 handlePostTask 拒
// ("task_id 不能为空")——rescue 全系命令实际不可用。
func runRescuePS(cfg config, masterID, desc, cmd string, timeout int) error {
	taskID := "rescue-" + desc + "-" + fmt.Sprint(time.Now().UnixNano())
	body := map[string]any{
		"task_id":  taskID,
		"agent_id": masterID,
		"type":     "powershell",
		"payload":  map[string]string{"command": cmd},
		"timeout":  timeout,
	}
	var resp struct {
		TaskID  string `json:"task_id"`
		Status  string `json:"status"`
		Message string `json:"message"`
	}
	if err := apiPost(cfg, "/api/tasks", body, &resp); err != nil {
		return err
	}
	if !jsonMode {
		fmt.Printf("已下发到主 Agent %s(%s),task_id: %s\n", masterID, desc, resp.TaskID)
	}
	if resp.Status == "pending" && resp.Message != "" {
		if !jsonMode {
			fmt.Printf("Agent 离线,任务已缓存: %s\n", resp.Message)
		}
		return nil
	}
	done, _ := waitResult(context.Background(), cfg, taskID, timeout)
	if !done && !jsonMode {
		fmt.Println("等待超时,可用 task result 查询结果")
	}
	return nil
}

// rescueStatus 查从的看门狗状态(读 slave-state.json)。
func rescueStatus(cfg config, args []string) error {
	masterID := extractArg(args, "-a")
	if masterID == "" {
		return fmt.Errorf("用法: go-rmm rescue status -a <master-agent-id>\n  (查主 Agent 监控的从的状态)")
	}
	cmd := `Get-Content (Join-Path $env:ProgramFiles "go-rmm-master\data\slave-state.json") -ErrorAction SilentlyContinue; if (-not $?) { "无状态记录(从可能从未故障)" }`
	return runRescuePS(cfg, masterID, "status", cmd, 30)
}

// rescueRestart 下发重启从服务的命令(经主 Agent 本地 sc.exe)。
func rescueRestart(cfg config, args []string) error {
	masterID := extractArg(args, "-a")
	if masterID == "" {
		return fmt.Errorf("用法: go-rmm rescue restart -a <master-agent-id>")
	}
	cmd := `sc.exe stop go-rmm-slave 2>$null; Start-Sleep 2; sc.exe start go-rmm-slave; Start-Sleep 2; sc.exe query go-rmm-slave | Select-String "STATE"`
	return runRescuePS(cfg, masterID, "restart", cmd, 60)
}

// rescueRollback 下发 .bak 回滚命令(经主 Agent 本地 rename)。
func rescueRollback(cfg config, args []string) error {
	masterID := extractArg(args, "-a")
	if masterID == "" {
		return fmt.Errorf("用法: go-rmm rescue rollback -a <master-agent-id>")
	}
	cmd := `
$slaveDir = Join-Path $env:ProgramFiles "go-rmm-slave"
$exe = Join-Path $slaveDir "go-rmm-slave.exe"
$bak = $exe + ".bak"
if (-not (Test-Path $bak)) { "无 .bak 备份,无法回滚"; exit 1 }
sc.exe stop go-rmm-slave 2>$null
Start-Sleep 2
Move-Item $exe ($exe + ".bad") -Force -ErrorAction SilentlyContinue
Move-Item $bak $exe -Force
sc.exe start go-rmm-slave
Start-Sleep 2
"回滚完成: " + (sc.exe query go-rmm-slave | Select-String "STATE")
`
	return runRescuePS(cfg, masterID, "rollback", cmd, 90)
}

// rescueUpdate 经 Relay 直接给从推版本(复用 update push 机制)。
// 这是最强的救援:推一个已知好的版本覆盖故障的从。
func rescueUpdate(cfg config, args []string) error {
	slaveID := extractArg(args, "-a")
	version := extractArg(args, "-v")
	if slaveID == "" || version == "" {
		return fmt.Errorf("用法: go-rmm rescue update -a <slave-agent-id> -v <version>\n  (直接给从推指定版本,复用 update 机制)")
	}
	// 复用 update push 逻辑(直接调 apply API)
	body := map[string]any{
		"agent_id": slaveID,
		"version":  version,
		"force":    true, // 救援场景强制更新
	}
	var resp map[string]any
	if err := apiPost(cfg, "/api/updates/apply", body, &resp); err != nil {
		return err
	}
	fmt.Printf("✅ 救援更新已下发:从 %s → 版本 %s (force=true)\n", slaveID, version)
	fmt.Printf("   task_id: %v\n", resp["task_id"])
	fmt.Printf("   从将走 rename+copy+SCM 重启,用 agent list 查版本变化\n")
	return nil
}
