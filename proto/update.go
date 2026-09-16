// Package proto — Agent 自更新任务类型。
//
// 自更新流程(rename + copy + SCM 自重启):
//  1. Console/CLI 把新二进制上传到 Agent(Relay 仓库或直接推)
//  2. 下发 update 任务,BinaryPath 指向 Agent 侧已上传的 .new 文件
//  3. Agent 校验 SHA256 + 版本(防降级)→ rename .bak → rename .new → 退出
//  4. SCM OnFailure=restart 自动拉起新版本
//  5. 新版本启动健康自检,失败则用 .bak 自动回滚
package proto

// UpdateTask Agent 自更新任务载荷。
// 支持三种模式(优先级从高到低):
//   - DownloadURL: Agent 自己经 HTTP 从 Relay 版本仓库流式拉取(公网大文件推荐,
//     不占 WS 帧也不内联进内存;Agent 用自身 token/mTLS 鉴权)
//   - BinaryPath: 指向 Agent 侧已上传的文件路径(经 filetransfer 预上传)
//   - BinaryB64: 内联 base64 二进制(遗留模式,低延迟链路小体积适用)
type UpdateTask struct {
	// DownloadURL 新二进制的 HTTP 下载地址(Relay /api/updates/download?version=)。
	// Agent 流式下载到 exeDir/.auto 再走统一 execUpdate(sha/防降级/rename/健康回滚)。
	DownloadURL string `json:"download_url,omitempty"`

	// BinaryPath Agent 侧新二进制的绝对路径(已通过 file upload 推送)。
	BinaryPath string `json:"binary_path,omitempty"`

	// BinaryB64 内联 base64 二进制(与 BinaryPath 二选一)。
	BinaryB64 string `json:"binary_b64,omitempty"`

	// SHA256 新二进制的期望摘要(防传输损坏 + 防篡改)。
	SHA256 string `json:"sha256"`

	// Version 新二进制的版本号(如 "v1.2.0")。
	Version string `json:"version"`

	// MinVersion 允许的最低版本(防降级)。
	MinVersion string `json:"min_version,omitempty"`

	// Force 强制更新(跳过版本防降级检查,仅校验 SHA256)。
	Force bool `json:"force,omitempty"`
}
