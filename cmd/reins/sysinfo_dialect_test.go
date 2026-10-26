package main

import (
	"strings"
	"testing"
)

// TestBuildSysinfoScriptDialects sysinfo 双方言脚本构造:
// 两版输出同构 JSON(统一解析),方言特征命令正确。
func TestBuildSysinfoScriptDialects(t *testing.T) {
	sh := buildSysinfoScript(true)
	ps := buildSysinfoScript(false)

	for _, want := range []string{"/proc/meminfo", "df -B1G", "uname -s", "hostname", "printf '{\"hostname\""} {
		if !strings.Contains(sh, want) {
			t.Fatalf("sh 版应含 %q:\n%s", want, sh)
		}
	}
	if strings.Contains(sh, "Get-") {
		t.Fatal("sh 版不应含 PowerShell 命令")
	}
	for _, want := range []string{"Get-CimInstance", "Get-Counter", "ConvertTo-Json", "mem_total_gb", "disks"} {
		if !strings.Contains(ps, want) {
			t.Fatalf("PS 版应含 %q", want)
		}
	}
	// 同构 JSON 字段:两版都要产出统一字段集(解析端契约)
	for _, field := range []string{"cpu_pct", "mem_total_gb", "mem_free_gb", "mem_used_pct", "uptime_days", "disks"} {
		if !strings.Contains(sh, field) || !strings.Contains(ps, field) {
			t.Fatalf("双方言都应产出字段 %s", field)
		}
	}
}
