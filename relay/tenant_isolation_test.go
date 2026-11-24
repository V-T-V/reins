package main

// 2026 租户隔离加固 · HTTP 级验收(双租户 × admin/operator/viewer)。
//
// 针对外部只读审查(d739e85)的结论「共享 Relay 的租户隔离没有贯穿实际接口」,
// 用真实 WS Agent + 真实 bbolt + 真实 HTTP 走完整回路,逐条钉死:
//
//	P0-1 凭据绑定身份:请求方声明的 tenant/role 前缀不参与判定(省略/伪造前缀都提不了权)
//	P1   cookie 与 Bearer 同一道闸:cookie 身份不再是默认 admin
//	P0-2 对象级授权:Agent/任务/取消/批量/DAG/shell/传输/隧道/脚本/cron 逐面校验归属
//	P1   审计查询与 SSE 事件流按租户过滤
//	P1   Secret 归属校验(被拒的读取不烧 one-time)
//	     部署 Token 不再能拉全局清单
//	角色落地:viewer 真的只读;update.push 不再被 /api/updates/apply 绕过
//	深推  :bootstrap 不泄漏凭据列表;Agent ID 不可被顶替;离线缓存只回本租户连接
//
// 红绿锚定:本文件只使用加固前后都存在的 API,可原样放到旧代码上运行——
// 旧代码下应当成片失败(红),加固后全绿。用到新签名的断言在 tenant_units_test.go。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/V-T-V/reins/internal/ca"
	"github.com/V-T-V/reins/proto"
)

// 测试凭据(多凭据:逗号分隔)。secret 之间无前缀包含关系,断言「不泄漏」时才有判别力。
const (
	isoAdmin    = "qx-admin-secret-7f31"
	isoAcmeOp   = "acme:operator:qx-acme-secret-5c02"
	isoGlobOp   = "globex:operator:qx-glob-secret-9d14"
	isoGlobView = "globex:viewer:qx-view-secret-3b77"
	isoHcorpAdm = "hcorp:admin:qx-hcorp-secret-6e21" // 另一个租户里的 **admin** 角色
	isoDeploy   = "qx-deploy-token-aa48"
	isoTokenCfg = ":admin:" + isoAdmin + "," + isoAcmeOp + "," + isoGlobOp + "," + isoGlobView + "," + isoHcorpAdm
)

type isoEvent struct {
	kind    string
	agentID string
}

type isoResult struct {
	code int
	body map[string]any
	raw  string
}

// isoRequest 发一次 REST 请求。body 为 string 时原样发送,否则 JSON 编码。
func isoRequest(t *testing.T, srv *httptest.Server, method, path, token string, body any) isoResult {
	t.Helper()
	var rd io.Reader
	if body != nil {
		if s, ok := body.(string); ok {
			rd = strings.NewReader(s)
		} else {
			buf, _ := json.Marshal(body)
			rd = bytes.NewReader(buf)
		}
	}
	req, err := http.NewRequest(method, srv.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return isoResult{code: resp.StatusCode, body: m, raw: string(raw)}
}

func isoWantCode(t *testing.T, res isoResult, want int, what string) isoResult {
	t.Helper()
	if res.code != want {
		t.Errorf("%s: 状态码 = %d,期望 %d(响应: %.300s)", what, res.code, want, res.raw)
	}
	return res
}

// isoStrs 从 body[field] 取 ID 列表并排序。默认键序 agent_id → request.task_id →
// dag_id → name(覆盖 agents/tasks/dag/scripts);显式给 keys 时只按它取
// (cron 规则同时有 agent_id 与 id,须显式指定 "id")。
func isoStrs(t *testing.T, body map[string]any, field string, keys ...string) []string {
	t.Helper()
	raw, _ := body[field].([]any)
	out := make([]string, 0, len(raw))
	order := keys
	if len(order) == 0 {
		order = []string{"agent_id", "request.task_id", "dag_id", "name"}
	}
	for _, v := range raw {
		s, ok := v.(string)
		if !ok {
			x, isObj := v.(map[string]any)
			if !isObj {
				continue
			}
			for _, key := range order {
				if key == "request.task_id" {
					if req, hasReq := x["request"].(map[string]any); hasReq {
						s, _ = req["task_id"].(string)
					}
				} else {
					s, _ = x[key].(string)
				}
				if s != "" {
					break
				}
			}
		}
		if s != "" {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func isoEqual(t *testing.T, got, want []string, what string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s: 得到 %v,期望 %v", what, got, want)
	}
}

// isoDial 以指定凭据拨 /agent 并发送 register。**不**断言注册结果——
// 调用方用 isoExpectRejected / isoExpectAccepted / 自己读帧来表达预期。
func isoDial(t *testing.T, srv *httptest.Server, token, agentID string, tags map[string]string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hdr := http.Header{}
	if token != "" {
		hdr.Set("Authorization", "Bearer "+token)
	}
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/agent", &websocket.DialOptions{ //nolint:bodyclose // coder/websocket:成功时 resp.Body 为 nil
		HTTPHeader: hdr,
	})
	if err != nil {
		t.Fatalf("agent %s 拨号失败: %v", agentID, err)
	}
	t.Cleanup(func() { c.CloseNow() })
	c.SetReadLimit(1 << 20)
	regEnv, _ := proto.NewEnvelope(proto.TypeRegister, agentID, "relay", proto.Register{
		AgentID: agentID, Hostname: "host-" + agentID, OS: "windows/amd64",
		Capabilities: []string{proto.TaskPowerShell, proto.TaskScript, proto.TaskMonitor},
		Tags:         tags,
	})
	if err := c.Write(ctx, websocket.MessageText, mustJSON(regEnv)); err != nil {
		t.Fatalf("agent %s register 失败: %v", agentID, err)
	}
	return c
}

// isoExpectRejected 注册必须被拒:Relay 以 policy violation 关闭连接。
func isoExpectRejected(t *testing.T, c *websocket.Conn, why string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, data, err := c.Read(ctx)
	if err == nil {
		t.Fatalf("%s: 注册应被拒绝,连接却保持打开(收到 %.80s)", why, data)
	}
	// 库的 CloseError 是值类型且可能被包装——用库自带的 CloseStatus 取关闭码。
	if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Fatalf("%s: 期望以 policy violation(1008)关闭,得到 %v", why, err)
	}
}

