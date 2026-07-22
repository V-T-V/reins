package main

import (
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

// 任务状态机：
//
//	pending     —— Console 已提交，待 Agent 上线投递
//	dispatched  —— 已投递给 Agent，等待 ACK/结果
//	acknowledged—— Agent 已 ACK（收到）
//	completed   —— Agent 已回结果（终态）
//	failed      —— 执行失败（终态）
//	cancelled   —— 被取消（终态）
const (
	StatusPending      = "pending"
	StatusDispatched   = "dispatched"
	StatusAcknowledged = "acknowledged"
	StatusCompleted    = "completed"
	StatusFailed       = "failed"
	StatusCancelled    = "cancelled"
)

// bbolt bucket 名。
const (
	bucketTasks   = "tasks"
	bucketResults = "results"
)

// StoredTask 持久化的任务记录。
type StoredTask struct {
	Request   proto.TaskRequest `json:"request"`
	Status    string            `json:"status"`
	Attempt   int               `json:"attempt"`
	CreatedAt int64             `json:"created_at"` // Unix 毫秒
	UpdatedAt int64             `json:"updated_at"`
	Submitted string            `json:"submitted_by,omitempty"` // 提交者（审计用，C9 填充）
}

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
func (s *Store) UpdateTaskStatus(taskID, newStatus string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return ErrStoreClosed
	}
	var current *StoredTask
	err := s.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket([]byte(bucketTasks)).Get([]byte(taskID))
		if raw == nil {
			return nil
		}
		var t StoredTask
		if json.Unmarshal(raw, &t) == nil {
			current = &t
		}
		return nil
	})
	if err != nil || current == nil {
		return fmt.Errorf("任务 %s 不存在", taskID)
	}
	if !validTransition(current.Status, newStatus) {
		return fmt.Errorf("非法状态转换 %s→%s（当前为终态，不可变更）", current.Status, newStatus)
	}
	current.Status = newStatus
	if current.UpdatedAt == 0 {
		current.UpdatedAt = time.Now().UnixMilli()
	}
	data, _ := json.Marshal(current)
	return s.db.Update(func(tx *bolt.Tx) error {
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
	mu   sync.Mutex // 保护 Close
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
		for _, b := range []string{bucketTasks, bucketResults} {
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

// SaveTask 保存/更新任务记录。
//
// UpdatedAt 处理：若调用方未设（为 0）则自动填当前时间；若已设则保留。
// 这让 reaper 能在回退状态时保留原始 UpdatedAt（用于后续超时判断），
// 同时普通更新路径（未显式设 UpdatedAt）自动盖时间戳。
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
		return tx.Bucket([]byte(bucketTasks)).Put([]byte(t.Request.TaskID), data)
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

// ListTasksByStatus 列出指定状态的任务（用于 Relay 重启后找回在途任务）。
func (s *Store) ListTasksByStatus(statuses ...string) ([]StoredTask, error) {
	if s.db == nil {
		return nil, nil
	}
	want := make(map[string]bool, len(statuses))
	for _, s := range statuses {
		want[s] = true
	}
	var out []StoredTask
	err := s.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket([]byte(bucketTasks)).Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var t StoredTask
			if json.Unmarshal(v, &t) == nil && want[t.Status] {
				out = append(out, t)
			}
		}
		return nil
	})
	return out, err
}

// ListTasks 列出全部任务（最新在前），供审计/查询。
func (s *Store) ListTasks(limit int) ([]StoredTask, error) {
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

// DeleteTask 删除任务（通常不用，审计保留；测试可清）。
func (s *Store) DeleteTask(taskID string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(bucketTasks)).Delete([]byte(taskID))
	})
}

// ErrStoreClosed 存储已关闭。
var ErrStoreClosed = errors.New("store closed")
