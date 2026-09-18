package main

// v4.7.2 深度优化测试:
//   1. placeBinary:rename 成功直走 / rename 被拒回退 copy 落位(Defender 根治)
//   2. copyBinaryStream:内容保真(含 >缓冲尺寸)
//   3. 共享直连 client 单例(连接复用的前提)

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPlaceBinaryRenameOK(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "new.exe")
	dst := filepath.Join(dir, "cur.exe")
	os.WriteFile(src, []byte("NEWBIN"), 0755)

	usedCopy, err := placeBinary(os.Rename, src, dst)
	if err != nil {
		t.Fatal(err)
	}
	if usedCopy {
		t.Error("rename 可用时应直走 rename,不进 copy 回退")
	}
	b, _ := os.ReadFile(dst)
	if string(b) != "NEWBIN" {
		t.Errorf("dst 内容不符: %q", b)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("rename 后源应不存在")
	}
}

func TestPlaceBinaryFallbackToCopy(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "new.exe")
	dst := filepath.Join(dir, "cur.exe")
	content := bytes.Repeat([]byte("X"), 300_000) // >64K,跨缓冲
	os.WriteFile(src, content, 0755)

	// 模拟 Defender:rename 一律拒绝
	denied := func(from, to string) error { return os.ErrPermission }
	usedCopy, err := placeBinary(denied, src, dst)
	if err != nil {
		t.Fatalf("copy 回退应成功: %v", err)
	}
	if !usedCopy {
		t.Error("rename 被拒应走 copy 回退")
	}
	b, _ := os.ReadFile(dst)
	if !bytes.Equal(b, content) {
		t.Errorf("copy 内容不符: %d bytes", len(b))
	}
}

func TestPlaceBinaryBothFail(t *testing.T) {
	dir := t.TempDir()
	// 源不存在 → rename 拒 + copy 拒
	_, err := placeBinary(func(a, b string) error { return os.ErrPermission },
		filepath.Join(dir, "nosuch"), filepath.Join(dir, "dst"))
	if err == nil || !strings.Contains(err.Error(), "rename 与 copy 均失败") {
		t.Errorf("双失败应报聚合错误: %v", err)
	}
}

func TestCopyBinaryStreamFidelity(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "s.bin")
	dst := filepath.Join(dir, "d.bin")
	content := bytes.Repeat([]byte{0xA5, 0x00, 0xFF}, 100_000)
	os.WriteFile(src, content, 0644)
	if err := copyBinaryStream(src, dst); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(dst)
	if !bytes.Equal(b, content) {
		t.Errorf("内容失真: got %d bytes", len(b))
	}
	// 覆盖已有目标(O_TRUNC)
	if err := copyBinaryStream(src, dst); err != nil {
		t.Fatal(err)
	}
}
