//go:build windows

package win32

import (
	"fmt"
	"syscall"
	"unsafe"
)

// --- user32.dll 函数与常量 ---

var (
	user32               = syscall.NewLazyDLL("user32.dll")
	procSendInput        = user32.NewProc("SendInput")
	procGetSystemMetrics = user32.NewProc("GetSystemMetrics")
)

// SendInput 输入类型与标志位。
const (
	inputMouse          = 0
	mouseEventMove      = 0x0001 // MOUSEEVENTF_MOVE
	mouseEventLeftDown  = 0x0002
	mouseEventLeftUp    = 0x0004
	mouseEventRightDown = 0x0008
	mouseEventRightUp   = 0x0010
	mouseEventAbsolute  = 0x8000
)

type mouseInput struct {
	Type      uint32
	Dx, Dy    int32
	MouseData uint32
	Flags     uint32
	Time      uint32
	Extra     uintptr
}

// 屏幕尺寸（虚拟桌面），用于把绝对坐标归一化到 0..65535。
func ScreenSize() (width, height int, err error) {
	// SM_CXSCREEN=0, SM_CYSCREEN=1
	w, _, e := procGetSystemMetrics.Call(0)
	h, _, e2 := procGetSystemMetrics.Call(1)
	if w == 0 || h == 0 {
		return 0, 0, fmt.Errorf("GetSystemMetrics 失败: %v/%v", e, e2)
	}
	return int(w), int(h), nil
}

// ClickAt 在屏幕绝对坐标 (x,y) 执行点击。
// 用绝对坐标 + MOUSEEVENTF_ABSOLUTE，避免 SetCursorPos 前台锁竞态。
func ClickAt(x, y int, action ClickAction) error {
	w, h, err := ScreenSize()
	if err != nil {
		return err
	}
	if w <= 0 || h <= 0 {
		return fmt.Errorf("屏幕尺寸异常 %dx%d", w, h)
	}
	// 归一化到 0..65535
	normX := (int32(x) * 65535) / int32(w)
	normY := (int32(y) * 65535) / int32(h)

	down, up := uint32(mouseEventLeftDown), uint32(mouseEventLeftUp)
	if action == ClickRight {
		down, up = uint32(mouseEventRightDown), uint32(mouseEventRightUp)
	}
	flags := uint32(mouseEventAbsolute | mouseEventMove)

	inputs := []mouseInput{
		{Type: inputMouse, Dx: normX, Dy: normY, Flags: flags | down},
		{Type: inputMouse, Dx: normX, Dy: normY, Flags: flags | up},
	}
	if action == ClickDouble {
		inputs = append(inputs,
			mouseInput{Type: inputMouse, Dx: normX, Dy: normY, Flags: flags | down},
			mouseInput{Type: inputMouse, Dx: normX, Dy: normY, Flags: flags | up},
		)
	}
	return sendInputs(mouseInputsToRaw(inputs))
}

// --- 底层发送 ---

func mouseInputsToRaw(mis []mouseInput) []byte {
	// 每个 mouseInput 作为一条 INPUT 记录：Type(4) + mouseInput(28)
	const size = 4 + 28
	buf := make([]byte, size*len(mis))
	for i, mi := range mis {
		off := i * size
		*(*uint32)(unsafe.Pointer(&buf[off])) = mi.Type
		copy(buf[off+4:], mi.asBytes())
	}
	return buf
}

func (mi mouseInput) asBytes() []byte {
	var b [28]byte
	*(*int32)(unsafe.Pointer(&b[0])) = mi.Dx
	*(*int32)(unsafe.Pointer(&b[4])) = mi.Dy
	*(*uint32)(unsafe.Pointer(&b[8])) = mi.MouseData
	*(*uint32)(unsafe.Pointer(&b[12])) = mi.Flags
	*(*uint32)(unsafe.Pointer(&b[16])) = mi.Time
	*(*uintptr)(unsafe.Pointer(&b[20])) = mi.Extra
	// padding 4 bytes
	return b[:]
}

func sendInputs(buf []byte) error {
	if len(buf) == 0 {
		return nil
	}
	n := len(buf) / (4 + 28)
	r, _, e := procSendInput.Call(uintptr(n), uintptr(unsafe.Pointer(&buf[0])), uintptr(4+28))
	if r != uintptr(n) {
		return fmt.Errorf("SendInput 失败: 注入 %d/%d: %v", r, n, e)
	}
	return nil
}
