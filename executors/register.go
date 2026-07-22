package executors

import (
	"runtime"

	"github.com/QiuShichang/go-rmm/vision"
)

// Options 构造 Dispatcher 时的依赖注入。
type Options struct {
	VisionClient *vision.Client // guiclick 执行器需要（nil 则不注册）
}

// RegisterAll 注册所有可用的执行器到 d，返回实际注册的类型。
//
// 能力探测逻辑：
//   - powershell/script/screenshot：仅 Windows
//   - guiclick：Windows + vision.APIKey
//   - file：跨平台
//   - browse：始终注册（编译 tag 决定真实实现还是 stub）
func RegisterAll(d *Dispatcher, opts Options) []string {
	if runtime.GOOS == "windows" {
		d.Register(NewPowerShell())
		d.Register(NewScript())
		d.Register(NewScreenshot())
		if opts.VisionClient != nil && opts.VisionClient.APIKey != "" {
			d.Register(NewGUIClick(opts.VisionClient))
		}
	}
	d.Register(NewFile())
	d.Register(newBrowseImpl()) // 默认 stub；-tags browse 编译时为真实实现

	return d.Types()
}
