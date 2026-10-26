//go:build browse

// 真实 chromedp 实现。编译时加 -tags browse 启用。
// 需先安装 Chrome/Chromium。

package executors

import (
	"context"
	"encoding/base64"
	"fmt"
	"time"

	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"

	"github.com/QiuShichang/reins/proto"
)

type chromedpBrowseExecutor struct{}

func newBrowseImpl() BrowseExecutor { return &chromedpBrowseExecutor{} }

func (e *chromedpBrowseExecutor) Type() string { return proto.TaskBrowse }

func (e *chromedpBrowseExecutor) Exec(ctx context.Context, payload []byte, progress ProgressFunc) proto.TaskResult {
	var task proto.BrowseTask
	if err := decode(payload, &task); err != nil {
		return fail("", err.Error())
	}
	if task.URL == "" {
		return fail("", "url 不能为空")
	}

	// headless Chrome
	allocCtx, allocCancel := chromedp.NewExecAllocator(ctx,
		append(chromedp.DefaultExecAllocatorOptions[:],
			chromedp.Flag("headless", true),
		)...)
	defer allocCancel()

	taskCtx, taskCancel := chromedp.NewContext(allocCtx)
	defer taskCancel()

	progress("navigate", "打开 "+task.URL, 10)
	var screenshot []byte
	actions := []chromedp.Action{
		chromedp.Navigate(task.URL),
		chromedp.ActionFunc(func(ctx context.Context) error {
			return chromedp.WaitReady("body", chromedp.ByQuery).Do(ctx)
		}),
	}

	total := len(task.Actions)
	for i, act := range task.Actions {
		pct := 20 + (i+1)*60/(total+1)
		progress(act.Op, fmt.Sprintf("动作 %d/%d: %s", i+1, total, act.Op), pct)
		switch act.Op {
		case "navigate":
			actions = append(actions, chromedp.Navigate(act.Value))
		case "click":
			actions = append(actions, chromedp.Click(act.Value, chromedp.ByQuery))
		case "fill":
			// Value 格式 "selector\tvalue" 或直接是 selector（清空）
			sel, val := splitSelVal(act.Value)
			actions = append(actions, chromedp.WaitVisible(sel, chromedp.ByQuery))
			actions = append(actions, chromedp.SendKeys(sel, val, chromedp.ByQuery))
		case "wait":
			d := time.Duration(act.Wait) * time.Millisecond
			if d == 0 {
				d = 500 * time.Millisecond
			}
			actions = append(actions, chromedp.Sleep(d))
		case "screenshot":
			actions = append(actions, chromedp.CaptureScreenshot(&screenshot))
		case "eval":
			var res any
			actions = append(actions, chromedp.ActionFunc(func(ctx context.Context) error {
				_, exc, err := runtime.Evaluate(act.Value).Do(ctx)
				if err != nil {
					return err
				}
				if exc != nil {
					return fmt.Errorf("JS 异常: %s", exc.Text)
				}
				res = nil
				return nil
			}))
			_ = res
		}
	}

	// 末尾默认截图（若用户未显式 screenshot）
	if screenshot == nil {
		actions = append(actions, chromedp.CaptureScreenshot(&screenshot))
	}

	progress("run", "执行动作序列", 80)
	if err := chromedp.Run(taskCtx, actions...); err != nil {
		return fail("", "chromedp 执行失败: "+err.Error())
	}

	progress("done", "完成", 100)
	r := ok("")
	r.Data, _ = jsonMarshal(map[string]any{
		"url":        task.URL,
		"screenshot": "data:image/png;base64," + base64.StdEncoding.EncodeToString(screenshot),
		"bytes":      len(screenshot),
	})
	return r
}

func splitSelVal(s string) (string, string) {
	for i, sep := range []string{"\t", "|"} {
		if idx := indexByte(s, sep[0]); idx >= 0 {
			return s[:idx], s[idx+1:]
		}
		_ = i
	}
	return s, ""
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}
