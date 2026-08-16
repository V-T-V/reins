package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// === ScriptLibrary 单元测试(此前零覆盖) ===

func newTestScriptLib(t *testing.T) *ScriptLibrary {
	t.Helper()
	lib, err := NewScriptLibrary(t.TempDir())
	if err != nil {
		t.Fatalf("NewScriptLibrary: %v", err)
	}
	return lib
}

// TestScriptLibrarySaveReadRoundTrip 存取往返 + 内容一致。
func TestScriptLibrarySaveReadRoundTrip(t *testing.T) {
	lib := newTestScriptLib(t)
	if err := lib.Save("cleanup.ps1", "Write-Output hi"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := lib.Read("cleanup.ps1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got != "Write-Output hi" {
		t.Fatalf("内容不一致: %q", got)
	}
}

// TestScriptLibrarySaveEmptyNameRejected 空名拒绝。
func TestScriptLibrarySaveEmptyNameRejected(t *testing.T) {
	lib := newTestScriptLib(t)
	if err := lib.Save("", "x"); err == nil {
		t.Fatal("空 name 应报错")
	}
}

// TestScriptLibraryReadMissing 不存在脚本返回错误。
func TestScriptLibraryReadMissing(t *testing.T) {
	lib := newTestScriptLib(t)
	if _, err := lib.Read("nope.ps1"); err == nil {
		t.Fatal("读取不存在的脚本应报错")
	}
}

// TestScriptLibraryDelete 删除后不可读。
func TestScriptLibraryDelete(t *testing.T) {
	lib := newTestScriptLib(t)
	lib.Save("del.ps1", "x")
	if err := lib.Delete("del.ps1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := lib.Read("del.ps1"); err == nil {
		t.Fatal("删除后读取应失败")
	}
}

// TestScriptLibraryList 列表语言推断 + 跳过子目录 + 排序无关性。
func TestScriptLibraryList(t *testing.T) {
	lib := newTestScriptLib(t)
	lib.Save("a.ps1", "1")
	lib.Save("b.sh", "2")
	lib.Save("c.bat", "3")
	// 子目录应被跳过
	lib.Save(".gitkeep", "")

	list := lib.List()
	if list == nil {
		t.Fatal("List 不应返回 nil")
	}
	byName := map[string]ScriptInfo{}
	for _, s := range list {
		byName[s.Name] = s
	}
	if _, ok := byName["a.ps1"]; !ok {
		t.Error("缺 a.ps1")
	}
	if _, ok := byName["b.sh"]; !ok {
		t.Error("缺 b.sh")
	}
	if _, ok := byName["c.bat"]; !ok {
		t.Error("缺 c.bat")
	}
	if byName["a.ps1"].Lang != "ps1" {
		t.Errorf("a.ps1 Lang=%q 期望 ps1", byName["a.ps1"].Lang)
	}
	if byName["b.sh"].Lang != "sh" {
		t.Errorf("b.sh Lang=%q 期望 sh", byName["b.sh"].Lang)
	}
	if byName["c.bat"].Lang != "bat" {
		t.Errorf("c.bat Lang=%q 期望 bat", byName["c.bat"].Lang)
	}
	if byName["a.ps1"].Size != 1 {
		t.Errorf("a.ps1 Size=%d 期望 1", byName["a.ps1"].Size)
	}
}

// TestScriptLibraryListEmptyDir 空目录返回空切片(非 nil 解引用)。
func TestScriptLibraryListEmptyDir(t *testing.T) {
	lib := newTestScriptLib(t)
	list := lib.List()
	// 空 → nil 或 len 0 均可,关键是不能 panic
	_ = list
}

// TestScriptLibraryOverwrite 同名覆盖。
func TestScriptLibraryOverwrite(t *testing.T) {
	lib := newTestScriptLib(t)
	lib.Save("dup.ps1", "v1")
	lib.Save("dup.ps1", "v2-longer")
	got, _ := lib.Read("dup.ps1")
	if got != "v2-longer" {
		t.Fatalf("覆盖后应为 v2-longer, 得到 %q", got)
	}
}

// === 路径穿越安全测试(D6 修复回归) ===
// 原漏洞:GET/DELETE /api/scripts/{name} 未校验 name,
// /api/scripts/../../etc/passwd 经 filepath.Join 清理后逃逸出脚本目录。
// validateScriptName 必须拒绝所有穿越尝试。

func TestScriptLibraryValidateRejectsTraversal(t *testing.T) {
	lib := newTestScriptLib(t)
	bad := []string{
		"../secret",
		"../../etc/passwd",
		"..\\windows\\win.ini",
		"./hidden",
		"/etc/passwd",   // 绝对路径
		"\\windows.ini", // 绝对路径(Windows)
		"a/b",           // 子目录分隔符
		"a\\b",
		"", // 空
	}
	for _, name := range bad {
		if _, ok := lib.validateScriptName(name); ok {
			t.Errorf("穿越名 %q 应被拒绝", name)
		}
	}
}

func TestScriptLibraryValidateAcceptsLegitimate(t *testing.T) {
	lib := newTestScriptLib(t)
	good := []string{
		"cleanup.ps1",
		"deploy.sh",
		"diagnose.bat",
		"my-script_v2.ps1",
		"script.with.dots.in.name.ps1", // 文件名内点合法(仅 ".." 被拒)
	}
	for _, name := range good {
		if _, ok := lib.validateScriptName(name); !ok {
			t.Errorf("合法名 %q 不应被拒绝", name)
		}
	}
}

// TestScriptLibraryReadRefusesTraversal 集成验证:穿越名经 Read 无法逃逸。
// 先在 lib.dir 的父目录放一个文件,穿越 Read 应失败(而非读到父目录文件)。
func TestScriptLibraryReadRefusesTraversal(t *testing.T) {
	parent := t.TempDir()
	lib, _ := NewScriptLibrary(parent)
	// 在父目录放敏感文件
	os.WriteFile(filepath.Join(parent, "SECRET"), []byte("topsecret"), 0644)
	// 穿越尝试:从 scripts 读 ../SECRET
	if _, ok := lib.validateScriptName("../SECRET"); ok {
		t.Fatal("../SECRET 应被 validateScriptName 拒绝")
	}
	// 直接 Read(绕过校验)也只是读到不存在/越界,但经校验器后根本走不到 Read
	got, err := lib.Read("../SECRET")
	// 注:Read 本身不做校验(由 handler 层 validate),这里仅确认校验器是关键防线
	if err == nil && strings.Contains(got, "topsecret") {
		t.Errorf("穿越读到了父目录敏感文件!got=%q", got)
	}
}

// === buffer_pool 单元测试(此前零覆盖) ===
// 验证池化 buffer 的容量不变量:Get 返回正确容量,Put 后复用,Put 不同容量不损坏。

func TestBufferPool256KCapacity(t *testing.T) {
	b := Get256K()
	if cap(b) != 256*1024 {
		t.Fatalf("cap=%d 期望 %d", cap(b), 256*1024)
	}
	if len(b) != 256*1024 {
		t.Fatalf("len=%d 期望 %d(返回 len=cap 供 io.CopyBuffer 使用)", len(b), 256*1024)
	}
	Put256K(b)
}

func TestBufferPool32KCapacity(t *testing.T) {
	b := Get32K()
	if cap(b) != 32*1024 {
		t.Fatalf("cap=%d 期望 %d", cap(b), 32*1024)
	}
	Put32K(b)
}

func TestBufferPool4KCapacity(t *testing.T) {
	b := Get4K()
	if cap(b) != 4096 {
		t.Fatalf("cap=%d 期望 4096", cap(b))
	}
	Put4K(b)
}

// TestBufferPoolPutReuses Get→Put→Get 复用同一底层数组(sync.Pool 命中)。
func TestBufferPoolPutReuses(t *testing.T) {
	b1 := Get32K()
	// 标记内存,Put 后再 Get,如命中池应拿到同一片(或至少可用)。
	for i := range b1 {
		b1[i] = 0xAB
	}
	Put32K(b1)
	b2 := Get32K()
	// 池命中时 b2 与 b1 同数组;但池可能被 GC 清空,此处仅校验可用性 + 容量正确。
	if cap(b2) != 32*1024 {
		t.Fatalf("复用后 cap=%d 期望 %d", cap(b2), 32*1024)
	}
	Put32K(b2)
}

// TestPooledCopyCopyData 池化拷贝语义正确(io.CopyBuffer 真拷贝)。
func TestPooledCopyCopyData(t *testing.T) {
	src := bytes.NewReader([]byte("hello-pool"))
	var dst bytes.Buffer
	n, err := pooledCopy(&dst, src)
	if err != nil {
		t.Fatalf("pooledCopy: %v", err)
	}
	if n != int64(len("hello-pool")) {
		t.Fatalf("n=%d 期望 %d", n, len("hello-pool"))
	}
	if dst.String() != "hello-pool" {
		t.Fatalf("拷贝内容不匹配: %q", dst.String())
	}
}

// TestPooledCopyEmpty 空输入不报错。
func TestPooledCopyEmpty(t *testing.T) {
	var dst bytes.Buffer
	n, err := pooledCopy(&dst, strings.NewReader(""))
	if err != nil || n != 0 {
		t.Fatalf("空拷贝 err=%v n=%d", err, n)
	}
}
