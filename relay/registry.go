package main

import (
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/QiuShichang/go-rmm/proto"
)

// agentEntry 一个在线 Agent 的全部状态。
type agentEntry struct {
	info       proto.Register   // 注册信息
	conn       *wsConn          // WS 连接
	lastBeat   time.Time        // 最近一次心跳时间
	lastSeq    int64            // 最近心跳序号
	registeredAt time.Time      // 注册时间
}

// Registry 维护在线 Agent 注册表。
//
// 线程安全：所有方法内部加锁。Relay 的所有组件（WS handler、REST handler、SSE 推送器）
// 都通过 Registry 读写 Agent 状态。
type Registry struct {
	mu     sync.RWMutex
	agents map[string]*agentEntry // agentID → entry
}

func NewRegistry() *Registry {
	return &Registry{agents: make(map[string]*agentEntry)}
}

// Register 登记一个 Agent 上线。若已存在则覆盖（重连场景）。
func (r *Registry) Register(info proto.Register, conn *wsConn) (wasOnline bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, wasOnline = r.agents[info.AgentID]
	r.agents[info.AgentID] = &agentEntry{
		info:         info,
		conn:         conn,
		lastBeat:     time.Now(),
		registeredAt: time.Now(),
	}
	return wasOnline
}

// Unregister 移除一个 Agent。仅当 conn 匹配时才移除（防止新连接被旧连接的断开事件误删）。
func (r *Registry) Unregister(agentID string, conn *wsConn) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.agents[agentID]
	if !ok || e.conn != conn {
		return false
	}
	delete(r.agents, agentID)
	return true
}

// Touch 更新心跳时间与序号。
func (r *Registry) Touch(agentID string, seq int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.agents[agentID]
	if !ok {
		return false
	}
	e.lastBeat = time.Now()
	e.lastSeq = seq
	return true
}

// Conn 取 Agent 的连接（用于转发任务）。不存在返回 nil。
func (r *Registry) Conn(agentID string) *wsConn {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if e, ok := r.agents[agentID]; ok {
		return e.conn
	}
	return nil
}

// Snapshot 返回所有在线 Agent 的注册信息快照（供控制台列表展示）。
func (r *Registry) Snapshot() []proto.Register {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]proto.Register, 0, len(r.agents))
	for _, e := range r.agents {
		out = append(out, e.info)
	}
	return out
}

// FindByTag 返回带指定标签的在线 Agent ID 列表（C10 批量下发用）。
// tag 格式 "key=value"，匹配 Agent 的 Tags。
func (r *Registry) FindByTag(tag string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []string
	for id, e := range r.agents {
		for k, v := range e.info.Tags {
			if k+"="+v == tag {
				out = append(out, id)
				break
			}
		}
	}
	return out
}

// Sweep 清理超时未心跳的 Agent，返回被清理的 agentID 列表。
// Sweep 清理超时未心跳的 Agent，返回被清理的 agentID 列表。
// 超时阈值建议为心跳间隔的 3 倍（默认心跳 15s → 阈值 45s）。
//
// R4 修复：删除条目时主动关闭底层 WS 连接，驱动 serveAgent 的 Read 循环退出，
// 避免连接泄漏与状态分裂（Registry 已删但 serveAgent 仍在读）。
func (r *Registry) Sweep(timeout time.Duration) []string {
	r.mu.Lock()
	var removed []string
	var connsToClose []*wsConn
	now := time.Now()
	for id, e := range r.agents {
		if now.Sub(e.lastBeat) > timeout {
			delete(r.agents, id)
			removed = append(removed, id)
			connsToClose = append(connsToClose, e.conn)
		}
	}
	r.mu.Unlock()
	// 在锁外关闭连接（close 可能触发对方回调，避免持锁死锁）
	for _, c := range connsToClose {
		c.close(websocket.StatusPolicyViolation, "heartbeat timeout")
	}
	return removed
}