// isoExpectAccepted 注册必须被接受——以**服务端状态**判定:Agent 出现在注册表里。
// 不能用「读超时」探测连接是否仍打开:coder/websocket 在 Read 的 ctx 到期时会
// 关闭连接,探测本身就会把 Agent 弄掉线(首版正是这样误报的)。
func isoExpectAccepted(t *testing.T, r *Relay, agentID, why string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		online := false
		for _, a := range r.agents.Snapshot() {
			if a.AgentID == agentID {
				online = true
			}
		}
		if online {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: 注册应被接受,但 Agent 未上线", why)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func isoWaitOnline(t *testing.T, r *Relay, agentID string, want bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		seen := false
		for _, a := range r.agents.Snapshot() {
			if a.AgentID == agentID {
				seen = true
			}
		}
		if seen == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待 agent %s 在线=%v 超时", agentID, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// isoTaskBody 一条普通的 powershell 任务体。
func isoTaskBody(taskID, agentID string) map[string]any {
	return map[string]any{"task_id": taskID, "type": proto.TaskPowerShell, "agent_id": agentID,
		"payload": map[string]any{"command": "Get-Date"}, "timeout": 30}
}

// newIsoRelay 搭建带持久化(bbolt)+ 审计 + 调度器的多租户测试 Relay。
func newIsoRelay(t *testing.T) (*Relay, *httptest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	r := NewRelay(Config{Addr: ":0", Token: isoTokenCfg, InstallToken: isoDeploy})
	store, err := OpenStore(filepath.Join(dir, "tasks.db"))
	if err != nil {
		t.Fatalf("打开 bbolt: %v", err)
	}
	r.WithStore(store)
	t.Cleanup(func() { _ = store.Close() })
	audit, err := OpenAuditLog(filepath.Join(dir, "audit.log"))
	if err != nil {
		t.Fatalf("打开审计: %v", err)
	}
	r.WithAudit(audit)
	t.Cleanup(func() { _ = audit.Close() })
	sched := NewScheduler(store, r)
	if err := sched.Start(); err != nil {
		t.Fatalf("启动调度器: %v", err)
	}
	r.WithScheduler(sched)
	// 挂上 CA(不开 mTLS):证书管理端点(签发注册令牌 / 吊销)才可用——它们同样需要对象级归属校验
	authority, err := ca.LoadOrCreateCA(dir)
	if err != nil {
		t.Fatalf("加载 CA: %v", err)
	}
	r.WithCA(authority)
	t.Cleanup(sched.Stop)

	// 脚本库是全局实例:换入临时目录,测试结束还原,不污染其它用例。
	oldLib := scriptLib
	lib, err := NewScriptLibrary(dir)
	if err != nil {
		t.Fatalf("脚本库: %v", err)
	}
	scriptLib = lib
	t.Cleanup(func() { scriptLib = oldLib })

	mux := http.NewServeMux()
	r.registerHTTP(mux)
	mux.HandleFunc("/agent", r.handleAgentWS)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return r, srv, dir
}

// isoRegister 三台归属分明的 Agent + 一台 admin 域 Agent,并等到全部在线。
func isoRegister(t *testing.T, r *Relay, srv *httptest.Server) map[string]*websocket.Conn {
	t.Helper()
	conns := map[string]*websocket.Conn{
		// acme-01 故意**自报** tags["tenant"]="globex":自报标签不参与鉴权,归属只认注册凭据
		"acme-01":   isoDial(t, srv, isoAcmeOp, "acme-01", map[string]string{"env": "prod", "tenant": "globex"}),
		"acme-02":   isoDial(t, srv, isoAcmeOp, "acme-02", nil),
		"globex-01": isoDial(t, srv, isoGlobOp, "globex-01", map[string]string{"env": "prod"}),
		"admin-01":  isoDial(t, srv, isoAdmin, "admin-01", map[string]string{"env": "prod"}),
	}
	for id := range conns {
		isoWaitOnline(t, r, id, true)
	}
	return conns
}

// --- P0-1:凭据绑定身份 ---

// TestCredentialIdentityIsServerBound 单凭据部署 `acme:operator:s3cret`:
// 知道 secret 的一方无论怎么声明前缀(省略 / 自称别的租户 / 自称 admin),
// 身份都只能是服务端配置的 acme/operator —— admin-only 操作一律 403。
func TestCredentialIdentityIsServerBound(t *testing.T) {
	r := NewRelay(Config{Addr: ":0", Token: "acme:operator:s3cret"})
	r.WithStore(NewMemoryStore())
	mux := http.NewServeMux()
	r.registerHTTP(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	_ = r

	secretBody := map[string]any{"value": "v", "ttl": 60}
	for _, presented := range []string{"s3cret", "victim:admin:s3cret", "acme:admin:s3cret", "acme:operator:s3cret", ":admin:s3cret"} {
		res := isoRequest(t, srv, "POST", "/api/secrets", presented, secretBody)
		if res.code != http.StatusForbidden {
			t.Errorf("出示 %q(应为 acme/operator):secret.put 状态码 = %d,期望 403 —— 知道 secret 即可提权为 admin", presented, res.code)
		}
		res = isoRequest(t, srv, "POST", "/api/power", presented, map[string]any{"agent_id": "x", "op": "restart"})
		if res.code != http.StatusForbidden {
			t.Errorf("出示 %q:power 状态码 = %d,期望 403", presented, res.code)
		}
		res = isoRequest(t, srv, "POST", "/api/agents/revoke", presented, map[string]any{"agent_id": "x"})
		if res.code != http.StatusForbidden {
			t.Errorf("出示 %q:agent.revoke 状态码 = %d,期望 403", presented, res.code)
		}
	}
	// 凭据本身仍然有效(不是被一律拒绝):只读端点 200
	if res := isoRequest(t, srv, "GET", "/api/tasks", "s3cret", nil); res.code != http.StatusOK {
		t.Errorf("租户凭据访问只读端点应 200,得到 %d", res.code)
	}
	// 真正的错 secret 仍是 401,且错误消息是 e2e/verify 的契约面
	res := isoRequest(t, srv, "GET", "/api/tasks", "nope", nil)
	if res.code != http.StatusUnauthorized || !strings.Contains(res.raw, "无效的鉴权 token") {
		t.Errorf("错误凭据应 401 + 契约消息,得到 %d %s", res.code, res.raw)
	}
}

// --- P1:cookie 与 Bearer 同一道闸 ---

// TestCookieIdentityIsNotAdmin 旧实现的角色守卫只读 Authorization 头,
// cookie 过鉴权的请求一律被当成 admin。现在 cookie 身份 = 服务端绑定的身份。
func TestCookieIdentityIsNotAdmin(t *testing.T) {
	r := NewRelay(Config{Addr: ":0", Token: "acme:operator:s3cret"})
	r.WithStore(NewMemoryStore())
	mux := http.NewServeMux()
	r.registerHTTP(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	do := func(cookieValue string, raw string) int {
		req, _ := http.NewRequest("POST", srv.URL+"/api/secrets", strings.NewReader(raw))
		req.AddCookie(&http.Cookie{Name: "reins_token", Value: cookieValue})
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}
	// 前端 setToken 用 encodeURIComponent 写 cookie(: → %3A);两种写法都必须被识别为 operator
	if code := do("acme:operator:s3cret", `{"value":"v","ttl":60}`); code != http.StatusForbidden {
		t.Errorf("operator cookie 打 secret.put = %d,期望 403(cookie 路径不得默认 admin)", code)
	}
	if code := do("acme%3Aoperator%3As3cret", `{"value":"v","ttl":60}`); code != http.StatusForbidden {
		t.Errorf("URL 编码的 operator cookie 打 secret.put = %d,期望 403", code)
	}
	// 伪造前缀的 cookie 同样提不了权
	if code := do("acme%3Aadmin%3As3cret", `{"value":"v","ttl":60}`); code != http.StatusForbidden {
		t.Errorf("自称 admin 的 cookie 打 secret.put = %d,期望 403", code)
	}
	// admin 的 cookie 仍可用(真 admin 不受影响)
	admin := NewRelay(Config{Addr: ":0", Token: "realadmin"})
	admin.WithStore(NewMemoryStore())
	amux := http.NewServeMux()
	admin.registerHTTP(amux)
	asrv := httptest.NewServer(amux)
	defer asrv.Close()
	req, _ := http.NewRequest("POST", asrv.URL+"/api/secrets", strings.NewReader(`{"value":"v","ttl":60}`))
	req.AddCookie(&http.Cookie{Name: "reins_token", Value: "realadmin"})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("admin cookie 打 secret.put = %d,期望 201", resp.StatusCode)
	}
}

// --- 角色落地 ---

// TestViewerIsReadOnly viewer 只能读 + 只能提交 monitor 类型任务。
func TestViewerIsReadOnly(t *testing.T) {
	r := NewRelay(Config{Addr: ":0", Token: ":admin:qx-admin-1,globex:viewer:qx-view-1"})
	r.WithStore(NewMemoryStore())
	mux := http.NewServeMux()
	r.registerHTTP(mux)
	mux.HandleFunc("/agent", r.handleAgentWS)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	g := isoDial(t, srv, "qx-view-1", "globex-01", nil) // 用同一凭据注册,Agent 归属 globex
	_ = g
	isoWaitOnline(t, r, "globex-01", true)
	vw := "qx-view-1"

	// 只读面:放行(审计端点需要开启审计,在双租户用例里覆盖)
	for _, path := range []string{"/api/agents", "/api/tasks", "/api/dag", "/api/crons", "/api/metrics", "/healthz"} {
		if res := isoRequest(t, srv, "GET", path, vw, nil); res.code != http.StatusOK {
			t.Errorf("viewer GET %s = %d,期望 200(只读应放行)", path, res.code)
		}
	}
	if got := isoStrs(t, isoRequest(t, srv, "GET", "/api/agents", vw, nil).body, "agents"); !reflect.DeepEqual(got, []string{"globex-01"}) {
		t.Errorf("viewer 清单 = %v,期望 [globex-01]", got)
	}
	// 写面:默认拒绝(此前这些端点只验凭据,viewer 可开 shell、建 cron、触发自更新)
	posts := []struct {
		path string
		body any
	}{
		{"/api/tasks", isoTaskBody("vw-1", "globex-01")}, // powershell
		{"/api/tasks", map[string]any{"task_id": "vw-2", "type": proto.TaskScript, "agent_id": "globex-01", "payload": map[string]any{"content": "x"}}},
		{"/api/tasks/batch", map[string]any{"tag": "x=1", "type": proto.TaskPowerShell, "payload": map[string]any{"command": "Get-Date"}}},
		{"/api/batch", map[string]any{"tag": "x=1", "type": proto.TaskPowerShell, "payload": map[string]any{"command": "Get-Date"}}},
		{"/api/dag", map[string]any{"nodes": []any{map[string]any{"id": "n", "agent_id": "globex-01", "type": proto.TaskPowerShell, "payload": map[string]any{"command": "Get-Date"}}}}},
		{"/api/tasks/cancel", map[string]any{"task_id": "vw-1"}},
		{"/api/crons", map[string]any{"cron_expr": "* * * * *", "agent_id": "globex-01", "task_type": proto.TaskPowerShell, "payload": map[string]any{"command": "Get-Date"}}},
		{"/api/shell/open", map[string]any{"agent_id": "globex-01"}},
		{"/api/transfer/upload?agent_id=globex-01&path=C:/tmp/x", "data"},
		{"/api/tunnel/open", map[string]any{"tunnel_id": "t", "agent_id": "globex-01", "network": "tcp", "target": "127.0.0.1:1"}},
		{"/api/updates/apply", map[string]any{"agent_id": "globex-01", "version": "1.0.0"}},
		{"/api/updates/upload?version=1.0.0", "bin"},
		{"/api/scripts", map[string]any{"name": "s.ps1", "content": "x"}},
		{"/api/secrets", map[string]any{"value": "v"}},
	}
	for _, p := range posts {
		if res := isoRequest(t, srv, "POST", p.path, vw, p.body); res.code != http.StatusForbidden {
			t.Errorf("viewer POST %s = %d,期望 403(只读)", strings.SplitN(p.path, "?", 2)[0], res.code)
		}
	}
	for _, c := range []struct{ method, path string }{
		{"DELETE", "/api/crons/x"}, {"GET", "/api/secrets/sec_x"}, {"GET", "/api/bootstrap"},
		{"GET", "/api/tunnel/poll?tunnel_id=t"}, {"DELETE", "/api/scripts/s.ps1"},
	} {
		if res := isoRequest(t, srv, c.method, c.path, vw, nil); res.code != http.StatusForbidden {
			t.Errorf("viewer %s %s = %d,期望 403", c.method, c.path, res.code)
		}
	}
	// 只读型任务:viewer 可以提交 monitor
	if res := isoRequest(t, srv, "POST", "/api/tasks", vw, map[string]any{"task_id": "vw-mon", "type": proto.TaskMonitor, "agent_id": "globex-01", "payload": map[string]any{"mode": "cpu"}, "timeout": 5}); res.code != http.StatusAccepted {
		t.Errorf("viewer 提交 monitor 任务 = %d %s,期望 202", res.code, res.raw)
	}
}

// TestUpdatePushNotBypassable operator 不得经 /api/updates/* 绕过 update.push(admin-only)。
func TestUpdatePushNotBypassable(t *testing.T) {
	r := NewRelay(Config{Addr: ":0", Token: ":admin:qx-admin-1,acme:operator:qx-op-1"})
	r.WithStore(NewMemoryStore())
	mux := http.NewServeMux()
	r.registerHTTP(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	for _, path := range []string{"/api/updates/apply", "/api/updates/upload?version=9.9.9"} {
		if res := isoRequest(t, srv, "POST", path, "acme:operator:qx-op-1", map[string]any{"agent_id": "a", "version": "9.9.9"}); res.code != http.StatusForbidden {
			t.Errorf("operator POST %s = %d,期望 403(与 POST /api/tasks 的 update 类型同一道闸)", path, res.code)
		}
	}
}

// --- 部署 Token ---

// TestInstallTokenOnlySingleProbe 部署 Token 烘焙在未鉴权的 install.ps1 里,视同半公开:
// 只能问「这一台在不在线」,不能拉清单。用部署 Token 注册的 Agent 落在 admin 域。
func TestInstallTokenOnlySingleProbe(t *testing.T) {
	r := NewRelay(Config{Addr: ":0", Token: isoAdmin, InstallToken: isoDeploy})
	r.WithStore(NewMemoryStore())
	mux := http.NewServeMux()
	r.registerHTTP(mux)
	mux.HandleFunc("/agent", r.handleAgentWS)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	isoDial(t, srv, isoDeploy, "kiosk-77", nil)
	isoWaitOnline(t, r, "kiosk-77", true)

	res := isoWantCode(t, isoRequest(t, srv, "GET", "/api/agents", isoDeploy, nil), http.StatusForbidden, "部署 Token 拉全局清单")
	_ = res
	res = isoWantCode(t, isoRequest(t, srv, "GET", "/api/agents?agent_id=kiosk-77", isoDeploy, nil), http.StatusOK, "部署 Token 单机探活")
	if got := isoStrs(t, res.body, "agents"); !reflect.DeepEqual(got, []string{"kiosk-77"}) {
		t.Errorf("探活结果 = %v,期望 [kiosk-77]", got)
	}
	for _, leak := range []string{"hostname", "capabilities", "tags", "xfer_stats"} {
		if strings.Contains(res.raw, `"`+leak+`"`) {
			t.Errorf("探活响应泄漏字段 %s: %s", leak, res.raw)
		}
	}
	res = isoWantCode(t, isoRequest(t, srv, "GET", "/api/agents?agent_id=never-seen", isoDeploy, nil), http.StatusOK, "探活不存在的 Agent")
	if res.body["count"] != float64(0) {
		t.Errorf("不在线应 count=0,得到 %v", res.body)
	}
	for _, path := range []string{"/api/tasks", "/api/audit/query", "/api/bootstrap", "/api/events"} {
		if res := isoRequest(t, srv, "GET", path, isoDeploy, nil); res.code == http.StatusOK {
			t.Errorf("部署 Token 访问 %s 不应 200(仅限注册与单机探活)", path)
		}
	}
}

// --- 双租户全回路 ---

// TestTenantIsolationTwoTenants 审查验收标准:两个租户 × admin/operator/viewer,
// 跨租户访问被拒,同租户正常运维可用。
func TestTenantIsolationTwoTenants(t *testing.T) {
	r, srv, _ := newIsoRelay(t)
	conns := isoRegister(t, r, srv)
	A, G, V, ADM := isoAcmeOp, isoGlobOp, isoGlobView, isoAdmin

	t.Run("提权灭_声明前缀不参与判定", func(t *testing.T) {
		for _, presented := range []string{"victim:admin:qx-acme-secret-5c02", "acme:admin:qx-acme-secret-5c02", "qx-acme-secret-5c02", "globex:operator:qx-acme-secret-5c02"} {
			res := isoWantCode(t, isoRequest(t, srv, "GET", "/api/agents", presented, nil), http.StatusOK, "acme 凭据变体")
			isoEqual(t, isoStrs(t, res.body, "agents"), []string{"acme-01", "acme-02"}, "acme 凭据变体 "+presented+" 的清单")
			isoWantCode(t, isoRequest(t, srv, "POST", "/api/secrets", presented, map[string]any{"value": "v"}), http.StatusForbidden, "acme 凭据变体 secret.put")
		}
	})

	t.Run("Agent 清单隔离", func(t *testing.T) {
		isoEqual(t, isoStrs(t, isoRequest(t, srv, "GET", "/api/agents", A, nil).body, "agents"), []string{"acme-01", "acme-02"}, "acme 清单")
		isoEqual(t, isoStrs(t, isoRequest(t, srv, "GET", "/api/agents", G, nil).body, "agents"), []string{"globex-01"}, "globex 清单")
		isoEqual(t, isoStrs(t, isoRequest(t, srv, "GET", "/api/agents", V, nil).body, "agents"), []string{"globex-01"}, "globex viewer 清单")
		isoEqual(t, isoStrs(t, isoRequest(t, srv, "GET", "/api/agents", ADM, nil).body, "agents"), []string{"acme-01", "acme-02", "admin-01", "globex-01"}, "admin 清单")
		// Agent 自报的 tags["tenant"] 不参与鉴权:它只是展示字段
		// 在线数同样按租户
		if n := isoRequest(t, srv, "GET", "/healthz", A, nil).body["agents"]; n != float64(2) {
			t.Errorf("acme 的 healthz.agents = %v,期望 2", n)
		}
		if n := isoRequest(t, srv, "GET", "/api/metrics", A, nil).body["agents_online"]; n != float64(2) {
			t.Errorf("acme 的 agents_online = %v,期望 2", n)
		}
	})

	t.Run("任务_对象级授权", func(t *testing.T) {
		isoWantCode(t, isoRequest(t, srv, "POST", "/api/tasks", A, isoTaskBody("t-acme-1", "acme-01")), http.StatusAccepted, "acme 下发给自己 Agent")
		isoWantCode(t, isoRequest(t, srv, "POST", "/api/tasks", A, isoTaskBody("t-x1", "globex-01")), http.StatusForbidden, "acme 下发给 globex Agent")
		isoWantCode(t, isoRequest(t, srv, "POST", "/api/tasks", A, isoTaskBody("t-x2", "admin-01")), http.StatusForbidden, "acme 下发给 admin 域 Agent")
		isoWantCode(t, isoRequest(t, srv, "POST", "/api/tasks", A, isoTaskBody("t-x3", "ghost-99")), http.StatusForbidden, "acme 下发给未知 Agent")
		isoWantCode(t, isoRequest(t, srv, "POST", "/api/tasks", G, isoTaskBody("t-glob-1", "globex-01")), http.StatusAccepted, "globex 下发给自己 Agent")
		isoWantCode(t, isoRequest(t, srv, "POST", "/api/tasks", G, isoTaskBody("t-x4", "acme-01")), http.StatusForbidden, "globex 下发给 acme Agent")

		isoWantCode(t, isoRequest(t, srv, "GET", "/api/tasks/t-acme-1", G, nil), http.StatusNotFound, "globex 读 acme 任务详情")
		isoWantCode(t, isoRequest(t, srv, "GET", "/api/tasks/t-acme-1", V, nil), http.StatusNotFound, "globex viewer 读 acme 任务详情")
		isoWantCode(t, isoRequest(t, srv, "GET", "/api/tasks/t-acme-1", A, nil), http.StatusOK, "acme 读自己的任务详情")
		isoWantCode(t, isoRequest(t, srv, "GET", "/api/tasks/t-acme-1", ADM, nil), http.StatusOK, "admin 读任意任务详情")

		isoEqual(t, isoStrs(t, isoRequest(t, srv, "GET", "/api/tasks?limit=500", A, nil).body, "tasks"), []string{"t-acme-1"}, "acme 任务列表")
		isoEqual(t, isoStrs(t, isoRequest(t, srv, "GET", "/api/tasks?limit=500", G, nil).body, "tasks"), []string{"t-glob-1"}, "globex 任务列表")
		isoEqual(t, isoStrs(t, isoRequest(t, srv, "GET", "/api/tasks?limit=500", V, nil).body, "tasks"), []string{"t-glob-1"}, "globex viewer 任务列表")
		isoEqual(t, isoStrs(t, isoRequest(t, srv, "GET", "/api/tasks?limit=500", ADM, nil).body, "tasks"), []string{"t-acme-1", "t-glob-1"}, "admin 任务列表")

		isoWantCode(t, isoRequest(t, srv, "POST", "/api/tasks/cancel", G, map[string]any{"task_id": "t-acme-1"}), http.StatusForbidden, "globex 取消 acme 任务")
		isoWantCode(t, isoRequest(t, srv, "POST", "/api/tasks", A, map[string]any{"task_id": "t-pw", "type": proto.TaskPower, "agent_id": "acme-01", "payload": map[string]any{"op": "restart"}}), http.StatusForbidden, "acme operator 下发电源任务")
	})

	t.Run("批量与DAG_目标解析收口", func(t *testing.T) {
		// env=prod 同时命中 acme-01 / globex-01 / admin-01,但 acme 只能扇到自己的
		res := isoWantCode(t, isoRequest(t, srv, "POST", "/api/tasks/batch", A, map[string]any{"tag": "env=prod", "type": proto.TaskPowerShell, "payload": map[string]any{"command": "Get-Date"}}), http.StatusAccepted, "acme 同步批量")
		isoEqual(t, isoStrs(t, res.body, "dispatched"), []string{"acme-01"}, "acme 同步批量的目标")
		res = isoWantCode(t, isoRequest(t, srv, "POST", "/api/batch", A, map[string]any{"tag": "env=prod", "type": proto.TaskPowerShell, "payload": map[string]any{"command": "Get-Date"}}), http.StatusAccepted, "acme 异步批量")
		if res.body["total"] != float64(1) {
			t.Errorf("acme 异步批量 total = %v,期望 1", res.body["total"])
		}
		batchID, _ := res.body["batch_id"].(string)
		isoWantCode(t, isoRequest(t, srv, "GET", "/api/batch/"+batchID, G, nil), http.StatusNotFound, "globex 读 acme 批次")
		isoWantCode(t, isoRequest(t, srv, "GET", "/api/batch/"+batchID, A, nil), http.StatusOK, "acme 读自己的批次")

		res = isoWantCode(t, isoRequest(t, srv, "POST", "/api/dag", A, map[string]any{"name": "iso", "nodes": []any{
			map[string]any{"id": "n1", "tag": "env=prod", "type": proto.TaskPowerShell, "payload": map[string]any{"command": "Get-Date"}, "timeout": 1},
		}}), http.StatusAccepted, "acme 提交 tag 扇出 DAG")
		dagID, _ := res.body["dag_id"].(string)
		st := isoWantCode(t, isoRequest(t, srv, "GET", "/api/dag/"+dagID, A, nil), http.StatusOK, "acme 读自己的 DAG")
		if nodes, _ := st.body["nodes"].([]any); len(nodes) != 1 {
			t.Errorf("DAG 节点数 = %d,期望 1", len(nodes))
		} else if tg, _ := nodes[0].(map[string]any)["targets"].([]any); len(tg) != 1 || tg[0] != "acme-01" {
			t.Errorf("DAG tag 扇出目标 = %v,期望 [acme-01]", tg)
		}
		isoWantCode(t, isoRequest(t, srv, "POST", "/api/dag", A, map[string]any{"nodes": []any{
			map[string]any{"id": "n1", "agent_id": "globex-01", "type": proto.TaskPowerShell, "payload": map[string]any{"command": "Get-Date"}},
		}}), http.StatusForbidden, "acme 提交直指 globex Agent 的 DAG")
		isoWantCode(t, isoRequest(t, srv, "GET", "/api/dag/"+dagID, G, nil), http.StatusNotFound, "globex 读 acme DAG")
		if ids := isoStrs(t, isoRequest(t, srv, "GET", "/api/dag", G, nil).body, "dags"); len(ids) != 0 {
			t.Errorf("globex 的 DAG 列表 = %v,期望为空(不含 acme 的)", ids)
		}

		// 让编排干净收尾:节点投递出的任务由测试在存储里标记完成,编排的轮询随之结束。
		// 否则这个后台 goroutine 会活到测试清理关闭 bbolt 之后还在读库——竞态检测首跑就报了。
		// (假 Agent 不会回任务结果,只能这样给它一个终态。)
		deadline := time.Now().Add(3 * time.Second)
		for {
			st := isoRequest(t, srv, "GET", "/api/dag/"+dagID, ADM, nil)
			nodes, _ := st.body["nodes"].([]any)
			if len(nodes) == 1 {
				if n, _ := nodes[0].(map[string]any); n != nil {
					if ids, _ := n["task_ids"].([]any); len(ids) > 0 {
						for _, v := range ids {
							_ = r.store.UpdateTaskStatus(fmt.Sprint(v), StatusCompleted)
						}
						break
					}
				}
			}
			if time.Now().After(deadline) {
				t.Fatal("DAG 节点未投递出任务,无法收尾")
			}
			time.Sleep(10 * time.Millisecond)
		}
		for {
			if st := isoRequest(t, srv, "GET", "/api/dag/"+dagID, ADM, nil); st.body["status"] == "finished" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("DAG 未在收尾后结束")
			}
			time.Sleep(10 * time.Millisecond)
		}
	})

	t.Run("shell_传输_隧道_脚本_run", func(t *testing.T) {
		isoWantCode(t, isoRequest(t, srv, "POST", "/api/shell/open", A, map[string]any{"agent_id": "globex-01"}), http.StatusForbidden, "acme 在 globex Agent 上开 shell")
		isoWantCode(t, isoRequest(t, srv, "POST", "/api/shell/open", A, map[string]any{"agent_id": "acme-01", "cols": 80, "rows": 24}), http.StatusOK, "acme 在自己 Agent 上开 shell")
		isoWantCode(t, isoRequest(t, srv, "POST", "/api/shell/input", A, map[string]any{"session_id": "s", "agent_id": "globex-01", "data_b64": "YQ=="}), http.StatusForbidden, "acme 向 globex 会话注入 stdin")
		isoWantCode(t, isoRequest(t, srv, "POST", "/api/transfer/upload?agent_id=globex-01&path=C:/tmp/x", A, "data"), http.StatusForbidden, "acme 向 globex Agent 上传")
		isoWantCode(t, isoRequest(t, srv, "POST", "/api/transfer/download?agent_id=globex-01&path=C:/Windows/win.ini", A, nil), http.StatusForbidden, "acme 从 globex Agent 下载")
		isoWantCode(t, isoRequest(t, srv, "POST", "/api/transfer/direct", A, map[string]any{"agent_id": "globex-01", "path": "C:/x", "direction": "download"}), http.StatusForbidden, "acme 申请 globex Agent 的直连票据")
		isoWantCode(t, isoRequest(t, srv, "POST", "/api/tunnel/open", A, map[string]any{"tunnel_id": "tn", "agent_id": "globex-01", "network": "tcp", "target": "127.0.0.1:1"}), http.StatusForbidden, "acme 经 globex Agent 开隧道")

		isoWantCode(t, isoRequest(t, srv, "POST", "/api/scripts", A, map[string]any{"name": "acme.ps1", "content": "Write-Output acme"}), http.StatusCreated, "acme 上传脚本")
		isoWantCode(t, isoRequest(t, srv, "POST", "/api/scripts", G, map[string]any{"name": "globex.ps1", "content": "Write-Output globex"}), http.StatusCreated, "globex 上传脚本")
		isoWantCode(t, isoRequest(t, srv, "GET", "/api/scripts/acme.ps1", G, nil), http.StatusNotFound, "globex 读 acme 脚本")
		isoWantCode(t, isoRequest(t, srv, "GET", "/api/scripts/acme.ps1?tenant=acme", G, nil), http.StatusNotFound, "globex 用 ?tenant= 进 acme 命名空间")
		isoWantCode(t, isoRequest(t, srv, "GET", "/api/scripts/globex.ps1", A, nil), http.StatusNotFound, "acme 读 globex 脚本")
		isoWantCode(t, isoRequest(t, srv, "DELETE", "/api/scripts/acme.ps1", G, nil), http.StatusNotFound, "globex 删 acme 脚本")
		isoWantCode(t, isoRequest(t, srv, "POST", "/api/scripts/acme.ps1/run", G, map[string]any{"agent_id": "globex-01"}), http.StatusNotFound, "globex 运行 acme 脚本")
		isoWantCode(t, isoRequest(t, srv, "POST", "/api/scripts/globex.ps1/run", G, map[string]any{"agent_id": "acme-01"}), http.StatusForbidden, "globex 在 acme Agent 上运行脚本")
		isoEqual(t, isoStrs(t, isoRequest(t, srv, "GET", "/api/scripts", A, nil).body, "scripts", "name"), []string{"acme.ps1"}, "acme 脚本列表")
		isoEqual(t, isoStrs(t, isoRequest(t, srv, "GET", "/api/scripts", G, nil).body, "scripts", "name"), []string{"globex.ps1"}, "globex 脚本列表")
		isoEqual(t, isoStrs(t, isoRequest(t, srv, "GET", "/api/scripts", ADM, nil).body, "scripts", "name"), []string{}, "admin 根命名空间列表")
		isoWantCode(t, isoRequest(t, srv, "GET", "/api/scripts/globex.ps1?tenant=globex", ADM, nil), http.StatusOK, "admin 进 globex 命名空间读脚本")
		isoWantCode(t, isoRequest(t, srv, "POST", "/api/scripts/acme.ps1/run", A, map[string]any{"agent_id": "acme-01"}), http.StatusAccepted, "acme 在自己 Agent 上运行脚本")
	})

	t.Run("cron_归属与继承", func(t *testing.T) {
		isoWantCode(t, isoRequest(t, srv, "POST", "/api/crons", A, map[string]any{"cron_expr": "*/5 * * * *", "agent_id": "globex-01", "task_type": proto.TaskPowerShell, "payload": map[string]any{"command": "Get-Date"}}), http.StatusForbidden, "acme 给 globex Agent 建 cron")
		res := isoWantCode(t, isoRequest(t, srv, "POST", "/api/crons", A, map[string]any{"id": "cron-acme", "cron_expr": "*/5 * * * *", "agent_id": "acme-01", "task_type": proto.TaskPowerShell, "payload": map[string]any{"command": "Get-Date"}}), http.StatusCreated, "acme 给自己 Agent 建 cron")
		if res.body["tenant"] != "acme" {
			t.Errorf("cron 规则应记录租户 acme,得到 %v", res.body)
		}
		isoEqual(t, isoStrs(t, isoRequest(t, srv, "GET", "/api/crons", A, nil).body, "crons", "id"), []string{"cron-acme"}, "acme cron 列表")
		isoEqual(t, isoStrs(t, isoRequest(t, srv, "GET", "/api/crons", G, nil).body, "crons", "id"), []string{}, "globex cron 列表")
		isoWantCode(t, isoRequest(t, srv, "DELETE", "/api/crons/cron-acme", G, nil), http.StatusNotFound, "globex 删 acme cron")
		isoWantCode(t, isoRequest(t, srv, "DELETE", "/api/crons/cron-acme", A, nil), http.StatusOK, "acme 删自己的 cron")
	})

	t.Run("Secret_归属校验_被拒不烧", func(t *testing.T) {
		isoWantCode(t, isoRequest(t, srv, "POST", "/api/secrets", A, map[string]any{"value": "v"}), http.StatusForbidden, "operator 不得创建 Secret(既有角色矩阵)")
		// admin 为 acme 放一枚一次性凭据
		res := isoWantCode(t, isoRequest(t, srv, "POST", "/api/secrets", ADM, map[string]any{"value": "acme-db-pass", "tenant": "acme", "one_time": true, "ttl": 120}), http.StatusCreated, "admin 为 acme 创建 Secret")
		ref, _ := res.body["ref"].(string)
		isoWantCode(t, isoRequest(t, srv, "GET", "/api/secrets/"+ref, G, nil), http.StatusNotFound, "globex operator 读 acme 的 Secret")
		isoWantCode(t, isoRequest(t, srv, "GET", "/api/secrets/"+ref, V, nil), http.StatusForbidden, "viewer 读 Secret")
		got := isoWantCode(t, isoRequest(t, srv, "GET", "/api/secrets/"+ref, A, nil), http.StatusOK, "acme operator 读自己的 Secret(被拒的读取不得把它烧掉)")
		if got.body["value"] != "acme-db-pass" {
			t.Errorf("Secret 值不符: %v", got.body)
		}
		isoWantCode(t, isoRequest(t, srv, "GET", "/api/secrets/"+ref, A, nil), http.StatusNotFound, "一次性 Secret 已被合法读取消费")
		// admin 域的 Secret(未指定 tenant)对租户不可见
		res = isoWantCode(t, isoRequest(t, srv, "POST", "/api/secrets", ADM, map[string]any{"value": "root-only", "ttl": 120}), http.StatusCreated, "admin 创建 admin 域 Secret")
		ref2, _ := res.body["ref"].(string)
		isoWantCode(t, isoRequest(t, srv, "GET", "/api/secrets/"+ref2, A, nil), http.StatusNotFound, "租户读 admin 域 Secret")
	})

	t.Run("bootstrap_不泄漏凭据列表", func(t *testing.T) {
		res := isoWantCode(t, isoRequest(t, srv, "GET", "/api/bootstrap", A, nil), http.StatusOK, "acme 取 bootstrap")
		if res.body["token"] != isoAcmeOp {
			t.Errorf("bootstrap 应只回显调用者自己的凭据,得到 %q", res.body["token"])
		}
		for _, mustNot := range []string{isoAdmin, isoGlobOp, isoGlobView, isoDeploy} {
			if strings.Contains(res.raw, mustNot) {
				t.Errorf("bootstrap 响应泄漏了其它凭据 %q", mustNot)
			}
		}
		isoWantCode(t, isoRequest(t, srv, "GET", "/api/bootstrap", V, nil), http.StatusForbidden, "viewer 取 bootstrap")
	})

	t.Run("审计查询按租户过滤", func(t *testing.T) {
		type entry = map[string]any
		collect := func(token string) ([]entry, map[string]int) {
			deadline := time.Now().Add(3 * time.Second)
			for {
				res := isoRequest(t, srv, "GET", "/api/audit/query?limit=500", token, nil)
				raw, _ := res.body["entries"].([]any)
				tenants := map[string]int{}
				out := make([]entry, 0, len(raw))
				for _, e := range raw {
					m, _ := e.(map[string]any)
					out = append(out, m)
					tn, _ := m["tenant"].(string)
					tenants[tn]++
				}
				if len(out) > 0 || time.Now().After(deadline) {
					return out, tenants
				}
				time.Sleep(20 * time.Millisecond)
			}
		}
		acme, acmeTenants := collect(A)
		if len(acme) < 3 {
			t.Fatalf("acme 的审计条目过少: %d", len(acme))
		}
		if len(acmeTenants) != 1 || acmeTenants["acme"] != len(acme) {
			t.Errorf("acme 审计查询混入了别的租户/admin 域条目: %v", acmeTenants)
		}
		hasOperator := false
		for _, e := range acme {
			if e["event"] == "task_submitted" && e["operator"] == "acme/operator" {
				hasOperator = true
			}
		}
		if !hasOperator {
			t.Errorf("acme 的 task_submitted 审计应带 operator=acme/operator(此前恒空): %v", acme)
		}
		globex, globexTenants := collect(G)
		if len(globex) == 0 || len(globexTenants) != 1 || globexTenants["globex"] != len(globex) {
			t.Errorf("globex 审计查询混入了别的租户条目: %v", globexTenants)
		}
		_, adminTenants := collect(ADM)
		if adminTenants["acme"] == 0 || adminTenants["globex"] == 0 {
			t.Errorf("admin 应看到全部租户的审计: %v", adminTenants)
		}
	})

	t.Run("SSE 事件流按租户过滤", func(t *testing.T) {
		events, stop := isoSubscribe(t, srv, A)
		defer stop()
		// 等 hello:订阅已建立
		select {
		case e := <-events:
			if e.kind != "hello" {
				t.Fatalf("首个事件应为 hello,得到 %+v", e)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("未收到 SSE hello")
		}
		fire := func(agentID string) {
			r.console.Broadcast(Event{Kind: proto.TypeTaskResult, AgentID: agentID, Payload: json.RawMessage(`{"ok":true}`), TS: time.Now().UnixMilli()})
		}
		// 先发他租户的,再发本租户的:顺序投递,收到本租户事件时他租户的(若泄漏)必然已先到
		fire("globex-01")
		fire("admin-01")
		fire("ghost-99")
		fire("acme-01")
		seen := map[string]bool{}
		deadline := time.After(3 * time.Second)
		for !seen["acme-01"] {
			select {
			case e := <-events:
				if e.agentID != "" {
					seen[e.agentID] = true
				}
			case <-deadline:
				t.Fatalf("acme 的 SSE 未收到自己 Agent 的事件(已见 %v)", seen)
			}
		}
		for _, leak := range []string{"globex-01", "admin-01", "ghost-99"} {
			if seen[leak] {
				t.Errorf("acme 的 SSE 收到了不该看到的事件: %s", leak)
			}
		}
	})

	t.Run("Prometheus 与 JSON 指标的可见范围", func(t *testing.T) {
		isoWantCode(t, isoRequest(t, srv, "GET", "/metrics", G, nil), http.StatusUnauthorized, "租户凭据抓 /metrics(全 Relay 聚合)")
		isoWantCode(t, isoRequest(t, srv, "GET", "/metrics", ADM, nil), http.StatusOK, "admin 凭据抓 /metrics")
	})

	t.Run("Agent ID 不可顶替", func(t *testing.T) {
		isoExpectRejected(t, isoDial(t, srv, G, "acme-01", nil), "globex 凭据注册 acme 的 Agent ID")
		isoExpectRejected(t, isoDial(t, srv, isoDeploy, "acme-01", nil), "部署 Token 注册 acme 的 Agent ID")
		isoWaitOnline(t, r, "acme-01", true)
		isoEqual(t, isoStrs(t, isoRequest(t, srv, "GET", "/api/agents", A, nil).body, "agents"), []string{"acme-01", "acme-02"}, "顶替未遂后 acme 清单不变")
		isoWantCode(t, isoRequest(t, srv, "POST", "/api/tasks", A, isoTaskBody("t-after", "acme-01")), http.StatusAccepted, "顶替未遂后 acme 仍可运维自己的 Agent")

		// 部署 Token 可以注册**未被占用**的 ID,但落在 admin 域:租户看不见也打不到
		isoDial(t, srv, isoDeploy, "fresh-kiosk", map[string]string{"env": "prod"})
		isoExpectAccepted(t, r, "fresh-kiosk", "部署 Token 注册全新 ID")
		isoEqual(t, isoStrs(t, isoRequest(t, srv, "GET", "/api/agents", A, nil).body, "agents"), []string{"acme-01", "acme-02"}, "部署 Token 新增的 Agent 不进租户清单")
		isoWantCode(t, isoRequest(t, srv, "POST", "/api/tasks", A, isoTaskBody("t-x9", "fresh-kiosk")), http.StatusForbidden, "租户给部署 Token 注册的 Agent 下发任务")
	})

	t.Run("证书管理_归属校验", func(t *testing.T) {
		H := isoHcorpAdm // 另一个租户里的 admin 角色:角色门放行,归属必须拦住
		isoWantCode(t, isoRequest(t, srv, "POST", "/api/enroll-token", G, map[string]any{"agent_id": "globex-01"}), http.StatusForbidden, "operator 签发注册令牌(角色门)")
		isoWantCode(t, isoRequest(t, srv, "POST", "/api/enroll-token", H, map[string]any{"agent_id": "acme-01"}), http.StatusForbidden, "他租户 admin 给 acme 的 Agent 签发注册令牌")
		res := isoWantCode(t, isoRequest(t, srv, "POST", "/api/enroll-token", H, map[string]any{"agent_id": "hcorp-new-01"}), http.StatusOK, "租户 admin 给尚未绑定的 ID 签发注册令牌")
		if res.body["token"] == "" {
			t.Errorf("签发注册令牌应返回 token: %v", res.body)
		}
		isoWantCode(t, isoRequest(t, srv, "POST", "/api/agents/revoke", H, map[string]any{"agent_id": "acme-01"}), http.StatusForbidden, "他租户 admin 吊销 acme 的证书")
		isoWaitOnline(t, r, "acme-01", true)
		isoWantCode(t, isoRequest(t, srv, "POST", "/api/tasks", A, isoTaskBody("t-after-revoke", "acme-01")), http.StatusAccepted, "吊销未遂后 acme 仍可运维自己的 Agent")
		isoWantCode(t, isoRequest(t, srv, "POST", "/api/agents/revoke", ADM, map[string]any{"agent_id": "fresh-kiosk"}), http.StatusOK, "全局 admin 吊销(admin 域 Agent)")
		isoWaitOnline(t, r, "fresh-kiosk", false)
	})

	t.Run("离线缓存_只回本租户连接", func(t *testing.T) {
		// 让 acme-02 真实掉线:关闭它的 WS,Relay 读循环退出并注销
		conns["acme-02"].CloseNow()
		isoWaitOnline(t, r, "acme-02", false)

		// 同租户可以给自己离线的 Agent 缓存任务(离线归属回落持久绑定)
		res := isoWantCode(t, isoRequest(t, srv, "POST", "/api/tasks", A, isoTaskBody("t-offline-1", "acme-02")), http.StatusAccepted, "acme 给自己离线的 Agent 缓存任务")
		if !strings.Contains(fmt.Sprint(res.body["message"]), "离线") {
			t.Errorf("期望走离线缓存分支,得到 %v", res.body)
		}
		isoWantCode(t, isoRequest(t, srv, "POST", "/api/tasks", G, isoTaskBody("t-offline-x", "acme-02")), http.StatusForbidden, "globex 给 acme 的离线 Agent 缓存任务")

		// 顶替者想上线接走缓存任务:一律被拒
		isoExpectRejected(t, isoDial(t, srv, isoDeploy, "acme-02", nil), "部署 Token 想顶替离线的 acme-02")
		isoExpectRejected(t, isoDial(t, srv, G, "acme-02", nil), "globex 凭据想顶替离线的 acme-02")

		// 真正的 acme-02 回来:拿到缓存任务
		c := isoDial(t, srv, A, "acme-02", nil)
		got := isoReadTaskRequests(t, c, 3*time.Second, "t-offline-1")
		if !got {
			t.Fatalf("acme-02 重新上线后应收到缓存任务 t-offline-1")
		}
		isoWaitOnline(t, r, "acme-02", true)
	})

	t.Run("组织禁用即失效", func(t *testing.T) {
		if !r.tenants.SetOrgEnabled("acme", false) {
			t.Fatal("禁用组织失败(凭据自动供给的组织应存在)")
		}
		isoWantCode(t, isoRequest(t, srv, "GET", "/api/agents", A, nil), http.StatusUnauthorized, "组织禁用后 acme 凭据")
		isoWantCode(t, isoRequest(t, srv, "POST", "/api/tasks", A, isoTaskBody("t-x10", "acme-01")), http.StatusUnauthorized, "组织禁用后 acme 下发")
		isoWantCode(t, isoRequest(t, srv, "GET", "/api/agents", G, nil), http.StatusOK, "禁用 acme 不影响 globex")
		r.tenants.SetOrgEnabled("acme", true)
	})
}

// isoReadTaskRequests 在超时内读帧,直到看到 task_id 匹配的 task_request。
func isoReadTaskRequests(t *testing.T, c *websocket.Conn, timeout time.Duration, taskID string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	deadline := time.After(timeout)
	for {
		type frame struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		_, data, err := c.Read(ctx)
		if err != nil {
			return false
		}
		var f frame
		if json.Unmarshal(data, &f) == nil && f.Type == proto.TypeTaskRequest {
			var task struct {
				TaskID string `json:"task_id"`
			}
			if json.Unmarshal(f.Payload, &task) == nil && task.TaskID == taskID {
				return true
			}
		}
		select {
		case <-deadline:
			return false
		default:
		}
	}
}

// isoSubscribe 以 Bearer 订阅全量 SSE,返回 (kind, agent_id) 事件流与停止函数。
func isoSubscribe(t *testing.T, srv *httptest.Server, token string) (<-chan isoEvent, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/events", nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req) //nolint:bodyclose // 长连接响应体由下方订阅 goroutine 在流结束时关闭
	if err != nil {
		cancel()
		t.Fatalf("SSE 订阅失败: %v", err)
	}
	out := make(chan isoEvent, 64)
	go func() {
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		kind := ""
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event:"):
				kind = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				var d struct {
					AgentID string `json:"agent_id"`
				}
				_ = json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &d)
				select {
				case out <- isoEvent{kind: kind, agentID: d.AgentID}:
				default:
				}
			}
		}
		close(out)
	}()
	return out, func() { cancel(); _ = resp.Body.Close() }
}

// TestTenantBindingWithoutPersistence 未启用持久化时,绑定仅存于进程内,
// 顶替防护同样生效(否则关掉持久化就等于关掉隔离)。
func TestTenantBindingWithoutPersistence(t *testing.T) {
	r := NewRelay(Config{Addr: ":0", Token: ":admin:" + isoAdmin + "," + isoAcmeOp, InstallToken: isoDeploy})
	mux := http.NewServeMux()
	r.registerHTTP(mux)
	mux.HandleFunc("/agent", r.handleAgentWS)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	_ = isoDial(t, srv, isoAcmeOp, "acme-01", nil)
	isoWaitOnline(t, r, "acme-01", true)
	isoExpectRejected(t, isoDial(t, srv, isoDeploy, "acme-01", nil), "无持久化:部署 Token 顶替租户 Agent")
}
