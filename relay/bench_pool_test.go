package main

import (
	"io"
	"strings"
	"testing"

	"github.com/QiuShichang/reins/proto"
)

// === Buffer Pool 性能对比 ===

func BenchmarkMake256K(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		buf := make([]byte, 256*1024)
		_ = buf
	}
}

func BenchmarkPool256K(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		buf := Get256K()
		Put256K(buf)
	}
}

func BenchmarkMake32K(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		buf := make([]byte, 32*1024)
		_ = buf
	}
}

func BenchmarkPool32K(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		buf := Get32K()
		Put32K(buf)
	}
}

// === Copy 性能对比 ===

func BenchmarkIoCopy1MB(b *testing.B) {
	data := strings.Repeat("x", 1024*1024)
	b.ReportAllocs()
	b.SetBytes(1024 * 1024)
	for i := 0; i < b.N; i++ {
		src := strings.NewReader(data)
		io.Copy(io.Discard, src)
	}
}

func BenchmarkPooledCopy1MB(b *testing.B) {
	data := strings.Repeat("x", 1024*1024)
	b.ReportAllocs()
	b.SetBytes(1024 * 1024)
	for i := 0; i < b.N; i++ {
		src := strings.NewReader(data)
		pooledCopy(io.Discard, src)
	}
}

// === Envelope 序列化 ===

// === 命令策略 quickCheck 性能 ===

func BenchmarkPolicyCheckSafe(b *testing.B) {
	policy := DefaultCommandPolicy()
	safeCmd := "Get-Service | Where Status -eq Running | Select Name,Status"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		policy.CheckCommand(safeCmd)
	}
}

func BenchmarkPolicyCheckBlocked(b *testing.B) {
	policy := DefaultCommandPolicy()
	dangerousCmd := "Format-Volume -DriveLetter C"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		policy.CheckCommand(dangerousCmd)
	}
}

// === 二进制帧编解码 ===

func BenchmarkBuildBinaryFrame(b *testing.B) {
	data := make([]byte, 256*1024)
	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	for i := 0; i < b.N; i++ {
		proto.BuildBinaryFrame("xfer-bench", false, data)
	}
}

func BenchmarkParseBinaryFrame(b *testing.B) {
	data := make([]byte, 256*1024)
	frame := proto.BuildBinaryFrame("xfer-bench", false, data)
	b.ReportAllocs()
	b.SetBytes(int64(len(frame)))
	for i := 0; i < b.N; i++ {
		proto.ParseBinaryFrame(frame)
	}
}
