package executors

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/QiuShichang/go-rmm/proto"
)

// mkRulesFile 在 dir 下创建 .go-rmm/rules.md,返回项目目录。
func mkRulesFile(t *testing.T, dir, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, RulesDirName), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, RulesDirName, RulesFileName), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestRulesAckForContent(t *testing.T) {
	ack := RulesAckForContent([]byte("规则内容"))
	if len(ack) != 16 {
		t.Fatalf("ack 应为 16 hex 字符,实际 %q", ack)
	}
	if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(ack) {
		t.Fatalf("ack 应为小写 hex: %q", ack)
	}
	// 确定性 + 内容敏感
	if RulesAckForContent([]byte("规则内容")) != ack {
		t.Fatal("同内容 ack 应一致")
	}
	if RulesAckForContent([]byte("规则内容2")) == ack {
		t.Fatal("不同内容 ack 应不同")
	}
}

func TestRulesAckCombined(t *testing.T) {
	a := RuleFileEntry{Path: `D:\b`, SHA16: "bbbbbbbbbbbbbbbb"}
	b := RuleFileEntry{Path: `D:\a`, SHA16: "aaaaaaaaaaaaaaaa"}
	// 单文件:组合指纹退化为自身
	if got := RulesAckCombined([]RuleFileEntry{a}); got != a.SHA16 {
		t.Fatalf("单文件组合指纹应等于自身,实际 %s", got)
	}
	// 多文件:与顺序无关(内部按路径排序)
	c1 := RulesAckCombined([]RuleFileEntry{a, b})
	c2 := RulesAckCombined([]RuleFileEntry{b, a})
	if c1 == "" || c1 != c2 {
		t.Fatalf("多文件组合指纹应非空且顺序无关: %q vs %q", c1, c2)
	}
	if c1 == a.SHA16 || c1 == b.SHA16 {
		t.Fatal("多文件组合指纹不应等于任一单文件指纹")
	}
}

func TestDiscoverRuleFiles(t *testing.T) {
	root := t.TempDir()
	// 深度 0: root/.go-rmm/rules.md
	mkRulesFile(t, root, "root project")
	// 深度 1: root/proj1/.go-rmm/rules.md
	mkRulesFile(t, filepath.Join(root, "proj1"), "p1")
	// 深度 2: root/group/proj2/.go-rmm/rules.md
	mkRulesFile(t, filepath.Join(root, "group", "proj2"), "p2")
	// 深度 3: 超出发现半径,不应出现
	mkRulesFile(t, filepath.Join(root, "group", "deep", "proj3"), "p3")
	// 跳过目录: root/node_modules/hidden/.go-rmm/rules.md
	mkRulesFile(t, filepath.Join(root, "node_modules", "hidden"), "skipme")

	found := DiscoverRuleFiles([]string{root})
	joined := strings.Join(found, ";")
	for _, want := range []string{
		filepath.Join(root, RulesDirName, RulesFileName),
		filepath.Join(root, "proj1"),
		filepath.Join(root, "group", "proj2"),
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("应发现 %s,实际 %v", want, found)
		}
	}
	if strings.Contains(joined, "proj3") {
		t.Error("深度 3 的规则文件不应被发现")
	}
	if strings.Contains(joined, "node_modules") {
		t.Error("node_modules 内的规则文件应被跳过")
	}
	// 排序确定性
	for i := 1; i < len(found); i++ {
		if found[i-1] > found[i] {
			t.Fatalf("发现结果应按字典序排序: %v", found)
		}
	}
}

