package main

// 2026 租户加固 · 单元级断言(用到加固后的新签名,不进 HEAD 兼容的验收文件)。

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/V-T-V/reins/proto"
)

// TestParseCredentialsGuards 凭据配置的结构守卫:每一条都是「配置写错 → 凭据被悄悄削弱」的陷阱。
func TestParseCredentialsGuards(t *testing.T) {
	ok := func(tok string) bool {
		_, err := parseCredentials(tok)
		return err == nil
	}
	bad := func(tok, why string) {
		t.Helper()
		if _, err := parseCredentials(tok); err == nil {
			t.Errorf("%s: %q 应被拒绝", why, tok)
		}
	}
	if !ok("legacy-secret-123") {
		t.Error("旧格式纯 secret 应合法")
	}
	if !ok(":admin:qx-admin-secret-1,acme:operator:qx-acme-secret-2,globex:viewer:qx-view-secret-3") {
		t.Error("合法的多凭据应通过")
	}
	if !ok(":admin:qx-op-secret-1,:operator:qx-op-secret-2") {
		t.Error("无租户的 operator(:operator:secret,全局 operator)应合法")
	}
	bad("ab,cd", "含逗号的旧式 token 会被拆成两条,拆出的 ab 单独就能过鉴权")
	bad("qx-a-secret-1,acme:operator:qx-acme-secret-2", "列表里的裸 secret:与被逗号拆开的旧式单 token 无法区分,须写 :admin:secret")
	bad("qx-a-secret-1,acme:operator:qx-b-secret-2,ab,cd", "多凭据里混入裸条目")
	bad("acme:operator", "漏写 secret:会被当成 secret 为字面量 acme:operator 的 admin 凭据")
	bad("acme:boss:qx-secret-1", "非法角色")
	bad("bad tenant:admin:qx-secret-1", "租户 ID 含空格(会用作脚本库子目录名)")
	bad("../x:admin:qx-secret-1", "租户 ID 含路径穿越片段")
	bad("a:admin:qx-secret-1,b:viewer:qx-secret-1", "两条凭据复用同一 secret(secret 是唯一实际凭据)")
	bad("a:admin:qx secret 1", "secret 含空格")
}

// TestConfigValidatePerCredential 配置校验按**每条 secret**计长(原按整串长度,多凭据下会被列表总长掩盖)。
func TestConfigValidatePerCredential(t *testing.T) {
	if err := (Config{Addr: ":7780", Token: ":admin:qx-admin-secret-1,acme:operator:short"}).Validate(); err == nil || !strings.Contains(err.Error(), "过短") {
		t.Errorf("多凭据里的短 secret 应报错,得到 %v", err)
	}
	if err := (Config{Addr: ":7780", Token: ":admin:qx-admin-secret-1,acme:operator:qx-acme-secret-2"}).Validate(); err != nil {
		t.Errorf("合法配置不应报错: %v", err)
	}
	if err := (Config{Addr: ":7780", Token: "ab,cd"}).Validate(); err == nil {
		t.Error("含逗号的 secret 应在配置校验阶段报错")
	}
}

// TestConfigStringNeverEchoesMultiCredential 配置串的多凭据形态不回显任何 secret 片段。
func TestConfigStringNeverEchoesMultiCredential(t *testing.T) {
	s := Config{Addr: ":7780", Token: ":admin:qx-admin-secret-1,acme:operator:qx-acme-secret-2"}.String()
	for _, frag := range []string{"qx-a", "qx-admin", "acme-secret"} {
		if strings.Contains(s, frag) {
			t.Errorf("Config.String() 泄漏了凭据片段 %q: %s", frag, s)
		}
	}
	if !strings.Contains(s, "2 条凭据") {
		t.Errorf("Config.String() 应说明凭据条数: %s", s)
	}
}

