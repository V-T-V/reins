package executors

import (
	"runtime"

	"github.com/QiuShichang/go-rmm/vision"
)

// Options 构造 Dispatcher 时的依赖注入。
type Options struct {
	VisionClient *vision.Client // guiclick 执行器需要（nil 则不注册）
	// Workspace 相对路径的解析基准目录,注入到 FileExecutor 与共享策略。
	// 留空则回退到 filepath.Abs 的默认行为(CWD)。
	Workspace string
	// Role 角色:"master"(主,最小能力集)/ "slave"(从,全集)/ ""(兼容=slave)。
	// master 只注册 powershell/script/file/browse(基础运维),不注册 screenshot/monitor/power/guiclick,
	// 减少攻击面 + 保证稳定性。master 还会由 agent 包额外注册 watchdog executor。
	Role string
}

// RegisterAll 注册所有可用的执行器到 d，返回实际注册的类型。
//
// 能力探测逻辑：
//   - powershell/screenshot：仅 Windows
//   - script：跨平台（Windows 执行 ps1/bat，Linux/macOS 执行 sh——P1-4 第一步）
//   - guiclick：Windows + vision.APIKey
//   - file/browse：跨平台
//
// M1 主从架构:Role=master 时只注册基础运维子集,保证主极简稳定。
func RegisterAll(d *Dispatcher, opts Options) []string {
	isMaster := opts.Role == "master"
	if runtime.GOOS == "windows" {
		// 基础运维(master + slave 都有):命令执行
		d.Register(NewPowerShell())
		if !isMaster {
			// slave 专属:业务能力(主不需要,减少攻击面)
			d.Register(NewScreenshot())
			d.Register(NewPower())   // 电源操作
			d.Register(NewMonitor()) // 流式监测
			if opts.VisionClient != nil && opts.VisionClient.APIKey != "" {
				d.Register(NewGUIClick(opts.VisionClient))
			}
		}
	}
	// script 跨平台:Windows 走 ps1/bat,Linux/macOS 走 sh
	d.Register(NewScript())
	// D4:把 Workspace 注入 FileExecutor 的策略,使相对路径可预期解析。
	// file 跨平台,master/slave 都需要(主用它推救援文件/读日志)。
	fileExec := NewFile()
	if opts.Workspace != "" {
		fileExec.policy.Workspace = opts.Workspace
	}
	d.Register(fileExec)
	d.Register(newBrowseImpl()) // 默认 stub;-tags browse 编译时为真实实现

	return d.Types()
}
