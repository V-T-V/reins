package executors

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestFilePolicyTraversalProtection 验证 S5 修复：路径穿越被拦截。
func TestFilePolicyTraversalProtection(t *testing.T) {
	tmpDir := t.TempDir()
	e := NewFile(WithAllowedRoots(tmpDir))

	// 合法路径：在 tmpDir 内
	t.Run("合法路径放行", func(t *testing.T) {
		legal := filepath.Join(tmpDir, "sub", "file.txt")
		if err := e.policy.Check(legal); err != nil {
			t.Fatalf("合法路径应放行，却被拒: %v", err)
		}
	})

	// 穿越：../ 逃逸出 tmpDir
	t.Run("点点点穿越拦截", func(t *testing.T) {
		evil := filepath.Join(tmpDir, "..", "..", "etc", "passwd")
		if err := e.policy.Check(evil); err == nil {
			t.Fatal("穿越路径应被拦截")
		}
	})

	// 系统目录黑名单
	t.Run("系统目录拦截", func(t *testing.T) {
		var sysDir string
		if runtime.GOOS == "windows" {
			sysDir = `C:\Windows\System32\config\SAM`
		} else {
			sysDir = "/etc/shadow"
		}
		if err := e.policy.Check(sysDir); err == nil {
			t.Fatalf("系统目录 %s 应被拦截", sysDir)
		}
	})

	// 私钥/凭据拦截
	t.Run("私钥拦截", func(t *testing.T) {
		keyPath := filepath.Join(tmpDir, "id_rsa")
		// 即使在允许根内，id_rsa 命中黑名单
		if err := e.policy.Check(keyPath); err == nil {
			t.Fatal("id_rsa 应被黑名单拦截")
		}
	})

	// 空策略拒绝所有
	t.Run("无白名单拒绝所有", func(t *testing.T) {
		strict := NewFile() // DefaultFilePolicy，无 AllowedRoots
		if err := strict.policy.Check(filepath.Join(tmpDir, "x.txt")); err == nil {
			t.Fatal("无白名单应拒绝所有路径")
		}
	})

	// AllowAll 放开
	t.Run("AllowAll放行", func(t *testing.T) {
		open := FilePolicy{AllowAll: true}
		if err := open.Check("/anywhere"); err != nil {
			t.Fatalf("AllowAll 应放行: %v", err)
		}
	})

	// D4:相对路径解析到 Workspace 而非 CWD
	t.Run("相对路径解析到Workspace", func(t *testing.T) {
		ws := filepath.Join(tmpDir, "workspace")
		p := FilePolicy{AllowedRoots: []string{ws}, Workspace: ws}
		// 相对路径 "foo.txt" 应解析到 ws/foo.txt 并放行
		if err := p.Check("foo.txt"); err != nil {
			t.Fatalf("相对路径应解析到 Workspace 并放行，却被拒: %v", err)
		}
		// "workspace\foo.txt" 也能放行(在 ws 内)
		if err := p.Check(filepath.Join("workspace", "foo.txt")); err != nil {
			t.Fatalf("workspace 子路径应放行，却被拒: %v", err)
		}
		// 相对路径逃逸仍被拦截
		evil := filepath.Join("..", "..", "etc", "passwd")
		if err := p.Check(evil); err == nil {
			t.Fatal("相对路径逃逸应被拦截")
		}
	})
}

// TestFileExecutorRejectsTraversal 端到端验证：穿越路径的任务被拒绝。
func TestFileExecutorRejectsTraversal(t *testing.T) {
	tmpDir := t.TempDir()
	e := NewFile(WithAllowedRoots(tmpDir))

	// 构造穿越路径的上传任务
	evilPath := filepath.Join(tmpDir, "..", "..", "evil.txt")
	payload, _ := jsonMarshal(map[string]any{
		"op":          "upload",
		"path":        evilPath,
		"content_b64": b64enc("malicious"),
	})
	r := e.Exec(context.TODO(), payload, nil)
	if r.Success {
		t.Fatal("穿越路径的任务应失败")
	}
	if !strings.Contains(r.Error, "允许根") && !strings.Contains(r.Error, "禁止") {
		t.Fatalf("错误信息应提示路径策略，得到: %s", r.Error)
	}
}
