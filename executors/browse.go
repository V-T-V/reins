package executors

import (
	"context"

	"github.com/V-T-V/reins/proto"
)

// BrowseExecutor 接口（实现由 build tag 切换）。
//
//   - 有 chromedp 编译（build tag browse）：真实 chromedp headless 浏览器
//   - 默认：返回"不可用"的 stub
//
// 这样无 Chrome 环境的机器也能编译整个项目。
type BrowseExecutor interface {
	Executor
}

// --- 默认 stub（无 chromedp 编译 tag） ---

type stubBrowseExecutor struct{}

func newBrowseImpl() BrowseExecutor { return &stubBrowseExecutor{} }

func (e *stubBrowseExecutor) Type() string { return proto.TaskBrowse }

func (e *stubBrowseExecutor) Exec(_ context.Context, _ []byte, _ ProgressFunc) proto.TaskResult {
	return fail("", "browse 执行器未编译（需用 -tags browse 编译，并安装 Chrome）")
}
