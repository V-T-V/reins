package executors

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/QiuShichang/go-rmm/proto"
)

// TestPowerShellBasic 验证 PowerShell 执行器能跑真实命令。
func TestPowerShellBasic(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("仅 Windows")
	}
	e := NewPowerShell()
	task := proto.PowerShellTask{Command: `Write-Output "hello-你好"`}
	payload, _ := jsonMarshal(task)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	r := e.Exec(ctx, payload, nil)
	if !r.Success {
		t.Fatalf("期望成功，失败: %s (stderr=%s)", r.Error, r.Stderr)
	}
	if !strings.Contains(r.Stdout, "hello-你好") {
		t.Fatalf("输出应含中文 hello-你好，得到: %q", r.Stdout)
	}
}

// TestPowerShellExitCode 验证非零退出码被捕获。
func TestPowerShellExitCode(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("仅 Windows")
	}
	e := NewPowerShell()
	task := proto.PowerShellTask{Command: `exit 42`}
	payload, _ := jsonMarshal(task)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	r := e.Exec(ctx, payload, nil)
	if r.Success {
		t.Fatal("期望失败（exit 42）")
	}
	if r.ExitCode != 42 {
		t.Fatalf("期望 exit code 42，得到 %d", r.ExitCode)
	}
}

// TestPowerShellTimeout 验证超时被正确处理。
func TestPowerShellTimeout(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("仅 Windows")
	}
	e := NewPowerShell()
	task := proto.PowerShellTask{Command: `Start-Sleep -Seconds 10`}
	payload, _ := jsonMarshal(task)

	// 1 秒超时，命令需 10 秒
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	r := e.Exec(ctx, payload, nil)
	if r.Success {
		t.Fatal("期望超时失败")
	}
	// exec.CommandContext 超时 kill 后报错形式可能多样(超时/killed/取消),这里宽松校验,不断言错误文本
	_ = r.Error
}

// TestScriptPS1 验证 script 执行器（含 BOM 处理）。
func TestScriptPS1(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("仅 Windows")
	}
	e := NewScript()
	task := proto.ScriptTask{
		Lang:    "ps1",
		Content: `Write-Output "脚本-中文测试"`,
	}
	payload, _ := jsonMarshal(task)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	r := e.Exec(ctx, payload, nil)
	if !r.Success {
		t.Fatalf("期望成功，失败: %s (stderr=%s)", r.Error, r.Stderr)
	}
	if !strings.Contains(r.Stdout, "脚本-中文测试") {
		t.Fatalf("输出应含中文，得到: %q", r.Stdout)
	}
}

// TestFileUploadDownload 验证文件上传下载往返（含路径白名单）。
func TestFileUploadDownload(t *testing.T) {
	tmpDir := t.TempDir()
	e := NewFile(WithAllowedRoots(tmpDir))

	// upload
	content := "hello-文件内容"
	uploadPayload, _ := jsonMarshal(proto.FileTask{
		Op:         "upload",
		Path:       tmpDir + "/test.txt",
		ContentB64: b64enc(content),
	})
	r := e.Exec(context.Background(), uploadPayload, nil)
	if !r.Success {
		t.Fatalf("upload 失败: %s", r.Error)
	}

	// download
	dlPayload, _ := jsonMarshal(proto.FileTask{
		Op:   "download",
		Path: tmpDir + "/test.txt",
	})
	r = e.Exec(context.Background(), dlPayload, nil)
	if !r.Success {
		t.Fatalf("download 失败: %s", r.Error)
	}
	var data struct {
		ContentB64 string `json:"content_b64"`
	}
	jsonUnmarshal(r.Data, &data)
	if b64dec(data.ContentB64) != content {
		t.Fatalf("往返内容不匹配: 期望 %q", content)
	}
}

// TestDispatcherRoute 验证 Dispatcher 按 type 路由。
func TestDispatcherRoute(t *testing.T) {
	d := New()
	d.Register(NewFile())
	r := d.Dispatch(context.Background(), proto.TaskRequest{
		TaskID:  "t1",
		Type:    proto.TaskFile,
		Payload: []byte(`{"op":"download","path":"/nonexistent/go-rmm-test"}`),
	}, nil)
	if r.Success {
		t.Fatal("不存在的文件应失败")
	}

	// 未知类型
	r = d.Dispatch(context.Background(), proto.TaskRequest{
		TaskID: "t2",
		Type:   "unknown_type",
	}, nil)
	if r.Success {
		t.Fatal("未知类型应失败")
	}
}
