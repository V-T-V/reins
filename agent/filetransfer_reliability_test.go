package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/V-T-V/reins/proto"
)

// TestPathMutexRejectConcurrentSamePath 验证 R1:path 互斥拒绝并发同 path。
// 同一个 path 第二次 HandleInit(upload)应被拒绝(Accepted=false),防双写 .part。
func TestPathMutexRejectConcurrentSamePath(t *testing.T) {
	tmp := t.TempDir()
	target := filepath.Join(tmp, "data.bin")

	te := &TransferEngine{
		sessions:  make(map[string]*transferSession),
		pathInUse: make(map[string]struct{}),
		policy:    &fileTransferPolicy{check: allowAllPaths},
		workspace: tmp,
	}

	// 第一次 HandleInit:占用 path
	ready1 := te.HandleInit(proto.FileTransferInit{
		TransferID: "session-A", Direction: "upload", Path: target, Size: 100,
	})
	if !ready1.Accepted {
		t.Fatalf("第一次 HandleInit 应成功,reason=%s", ready1.Reason)
	}

	// 第二次 HandleInit(不同 transferID,相同 path):应被拒绝
	ready2 := te.HandleInit(proto.FileTransferInit{
		TransferID: "session-B", Direction: "upload", Path: target, Size: 100,
	})
	if ready2.Accepted {
		t.Error("并发同 path 应被拒绝(pathInUse 生效)")
	}
	if ready2.Reason == "" {
		t.Error("拒绝时应给出 reason")
	}

	// completeUpload 释放后,第三次应成功
	te.completeUpload("session-A", false, 0, "test cleanup", func(proto.FileTransferComplete) {})
	ready3 := te.HandleInit(proto.FileTransferInit{
		TransferID: "session-C", Direction: "upload", Path: target, Size: 100,
	})
	if !ready3.Accepted {
		t.Error("释放 pathInUse 后应允许新传输")
	}
	// 清理
	te.completeUpload("session-C", false, 0, "cleanup", func(proto.FileTransferComplete) {})
	os.Remove(target + ".part")
}

// TestTryCompleteFullPartSuccess 验证 R2:已满快路径成功(rename + 返回 sha)。
func TestTryCompleteFullPartSuccess(t *testing.T) {
	tmp := t.TempDir()
	target := filepath.Join(tmp, "data.bin")
	partPath := target + ".part"

	// 准备一个完整的 .part(内容已知 sha256)
	content := []byte("hello world complete content")
	if err := os.WriteFile(partPath, content, 0644); err != nil {
		t.Fatal(err)
	}
	// 算期望 sha
	expectedSHA := computeTestSHA256(t, content)

	te := &TransferEngine{
		sessions: make(map[string]*transferSession), pathInUse: make(map[string]struct{}),
		policy: &fileTransferPolicy{check: allowAllPaths}, workspace: tmp,
	}

	sha, ok := te.tryCompleteFullPart(proto.FileTransferInit{
		TransferID: "full-1", Direction: "upload", Path: target,
		Size: int64(len(content)), SHA256: expectedSHA,
	}, partPath, int64(len(content)))

	if !ok {
		t.Fatal("已满快路径应成功")
	}
	if sha != expectedSHA {
		t.Errorf("sha 不匹配: got %s want %s", sha, expectedSHA)
	}
	// .part 应被 rename 成正式文件
	if _, err := os.Stat(partPath); err == nil {
		t.Error(".part 应已被 rename 不存在")
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("正式文件应存在: %v", err)
	}
	if string(got) != string(content) {
		t.Error("rename 后内容应一致")
	}
}

