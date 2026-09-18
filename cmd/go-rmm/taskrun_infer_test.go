package main

// v4.7.4 A 项测试:-s 自动 script 类型推断 + 默认超时提升。

import (
	"testing"

	"github.com/QiuShichang/go-rmm/proto"
)

func TestInferScriptDefaults(t *testing.T) {
	cases := []struct {
		name                                   string
		typeGiven, timeoutGiven                bool
		taskType, script, command, payloadFile string
		timeout                                int
		wantType                               string
		wantTimeout                            int
	}{
		{"-s 单独出现", false, false, "powershell", "x.ps1", "", "", 60, proto.TaskScript, 120},
		{"显式 -t 优先", true, false, "powershell", "x.ps1", "", "", 60, "powershell", 60},
		{"显式 --timeout 优先", false, true, "powershell", "x.ps1", "", "", 300, proto.TaskScript, 300},
		{"-c 同时给出不推断", false, false, "powershell", "x.ps1", "Get-X", "", 60, "powershell", 60},
		{"无 -s 不推断", false, false, "powershell", "", "Get-X", "", 60, "powershell", 60},
		{"已非默认超时不提升", false, false, "powershell", "x.ps1", "", "", 90, proto.TaskScript, 90},
	}
	for _, c := range cases {
		gotType, gotTO := inferScriptDefaults(c.typeGiven, c.timeoutGiven, c.taskType, c.script, c.command, c.payloadFile, c.timeout)
		if gotType != c.wantType || gotTO != c.wantTimeout {
			t.Errorf("%s: got (%s,%d) want (%s,%d)", c.name, gotType, gotTO, c.wantType, c.wantTimeout)
		}
	}
}
