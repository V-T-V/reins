package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/QiuShichang/go-rmm/proto"
)

// captureStdout 捕获执行 fn 期间的 stdout 输出。
func captureStdout(fn func()) string {
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	fn()
	w.Close()
	os.Stdout = old
	var buf bytes.Buffer
	io.Copy(&buf, r)
	return buf.String()
}

// withJSONMode 临时切换 jsonMode,测试后恢复。
func withJSONMode(t *testing.T, on bool) {
	t.Helper()
	orig := jsonMode
	jsonMode = on
	t.Cleanup(func() { jsonMode = orig })
}

// TestPrintResultJSONMode 验证 task run/task result 共用的 printResult 在 JSON 模式输出结构化结果。
func TestPrintResultJSONMode(t *testing.T) {
	withJSONMode(t, true)
	r := &proto.TaskResult{
		TaskID: "cli-powershell-123", Success: true, ExitCode: 0,
		Duration: 42, Stdout: "hello world", Stderr: "", Error: "",
	}
	out := captureStdout(func() { printResult(r) })

	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("JSON 输出解析失败: %v\n输出: %s", err, out)
	}
	if parsed["command"] != "task_result" {
		t.Errorf("command = %v, want task_result", parsed["command"])
	}
	if parsed["ok"] != true || parsed["success"] != true {
		t.Errorf("ok/success = %v/%v, want true/true", parsed["ok"], parsed["success"])
	}
	if parsed["task_id"] != "cli-powershell-123" {
		t.Errorf("task_id = %v", parsed["task_id"])
	}
	if parsed["exit_code"] != float64(0) || parsed["duration_ms"] != float64(42) {
		t.Errorf("exit_code/duration = %v/%v", parsed["exit_code"], parsed["duration_ms"])
	}
	if parsed["stdout"] != "hello world" {
		t.Errorf("stdout = %v, want hello world", parsed["stdout"])
	}
}

// TestPrintResultJSONNil 验证空结果时 JSON 输出含错误字段(不 panic)。
func TestPrintResultJSONNil(t *testing.T) {
	withJSONMode(t, true)
	out := captureStdout(func() { printResult(nil) })
	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("JSON 输出解析失败: %v\n输出: %s", err, out)
	}
	if parsed["ok"] != false {
		t.Errorf("ok = %v, want false", parsed["ok"])
	}
	if parsed["error"] != "结果为空" {
		t.Errorf("error = %v, want 结果为空", parsed["error"])
	}
}

// TestPrintResultTextMode 验证文本模式保持原格式(含 CLIXML 过滤)。
func TestPrintResultTextMode(t *testing.T) {
	withJSONMode(t, false)
	r := &proto.TaskResult{
		TaskID: "t1", Success: false, ExitCode: 1,
		Stdout: "#< CLIXML\n<Objs>\n</Objs>\nreal-out", Stderr: "",
	}
	out := captureStdout(func() { printResult(r) })
	if strings.Contains(out, "CLIXML") {
		t.Errorf("文本模式应过滤 CLIXML 噪音, got: %s", out)
	}
	if !strings.Contains(out, "real-out") {
		t.Errorf("文本模式应保留有效 stdout, got: %s", out)
	}
	if !strings.Contains(out, "成功:      false") {
		t.Errorf("文本模式应含成功字段, got: %s", out)
	}
	if strings.Contains(out, `"command"`) {
		t.Errorf("文本模式不应输出 JSON, got: %s", out)
	}
}

// TestProgressSuppressedInJSONMode 验证进度条在 JSON 模式静默(不污染 stdout)。
func TestProgressSuppressedInJSONMode(t *testing.T) {
	withJSONMode(t, true)
	pr := &progressReader{r: strings.NewReader(strings.Repeat("x", 100)), total: 100, label: "上传"}
	buf := make([]byte, 50)
	pr.Read(buf)
	out := captureStdout(func() { pr.maybePrint() })
	if out != "" {
		t.Errorf("JSON 模式进度条应静默, got: %q", out)
	}
}