func TestRulesGateCheck(t *testing.T) {
	root := t.TempDir()
	mkRulesFile(t, filepath.Join(root, "proj"), "# 规则\n勿在营业时段杀壳")

	gate := NewRulesGate(true, []string{root}, "")
	gate.Refresh() // 强制重扫,绕过 TTL 缓存初值

	combined, singles := gate.CurrentAcks()
	if combined == "" || len(singles) != 1 {
		t.Fatalf("应算出组合指纹与 1 个单文件指纹,实际 %q %v", combined, singles)
	}

	// 空 ack / 错 ack → 拒绝,且拒绝信息不泄露当前指纹
	for _, bad := range []string{"", "deadbeefdeadbeef", combined + "x"} {
		err := gate.Check(proto.TaskPowerShell, bad)
		if err == nil {
			t.Fatalf("ack=%q 应被拒绝", bad)
		}
		if strings.Contains(err.Error(), combined) || strings.Contains(err.Error(), singles[0]) {
			t.Fatal("拒绝信息不应回显当前指纹(防跳过读规则直接重放)")
		}
		if !strings.Contains(err.Error(), "rules read") {
			t.Fatal("拒绝信息应指引 rules read 命令")
		}
	}
	// 正确单文件 ack / 组合 ack → 放行
	if err := gate.Check(proto.TaskPowerShell, singles[0]); err != nil {
		t.Fatalf("正确单文件 ack 应放行: %v", err)
	}
	if err := gate.Check(proto.TaskScript, combined); err != nil {
		t.Fatalf("组合 ack 应放行: %v", err)
	}
	// 豁免类型:file/screenshot/monitor 不拦(读规则与只读观测)
	for _, typ := range []string{proto.TaskFile, proto.TaskScreenshot, proto.TaskMonitor} {
		if err := gate.Check(typ, ""); err != nil {
			t.Fatalf("%s 应豁免门禁: %v", typ, err)
		}
	}
	// 无规则文件 → 不拦
	empty := NewRulesGate(true, []string{t.TempDir()}, "")
	empty.Refresh()
	if err := empty.Check(proto.TaskPowerShell, ""); err != nil {
		t.Fatalf("无规则文件时不应拦截: %v", err)
	}
	// 门禁关闭 → 不拦
	disabled := NewRulesGate(false, []string{root}, "")
	disabled.Refresh()
	if err := disabled.Check(proto.TaskPowerShell, ""); err != nil {
		t.Fatalf("门禁关闭不应拦截: %v", err)
	}
}

// fakeExec 测试用执行器:直接返回成功。
type fakeExec struct{ typ string }

func (f *fakeExec) Type() string { return f.typ }
func (f *fakeExec) Exec(ctx context.Context, payload []byte, p ProgressFunc) proto.TaskResult {
	return proto.TaskResult{TaskID: "t", Success: true, ExitCode: 0}
}

