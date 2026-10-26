package main

// Phase1 直连传输 Agent 侧测试:
//   1. 票据存储语义:hash 恒时比对/一次性消费/过期/方向绑定/分片前缀绑定
//   2. 直连 upload E2E:begin → data → 文件落盘 + SHA256 对拍 + 重传 ALREADY_COMPLETE
//   3. 直连 download E2E:begin → fetch → 内容逐位 + trailer hash
//   4. 路径策略拒绝:begin 403
//   5. data_token 一次性:第二次 data 拒绝

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/QiuShichang/reins/proto"
)

func ticketSecretHash(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func newTestTicket(path, direction string) (id, secret string) {
	return "tkt-" + fmt.Sprint(time.Now().UnixNano()), hex.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
}

func TestXferTicketStoreSemantics(t *testing.T) {
	s := &xferTicketStore{tickets: make(map[string]proto.XferTicketGrant)}
	sec := "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	s.Add(proto.XferTicketGrant{
		TicketID:   "t1",
		SecretHash: ticketSecretHash(sec),
		Path:       `C:\ws\big.tar`,
		Direction:  "upload",
		ExpiryUnix: time.Now().Add(time.Minute).Unix(),
	})

	// 正确 secret + 绑定路径 + 方向 → 通过
	if !s.verifyConsume("t1", sec, `C:\ws\big.tar`, "upload", false) {
		t.Error("合法票据 peek 应通过")
	}
	// peek 不消费:仍可再次通过
	if !s.verifyConsume("t1", sec, `C:\ws\big.tar`, "upload", false) {
		t.Error("peek 不应消费票据")
	}
	// 错误 secret → 拒
	if s.verifyConsume("t1", "x"+sec[1:], `C:\ws\big.tar`, "upload", false) {
		t.Error("错误 secret 应拒绝")
	}
	// 错误方向 → 拒
	if s.verifyConsume("t1", sec, `C:\ws\big.tar`, "download", false) {
		t.Error("方向不匹配应拒绝")
	}
	// 非绑定路径 → 拒;分片前缀 <Path>.ftpartNNNN → 过
	if s.verifyConsume("t1", sec, `C:\ws\other.bin`, "upload", false) {
		t.Error("路径不绑定应拒绝")
	}
	if !s.verifyConsume("t1", sec, `C:\ws\big.tar.ftpart0003`, "upload", false) {
		t.Error("分片前缀应放行")
	}
	// 一次性:consume 后再验 → 拒
	if !s.verifyConsume("t1", sec, `C:\ws\big.tar`, "upload", true) {
		t.Error("合法票据 consume 应通过")
	}
	if s.verifyConsume("t1", sec, `C:\ws\big.tar`, "upload", false) {
		t.Error("一次性:consume 后应拒绝")
	}

	// 过期票据 → 拒 + 清理
	s.Add(proto.XferTicketGrant{
		TicketID: "t2", SecretHash: ticketSecretHash(sec),
		Path: "p", Direction: "upload", ExpiryUnix: time.Now().Add(-time.Second).Unix(),
	})
	if s.verifyConsume("t2", sec, "p", "upload", false) {
		t.Error("过期票据应拒绝")
	}
	if _, ok := s.tickets["t2"]; ok {
		t.Error("过期票据应被清理")
	}

	// consume 失败也消费(防在线穷举)
	s.Add(proto.XferTicketGrant{
		TicketID: "t3", SecretHash: ticketSecretHash(sec),
		Path: "p", Direction: "upload", ExpiryUnix: time.Now().Add(time.Minute).Unix(),
	})
	_ = s.verifyConsume("t3", "wrong", "p", "upload", true)
	if _, ok := s.tickets["t3"]; ok {
		t.Error("consume 校验失败也应消费(防穷举重试)")
	}
}

