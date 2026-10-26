// Package main — taskstore 拆分的包内兼容层。
//
// store/memstore/CronRule 已拆入 internal/taskstore(单一职责的存储域包)。
// 本文件以 alias/转发保持 relay 包内既有引用不变,后续可逐步迁移为
// 显式 taskstore. 前缀。
package main

import (
	"github.com/V-T-V/reins/internal/taskstore"
)

// 类型 alias。
type (
	TaskStore   = taskstore.TaskStore
	StoredTask  = taskstore.StoredTask
	Store       = taskstore.Store
	MemoryStore = taskstore.MemoryStore
	CronRule    = taskstore.CronRule
)

// 状态常量 alias(proto 状态机的存储侧引用)。
const (
	StatusPending      = taskstore.StatusPending
	StatusDispatched   = taskstore.StatusDispatched
	StatusAcknowledged = taskstore.StatusAcknowledged
	StatusCompleted    = taskstore.StatusCompleted
	StatusFailed       = taskstore.StatusFailed
	StatusCancelled    = taskstore.StatusCancelled
	MaxTaskAttempts    = taskstore.MaxTaskAttempts
)

// 错误/构造函数转发。
var ErrStoreClosed = taskstore.ErrStoreClosed

func OpenStore(path string) (*Store, error) { return taskstore.OpenStore(path) }
func NewMemoryStore() *MemoryStore          { return taskstore.NewMemoryStore() }
