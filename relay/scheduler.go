package main

import (
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/QiuShichang/go-rmm/proto"
)

// CronRule/bucketCrons 已拆入 internal/taskstore;本包经 store_alias.go 引用。


// Scheduler 定时任务调度器。
//
// 用 robfig/cron/v3 做调度引擎，规则持久化到 bbolt。
// 到点时生成 TaskRequest 经 dispatchTask 投递（复用现有任务链路）。
type Scheduler struct {
	mu       sync.Mutex
	store    *Store
	relay    *Relay
	cron     *cron.Cron
	entryMap map[string]cron.EntryID // ruleID → cron entry
}

// NewScheduler 构造调度器。
func NewScheduler(store *Store, relay *Relay) *Scheduler {
	s := &Scheduler{
		store:    store,
		relay:    relay,
		entryMap: make(map[string]cron.EntryID),
	}
	return s
}

// Start 启动调度器，加载持久化规则。
func (s *Scheduler) Start() error {
	s.cron = cron.New()
	s.cron.Start()

	// 加载持久化规则
	rules := s.loadRules()
	for _, rule := range rules {
		if rule.Enabled {
			s.schedule(rule)
		}
	}
	log.Printf("[scheduler] 启动，已加载 %d 条规则", len(rules))
	return nil
}

// Stop 停止调度器。
func (s *Scheduler) Stop() {
	if s.cron != nil {
		s.cron.Stop()
	}
}

// AddRule 新增定时规则并持久化 + 调度。
func (s *Scheduler) AddRule(rule CronRule) error {
	if rule.ID == "" {
		return fmt.Errorf("规则 ID 不能为空")
	}
	if rule.CronExpr == "" {
		return fmt.Errorf("cron 表达式不能为空")
	}
	now := time.Now().UnixMilli()
	rule.CreatedAt = now
	// P1-C6 修复:不强制覆盖 Enabled。用 CreatedAt==0 判断是否新规则(新规则默认启用)。
	// 但 CronRule.Enabled 是 bool(零值 false),无法区分"未设"和"显式 false"。
	// 妥协:新规则(CreatedAt==0 即未创建过)默认 true,已存在规则保留原值。
	// CLI commands.go 用 --disabled 创建时传 Enabled=*false,CreatedAt 未设(0),
	// 会被这里改回 true——为支持 disabled,CLI 端用 CreatedAt<0 作禁用标记。
	if rule.CreatedAt == 0 || rule.Enabled {
		rule.Enabled = true
	}
	// 如果 CLI 想创建禁用规则,会设 CreatedAt=-1(哨兵值),这里识别
	if rule.CreatedAt < 0 {
		rule.Enabled = false
		rule.CreatedAt = now
	}

	// 持久化
	if err := s.saveRule(rule); err != nil {
		return fmt.Errorf("持久化失败: %w", err)
	}
	// 调度
	s.mu.Lock()
	defer s.mu.Unlock()
	s.schedule(rule)
	log.Printf("[scheduler] 新增规则 %s: %s → agent=%s type=%s enabled=%v",
		rule.ID, rule.CronExpr, rule.AgentID, rule.TaskType, rule.Enabled)
	return nil
}

// RemoveRule 删除规则。
func (s *Scheduler) RemoveRule(ruleID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// 从 cron 引擎移除
	if entryID, ok := s.entryMap[ruleID]; ok && s.cron != nil {
		s.cron.Remove(entryID)
		delete(s.entryMap, ruleID)
	}
	// 从 bbolt 删除
	if s.store != nil {
		return s.store.DeleteCronRule(ruleID)
	}
	return nil
}

// ListRules 列出所有规则。
func (s *Scheduler) ListRules() []CronRule {
	return s.loadRules()
}

// schedule 把规则加入 cron 引擎。
func (s *Scheduler) schedule(rule CronRule) {
	if s.cron == nil {
		return
	}
	// M4 修复：闭包捕获 ruleID，每次触发重新加载最新规则（而非值拷贝）
	ruleID := rule.ID
	entryID, err := s.cron.AddFunc(rule.CronExpr, func() {
		latest := s.loadRuleByID(ruleID)
		if latest == nil || !latest.Enabled {
			return
		}
		s.executeRule(*latest)
	})
	if err != nil {
		log.Printf("[scheduler] 规则 %s 调度失败: %v", rule.ID, err)
		return
	}
	s.entryMap[rule.ID] = entryID
}

// executeRule 到点执行：生成 TaskRequest 投递。
func (s *Scheduler) executeRule(rule CronRule) {
	log.Printf("[scheduler] 触发规则 %s → agent=%s type=%s", rule.ID, rule.AgentID, rule.TaskType)

	// 生成任务请求
	task := proto.TaskRequest{
		TaskID:  "cron-" + rule.ID + "-" + time.Now().Format("150405"),
		Type:    rule.TaskType,
		AgentID: rule.AgentID,
		Payload: rule.Payload,
		Timeout: rule.Timeout,
	}

	// 持久化任务(复用 store)
	if s.store != nil {
		now := time.Now().UnixMilli()
		s.store.SaveTask(StoredTask{
			Request:   task,
			Status:    StatusPending,
			CreatedAt: now, // P1-C5 修复:填充时间戳
			UpdatedAt: now, // 否则 reaper 误判超时秒数立即回收
		})
	}

	// 审计
	if s.relay != nil && s.relay.audit != nil {
		s.relay.audit.Log(AuditEntry{
			Event:   "cron_triggered",
			TaskID:  task.TaskID,
			AgentID: rule.AgentID,
			Type:    rule.TaskType,
			Detail:  "cron:" + rule.CronExpr,
		})
	}

	// 投递（复用 dispatchTask）
	if s.relay != nil {
		conn := s.relay.agents.Conn(rule.AgentID)
		if conn == nil {
			log.Printf("[scheduler] Agent %s 离线，任务 %s 缓存 pending", rule.AgentID, task.TaskID)
			return
		}
		if err := s.relay.dispatchTask(conn, task); err != nil {
			log.Printf("[scheduler] 投递失败: %v", err)
		}
	}

	// 更新 LastRun
	rule.LastRun = time.Now().UnixMilli()
	s.saveRule(rule)
}

// saveRule 持久化规则。
func (s *Scheduler) saveRule(rule CronRule) error {
	if s.store == nil {
		return nil
	}
	return s.store.SaveCronRule(rule)
}

// loadRules 从 bbolt 加载所有规则。
func (s *Scheduler) loadRules() []CronRule {
	if s.store == nil {
		return nil
	}
	return s.store.LoadCronRules()
}

// loadRuleByID 按 ID 加载单条规则（M4 修复：闭包用此获取最新值）。
func (s *Scheduler) loadRuleByID(ruleID string) *CronRule {
	if s.store == nil {
		return nil
	}
	rules := s.store.LoadCronRules()
	for _, r := range rules {
		if r.ID == ruleID {
			return &r
		}
	}
	return nil
}