// startTestDirectServer 起 TLS 直连服务器(绑定 transfers 引擎与临时 workspace)。
func startTestDirectServer(t *testing.T, ws string) *httptest.Server {
	t.Helper()
	origWorkspace := transfers.workspace
	transfers.SetWorkspace(ws)
	t.Cleanup(func() { transfers.SetWorkspace(origWorkspace) })

	xd := &xferDirectServer{dataTk: make(map[string]xferDataToken)}
	t.Cleanup(func() { transfers.CloseAll() }) // 兜底:悬空 session 句柄会卡 TempDir 清理
	// 策略隔离:其它测试(buildDispatcher)可能已把全局策略改成 fail-closed,
	// 直连 E2E 用默认放行,退出还原(拒绝场景由 TestXferDirectPolicyReject 自管)
	origPolicy := transfers.policy
	transfers.SetPolicy(allowAllPaths)
	t.Cleanup(func() { transfers.policy = origPolicy })
	mux := http.NewServeMux()
	mux.HandleFunc("POST /xfer/probe", xd.handleProbe)
	mux.HandleFunc("POST /xfer/begin", xd.handleBegin)
	mux.HandleFunc("POST /xfer/data", xd.handleData)
	mux.HandleFunc("POST /xfer/fetch", xd.handleFetch)
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

type directClient struct {
	base  string
	http  *http.Client
	token string // relay 签发的票据 secret
}

func (c *directClient) begin(t *testing.T, ticketID, path, direction string, extra map[string]any) map[string]any {
	t.Helper()
	body := map[string]any{"path": path, "direction": direction}
	for k, v := range extra {
		body[k] = v
	}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", c.base+"/xfer/begin", bytes.NewReader(b))
	req.Header.Set("X-Reins-Ticket", ticketID+":"+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatalf("begin 失败: %v", err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != 200 {
		t.Fatalf("begin HTTP %d: %v", resp.StatusCode, out)
	}
	return out
}

func TestXferDirectUploadE2E(t *testing.T) {
	ws := t.TempDir()
	srv := startTestDirectServer(t, ws)
	cli := &directClient{base: srv.URL, http: srv.Client()}

	content := bytes.Repeat([]byte("direct-upload-payload-"), 100_000) // ~2.2MB
	sum := sha256.Sum256(content)
	wantSHA := hex.EncodeToString(sum[:])
	target := filepath.Join(ws, "up.bin")

	// Relay 签发票据(hash 进 Agent)
	secret := "feedfacefeedfacefeedfacefeedfacefeedfacefeedfacefeedfacefeedface"
	xferTickets.Add(proto.XferTicketGrant{
		TicketID:   "e2e-up",
		SecretHash: ticketSecretHash(secret),
		Path:       target, Direction: "upload",
		ExpiryUnix: time.Now().Add(time.Minute).Unix(),
	})
	cli.token = secret

	ready := cli.begin(t, "e2e-up", target, "upload", map[string]any{
		"transfer_id": "dx-1", "size": int64(len(content)), "sha256": wantSHA,
	})
	if ready["accepted"] != true {
		t.Fatalf("begin 未接受: %v", ready)
	}
	if ready["resume_offset"].(float64) != 0 {
		t.Errorf("新文件 resume_offset 应为 0: %v", ready)
	}
	dataToken, _ := ready["data_token"].(string)
	if dataToken == "" {
		t.Fatal("begin 应签发 data_token")
	}

	// data:流式上传
	req, _ := http.NewRequest("POST", srv.URL+"/xfer/data?transfer_id=dx-1", bytes.NewReader(content))
	req.Header.Set("X-Reins-Data-Token", dataToken)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var result struct {
		TransferID string `json:"transfer_id"`
		Bytes      int64  `json:"bytes"`
		SHA256     string `json:"sha256"`
		Error      string `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&result)
	if resp.StatusCode != 200 {
		t.Fatalf("data HTTP %d: %s", resp.StatusCode, result.Error)
	}
	if result.SHA256 != wantSHA || result.Bytes != int64(len(content)) {
		t.Errorf("上传结果不符: bytes=%d sha=%s", result.Bytes, result.SHA256)
	}

	// 落盘逐位对拍
	got, _ := os.ReadFile(target)
	if !bytes.Equal(got, content) {
		t.Errorf("落盘内容不符: %d bytes", len(got))
	}
	// D:直连路径计量(成功传输入 direct 计数)
	if db, dc := transfers.SnapshotDirectStats(); dc < 1 || db < int64(len(content)) {
		t.Errorf("直连计量缺失: bytes=%d count=%d", db, dc)
	}
	// .part 已清理(rename 完成)
	if _, err := os.Stat(target + ".part"); !os.IsNotExist(err) {
		t.Errorf(".part 应已 rename: %v", err)
	}

	// data_token 一次性:重复 data 拒绝
	req2, _ := http.NewRequest("POST", srv.URL+"/xfer/data?transfer_id=dx-1", bytes.NewReader(content))
	req2.Header.Set("X-Reins-Data-Token", dataToken)
	resp2, _ := srv.Client().Do(req2)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Errorf("data_token 二次使用应 401,实际 %d", resp2.StatusCode)
	}

	// ALREADY_COMPLETE 快路径:构造 ".part 已满但正式文件不存在"(上次传输
	// 中断在 rename 前的形态)——begin 直接算 hash 校验 rename 返回,不建 session
	target2 := filepath.Join(ws, "up2.bin")
	os.WriteFile(target2+".part", content, 0644)
	xferTickets.Add(proto.XferTicketGrant{
		TicketID: "e2e-up2", SecretHash: ticketSecretHash(secret),
		Path: target2, Direction: "upload", ExpiryUnix: time.Now().Add(time.Minute).Unix(),
	})
	ready2 := cli.begin(t, "e2e-up2", target2, "upload", map[string]any{
		"transfer_id": "dx-2", "size": int64(len(content)), "sha256": wantSHA,
	})
	if ready2["already_complete"] != true || ready2["sha256"] != wantSHA {
		t.Errorf("已满 .part 应走 ALREADY_COMPLETE: %v", ready2)
	}
	if b, _ := os.ReadFile(target2); !bytes.Equal(b, content) {
		t.Errorf("快路径应完成 rename: %d bytes", len(b))
	}
}

func TestXferDirectDownloadE2E(t *testing.T) {
	ws := t.TempDir()
	srv := startTestDirectServer(t, ws)
	cli := &directClient{base: srv.URL, http: srv.Client()}

	content := bytes.Repeat([]byte("DL-DIRECT-"), 200_000) // ~2MB
	sum := sha256.Sum256(content)
	wantSHA := hex.EncodeToString(sum[:])
	src := filepath.Join(ws, "down.bin")
	os.WriteFile(src, content, 0644)

	secret := "cafebabecafebabecafebabecafebabecafebabecafebabecafebabecafebabe"
	xferTickets.Add(proto.XferTicketGrant{
		TicketID: "e2e-dl", SecretHash: ticketSecretHash(secret),
		Path: src, Direction: "download", ExpiryUnix: time.Now().Add(time.Minute).Unix(),
	})
	cli.token = secret

	ready := cli.begin(t, "e2e-dl", src, "download", map[string]any{"transfer_id": "dx-dl-1"})
	if ready["accepted"] != true {
		t.Fatalf("begin 未接受: %v", ready)
	}
	if int64(ready["size"].(float64)) != int64(len(content)) {
		t.Errorf("download ready 应回填文件大小: %v", ready)
	}

	req, _ := http.NewRequest("POST", srv.URL+"/xfer/fetch?transfer_id=dx-dl-1", nil)
	req.Header.Set("X-Reins-Data-Token", ready["data_token"].(string))
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("下载内容不符: %d bytes want %d", len(got), len(content))
	}
	// trailer 携带 SHA256(httptest 走真实 HTTP/1.1 chunked,trailer 可读)
	if resp.Trailer.Get("X-Xfer-Sha256") != wantSHA {
		t.Errorf("trailer hash 不符: got %q want %q", resp.Trailer.Get("X-Xfer-Sha256"), wantSHA)
	}
}

func TestXferDirectPolicyReject(t *testing.T) {
	ws := t.TempDir()
	srv := startTestDirectServer(t, ws)

	// 注入拒绝一切的路径策略(begin 会走 HandleInit 的策略校验)
	orig := transfers.policy
	transfers.SetPolicy(func(string) error { return fmt.Errorf("测试拒绝") })
	t.Cleanup(func() { transfers.policy = orig })

	secret := "abcdef01abcdef01abcdef01abcdef01abcdef01abcdef01abcdef01abcdef01"
	xferTickets.Add(proto.XferTicketGrant{
		TicketID: "e2e-deny", SecretHash: ticketSecretHash(secret),
		Path: filepath.Join(ws, "x.bin"), Direction: "upload",
		ExpiryUnix: time.Now().Add(time.Minute).Unix(),
	})

	body, _ := json.Marshal(map[string]any{"path": filepath.Join(ws, "x.bin"), "direction": "upload"})
	req, _ := http.NewRequest("POST", srv.URL+"/xfer/begin", bytes.NewReader(body))
	req.Header.Set("X-Reins-Ticket", "e2e-deny:"+secret)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("策略拒绝应 403,实际 %d", resp.StatusCode)
	}
}

func TestAdvertiseAddrs(t *testing.T) {
	// 显式覆盖优先
	got := advertiseAddrs(":7783", []string{"10.1.2.3:7783"})
	if len(got) != 1 || got[0] != "10.1.2.3:7783" {
		t.Errorf("显式覆盖应原样返回: %v", got)
	}
	// 自动枚举:至少包含回环外地址形态 host:port(CI 环境网卡不定,只验形态)
	got2 := advertiseAddrs("127.0.0.1:0", nil)
	for _, a := range got2 {
		if _, _, err := splitHostPort(a); err != nil {
			t.Errorf("地址形态非法: %q (%v)", a, err)
		}
	}
	_ = tls.VersionTLS12 // 保持 import 引用
}

func splitHostPort(s string) (string, int, error) {
	var host, port string
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == ':' {
			host, port = s[:i], s[i+1:]
			var p int
			if _, err := fmt.Sscanf(port, "%d", &p); err != nil {
				return "", 0, err
			}
			return host, p, nil
		}
	}
	return "", 0, fmt.Errorf("无端口")
}
