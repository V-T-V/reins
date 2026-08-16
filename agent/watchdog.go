// Package main — 主 Agent 看门狗(M1 主从架构)。
//
// 本地看门狗 goroutine:主 Agent 每 30 秒检查从 Agent 的服务状态,
// 从挂了就在本地拉起(不依赖 Relay)。这是"主救援从"的兜底通道,
// 即使 Relay 宕机、网络中断,主仍能本地救援从。
//
// 救援策略(三级):
//  1. 轻度(服务停了):sc.exe start 拉起
//  2. 中度(连续 3 次启动失败):.bak 回滚(rename .bak→exe 再 start)
//  3. 重度(回滚也失败):记 slave-state.json,等 Relay 恢复后告警
//
// 不依赖 Relay,纯本地 sc.exe + 文件操作。

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// watchdogInterval 看门狗检查周期。
// 30s 平衡响应速度与系统开销。
const watchdogInterval = 30 * time.Second

// maxStartRetries 连续启动失败多少次后触发 .bak 回滚。
const maxStartRetries = 3

// slaveStatePath 从状态记录文件路径(master 的 data 目录下)。
// 供 Relay/CLI 查询救援历史。
func slaveStatePath() string {
	return filepath.Join(agentDataDir(), "slave-state.json")
}

// slaveState 从的状态快照(每次救援后更新)。
type slaveState struct {
	SlaveAgentID  string    `json:"slave_agent_id"`
	SlaveService  string    `json:"slave_service"`
	LastStatus    string    `json:"last_status"` // running / stopped / unknown
	LastCheck     time.Time `json:"last_check"`
	RescueCount   int       `json:"rescue_count"`    // 累计救援次数
	LastRescue    time.Time `json:"last_rescue"`     // 最近一次救援时间
	LastRescueMsg string    `json:"last_rescue_msg"` // 最近一次救援描述
	RollbackCount int       `json:"rollback_count"`  // .bak 回滚次数
}

// StartWatchdog 启动看门狗 goroutine(仅 master 角色调用)。
//
// 非 master 角色或 SlaveExeDir 未配置时直接返回(无操作)。
// goroutine 生命周期与 Agent 主进程绑定(不响应 ctx,简化实现)。
func StartWatchdog(cfg Config) {
	if cfg.Role != "master" {
		return
	}
	if cfg.SlaveExeDir == "" {
		log.Printf("[watchdog] 未配置 slave_exe_dir,看门狗禁用")
		return
	}
	if runtime.GOOS != "windows" {
		log.Printf("[watchdog] 非 Windows 平台,看门狗禁用(当前 %s)", runtime.GOOS)
		return
	}
	slaveSvc := cfg.DerivedSlaveServiceName()
	log.Printf("[watchdog] 启动:监控从服务 %s (exeDir=%s)", slaveSvc, cfg.SlaveExeDir)
	go watchdogLoop(cfg, slaveSvc)
}

// DerivedSlaveServiceName 推导从的服务名。
// slave 的服务名通常是 go-rmm-slave,但允许从 config 推导。
func (c Config) DerivedSlaveServiceName() string {
	// 从的服务名:如果 slave 显式配了 ServiceName 用它,否则按 slave role 推导
	// 注意:这里我们无法读到 slave 的 config,用约定:go-rmm-slave
	// (主从配对时 install.ps1 会保证命名一致)
	return "go-rmm-slave"
}

// watchdogLoop 看门狗主循环。
func watchdogLoop(cfg Config, slaveSvc string) {
	// 启动后先等一个周期,给从服务启动时间
	consecutiveFailures := 0
	firstCheck := true

	for {
		time.Sleep(watchdogInterval)
		status := queryServiceStatus(slaveSvc)

		if status == "running" {
			if !firstCheck && consecutiveFailures > 0 {
				log.Printf("[watchdog] 从服务 %s 已恢复运行(之前连续失败 %d 次)", slaveSvc, consecutiveFailures)
			}
			consecutiveFailures = 0
			writeSlaveState(cfg, slaveSvc, "running", "", 0)
			firstCheck = false
			continue
		}

		// 从未运行 → 救援
		firstCheck = false
		consecutiveFailures++
		log.Printf("[watchdog] 从服务 %s 状态=%s,启动失败累计 %d/%d,触发救援",
			slaveSvc, status, consecutiveFailures, maxStartRetries)

		msg := rescueSlave(cfg, slaveSvc, consecutiveFailures)
		writeSlaveState(cfg, slaveSvc, status, msg, consecutiveFailures)

		// 连续失败超阈值 → 尝试 .bak 回滚
		if consecutiveFailures >= maxStartRetries {
			log.Printf("[watchdog] 连续 %d 次救援失败,尝试 .bak 回滚", consecutiveFailures)
			if rollbackSlave(cfg) {
				log.Printf("[watchdog] .bak 回滚成功,重置失败计数")
				consecutiveFailures = 0
				writeSlaveState(cfg, slaveSvc, "rolled_back", ".bak 回滚成功", 0)
			} else {
				log.Printf("[watchdog] .bak 回滚失败,等待下个周期重试")
			}
		}
	}
}

