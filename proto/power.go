// Package proto — 电源操作任务类型。
//
// 关机/重启/休眠,经独立 task 类型(不走 powershell 黑名单)。
// Relay 侧强制校验 Agent 带 power_allowed=true 标签。
package proto

// PowerTask 电源操作任务。
type PowerTask struct {
	// Op 操作类型: shutdown(关机)/ restart(重启)/ sleep(休眠)/ lock(锁屏)
	Op string `json:"op"`
	// Force 强制(不等待应用关闭),默认 false
	Force bool `json:"force,omitempty"`
	// Delay 延迟秒数(给用户保存工作的时间),默认 0(立即)
	Delay int `json:"delay,omitempty"`
	// Message 显示给用户的提示消息(Windows 关机对话框)
	Message string `json:"message,omitempty"`
}
