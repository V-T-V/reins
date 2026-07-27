// Package proto 定义 go-rmm 三组件（Relay / Agent / Console）共享的通信协议。
//
// 所有 WebSocket 与 HTTP 消息都使用统一的 Envelope 信封包装，通过 Type 字段区分种类，
// Payload 为原始 JSON，由收件方按 Type 反序列化为对应结构体。
//
// 消息流向：
//
//	Agent  ──register/heartbeat/task_result/task_progress──►  Relay
//	Agent  ◄──────────────── task_request ──────────────────  Relay
//	Console ──(HTTP POST /api/tasks) task_request────────────►  Relay
//	Console ◄───(SSE) task_result/task_progress/agent_*──────  Relay
package proto

import (
	"encoding/json"
	"time"
)

// 协议版本
const Version = 1

// 消息类型常量。新增类型时务必同步更新 Is* 辅助函数与各组件的分发逻辑。
const (
	// TypeRegister —— Agent→Relay：连接建立后首条消息，登记身份与能力。
	TypeRegister = "register"
	// TypeHeartbeat —— Agent→Relay：周期心跳，维持连接与在线判定。
	TypeHeartbeat = "heartbeat"
	// TypeTaskRequest —— Console→Relay→Agent：下发任务。
	TypeTaskRequest = "task_request"
	// TypeTaskResult —— Agent→Relay→Console：任务终态结果。
	TypeTaskResult = "task_result"
	// TypeTaskProgress —— Agent→Relay→Console：长任务流式进度。
	TypeTaskProgress = "task_progress"
	// TypeAgentOnline —— Relay→Console：Agent 上线通知。
	TypeAgentOnline = "agent_online"
	// TypeAgentOffline —— Relay→Console：Agent 离线通知。
	TypeAgentOffline = "agent_offline"
	// TypeTaskAck —— Agent→Relay：任务已接收/已开始执行（可靠性 ACK）。
	TypeTaskAck = "task_ack"
	// TypeTaskCancel —— Console→Relay→Agent：取消正在执行的任务。
	TypeTaskCancel = "task_cancel"
	// TypeTaskNack —— Agent→Relay：任务无法执行（pool 满/连接断），Relay 应回退 pending。
	TypeTaskNack = "task_nack"
	// TypeEnroll —— Agent→Relay：用一次性注册令牌换取客户端证书（mTLS 引导）。
	// 注：enroll 实际走 HTTP POST /api/enroll（非 WS 消息），此常量保留供协议文档一致性。
	TypeEnroll = "enroll"
	// TypeEnrollResp —— Relay→Agent：签发的证书 + CA 证书。
	// 注：实际通过 HTTP 响应体返回（EnrollResp JSON），不走 WS。常量保留供文档引用。
	TypeEnrollResp = "enroll_resp"
	// TypeError —— 任意方→对方：协议级错误（预留，当前未使用，P4 可用于鉴权失败等场景）。
	TypeError = "error"
	// H3 交互式终端消息类型：
	TypeShellOpen  = "shell_open"  // Console→Relay→Agent：开会话
	TypeShellInput = "shell_input" // Console→Relay→Agent：发 stdin
	TypeShellOutput = "shell_output" // Agent→Relay→Console：推 stdout/stderr
	TypeShellClose = "shell_close" // 双向：关闭会话
	// J1 大文件传输消息类型（控制信令走 JSON，数据走二进制 WS 帧）：
	TypeFileTransferInit     = "file_xfer_init"      // Console→Relay→Agent：发起传输
	TypeFileTransferReady    = "file_xfer_ready"     // Agent→Relay→Console：就绪 + 续传偏移
	TypeFileTransferProgress = "file_xfer_progress"  // Agent→Relay→Console：周期进度
	TypeFileTransferComplete = "file_xfer_complete"  // Agent→Relay→Console：终态 + SHA256
	TypeFileTransferCancel   = "file_xfer_cancel"    // 双向：取消
)

// 任务类型常量，与 TaskRequest.Type 取值对应。
const (
	TaskPowerShell = "powershell" // 执行 PowerShell 命令
	TaskScript     = "script"     // 执行下发的 .ps1/.bat 脚本
	TaskFile       = "file"       // 文件上传/下载
	TaskScreenshot = "screenshot" // 窗口/全屏截图
	TaskBrowse     = "browse"     // headless 浏览器自动化
	TaskGUIClick   = "guiclick"   // 视觉驱动的桌面 GUI 点击
	TaskUpdate     = "update"     // Agent 自更新(替换二进制 + 重启)
	TaskPower      = "power"      // 电源操作(关机/重启/休眠)
	TaskMonitor    = "monitor"    // 流式系统监测(top/htop)
)

