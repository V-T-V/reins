package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/QiuShichang/reins/executors"
)

// withCacheDir 把用户缓存目录重定向到临时目录(ack 缓存测试用)。
func withCacheDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("LocalAppData", dir) // Windows os.UserCacheDir
	t.Setenv("XDG_CACHE_HOME", dir)
	return dir
}

func TestAckCacheRoundtrip(t *testing.T) {
	withCacheDir(t)
	if err := saveAckCache("agent-x", "aabbccddeeff0011"); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if got := loadAckCache("agent-x"); got != "aabbccddeeff0011" {
		t.Fatalf("读取应回放写入值,实际 %q", got)
	}
	if got := loadAckCache("agent-不存在"); got != "" {
		t.Fatalf("无缓存应返回空串,实际 %q", got)
	}
	// agentID 含路径字符时文件名安全化
	if err := saveAckCache("agent-a/b:c", "v"); err != nil {
		t.Fatalf("含分隔符的 agentID 不应炸: %v", err)
	}
	if got := loadAckCache("agent-a/b:c"); got != "v" {
		t.Fatalf("安全化后应可回读,实际 %q", got)
	}
}

func TestLocalRules(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	if ack, _ := localRules(); ack != "" {
		t.Fatal("无规则文件时应返回空")
	}
	content := "# 规则\n部署排除 runtime 目录"
	if err := os.MkdirAll(filepath.Join(dir, ".reins"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, localRulesRelPath), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	ack, got := localRules()
	if ack == "" || got != content {
		t.Fatalf("应读到本地规则: ack=%q content=%q", ack, got)
	}
	if ack != executors.RulesAckForContent([]byte(content)) {
		t.Fatal("本地 ack 应与执行器同公式(同内容即同指纹)")
	}
}

func TestResolveRulesAckPriority(t *testing.T) {
	withCacheDir(t)
	_ = saveAckCache("agent-c", "cached-ack-0000000")

	// 显式最优先
	if got := resolveRulesAck("agent-c", "explicit-ack-0000", false); got != "explicit-ack-0000" {
		t.Fatalf("显式 ack 应最优先,实际 %q", got)
	}
	// 无本地规则 → 缓存
	if got := resolveRulesAck("agent-c", "", false); got != "cached-ack-0000000" {
		t.Fatalf("应回退到缓存,实际 %q", got)
	}
	// 本地规则 > 缓存
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.MkdirAll(filepath.Join(dir, ".reins"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, localRulesRelPath), []byte("# local"), 0644); err != nil {
		t.Fatal(err)
	}
	want := executors.RulesAckForContent([]byte("# local"))
	if got := resolveRulesAck("agent-c", "", false); got != want {
		t.Fatalf("本地规则应优先于缓存: %q vs %q", got, want)
	}
}

func TestTaskSubmitBodyRulesAckOmit(t *testing.T) {
	b, err := json.Marshal(taskSubmitBody{TaskID: "t", Type: "script", AgentID: "a", Timeout: 5})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "rules_ack") {
		t.Fatalf("空 ack 应 omitempty: %s", b)
	}
	b, _ = json.Marshal(taskSubmitBody{TaskID: "t", RulesAck: "abc"})
	if !strings.Contains(string(b), `"rules_ack":"abc"`) {
		t.Fatalf("ack 应序列化: %s", b)
	}
}

