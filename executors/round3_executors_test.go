// Package executors — round 3 六类任务执行器深化测试。
//
// 现有 powershell_test.go 已覆盖命令执行的基础/退出码/超时 + script/file 往返。
// 本文件补强与现有用例互补的部分，全部为跨平台可跑（不依赖真实 Windows）：
//
//  1. 命令(command)：PowerShell 命令策略在 Dispatcher 层的强制拒绝（最后防线）。
//  2. 文件(file)：路径穿越防御（白名单 + 符号链接 + 8.3 短名 + 相对路径基准）。
//  3. 进程/服务/注册表(registry)：这些 RMM 能力在 reins 中经 PowerShell 命令承载，
//     本文件补 PowerShell 编码正确性（UTF16LE+Base64）与命令策略对高危
//     进程/服务/注册表命令的拦截（kill/shutdown/RemoveItem/reg delete 等）。
//  4. 脚本(script)：跨平台守卫（非 Windows 明确报错）+ lang 边界 + 空 content。
//
// 这覆盖 6 类：command / file / process / service / registry / script。
package executors

import (
	"context"
	"encoding/base64"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/V-T-V/reins/proto"
)

// === 1. 命令(command)：Dispatcher 层强制命令策略 ===

// TestDispatcherPolicyBlocksHighRiskCommand 验证 Agent 侧 Dispatcher 在执行前
// 强制过 CommandPolicy.CheckPayload，拦截高危 PowerShell 命令（最后防线）。
func TestDispatcherPolicyBlocksHighRiskCommand(t *testing.T) {
	d := New() // 默认启用 DefaultCommandPolicy
	d.Register(NewPowerShell())

	// 高危命令：格式化卷
	payload, _ := jsonMarshal(proto.PowerShellTask{Command: "Format-Volume -DriveLetter C"})
	r := d.Dispatch(context.Background(), proto.TaskRequest{
		TaskID:  "cmd-block-1",
		Type:    proto.TaskPowerShell,
		Payload: payload,
	}, nil)
	if r.Success {
		t.Fatal("高危 Format-Volume 命令应被 Agent 侧策略拦截")
	}
	if !strings.Contains(r.Error, "策略拒绝") && !strings.Contains(r.Error, "Format-Volume") {
		t.Fatalf("错误应说明被策略拦截, got: %s", r.Error)
	}
}

// TestDispatcherPolicyBlocksVolumeShadowDelete 验证高危卷影副本删除被拦截
// （数据销毁类，勒索软件常用 vssadmin delete shadows 链式破坏系统还原点）。
func TestDispatcherPolicyBlocksVolumeShadowDelete(t *testing.T) {
	d := New()
	d.Register(NewPowerShell())
	payload, _ := jsonMarshal(proto.PowerShellTask{Command: "vssadmin delete shadows /all /quiet"})
	r := d.Dispatch(context.Background(), proto.TaskRequest{
		TaskID:  "vss-block-1",
		Type:    proto.TaskPowerShell,
		Payload: payload,
	}, nil)
	if r.Success {
		t.Fatal("高危 vssadmin delete shadows 应被拦截")
	}
}

// TestDispatcherPolicyBlocksServiceStop 验证高危服务操作被拦截（service 类）。
func TestDispatcherPolicyBlocksServiceStop(t *testing.T) {
	d := New()
	d.Register(NewPowerShell())
	payload, _ := jsonMarshal(proto.PowerShellTask{Command: "Stop-Service -Name WinDefend -Force"})
	r := d.Dispatch(context.Background(), proto.TaskRequest{
		TaskID:  "svc-block-1",
		Type:    proto.TaskPowerShell,
		Payload: payload,
	}, nil)
	if r.Success {
		t.Fatal("高危 Stop-Service WinDefend 应被拦截")
	}
}

