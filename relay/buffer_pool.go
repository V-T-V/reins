// Package main — 全局 buffer 池(O2:减少 GC 压力)。
//
// 文件传输/隧道/shell 大量分配 256KB/32KB/4KB buffer。
// 用 sync.Pool 复用,减少 heap 分配 + GC 停顿。
package main

import (
	"io"
	"sync"
)

// 不同尺寸的 buffer 池(按使用场景分)。
var (
	// 256KB:文件传输 stream-through
	pool256K = &sync.Pool{
		New: func() interface{} { b := make([]byte, 256*1024); return &b },
	}
	// 32KB:隧道/下载
	pool32K = &sync.Pool{
		New: func() interface{} { b := make([]byte, 32*1024); return &b },
	}
	// 4KB:shell stdout/小消息
	pool4K = &sync.Pool{
		New: func() interface{} { b := make([]byte, 4096); return &b },
	}
)

// Get256K 获取一个 256KB buffer(用完必须 Put256K)。
// 返回长度=cap 的 slice(io.CopyBuffer 要求非空 buffer)。
func Get256K() []byte  { bp := pool256K.Get().(*[]byte); return (*bp)[:cap(*bp)] }
func Put256K(b []byte) { full := b[:cap(b)]; pool256K.Put(&full) }

// Get32K / Put32K:32KB buffer。
func Get32K() []byte  { bp := pool32K.Get().(*[]byte); return (*bp)[:cap(*bp)] }
func Put32K(b []byte) { full := b[:cap(b)]; pool32K.Put(&full) }

// Get4K / Put4K:4KB buffer。
func Get4K() []byte  { bp := pool4K.Get().(*[]byte); return (*bp)[:cap(*bp)] }
func Put4K(b []byte) { full := b[:cap(b)]; pool4K.Put(&full) }

// pooledCopy 用池化 buffer 复制(reader → writer),减少 GC。
func pooledCopy(dst io.Writer, src io.Reader) (int64, error) {
	buf := Get32K()
	defer Put32K(buf)
	return io.CopyBuffer(dst, src, buf)
}

// 确保避免 io.CopyBuffer 导入问题(它需要 []byte 不是 *[]byte)。
// 直接用 Get32K 返回的 slice(容量 32K,长度 0 → CopyBuffer 会用到 cap)。
