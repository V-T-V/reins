package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAuditHashChain(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "audit.log")

	audit, err := OpenAuditLog(path)
	if err != nil {
		t.Fatalf("OpenAuditLog 失败: %v", err)
	}

	// 写 5 条记录
	for i := 0; i < 5; i++ {
		audit.Log(AuditEntry{
			Event:   "task_submitted",
			AgentID: "agent-1",
			TaskID:  "task-" + string(rune('A'+i)),
			Detail:  "test entry",
		})
	}

	// 等异步写入完成
	time.Sleep(200 * time.Millisecond)
	audit.Close()

	// 校验 chain 完整性
	line, err := VerifyChain(path)
	if err != nil {
		t.Fatalf("chain 校验失败(行 %d): %v", line, err)
	}
	if line != -1 {
		t.Errorf("期望 chain 完整(-1),得到篡改行 %d", line)
	}
}

func TestAuditHashChainTamper(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "audit.log")

	audit, _ := OpenAuditLog(path)
	for i := 0; i < 3; i++ {
		audit.Log(AuditEntry{Event: "test", AgentID: "a1"})
	}
	time.Sleep(200 * time.Millisecond)
	audit.Close()

	// 篡改文件(修改中间一行)
	data, _ := os.ReadFile(path)
	// 在中间插入一个字符(破坏 hash)
	if len(data) > 20 {
		data[15] ^= 0xFF
		os.WriteFile(path, data, 0600)
	}

	// 校验应检测到篡改
	line, err := VerifyChain(path)
	if err == nil {
		t.Error("篡改后应检测到错误")
	}
	if line == -1 {
		t.Error("应返回篡改行号(非 -1)")
	}
}
