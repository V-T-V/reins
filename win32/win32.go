// Package win32 封装 reins 所需的 Windows API：窗口枚举、截图、输入。
//
// 跨平台策略：
//   - windows_amd64/windows_386：真实实现（syscall）
//   - 其他平台：stub（返回错误），保证整个项目在任何平台可编译
//
// 这是对 computer-use-rtc/win32.cs 的 Go 等价重写，核心常量与行为对齐：
//   - PrintWindow 用 PW_RENDERFULLCONTENT=0x2（截 Chromium/Electron 不黑屏）
//   - SendInput 鼠标/键盘事件
//   - 窗口枚举与按进程/标题定位
package win32

// 常量对齐 computer-use-rtc/win32.cs。
const (
	pwRenderFullContent = 0x2 // PrintWindow 标志位：截硬件加速内容
)

// Rect 矩形（屏幕坐标）。
type Rect struct {
	Left, Top, Right, Bottom int32
}

// Width/Height 辅助。
func (r Rect) Width() int  { return int(r.Right - r.Left) }
func (r Rect) Height() int { return int(r.Bottom - r.Top) }

// WindowInfo 窗口信息快照。
type WindowInfo struct {
	Hwnd      uintptr
	Title     string
	ClassName string
	PID       uint32
	Rect      Rect
}

// ClickAction 鼠标动作类型。
type ClickAction int

const (
	ClickSingle ClickAction = iota
	ClickDouble
	ClickRight
)