// ValidTaskTypes 已知任务类型集合（用于 Relay 侧 type 校验，防透传未知类型）。
var ValidTaskTypes = map[string]bool{
	TaskPowerShell: true,
	TaskScript:     true,
	TaskFile:       true,
	TaskScreenshot: true,
	TaskBrowse:     true,
	TaskGUIClick:   true,
	TaskUpdate:     true,
	TaskPower:      true,
	TaskMonitor:    true,
}

// IsValidTaskType 判断是否为已知任务类型。
func IsValidTaskType(t string) bool { return ValidTaskTypes[t] }

// P0-P2 修复:协议版本校验。
// 当前只支持 v1,未来演进时需放宽或协商。
// 不匹配时调用方应记录/拒绝,防止 v2 载荷被当 v1 解析。
func (e Envelope) IsValidVersion() bool {
	return e.Version == Version
}

// P1-P9 修复:任务状态公开常量(供 CLI/Console 复用,避免硬编码漂移)。
// 与 relay/store.go 的 Status* 保持一致(单向依赖:relay 定义,这里导出给外部)。
const (
	StatusPending      = "pending"
	StatusDispatched   = "dispatched"
	StatusAcknowledged = "acknowledged"
	StatusCompleted    = "completed"
	StatusFailed       = "failed"
	StatusCancelled    = "cancelled"
)

// IsTerminalStatus 判断是否终态(不可变更)。
func IsTerminalStatus(status string) bool {
	return status == StatusCompleted || status == StatusFailed || status == StatusCancelled
}

// 默认任务超时（秒）。TaskRequest.Timeout 为 0 时使用此值。
const DefaultTaskTimeout = 60

// Envelope 是所有消息的统一信封。
//
// 设计要点：
//   - Payload 用 json.RawMessage 延迟解码，允许 Relay 仅做路由而不关心具体载荷。
//   - To 为 "*" 时表示广播（仅用于 agent_online/offline 等通知）。
//   - ID 由发送方生成（UUIDv4），ReplyTo 用于结果/进度回填对应请求。
type Envelope struct {
	Version int             `json:"v"`                 // 协议版本，当前 1
	Type    string          `json:"type"`              // 消息类型，取 Type* 常量
	From    string          `json:"from"`              // 发送方 ID（agent-xxx / console-xxx / relay）
	To      string          `json:"to,omitempty"`      // 接收方 ID；"*" 表示广播
	ID      string          `json:"id"`                // 消息 ID（UUIDv4）
	ReplyTo string          `json:"reply_to,omitempty"` // 对应请求的 ID（结果/进度回填）
	Payload json.RawMessage `json:"payload,omitempty"` // 按 Type 反序列化
	TS      int64           `json:"ts"`                // Unix 毫秒时间戳
}

// NewEnvelope 构造一个带默认字段的信封。payload 可为 nil。
func NewEnvelope(typ, from, to string, payload any) (Envelope, error) {
	env := Envelope{
		Version: Version,
		Type:    typ,
		From:    from,
		To:      to,
		ID:      uuidString(),
		TS:      time.Now().UnixMilli(),
	}
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return Envelope{}, err
		}
		env.Payload = raw
	}
	return env, nil
}

// Decode 把信封的 Payload 反序列化到 v。
func (e Envelope) Decode(v any) error {
	if len(e.Payload) == 0 {
		return nil
	}
	return json.Unmarshal(e.Payload, v)
}

// --- Agent 注册与心跳 ---

// Register 由 Agent 在连接建立后发送，登记身份与支持的任务类型。
type Register struct {
	AgentID      string            `json:"agent_id"`              // Agent 唯一 ID
	Hostname     string            `json:"hostname"`              // 主机名
	OS           string            `json:"os"`                    // runtime.GOOS/runtime.GOARCH
	Version      string            `json:"version"`               // Agent 二进制版本
	Capabilities []string          `json:"capabilities"`          // 支持的任务类型列表
	Tags         map[string]string `json:"tags,omitempty"`        // 业务自定义标签
}

// Heartbeat 心跳载荷。Seq 为递增序号，用于检测丢包。
type Heartbeat struct {
	AgentID string `json:"agent_id"`
	Seq     int64  `json:"seq"`
}

// --- 任务请求与结果 ---

