package main

import "sync"

// 全局 buffer 池(Agent 侧文件传输/隧道/更新 大量分配,用 sync.Pool 复用减少 GC)。
// 复用 relay 侧同模式(relay 与 agent 是独立 main 包,不共享 pool)。
var (
	pool256K = &sync.Pool{New: func() any { b := make([]byte, 256*1024); return &b }}
	pool64K  = &sync.Pool{New: func() any { b := make([]byte, 64*1024); return &b }}
	pool32K  = &sync.Pool{New: func() any { b := make([]byte, 32*1024); return &b }}
	pool4K   = &sync.Pool{New: func() any { b := make([]byte, 4096); return &b }}
)

func Get256K() []byte  { bp := pool256K.Get().(*[]byte); return (*bp)[:cap(*bp)] }
func Put256K(b []byte) { full := b[:cap(b)]; pool256K.Put(&full) }

func Get64K() []byte  { bp := pool64K.Get().(*[]byte); return (*bp)[:cap(*bp)] }
func Put64K(b []byte) { full := b[:cap(b)]; pool64K.Put(&full) }

func Get32K() []byte  { bp := pool32K.Get().(*[]byte); return (*bp)[:cap(*bp)] }
func Put32K(b []byte) { full := b[:cap(b)]; pool32K.Put(&full) }

func Get4K() []byte  { bp := pool4K.Get().(*[]byte); return (*bp)[:cap(*bp)] }
func Put4K(b []byte) { full := b[:cap(b)]; pool4K.Put(&full) }