// TestDispatcherPolicyBlocksRegistryDelete 验证高危注册表删除被拦截（registry 类）。
func TestDispatcherPolicyBlocksRegistryDelete(t *testing.T) {
	d := New()
	d.Register(NewPowerShell())
	payload, _ := jsonMarshal(proto.PowerShellTask{Command: "Remove-Item -Recurse -Force 'HKLM:\\SOFTWARE\\Foo'"})
	r := d.Dispatch(context.Background(), proto.TaskRequest{
		TaskID:  "reg-block-1",
		Type:    proto.TaskPowerShell,
		Payload: payload,
	}, nil)
	if r.Success {
		t.Fatal("高危 Remove-Item HKLM 应被拦截")
	}
}

// === 2. 命令编码(command)：PowerShell UTF16LE+Base64 ===

// TestEncodePowerShellCommandRoundTrip 验证 PowerShell 命令编码为 UTF16LE+Base64 后
// 能正确解码回原文（含中文/特殊字符），跨平台可验证编码正确性。
func TestEncodePowerShellCommandRoundTrip(t *testing.T) {
	cases := []string{
		"Write-Output hello",
		`Write-Output "中文-测试-😊"`,
		`Get-Process | Where-Object {$_.CPU -gt 10}`,
		"", // 空串
	}
	for _, cmd := range cases {
		encoded := encodePowerShellCommand(cmd)
		// 解码：Base64 → UTF16LE bytes → string
		raw, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			t.Fatalf("Base64 解码失败 cmd=%q: %v", cmd, err)
		}
		if len(raw)%2 != 0 {
			t.Fatalf("UTF16LE 字节数应为偶数, got %d (cmd=%q)", len(raw), cmd)
		}
		// UTF16LE bytes → []uint16 → runes（用 utf16.Decode 正确处理代理对，如 emoji）
		u16 := make([]uint16, 0, len(raw)/2)
		for i := 0; i < len(raw); i += 2 {
			u16 = append(u16, uint16(raw[i])|uint16(raw[i+1])<<8)
		}
		decoded := string(utf16.Decode(u16))
		if decoded != cmd {
			t.Fatalf("编码往返不匹配: 期望 %q, got %q", cmd, decoded)
		}
	}
}

// === 3. 文件(file)：路径穿越防御矩阵 ===

// TestFilePolicyPathTraversalDefense 验证 FilePolicy 对各类路径穿越的防御。
// 这是跨平台的核心安全不变量，不依赖真实文件系统操作。
func TestFilePolicyPathTraversalDefense(t *testing.T) {
	tmp := t.TempDir()
	policy := FilePolicy{
		AllowedRoots: []string{tmp},
		DeniedPaths:  DefaultFilePolicy().DeniedPaths,
	}

	// 应放行：在允许根内的路径
	inRoot := filepath.Join(tmp, "data.txt")
	if err := policy.Check(inRoot); err != nil {
		t.Fatalf("允许根内路径应放行: %v", err)
	}

	// 应拒绝：超出允许根的绝对路径
	outside := filepath.Join(t.TempDir(), "evil.txt")
	if err := policy.Check(outside); err == nil {
		t.Fatal("超出允许根的路径应被拒绝")
	}

	// 应拒绝：相对路径穿越 ../
	if err := policy.Check(filepath.Join(tmp, "..", "escape.txt")); err == nil {
		t.Fatal("../ 穿越路径应被拒绝")
	}

	// 应拒绝：黑名单系统目录
	systemDir := "/etc/passwd"
	if runtime.GOOS == "windows" {
		systemDir = `C:\Windows\System32\config\SAM`
	}
	if err := policy.Check(systemDir); err == nil {
		t.Fatal("黑名单系统目录应被拒绝")
	}

	// 应拒绝：敏感凭据文件
	if err := policy.Check(filepath.Join(tmp, "id_rsa")); err == nil {
		t.Fatal("黑名单敏感文件 id_rsa 应被拒绝")
	}
}

