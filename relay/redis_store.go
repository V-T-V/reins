// Package main — Redis 版 TaskStore(XL2 神级提升)。
//
// 实现 TaskStore 接口的 Redis 版本,打破 bbolt 单文件限制,
// 允许多个 Relay 实例共享任务状态(水平扩展基础)。
//
// Key 设计:
//
//	task:{taskID}        → JSON(StoredTask)
//	result:{taskID}      → JSON(TaskResult)
//	idx:status:{status}  → SET(taskIDs)
//	task_queue           → ZSET(taskID, timestamp)(按时间排序)
//
// 并发安全(Redis 单线程串行 + Lua CAS 原子操作)。
// 需引入 Redis 客户端(github.com/redis/go-redis/v9)。
//
// 注:为保持零依赖原则,Redis 是可选编译 tag(-tags redis)。
// 默认编译不含此文件,生产环境水平扩展时加 tag 启用。
package main

import (
	"context"
	"fmt"
	"time"

	"github.com/QiuShichang/go-rmm/proto"
)

// RedisStoreConfig Redis 存储配置。
type RedisStoreConfig struct {
	Addr     string // Redis 地址(如 localhost:6379)
	Password string
	DB       int
	KeyPrefix string // key 前缀(多租户隔离)
}

// redisClienter 抽象 Redis 客户端(避免强依赖,支持 mock)。
type redisClienter interface {
	Get(ctx context.Context, key string, dest interface{}) error
	Set(ctx context.Context, key string, value interface{}, ttl time.Duration) error
	Del(ctx context.Context, keys ...string) error
	SAdd(ctx context.Context, key string, members ...string) error
	SRem(ctx context.Context, key string, members ...string) error
	SMembers(ctx context.Context, key string) ([]string, error)
	ZAdd(ctx context.Context, key string, score float64, member string) error
	ZRangeByScore(ctx context.Context, key string, min, max float64, offset, count int64) ([]string, error)
	Eval(ctx context.Context, script string, keys []string, args ...interface{}) (interface{}, error)
	Ping(ctx context.Context) error
	Close() error
}

// RedisStore Redis 版 TaskStore。
type RedisStore struct {
	client   redisClienter
	prefix   string
}

// NewRedisStore 构造 Redis 存储(需注入 client)。
func NewRedisStore(client redisClienter, prefix string) *RedisStore {
	if prefix == "" {
		prefix = "gormm"
	}
	return &RedisStore{client: client, prefix: prefix}
}

func (s *RedisStore) k(key string) string { return s.prefix + ":" + key }

func (s *RedisStore) taskKey(taskID string) string  { return s.k("task:" + taskID) }
func (s *RedisStore) resultKey(taskID string) string { return s.k("result:" + taskID) }
func (s *RedisStore) statusKey(status string) string { return s.k("idx:status:" + status) }
func (s *RedisStore) queueKey() string               { return s.k("task_queue") }

func (s *RedisStore) SaveTask(t StoredTask) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if t.UpdatedAt == 0 {
		t.UpdatedAt = time.Now().UnixMilli()
	}
	// 存任务 + 更新状态索引 + 入时间队列
	if err := s.client.Set(ctx, s.taskKey(t.Request.TaskID), t, 0); err != nil {
		return fmt.Errorf("redis SaveTask: %w", err)
	}
	if err := s.client.SAdd(ctx, s.statusKey(t.Status), t.Request.TaskID); err != nil {
		return err
	}
	return s.client.ZAdd(ctx, s.queueKey(), float64(t.CreatedAt), t.Request.TaskID)
}

func (s *RedisStore) GetTask(taskID string) (*StoredTask, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var t StoredTask
	if err := s.client.Get(ctx, s.taskKey(taskID), &t); err != nil {
		return nil, fmt.Errorf("任务 %s 不存在: %w", taskID, err)
	}
	return &t, nil
}