// TestTryCompleteFullPartHashMismatch 验证 R2:hash 不匹配时删除 .part 返回 false。
func TestTryCompleteFullPartHashMismatch(t *testing.T) {
	tmp := t.TempDir()
	target := filepath.Join(tmp, "data.bin")
	partPath := target + ".part"

	content := []byte("some content")
	os.WriteFile(partPath, content, 0644)

	te := &TransferEngine{
		sessions: make(map[string]*transferSession), pathInUse: make(map[string]struct{}),
		policy: &fileTransferPolicy{check: allowAllPaths}, workspace: tmp,
	}

	_, ok := te.tryCompleteFullPart(proto.FileTransferInit{
		TransferID: "bad-1", Path: target, Size: int64(len(content)),
		SHA256: "0000000000000000000000000000000000000000000000000000000000000000",
	}, partPath, int64(len(content)))

	if ok {
		t.Error("hash 不匹配应返回 false")
	}
	// .part 应被删除(防死循环)
	if _, err := os.Stat(partPath); err == nil {
		t.Error("hash 不匹配时 .part 应被删除")
	}
}

// TestSweepOrphanParts 验证 S3:孤儿分片清理。
// 过期的 .ftpart 被删,新文件保留,活跃传输 path 不动。
func TestSweepOrphanParts(t *testing.T) {
	tmp := t.TempDir()
	te := &TransferEngine{
		sessions:  make(map[string]*transferSession),
		pathInUse: make(map[string]struct{}),
		policy:    &fileTransferPolicy{check: allowAllPaths},
		workspace: tmp,
	}

	// 造 3 个文件:
	// 1) 过期 .ftpart(应删)
	// 2) 新 .ftpart(应保留)
	// 3) 过期但 pathInUse 占用(应保留)
	oldPart := filepath.Join(tmp, "file1.bin.ftpart00")
	os.WriteFile(oldPart, []byte("old"), 0644)
	os.Chtimes(oldPart, time.Now().Add(-3*time.Hour), time.Now().Add(-3*time.Hour))

	newPart := filepath.Join(tmp, "file2.bin.ftpart00")
	os.WriteFile(newPart, []byte("new"), 0644)

	activePart := filepath.Join(tmp, "file3.bin.ftpart00")
	os.WriteFile(activePart, []byte("active"), 0644)
	os.Chtimes(activePart, time.Now().Add(-3*time.Hour), time.Now().Add(-3*time.Hour))
	// 模拟活跃传输占用 file3.bin
	te.pathInUse["C:"+filepath.Join(tmp, "file3.bin")] = struct{}{}
	// 上面用绝对路径模拟,但 sweep 用 filepath.Base 比较,这里直接用 base
	for k := range te.pathInUse {
		delete(te.pathInUse, k)
	}
	te.pathInUse[filepath.Join(tmp, "file3.bin")] = struct{}{}

	te.sweepOrphanParts(2 * time.Hour)

	// 1 应删
	if _, err := os.Stat(oldPart); err == nil {
		t.Error("过期 .ftpart 应被删除")
	}
	// 2 应保留
	if _, err := os.Stat(newPart); err != nil {
		t.Error("新 .ftpart 应保留")
	}
	// 3 应保留(活跃)
	if _, err := os.Stat(activePart); err != nil {
		t.Error("活跃传输的 path 不应被清理")
	}
}

// TestDeadConnWatchdogExposesLastRecv 验证 S1:wsConn.lastRecv 在 read 后更新。
// (完整 watchdog 需要真实 WS,这里只验证时间戳机制)
func TestDeadConnWatchdogExposesLastRecv(t *testing.T) {
	w := &wsConn{}
	w.lastRecv.Store(0) // 模拟从未读取
	if w.lastRecvMs() != 0 {
		t.Error("初始 lastRecv 应为 0")
	}
	w.lastRecv.Store(time.Now().UnixMilli())
	if w.lastRecvMs() == 0 {
		t.Error("Store 后 lastRecv 应非 0")
	}
}

// computeTestSHA256 算测试内容的 sha256 hex。
func computeTestSHA256(t *testing.T, data []byte) string {
	t.Helper()
	h := sha256.New()
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}
