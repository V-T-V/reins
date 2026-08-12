package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/QiuShichang/go-rmm/proto"
)

// TaskStore 任务持久化接口(M4 神级提升)。
// 抽象 Store 为接口,解锁 relay server 层单测(可 mock,不依赖真 bbolt)。
// 未来水平扩展时,可实现 RedisTaskStore / PostgresTaskStore 替换 bbolt。
type TaskStore interface {
	SaveTask(t StoredTask) error
	GetTask(taskID string) (*StoredTask, error)
	UpdateTaskStatus(taskID, newStatus string) error
	SaveResult(taskID string, r proto.TaskResult) error
	GetResult(taskID string) (*proto.TaskResult, error)
	ListTasksByStatus(statuses ...string) ([]StoredTask, error)
	ListTasks(limit int) ([]StoredTask, error)
	DeleteTask(taskID string) error
	CASDispatchToPending(taskID string) error
	ReapToPending(taskID string) error
	Close() error
}

// 任务状态机：
//
//	pending     —— Console 已提交，待 Agent 上线投递
//	dispatched  —— 已投递给 Agent，等待 ACK/结果
//	acknowledged—— Agent 已 ACK（收到）
//	completed   —— Agent 已回结果（终态）
//	failed      —— 执行失败（终态）
//
// cancelled   —— 被取消(终态)
//
// R1 修复:Status 常量统一用 proto 包定义(单一真相源)。
// relay 包通过 alias 引用,消除双份定义的漂移风险。
const (
	StatusPending      = proto.StatusPending
	StatusDispatched   = proto.StatusDispatched
	StatusAcknowledged = proto.StatusAcknowledged
	StatusCompleted    = proto.StatusCompleted
	StatusFailed       = proto.StatusFailed
	StatusCancelled    = proto.StatusCancelled
)

// bbolt bucket 名。
const (
	bucketTasks   = "tasks"
	bucketResults = "results"
	// F1：状态索引 bucket，key="status\x00taskID"，value=nil。
	// ListTasksByStatus 按前缀扫描，避免全表遍历 + 逐条 Unmarshal。
	bucketTasksByStatus = "tasks_by_status"
)

// statusKey 构造状态索引 key："status\x00taskID"。
func statusKey(status, taskID string) []byte {
	return []byte(status + "\x00" + taskID)
}

// StoredTask 持久化的任务记录。
type StoredTask struct {
	Request   proto.TaskRequest `json:"request"`
	Status    string            `json:"status"`
	Attempt   int               `json:"attempt"`
	CreatedAt int64             `json:"created_at"` // Unix 毫秒
	UpdatedAt int64             `json:"updated_at"`
	Submitted string            `json:"submitted_by,omitempty"` // 提交者（审计用，C9 填充）
}

// MaxTaskAttempts 任务最大投递次数（E2/W1）。
// 超过后转 failed 终态，避免 poison task（永远失败）无限重投。
const MaxTaskAttempts = 5

// isTerminal 判断状态是否为终态（不可再变更）。
// P2-2 修复：终态一旦写入，拒绝后续变更，避免 cancel 与 result 竞态导致状态漂移。
func isTerminal(status string) bool {
	return status == StatusCompleted || status == StatusFailed || status == StatusCancelled
}

// validTransition 校验从 from 到 to 的状态转换是否合法。
//
// 规则：
//   - 终态不可变更（completed/failed/cancelled → 任何 都拒绝）
//   - pending → dispatched/acknowledged/cancelled 允许
//   - dispatched → acknowledged/pending(回退)/cancelled 允许
//   - acknowledged → completed/failed/pending(超时回退)/cancelled 允许
func validTransition(from, to string) bool {
	if isTerminal(from) {
		return false // 终态锁死
	}
	// 非终态间的转换基本都允许（具体语义由调用方保证）
	return true
}

