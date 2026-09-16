package main

// P2-11 per-chunk SHA256 强制测试:
//   - computeSectionSHA256 纯函数:分片段 hash 正确性(尾片不足 chunkSize/跨片边界)
//   - uploadSingleChunk:非空 chunkSHA 必须随 query 下发且与 body 内容一致
//   - fileUpload 非分片路径:整文件 sha256 预计算进 query(httptest mock Relay E2E)
//   - 管道模式(-)不带 sha256(单遍流无法预计算,事后比对兜底)

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// sha256Hex 测试侧独立计算(避免直接复用被测函数导致假绿)。
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestComputeSectionSHA256(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "blob.bin")
	// 构造跨分片边界的内容:part0 + part1 + 尾片不足
	part0 := make([]byte, 4096)
	part1 := make([]byte, 4096)
	tail := []byte("short tail chunk")
	for i := range part0 {
		part0[i] = byte(i)
	}
	for i := range part1 {
		part1[i] = byte(255 - i)
	}
	full := append(append([]byte{}, part0...), part1...)
	full = append(full, tail...)
	if err := os.WriteFile(path, full, 0644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	// 分片 0(满片)
	got0, err := computeSectionSHA256(f, 0, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if want := sha256Hex(part0); got0 != want {
		t.Errorf("分片 0 hash 不匹配: got %s want %s", got0, want)
	}
	// 分片 1(满片,offset 起点非 0)
	got1, err := computeSectionSHA256(f, 4096, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if want := sha256Hex(part1); got1 != want {
		t.Errorf("分片 1 hash 不匹配: got %s want %s", got1, want)
	}
	// 尾片(不足 chunkSize)
	got2, err := computeSectionSHA256(f, 8192, int64(len(tail)))
	if err != nil {
		t.Fatal(err)
	}
	if want := sha256Hex(tail); got2 != want {
		t.Errorf("尾片 hash 不匹配: got %s want %s", got2, want)
	}
	// 单片即整文件时与 computeFileSHA256 一致(非分片路径的同一语义)
	gotFull, err := computeFileSHA256(path)
	if err != nil {
		t.Fatal(err)
	}
	gotAll, err := computeSectionSHA256(f, 0, int64(len(full)))
	if err != nil {
		t.Fatal(err)
	}
	if gotAll != gotFull {
		t.Errorf("全文件段 hash 应与 computeFileSHA256 一致: %s vs %s", gotAll, gotFull)
	}
	// 越界段:SectionReader 语义是读到 EOF 为止(size 超文件尾收敛),不报错
	// 但 hash 只覆盖实际存在字节——与 io.SectionReader 文档语义一致
	gotClip, err := computeSectionSHA256(f, 0, int64(len(full))+100)
	if err != nil {
		t.Fatal(err)
	}
	if gotClip != gotFull {
		t.Errorf("越界 size 应收敛到文件尾: %s vs %s", gotClip, gotFull)
	}
}

func TestUploadSingleChunkSendsSHA(t *testing.T) {
	body := []byte("chunk-content-0123456789")
	var gotQuerySHA, gotPath string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuerySHA = r.URL.Query().Get("sha256")
		gotPath = r.URL.Query().Get("path")
		var err error
		gotBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("读 body 失败: %v", err)
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	err := uploadSingleChunk(config{relay: srv.URL}, "ag-1", "remote.zip.ftpart0000",
		bytes.NewReader(body), int64(len(body)), "local.zip", 0, sha256Hex(body))
	if err != nil {
		t.Fatalf("上传应成功: %v", err)
	}
	if gotPath != "remote.zip.ftpart0000" {
		t.Errorf("path 不匹配: %s", gotPath)
	}
	if gotQuerySHA == "" {
		t.Fatal("非空 chunkSHA 必须随 query 下发(Agent 侧据此单片校验)")
	}
	if gotQuerySHA != sha256Hex(gotBody) {
		t.Errorf("query sha256 与 body 内容不一致: query=%s body=%s", gotQuerySHA, sha256Hex(gotBody))
	}
}

func TestUploadSingleChunkEmptySHAOmitted(t *testing.T) {
	var sawQuery bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, sawQuery = r.URL.Query()["sha256"]
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	// 空 chunkSHA(向后兼容路径,如旧部署关闭校验):不携带该 query
	if err := uploadSingleChunk(config{relay: srv.URL}, "ag-1", "p",
		bytes.NewReader([]byte("x")), 1, "f", 0, ""); err != nil {
		t.Fatalf("上传应成功: %v", err)
	}
	if sawQuery {
		t.Error("空 chunkSHA 不应携带 sha256 query")
	}
}

// TestFileUploadPrecomputedSHA E2E:非分片 fileUpload 必须带整文件 sha256 query。
func TestFileUploadPrecomputedSHA(t *testing.T) {
	tmp := t.TempDir()
	local := filepath.Join(tmp, "small.bin")
	content := []byte("plain upload content for sha e2e")
	if err := os.WriteFile(local, content, 0644); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var gotQuerySHA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Phase1:fileUpload 先试直连票据请求,stub 应答未开启(回落中继后走下方断言)
		if r.URL.Path == "/api/transfer/direct" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"direct": false, "reason": "stub 未开启直连"})
			return
		}
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotQuerySHA = r.URL.Query().Get("sha256")
		mu.Unlock()
		if qs := r.URL.Query().Get("sha256"); qs != sha256Hex(body) {
			t.Errorf("query sha256 与 body 不一致: query=%s body=%s", qs, sha256Hex(body))
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"transfer_id": r.URL.Query().Get("transfer_id"),
			"bytes":       len(body),
			"sha256":      sha256Hex(body),
			"status":      "uploaded",
		})
	}))
	defer srv.Close()

	if err := fileUpload(config{relay: srv.URL}, []string{"-a", "ag-1", local, "remote/small.bin"}); err != nil {
		t.Fatalf("上传应成功: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotQuerySHA != sha256Hex(content) {
		t.Errorf("非分片上传应预计算整文件 sha256 进 query: got %s want %s", gotQuerySHA, sha256Hex(content))
	}
}

// TestFileUploadAgentRejectBubblesUp:Agent 校验失败(Relay 500)时 CLI 必须报错而非静默成功。
// (per-chunk hash 生效后的可观测行为:损坏片当场失败,重试由上层 chunk retry 承担)
func TestFileUploadAgentRejectBubblesUp(t *testing.T) {
	tmp := t.TempDir()
	local := filepath.Join(tmp, "small2.bin")
	if err := os.WriteFile(local, []byte("will be rejected"), 0644); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]any{
			"error": "agent 校验失败: SHA256 校验失败: 期望 abc 实际 def",
		})
	}))
	defer srv.Close()

	err := fileUpload(config{relay: srv.URL}, []string{"-a", "ag-1", local, "remote/small2.bin"})
	if err == nil {
		t.Fatal("Agent 侧 SHA 校验失败(Relay 500)应使 fileUpload 返回错误")
	}
}
