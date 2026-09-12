// Package main — scheduler_smart.go 单元测试(智能调度)。
//
// 覆盖:
//   - matchTagAny 的 key=value / 裸 value / nil map / 不匹配 四态
//   - SelectBestAgent:空注册表、能力过滤、标签过滤、在途负载均衡、唯一候选
//   - getInflightCount:store 为 nil 与正常两条路径
//   - GET /api/scheduler/best 端点(200 推荐 / 404 无匹配)
package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/QiuShichang/go-rmm/proto"
)

// TestMatchTagAnyExtra —— round9_secrets_test 已覆盖基本四态;
// 此处补边界:跨 key 裸 value / 只有 key / 空 map / 空 filter×空 value。
func TestMatchTagAnyExtra(t *testing.T) {
	tags := map[string]string{"env": "prod", "region": "us-east"}
	if !matchTagAny(tags, "us-east") {
		t.Fatal("bare value across keys should match")
	}
	if matchTagAny(tags, "env") {
		t.Fatal("key alone must not match")
	}
	if matchTagAny(map[string]string{}, "x") {
		t.Fatal("empty map must not match")
	}
	if !matchTagAny(map[string]string{"a": ""}, "") {
		t.Fatal("empty filter matches empty value (k+\"=\"+v == \"\")")
	}
}

// smartRelay 构造带 Registry+store 的最小 Relay(不走 HTTP)。
func smartRelay() *Relay {
	return &Relay{
		cfg:    Config{Addr: ":0"},
		agents: NewRegistry(),
		store:  NewMemoryStore(),
	}
}

func regAgent(r *Relay, id string, caps []string, tags map[string]string) {
	r.agents.Register(proto.Register{
		AgentID: id, Hostname: id, OS: "windows/amd64", Version: "v4.4.0",
		Capabilities: caps, Tags: tags,
	}, nil)
}

func TestSelectBestAgentTagFilter(t *testing.T) {
	r := smartRelay()
	regAgent(r, "a1", []string{"powershell"}, map[string]string{"env": "prod"})
	regAgent(r, "a2", []string{"powershell"}, map[string]string{"env": "staging"})

	got, err := r.SelectBestAgent("env=prod", "powershell")
	if err != nil {
		t.Fatal(err)
	}
	if got != "a1" {
		t.Fatalf("want a1 (env=prod), got %s", got)
	}
	// 裸 value 过滤。
	got, err = r.SelectBestAgent("staging", "")
	if err != nil || got != "a2" {
		t.Fatalf("bare value filter: got=%s err=%v, want a2", got, err)
	}
	// 标签无人匹配 → 错误信息含 tag/type。
	_, err = r.SelectBestAgent("env=dev", "")
	if err == nil || !strings.Contains(err.Error(), "env=dev") {
		t.Fatalf("no-match error should mention filter: %v", err)
	}
}

func TestSelectBestAgentLoadBalance(t *testing.T) {
	r := smartRelay()
	regAgent(r, "busy", []string{"powershell"}, nil)
	regAgent(r, "idle", []string{"powershell"}, nil)

	// busy 压 3 个在途任务 → score = 3*10 = 30,idle = 0,必须选 idle。
	for i := 0; i < 3; i++ {
		r.agents.IncInFlight("busy")
	}
	got, err := r.SelectBestAgent("", "powershell")
	if err != nil {
		t.Fatal(err)
	}
	if got != "idle" {
		t.Fatalf("load balance: want idle (inflight 0), got %s (busy inflight=%d)",
			got, r.agents.InFlight("busy"))
	}
	// 两个都空闲时,任意一个都是合法结果(排序稳定性依赖 Snapshot 顺序,不锚定)。
	r.agents.DecInFlight("busy")
	r.agents.DecInFlight("busy")
	r.agents.DecInFlight("busy")
	got, err = r.SelectBestAgent("", "powershell")
	if err != nil || (got != "busy" && got != "idle") {
		t.Fatalf("both idle: got=%s err=%v", got, err)
	}
}

func TestGetInflightCountNilStore(t *testing.T) {
	r := &Relay{agents: NewRegistry()} // store 为 nil
	regAgent(r, "a", nil, nil)
	if n := r.getInflightCount("a"); n != 0 {
		t.Fatalf("nil store: want 0, got %d", n)
	}
	r2 := smartRelay()
	regAgent(r2, "a", nil, nil)
	r2.agents.IncInFlight("a")
	r2.agents.IncInFlight("a")
	if n := r2.getInflightCount("a"); n != 2 {
		t.Fatalf("want 2, got %d", n)
	}
	// 不存在的 Agent → 0。
	if n := r2.getInflightCount("ghost"); n != 0 {
		t.Fatalf("ghost agent: want 0, got %d", n)
	}
}

func TestHandleSelectBestEndpoint(t *testing.T) {
	r, srv := newTestRelay(t)
	defer srv.Close()
	registerMockAgent(r, "best-1", []string{"powershell"})

	// 命中:返回推荐字段。
	resp, err := http.Get(srv.URL + "/api/scheduler/best?type=powershell")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var body struct {
		AgentID string `json:"agent_id"`
		Tag     string `json:"tag"`
		Type    string `json:"type"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.AgentID != "best-1" || body.Type != "powershell" {
		t.Fatalf("body: %+v", body)
	}
	if body.Message == "" {
		t.Fatal("message should be present")
	}

	// 无匹配(能力不支持)→ 404。
	resp2, err := http.Get(srv.URL + "/api/scheduler/best?type=browse")
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("no match: want 404, got %d", resp2.StatusCode)
	}
}
