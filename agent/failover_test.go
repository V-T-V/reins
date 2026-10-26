package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/QiuShichang/reins/executors"
	"github.com/QiuShichang/reins/proto"
)

// TestBuildRelayCandidates W1 候选列表构造:主 URL 优先 + 备用去重 + 全空。
func TestBuildRelayCandidates(t *testing.T) {
	got := buildRelayCandidates("ws://a/agent", []string{"ws://b/agent", "ws://a/agent", "ws://c/agent"})
	want := []string{"ws://a/agent", "ws://b/agent", "ws://c/agent"}
	if len(got) != len(want) {
		t.Fatalf("候选数不符: %v (want %v)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("候选顺序/去重不符: %v (want %v)", got, want)
		}
	}
	if got := buildRelayCandidates("", nil); len(got) != 0 {
		t.Fatalf("全空应返回空: %v", got)
	}
	if got := buildRelayCandidates("", []string{"ws://b/agent"}); len(got) != 1 || got[0] != "ws://b/agent" {
		t.Fatalf("仅备用时应返回备用: %v", got)
	}
}

// fakeRelay 极简 Relay:接受 Agent WS 连接,记录 Register(agent 包无法
// import relay 包——故障切换 E2E 只需 Register 可观测,无需任务路由)。
type fakeRelay struct {
	srv   *httptest.Server
	wsURL string
	regCh chan string

	mu    sync.Mutex
	conns []*websocket.Conn
}

func newFakeRelay(t *testing.T) *fakeRelay {
	t.Helper()
	fr := &fakeRelay{regCh: make(chan string, 8)}
	mux := http.NewServeMux()
	mux.HandleFunc("/agent", func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		fr.mu.Lock()
		fr.conns = append(fr.conns, c)
		fr.mu.Unlock()
		defer c.CloseNow()
		c.SetReadLimit(1 << 20)
		for {
			_, data, err := c.Read(r.Context())
			if err != nil {
				return
			}
			var env proto.Envelope
			if json.Unmarshal(data, &env) == nil && env.Type == proto.TypeRegister {
				select {
				case fr.regCh <- env.From:
				default:
				}
			}
		}
	})
	fr.srv = httptest.NewServer(mux)
	fr.wsURL = "ws://" + strings.TrimPrefix(fr.srv.URL, "http://") + "/agent"
	t.Cleanup(fr.srv.Close)
	return fr
}

// killConns 立即断开全部 WS 连接(模拟 Relay 宕机,保留 HTTP server 以便
// 观察是否有错误重连——Agent 应转而切向备用)。
func (f *fakeRelay) killConns() {
	f.mu.Lock()
	for _, c := range f.conns {
		c.CloseNow()
	}
	f.conns = nil
	f.mu.Unlock()
}

// waitRegister 等待下一个 Register 帧(带超时)。
func (f *fakeRelay) waitRegister(t *testing.T, timeout time.Duration) string {
	t.Helper()
	select {
	case id := <-f.regCh:
		return id
	case <-time.After(timeout):
		t.Fatalf("等待 Register 超时(%v)", timeout)
		return ""
	}
}

// TestAgentFailoverBetweenRelays W1 故障切换 E2E(P0-2 水平扩展核心):
// Agent 主连 relay1 → relay1 宕机 → 1s 级退避后自动切换 relay2 并重新注册。
func TestAgentFailoverBetweenRelays(t *testing.T) {
	r1 := newFakeRelay(t)
	r2 := newFakeRelay(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	agentID := "failover-test-agent"
	cfg := Config{
		AgentID:   agentID,
		RelayURL:  r1.wsURL,
		RelayURLs: []string{r2.wsURL},
		Heartbeat: 15 * time.Second, // watchdog(2×心跳)不触发,断线由连接关闭即时感知
	}
	a := NewAgent(cfg, executors.New()) // 空 Dispatcher:无任务下发,仅连接/注册路径
	go func() { _ = a.Run(ctx) }()

	// 阶段 1:连上主 Relay 并注册
	if id := r1.waitRegister(t, 5*time.Second); id != agentID {
		t.Fatalf("relay1 收到的 agentID 不符: %s", id)
	}

	// 阶段 2:主 Relay 宕机
	r1.killConns()

	// 阶段 3:W1 切换——initialBackoff 1s + full jitter,5s 内应切到 relay2
	switched := make(chan string, 1)
	go func() { switched <- r2.waitRegister(t, 10*time.Second) }()
	select {
	case id := <-switched:
		if id != agentID {
			t.Fatalf("relay2 收到的 agentID 不符: %s", id)
		}
	case <-time.After(12 * time.Second):
		t.Fatal("Agent 未在 12s 内切换到备用 Relay(W1 故障切换失败)")
	}

	// 阶段 4:切换后连接保持稳定(无抖动循环)——2s 内 relay2 不应再收到重复 Register
	select {
	case id := <-r2.regCh:
		t.Fatalf("切换后不应立即重复注册(抖动?): %s", id)
	case <-time.After(2 * time.Second):
	}
}
