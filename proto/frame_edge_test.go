package proto

import (
	"strings"
	"testing"
)

// === 极端边界测试 ===

func TestParseBinaryFrameMaxLengthID(t *testing.T) {
	// v4.7.5 命名空间契约:旧格式 id_len 上限 127(首字节 >= 0x80 是隧道标记
	// 命名空间;fuzz 逮住 >=128 的歧义帧会被误判为隧道格式后修订)
	maxID := strings.Repeat("a", 127)
	frame := BuildBinaryFrame(maxID, true, []byte("data"))
	tid, isLast, data, err := ParseBinaryFrame(frame)
	if err != nil {
		t.Fatalf("Parse 失败: %v", err)
	}
	if tid != maxID {
		t.Error("127 字节 ID 应完整保留")
	}
	if !isLast || string(data) != "data" {
		t.Error("isLast/data 不匹配")
	}
	// 超长 ID 截断到 127(防御性兜底,内部 ID 均远短于此)
	over := BuildBinaryFrame(strings.Repeat("b", 300), true, nil)
	tid2, _, _, err2 := ParseBinaryFrame(over)
	if err2 != nil || len(tid2) != 127 {
		t.Errorf("300 字节 ID 应截断到 127: err=%v len=%d", err2, len(tid2))
	}
	// 歧义帧(id_len >= 0x80 且次字节 0x10/0x11):必须显式拒绝而非误判隧道
	amb := append([]byte{0x91, 0x11}, []byte("garbage")...)
	if _, _, _, err := ParseBinaryFrame(amb); err == nil {
		t.Error("id_len >= 0x80 的旧格式帧应被拒绝(与隧道标记冲突)")
	}
}

func TestParseBinaryFrameTruncatedID(t *testing.T) {
	// 声称 id_len=10 但只有 3 字节 ID
	frame := []byte{10, 'a', 'b', 'c'}
	_, _, _, err := ParseBinaryFrame(frame)
	if err == nil {
		t.Error("截断的 ID 应报错")
	}
}

func TestParseBinaryFrameZeroLength(t *testing.T) {
	// id_len=0(空 ID)
	frame := BuildBinaryFrame("", true, []byte("x"))
	tid, _, data, err := ParseBinaryFrame(frame)
	if err != nil {
		t.Fatalf("空 ID 应合法: %v", err)
	}
	if tid != "" {
		t.Errorf("tid = %s, 期望空", tid)
	}
	if string(data) != "x" {
		t.Errorf("data = %s", string(data))
	}
}

func TestParseBinaryFrameLargeData(t *testing.T) {
	// 大 payload(1MB)
	largeData := make([]byte, 1024*1024)
	for i := range largeData {
		largeData[i] = byte(i % 256)
	}
	frame := BuildBinaryFrame("big", false, largeData)
	tid, isLast, data, err := ParseBinaryFrame(frame)
	if err != nil {
		t.Fatalf("大 payload 失败: %v", err)
	}
	if tid != "big" || isLast {
		t.Error("字段不匹配")
	}
	if len(data) != len(largeData) {
		t.Errorf("data len = %d, 期望 %d", len(data), len(largeData))
	}
}

// === Tunnel Frame 边界 ===

func TestParseTunnelFrameMinLength(t *testing.T) {
	// 最小合法隧道帧
	frame := BuildTunnelFrame(ChanTypeTunnelData, "t", 0, nil)
	tid, chanType, _, _, err := ParseTunnelFrame(frame)
	if err != nil {
		t.Fatalf("最小隧道帧应合法: %v", err)
	}
	if tid != "t" {
		t.Errorf("tid = %s", tid)
	}
	if chanType != ChanTypeTunnelData {
		t.Errorf("chanType = %d", chanType)
	}
}

func TestParseTunnelFrameTooShort(t *testing.T) {
	// 太短(缺字段)
	short := []byte{0x80, 0x10, 0x01} // marker + chanType + idLen 但无 id
	_, _, _, _, err := ParseTunnelFrame(short)
	if err == nil {
		t.Error("太短的隧道帧应报错")
	}
}

func TestIsTunnelFrameEmpty(t *testing.T) {
	if IsTunnelFrame(nil) {
		t.Error("nil 不应是隧道帧")
	}
	if IsTunnelFrame([]byte{}) {
		t.Error("空切片不应是隧道帧")
	}
}

func TestParseBinaryFrameOnTunnelLarge(t *testing.T) {
	// ParseBinaryFrame 对大隧道帧也能处理
	largeData := make([]byte, 64*1024)
	frame := BuildTunnelFrame(ChanTypeTunnelData, "tun-large", 0, largeData)
	tid, _, data, err := ParseBinaryFrame(frame)
	if err != nil {
		t.Fatalf("大隧道帧 ParseBinaryFrame 失败: %v", err)
	}
	if tid != "tun-large" {
		t.Errorf("tid = %s", tid)
	}
	if len(data) != len(largeData) {
		t.Errorf("data len = %d", len(data))
	}
}

// === 消息类型互斥性 ===

func TestTaskTypeNotOverlap(t *testing.T) {
	// 确保所有任务类型字符串互不相同
	seen := map[string]bool{}
	types := []string{
		TaskPowerShell, TaskScript, TaskFile, TaskScreenshot,
		TaskBrowse, TaskGUIClick, TaskUpdate, TaskPower, TaskMonitor,
	}
	for _, tt := range types {
		if seen[tt] {
			t.Errorf("任务类型重复: %s", tt)
		}
		seen[tt] = true
	}
}

func TestStatusConstantsNotOverlap(t *testing.T) {
	statuses := []string{
		StatusPending, StatusDispatched, StatusAcknowledged,
		StatusCompleted, StatusFailed, StatusCancelled,
	}
	seen := map[string]bool{}
	for _, s := range statuses {
		if seen[s] {
			t.Errorf("状态常量重复: %s", s)
		}
		seen[s] = true
	}
}
