package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QiuShichang/reins/internal/taskstore"
	"github.com/QiuShichang/reins/proto"
)

// newDAGTestServer 构造带 DAG 路由的测试服务器。
func newDAGTestServer(relay *Relay) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/dag", relay.handleDAGSubmit)
	mux.HandleFunc("GET /api/dag", relay.handleDAGList)
	mux.HandleFunc("GET /api/dag/{id}", relay.handleDAGStatus)
	return httptest.NewServer(mux)
}

func dagDo(t *testing.T, srv *httptest.Server, method, path, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer t")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	if method == "POST" || resp.StatusCode == http.StatusOK {
		_ = json.NewDecoder(resp.Body).Decode(&out)
	}
	return resp.StatusCode, out
}

// TestDAGSubmitValidation 提交期校验:空 nodes/悬空依赖/依赖环逐一拒绝;
// 未带鉴权 401;未知编排 404。
func TestDAGSubmitValidation(t *testing.T) {
	relay := NewRelay(Config{Token: "t"})
	srv := newDAGTestServer(relay)
	defer srv.Close()

	cases := []struct {
		name string
		body string
		code int
	}{
		{"空节点", `{"name":"x","nodes":[]}`, http.StatusBadRequest},
		{"悬空依赖", `{"nodes":[{"id":"a","deps":["ghost"]}]}`, http.StatusBadRequest},
		{"依赖环", `{"nodes":[{"id":"a","deps":["b"]},{"id":"b","deps":["a"]}]}`, http.StatusBadRequest},
		{"自依赖", `{"nodes":[{"id":"a","deps":["a"]}]}`, http.StatusBadRequest},
		{"非法类型", `{"nodes":[{"id":"a","tag":"env=prod","type":"no-such"}]}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		code, _ := dagDo(t, srv, "POST", "/api/dag", tc.body)
		if code != tc.code {
			t.Fatalf("%s 应 %d,实际 %d", tc.name, tc.code, code)
		}
	}

	// 未鉴权 401
	req, _ := http.NewRequest("POST", srv.URL+"/api/dag", strings.NewReader(`{"nodes":[{"id":"a"}]}`))
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("未鉴权应 401,实际 %d", resp.StatusCode)
	}

	// 未知编排 404
	if code, _ := dagDo(t, srv, "GET", "/api/dag/dag-none", ""); code != http.StatusNotFound {
		t.Fatalf("未知编排应 404,实际 %d", code)
	}
}

// TestDAGEndpointE2EFailCascade 端到端确定性剧场:3 节点 DAG(串行)
//
//	prep(纯占位,立即成功) → deploy(agent 离线,投递失败) → verify(级联 skipped)
//
// 断言:终态 1 成功/1 失败/1 跳过;verify 从未启动(start_order 不含)且
// 未投递任何任务;列表端点可见该编排。
func TestDAGEndpointE2EFailCascade(t *testing.T) {
	relay := NewRelay(Config{Token: "t"}).WithStore(taskstore.NewMemoryStore())
	relay.agents.Register(proto.Register{AgentID: "a-1", Tags: map[string]string{"env": "prod"}}, nil) // conn=nil → 离线
	srv := newDAGTestServer(relay)
	defer srv.Close()

	body := `{"name":"upgrade-flow","max_concurrency":1,"nodes":[
		{"id":"prep"},
		{"id":"deploy","deps":["prep"],"tag":"env=prod","type":"powershell","payload":{"command":"upgrade.ps1"},"timeout":5},
		{"id":"verify","deps":["deploy"],"tag":"env=prod","type":"powershell","payload":{"command":"check.ps1"},"timeout":5}
	]}`
	code, started := dagDo(t, srv, "POST", "/api/dag", body)
	if code != http.StatusAccepted {
		t.Fatalf("提交应 202,实际 %d: %v", code, started)
	}
	dagID, _ := started["dag_id"].(string)
	if dagID == "" || started["node_count"].(float64) != 3 {
		t.Fatalf("dag_id/node_count 不符: %v", started)
	}

	// 轮询直到终态
	deadline := time.Now().Add(15 * time.Second)
	var snap map[string]any
	for time.Now().Before(deadline) {
		_, snap = dagDo(t, srv, "GET", "/api/dag/"+dagID, "")
		if snap["status"] == "finished" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if snap["status"] != "finished" {
		t.Fatalf("编排未在期限内终态: %v", snap)
	}

	// 汇总口径:1 成功 / 1 失败 / 1 跳过,success=false
	if snap["succeeded"].(float64) != 1 || snap["failed"].(float64) != 1 || snap["skipped"].(float64) != 1 {
		t.Fatalf("汇总应 1/1/1,实际 %v/%v/%v", snap["succeeded"], snap["failed"], snap["skipped"])
	}
	if snap["success"] != false {
		t.Fatalf("含失败+跳过的编排 success 应为 false")
	}
	// 串行启动序列完全确定:prep → deploy;verify 从未启动
	if order, _ := snap["start_order"].([]any); len(order) != 2 || order[0] != "prep" || order[1] != "deploy" {
		t.Fatalf("start_order 应 [prep deploy],实际 %v", order)
	}

	// 逐节点状态:deploy=failed(点名离线),verify=skipped 且无任务投递
	nodes, _ := snap["nodes"].([]any)
	byID := map[string]map[string]any{}
	for _, n := range nodes {
		m, _ := n.(map[string]any)
		byID[m["id"].(string)] = m
	}
	if byID["prep"]["status"] != "succeeded" {
		t.Fatalf("prep 应 succeeded,实际 %v", byID["prep"]["status"])
	}
	if byID["deploy"]["status"] != "failed" || !strings.Contains(byID["deploy"]["err"].(string), "离线") {
		t.Fatalf("deploy 应 failed 且点名离线,实际 %v", byID["deploy"])
	}
	if byID["verify"]["status"] != "skipped" {
		t.Fatalf("verify 应 skipped(级联),实际 %v", byID["verify"]["status"])
	}
	if tids, _ := byID["verify"]["task_ids"].([]any); len(tids) != 0 {
		t.Fatalf("被跳过的节点不应投递任何任务,实际 %v", tids)
	}

	// 列表端点可见
	_, list := dagDo(t, srv, "GET", "/api/dag", "")
	if list["count"].(float64) != 1 {
		t.Fatalf("列表应含 1 条,实际 %v", list)
	}
	dags, _ := list["dags"].([]any)
	entry, _ := dags[0].(map[string]any)
	if entry["dag_id"] != dagID || entry["state"] != "finished" || entry["name"] != "upgrade-flow" {
		t.Fatalf("列表条目不符: %v", entry)
	}
}

// TestDAGSoftDependency ContinueOnError 软依赖:上游失败后"告警"节点
// 照跑(不级联跳过),下游硬依赖节点仍被跳过。
func TestDAGSoftDependency(t *testing.T) {
	relay := NewRelay(Config{Token: "t"}).WithStore(taskstore.NewMemoryStore())
	relay.agents.Register(proto.Register{AgentID: "a-1", Tags: map[string]string{"env": "prod"}}, nil)
	srv := newDAGTestServer(relay)
	defer srv.Close()

	body := `{"name":"with-alert","nodes":[
		{"id":"deploy","tag":"env=prod","type":"powershell","payload":{"command":"x"},"timeout":5},
		{"id":"alert","deps":["deploy"],"agent_id":"a-1","type":"powershell","payload":{"command":"notify"},"continue_on_error":true,"timeout":5},
		{"id":"report","deps":["alert"]}
	]}`
	code, started := dagDo(t, srv, "POST", "/api/dag", body)
	if code != http.StatusAccepted {
		t.Fatalf("提交应 202,实际 %d", code)
	}
	dagID := started["dag_id"].(string)

	deadline := time.Now().Add(15 * time.Second)
	var snap map[string]any
	for time.Now().Before(deadline) {
		_, snap = dagDo(t, srv, "GET", "/api/dag/"+dagID, "")
		if snap["status"] == "finished" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if snap["status"] != "finished" {
		t.Fatalf("编排未终态: %v", snap)
	}
	nodes, _ := snap["nodes"].([]any)
	byID := map[string]map[string]any{}
	for _, n := range nodes {
		m, _ := n.(map[string]any)
		byID[m["id"].(string)] = m
	}
	// deploy 失败(离线);alert 软依赖 → 照样投递(同样离线 → failed,但不是 skipped);
	// report 依赖 alert(failed)→ 硬依赖级联 skipped。
	if byID["alert"]["status"] != "failed" {
		t.Fatalf("软依赖节点应照跑(此处因离线而 failed,而非 skipped),实际 %v", byID["alert"]["status"])
	}
	if byID["report"]["status"] != "skipped" {
		t.Fatalf("report 应被级联跳过,实际 %v", byID["report"]["status"])
	}
	// 启动序:alert 启动过(出现在 start_order),report 没有
	startedSet := map[string]bool{}
	if order, _ := snap["start_order"].([]any); len(order) == 2 {
		for _, v := range order {
			startedSet[v.(string)] = true
		}
	}
	if !startedSet["alert"] || startedSet["report"] {
		t.Fatalf("start_order 应含 alert 不含 report,实际 %v", snap["start_order"])
	}
}

// TestDAGPolicyBlocked 策略拦截:编排节点携带高危命令在提交期被拒(403),
// 与单任务/批量端点同款策略钩子。
func TestDAGPolicyBlocked(t *testing.T) {
	relay := NewRelay(Config{Token: "t"})
	relay.agents.Register(proto.Register{AgentID: "a-1", Tags: map[string]string{"env": "prod"}}, nil)
	srv := newDAGTestServer(relay)
	defer srv.Close()

	// Format-Volume 在默认策略黑名单内(见 policy.go/P1-1)
	body := `{"nodes":[{"id":"wipe","tag":"env=prod","type":"powershell","payload":{"command":"Format-Volume -DriveLetter C"}}]}`
	code, _ := dagDo(t, srv, "POST", "/api/dag", body)
	if code != http.StatusForbidden {
		t.Fatalf("高危节点应被策略拦截 403,实际 %d", code)
	}
}