// TaskRequest 由控制台构造，经 Relay 路由到指定 Agent。
type TaskRequest struct {
	TaskID   string          `json:"task_id"`         // 任务唯一 ID（UUIDv4）
	Type     string          `json:"type"`            // 任务类型，取 Task* 常量
	AgentID  string          `json:"agent_id"`        // 目标 Agent ID
	Payload  json.RawMessage `json:"payload"`         // 按 Type 反序列化
	Timeout  int             `json:"timeout"`         // 秒，0 表示用 DefaultTaskTimeout
	Priority int             `json:"priority,omitempty"` // 0-9，预留
	// IdempotencyKey 幂等键：同一 key 在窗口期内只执行一次（防重投重复执行）。
	// 为空时由 Relay 用 TaskID 兜底。
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	// Attempt 第几次投递（Relay 重投时递增，Agent 可据此判断是否重复）。
	Attempt int `json:"attempt,omitempty"`
}

// TaskResult 任务终态结果。
type TaskResult struct {
	TaskID   string          `json:"task_id"`
	Success  bool            `json:"success"`
	Stdout   string          `json:"stdout,omitempty"`
	Stderr   string          `json:"stderr,omitempty"`
	ExitCode int             `json:"exit_code"`
	Data     json.RawMessage `json:"data,omitempty"` // 结构化结果（截图 base64、元素坐标等）
	Duration int             `json:"duration_ms"`
	Error    string          `json:"error,omitempty"`
}

// TaskProgress 长任务（browse/guiclick）的流式进度。
type TaskProgress struct {
	TaskID  string `json:"task_id"`
	Step    string `json:"step"`    // 当前步骤标识
	Message string `json:"message"` // 人类可读描述
	Percent int    `json:"percent"` // 0-100
}

// --- 各任务类型的 Payload ---

// PowerShellTask 执行内联 PowerShell 命令。
type PowerShellTask struct {
	Command        string `json:"command"`                 // PowerShell 命令文本
	ExecutionPolicy string `json:"execution_policy,omitempty"` // 默认 Bypass
}

// ScriptTask 执行下发的脚本文件。
type ScriptTask struct {
	Lang    string `json:"lang"`    // "ps1" 或 "bat"
	Content string `json:"content"` // 脚本文本（含中文需 UTF-8 BOM，Agent 负责）
}

// FileTask 文件上传/下载。
type FileTask struct {
	Op         string `json:"op"`          // "upload"（写文件）或 "download"（读文件）
	Path       string `json:"path"`        // 目标机绝对路径
	ContentB64 string `json:"content_b64"` // upload 时为 base64 编码的文件内容
	// H1 分块传输（突破 10MB 限制）：
	// chunk_upload: 按 ChunkIndex 写临时分片，TotalChunks 到齐后合并到 Path
	// chunk_download: 按 ChunkIndex 读取对应分片返回
	ChunkIndex  int    `json:"chunk_index,omitempty"`  // 当前分块序号（从 0 开始）
	TotalChunks int    `json:"total_chunks,omitempty"` // 总分块数
	UploadID    string `json:"upload_id,omitempty"`    // 分块会话 ID（同一文件的分块共享）
}

// ScreenshotTask 截图任务。
type ScreenshotTask struct {
	Target   string `json:"target,omitempty"`   // 进程名或窗口标题（模糊匹配），空则全屏
	Hwnd     int64  `json:"hwnd,omitempty"`     // 直接指定窗口句柄（优先级高于 Target）
	Full     bool   `json:"full,omitempty"`     // 强制全屏 CopyFromScreen
	Activate bool   `json:"activate,omitempty"` // 截图前激活窗口到前台
}

// BrowseTask headless 浏览器自动化。
type BrowseTask struct {
	URL     string          `json:"url"`     // 起始 URL
	Actions []BrowseAction  `json:"actions"` // 有序动作序列
}
type BrowseAction struct {
	Op    string `json:"op"`              // navigate/click/fill/wait/screenshot/eval
	Value string `json:"value,omitempty"` // 选择器、文本、URL、JS 表达式
	Wait  int    `json:"wait,omitempty"`  // 动作后等待毫秒数
}

// GUIClickTask 视觉驱动的桌面点击。
type GUIClickTask struct {
	Intent   string `json:"intent"`            // 自然语言意图描述（如"点击登录按钮"）
	Target   string `json:"target,omitempty"`  // 目标窗口进程名/标题（限定截图范围）
	MaxSteps int    `json:"max_steps,omitempty"` // 最大交互步数，默认 5
}

// --- 错误 ---

// ErrorPayload 协议级错误载荷。
type ErrorPayload struct {
	Code    string `json:"code"`    // 机器可读错误码
	Message string `json:"message"` // 人类可读描述
}

// --- 可靠性与取消 ---

// TaskAck 任务确认。Agent 收到任务后立即发（status=received），执行完成也可再发（status=completed）。
// Relay 据此判断任务是否抵达 Agent，未收到 ACK 的任务在 Relay 重启后会重新投递。
type TaskAck struct {
	TaskID string `json:"task_id"`
	Status string `json:"status"` // received / completed / failed
}

