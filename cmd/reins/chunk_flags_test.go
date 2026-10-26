package main

import (
	"strings"
	"testing"
)

// TestApplyChunkFlags 验证 Q3 抽出的分片 flag 解析(三 flag + 上限 + 剥离)。
func TestApplyChunkFlags(t *testing.T) {
	// 保存/恢复全局默认值(测试间隔离)
	origCS, origConc, origRetry := chunkSizeArg, concurrencyArg, chunkMaxRetryArg
	defer func() {
		chunkSizeArg, concurrencyArg, chunkMaxRetryArg = origCS, origConc, origRetry
	}()

	t.Run("三 flag 空格形式", func(t *testing.T) {
		chunkSizeArg, concurrencyArg, chunkMaxRetryArg = origCS, origConc, origRetry
		args := []string{"--concurrency", "8", "--chunk-size", "20M", "--chunk-retry", "5", "file.bin", "remote.bin"}
		out, err := applyChunkFlags(args)
		if err != nil {
			t.Fatalf("不应报错: %v", err)
		}
		if concurrencyArg != 8 {
			t.Errorf("concurrency = %d, want 8", concurrencyArg)
		}
		if chunkSizeArg != 20*1024*1024 {
			t.Errorf("chunkSize = %d, want %d", chunkSizeArg, 20*1024*1024)
		}
		if chunkMaxRetryArg != 5 {
			t.Errorf("retry = %d, want 5", chunkMaxRetryArg)
		}
		// flag 应被剥离,只留位置参数
		if len(out) != 2 || out[0] != "file.bin" || out[1] != "remote.bin" {
			t.Errorf("剥离后 args = %v, want [file.bin remote.bin]", out)
		}
	})

	t.Run("等号形式", func(t *testing.T) {
		chunkSizeArg, concurrencyArg, chunkMaxRetryArg = origCS, origConc, origRetry
		args := []string{"--concurrency=2", "--chunk-size=5M", "a", "b"}
		out, err := applyChunkFlags(args)
		if err != nil {
			t.Fatalf("不应报错: %v", err)
		}
		if concurrencyArg != 2 {
			t.Errorf("concurrency = %d, want 2", concurrencyArg)
		}
		if chunkSizeArg != 5*1024*1024 {
			t.Errorf("chunkSize = %d, want %d", chunkSizeArg, 5*1024*1024)
		}
		if len(out) != 2 {
			t.Errorf("剥离后应剩 2 个位置参数, got %v", out)
		}
	})

	t.Run("chunk-size 超上限拒绝", func(t *testing.T) {
		chunkSizeArg = origCS
		args := []string{"--chunk-size", "256M", "a", "b"}
		_, err := applyChunkFlags(args)
		if err == nil {
			t.Error("256M 超过 128M 上限应报错")
		}
		if !strings.Contains(err.Error(), "128M") {
			t.Errorf("错误信息应提到 128M 上限, got: %v", err)
		}
	})

	t.Run("无 flag 保持默认", func(t *testing.T) {
		chunkSizeArg, concurrencyArg, chunkMaxRetryArg = origCS, origConc, origRetry
		args := []string{"file.bin", "remote.bin"}
		out, err := applyChunkFlags(args)
		if err != nil {
			t.Fatalf("不应报错: %v", err)
		}
		if concurrencyArg != 4 { // 默认值
			t.Errorf("concurrency = %d, want 默认 4", concurrencyArg)
		}
		if len(out) != 2 {
			t.Errorf("无 flag 时 args 不应变, got %v", out)
		}
	})

	t.Run("无效值忽略(不报错)", func(t *testing.T) {
		chunkSizeArg, concurrencyArg = origCS, origConc
		args := []string{"--concurrency", "abc", "--chunk-size", "-5", "a", "b"}
		_, err := applyChunkFlags(args)
		if err != nil {
			t.Errorf("无效值应静默忽略而非报错: %v", err)
		}
		if concurrencyArg != 4 { // 保持默认
			t.Errorf("无效 concurrency 应保持默认, got %d", concurrencyArg)
		}
	})
}

// TestStripValueFlags 验证 --flag value 和 --flag=value 的剥离。
func TestStripValueFlags(t *testing.T) {
	tests := []struct {
		name  string
		args  []string
		flags []string
		want  []string
	}{
		{"空格形式", []string{"--x", "v", "keep"}, []string{"--x"}, []string{"keep"}},
		{"等号形式", []string{"--x=v", "keep"}, []string{"--x"}, []string{"keep"}},
		{"多 flag", []string{"--a", "1", "--b", "2", "k1", "k2"}, []string{"--a", "--b"}, []string{"k1", "k2"}},
		{"无匹配 flag", []string{"--other", "v", "keep"}, []string{"--x"}, []string{"--other", "v", "keep"}},
		{"混合", []string{"--a=1", "--b", "2", "k"}, []string{"--a", "--b"}, []string{"k"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripValueFlags(tt.args, tt.flags...)
			if !sliceEq(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

// TestFlagVal 验证 flag 值提取。
func TestFlagVal(t *testing.T) {
	if v := flagVal([]string{"--x", "val"}, "--x"); v != "val" {
		t.Errorf("空格形式: got %q want val", v)
	}
	if v := flagVal([]string{"--x=val"}, "--x"); v != "val" {
		t.Errorf("等号形式: got %q want val", v)
	}
	if v := flagVal([]string{"--y", "v"}, "--x"); v != "" {
		t.Errorf("不存在: got %q want empty", v)
	}
}

// TestParseSize 验证带单位大小解析。
func TestParseSize(t *testing.T) {
	tests := []struct {
		input string
		want  int64
	}{
		{"1024", 1024},
		{"1k", 1024},
		{"1K", 1024},
		{"1m", 1024 * 1024},
		{"10M", 10 * 1024 * 1024},
		{"1g", 1024 * 1024 * 1024},
		{"2G", 2 * 1024 * 1024 * 1024},
	}
	for _, tt := range tests {
		got, err := parseSize(tt.input)
		if err != nil {
			t.Errorf("parseSize(%q) 报错: %v", tt.input, err)
			continue
		}
		if got != tt.want {
			t.Errorf("parseSize(%q) = %d, want %d", tt.input, got, tt.want)
		}
	}
	// 无效应报错
	if _, err := parseSize("abc"); err == nil {
		t.Error("parseSize(abc) 应报错")
	}
	if _, err := parseSize(""); err == nil {
		t.Error("parseSize(空) 应报错")
	}
}

// sliceEq 简单字符串切片比较。
func sliceEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