// rescueSlave 执行一次救援:尝试 sc.exe start。
// 返回救援描述(用于 state 记录)。
func rescueSlave(cfg Config, slaveSvc string, attempt int) string {
	// 1. 尝试启动服务
	out, err := exec.Command("sc.exe", "start", slaveSvc).CombinedOutput()
	outStr := strings.TrimSpace(string(out))
	if err == nil {
		log.Printf("[watchdog] sc start %s 成功: %s", slaveSvc, outStr)
		return fmt.Sprintf("sc start 成功(第 %d 次): %s", attempt, outStr)
	}
	// 启动失败可能是服务已禁用/二进制损坏/依赖缺失
	log.Printf("[watchdog] sc start %s 失败(第 %d 次): %v, out=%s", slaveSvc, attempt, err, outStr)
	return fmt.Sprintf("sc start 失败(第 %d 次): %s", attempt, outStr)
}

// rollbackSlave 从的 .bak 回滚:rename 当前 exe→.bad, rename .bak→exe。
// 回滚后由 SCM 的 OnFailure=restart 自动拉起旧版本。
//
// 路径约定:
//   - 从 exe: <SlaveExeDir>/go-rmm-slave.exe
//   - 备份:   <SlaveExeDir>/go-rmm-slave.exe.bak
//   - 损坏:   <SlaveExeDir>/go-rmm-slave.exe.bad
func rollbackSlave(cfg Config) bool {
	slaveExe := filepath.Join(cfg.SlaveExeDir, "go-rmm-slave.exe")
	bakPath := slaveExe + ".bak"
	badPath := slaveExe + ".bad"

	// 1. 检查 .bak 是否存在
	if _, err := os.Stat(bakPath); err != nil {
		log.Printf("[watchdog] .bak 不存在,无法回滚: %s", bakPath)
		return false
	}

	// 2. 先停服务(避免文件锁)
	exec.Command("sc.exe", "stop", cfg.DerivedSlaveServiceName()).Run()
	time.Sleep(2 * time.Second)

	// 3. rename 坏 exe → .bad(保留以供分析)
	_ = os.Remove(badPath) // 清理旧 .bad
	if err := os.Rename(slaveExe, badPath); err != nil {
		log.Printf("[watchdog] rename 坏 exe→.bad 失败: %v", err)
		// 坏 exe 可能被锁,继续尝试 .bak 覆盖(Windows 允许 rename 运行中的 exe,但被锁的会失败)
	}

	// 4. rename .bak → exe
	if err := os.Rename(bakPath, slaveExe); err != nil {
		log.Printf("[watchdog] rename .bak→exe 失败: %v", err)
		return false
	}
	log.Printf("[watchdog] .bak 回滚完成: %s → %s", bakPath, slaveExe)

	// 5. 启动回滚后的服务
	if _, err := exec.Command("sc.exe", "start", cfg.DerivedSlaveServiceName()).CombinedOutput(); err != nil {
		log.Printf("[watchdog] 回滚后 sc start 失败(等 SCM 自动拉起): %v", err)
	}
	return true
}

// queryServiceStatus 用 sc.exe query 查询服务状态。
// 返回 "running" / "stopped" / "unknown"。
func queryServiceStatus(svcName string) string {
	out, err := exec.Command("sc.exe", "query", svcName).CombinedOutput()
	if err != nil {
		return "unknown" // 服务不存在或查询失败
	}
	// 解析 "STATE: 4 RUNNING" 或 "STATE: 1 STOPPED"
	outStr := strings.ToUpper(string(out))
	if strings.Contains(outStr, "RUNNING") {
		return "running"
	}
	if strings.Contains(outStr, "STOPPED") || strings.Contains(outStr, "STOP_PENDING") {
		return "stopped"
	}
	if strings.Contains(outStr, "START_PENDING") {
		return "running" // 正在启动,视为 running
	}
	return "unknown"
}

// writeSlaveState 记录从的状态到 slave-state.json。
// 供 Relay/CLI 查询救援历史(失败不影响主流程,仅记录)。
func writeSlaveState(cfg Config, slaveSvc, status, rescueMsg string, failCount int) {
	state := slaveState{
		SlaveAgentID:  cfg.SlaveAgentID,
		SlaveService:  slaveSvc,
		LastStatus:    status,
		LastCheck:     time.Now(),
		LastRescueMsg: rescueMsg,
	}
	if rescueMsg != "" {
		state.LastRescue = time.Now()
		state.RescueCount++
	}
	// 读旧 state 累加 rollback count
	if old, err := readSlaveState(); err == nil {
		state.RollbackCount = old.RollbackCount
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(agentDataDir(), 0755)
	_ = os.WriteFile(slaveStatePath(), data, 0644) // 非敏感,0644 即可
}

// readSlaveState 读取从状态(供 CLI/Relay 查询)。
func readSlaveState() (*slaveState, error) {
	data, err := os.ReadFile(slaveStatePath())
	if err != nil {
		return nil, err
	}
	var s slaveState
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}
