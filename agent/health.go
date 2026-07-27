// Package main — Agent 健康自检 + 自动回滚。
//
// 自更新后的安全网:
//  1. 启动时检查 .update-pending 标记
//  2. 若存在:说明刚更新,需要验证新版本是否健康
//  3. 启动 60s 倒计时,成功注册 Relay + 发首条心跳后调 ClearUpdatePending()
//  4. 60s 内未成功 → RollbackUpdate():rename 当前(坏)exe→.bad,rename .bak→exe,退出让 SCM 重启旧版
//
// 这样新版本崩溃(连不上 Relay/启动失败)会自动回到 .bak,无需人工干预。
package main

import (
	"log"
	"os"
	"path/filepath"
	"time"
)

// healthCheckTimeout 自更新后等待健康确认的超时(60s)。
const healthCheckTimeout = 60 * time.Second

// StartHealthCheck 启动健康自检(若有 .update-pending 标记)。
// 在 Agent 启动早期、连接 Relay 之前调用。
// 成功注册 Relay 后调 ConfirmUpdateHealthy() 取消倒计时。
func StartHealthCheck() {
	marker := updatePendingMarkerPath()
	data, err := os.ReadFile(marker)
	if err != nil {
		return // 无标记,正常启动
	}
	pendingVersion := string(data)
	log.Printf("[health] 检测到更新待确认标记: 新版本 %s(等待 %v 内确认健康)", pendingVersion, healthCheckTimeout)

	// 启动倒计时 goroutine
	done := make(chan struct{})
	healthCheckCancel = func() {
		close(done)
		ClearUpdatePending()
	}
	go func() {
		select {
		case <-time.After(healthCheckTimeout):
			// 超时:新版本不健康,自动回滚
			log.Printf("[health] ⚠️ 新版本 %s 在 %v 内未确认健康,执行自动回滚", pendingVersion, healthCheckTimeout)
			if err := RollbackUpdate(); err != nil {
				log.Printf("[health] ❌ 自动回滚失败: %v(需人工介入)", err)
			} else {
				log.Printf("[health] 已回滚到 .bak,退出等待 SCM 重启旧版本")
				os.Exit(0) // 退出让 SCM 拉起旧版本
			}
		case <-done:
			// 成功确认,不回滚
		}
	}()
}

// healthCheckCancel 全局 cancel 函数(由 StartHealthCheck 设置,sendRegister 成功后调用)。
var healthCheckCancel func()

// ConfirmUpdateHealthy 注册成功后调用,取消健康倒计时 + 清除标记。
func ConfirmUpdateHealthy() {
	if healthCheckCancel != nil {
		healthCheckCancel()
		healthCheckCancel = nil
	}
}

// ClearUpdatePending 更新成功确认后删除标记文件。
func ClearUpdatePending() {
	marker := updatePendingMarkerPath()
	if err := os.Remove(marker); err == nil {
		log.Printf("[health] 更新确认成功,已清除待确认标记")
	}
}

// RollbackUpdate 回滚到 .bak(新版本崩溃时调用)。
// 流程:rename 当前(坏)exe → .bad,rename .bak → exe,删除 .update-pending 标记。
// 返回后调用方应 os.Exit 让 SCM 重启旧版本。
func RollbackUpdate() error {
	currentExe, err := os.Executable()
	if err != nil {
		return err
	}
	currentExe, _ = filepath.EvalSymlinks(currentExe)
	bakPath := currentExe + ".bak"
	badPath := currentExe + ".bad"

	// 检查 .bak 是否存在
	if _, err := os.Stat(bakPath); err != nil {
		return err // 无 .bak 可回滚
	}

	// rename 当前(坏)exe → .bad(Windows 允许重命名运行中的 exe)
	os.Remove(badPath) // 清理上次回滚残留
	if err := os.Rename(currentExe, badPath); err != nil {
		return err
	}

	// rename .bak → exe(恢复旧版本)
	if err := os.Rename(bakPath, currentExe); err != nil {
		// 失败:尝试把 .bad 改回来(至少保持能跑)
		os.Rename(badPath, currentExe)
		return err
	}

	// 删除更新标记
	os.Remove(updatePendingMarkerPath())

	// .bad 保留(供诊断),下次成功更新时清理
	log.Printf("[health] 回滚完成: %s → %s(旧版),坏版本保留在 %s", currentExe, bakPath, badPath)
	return nil
}
