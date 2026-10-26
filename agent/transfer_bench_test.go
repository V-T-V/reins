package main

// v4.7.5 传输基准:为帧尺寸/缓冲调优提供实测依据(不拍脑袋)。
//   BenchmarkFrameBuildParse     帧构造+解析纯开销(按载荷尺寸分档)
//   BenchmarkStreamDownloadCore  下载流核心(fake 写出,隔离磁盘,测 hash/限速/进度聚合开销)
//   BenchmarkReceiveUploadCore   上传落盘核心(真实临时文件)

import (
	"context"
	"crypto/sha256"

	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/V-T-V/reins/proto"
)

type benchSize struct {
	name string
	n    int
}

func benchSizes() []benchSize {
	return []benchSize{
		{"4K", 4 << 10},
		{"64K", 64 << 10},
		{"256K", 256 << 10},
		{"512K", 512 << 10},
		{"1M", 1 << 20},
	}
}

func BenchmarkFrameBuildParse(b *testing.B) {
	for _, sz := range benchSizes() {
		payload := make([]byte, sz.n)
		b.Run(sz.name, func(b *testing.B) {
			b.SetBytes(int64(sz.n))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				frame := proto.BuildBinaryFrame("bench-xfer-id-001", false, payload)
				if _, _, _, err := proto.ParseBinaryFrame(frame); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkSHA256Chunk(b *testing.B) {
	// hash 是流式路径的固定成本,独立计量(256K 档)
	payload := make([]byte, 256<<10)
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h := sha256.New()
		h.Write(payload)
		_ = h.Sum(nil)
	}
}

func BenchmarkStreamDownloadCore(b *testing.B) {
	// 下载流核心:读文件→限速→fake 写出→hash。写出用 io.Discard 等价物
	// (no-op func),隔离目标端差异,测量引擎自身每块开销。
	dir := b.TempDir()
	src := filepath.Join(dir, "bench.bin")
	const total = 8 << 20
	if err := os.WriteFile(src, make([]byte, total), 0644); err != nil {
		b.Fatal(err)
	}
	noopProgress := func(proto.FileTransferProgress) {}
	noopComplete := func(proto.FileTransferComplete) {}
	ctx := context.Background()
	b.SetBytes(total)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		te := &TransferEngine{sessions: map[string]*transferSession{}}
		// 手工建会话(绕过 HandleInit 的文件准备,专注流核心)
		f, _ := os.Open(src)
		te.sessions["bench"] = &transferSession{
			id: "bench", direction: "download", file: f, total: total, lastProg: time.Now(),
		}
		writeChunk := func([]byte) error { return nil }
		te.streamDownload(ctx, "bench", writeChunk, nil, noopProgress, noopComplete)
	}
}

func BenchmarkReceiveUploadCore(b *testing.B) {
	// 上传落盘核心:接收缓冲→写文件+hash(256K 块 × 32 块 = 8M/轮)
	dir := b.TempDir()
	target := filepath.Join(dir, "bench-part")
	buf := make([]byte, 256<<10)
	noopProgress := func(proto.FileTransferProgress) {}
	noopComplete := func(proto.FileTransferComplete) {}
	b.SetBytes(int64(len(buf)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
		if err != nil {
			b.Fatal(err)
		}
		te := &TransferEngine{sessions: map[string]*transferSession{}}
		te.sessions["bench"] = &transferSession{
			id: "bench", direction: "upload", file: f, total: int64(len(buf)),
			lastProg: time.Now(), hash: sha256.New(),
		}
		// isLast=false:纯测写盘+hash 每块成本,不触发 rename/清理副作用
		te.ReceiveUploadData("bench", buf, false, noopProgress, noopComplete)
		f.Close()
	}
}
