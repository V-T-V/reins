package executors

import (
	"context"
	"encoding/base64"
	"runtime"
	"strconv"

	"github.com/QiuShichang/go-rmm/proto"
	"github.com/QiuShichang/go-rmm/win32"
)

// ScreenshotExecutor 截图。
//
// 定位优先级：Hwnd > Target（进程名/标题）> 全屏。
// PrintWindow + PW_RENDERFULLCONTENT 截 Chromium/Electron；失败降级 BitBlt。
type ScreenshotExecutor struct{}

func NewScreenshot() *ScreenshotExecutor { return &ScreenshotExecutor{} }

func (e *ScreenshotExecutor) Type() string { return proto.TaskScreenshot }

func (e *ScreenshotExecutor) Exec(ctx context.Context, payload []byte, _ ProgressFunc) proto.TaskResult {
	if runtime.GOOS != "windows" {
		return fail("", "screenshot 仅在 Windows 上可用")
	}
	var task proto.ScreenshotTask
	if err := decode(payload, &task); err != nil {
		return fail("", err.Error())
	}

	var hwnd uintptr
	originX, originY := 0, 0

	if task.Hwnd != 0 {
		hwnd = uintptr(task.Hwnd)
	} else if task.Target != "" && !task.Full {
		// 按进程名或标题查找
		if w, err := win32.FindWindowByProcess(task.Target); err == nil {
			hwnd = w.Hwnd
			originX = int(w.Rect.Left)
			originY = int(w.Rect.Top)
		} else if w, err := win32.FindWindowByTitle(task.Target); err == nil {
			hwnd = w.Hwnd
			originX = int(w.Rect.Left)
			originY = int(w.Rect.Top)
		}
		// 找不到则降级全屏
	}

	if hwnd != 0 && task.Activate {
		_ = win32.Activate(hwnd)
	}

	pngBytes, err := win32.CaptureWindow(hwnd, task.Full || hwnd == 0)
	if err != nil {
		return fail("", "截图失败: "+err.Error())
	}

	r := ok("")
	r.Data, _ = jsonMarshal(map[string]any{
		"image":    "data:image/png;base64," + base64.StdEncoding.EncodeToString(pngBytes),
		"bytes":    len(pngBytes),
		"origin_x": originX, // 截图左上角在虚拟桌面的偏移（点击坐标换算用）
		"origin_y": originY,
		"target":   task.Target,
		"hwnd":     strconv.FormatUint(uint64(hwnd), 16),
	})
	return r
}