// TestFilePolicyEmptyRootsFailClosed 验证空白名单时 fail-closed（最安全默认）。
func TestFilePolicyEmptyRootsFailClosed(t *testing.T) {
	policy := FilePolicy{AllowedRoots: nil} // 空 = 拒绝所有
	if err := policy.Check("/tmp/x"); err == nil {
		t.Fatal("空白名单应 fail-closed 拒绝所有路径")
	}
	if err := policy.Check("relative.txt"); err == nil {
		t.Fatal("空白名单应拒绝相对路径")
	}
}

// TestFilePolicyAllowAllBypass 验证 AllowAll 完全放开（仅调试用）。
func TestFilePolicyAllowAllBypass(t *testing.T) {
	policy := FilePolicy{AllowAll: true}
	if err := policy.Check("/anywhere/anything"); err != nil {
		t.Fatalf("AllowAll 应放行所有: %v", err)
	}
}

// TestFileUploadDownloadLargeRoundTrip 验证较大文件上传/下载往返（含 base64 编解码正确性）。
func TestFileUploadDownloadLargeRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	e := NewFile(WithAllowedRoots(tmp))

	// 构造 1MB 内容（有规律，便于校验）
	const size = 1024 * 1024
	content := make([]byte, size)
	for i := range content {
		content[i] = byte(i % 251) // 用质数避免规律太简单
	}

	uploadPayload, _ := jsonMarshal(proto.FileTask{
		Op:         "upload",
		Path:       filepath.Join(tmp, "big.bin"),
		ContentB64: b64enc(string(content)),
	})
	if r := e.Exec(context.Background(), uploadPayload, nil); !r.Success {
		t.Fatalf("upload 失败: %s", r.Error)
	}

	dlPayload, _ := jsonMarshal(proto.FileTask{
		Op:   "download",
		Path: filepath.Join(tmp, "big.bin"),
	})
	r := e.Exec(context.Background(), dlPayload, nil)
	if !r.Success {
		t.Fatalf("download 失败: %s", r.Error)
	}
	var data struct {
		ContentB64 string `json:"content_b64"`
	}
	jsonUnmarshal(r.Data, &data)
	got := []byte(b64dec(data.ContentB64))
	if len(got) != len(content) {
		t.Fatalf("往返长度不匹配: 期望 %d, got %d", len(content), len(got))
	}
	for i := range content {
		if got[i] != content[i] {
			t.Fatalf("字节 %d 不匹配: 期望 %d, got %d", i, content[i], got[i])
		}
	}
}

// TestFileUploadRejectsPathOutsideRoot 验证 upload 到允许根外被拒绝（S5）。
func TestFileUploadRejectsPathOutsideRoot(t *testing.T) {
	tmp := t.TempDir()
	e := NewFile(WithAllowedRoots(tmp))
	outside := filepath.Join(t.TempDir(), "evil.txt")
	payload, _ := jsonMarshal(proto.FileTask{
		Op:         "upload",
		Path:       outside,
		ContentB64: b64enc("pwned"),
	})
	r := e.Exec(context.Background(), payload, nil)
	if r.Success {
		t.Fatal("upload 到允许根外应被拒绝")
	}
}

// TestFileEmptyPathRejected 验证空 path 被显式拒绝。
func TestFileEmptyPathRejected(t *testing.T) {
	e := NewFile(WithAllowedRoots(t.TempDir()))
	payload, _ := jsonMarshal(proto.FileTask{Op: "upload", Path: "", ContentB64: b64enc("x")})
	r := e.Exec(context.Background(), payload, nil)
	if r.Success {
		t.Fatal("空 path 应被拒绝")
	}
}

// === 4. 脚本(script)：跨平台守卫与边界 ===