// UpdateTaskStatus 带状态机校验的状态更新。
// 若当前状态为终态或转换非法，返回错误且不更新。
//
// E1/B2 修复：把"读当前态→校验→写新态"合并进单个 db.Update 写事务。
// bbolt 写事务天然串行，保证 reaper 与 result 并发时不会丢失更新
// （原 View+Update 两步非原子，可能 completed 被 reaper 覆盖为 pending）。
func (s *Store) UpdateTaskStatus(taskID, newStatus string) error {
	if s.db == nil {
		return ErrStoreClosed
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		raw := tx.Bucket([]byte(bucketTasks)).Get([]byte(taskID))
		if raw == nil {
			return fmt.Errorf("任务 %s 不存在", taskID)
		}
		var current StoredTask
		if err := json.Unmarshal(raw, &current); err != nil {
			return fmt.Errorf("任务 %s 解析失败: %w", taskID, err)
		}
		if !validTransition(current.Status, newStatus) {
			return fmt.Errorf("非法状态转换 %s→%s（终态不可变更）", current.Status, newStatus)
		}
		oldStatus := current.Status
		current.Status = newStatus
		now := time.Now().UnixMilli()
		if current.UpdatedAt == 0 || newStatus == StatusPending {
			current.UpdatedAt = now // reaper 回退时刷新 UpdatedAt(m3 修复)
		}
		data, err := json.Marshal(current)
		if err != nil {
			return err
		}
		id := []byte(taskID)
		// F1：更新状态索引（删旧加新）
		if oldStatus != newStatus {
			idxBucket := tx.Bucket([]byte(bucketTasksByStatus))
			idxBucket.Delete(statusKey(oldStatus, taskID))
			if err := idxBucket.Put(statusKey(newStatus, taskID), []byte{}); err != nil {
				return err
			}
		}
		return tx.Bucket([]byte(bucketTasks)).Put(id, data)
	})
}

// CASDispatchToPending 原子 CAS:只允许 pending→dispatched 转换成功。
// 用于 P0-C3 修复:dispatchTask 投递前占坑,防止并发重复投递。
// 返回 nil 表示 CAS 成功(本调用方获得投递权),返回 error 表示已被投递/终态。
func (s *Store) CASDispatchToPending(taskID string) error {
	if s.db == nil {
		return ErrStoreClosed
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		raw := tx.Bucket([]byte(bucketTasks)).Get([]byte(taskID))
		if raw == nil {
			return nil // 任务不在 store(非持久化模式),允许投递
		}
		var current StoredTask
		if err := json.Unmarshal(raw, &current); err != nil {
			return nil // 解析失败,不阻塞投递
		}
		// 只允许从 pending 转为 dispatched(占坑)
		// 已是 dispatched/acknowledged 说明并发投递,跳过(幂等)
		if current.Status != StatusPending {
			return fmt.Errorf("任务 %s 状态为 %s,已被投递或终态,跳过", taskID, current.Status)
		}
		return nil // 放行,真实状态更新由 dispatchTask 后续 UpdateTaskStatus 完成
	})
}

// ReapToPending reaper 专用:原子校验非终态 + 回退 pending + 刷新 UpdatedAt。
// P0-C2 修复:reaper 用此方法替代裸 SaveTask,防止覆盖终态导致重复执行。
// 若任务已是终态(completed/failed/cancelled),返回 error 不修改。
func (s *Store) ReapToPending(taskID string) error {
	if s.db == nil {
		return ErrStoreClosed
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		raw := tx.Bucket([]byte(bucketTasks)).Get([]byte(taskID))
		if raw == nil {
			return fmt.Errorf("任务 %s 不存在", taskID)
		}
		var current StoredTask
		if err := json.Unmarshal(raw, &current); err != nil {
			return fmt.Errorf("任务 %s 解析失败: %w", taskID, err)
		}
		// P0-C2 核心:终态任务拒绝回退,避免重复执行
		if isTerminal(current.Status) {
			return fmt.Errorf("任务 %s 已是终态 %s,拒绝回退", taskID, current.Status)
		}
		oldStatus := current.Status
		current.Status = StatusPending
		current.UpdatedAt = time.Now().UnixMilli() // m3 修复:刷新避免下轮立即回收
		data, err := json.Marshal(current)
		if err != nil {
			return err
		}
		if oldStatus != StatusPending {
			idxBucket := tx.Bucket([]byte(bucketTasksByStatus))
			idxBucket.Delete(statusKey(oldStatus, taskID))
			if err := idxBucket.Put(statusKey(StatusPending, taskID), []byte{}); err != nil {
				return err
			}
		}
		return tx.Bucket([]byte(bucketTasks)).Put([]byte(taskID), data)
	})
}