// TestViewerMayCallTable viewer 只读白名单的判定表。
func TestViewerMayCallTable(t *testing.T) {
	req := func(method, path string) *http.Request {
		r := httptest.NewRequest(method, path, nil)
		return r
	}
	allowed := []struct{ m, p string }{
		{"GET", "/api/agents"}, {"HEAD", "/api/agents"}, {"GET", "/api/tasks"}, {"GET", "/api/tasks/t-1"},
		{"GET", "/api/tasks/"}, {"GET", "/api/batch/b-1/progress"}, {"GET", "/api/dag"}, {"GET", "/api/dag/d-1"},
		{"GET", "/api/events"}, {"GET", "/api/metrics"}, {"GET", "/api/crons"}, {"GET", "/api/scripts"},
		{"GET", "/api/scripts/a.ps1"}, {"GET", "/api/audit/query"}, {"GET", "/api/audit/export"},
		{"GET", "/api/scheduler/best"}, {"GET", "/api/tools"}, {"GET", "/api/version"},
	}
	for _, c := range allowed {
		if !viewerMayCall(req(c.m, c.p)) {
			t.Errorf("viewer 应可访问 %s %s", c.m, c.p)
		}
	}
	denied := []struct{ m, p string }{
		{"POST", "/api/tasks/stream"}, {"POST", "/api/tasks/cancel"}, {"POST", "/api/tasks/batch"},
		{"POST", "/api/tasks"}, // 方法级不放行:由 enforceRolePolicy 单独特判 + handler 限定任务类型
		{"POST", "/api/shell/open"}, {"DELETE", "/api/scripts/a.ps1"}, {"DELETE", "/api/crons/x"},
		{"GET", "/api/bootstrap"}, {"GET", "/api/tunnel/poll"}, {"GET", "/api/secrets/sec_x"},
		{"GET", "/api/agents/../../etc"}, {"POST", "/api/new-endpoint-added-later"},
	}
	for _, c := range denied {
		if viewerMayCall(req(c.m, c.p)) {
			t.Errorf("viewer 不应可访问 %s %s(默认拒绝)", c.m, c.p)
		}
	}
}