// TestBuildPayloadScriptPathGuard -s 传路径形态但文件不存在时硬错误
// (2026-08-31 loan-kiosk 会话实证:Temp 脚本被清理,整条路径被当脚本文本
// 发远端执行,PowerShell ParserError 浪费一轮)。
func TestBuildPayloadScriptPathGuard(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	for _, p := range []string{`C:\tmp\not-exist.ps1`, `C:/tmp/not-exist.ps1`, `.\rel.ps1`, `/abs/x.ps1`} {
		_, err := buildPayload("script", "", p, "")
		if err == nil {
			t.Fatalf("路径形态但文件不存在应报错: %q", p)
		}
		if !strings.Contains(err.Error(), "不存在") {
			t.Fatalf("错误应说明文件不存在: %v", err)
		}
	}

	// 文件存在 → 正常读内容
	good := filepath.Join(dir, "ok.ps1")
	os.WriteFile(good, []byte("Write-Host ok"), 0644)
	payload, err := buildPayload("script", "", good, "")
	if err != nil {
		t.Fatalf("存在的脚本文件应正常: %v", err)
	}
	var st struct {
		Lang    string `json:"lang"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(payload, &st); err != nil {
		t.Fatal(err)
	}
	if st.Lang != "ps1" || st.Content != "Write-Host ok" {
		t.Fatalf("内容读取错误: %+v", st)
	}

	// 多行字面内容(非路径形态)→ 原样发送
	payload, err = buildPayload("script", "", "Write-Host \"C:\\x\"\nWrite-Host done", "")
	if err != nil {
		t.Fatalf("含盘符的命令文本不应被误判为路径: %v", err)
	}
	if err := json.Unmarshal(payload, &st); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(st.Content, "Write-Host done") {
		t.Fatalf("多行内容应原样: %+v", st)
	}
}

func TestLooksLikeLocalPath(t *testing.T) {
	yes := []string{`C:\a.ps1`, `C:/a.ps1`, `\srv\share\x`, `/abs/x`, `./x.ps1`, `.\x.ps1`, `../x`}
	no := []string{"Write-Host \"C:\\x\"", "Get-Date", "多行\nC:\\x", "", "Get-Item C:\\x"}
	for _, s := range yes {
		if !looksLikeLocalPath(s) {
			t.Errorf("%q 应识别为路径形态", s)
		}
	}
	for _, s := range no {
		if looksLikeLocalPath(s) {
			t.Errorf("%q 不应识别为路径形态", s)
		}
	}
}

// TestRulesReadE2E httptest mock Relay 端到端:rules read 读取 → 打印 ack → 缓存。
func TestRulesReadE2E(t *testing.T) {
	withCacheDir(t)
	content := "# 项目规则\n1. 勿在营业时段杀壳\n2. 部署排除 .env"
	ack := executors.RulesAckForContent([]byte(content))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/api/tasks"):
			// 回显真实 task_id,并断言提交的任务是 rules op
			var body struct {
				TaskID  string          `json:"task_id"`
				Type    string          `json:"type"`
				Payload json.RawMessage `json:"payload"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Type != "file" || !strings.Contains(string(body.Payload), `"op":"rules"`) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"expect rules op"}`))
				return
			}
			_, _ = w.Write([]byte(`{"task_id":"` + body.TaskID + `","status":"dispatched"}`))
		case r.Method == "GET" && strings.Contains(r.URL.Path, "/api/tasks/"):
			data, _ := json.Marshal(map[string]any{
				"files": []map[string]any{{
					"path": `D:\myapp\.reins\rules.md`, "bytes": len(content),
					"sha256_16": ack, "exists": true, "content": content,
				}},
				"ack": ack,
			})
			resultJSON, _ := json.Marshal(map[string]any{
				"task_id": "rules-t1", "success": true, "exit_code": 0,
				"data": json.RawMessage(data), // []byte 会被 base64 成字符串,必须 Raw
			})
			_, _ = w.Write([]byte(`{"task":{"status":"completed"},"result":` + string(resultJSON) + `}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	out := captureStdout(t, func() {
		if err := rulesRead(config{relay: srv.URL}, []string{"-a", "agent-e2e"}); err != nil {
			t.Errorf("rules read 应成功: %v", err)
		}
	})
	for _, want := range []string{"项目规则", "勿在营业时段杀壳", "RULES_ACK: " + ack} {
		if !strings.Contains(out, want) {
			t.Errorf("输出应包含 %q,实际:\n%s", want, out)
		}
	}
	if got := loadAckCache("agent-e2e"); got != ack {
		t.Fatalf("ack 应已缓存,实际 %q", got)
	}
}

func TestDeployInitTemplates(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	if err := deployInit([]string{"myapp"}); err != nil {
		t.Fatalf("basic 模板生成失败: %v", err)
	}
	// 已存在不覆盖
	if err := deployInit([]string{"myapp"}); err == nil {
		t.Fatal("重复生成应拒绝")
	}
	// --force 覆盖
	if err := deployInit([]string{"myapp", "--force"}); err != nil {
		t.Fatalf("--force 应覆盖: %v", err)
	}

	if err := deployInit([]string{"node", "--template", "zip-node", "-o", "node.pb.json"}); err != nil {
		t.Fatalf("zip-node 模板生成失败: %v", err)
	}
	data, err := os.ReadFile("node.pb.json")
	if err != nil {
		t.Fatal(err)
	}
	var pb PlayBook
	if err := json.Unmarshal(data, &pb); err != nil {
		t.Fatalf("模板应为合法 playbook JSON: %v", err)
	}
	if len(pb.Steps) != 6 {
		t.Fatalf("zip-node 应 6 步,实际 %d", len(pb.Steps))
	}
	// 关键防坑断言:robocopy 退出码归一化 / npm 步骤长超时 / 健康检查重试
	var hasRobocopyNorm, hasLongTimeout, hasRetry bool
	for _, s := range pb.Steps {
		if strings.Contains(s.Cmd, "$LASTEXITCODE -ge 8") {
			hasRobocopyNorm = true
		}
		if s.Timeout >= 600 {
			hasLongTimeout = true
		}
		if s.Retry >= 5 {
			hasRetry = true
		}
	}
	if !hasRobocopyNorm {
		t.Fatal("zip-node 模板必须包含 robocopy 退出码归一化(0-7 为成功)")
	}
	if !hasLongTimeout {
		t.Fatal("npm/镜像步骤必须有 ≥600s 超时(默认 60s 会中途杀树)")
	}
	if !hasRetry {
		t.Fatal("健康检查步骤必须带重试")
	}
	// 生成的模板应能通过 validate 的解析
	if _, err := loadPlaybook("node.pb.json"); err != nil {
		t.Fatalf("模板应可被 loadPlaybook 解析: %v", err)
	}
}

func TestStepTimeout(t *testing.T) {
	if got := stepTimeout(PlayStep{}, 60); got != 60 {
		t.Fatalf("默认超时应生效,实际 %d", got)
	}
	if got := stepTimeout(PlayStep{Timeout: 900}, 60); got != 900 {
		t.Fatalf("显式超时应优先,实际 %d", got)
	}
}
