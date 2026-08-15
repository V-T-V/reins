// Package main — Agent 隧道(端口转发)管理器。
//
// 阶段 3.1:本地端口转发(`ssh -L` 体验)。
//
// 工作原理:
//   - 收到 TunnelOpen → net.Dial(target) → 回 TunnelReady
//   - 远端 conn → BuildTunnelFrame → WS 写(经 recvLoop 的 Write)
//   - 收到隧道二进制帧 → 按 tunnelID 路由到对应 conn
//   - 任一方 EOF → 发 TunnelClose + 清理
//
// 与文件传输/shell 的差异:隧道是双向无终止字节流,
// 需要两个 goroutine(本地→远端 / 远端→本地)per tunnel。
package main

import (
	"context"
	"io"
	"log"
	"net"
	"sync"
	"time"


	"github.com/QiuShichang/go-rmm/proto"
)

// TunnelManager 管理所有活跃隧道会话。
type TunnelManager struct {
	mu      sync.Mutex
	tunnels map[string]*tunnelSession
}

// tunnelSession 单条隧道(一对本地 conn + 控制)。
type tunnelSession struct {
	id     string
	conn   net.Conn
	cancel context.CancelFunc
}

// tunnels 全局实例。
var tunnelMgr = &TunnelManager{tunnels: make(map[string]*tunnelSession)}

// HandleOpen 处理 TunnelOpen 消息:net.Dial 远端 + 启动双向转发。
// writeBin 是发送二进制帧到 Relay 的回调(由 recvLoop 注入,串行化 WS 写)。
func (tm *TunnelManager) HandleOpen(open proto.TunnelOpen, writeBin func([]byte) error) {
	log.Printf("[tunnel] 开启隧道 %s → %s", open.TunnelID, open.Target)

	ready := proto.TunnelReady{TunnelID: open.TunnelID}
	// dial 远端(带超时)
	network := open.Network
	if network == "" {
		network = "tcp"
	}
	start := time.Now()
	conn, err := net.DialTimeout(network, open.Target, 10*time.Second)
	if err != nil {
		ready.Accepted = false
		ready.Reason = "dial 失败: " + err.Error()
		log.Printf("[tunnel] dial 失败 %s: %v", open.Target, err)
		// 通过 Envelope 回 ready(失败)
		tm.sendReady(ready, false)
		return
	}
	ready.Accepted = true
	ready.Latency = time.Since(start).Milliseconds()

	// P0-A 修复:先注册 session 再发 ready。
	// 原顺序(sendReady → 注册 session)下,CLI 在收到 ready 后立即发数据,
	// 数据帧可能先于 session 注册到达 HandleData 而被静默丢弃("隧道已关闭或不存在")。
	ctx, cancel := context.WithCancel(context.Background())
	sess := &tunnelSession{
		id:     open.TunnelID,
		conn:   conn,
		cancel: cancel,
	}
	_ = ctx // 保留 ctx 供未来超时控制(当前 dial 用 DialTimeout)
	tm.mu.Lock()
	tm.tunnels[open.TunnelID] = sess
	tm.mu.Unlock()

	tm.sendReady(ready, true)
	log.Printf("[tunnel] 隧道 %s 就绪 → %s (latency=%dms)",
		open.TunnelID, open.Target, ready.Latency)

	// goroutine: 远端 → WS(读 conn,发隧道帧)
	go func() {
		defer tm.Close(open.TunnelID, "eof")
		buf := Get32K() // H1:pool 复用(原 make 32K)
		defer Put32K(buf)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				frame := proto.BuildTunnelFrame(proto.ChanTypeTunnelData, open.TunnelID, 0, buf[:n])
				if werr := writeBin(frame); werr != nil {
					log.Printf("[tunnel] WS 写失败 %s: %v", open.TunnelID, werr)
					return
				}
			}
			if err != nil {
				if err != io.EOF {
					log.Printf("[tunnel] 远端读结束 %s: %v", open.TunnelID, err)
				}
				return
			}
		}
	}()
}

// HandleData 处理收到的隧道数据帧(WS → 远端 conn)。
func (tm *TunnelManager) HandleData(tunnelID string, data []byte) {
	tm.mu.Lock()
	sess, ok := tm.tunnels[tunnelID]
	tm.mu.Unlock()
	if !ok {
		return // 隧道已关闭或不存在,丢弃
	}
	// W4 修复:写远端加超时。原写无超时,慢消费(如向慢远端 SSH 上传)会阻塞 recvLoop,
	// 导致 Relay 的 heartbeat_ack 堆在 TCP 接收缓冲无人读 → deadConnWatchdog 误判死连接
	// → 强制重连 → 所有隧道/传输中断。10s 写超时让慢写不拖垮心跳探活。
	sess.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := sess.conn.Write(data); err != nil {
		log.Printf("[tunnel] 远端写失败 %s: %v", tunnelID, err)
		tm.Close(tunnelID, "write error")
	}
}

// Close 关闭指定隧道。
func (tm *TunnelManager) Close(tunnelID, reason string) {
	tm.mu.Lock()
	sess, ok := tm.tunnels[tunnelID]
	if ok {
		delete(tm.tunnels, tunnelID)
	}
	tm.mu.Unlock()
	if !ok {
		return
	}
	sess.cancel()
	sess.conn.Close()
	log.Printf("[tunnel] 关闭 %s: %s", tunnelID, reason)
}

// CloseAll 关闭所有隧道(连接断开时调用)。
func (tm *TunnelManager) CloseAll() {
	tm.mu.Lock()
	ids := make([]string, 0, len(tm.tunnels))
	for id := range tm.tunnels {
		ids = append(ids, id)
	}
	tm.mu.Unlock()
	for _, id := range ids {
		tm.Close(id, "connection lost")
	}
}

// sendReady 发送 TunnelReady 消息(经 writeEnvelope 回传 Relay)。
// 简化实现:通过全局 wsConn 引用(在 connectAndServe 中注入)。
func (tm *TunnelManager) sendReady(ready proto.TunnelReady, success bool) {
	// 由 connection.go 的 handleTunnelOpen 注入 writeEnvelope 回调
	if tunnelReadyCb != nil {
		tunnelReadyCb(ready)
	}
}

// tunnelReadyCb 由 connectAndServe 注入的回调,用于回传 TunnelReady。
var tunnelReadyCb func(proto.TunnelReady)

// available 隧道功能是否可用(始终可用,纯标准库)。
func (tm *TunnelManager) available() bool { return true }

// 辅助:占位避免 websocket 未用 import(实际 Write 经 writeBin 回调)。
