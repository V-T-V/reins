package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QiuShichang/reins/proto"
)

// TestRegistryOnlineSince OnlineSince 补齐注册时刻(原列表端点漏填,CLI 恒显"—")。
func TestRegistryOnlineSince(t *testing.T) {
	r := NewRegistry()
	if got := r.OnlineSince("nope"); got != 0 {
		t.Fatalf("不在线的 Agent 应返回 0,实际 %d", got)
	}
	before := time.Now().UnixMilli()
	r.Register(proto.Register{AgentID: "a-1"}, nil)
	got := r.OnlineSince("a-1")
	if got < before {
		t.Fatalf("online_since 应不早于注册时刻: %d < %d", got, before)
	}
	if got == 0 {
		t.Fatal("在线 Agent 的 online_since 不应为 0")
	}
}

// TestHandleListAgentsOnlineSince 端点层:/api/agents 响应含非零 online_since。
func TestHandleListAgentsOnlineSince(t *testing.T) {
	relay := NewRelay(Config{Token: "t"})
	relay.agents.Register(proto.Register{AgentID: "a-1"}, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/agents", nil)
	req.Header.Set("Authorization", "Bearer t")
	rec := httptest.NewRecorder()
	relay.handleListAgents(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `"online_since"`) {
		t.Fatalf("响应应含 online_since 字段: %s", body)
	}
	if strings.Contains(body, `"online_since":0`) {
		t.Fatalf("在线 Agent 的 online_since 不应为 0: %s", body)
	}
}
