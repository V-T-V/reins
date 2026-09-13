package cmdpolicy

import (
	"strings"
	"testing"
)

// TestBlockedHints 高危命令拦截信息的操作指引(hints)。
// 2026-08-31 loan-kiosk 会话实证:schtasks /create 被拦后无从知晓正解,
// 手工摸索出 Register-ScheduledTask;Stop-Computer 类应引导走平台原生
// power 通道(标签白名单+审计)而非裸命令。
func TestBlockedHints(t *testing.T) {
	p := Default()
	cases := []struct {
		cmd      string
		wantHint []string // 拒绝信息应包含的关键片段(全部)
	}{
		{
			cmd:      `schtasks /create /tn "x" /tr "y" /sc once`,
			wantHint: []string{"Register-ScheduledTask"},
		},
		{
			cmd:      "Stop-Computer -Force",
			wantHint: []string{"power shutdown"},
		},
		{
			cmd:      "Restart-Computer -Force",
			wantHint: []string{"power restart"},
		},
		{
			cmd:      "powershell -EncodedCommand AAAABBBBCCCCDDDDEEEEFFFF",
			wantHint: []string{"script 任务"},
		},
	}
	for _, c := range cases {
		err := p.CheckCommand(c.cmd)
		if err == nil {
			t.Errorf("%q 应被拦截", c.cmd)
			continue
		}
		for _, want := range c.wantHint {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%q 拦截信息应包含 %q,实际: %v", c.cmd, want, err)
			}
		}
	}
}