func TestDispatcherRulesGate(t *testing.T) {
	root := t.TempDir()
	mkRulesFile(t, filepath.Join(root, "proj"), "# 规则")

	d := New()
	d.Register(&fakeExec{typ: proto.TaskScript})
	gate := NewRulesGate(true, []string{root}, "")
	gate.Refresh()
	d.SetRulesGate(gate)

	_, singles := gate.CurrentAcks()

	// 无 ack → Dispatcher 拒绝(不执行)
	r := d.Dispatch(context.Background(), proto.TaskRequest{
		TaskID: "t1", Type: proto.TaskScript,
		Payload: mustJSON(t, proto.ScriptTask{Lang: "ps1", Content: "echo hi"}),
	}, nil)
	if r.Success {
		t.Fatal("无 ack 的变更类任务应被门禁拒绝")
	}
	if !strings.Contains(r.Error, "规则门禁") {
		t.Fatalf("错误应说明规则门禁: %s", r.Error)
	}

	// 正确 ack → 执行
	r = d.Dispatch(context.Background(), proto.TaskRequest{
		TaskID: "t2", Type: proto.TaskScript, RulesAck: singles[0],
		Payload: mustJSON(t, proto.ScriptTask{Lang: "ps1", Content: "echo hi"}),
	}, nil)
	if !r.Success {
		t.Fatalf("正确 ack 应执行: %+v", r)
	}

	// file 任务豁免:无需 ack 直接执行
	d.Register(&fakeExec{typ: proto.TaskFile})
	r = d.Dispatch(context.Background(), proto.TaskRequest{
		TaskID: "t3", Type: proto.TaskFile, Payload: mustJSON(t, proto.FileTask{Op: "rules"}),
	}, nil)
	if !r.Success {
		t.Fatalf("file 任务应豁免门禁: %+v", r)
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestFileExecutorRules(t *testing.T) {
	root := t.TempDir()
	proj := mkRulesFile(t, filepath.Join(root, "myapp"), "# 项目规则\n勿杀壳进程")
	e := NewFile(WithAllowedRoots(root))

	// 指定项目目录:读该目录的规则
	r := e.Exec(context.Background(), mustJSON(t, proto.FileTask{Op: "rules", Path: proj}), nil)
	if !r.Success {
		t.Fatalf("op=rules 应成功: %s", r.Error)
	}
	var data struct {
		Files []RuleFileEntry `json:"files"`
		Ack   string          `json:"ack"`
	}
	if err := json.Unmarshal(r.Data, &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Files) != 1 || !data.Files[0].Exists || data.Files[0].Content != "# 项目规则\n勿杀壳进程" {
		t.Fatalf("应读到规则全文: %+v", data.Files)
	}
	if data.Ack == "" {
		t.Fatal("应返回非空 ack")
	}

	// 指定无规则的项目目录(允许根内):exists=false 而非失败
	bare := filepath.Join(root, "bare")
	if err := os.MkdirAll(bare, 0755); err != nil {
		t.Fatal(err)
	}
	r = e.Exec(context.Background(), mustJSON(t, proto.FileTask{Op: "rules", Path: bare}), nil)
	if !r.Success {
		t.Fatalf("无规则目录应成功返回 exists=false: %s", r.Error)
	}
	if err := json.Unmarshal(r.Data, &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Files) != 1 || data.Files[0].Exists {
		t.Fatalf("无规则目录应返回 exists=false: %+v", data.Files)
	}

	// 自动发现模式
	r = e.Exec(context.Background(), mustJSON(t, proto.FileTask{Op: "rules"}), nil)
	if !r.Success {
		t.Fatalf("自动发现应成功: %s", r.Error)
	}
	if err := json.Unmarshal(r.Data, &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Files) != 1 || data.Files[0].Path == "" {
		t.Fatalf("自动发现应找到 myapp 规则: %+v", data.Files)
	}

	// 越界目录:FilePolicy 拒绝
	r = e.Exec(context.Background(), mustJSON(t, proto.FileTask{Op: "rules", Path: `Z:\elsewhere`}), nil)
	if r.Success {
		t.Fatal("越界目录应被 FilePolicy 拒绝")
	}
}

// TestRulesGateFileChangeInvalidatesAck 规则内容变更后旧 ack 立即失效
// (Refresh 重扫后指纹变化)——"规则改了必须重新读"的核心机制。
func TestRulesGateFileChangeInvalidatesAck(t *testing.T) {
	root := t.TempDir()
	proj := mkRulesFile(t, filepath.Join(root, "proj"), "v1 规则")

	gate := NewRulesGate(true, []string{root}, "")
	gate.Refresh()
	_, singles := gate.CurrentAcks()
	if err := gate.Check(proto.TaskPowerShell, singles[0]); err != nil {
		t.Fatalf("变更前 ack 应有效: %v", err)
	}

	// 规则文件变更(v1→v2)
	if err := os.WriteFile(filepath.Join(proj, RulesDirName, RulesFileName), []byte("v2 规则"), 0644); err != nil {
		t.Fatal(err)
	}
	gate.Refresh()
	if err := gate.Check(proto.TaskPowerShell, singles[0]); err == nil {
		t.Fatal("规则变更后旧 ack 应失效")
	}
	_, singles = gate.CurrentAcks()
	if err := gate.Check(proto.TaskPowerShell, singles[0]); err != nil {
		t.Fatalf("新 ack 应有效: %v", err)
	}
}