// TestAdmitAgentRegistrationRules 注册准入规则,三种存储形态(bbolt/内存/无持久化)逐一对拍——
// 关掉持久化不等于关掉隔离。
func TestAdmitAgentRegistrationRules(t *testing.T) {
	build := map[string]func(t *testing.T) *Relay{
		"bbolt": func(t *testing.T) *Relay {
			s, err := OpenStore(filepath.Join(t.TempDir(), "t.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			return NewRelay(Config{Token: isoTokenCfg}).WithStore(s)
		},
		"memory": func(t *testing.T) *Relay {
			return NewRelay(Config{Token: isoTokenCfg}).WithStore(NewMemoryStore())
		},
		"no-store": func(t *testing.T) *Relay {
			return NewRelay(Config{Token: isoTokenCfg})
		},
	}
	for name, b := range build {
		t.Run(name, func(t *testing.T) {
			r := b(t)
			conn := func(tenant string, admin bool) *wsConn { return &wsConn{tenant: tenant, adminCred: admin} }

			// ① 无租户连接(部署 Token / 不带凭据的 mTLS)注册全新 ID:接受,且**不**建立绑定
			if err := r.admitAgentRegistration(conn("", false), "free-01"); err != nil {
				t.Fatalf("无租户连接注册全新 ID 应接受: %v", err)
			}
			if _, bound := r.agentBoundTenant("free-01"); bound {
				t.Error("无租户连接不应建立绑定(单 admin 部署零行为变化)")
			}
			// ② 租户凭据认领未绑定的 ID(admin 域 → 租户 的迁移路径):接受并建立绑定
			if err := r.admitAgentRegistration(conn("acme", false), "free-01"); err != nil {
				t.Fatalf("租户凭据认领未绑定 ID 应接受: %v", err)
			}
			if tn, ok := r.agentBoundTenant("free-01"); !ok || tn != "acme" {
				t.Fatalf("认领后归属 = %q,%v,期望 acme", tn, ok)
			}
			// ③ 此后其它租户 / 无租户连接都不得顶替
			if err := r.admitAgentRegistration(conn("globex", false), "free-01"); err == nil {
				t.Error("其它租户不得顶替已绑定的 ID")
			}
			if err := r.admitAgentRegistration(conn("", false), "free-01"); err == nil {
				t.Error("无租户连接(部署 Token)不得顶替已绑定的 ID")
			}
			// ④ 同租户重连:接受
			if err := r.admitAgentRegistration(conn("acme", false), "free-01"); err != nil {
				t.Errorf("同租户重连应接受: %v", err)
			}
			// ⑤ 全局 admin 凭据不受约束,且不改写绑定
			if err := r.admitAgentRegistration(conn("", true), "free-01"); err != nil {
				t.Errorf("admin 凭据注册应不受归属约束: %v", err)
			}
			if tn, _ := r.agentBoundTenant("free-01"); tn != "acme" {
				t.Errorf("admin 注册不得改写绑定: %q", tn)
			}
			// ⑥ 持久化开启时绑定经 store 可查(离线归属的事实来源)
			if r.store != nil {
				if tn, ok := r.store.AgentTenant("free-01"); !ok || tn != "acme" {
					t.Errorf("store 中的绑定 = %q,%v,期望 acme", tn, ok)
				}
			}
		})
	}
}

// TestDispatchTaskTenantGate 投递中央闸:租户任务不经他租户/无租户连接投递,且被拒时不改任务状态。
func TestDispatchTaskTenantGate(t *testing.T) {
	r := NewRelay(Config{}).WithStore(NewMemoryStore())
	task := proto.TaskRequest{TaskID: "g-1", Type: proto.TaskPowerShell, AgentID: "a1", Payload: json.RawMessage(`{"command":"Get-Date"}`)}
	if err := r.store.SaveTask(StoredTask{Request: task, Status: StatusPending, Tenant: "acme"}); err != nil {
		t.Fatal(err)
	}
	for _, connTenant := range []string{"globex", ""} {
		if err := r.dispatchTask(&wsConn{tenant: connTenant}, task); err == nil {
			t.Errorf("租户任务经租户 %q 的连接投递应被拒", connTenant)
		}
	}
	// 被拒的投递必须把任务留在 pending(闸在 CAS 之前)
	if st, _ := r.store.GetTask("g-1"); st == nil || st.Status != StatusPending || st.Attempt != 0 {
		t.Errorf("被拒的投递改动了任务状态: %+v", st)
	}
}

// TestOfflineSubmitActuallyPersists 离线提交必须真的落盘(既有缺陷:回 202「已缓存待投递」却从未保存)。
func TestOfflineSubmitActuallyPersists(t *testing.T) {
	r := NewRelay(Config{Token: ":admin:" + isoAdmin + "," + isoAcmeOp}).WithStore(NewMemoryStore())
	// acme 的 Agent 曾注册过 → 留下绑定;现在离线
	if err := r.admitAgentRegistration(&wsConn{tenant: "acme"}, "acme-off"); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	r.registerHTTP(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	res := isoRequest(t, srv, "POST", "/api/tasks", isoAcmeOp, isoTaskBody("q-1", "acme-off"))
	if res.code != http.StatusAccepted {
		t.Fatalf("给离线的自有 Agent 提交应 202,得到 %d %s", res.code, res.raw)
	}
	st, err := r.store.GetTask("q-1")
	if err != nil || st == nil {
		t.Fatalf("离线提交的任务没有落盘(旧实现:回 202 却静默丢弃): %v", err)
	}
	if st.Status != StatusPending || st.Tenant != "acme" {
		t.Errorf("落盘的任务 = status %q tenant %q,期望 pending/acme", st.Status, st.Tenant)
	}
	// 未启用持久化时如实拒绝,不再假称已缓存
	bare := NewRelay(Config{Token: ":admin:" + isoAdmin + "," + isoAcmeOp})
	_ = bare.admitAgentRegistration(&wsConn{tenant: "acme"}, "acme-off")
	bmux := http.NewServeMux()
	bare.registerHTTP(bmux)
	bsrv := httptest.NewServer(bmux)
	defer bsrv.Close()
	if res := isoRequest(t, bsrv, "POST", "/api/tasks", isoAcmeOp, isoTaskBody("q-2", "acme-off")); res.code != http.StatusServiceUnavailable {
		t.Errorf("无持久化 + 离线 Agent 应 503,得到 %d %s", res.code, res.raw)
	}
}

// TestAuditTenantFilterAndChainCompat 审计按租户过滤;且新增字段不破坏旧记录的 hash chain。
func TestAuditTenantFilterAndChainCompat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")

	// 手工构造一条**旧格式**链(加固前的记录没有 operator/tenant 字段),哈希方案与旧代码逐字节一致:
	// this_hash = sha256(prev_hash + json(entry)),创世 prev 为空串。
	oldEntry := AuditEntry{TS: 1700000000000, Event: "task_submitted", AgentID: "a1", TaskID: "t-old", Type: "powershell"}
	oldData, _ := json.Marshal(oldEntry)
	if strings.Contains(string(oldData), "operator") || strings.Contains(string(oldData), "tenant") {
		t.Fatalf("构造旧格式记录失败(不应含新字段): %s", oldData)
	}
	sum := sha256.Sum256(append([]byte(""), oldData...))
	oldLine, _ := json.Marshal(auditRecord{AuditEntry: oldEntry, PrevHash: "", ThisHash: hex.EncodeToString(sum[:])})
	if err := os.WriteFile(path, append(oldLine, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	// 新二进制**接在旧链后面**续写新格式记录:打开时回放尾部播种 prev_hash。
	// 整条文件(旧格式 1 条 + 新格式 3 条)必须作为一条连续的链通过校验——同时证明两件事:
	// ① 新增字段 omitempty,旧记录 re-marshal 逐字节不变;
	// ② 重启后链续得上(既有缺陷:此前 prev_hash 从空串起步,每次重启链自己断一次)。
	a, err := OpenAuditLog(path)
	if err != nil {
		t.Fatal(err)
	}
	a.Log(AuditEntry{Event: "e1", Tenant: "acme", Operator: "acme/operator", TS: 1})
	a.Log(AuditEntry{Event: "e2", Tenant: "globex", TS: 2})
	a.Log(AuditEntry{Event: "e3", TS: 3}) // admin 域:无 tenant 字段
	a.Close()
	if bad, err := VerifyChain(path); bad != -1 || err != nil {
		t.Fatalf("旧格式记录 + 重启后续写的新格式记录应为一条连续完整的链: bad=%d err=%v", bad, err)
	}
	newPath := path

	// 再重启一次,继续写:仍然连续
	again, err := OpenAuditLog(newPath)
	if err != nil {
		t.Fatal(err)
	}
	again.Log(AuditEntry{Event: "e4", Tenant: "acme", TS: 4})
	again.Close()
	if bad, err := VerifyChain(newPath); bad != -1 || err != nil {
		t.Errorf("二次重启后续写应仍连续: bad=%d err=%v", bad, err)
	}
	// 文件里确实有 5 条,且第 2 条(新二进制写的第一条)接在旧记录之后
	raw, _ := os.ReadFile(newPath)
	if recs := splitLines(raw); len(recs) != 5 {
		t.Errorf("审计文件应有 5 条记录,得到 %d", len(recs))
	} else {
		var second auditRecord
		if err := json.Unmarshal(recs[1], &second); err != nil || second.PrevHash != hex.EncodeToString(sum[:]) {
			t.Errorf("重启后首条记录应接在旧记录之后: err=%v prev=%s", err, second.PrevHash)
		}
	}

	// 导出与查询都按租户过滤;admin(空)全量
	if got := readAuditEntries(newPath, 0, 100, "acme"); len(got) != 2 || got[0].Event != "e1" || got[1].Event != "e4" {
		t.Errorf("导出 tenant=acme = %+v,期望 e1,e4", got)
	}
	if got := readAuditEntries(newPath, 0, 100, "nobody"); len(got) != 0 {
		t.Errorf("导出 tenant=nobody = %+v,期望空", got)
	}
	if got := readAuditEntries(newPath, 0, 100, ""); len(got) != 5 {
		t.Errorf("admin 导出应全量 5 条(含旧记录),得到 %d", len(got))
	}
	if got := queryAuditFile(newPath, auditQueryFilter{Tenant: "globex"}); len(got) != 1 || got[0].Event != "e2" {
		t.Errorf("查询 tenant=globex = %+v,期望仅 e2", got)
	}
	// 过滤与其它条件叠加
	if got := queryAuditFile(newPath, auditQueryFilter{Tenant: "acme", Events: []string{"e2"}}); len(got) != 0 {
		t.Errorf("tenant=acme + event=e2 应为空,得到 %+v", got)
	}
}

// TestScriptLibraryNamespaces 脚本库按租户分命名空间;名字无法越出自己的目录。
func TestScriptLibraryNamespaces(t *testing.T) {
	lib, err := NewScriptLibrary(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := lib.SaveIn("acme", "deploy.ps1", "acme"); err != nil {
		t.Fatal(err)
	}
	if err := lib.SaveIn("globex", "deploy.ps1", "globex"); err != nil { // 同名不同租户互不覆盖
		t.Fatal(err)
	}
	if err := lib.Save("root.ps1", "root"); err != nil {
		t.Fatal(err)
	}
	if got, _ := lib.ReadIn("acme", "deploy.ps1"); got != "acme" {
		t.Errorf("acme 读到 %q", got)
	}
	if got, _ := lib.ReadIn("globex", "deploy.ps1"); got != "globex" {
		t.Errorf("globex 读到 %q", got)
	}
	names := func(infos []ScriptInfo) string {
		s := make([]string, 0, len(infos))
		for _, i := range infos {
			s = append(s, i.Name)
		}
		return strings.Join(s, ",")
	}
	if got := names(lib.ListIn("acme")); got != "deploy.ps1" {
		t.Errorf("acme 列表 = %q", got)
	}
	if got := names(lib.List()); got != "root.ps1" { // 根命名空间看不到租户子目录
		t.Errorf("根命名空间列表 = %q,期望 root.ps1", got)
	}
	for _, bad := range []struct{ scope, name string }{
		{"acme", "../root.ps1"}, {"acme", "globex/deploy.ps1"}, {"acme", "/etc/passwd"}, {"acme", ".."},
		{"../globex", "deploy.ps1"}, {"a/b", "x"}, {"", "globex/deploy.ps1"},
	} {
		if _, err := lib.ReadIn(bad.scope, bad.name); err == nil {
			t.Errorf("scope=%q name=%q 应被拒绝(越出命名空间)", bad.scope, bad.name)
		}
	}
	if err := lib.DeleteIn("globex", "deploy.ps1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := lib.ReadIn("acme", "deploy.ps1"); got != "acme" {
		t.Errorf("删 globex 的脚本不应影响 acme 的,acme 读到 %q", got)
	}
}

// TestSecretStoreTenantBinding Secret 的归属校验在消费 one-time 之前(被拒不烧)。
func TestSecretStoreTenantBinding(t *testing.T) {
	ss := NewSecretStore()
	ref, err := ss.Put("v", time.Hour, true, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ss.GetForTenant(ref, "globex"); ok {
		t.Error("他租户不得读取")
	}
	if v, ok := ss.GetForTenant(ref, "acme"); !ok || v != "v" {
		t.Fatalf("同租户应读到(被拒的读取不得把它烧掉): %q,%v", v, ok)
	}
	if _, ok := ss.GetForTenant(ref, "acme"); ok {
		t.Error("一次性 Secret 应已被合法读取消费")
	}
	// admin 域 Secret:租户不可读
	ref2, _ := ss.Put("root", time.Hour, false, "")
	if _, ok := ss.GetForTenant(ref2, "acme"); ok {
		t.Error("租户不得读 admin 域 Secret")
	}
	if v, ok := ss.GetForTenant(ref2, ""); !ok || v != "root" {
		t.Error("admin 域身份应能读 admin 域 Secret")
	}
	// 无租户身份 = admin 域(全局 admin/operator),可读任意作用域的 Secret
	ref3, _ := ss.Put("tenant-value", time.Hour, false, "acme")
	if v, ok := ss.GetForTenant(ref3, ""); !ok || v != "tenant-value" {
		t.Error("admin 域身份应能读租户作用域的 Secret(admin 本就可见全部)")
	}
	if _, ok := ss.GetForTenant("sec_does_not_exist", "acme"); ok {
		t.Error("不存在的 ref 不应读到")
	}
}

// TestBootstrapSingleCredentialUnchanged 单凭据部署下 bootstrap 回显 == 配置串(向后兼容)。
func TestBootstrapSingleCredentialUnchanged(t *testing.T) {
	r := NewRelay(Config{Addr: ":7780", Token: "my-secret-1234567890"})
	mux := http.NewServeMux()
	r.registerHTTP(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	res := isoRequest(t, srv, "GET", "/api/bootstrap", "my-secret-1234567890", nil)
	if res.body["token"] != "my-secret-1234567890" {
		t.Errorf("单凭据 bootstrap.token = %v,期望与配置一致", res.body["token"])
	}
}

// TestPromMetricsRejectsTenantCredentials /metrics 回退到主凭据时拒绝租户身份。
func TestPromMetricsRejectsTenantCredentials(t *testing.T) {
	r := NewRelay(Config{Addr: ":0", Token: isoTokenCfg})
	mux := http.NewServeMux()
	r.registerHTTP(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	isoWantCode(t, isoRequest(t, srv, "GET", "/metrics", isoGlobOp, nil), http.StatusUnauthorized, "租户凭据抓 /metrics")
	isoWantCode(t, isoRequest(t, srv, "GET", "/metrics", isoAcmeOp, nil), http.StatusUnauthorized, "租户凭据(另一租户)抓 /metrics")
	isoWantCode(t, isoRequest(t, srv, "GET", "/metrics", isoAdmin, nil), http.StatusOK, "admin 抓 /metrics")
	isoWantCode(t, isoRequest(t, srv, "GET", "/metrics", "wrong", nil), http.StatusUnauthorized, "错误凭据抓 /metrics")
}

// TestConfigNeverLeaksIntoStructuredLog Config 进结构化日志时不得带出任何 secret。
// 既有泄漏:启动日志把 Config **结构体**交给 slog,JSON handler 把 Token(多凭据下是全部
// 租户的 secret)/InstallToken/MetricsToken 明文序列化进日志;日志被收进 journald/
// 容器日志/SIEM 即凭据外泄。LogValue 在类型层面封死——JSON 与 Text 两种 handler 都验。
func TestConfigNeverLeaksIntoStructuredLog(t *testing.T) {
	cfg := Config{
		Addr:         ":7780",
		Token:        ":admin:qx-admin-secret-1,acme:operator:qx-acme-secret-2",
		InstallToken: "qx-deploy-secret-3",
		MetricsToken: "qx-metrics-secret-4",
	}
	secrets := []string{"qx-admin-secret-1", "qx-acme-secret-2", "qx-deploy-secret-3", "qx-metrics-secret-4"}
	logged := map[string]*bytes.Buffer{}
	for name, newHandler := range map[string]func(io.Writer) slog.Handler{
		"json": func(w io.Writer) slog.Handler { return slog.NewJSONHandler(w, nil) },
		"text": func(w io.Writer) slog.Handler { return slog.NewTextHandler(w, nil) },
	} {
		buf := &bytes.Buffer{}
		logged[name] = buf
		slog.New(newHandler(buf)).Info("relay 启动", "version", "dev", "config", cfg)
	}
	for name, buf := range logged {
		out := buf.String()
		for _, s := range secrets {
			if strings.Contains(out, s) {
				t.Errorf("%s handler 泄漏了 secret %q: %s", name, s, out)
			}
		}
		if !strings.Contains(out, "2 条凭据") || !strings.Contains(out, "install_token=true") {
			t.Errorf("%s handler 应记录脱敏后的配置摘要: %s", name, out)
		}
	}
	// 单凭据:沿用既有行为只回显首 4 位;部署/指标 token 只报有无
	sbuf := &bytes.Buffer{}
	slog.New(slog.NewJSONHandler(sbuf, nil)).Info("x", "config",
		Config{Addr: ":7780", Token: "qx-admin-secret-1", InstallToken: "qx-deploy-secret-3"})
	if out := sbuf.String(); strings.Contains(out, "qx-admin-secret-1") || strings.Contains(out, "qx-deploy-secret-3") || !strings.Contains(out, "qx-a***") {
		t.Errorf("单凭据配置应只回显首 4 位且不回显部署 token: %s", out)
	}
}