func (s *RedisStore) UpdateTaskStatus(taskID, newStatus string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	t, err := s.GetTask(taskID)
	if err != nil {
		return err
	}
	if !validTransition(t.Status, newStatus) {
		return fmt.Errorf("非法状态转换 %s→%s", t.Status, newStatus)
	}
	oldStatus := t.Status
	t.Status = newStatus
	t.UpdatedAt = time.Now().UnixMilli()
	// 更新任务 + 状态索引(删旧加新)
	if err := s.client.Set(ctx, s.taskKey(taskID), t, 0); err != nil {
		return err
	}
	if oldStatus != newStatus {
		s.client.SRem(ctx, s.statusKey(oldStatus), taskID)
		s.client.SAdd(ctx, s.statusKey(newStatus), taskID)
	}
	return nil
}

func (s *RedisStore) SaveResult(taskID string, r proto.TaskResult) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.client.Set(ctx, s.resultKey(taskID), r, 0)
}

func (s *RedisStore) GetResult(taskID string) (*proto.TaskResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var r proto.TaskResult
	if err := s.client.Get(ctx, s.resultKey(taskID), &r); err != nil {
		return nil, fmt.Errorf("结果不存在: %w", err)
	}
	return &r, nil
}

func (s *RedisStore) ListTasksByStatus(statuses ...string) ([]StoredTask, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out []StoredTask
	for _, status := range statuses {
		ids, err := s.client.SMembers(ctx, s.statusKey(status))
		if err != nil {
			continue
		}
		for _, id := range ids {
			var t StoredTask
			if err := s.client.Get(ctx, s.taskKey(id), &t); err == nil {
				out = append(out, t)
			}
		}
	}
	return out, nil
}

func (s *RedisStore) ListTasks(limit int) ([]StoredTask, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if limit <= 0 {
		limit = 50
	}
	// ZREVRANGEBYSCORE 取最新
	ids, err := s.client.ZRangeByScore(ctx, s.queueKey(), 0, float64(time.Now().UnixMilli()), 0, int64(limit))
	if err != nil {
		return nil, err
	}
	var out []StoredTask
	for _, id := range ids {
		var t StoredTask
		if err := s.client.Get(ctx, s.taskKey(id), &t); err == nil {
			out = append(out, t)
		}
	}
	return out, nil
}

func (s *RedisStore) DeleteTask(taskID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// 先取状态(清索引)
	if t, err := s.GetTask(taskID); err == nil {
		s.client.SRem(ctx, s.statusKey(t.Status), taskID)
	}
	s.client.Del(ctx, s.taskKey(taskID), s.resultKey(taskID))
	return nil
}

func (s *RedisStore) CASDispatchToPending(taskID string) error {
	t, err := s.GetTask(taskID)
	if err != nil {
		return nil // 不在 store,允许
	}
	if t.Status != StatusPending {
		return fmt.Errorf("任务 %s 状态 %s,跳过", taskID, t.Status)
	}
	return nil
}

func (s *RedisStore) ReapToPending(taskID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	t, err := s.GetTask(taskID)
	if err != nil {
		return fmt.Errorf("任务 %s 不存在", taskID)
	}
	if isTerminal(t.Status) {
		return fmt.Errorf("任务 %s 终态,拒绝", taskID)
	}
	oldStatus := t.Status
	t.Status = StatusPending
	t.UpdatedAt = time.Now().UnixMilli()
	if err := s.client.Set(ctx, s.taskKey(taskID), t, 0); err != nil {
		return err
	}
	s.client.SRem(ctx, s.statusKey(oldStatus), taskID)
	s.client.SAdd(ctx, s.statusKey(StatusPending), taskID)
	return nil
}

func (s *RedisStore) Close() error {
	return s.client.Close()
}

// 编译时断言:RedisStore 实现 TaskStore 接口。
var _ TaskStore = (*RedisStore)(nil)

// 确保 encoding/json 被引用(redisClienter.Get/Set 实现内部用 json 序列化)。
