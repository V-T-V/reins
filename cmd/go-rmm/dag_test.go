package main

import (
	"os"
	"strings"
	"testing"
)

// TestCmdDAGArgParsing dag 子命令参数解析:未知子命令/缺必填参数逐一报错。
func TestCmdDAGArgParsing(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"无子命令", nil, "用法"},
		{"未知子命令", []string{"bogus"}, "未知子命令"},
		{"apply 缺文件", []string{"apply"}, "必填"},
		{"status 缺 ID", []string{"status"}, "用法"},
	}
	for _, tc := range cases {
		err := cmdDAG(config{}, tc.args)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: 应报错含 %q,实际 %v", tc.name, tc.want, err)
		}
	}
}

// TestDAGApplyBadFile apply 对不存在/非法 JSON 文件的报错。
func TestDAGApplyBadFile(t *testing.T) {
	if err := dagApply(config{}, []string{"-f", "no-such-file.json"}); err == nil || !strings.Contains(err.Error(), "读取") {
		t.Fatalf("缺文件应报读取错误,实际 %v", err)
	}
	f := t.TempDir() + "/bad.json"
	_ = os.WriteFile(f, []byte(`{"nodes":`), 0o644)
	if err := dagApply(config{}, []string{"-f", f}); err == nil || !strings.Contains(err.Error(), "解析") {
		t.Fatalf("坏 JSON 应报解析错误,实际 %v", err)
	}
	_ = os.WriteFile(f, []byte(`{"name":"x"}`), 0o644)
	if err := dagApply(config{}, []string{"-f", f}); err == nil || !strings.Contains(err.Error(), "nodes") {
		t.Fatalf("缺 nodes 字段应报错,实际 %v", err)
	}
}

// TestPrintDAGStatus 人类可读输出:逐节点行含 ID/状态/错误,汇总行含 1/1/1(复用 json_output_test 的 captureStdout)。
func TestPrintDAGStatus(t *testing.T) {
	snap := map[string]any{
		"status":      "finished",
		"name":        "upgrade-flow",
		"dag_id":      "dag-1",
		"succeeded":   float64(1),
		"failed":      float64(1),
		"skipped":     float64(1),
		"start_order": []any{"prep", "deploy"},
		"nodes": []any{
			map[string]any{"id": "prep", "status": "succeeded"},
			map[string]any{"id": "deploy", "status": "failed", "err": "投递失败: [a-1: agent 离线]", "targets": []any{"a-1"}},
			map[string]any{"id": "verify", "status": "skipped"},
		},
	}
	out := captureStdout(t, func() { printDAGStatus(snap) })
	for _, want := range []string{"upgrade-flow", "prep", "succeeded", "deploy", "failed", "agent 离线", "verify", "skipped", "[prep deploy]"} {
		if !strings.Contains(out, want) {
			t.Fatalf("输出应含 %q,实际:\n%s", want, out)
		}
	}
}
