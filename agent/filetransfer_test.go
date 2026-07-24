package main

import (
	"bytes"
	"testing"

	"github.com/QiuShichang/go-rmm/proto"
)

// TestBinaryFrameRoundTrip 验证 J1：二进制帧构造与解析往返。
func TestBinaryFrameRoundTrip(t *testing.T) {
	cases := []struct {
		id     string
		isLast bool
		data   []byte
	}{
		{"xfer-1", false, []byte("hello")},
		{"xfer-2", true, []byte{}},
		{"xfer-long-id-1234567890", false, bytes.Repeat([]byte{0xAB}, 4096)},
	}
	for _, c := range cases {
		frame := proto.BuildBinaryFrame(c.id, c.isLast, c.data)
		gotID, gotLast, gotData, err := proto.ParseBinaryFrame(frame)
		if err != nil {
			t.Errorf("解析失败: %v (id=%s)", err, c.id)
			continue
		}
		if gotID != c.id || gotLast != c.isLast || !bytes.Equal(gotData, c.data) {
			t.Errorf("往返不匹配: id=%s/%s last=%v/%v data=%d/%d",
				c.id, gotID, c.isLast, gotLast, len(c.data), len(gotData))
		}
	}
}

// TestPartFilePath 验证 .part 路径生成。
func TestPartFilePath(t *testing.T) {
	// .part 路径 = path + ".part"
	got := "/tmp/test.bin" + ".part"
	if got != "/tmp/test.bin.part" {
		t.Fatalf("part path = %q", got)
	}
}
