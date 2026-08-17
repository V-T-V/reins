package cmdpolicy

import (
	"strings"
	"testing"
)

// TestAllowlistSemantics 白名单核心语义:
// 1) 命中白名单 + 不碰黑名单 → 放行
// 2) 未命中白名单 → 拒绝(即使原本黑名单会放行)
// 3) 命中白名单但碰黑名单 → 仍拒绝(交集,白名单写宽也有黑名单兜底)
func TestAllowlistSemantics(t *testing.T) {
	p := Default()
	if err := p.SetAllowlist([]string{`^Get-`, `^node\s`}); err != nil {
		t.Fatalf("设置白名单失败: %v", err)
	}
	if !p.AllowlistEnabled() {
		t.Fatal("应处于白名单模式")
	}

	if err := p.CheckCommand("Get-Process"); err != nil {
		t.Fatalf("白名单命中的命令应放行: %v", err)
	}
	if err := p.CheckCommand("node app.js"); err != nil {
		t.Fatalf("白名单命中的命令应放行: %v", err)
	}

	// 未命中白名单:python 命令黑名单本会放行,白名单模式下拒绝
	if err := p.CheckCommand("python -c 'print(1)'"); err == nil {
		t.Fatal("未命中白名单的命令应被拒绝(allow-only)")
	}

	// 交集语义:^Get- 白名单很宽,但 Get-Content NTUSER.DAT 碰黑名单 → 仍拒
	if err := p.CheckCommand("Get-Content C:\\Users\\x\\NTUSER.DAT"); err == nil {
		t.Fatal("白名单命中但黑名单兜底的命令应被拒绝")
	}
}

// TestAllowlistClearAndInvalid 空 slice 清除白名单;非法正则报错。
func TestAllowlistClearAndInvalid(t *testing.T) {
	p := Default()
	if err := p.SetAllowlist([]string{`^Get-`}); err != nil {
		t.Fatalf("设置白名单失败: %v", err)
	}
	if err := p.SetAllowlist(nil); err != nil {
		t.Fatalf("清除白名单失败: %v", err)
	}
	if p.AllowlistEnabled() {
		t.Fatal("空白名单应回到纯黑名单模式")
	}
	if err := p.CheckCommand("python -c 'print(1)'"); err != nil {
		t.Fatalf("纯黑名单模式下 python 应放行: %v", err)
	}

	if err := p.SetAllowlist([]string{`^Get-(`}); err == nil {
		t.Fatal("非法正则应返回错误(fail-fast)")
	}
}

// TestEncodedCommandHint EncodedCommand 拦截错误带 script 任务指引
// (使用会话实证:用户以手动 -EncodedCommand 规避内联转义被拦,无从知晓正解)。
func TestEncodedCommandHint(t *testing.T) {
	p := Default()
	err := p.CheckCommand("powershell -EncodedCommand AAAABBBBBBBBBBCCCCCCCCDDDD")
	if err == nil {
		t.Fatal("EncodedCommand 应被黑名单拦截")
	}
	if !strings.Contains(err.Error(), "script 任务") || !strings.Contains(err.Error(), "-s") {
		t.Fatalf("拦截错误应指引 script 任务(-s)替代,实际: %v", err)
	}
}