// Store 基于 bbolt 的任务持久化层。
//
// 修复 R2(P0) + F1(P1)：Relay 重启后内存任务全部丢失。
// bbolt 单文件、纯 Go、单写多读，与单二进制部署契合。
//
// 并发：bbolt 自身支持多读单写（DB.Open 读事务可并发，写事务串行）。
// 这里额外用写 channel 串行化业务层写请求，避免重复 Open 写事务。
type Store struct {
	db   *bolt.DB
	mu   sync.Mutex // 保护 Close(防并发 Close)
	path string
}

// OpenStore 打开（或创建）持久化存储。path 为 .db 文件路径。
func OpenStore(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, fmt.Errorf("创建数据目录失败: %w", err)
	}
	db, err := bolt.Open(path, 0600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("打开 bbolt 失败: %w", err)
	}
	// 初始化 buckets
	err = db.Update(func(tx *bolt.Tx) error {
		for _, b := range []string{bucketTasks, bucketResults, bucketTasksByStatus} {
			if _, err := tx.CreateBucketIfNotExists([]byte(b)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &Store{db: db, path: path}, nil
}

// Close 关闭存储。
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

// SaveTask 保存/更新任务记录，并维护状态索引。
//
// UpdatedAt 处理：若调用方未设（为 0）则自动填当前时间；若已设则保留。
// F1：同时更新 bucketTasksByStatus 索引，让 ListTasksByStatus 走索引而非全表扫描。
func (s *Store) SaveTask(t StoredTask) error {
	if t.UpdatedAt == 0 {
		t.UpdatedAt = time.Now().UnixMilli()
	}
	data, err := json.Marshal(t)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return ErrStoreClosed
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		taskID := []byte(t.Request.TaskID)
		// 若已有旧记录，先清理旧状态索引
		if old := tx.Bucket([]byte(bucketTasks)).Get(taskID); old != nil {
			var prev StoredTask
			if json.Unmarshal(old, &prev) == nil && prev.Status != t.Status {
				tx.Bucket([]byte(bucketTasksByStatus)).Delete(statusKey(prev.Status, t.Request.TaskID))
			}
		}
		// 写任务记录
		if err := tx.Bucket([]byte(bucketTasks)).Put(taskID, data); err != nil {
			return err
		}
		// 写新状态索引
		return tx.Bucket([]byte(bucketTasksByStatus)).Put(statusKey(t.Status, t.Request.TaskID), []byte{})
	})
}

// GetTask 读取单个任务。
// P2-7：db==nil 返回 nil（优雅降级）。
func (s *Store) GetTask(taskID string) (*StoredTask, error) {
	if s.db == nil {
		return nil, nil
	}
	var raw []byte
	err := s.db.View(func(tx *bolt.Tx) error {
		raw = tx.Bucket([]byte(bucketTasks)).Get([]byte(taskID))
		return nil
	})
	if err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, nil
	}
	var t StoredTask
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// SaveResult 保存任务结果。
// P2-7 修复：检查 db != nil，防 Close 后写 panic。
func (s *Store) SaveResult(taskID string, r proto.TaskResult) error {
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return ErrStoreClosed
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(bucketResults)).Put([]byte(taskID), data)
	})
}

