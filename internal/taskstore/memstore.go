// Package main — 内存版 TaskStore(L4 神级提升)。
//
// 实现 TaskStore 接口的纯内存版本,用途:
//  1. 单元测试:不依赖 bbolt 文件,可 mock 任意场景(终态校验/CAS/状态机)
//  2. 未来 Redis 后端的参考实现:所有方法语义一致,换 Redis hash 即可
//  3. 轻量部署:内存模式(无持久化)的底层存储
//
// 并发安全(全程 mu 串行,与 bbolt 单写事务语义一致)。
package taskstore

import (
	"fmt"
	"sync"
	"time"

	"github.com/V-T-V/reins/proto"
)

// MemoryStore 内存版 TaskStore。
type MemoryStore struct {
	mu      sync.Mutex
	tasks   map[string]*StoredTask
	results map[string]*proto.TaskResult
	// agentTenants 2026 租户加固:agentID → tenant(与 bbolt 的 agent_tenants 同语义;
	// 惰性初始化,容忍测试里的零值构造)。
	agentTenants map[string]string
}

// NewMemoryStore 构造内存存储。
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		tasks:        make(map[string]*StoredTask),
		results:      make(map[string]*proto.TaskResult),
		agentTenants: make(map[string]string),
	}
}

func (s *MemoryStore) SaveTask(t StoredTask) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := t
	s.tasks[t.Request.TaskID] = &cp
	return nil
}

func (s *MemoryStore) GetTask(taskID string) (*StoredTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[taskID]
	if !ok {
		return nil, fmt.Errorf("任务 %s 不存在", taskID)
	}
	cp := *t
	return &cp, nil
}

func (s *MemoryStore) UpdateTaskStatus(taskID, newStatus string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[taskID]
	if !ok {
		return fmt.Errorf("任务 %s 不存在", taskID)
	}
	if !validTransition(t.Status, newStatus) {
		return fmt.Errorf("非法状态转换 %s→%s", t.Status, newStatus)
	}
	t.Status = newStatus
	if t.UpdatedAt == 0 || newStatus == StatusPending {
		t.UpdatedAt = 0 // caller sets
	}
	return nil
}

func (s *MemoryStore) SaveResult(taskID string, r proto.TaskResult) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := r
	s.results[taskID] = &cp
	return nil
}

func (s *MemoryStore) GetResult(taskID string) (*proto.TaskResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.results[taskID]
	if !ok {
		return nil, fmt.Errorf("结果不存在")
	}
	cp := *r
	return &cp, nil
}

func (s *MemoryStore) ListTasksByStatus(statuses ...string) ([]StoredTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []StoredTask
	for _, t := range s.tasks {
		for _, st := range statuses {
			if t.Status == st {
				out = append(out, *t)
				break
			}
		}
	}
	return out, nil
}

func (s *MemoryStore) ListTasks(limit int) ([]StoredTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []StoredTask
	for _, t := range s.tasks {
		out = append(out, *t)
		// limit<=0=不限(与 bbolt Store.ListTasks 口径对齐;原版 limit=0 返回 1 条)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// ListTasksQuery 内存版过滤(结构体已在手,无解析可省——直配即可;
// 语义与 Store.ListTasksQuery 对齐:Statuses 非空按状态过滤,Limit 按收录数计)。
func (s *MemoryStore) ListTasksQuery(q TaskQuery) ([]StoredTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]StoredTask, 0, 64)
	statusSet := make(map[string]bool, len(q.Statuses))
	for _, st := range q.Statuses {
		statusSet[st] = true
	}
	for _, t := range s.tasks {
		if len(statusSet) > 0 && !statusSet[t.Status] {
			continue
		}
		if q.AgentID != "" && t.Request.AgentID != q.AgentID {
			continue
		}
		if q.Type != "" && t.Request.Type != q.Type {
			continue
		}
		// 2026 租户加固:与 bbolt Store 同判据——非空 Tenant 只返回该租户的任务
		// (旧记录 Tenant="" 仅 admin 可见)。两实现的租户过滤必须同步,漏一份即泄漏。
		if q.Tenant != "" && t.Tenant != q.Tenant {
			continue
		}
		out = append(out, *t)
		if q.Limit > 0 && len(out) >= q.Limit {
			break
		}
	}
	return out, nil
}

// AgentTenant 返回 Agent 的服务端绑定租户;未绑定返回 ok=false。
func (s *MemoryStore) AgentTenant(agentID string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.agentTenants[agentID]
	return t, ok
}

// BindAgentTenant insert-if-absent(锁内查无才写),返回最终生效的租户。
func (s *MemoryStore) BindAgentTenant(agentID, tenant string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if agentID == "" {
		return "", fmt.Errorf("agent_id 不能为空")
	}
	if s.agentTenants == nil {
		s.agentTenants = make(map[string]string)
	}
	if cur, ok := s.agentTenants[agentID]; ok {
		return cur, nil
	}
	s.agentTenants[agentID] = tenant
	return tenant, nil
}

func (s *MemoryStore) DeleteTask(taskID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tasks, taskID)
	delete(s.results, taskID)
	return nil
}

func (s *MemoryStore) CASDispatchToPending(taskID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[taskID]
	if !ok {
		return nil // 不在 store,允许投递
	}
	if t.Status != StatusPending {
		return fmt.Errorf("任务 %s 状态 %s,跳过", taskID, t.Status)
	}
	// W2:原子 CAS(与 Store 一致)
	t.Status = StatusDispatched
	t.Attempt++
	t.UpdatedAt = time.Now().UnixMilli()
	return nil
}

func (s *MemoryStore) ReapToPending(taskID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[taskID]
	if !ok {
		return fmt.Errorf("任务 %s 不存在", taskID)
	}
	if isTerminal(t.Status) {
		return fmt.Errorf("任务 %s 终态 %s,拒绝回退", taskID, t.Status)
	}
	t.Status = StatusPending
	return nil
}

func (s *MemoryStore) Close() error { return nil }
