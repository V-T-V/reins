package main

// v4.7.1 自动更新链路 Relay 侧测试:
//   1. /api/updates/latest:数值最新/空仓库/鉴权
//   2. /api/updates/download:404/流式内容+sha 头
//   3. Save 拒收 dev 版本
//   4. apply download 模式:有 update_dl 能力 → 任务带 download_url 零内联
//   5. apply 能力门:旧 Agent(无 update_dl)→ 自动降级 legacy 内联
//   6. relayDownloadURL 推导:ExternalURL 优先/反代头尊重

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/V-T-V/reins/proto"
)

func newUpdateTestRelay(t *testing.T) (*Relay, *httptest.Server, *UpdateRepo) {
	t.Helper()
	relay := NewRelay(Config{Dev: true, Addr: ":0"})
	repo, err := NewUpdateRepo(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	relay.WithUpdateRepo(repo)
	mux := http.NewServeMux()
	relay.registerHTTP(mux)
	mux.HandleFunc("/agent", relay.handleAgentWS)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return relay, srv, repo
}

func TestUpdateLatestEndpoint(t *testing.T) {
	_, srv, repo := newUpdateTestRelay(t)
	// 存 v2.0.0 与 v10.0.0(数值感知:v10 才是最新)
	if _, _, err := repo.Save("v2.0.0", strings.NewReader("bin-v2")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := repo.Save("v10.0.0", strings.NewReader("bin-v10")); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(srv.URL + "/api/updates/latest")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Latest struct {
			Version string `json:"version"`
			SHA256  string `json:"sha256"`
		} `json:"latest"`
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if !out.Enabled || out.Latest.Version != "v10.0.0" {
		t.Errorf("latest 应为 v10.0.0(数值感知): %+v", out)
	}
	if len(out.Latest.SHA256) != 64 {
		t.Errorf("latest 应回填 sha256")
	}
}

func TestUpdateLatestEmptyRepo(t *testing.T) {
	_, srv, _ := newUpdateTestRelay(t)
	resp, err := http.Get(srv.URL + "/api/updates/latest")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Latest  any  `json:"latest"`
		Enabled bool `json:"enabled"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out.Latest != nil {
		t.Errorf("空仓库 latest 应为 null: %v", out.Latest)
	}
}

func TestUpdateDownloadEndpoint(t *testing.T) {
	_, srv, repo := newUpdateTestRelay(t)
	content := "fake-agent-binary-内容-for-download-test"
	if _, sha, err := repo.Save("v1.2.3", strings.NewReader(content)); err != nil || sha == "" {
		t.Fatal(err, sha)
	}

	// 未知版本 404
	resp, err := http.Get(srv.URL + "/api/updates/download?version=nosuch")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("未知版本应 404,实际 %d", resp.StatusCode)
	}

	// 正常下载:内容一致 + sha 头
	resp2, err := http.Get(srv.URL + "/api/updates/download?version=v1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("期望 200,实际 %d", resp2.StatusCode)
	}
	b, _ := io.ReadAll(resp2.Body)
	if string(b) != content {
		t.Errorf("下载内容不符: %q", string(b)[:min(30, len(b))])
	}
	if resp2.Header.Get("X-Xfer-SHA256") == "" {
		t.Error("应回传 X-Xfer-SHA256 头")
	}
	if resp2.Header.Get("Content-Length") != fmtInt(int64(len(content))) {
		t.Errorf("Content-Length 应精确: %s", resp2.Header.Get("Content-Length"))
	}
}

func fmtInt(n int64) string { return jsonInt(n) }
func jsonInt(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestUpdateSaveRejectsDev(t *testing.T) {
	_, _, repo := newUpdateTestRelay(t)
	if _, _, err := repo.Save("dev", strings.NewReader("x")); err == nil {
		t.Error("dev 版本应被拒收")
	}
	// 自动生成时间戳版本仍允许(空 version)
	if _, _, err := repo.Save("", strings.NewReader("x")); err != nil {
		t.Errorf("自动生成版本应允许: %v", err)
	}
}

// updatePayloadSink 线程安全收集 fake Agent 收到的 update 任务载荷。
type updatePayloadSink struct {
	mu sync.Mutex
	m  map[string]any
}

func (s *updatePayloadSink) store(p map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range p {
		s.m[k] = v
	}
}

func (s *updatePayloadSink) get(key string) (any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[key]
	return v, ok
}

// startUpdateCapAgent 起 fake Agent WS 并注册,收到 update 任务载荷存入 sink。
func startUpdateCapAgent(t *testing.T, srv *httptest.Server, agentID string, caps []string, sink *updatePayloadSink) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	conn, _, err := websocket.Dial(ctx, strings.Replace(srv.URL+"/agent", "http://", "ws://", 1), nil) //nolint:bodyclose
	if err != nil {
		t.Fatalf("agent 连接失败: %v", err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	conn.SetReadLimit(64 << 20)

	regEnv, _ := proto.NewEnvelope(proto.TypeRegister, agentID, "relay", proto.Register{
		AgentID: agentID, Hostname: "h", OS: "linux/amd64", Capabilities: caps,
	})
	if err := conn.Write(ctx, websocket.MessageText, mustJSON(regEnv)); err != nil {
		t.Fatal(err)
	}

	go func() {
		for {
			mt, data, rerr := conn.Read(ctx)
			if rerr != nil {
				return
			}
			if mt != websocket.MessageText {
				continue
			}
			var env proto.Envelope
			if json.Unmarshal(data, &env) != nil || env.Type != proto.TypeTaskRequest {
				continue
			}
			var tr proto.TaskRequest
			if env.Decode(&tr) != nil || tr.Type != proto.TaskUpdate {
				continue
			}
			var p map[string]any
			if json.Unmarshal(tr.Payload, &p) == nil {
				sink.store(p)
			}
		}
	}()
}

func TestUpdateApplyDownloadMode(t *testing.T) {
	relay, srv, repo := newUpdateTestRelay(t)
	if _, _, err := repo.Save("v4.7.1", strings.NewReader("new-agent-bin")); err != nil {
		t.Fatal(err)
	}
	_ = relay

	got := &updatePayloadSink{m: map[string]any{}}
	startUpdateCapAgent(t, srv, "dl-agent", []string{"update", "update_dl"}, got)
	waitFor(t, func() bool { return len(relay.agents.Snapshot()) > 0 })

	resp, err := http.Post(srv.URL+"/api/updates/apply", "application/json",
		strings.NewReader(`{"agent_id":"dl-agent","version":"v4.7.1","mode":"download"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("apply 应 202,实际 %d", resp.StatusCode)
	}

	waitFor(t, func() bool { _, ok := got.get("download_url"); return ok })
	if u, _ := got.get("download_url"); !strings.Contains(u.(string), "/api/updates/download?version=v4.7.1") {
		t.Errorf("download_url 应指向版本仓库端点: %v", u)
	}
	if _, hasB64 := got.get("binary_b64"); hasB64 {
		t.Error("download 模式不应内联 binary_b64")
	}
	if v, _ := got.get("version"); v != "v4.7.1" {
		t.Errorf("version 透传失败: %v", v)
	}
}

func TestUpdateApplyDownloadLegacyFallback(t *testing.T) {
	relay, srv, repo := newUpdateTestRelay(t)
	if _, _, err := repo.Save("v4.7.1", strings.NewReader("legacy-bin-content")); err != nil {
		t.Fatal(err)
	}

	// 旧 Agent:无 update_dl 能力
	got := &updatePayloadSink{m: map[string]any{}}
	startUpdateCapAgent(t, srv, "old-agent", []string{"update"}, got)
	waitFor(t, func() bool { return len(relay.agents.Snapshot()) > 0 })

	resp, err := http.Post(srv.URL+"/api/updates/apply", "application/json",
		strings.NewReader(`{"agent_id":"old-agent","version":"v4.7.1","mode":"download"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("apply 应 202,实际 %d", resp.StatusCode)
	}

	// 能力门:降级 legacy 内联(与旧版行为一致,滚动升级安全)
	waitFor(t, func() bool { _, ok := got.get("binary_b64"); return ok })
	if _, has := got.get("download_url"); has {
		t.Errorf("旧 Agent 不应收 download_url")
	}
	if b64, _ := got.get("binary_b64"); b64.(string) == "" {
		t.Error("降级路径应携带内联 binary_b64")
	}
}

func TestRelayDownloadURLDerivation(t *testing.T) {
	// ExternalURL 优先
	r := &Relay{cfg: Config{ExternalURL: "https://relay.example.com/rmm"}}
	req, _ := http.NewRequest("POST", "http://127.0.0.1:7780/api/updates/apply", nil)
	if got := r.relayDownloadURL(req, "v1.0.0"); got != "https://relay.example.com/rmm/api/updates/download?version=v1.0.0" {
		t.Errorf("ExternalURL 推导不符: %s", got)
	}
	// 反代头尊重
	r2 := &Relay{cfg: Config{}}
	req2, _ := http.NewRequest("POST", "http://127.0.0.1:7780/api/updates/apply", nil)
	req2.Header.Set("X-Forwarded-Proto", "https")
	req2.Header.Set("X-Forwarded-Host", "203.0.113.99")
	if got := r2.relayDownloadURL(req2, "v2.0.0"); !strings.HasPrefix(got, "https://203.0.113.99/") {
		t.Errorf("反代头推导不符: %s", got)
	}
	// 裸直连:http + req.Host
	req3, _ := http.NewRequest("POST", "http://10.1.2.3:7780/x", nil)
	if got := r2.relayDownloadURL(req3, "v3.0.0"); !strings.HasPrefix(got, "http://10.1.2.3:7780/") {
		t.Errorf("直连推导不符: %s", got)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("等待条件超时")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