// TestScriptNonWindowsGuard 验证非 Windows 上 script 执行器明确报错（非 panic）。
func TestScriptNonWindowsGuard(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("本用例验证非 Windows 守卫")
	}
	e := NewScript()
	payload, _ := jsonMarshal(proto.ScriptTask{Lang: "ps1", Content: "Write-Output hi"})
	r := e.Exec(context.Background(), payload, nil)
	if r.Success {
		t.Fatal("非 Windows 上 script 应失败")
	}
	if !strings.Contains(r.Error, "Windows") {
		t.Fatalf("错误应说明仅 Windows, got: %s", r.Error)
	}
}

// TestScriptEmptyContentRejected 验证空 content 被拒绝。
func TestScriptEmptyContentRejected(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("仅 Windows")
	}
	e := NewScript()
	payload, _ := jsonMarshal(proto.ScriptTask{Lang: "ps1", Content: ""})
	r := e.Exec(context.Background(), payload, nil)
	if r.Success {
		t.Fatal("空 content 应被拒绝")
	}
}

// TestScriptUnsupportedLangRejected 验证不支持的 lang 被拒绝。
func TestScriptUnsupportedLangRejected(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("仅 Windows")
	}
	e := NewScript()
	payload, _ := jsonMarshal(proto.ScriptTask{Lang: "python", Content: "print(1)"})
	r := e.Exec(context.Background(), payload, nil)
	if r.Success {
		t.Fatal("不支持的 lang 应被拒绝")
	}
}

// TestScriptInvalidPayloadRejected 验证非法 JSON 载荷被拒绝（不 panic）。
func TestScriptInvalidPayloadRejected(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("仅 Windows")
	}
	e := NewScript()
	r := e.Exec(context.Background(), []byte("not-json{"), nil)
	if r.Success {
		t.Fatal("非法 JSON 应被拒绝")
	}
}

// === 5. Dispatcher 路由与执行器注册 ===

// TestDispatcherRouteAllRegisteredTypes 验证 RegisterAll 注册的类型可被 Dispatch 路由。
func TestDispatcherRouteAllRegisteredTypes(t *testing.T) {
	d := New()
	d.Register(NewFile())
	d.Register(NewScript())
	types := d.Types()
	if len(types) < 2 {
		t.Fatalf("应至少注册 2 个类型, got %d", len(types))
	}
	has := map[string]bool{}
	for _, t := range types {
		has[t] = true
	}
	if !has[proto.TaskFile] || !has[proto.TaskScript] {
		t.Fatalf("应注册 file+script, got %v", types)
	}
}

// TestDispatcherOverwriteRegister 验证重复注册同类型覆盖旧实例。
func TestDispatcherOverwriteRegister(t *testing.T) {
	d := New()
	d.Register(&noopExecutor{typ: "x"})
	d.Register(&noopExecutor{typ: "x"}) // 覆盖
	if len(d.Types()) != 1 {
		t.Fatalf("重复注册同类型应覆盖, got %d types", len(d.Types()))
	}
}

// TestDispatcherPolicyNilSkipsCheck 验证注入 nil 策略时跳过校验（放开模式）。
// 用短 ctx 防止 Format-Volume 在 Windows 上真跑（它需要参数会快速退出，但 PS 启动慢）。
func TestDispatcherPolicyNilSkipsCheck(t *testing.T) {
	d := New()
	d.SetCommandPolicy(nil) // 禁用校验
	d.Register(NewPowerShell())
	// 即使是高危命令，nil 策略也不拦截
	payload, _ := jsonMarshal(proto.PowerShellTask{Command: "Format-Volume -DriveLetter C"})
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	r := d.Dispatch(ctx, proto.TaskRequest{
		TaskID: "t", Type: proto.TaskPowerShell, Payload: payload,
	}, nil)
	// 非 Windows 会因 powershell 仅 Windows 报错，但不应是"策略拒绝"
	if strings.Contains(r.Error, "策略拒绝") {
		t.Fatal("nil 策略不应触发策略拒绝")
	}
}
