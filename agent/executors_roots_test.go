package main

import (
	"reflect"
	"testing"
)

// TestResolveAllowedRoots 允许根解析三态:显式配置 > 整盘兜底 > fail-closed。
// rules 门禁的发现范围与 file 任务/流式传输的路径边界都依赖此函数同源。
func TestResolveAllowedRoots(t *testing.T) {
	explicit := []string{`D:\apps`, `E:\data`}
	if got := resolveAllowedRoots(Config{AllowedRoots: explicit}); !reflect.DeepEqual(got, explicit) {
		t.Fatalf("显式配置应原样返回: %v", got)
	}
	// 未配置 + AllowAllRoots(默认) → 整盘兜底
	got := resolveAllowedRoots(Config{AllowAllRoots: true})
	if len(got) != 1 {
		t.Fatalf("整盘兜底应 1 个根: %v", got)
	}
	// 未配置 + 显式收紧 → fail-closed(nil)
	if got := resolveAllowedRoots(Config{AllowAllRoots: false}); got != nil {
		t.Fatalf("fail-closed 应返回 nil: %v", got)
	}
}