// TaskCancel 任务取消请求。
type TaskCancel struct {
	TaskID string `json:"task_id"`
	Reason string `json:"reason,omitempty"`
}

// TaskNack 任务拒绝（Agent 无法执行）。Relay 收到后应把任务回退 pending。
type TaskNack struct {
	TaskID string `json:"task_id"`
	Reason string `json:"reason"` // pool_full / connection_lost / unknown
}

// --- mTLS 引导（Enrollment） ---

// Enroll Agent→Relay：用一次性注册令牌换取客户端证书。
type Enroll struct {
	AgentID string `json:"agent_id"` // 申请者自报 Agent ID（Relay 校验令牌绑定）
	Token   string `json:"token"`    // 一次性注册令牌
	CSRPEM  string `json:"csr_pem"`  // PEM 编码的 CSR（CN=agent-id）
}

// EnrollResp Relay→Agent：签发的客户端证书 + CA 证书。
type EnrollResp struct {
	AgentID    string `json:"agent_id"`
	CertPEM    string `json:"cert_pem"`    // 签发给 Agent 的客户端证书（短期）
	CAPEM      string `json:"ca_pem"`      // Relay 的 CA 根证书（Agent 用于校验 Relay）
	ExpiresAt  int64  `json:"expires_at"`  // 证书过期 Unix 时间戳
	Success    bool   `json:"success"`
	Error      string `json:"error,omitempty"`
}

// --- H3 交互式终端 ---

// ShellOpen 开会话（Console→Relay→Agent）。
type ShellOpen struct {
	SessionID string `json:"session_id"` // 会话 ID（Console 生成）
	Cols      int    `json:"cols,omitempty"`
	Rows      int    `json:"rows,omitempty"`
	Shell     string `json:"shell,omitempty"` // 指定 shell（默认 powershell/cmd/bash）
}

// ShellInput 发送 stdin（Console→Relay→Agent）。
type ShellInput struct {
	SessionID string `json:"session_id"`
	DataB64   string `json:"data_b64"` // base64 编码的 stdin 字节
}

// ShellOutput 推送 stdout/stderr（Agent→Relay→Console）。
type ShellOutput struct {
	SessionID string `json:"session_id"`
	DataB64   string `json:"data_b64"`   // base64 编码的输出字节
	Stream    string `json:"stream,omitempty"` // stdout/stderr
	Exit      bool   `json:"exit,omitempty"`   // 进程退出标记
	ExitCode  int    `json:"exit_code,omitempty"`
}

// ShellClose 关闭会话（双向）。
type ShellClose struct {
	SessionID string `json:"session_id"`
	Reason    string `json:"reason,omitempty"`
}

// --- J1 大文件传输 ---

// FileTransferInit 发起传输（Console→Relay→Agent）。
type FileTransferInit struct {
	TransferID string `json:"transfer_id"`         // UUID，贯穿全生命周期
	Direction  string `json:"direction"`           // "upload"(Console→Agent) / "download"(Agent→Console)
	Path       string `json:"path"`                // Agent 侧绝对路径
	Size       int64  `json:"size"`                // upload 时已知；download 时 Agent 回填
	SHA256     string `json:"sha256,omitempty"`    // upload: Console 预算的摘要；download: Agent 算完回填
}

// FileTransferReady Agent 就绪（含续传偏移）。
type FileTransferReady struct {
	TransferID   string `json:"transfer_id"`
	Accepted     bool   `json:"accepted"`
	ResumeOffset int64  `json:"resume_offset"`    // 已落盘字节数，发送方 Seek 到此偏移
	Size         int64  `json:"size,omitempty"`   // download 时 Agent 回填
	Reason       string `json:"reason,omitempty"`
}

// FileTransferProgress 传输进度。
type FileTransferProgress struct {
	TransferID string `json:"transfer_id"`
	BytesDone  int64  `json:"bytes_done"`
	Total      int64  `json:"total"`
	Percent    int    `json:"percent"`
	SpeedBps   int64  `json:"speed_bps"`         // Agent 按 Δbytes/Δt 估算
}

// FileTransferComplete 传输终态。
type FileTransferComplete struct {
	TransferID   string `json:"transfer_id"`
	Success      bool   `json:"success"`
	SHA256Actual string `json:"sha256_actual,omitempty"`
	Bytes        int64  `json:"bytes"`
	Error        string `json:"error,omitempty"`
}

// FileTransferCancel 取消传输。
type FileTransferCancel struct {
	TransferID string `json:"transfer_id"`
	Reason     string `json:"reason,omitempty"`
}

// --- 辅助 ---

// uuidString 生成 UUIDv4 字符串。通过 google/uuid 实现。
func uuidString() string {
	return uuidNewString()
}
