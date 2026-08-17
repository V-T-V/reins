package main

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/QiuShichang/go-rmm/proto"
)

// agentEntry 一个在线 Agent 的全部状态。
type agentEntry struct {
	info         proto.Register  // 注册信息
	conn         *wsConn         // WS 连接
	lastBeat     time.Time       // 最近一次心跳时间
	lastSeq      int64           // 最近心跳序号
	registeredAt time.Time       // 注册时间
	xferStats    proto.Heartbeat // V1:最近一次心跳携带的传输计数器
	inFlight     atomic.Int64    // H2:在途任务数(dispatch+1/终态·回退-1)
}

// Registry 维护在线 Agent 注册表。
//
// 线程安全:所有方法内部加锁。Relay 的所有组件(WS handler、REST handler、SSE 推送器)
// 都通过 Registry 读写 Agent 状态。
//
// D1 优化:capabilityCache 缓存 Agent 能力(避免 Supports() 每次遍历)。
type Registry struct {
	mu              sync.RWMutex
	agents          map[string]*agentEntry     // agentID → entry
	capabilityCache map[string]map[string]bool // agentID → {taskType: true}(D1 缓存)
}

func NewRegistry() *Registry {
	return &Registry{
		agents:          make(map[string]*agentEntry),
		capabilityCache: make(map[string]map[string]bool),
	}
}

// Register 登记一个 Agent 上线。若已存在则覆盖(重连场景)。
// D1:注册时更新 capabilityCache。
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
	// D1:更新能力缓存(O(1) 查找替代 O(N) 遍历)
	caps := make(map[string]bool, len(info.Capabilities))
	for _, c := range info.Capabilities {
		caps[c] = true
	}
	r.capabilityCache[info.AgentID] = caps
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
	delete(r.capabilityCache, agentID) // D1:清理缓存
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

// TouchWithStats 更新心跳 + 存储传输计数器(V1:Agent 心跳上报的 per-agent 画像)。
func (r *Registry) TouchWithStats(hb proto.Heartbeat) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.agents[hb.AgentID]
	if !ok {
		return false
	}
	e.lastBeat = time.Now()
	e.lastSeq = hb.Seq
	e.xferStats = hb // 存完整心跳(含 ub/db/uc/dc/fc)
	return true
}

// XferStatsOf 返回指定 Agent 的传输计数器快照(V1:/api/agents 输出用)。
func (r *Registry) XferStatsOf(agentID string) (proto.Heartbeat, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.agents[agentID]
	if !ok {
		return proto.Heartbeat{}, false
	}
	return e.xferStats, true
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

// FindByTag 返回带指定标签的在线 Agent ID 列表(C10 批量下发用)。
// tag 格式 "key=value",匹配 Agent 的 Tags。
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

// Capabilities 返回指定 Agent 的能力列表(S3 能力感知路由用)。
func (r *Registry) Capabilities(agentID string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if e, ok := r.agents[agentID]; ok {
		return e.info.Capabilities
	}
	return nil
}

// Supports 检查指定 Agent 是否支持给定任务类型。
// D1 优化:用 capabilityCache O(1) 查找,替代 O(N) 遍历 Capabilities slice。
func (r *Registry) Supports(agentID, taskType string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if caps, ok := r.capabilityCache[agentID]; ok {
		return caps[taskType] // O(1) hash 查找
	}
	return false
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

// IncInFlight/DecInFlight/InFlight:在途任务计数(H2,供 SelectBestAgent O(1) 读)。
func (r *Registry) IncInFlight(agentID string) {
	r.mu.RLock(); e := r.agents[agentID]; r.mu.RUnlock()
	if e != nil { e.inFlight.Add(1) }
}
func (r *Registry) DecInFlight(agentID string) {
	r.mu.RLock(); e := r.agents[agentID]; r.mu.RUnlock()
	if e == nil { return }
	// H-3:CAS 下限钳 0(防 cancel后result/重连清零后旧任务 Dec 致负漂移)
	for {
		old := e.inFlight.Load()
		if old <= 0 { return }
		if e.inFlight.CompareAndSwap(old, old-1) { return }
	}
}
func (r *Registry) InFlight(agentID string) int64 {
	r.mu.RLock(); e := r.agents[agentID]; r.mu.RUnlock()
	if e == nil { return 0 }
	return e.inFlight.Load()
}
