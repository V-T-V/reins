package main

// P2-11 per-chunk SHA256 的 Agent 侧核心防线测试:
// ReceiveUploadData 在 isLast 时比对 init.SHA256(expectedSHA256)——
// CLI 分片/非分片上传把单片 hash 随 query 下发后,这条路径是
// "损坏分片当场拒绝 + 不留正式文件" 的唯一执行点。
//   - hash 匹配:complete 成功 + .part rename 成正式文件
//   - hash 不匹配:complete 失败 + .part 删除(防断点续传把损坏片当已完成)

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/QiuShichang/go-rmm/proto"
)

// runSingleChunkUpload 模拟一次单片上传(init → 单帧 isLast),返回 complete 结果。
func runSingleChunkUpload(t *testing.T, tmp, name string, content []byte, expectedSHA string) proto.FileTransferComplete {
	t.Helper()
	target := filepath.Join(tmp, name)
	te := &TransferEngine{
		sessions: make(map[string]*transferSession), pathInUse: make(map[string]struct{}),
		policy: &fileTransferPolicy{check: allowAllPaths}, workspace: tmp,
	}
	init := proto.FileTransferInit{
		TransferID: "chunk-" + name, Direction: "upload", Path: target,
		Size: int64(len(content)), SHA256: expectedSHA,
	}
	ready := te.HandleInit(init)
	if !ready.Accepted {
		t.Fatalf("init 应被接受: %s", ready.Reason)
	}

	var result proto.FileTransferComplete
	te.ReceiveUploadData(init.TransferID, content, true,
		func(proto.FileTransferProgress) {},
		func(c proto.FileTransferComplete) { result = c })
	return result
}

func TestReceiveUploadDataSingleChunkSHAOk(t *testing.T) {
	tmp := t.TempDir()
	content := []byte("single chunk upload payload")
	sha := computeTestSHA256(t, content)

	result := runSingleChunkUpload(t, tmp, "ok.bin", content, sha)

	if !result.Success {
		t.Fatalf("hash 匹配的单片上传应成功: %+v", result)
	}
	if result.SHA256Actual != sha {
		t.Errorf("complete 应回传实际 hash: got %s want %s", result.SHA256Actual, sha)
	}
	// 损坏不应留正式文件——成功路径应 rename 成正式文件且内容一致
	got, err := os.ReadFile(filepath.Join(tmp, "ok.bin"))
	if err != nil {
		t.Fatalf("成功后正式文件应存在: %v", err)
	}
	if string(got) != string(content) {
		t.Error("正式文件内容应与上传一致")
	}
}

func TestReceiveUploadDataSingleChunkSHAMismatch(t *testing.T) {
	tmp := t.TempDir()
	content := []byte("corrupted chunk payload")

	result := runSingleChunkUpload(t, tmp, "bad.bin", content,
		"0000000000000000000000000000000000000000000000000000000000000000")

	if result.Success {
		t.Fatal("hash 不匹配的单片上传必须失败(Agent 侧最后防线)")
	}
	// .part 必须删除:断点续传按大小匹配跳过,残留损坏片 = 永久跳过死循环
	if _, err := os.Stat(filepath.Join(tmp, "bad.bin") + ".part"); err == nil {
		t.Error("hash 不匹配时 .part 应被删除(防损坏片被断点续传跳过)")
	}
	// 正式文件不应存在(rename 未发生)
	if _, err := os.Stat(filepath.Join(tmp, "bad.bin")); err == nil {
		t.Error("校验失败时不应产生正式文件")
	}
}
