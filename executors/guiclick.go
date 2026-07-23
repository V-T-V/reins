package executors

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"runtime"
	"time"

	"github.com/QiuShichang/go-rmm/proto"
	"github.com/QiuShichang/go-rmm/vision"
	"github.com/QiuShichang/go-rmm/win32"
)

// GUIClickExecutor 视觉驱动的桌面 GUI 点击。
//
// 复用 computer-use-rtc/cu.mjs 的 "看-判-点-验" agentic 循环：
//
//	截图 → GLM 视觉识别目标元素坐标 → win32.SendInput 点击 → 再截图验证
//	最多 MaxSteps 步，目标达成（模型返回空列表）则提前结束。
type GUIClickExecutor struct {
	client *vision.Client
}

// NewGUIClick 构造执行器。client 可为 nil（此时执行报错，能力上报时探测）。
func NewGUIClick(client *vision.Client) *GUIClickExecutor {
	return &GUIClickExecutor{client: client}
}

// Available 判断执行器是否可用（有 API key + Windows）。
func (e *GUIClickExecutor) Available() bool {
	return runtime.GOOS == "windows" && e.client != nil && e.client.APIKey != ""
}

func (e *GUIClickExecutor) Type() string { return proto.TaskGUIClick }

func (e *GUIClickExecutor) Exec(ctx context.Context, payload []byte, progress ProgressFunc) proto.TaskResult {
	if !e.Available() {
		return fail("", "guiclick 不可用（需 Windows + GLM API key）")
	}
	var task proto.GUIClickTask
	if err := decode(payload, &task); err != nil {
		return fail("", err.Error())
	}
	if task.Intent == "" {
		return fail("", "intent 不能为空")
	}
	maxSteps := task.MaxSteps
	if maxSteps <= 0 {
		maxSteps = 5
	}

	// 定位目标窗口（限定截图范围）
	var hwnd uintptr
	originX, originY := 0, 0
	if task.Target != "" {
		if w, err := win32.FindWindowByProcess(task.Target); err == nil {
			hwnd = w.Hwnd
			originX, originY = int(w.Rect.Left), int(w.Rect.Top)
		} else if w, err := win32.FindWindowByTitle(task.Target); err == nil {
			hwnd = w.Hwnd
			originX, originY = int(w.Rect.Left), int(w.Rect.Top)
		}
	}

	stepsLog := []map[string]any{}
	for step := 1; step <= maxSteps; step++ {
		select {
		case <-ctx.Done():
			return fail("", "超时或取消，已执行 "+fmt.Sprint(step-1)+" 步")
		default:
		}

		// 1. 截图
		if hwnd != 0 {
			_ = win32.Activate(hwnd)
		}
		pngBytes, err := win32.CaptureWindow(hwnd, hwnd == 0)
		if err != nil {
			return fail("", fmt.Sprintf("第 %d 步截图失败: %v", step, err))
		}

		// 2. 视觉识别：判断目标是否达成；未达成返回该点的元素坐标
		intent := task.Intent + "（若目标已达成，返回空数组 []；否则返回下一步该点击的元素）"
		elements, err := e.client.FindElements(ctx, pngBytes, intent)
		if err != nil {
			return fail("", fmt.Sprintf("第 %d 步视觉识别失败: %v", step, err))
		}

		// 目标达成（模型返回空）
		if len(elements) == 0 {
			progress("done", fmt.Sprintf("第 %d 步：目标达成", step), 100)
			stepsLog = append(stepsLog, map[string]any{
				"step": step, "status": "done",
				"screenshot": dataURL(pngBytes),
			})
			r := ok("")
			r.Stdout = fmt.Sprintf("目标达成，共 %d 步", step)
			r.Data, _ = jsonMarshal(map[string]any{"steps": stepsLog})
			return r
		}

		// 3. 点击第一个元素（坐标换算：模型坐标以截图左上为原点 → 屏幕绝对坐标）
		target := elements[0]
		clickX := target.X + originX
		clickY := target.Y + originY

		progress("click", fmt.Sprintf("第 %d 步：点击 %q (%d,%d)", step, target.Text, clickX, clickY),
			step*100/maxSteps)

		if err := win32.ClickAt(clickX, clickY, win32.ClickSingle); err != nil {
			stepsLog = append(stepsLog, map[string]any{
				"step": step, "status": "click_failed", "error": err.Error(),
				"element": target, "abs_x": clickX, "abs_y": clickY,
			})
			continue
		}

		stepsLog = append(stepsLog, map[string]any{
			"step": step, "status": "clicked",
			"element": map[string]any{
				"text": target.Text, "type": target.Type,
				"x": target.X, "y": target.Y,
			},
			"abs_x": clickX, "abs_y": clickY,
			"screenshot_before": dataURL(pngBytes),
		})

		// 4. 等待界面响应后再下一步
		select {
		case <-ctx.Done():
			return fail("", "点击后超时")
		case <-time.After(1500 * time.Millisecond):
		}
	}

	// 达到最大步数仍未完成
	r := proto.TaskResult{
		Success: false,
		ExitCode: 0,
		Error: fmt.Sprintf("达到最大步数 %d 未达成目标", maxSteps),
	}
	r.Data, _ = jsonMarshal(map[string]any{"steps": stepsLog, "completed": false})
	log.Printf("[guiclick] 意图 %q 达到最大步数未完成", task.Intent)
	return r
}

func dataURL(png []byte) string {
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
}
