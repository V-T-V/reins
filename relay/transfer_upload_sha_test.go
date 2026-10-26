package main

// P2-11 per-chunk SHA256 透传 E2E:
//
// POST /api/transfer/upload?sha256=<hash> 的 hash 必须原样进入发给 Agent 的
// file_xfer_init(Agent 侧 init.SHA256 → expectedSHA256 → 收完单片比对)。
// 该链路自 J2 stream-through 存在但一直无测试覆盖;分片上传接入后成为
// 单片损坏当场拒绝的唯一通道,此处用真 WS fake Agent 全链路验证:
//   1. fake Agent 收 init → 断言 SHA256 透传 → 回 ready
//   2. Relay 转发二进制帧 → Agent 累积 → isLast 算 hash 回 complete
//   3. HTTP 200 + 响应 sha256 == 本地内容 hash

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/V-T-V/reins/proto"
)

func TestTransferUploadSHA256PassthroughE2E(t *testing.T) {
	relay := NewRelay(Config{Dev: true, Addr: ":0"})
	mux := http.NewServeMux()
	relay.registerHTTP(mux)
	mux.HandleFunc("/agent", relay.handleAgentWS)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	agentURL := strings.Replace(srv.URL+"/agent", "http://", "ws://", 1)

	content := []byte("chunk payload for sha passthrough e2e test")
	contentSum := sha256.Sum256(content)
	wantSHA := hex.EncodeToString(contentSum[:])

	// fake Agent 状态
	var (
		mu         sync.Mutex
		gotInitSHA string
		gotBytes   []byte
	)
	conn, _, err := websocket.Dial(ctx, agentURL, nil) //nolint:bodyclose // 成功时 resp.Body 为 nil
	if err != nil {
		t.Fatalf("agent 连接失败: %v", err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 20)

	regEnv, _ := proto.NewEnvelope(proto.TypeRegister, "sha-agent", "relay", proto.Register{
		AgentID: "sha-agent", Hostname: "sha-host", OS: "linux/amd64",
		Capabilities: []string{"file"},
	})
	if err := conn.Write(ctx, websocket.MessageText, mustJSON(regEnv)); err != nil {
		t.Fatal(err)
	}

	// 读循环:处理 init(断言 SHA256) + 二进制帧(累积, isLast 回 complete)
	go func() {
		for {
			mt, data, rerr := conn.Read(ctx)
			if rerr != nil {
				return
			}
			switch mt {
			case websocket.MessageText:
				var env proto.Envelope
				if json.Unmarshal(data, &env) != nil || env.Type != proto.TypeFileTransferInit {
					continue
				}
				var init proto.FileTransferInit
				if err := env.Decode(&init); err != nil {
					continue
				}
				mu.Lock()
				gotInitSHA = init.SHA256
				mu.Unlock()
				readyEnv, _ := proto.NewEnvelope(proto.TypeFileTransferReady, "sha-agent", "relay",
					proto.FileTransferReady{TransferID: init.TransferID, Accepted: true})
				_ = conn.Write(ctx, websocket.MessageText, mustJSON(readyEnv))
			case websocket.MessageBinary:
				transferID, isLast, payload, perr := proto.ParseBinaryFrame(data)
				if perr != nil {
					return
				}
				mu.Lock()
				gotBytes = append(gotBytes, payload...)
				buf := append([]byte{}, gotBytes...)
				mu.Unlock()
				if !isLast {
					continue
				}
				sum := sha256.Sum256(buf)
				compEnv, _ := proto.NewEnvelope(proto.TypeFileTransferComplete, "sha-agent", "relay",
					proto.FileTransferComplete{
						TransferID: transferID, Success: true,
						SHA256Actual: hex.EncodeToString(sum[:]),
						Bytes:        int64(len(buf)),
					})
				_ = conn.Write(ctx, websocket.MessageText, mustJSON(compEnv))
			}
		}
	}()

	// 等 agent 上线
	deadline := time.Now().Add(3 * time.Second)
	for len(relay.agents.Snapshot()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("agent 未上线")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// 发起上传:query 携带 sha256(per-chunk 模式下单片即整文件)
	q := "agent_id=sha-agent&path=ws%2Fblob.bin&transfer_id=sha-e2e-1&size=" +
		strconv.FormatInt(int64(len(content)), 10) + "&sha256=" + wantSHA
	resp, err := http.Post(srv.URL+"/api/transfer/upload?"+q, "application/octet-stream",
		strings.NewReader(string(content)))
	if err != nil {
		t.Fatalf("上传请求失败: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("期望 200,实际 %d: %s", resp.StatusCode, string(body))
	}
	var result struct {
		TransferID string `json:"transfer_id"`
		Bytes      int64  `json:"bytes"`
		SHA256     string `json:"sha256"`
		Status     string `json:"status"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("解析响应失败: %v (body=%s)", err, string(body))
	}

	mu.Lock()
	defer mu.Unlock()
	// 1. init.SHA256 透传断言(核心:CLI query → Relay → Agent init)
	if gotInitSHA != wantSHA {
		t.Errorf("init.SHA256 未透传: got %q want %q", gotInitSHA, wantSHA)
	}
	// 2. Agent 收到完整内容
	if string(gotBytes) != string(content) {
		t.Errorf("Agent 收到内容不完整: got %d bytes want %d", len(gotBytes), len(content))
	}
	// 3. 响应回传 Agent 计算的 hash
	if result.SHA256 != wantSHA {
		t.Errorf("响应 sha256 不匹配: got %s want %s", result.SHA256, wantSHA)
	}
	if result.Status != "uploaded" {
		t.Errorf("status 应为 uploaded,实际 %q", result.Status)
	}
}
