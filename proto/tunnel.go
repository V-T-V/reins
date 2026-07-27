// Package proto — 隧道(端口转发)协议消息。
//
// 阶段 3.1:本地端口转发(`ssh -L` 体验)。
//
// 流程:
//  1. CLI 本地起 TCP listener,accept 连接后生成 tunnelID
//  2. CLI 发 TunnelOpen{tunnelID, network, target} 经 Relay 转给 Agent
//  3. Agent net.Dial(target) → 回 TunnelReady{tunnelID} (或 TunnelClose 表示失败)
//  4. 双向数据走二进制隧道帧(BuildTunnelFrame, chanType=0x10)
//  5. 任一方 EOF/错误 → 发 TunnelClose{tunnelID, reason}
//
// 数据帧不经过 JSON(用二进制帧),控制信令才用 JSON Envelope。
package proto

// 隧道相关消息类型(加到 message.go 的 Type 常量集)。
const (
	TypeTunnelOpen  = "tunnel_open"  // Console→Agent:开隧道
	TypeTunnelReady = "tunnel_ready" // Agent→Console:远端已连接
	TypeTunnelClose = "tunnel_close" // 双向:关闭隧道
)

// 隧道控制码(二进制帧 flags 字段,ChanTypeTunnelCtrl)。
const (
	TunnelCtrlRST byte = 0x01 // 强制重置
)

// TunnelOpen 开隧道请求(Console→Agent→远端服务)。
type TunnelOpen struct {
	TunnelID string `json:"tunnel_id"`          // 隧道唯一 ID(CLI 生成)
	Network  string `json:"network,omitempty"`   // "tcp"(默认)/"udp"
	Target   string `json:"target"`              // 远端目标 host:port(在 Agent 所在网络解析)
}

// TunnelReady Agent 已连接远端服务(Agent→Console)。
type TunnelReady struct {
	TunnelID string `json:"tunnel_id"`
	Accepted bool   `json:"accepted"`            // true=连接成功, false=失败(看 Reason)
	Reason   string `json:"reason,omitempty"`     // 失败原因
	Latency  int64  `json:"latency_ms,omitempty"` // Agent→target 的 dial 耗时(诊断用)
}

// TunnelClose 关闭隧道(双向)。
type TunnelClose struct {
	TunnelID string `json:"tunnel_id"`
	Reason   string `json:"reason,omitempty"` // "eof" / "error" / "closed"
}
