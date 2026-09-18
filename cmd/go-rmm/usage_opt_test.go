package main

// v4.7.3 使用日志驱动的 CLI 优化测试(证据:ZCode 日志 508 次真实调用,
// 12% 失败率,最大来源为 "-c powershell -File x.ps1" 反模式):
//   1. detectFileAntiPattern:本地/远端 -File 检测与非 powershell 免疫
//   2. resultFailureHint:四类高频失败的可执行建议
//   3. envBoolDefault:GO_RMM_INSECURE 语义

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDetectFileAntiPattern(t *testing.T) {
	dir := t.TempDir()
	local := filepath.Join(dir, "diag.ps1")
	os.WriteFile(local, []byte("echo hi"), 0644)

	cases := []struct {
		name, taskType, cmd string
		wantPath            string
		wantLocal           bool
	}{
		{"本地文件", "powershell", "powershell -ExecutionPolicy Bypass -File " + local, local, true},
		{"本地带引号", "powershell", `powershell -File "` + local + `"`, local, true},
		{"远端路径", "powershell", `powershell -File C:\go-rmm\workspace\x.ps1`, `C:\go-rmm\workspace\x.ps1`, false},
		{"反斜杠转义形态", "powershell", `powershell -File C:\\go-rmm\\ws\\x.ps1`, `C:\go-rmm\ws\x.ps1`, false},
		{"非 powershell 免疫", "script", "powershell -File " + local, "", false},
		{"无 -File", "powershell", "Get-Process node", "", false},
		{"非 ps1 后缀", "powershell", "powershell -File C:\\x.txt", "", false},
	}
	for _, c := range cases {
		p, local := detectFileAntiPattern(c.taskType, c.cmd)
		if p != c.wantPath || local != c.wantLocal {
			t.Errorf("%s: got (%q,%v) want (%q,%v)", c.name, p, local, c.wantPath, c.wantLocal)
		}
	}
}

func TestResultFailureHint(t *testing.T) {
	cases := []struct {
		name, stdout, stderr, err string
		want                      bool
	}{
		{"PS语法", "At line:1 char:5\n表达式或语句中", "", "", true},
		{"PS语法英文", "ParserError", "", "", true},
		{"路径策略", "agent 拒绝: 路径策略拒绝: 越界", "", "", true},
		{"占用", "", "该路径正在被另一个传输占用", "", true},
		{"超时错误", "", "", "等待被中断或超时", true},
		{"成功无hint", "ok output", "", "", false},
	}
	for _, c := range cases {
		h := resultFailureHint(c.stdout, c.stderr, c.err)
		if (h != "") != c.want {
			t.Errorf("%s: hint=%q want_nonempty=%v", c.name, h, c.want)
		}
	}
}

func TestEnvBoolDefault(t *testing.T) {
	if envBoolDefault("GO_RMM_TEST_ENV_XYZ") {
		t.Error("未设置时应 false")
	}
	t.Setenv("GO_RMM_TEST_ENV_XYZ", "1")
	if !envBoolDefault("GO_RMM_TEST_ENV_XYZ") {
		t.Error("设置为 1 应 true")
	}
}
