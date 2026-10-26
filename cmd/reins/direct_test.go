package main

// Phase1 CLI 直连层测试:
//   1. --no-direct flag 剥离 + REINS_NO_DIRECT env
//   2. Agent 未开启直连(direct=false)→ errDirectUnavailable + 熔断置位(静默回落语义)
//   3. stub 直连服务器协议契约:票据头/begin 握手 resume/data_token/data 流式
//      (真 Agent 端到端由本地三组件 E2E 覆盖,见 scripts/verify)
//   4. 策略拒绝(403)→ isExitBlocked 终态不回落

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func resetDirectState(t *testing.T) {
	t.Helper()
	oldNo, oldDis := noDirectMode, directDisabled.Load()
	t.Cleanup(func() {
		noDirectMode = oldNo
		directDisabled.Store(oldDis)
	})
	noDirectMode = false
	directDisabled.Store(false)
}

func TestApplyDirectFlag(t *testing.T) {
	resetDirectState(t)
	out := applyDirectFlag([]string{"up", "-a", "x", "--no-direct", "local", "remote"})
	for _, a := range out {
		if a == "--no-direct" {
			t.Errorf("--no-direct 应被剥离: %v", out)
		}
	}
	if !noDirectMode {
		t.Error("--no-direct 应置位 noDirectMode")
	}
}

func TestApplyDirectFlagEnv(t *testing.T) {
	resetDirectState(t)
	t.Setenv("REINS_NO_DIRECT", "1")
	out := applyDirectFlag([]string{"up", "local", "remote"})
	if !noDirectMode {
		t.Error("env 应置位 noDirectMode")
	}
	if len(out) != 3 {
		t.Errorf("args 应原样: %v", out)
	}
}

// fakeRelayDirect 起 relay stub:POST /api/transfer/direct 返回固定应答。
func fakeRelayDirect(t *testing.T, direct bool, addrs []string, secret string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/transfer/direct", func(w http.ResponseWriter, r *http.Request) {
		if !direct {
			_ = json.NewEncoder(w).Encode(map[string]any{"direct": false, "reason": "agent 未开启直连"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"direct": true, "ticket_id": "tid-1", "secret": secret, "addrs": addrs,
			"expiry_unix": 4102444800,
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestDirectUnavailableFallback(t *testing.T) {
	resetDirectState(t)
	relay := fakeRelayDirect(t, false, nil, "")
	err := tryDirectUploadFile(config{relay: relay.URL}, "ag-1", "nosuch.bin", "remote.bin")
	if !errors.Is(err, errDirectUnavailable) {
		t.Errorf("direct=false 应返回 errDirectUnavailable: %v", err)
	}
	if !directDisabled.Load() {
		t.Error("direct=false 应置位熔断(后续分片不再探测)")
	}
}

func TestDirectUploadStubE2E(t *testing.T) {
	resetDirectState(t)

	// stub 直连服务器:校验票据头 + resume 偏移 + data_token,累积收数据回 sha
	var (
		mu       sync.Mutex
		gotData  []byte
		beginHit bool
	)
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/xfer/probe":
			if r.Header.Get("X-Reins-Ticket") != "tid-1:sec-1" {
				w.WriteHeader(401)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "agent": "ag-1"})
		case "/xfer/begin":
			beginHit = true
			if r.Header.Get("X-Reins-Ticket") != "tid-1:sec-1" {
				w.WriteHeader(401)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"accepted": true, "transfer_id": "dx-stub", "resume_offset": 0,
				"data_token": "tok-1",
			})
		case "/xfer/data":
			if r.Header.Get("X-Reins-Data-Token") != "tok-1" || r.URL.Query().Get("transfer_id") != "dx-stub" {
				w.WriteHeader(401)
				return
			}
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			gotData = append(gotData, b...)
			mu.Unlock()
			sum := sha256.Sum256(gotData)
			_ = json.NewEncoder(w).Encode(map[string]any{"bytes": len(gotData), "sha256": hex.EncodeToString(sum[:])})
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(stub.Close)

	// stub 地址是 http://127.0.0.1:port → addrs 用 host:port 形态
	host := stub.Listener.Addr().String()
	relay := fakeRelayDirect(t, true, []string{host}, "sec-1")

	// 注意:CLI 直连固定 https://,stub 是 http —— probe 会 TLS 握手失败走熔断回落。
	// 本用例改为直接注入 baseURL 验证 begin/data 协议契约(传输层留给三组件 E2E)。
	ds := &directSession{ticketID: "tid-1", secret: "sec-1", addrs: []string{host}, baseURL: stub.URL}

	content := []byte("stub direct upload payload " + string(bytes.Repeat([]byte("x"), 1024)))
	dir := t.TempDir()
	local := filepath.Join(dir, "up.bin")
	os.WriteFile(local, content, 0644)

	begin, err := ds.begin("remote.bin", "upload", map[string]any{"size": int64(len(content))})
	if err != nil || !begin.Accepted {
		t.Fatalf("begin 失败: %v %+v", err, begin)
	}
	if begin.DataToken != "tok-1" {
		t.Errorf("data_token 未回传: %+v", begin)
	}
	f, _ := os.Open(local)
	defer f.Close()
	n, gotSHA, err := ds.uploadData(begin.TransferID, begin.DataToken, f, int64(len(content)))
	if err != nil {
		t.Fatalf("uploadData 失败: %v", err)
	}
	sum := sha256.Sum256(content)
	if n != int64(len(content)) || gotSHA != hex.EncodeToString(sum[:]) {
		t.Errorf("上传结果不符: n=%d sha=%s", n, gotSHA)
	}
	mu.Lock()
	defer mu.Unlock()
	if !bytes.Equal(gotData, content) {
		t.Errorf("stub 收到内容不符: %d bytes", len(gotData))
	}
	if !beginHit {
		t.Error("begin 未被调用")
	}
	_ = relay // relay 在 TLS 探测用例外不参与本断言
}

func TestIsExitBlocked(t *testing.T) {
	if !isExitBlocked(errBlocked("x")) {
		t.Error("errBlocked 应被识别")
	}
	if isExitBlocked(errors.New("x")) {
		t.Error("普通错误不应被识别为 blocked")
	}
}

// TestDirectBlockedNotFallback 直连策略拒绝(403)应是终态错误,不回落。
func TestDirectBlockedNotFallback(t *testing.T) {
	resetDirectState(t)
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/xfer/begin" {
			w.WriteHeader(403)
			_ = json.NewEncoder(w).Encode(map[string]any{"accepted": false, "reason": "路径策略拒绝: 越界"})
			return
		}
		w.WriteHeader(404)
	}))
	t.Cleanup(stub.Close)
	ds := &directSession{ticketID: "t", secret: "s", baseURL: stub.URL}
	_, err := ds.begin("x.bin", "upload", nil)
	if !isExitBlocked(err) {
		t.Errorf("403 应映射为 blocked 终态: %v", err)
	}
}
