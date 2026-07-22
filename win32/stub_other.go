//go:build !windows

// 非 Windows 平台的 stub 实现，保证项目可跨平台编译。
// 运行到这些函数会返回 ErrNotSupported。

package win32

import "errors"

// ErrNotSupported 当前平台不支持此操作。
var ErrNotSupported = errors.New("win32 操作仅在 Windows 上可用")

func ScreenSize() (int, int, error)         { return 0, 0, ErrNotSupported }
func ClickAt(int, int, ClickAction) error   { return ErrNotSupported }
func MoveTo(int, int) error                 { return ErrNotSupported }
func GetCursorPos() (int, int, error)       { return 0, 0, ErrNotSupported }
func TypeText(string) error                 { return ErrNotSupported }
func EnumWindows(func(WindowInfo) bool) ([]WindowInfo, error) { return nil, ErrNotSupported }
func FindWindowByProcess(string) (WindowInfo, error)          { return WindowInfo{}, ErrNotSupported }
func FindWindowByTitle(string) (WindowInfo, error)            { return WindowInfo{}, ErrNotSupported }
func FindWindow(uintptr) (WindowInfo, error)                  { return WindowInfo{}, ErrNotSupported }
func Activate(uintptr) error                                  { return ErrNotSupported }
func CaptureWindow(uintptr, bool) ([]byte, error)             { return nil, ErrNotSupported }
