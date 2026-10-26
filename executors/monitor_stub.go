//go:build !linux

// Package executors — 非 Linux 平台的监测桩。
// Windows 走 collectWindows(PowerShell),此桩仅为满足编译(collect 分派
// 在 Windows 上永不调用);darwin 等平台无 /proc,同样返回时间戳保底。
package executors

import (
	"time"

	"github.com/QiuShichang/reins/proto"
)

// collectProcFS 非 Linux 桩:Windows 不经此路径;macOS 无 /proc 可读。
func (e *MonitorExecutor) collectProcFS(_ []string, _ int) proto.MonitorSample {
	return proto.MonitorSample{Timestamp: time.Now().Unix()}
}
