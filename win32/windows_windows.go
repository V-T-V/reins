//go:build windows

package win32

import (
	"fmt"
	"strings"
	"syscall"
	"unsafe"
)

var (
	procEnumWindows              = user32.NewProc("EnumWindows")
	procGetWindowTextW           = user32.NewProc("GetWindowTextW")
	procGetClassNameW            = user32.NewProc("GetClassNameW")
	procGetWindowThreadProcessID = user32.NewProc("GetWindowThreadProcessId")
	procGetWindowRect            = user32.NewProc("GetWindowRect")
	procIsWindowVisible          = user32.NewProc("IsWindowVisible")
	procSetForegroundWindow      = user32.NewProc("SetForegroundWindow")
	procShowWindow               = user32.NewProc("ShowWindow")
	procIsIconic                 = user32.NewProc("IsIconic")
)

const (
	swRestore = 9
	swShow    = 5
)

type enumCtx struct {
	cb      func(WindowInfo) bool
	results []WindowInfo
}

// EnumWindows 枚举所有顶层窗口，对每个可见窗口调用 cb。
func EnumWindows(cb func(WindowInfo) bool) ([]WindowInfo, error) {
	ctx := &enumCtx{cb: cb}
	cbPtr := syscall.NewCallback(func(hwnd uintptr, lparam uintptr) uintptr {
		// 只处理可见窗口
		vis, _, _ := procIsWindowVisible.Call(hwnd)
		if vis == 0 {
			return 1 // 继续
		}
		info := WindowInfo{Hwnd: hwnd}
		info.Title = getWindowText(hwnd)
		info.ClassName = getClassName(hwnd)
		info.PID = getPid(hwnd)
		info.Rect = getRect(hwnd)
		if ctx.cb(info) {
			ctx.results = append(ctx.results, info)
			return 1
		}
		return 0 // 停止
	})
	r, _, e := procEnumWindows.Call(cbPtr, 0)
	if r == 0 {
		return nil, fmt.Errorf("EnumWindows 失败: %v", e)
	}
	return ctx.results, nil
}

// FindWindowByProcess 按进程名（不含 .exe，大小写不敏感）查找主窗口。
// 返回面积最大的可见窗口（排除空标题的小工具窗口）。
func FindWindowByProcess(procName string) (WindowInfo, error) {
	procLower := strings.ToLower(strings.TrimSuffix(strings.ToLower(procName), ".exe"))
	var best WindowInfo
	bestArea := 0
	_, err := EnumWindows(func(w WindowInfo) bool {
		if strings.Contains(strings.ToLower(w.Title), procLower) ||
			strings.Contains(strings.ToLower(w.ClassName), procLower) {
			area := w.Rect.Width() * w.Rect.Height()
			if area > bestArea {
				bestArea = area
				best = w
			}
		}
		return true
	})
	if err != nil {
		return WindowInfo{}, err
	}
	if best.Hwnd == 0 {
		return WindowInfo{}, fmt.Errorf("未找到进程 %s 的窗口", procName)
	}
	return best, nil
}

// FindWindowByTitle 按标题（大小写不敏感包含匹配）查找面积最大窗口。
func FindWindowByTitle(titleSub string) (WindowInfo, error) {
	titleLower := strings.ToLower(titleSub)
	var best WindowInfo
	bestArea := 0
	_, err := EnumWindows(func(w WindowInfo) bool {
		if strings.Contains(strings.ToLower(w.Title), titleLower) {
			area := w.Rect.Width() * w.Rect.Height()
			if area > bestArea {
				bestArea = area
				best = w
			}
		}
		return true
	})
	if err != nil {
		return WindowInfo{}, err
	}
	if best.Hwnd == 0 {
		return WindowInfo{}, fmt.Errorf("未找到标题含 %q 的窗口", titleSub)
	}
	return best, nil
}

// FindWindow 精确窗口句柄查找。
func FindWindow(hwnd uintptr) (WindowInfo, error) {
	if hwnd == 0 {
		return WindowInfo{}, fmt.Errorf("hwnd 为 0")
	}
	return WindowInfo{
		Hwnd:      hwnd,
		Title:     getWindowText(hwnd),
		ClassName: getClassName(hwnd),
		PID:       getPid(hwnd),
		Rect:      getRect(hwnd),
	}, nil
}

// Activate 把窗口激活到前台（先恢复最小化）。返回是否成功。
func Activate(hwnd uintptr) error {
	if iconic, _, _ := procIsIconic.Call(hwnd); iconic != 0 {
		procShowWindow.Call(hwnd, swRestore)
	}
	procShowWindow.Call(hwnd, swShow)
	r, _, e := procSetForegroundWindow.Call(hwnd)
	if r == 0 {
		return fmt.Errorf("SetForegroundWindow 失败（可能前台锁）: %v", e)
	}
	return nil
}

// --- 内部辅助 ---

func getWindowText(hwnd uintptr) string {
	buf := make([]uint16, 512)
	n, _, _ := procGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if n == 0 {
		return ""
	}
	return syscall.UTF16ToString(buf[:n])
}

func getClassName(hwnd uintptr) string {
	buf := make([]uint16, 256)
	n, _, _ := procGetClassNameW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if n == 0 {
		return ""
	}
	return syscall.UTF16ToString(buf[:n])
}

func getPid(hwnd uintptr) uint32 {
	var pid uint32
	procGetWindowThreadProcessID.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	return pid
}

func getRect(hwnd uintptr) Rect {
	var r Rect
	procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&r)))
	return r
}
