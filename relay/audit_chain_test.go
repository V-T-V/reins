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

// TestVerifyChainEmptyFile 空审计文件应判为完整(无记录=无篡改)。
func TestVerifyChainEmptyFile(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "empty.log")
	if err := os.WriteFile(path, []byte{}, 0600); err != nil {
		t.Fatal(err)
	}
	line, err := VerifyChain(path)
	if err != nil {
		t.Errorf("空文件应无错: %v", err)
	}
	if line != -1 {
		t.Errorf("空文件应返回 -1(完整), 得到 %d", line)
	}
}

// TestVerifyChainMissingFile 不存在的文件应返回错误(非 panic)。
func TestVerifyChainMissingFile(t *testing.T) {
	line, err := VerifyChain(filepath.Join(t.TempDir(), "nope.log"))
	if err == nil {
		t.Error("缺失文件应返回错误")
	}
	if line != -1 {
		t.Errorf("缺失文件行号应 -1, 得到 %d", line)
	}
}

// TestVerifyChainTrailingNewlineOnly 仅含空行/换行的文件应判完整(无有效记录)。
func TestVerifyChainTrailingNewlineOnly(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "blank.log")
	os.WriteFile(path, []byte("\n\n"), 0600)
	line, err := VerifyChain(path)
	if err != nil {
		t.Errorf("纯空行文件应无错: %v", err)
	}
	if line != -1 {
		t.Errorf("纯空行应 -1, 得到 %d", line)
	}
}

// TestAuditWriteEntryFailureNoPanic 写盘失败(file 已关闭)不 panic,仅记日志。
// D8 加固回归:原实现丢弃 Write 返回值,现应安全处理错误。
func TestAuditWriteEntryFailureNoPanic(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "audit.log")
	audit, err := OpenAuditLog(path)
	if err != nil {
		t.Fatalf("OpenAuditLog: %v", err)
	}
	// 等待 writeLoop 启动并写若干条
	for i := 0; i < 3; i++ {
		audit.Log(AuditEntry{Event: "before-close", AgentID: "a"})
	}
	time.Sleep(150 * time.Millisecond)
	// 关闭底层文件,模拟后续写盘失败
	audit.file.Close()
	// 再投递一条:writeEntry 写已关闭文件应记日志而不 panic
	audit.Log(AuditEntry{Event: "after-close", AgentID: "a"})
	time.Sleep(150 * time.Millisecond)
	// 到这里不 panic 即通过(幂等 Close 也应安全)
	audit.Close()
}
