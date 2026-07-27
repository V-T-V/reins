// Package proto — 流式系统监测任务类型 + 采样数据结构。
//
// top/htop/glances 体验:Agent 周期采集全维度系统指标,经 TaskProgress 流式上报。
// CLI 订阅 SSE 实时刷新多面板 TUI。
//
// P1 扩展:从 4 项基础指标(CPU/Mem/Disk/Proc)升级为全维度
// (每核CPU/磁盘IO/网络流量/TCP/GPU/服务/端口)。
package proto

// MonitorTask 流式监测任务。
type MonitorTask struct {
	// Interval 采样间隔秒,默认 3
	Interval int `json:"interval,omitempty"`
	// Metrics 采集项: cpu/mem/disk/diskio/net/gpu/svc/port/proc/load。空=全部
	Metrics []string `json:"metrics,omitempty"`
	// Duration 持续秒数,默认 60(0=无限,直到任务被取消)
	Duration int `json:"duration,omitempty"`
	// TopN 进程列表显示前 N 个(按 CPU 排序),默认 10
	TopN int `json:"top_n,omitempty"`
}

// MonitorSample 单次采样数据(经 TaskProgress.Message 以 JSON 承载)。
//
// P1 扩展:全维度资源监控。所有新增字段 omitempty,向后兼容。
type MonitorSample struct {
	Timestamp int64 `json:"ts"`

	// === 基础(保留)==
	CPUUsage float64 `json:"cpu_usage,omitempty"` // CPU 总使用率 0-100
	// 内存(MB)
	MemTotalMB int64   `json:"mem_total_mb,omitempty"`
	MemFreeMB  int64   `json:"mem_free_mb,omitempty"`
	MemUsedPct float64 `json:"mem_used_pct,omitempty"`
	// 磁盘容量
	Disks []DiskInfo `json:"disks,omitempty"`
	// 运行时长(秒)
	Uptime int64 `json:"uptime,omitempty"`
	// 负载
	LoadAvg1  float64 `json:"load1,omitempty"`
	LoadAvg5  float64 `json:"load5,omitempty"`
	LoadAvg15 float64 `json:"load15,omitempty"`
	// Top 进程
	TopProcesses []ProcInfo `json:"procs,omitempty"`

	// === P1 扩展:CPU 详情 ===
	CPUPerCore   []float64 `json:"cpu_per_core,omitempty"`   // 每核使用率
	CPUQueueLen  float64   `json:"cpu_queue_len,omitempty"`  // 处理器队列长度
	CPUCtxSwitch int64     `json:"cpu_ctx_switch,omitempty"` // 上下文切换/秒

	// === P1 扩展:内存详情 ===
	MemCommittedMB int64 `json:"mem_committed_mb,omitempty"` // 已提交
	MemCacheMB     int64 `json:"mem_cache_mb,omitempty"`     // 缓存

	// === P1 扩展:磁盘 IO ===
	DiskIO []DiskIOInfo `json:"disk_io,omitempty"`

	// === P1 扩展:网络 IO ===
	NetIO []NetIOInfo `json:"net_io,omitempty"`

	// === P1 扩展:TCP 连接 ===
	TCPEstablished int `json:"tcp_established,omitempty"`
	TCPListen      int `json:"tcp_listen,omitempty"`

	// === P1 扩展:GPU ===
	GPUs []GPUInfo `json:"gpus,omitempty"`

	// === P1 扩展:服务健康(异常服务)===
	UnhealthyServices []SvcHealth `json:"unhealthy_svc,omitempty"`

	// === P1 扩展:监听端口 ===
	Ports []PortInfo `json:"ports,omitempty"`

	// === P1 扩展:最近事件日志 ===
	RecentEvents []LogEvent `json:"events,omitempty"`
}

// DiskInfo 磁盘容量信息(保留)。
type DiskInfo struct {
	Device  string  `json:"device"`
	TotalGB float64 `json:"total_gb"`
	FreeGB  float64 `json:"free_gb"`
	UsedPct float64 `json:"used_pct"`
}

// DiskIOInfo 磁盘 IO 速率(P1 新增)。
type DiskIOInfo struct {
	Device      string  `json:"device"`         // C:, D: 或 PhysicalDrive0
	ReadMBps    float64 `json:"read_mbps"`      // 读 MB/s
	WriteMBps   float64 `json:"write_mbps"`     // 写 MB/s
	IOPS        float64 `json:"iops,omitempty"` // 传输/秒
	QueueLength float64 `json:"queue,omitempty"` // 当前队列长度
}

// NetInfo 网络接口信息(保留,IP 地址)。
type NetInfo struct {
	Interface string `json:"iface"`
	IPv4      string `json:"ipv4"`
}

// NetIOInfo 网络 IO 速率(P1 新增)。
type NetIOInfo struct {
	Interface string  `json:"iface"`      // Wi-Fi, 以太网, eth0
	RxKBps    float64 `json:"rx_kbps"`    // 接收 KB/s
	TxKBps    float64 `json:"tx_kbps"`    // 发送 KB/s
	RxErrs    int64   `json:"rx_errs,omitempty"`
	TxErrs    int64   `json:"tx_errs,omitempty"`
}

// ProcInfo 进程信息(保留)。
type ProcInfo struct {
	PID     int32   `json:"pid"`
	Name    string  `json:"name"`
	CPU     float64 `json:"cpu"`    // CPU 占用率(0-100)
	MemMB   float64 `json:"mem_mb"` // 内存 MB
	Threads int     `json:"threads,omitempty"`
}

// GPUInfo GPU 信息(P1 新增)。
type GPUInfo struct {
	Name       string  `json:"name"`
	UsagePct   float64 `json:"usage_pct,omitempty"`    // GPU 使用率
	MemUsedMB  int64   `json:"mem_used_mb,omitempty"`  // 已用显存
	MemTotalMB int64   `json:"mem_total_mb,omitempty"` // 总显存
	TempC      float64 `json:"temp_c,omitempty"`       // 温度
	Driver     string  `json:"driver,omitempty"`       // 驱动版本
}

// SvcHealth 服务健康状态(P1 新增)。
type SvcHealth struct {
	Name    string `json:"name"`
	Status  string `json:"status"` // Stopped/Running
	StartType string `json:"start_type"`
}

// PortInfo 监听端口(P1 新增)。
type PortInfo struct {
	Port    int    `json:"port"`
	Proto   string `json:"proto"`   // tcp/udp
	Address string `json:"address"` // 0.0.0.0 / 127.0.0.1
	PID     int32  `json:"pid"`
	Process string `json:"process,omitempty"`
}

// LogEvent 事件日志条目(P1 新增)。
type LogEvent struct {
	Time      int64  `json:"ts"`
	Level     string `json:"level"` // Critical/Error/Warning
	Source    string `json:"source"`
	EventID   int32  `json:"event_id"`
	Message   string `json:"message"`
}