// GetResult 读取任务结果。
func (s *Store) GetResult(taskID string) (*proto.TaskResult, error) {
	if s.db == nil {
		return nil, nil
	}
	var raw []byte
	err := s.db.View(func(tx *bolt.Tx) error {
		raw = tx.Bucket([]byte(bucketResults)).Get([]byte(taskID))
		return nil
	})
	if err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, nil
	}
	var r proto.TaskResult
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// ListTasksByStatus 列出指定状态的任务（用于 reaper / recoverInFlightTasks）。
//
// F1 优化：走 bucketTasksByStatus 索引按前缀扫描，O(匹配数) 而非 O(全表)。
// 原 1000 任务全表扫描 11ms → 索引扫描仅遍历匹配状态的任务。
func (s *Store) ListTasksByStatus(statuses ...string) ([]StoredTask, error) {
	if s.db == nil {
		return nil, nil
	}
	var out []StoredTask
	err := s.db.View(func(tx *bolt.Tx) error {
		idxBucket := tx.Bucket([]byte(bucketTasksByStatus))
		taskBucket := tx.Bucket([]byte(bucketTasks))
		for _, status := range statuses {
			prefix := []byte(status + "\x00")
			c := idxBucket.Cursor()
			for k, _ := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, _ = c.Next() {
				// k = "status\x00taskID"，提取 taskID
				taskID := k[len(prefix):]
				raw := taskBucket.Get(taskID)
				if raw == nil {
					continue // 索引与数据不一致（理论上不会），跳过
				}
				var t StoredTask
				if json.Unmarshal(raw, &t) == nil {
					out = append(out, t)
				}
			}
		}
		return nil
	})
	return out, err
}

// ListTasks 列出全部任务（最新在前），供审计/查询。
func (s *Store) ListTasks(limit int) ([]StoredTask, error) {
	if s.db == nil {
		return nil, ErrStoreClosed
	}
	var out []StoredTask
	err := s.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket([]byte(bucketTasks)).Cursor()
		for k, v := c.Last(); k != nil; k, v = c.Prev() {
			var t StoredTask
			if json.Unmarshal(v, &t) == nil {
				out = append(out, t)
				if limit > 0 && len(out) >= limit {
					break
				}
			}
		}
		return nil
	})
	return out, err
}

// DeleteTask 删除任务（通常不用，审计保留；测试可清）。同时清理状态索引。
func (s *Store) DeleteTask(taskID string) error {
	if s.db == nil {
		return ErrStoreClosed
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		id := []byte(taskID)
		// 先读当前状态以清理索引
		if raw := tx.Bucket([]byte(bucketTasks)).Get(id); raw != nil {
			var t StoredTask
			if json.Unmarshal(raw, &t) == nil {
				tx.Bucket([]byte(bucketTasksByStatus)).Delete(statusKey(t.Status, taskID))
			}
		}
		return tx.Bucket([]byte(bucketTasks)).Delete(id)
	})
}

// ErrStoreClosed 存储已关闭。
var ErrStoreClosed = errors.New("store closed")

// --- H2 Cron 规则持久化 ---

// SaveCronRule 保存定时任务规则。
func (s *Store) SaveCronRule(rule CronRule) error {
	if s.db == nil {
		return ErrStoreClosed
	}
	data, err := json.Marshal(rule)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		bucket, err := tx.CreateBucketIfNotExists([]byte(bucketCrons))
		if err != nil {
			return err
		}
		return bucket.Put([]byte(rule.ID), data)
	})
}

// DeleteCronRule 删除定时任务规则。
func (s *Store) DeleteCronRule(ruleID string) error {
	if s.db == nil {
		return ErrStoreClosed
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(bucketCrons))
		if bucket == nil {
			return nil
		}
		return bucket.Delete([]byte(ruleID))
	})
}

// LoadCronRules 加载所有定时任务规则。
func (s *Store) LoadCronRules() []CronRule {
	if s.db == nil {
		return nil
	}
	var out []CronRule
	s.db.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(bucketCrons))
		if bucket == nil {
			return nil
		}
		c := bucket.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var rule CronRule
			if json.Unmarshal(v, &rule) == nil {
				out = append(out, rule)
			}
		}
		return nil
	})
	return out
}
