package executors

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/QiuShichang/reins/proto"
)

// TestChunkUploadMerge 验证 H1：分块上传后合并正确。
func TestChunkUploadMerge(t *testing.T) {
	tmpDir := t.TempDir()
	e := NewFile(WithAllowedRoots(tmpDir))
	target := filepath.Join(tmpDir, "big.bin")

	// 构造 2.5MB 内容（超过 2 个 1MB 分块）
	content := make([]byte, chunkChunkSize*2+512*1024)
	for i := range content {
		content[i] = byte(i % 256)
	}

	// 分 3 块上传
	totalChunks := (len(content) + chunkChunkSize - 1) / chunkChunkSize
	for i := 0; i < totalChunks; i++ {
		start := i * chunkChunkSize
		end := start + chunkChunkSize
		if end > len(content) {
			end = len(content)
		}
		chunk := content[start:end]
		payload, _ := jsonMarshal(proto.FileTask{
			Op:          "chunk_upload",
			Path:        target,
			UploadID:    "test-upload-1",
			ChunkIndex:  i,
			TotalChunks: totalChunks,
			ContentB64:  base64.StdEncoding.EncodeToString(chunk),
		})
		r := e.Exec(context.Background(), payload, nil)
		if !r.Success {
			t.Fatalf("分块 %d 上传失败: %s", i, r.Error)
		}
	}

	// 验证合并后的文件
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("读目标文件失败: %v", err)
	}
	if len(data) != len(content) {
		t.Fatalf("文件大小不符: 期望 %d，得到 %d", len(content), len(data))
	}
	// 校验内容
	for i := range content {
		if data[i] != content[i] {
			t.Fatalf("内容不匹配 @ offset %d: 期望 %d，得到 %d", i, content[i], data[i])
		}
	}
}

// TestChunkDownload 验证 H1：分块下载读取正确。
func TestChunkDownload(t *testing.T) {
	tmpDir := t.TempDir()
	e := NewFile(WithAllowedRoots(tmpDir))

	// 先用 chunk_upload 放一个 2.5MB 文件
	content := make([]byte, chunkChunkSize*2+512*1024)
	for i := range content {
		content[i] = byte(i % 256)
	}
	target := filepath.Join(tmpDir, "dl-test.bin")
	os.WriteFile(target, content, 0644)

	// 分块下载
	totalChunks := (len(content) + chunkChunkSize - 1) / chunkChunkSize
	var reassembled []byte
	for i := 0; i < totalChunks+1; i++ { // +1 测试 eof
		payload, _ := jsonMarshal(proto.FileTask{
			Op:         "chunk_download",
			Path:       target,
			ChunkIndex: i,
		})
		r := e.Exec(context.Background(), payload, nil)
		if !r.Success {
			t.Fatalf("分块 %d 下载失败: %s", i, r.Error)
		}
		var data struct {
			ContentB64 string `json:"content_b64"`
			EOF        bool   `json:"eof"`
			Bytes      int    `json:"bytes"`
		}
		jsonUnmarshal(r.Data, &data)
		if data.EOF && data.Bytes == 0 {
			break // eof
		}
		chunk, _ := base64.StdEncoding.DecodeString(data.ContentB64)
		reassembled = append(reassembled, chunk...)
	}

	// 验证重组
	if len(reassembled) != len(content) {
		t.Fatalf("重组大小不符: 期望 %d，得到 %d", len(content), len(reassembled))
	}
	for i := range content {
		if reassembled[i] != content[i] {
			t.Fatalf("内容不匹配 @ offset %d", i)
		}
	}
}
