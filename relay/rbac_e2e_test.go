package main

// RBAC 端到端测试(架构短1:验证 RequireRole/CanPerform 从"定义"变"执行")。
// 覆盖:viewer 下发 power 任务被 403;admin 放行;错误 secret 401;旧格式纯 secret 兼容。

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QiuShichang/go-rmm/proto"
)

// newRBACTestRelay 构造带 tenant 格式 token 的 relay(服务端 admin 格式)。
func newRBACTestRelay(t *testing.T) (*Relay, *httptest.Server) {
	t.Helper()
	store := NewMemoryStore()
	r := NewRelay(Config{Addr: ":0", Token: "acme:admin:topsecret"}).WithStore(store)
	mux := http.NewServeMux()
	r.registerHTTP(mux)
	srv := httptest.NewServer(mux)
	return r, srv
}

func postTaskAs(t *testing.T, srv *httptest.Server, token string, taskType string) int {
	t.Helper()
	body, _ := json.Marshal(proto.TaskRequest{
		TaskID:  "rbac-" + taskType,
		Type:    taskType,
		AgentID: "agent-x",
		Payload: json.RawMessage(`{"action":"restart"}`),
	})
	req, _ := http.NewRequest("POST", srv.URL+"/api/tasks", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestRBACViewerForbiddenForPowerTask(t *testing.T) {
	_, srv := newRBACTestRelay(t)
	defer srv.Close()

	// viewer(同 secret):鉴权通过但 power 任务被 403
	if code := postTaskAs(t, srv, "acme:viewer:topsecret", proto.TaskPower); code != http.StatusForbidden {
		t.Errorf("viewer 下发 power 应 403, got %d", code)
	}
	// viewer 下发 update 同样 403
	if code := postTaskAs(t, srv, "acme:viewer:topsecret", proto.TaskUpdate); code != http.StatusForbidden {
		t.Errorf("viewer 下发 update 应 403, got %d", code)
	}
	// admin:放行(可能 400 因 agent 离线/不支持,但绝不该是 403)
	if code := postTaskAs(t, srv, "acme:admin:topsecret", proto.TaskPower); code == http.StatusForbidden {
		t.Errorf("admin 下发 power 不应 403, got %d", code)
	}
	// 普通任务(powershell)viewer 也被 403?——viewer 只读,无 task 提交权(canPerform 无该 action)
	// 注意:当前任务提交的 action 粒度是 power/update 特判,powershell 不在 viewerAllowed。
	// 本测试只断言高危类型;普通类型的 operator/viewer 区分属后续演进。
}

func TestRBACOperatorForbiddenForEnrollToken(t *testing.T) {
	_, srv := newRBACTestRelay(t)
	defer srv.Close()

	body, _ := json.Marshal(map[string]string{"agent_id": "a1"})
	doPost := func(token string) int {
		req, _ := http.NewRequest("POST", srv.URL+"/api/enroll-token", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("请求失败: %v", err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	// operator 被 RequireRole 拒 403(可能先因 CA 未启用 503?—— RequireRole 在 handler 前执行,应 403)
	if code := doPost("acme:operator:topsecret"); code != http.StatusForbidden {
		t.Errorf("operator 生成 enroll-token 应 403, got %d", code)
	}
}

func TestRBACWrongSecretUnauthorized(t *testing.T) {
	_, srv := newRBACTestRelay(t)
	defer srv.Close()
	// 错误 secret:401(鉴权层拒绝,非 403)
	if code := postTaskAs(t, srv, "acme:admin:WRONG", proto.TaskPower); code != http.StatusUnauthorized {
		t.Errorf("错误 secret 应 401, got %d", code)
	}
}

func TestRBACLegacyPlainTokenStillWorks(t *testing.T) {
	// 服务端旧格式纯 secret:客户端同格式可鉴权(向后兼容)
	store := NewMemoryStore()
	r := NewRelay(Config{Addr: ":0", Token: "plain-secret"}).WithStore(store)
	mux := http.NewServeMux()
	r.registerHTTP(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	if code := postTaskAs(t, srv, "plain-secret", proto.TaskPower); code == http.StatusUnauthorized {
		t.Errorf("旧格式纯 secret 应鉴权通过(ParseToken→Secret=全串,行为不变), got 401")
	}
}
