package executors

import (
	"context"
	"encoding/json"
	"runtime"
	"strings"
	"testing"

	"github.com/QiuShichang/go-rmm/proto"
)

// scriptPayload 构造 script 任务 JSON 载荷。
func scriptPayload(t *testing.T, lang, content string) []byte {
	t.Helper()
	data, err := json.Marshal(proto.ScriptTask{Lang: lang, Content: content})
	if err != nil {
		t.Fatalf("构造 payload 失败: %v", err)
	}
	return data
}

// TestScriptLangPlatformDispatch script 执行器的平台分派:
// ps1/bat 仅 Windows、sh 仅 POSIX——错误信息指明可用 lang(防误用排查弯路)。
func TestScriptLangPlatformDispatch(t *testing.T) {
	e := NewScript()
	isWindows := runtime.GOOS == "windows"

	for _, lang := range []string{"ps1", "bat"} {
		r := e.Exec(context.Background(), scriptPayload(t, lang, "echo x"), nil)
		if isWindows {
			if !r.Success {
				t.Fatalf("Windows 上 lang=%s 应可用: %v", lang, r.Error)
			}
		} else {
			if r.Success || !strings.Contains(r.Error, "lang=sh") {
				t.Fatalf("非 Windows 上 lang=%s 应拒绝并指引 lang=sh: %+v", lang, r)
			}
		}
	}

	r := e.Exec(context.Background(), scriptPayload(t, "sh", "echo hi"), nil)
	if isWindows {
		if r.Success || !strings.Contains(r.Error, "lang=ps1") {
			t.Fatalf("Windows 上 lang=sh 应拒绝并指引 lang=ps1: %+v", r)
		}
	} else {
		if !r.Success || !strings.Contains(r.Stdout, "hi") {
			t.Fatalf("POSIX 上 lang=sh 应执行成功: %+v", r)
		}
	}
}

// TestScriptUnknownLang 未知 lang 拒绝(fail-closed)。
func TestScriptUnknownLang(t *testing.T) {
	e := NewScript()
	r := e.Exec(context.Background(), scriptPayload(t, "python", "print(1)"), nil)
	if r.Success || !strings.Contains(r.Error, "不支持的 lang") {
		t.Fatalf("未知 lang 应拒绝: %+v", r)
	}
}
