// Package executors — 流式系统监测执行器。
//
// top/htop 体验:
//   - 每 Interval 秒采集 CPU/内存/磁盘/进程
//   - 经 TaskProgress 流式上报(JSON in Message)
//   - Duration 秒后结束(0=无限,直到任务被取消)
//
// 采集用 PowerShell(Windows)/ /proc(Linux),跨平台。
package executors

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/QiuShichang/reins/proto"
)

// MonitorExecutor 流式监测。
type MonitorExecutor struct{}

// NewMonitor 构造。
func NewMonitor() *MonitorExecutor { return &MonitorExecutor{} }

func (e *MonitorExecutor) Type() string { return proto.TaskMonitor }

// Exec 执行监测任务(长生命周期)。
func (e *MonitorExecutor) Exec(ctx context.Context, payload []byte, progress ProgressFunc) proto.TaskResult {
	var task proto.MonitorTask
	if err := decode(payload, &task); err != nil {
		return fail("", "解析 monitor 任务失败: "+err.Error())
	}
	if task.Interval <= 0 {
		task.Interval = 3
	}
	if task.Duration == 0 {
		task.Duration = 60 // 默认 60s
	}
	if task.TopN <= 0 {
		task.TopN = 10
	}
	metrics := task.Metrics
	if len(metrics) == 0 {
		metrics = []string{"cpu", "mem", "disk", "proc"}
	}

	start := time.Now()
	deadline := start.Add(time.Duration(task.Duration) * time.Second)
	ticker := time.NewTicker(time.Duration(task.Interval) * time.Second)
	defer ticker.Stop()

	// 首次立即采样
	sample := e.collect(metrics, task.TopN)
	e.report(progress, sample, 0, "采集开始")

	sampleCount := 1
	for {
		select {
		case <-ctx.Done():
			return proto.TaskResult{
				Success: true,
				Stdout:  fmt.Sprintf("监测结束: 共 %d 次采样, 持续 %v", sampleCount, time.Since(start).Round(time.Second)),
			}
		case <-ticker.C:
			if time.Now().After(deadline) && task.Duration > 0 {
				return proto.TaskResult{
					Success: true,
					Stdout:  fmt.Sprintf("监测完成: 共 %d 次采样, 持续 %v", sampleCount, time.Duration(sampleCount*task.Interval)*time.Second),
				}
			}
			sample := e.collect(metrics, task.TopN)
			elapsed := int(time.Since(start) / time.Second)
			pct := elapsed * 100 / task.Duration
			if pct > 100 {
				pct = 100
			}
			e.report(progress, sample, pct, fmt.Sprintf("采样 #%d", sampleCount+1))
			sampleCount++
		}
	}
}

// report 上报一次采样(经 TaskProgress)。
func (e *MonitorExecutor) report(progress ProgressFunc, sample proto.MonitorSample, pct int, msg string) {
	if progress == nil {
		return
	}
	data, _ := json.Marshal(sample)
	progress("monitor", msg+" | "+string(data), pct)
}

// collect 采集系统指标。
func (e *MonitorExecutor) collect(metrics []string, topN int) proto.MonitorSample {
	if runtime.GOOS == "windows" {
		return e.collectWindows(metrics, topN)
	}
	// Linux: /proc 实现(monitor_procfs.go);其他非 Windows 平台: 桩(monitor_stub.go)
	return e.collectProcFS(metrics, topN)
}

// collectWindows 用 PowerShell 采集全维度指标(P1 扩展)。
// 分两次执行避免长输出触发 CLIXML 编码:
//  1. 基础(cpu/mem/disk/proc) — 始终采集
//  2. 扩展(diskio/net/gpu/svc/port) — 按需
func (e *MonitorExecutor) collectWindows(metrics []string, topN int) proto.MonitorSample {
	sample := proto.MonitorSample{Timestamp: time.Now().Unix()}

	// 单次执行采集所有指标(基础+扩展合并),输出用标记提取避免 CLIXML 编码
	fullScript := buildFullScript(metrics, topN)
	out := runPowerShellShort(fullScript)
	parseSample(out, &sample)
	// 扩展字段从同一 JSON 解析(parseSample 已处理所有字段)
	return sample
}

// buildFullScript 构造完整采集脚本(基础+扩展合并,每段 try/catch 隔离)。
// 输出用 <<<JSON>>> 标记包裹,Go 侧提取避免 CLIXML 编码干扰。
func buildFullScript(metrics []string, topN int) string {
	want := func(m string) bool {
		for _, x := range metrics {
			if x == m {
				return true
			}
		}
		return false
	}
	s := fmt.Sprintf(`$ErrorActionPreference='SilentlyContinue';$r=@{}
try{$os=Get-CimInstance Win32_OperatingSystem;$ub=(Get-Date)-$os.LastBootUpTime;$r.uptime=[int]$ub.TotalSeconds;$r.memTotal=[int]($os.TotalVisibleMemorySize/1024);$r.memFree=[int]($os.FreePhysicalMemory/1024)}catch{}
try{$r.cpu=[math]::Round((Get-Counter '\Processor(_Total)\%% Processor Time').CounterSamples.CookedValue,1)}catch{}
try{$r.cpu_per_core=@((Get-Counter '\Processor(*)\%% Processor Time').CounterSamples|Where{$_.InstanceName -ne '_total'}|%%{[math]::Round($_.CookedValue,0)})}catch{}
try{$r.cpu_queue=(Get-Counter '\System\Processor Queue Length').CounterSamples.CookedValue}catch{}
try{$r.disks=@((Get-CimInstance Win32_LogicalDisk -Filter 'DriveType=3')|ForEach-Object{@{dev=$_.DeviceID;totalGB=[math]::Round($_.Size/1GB,1);freeGB=[math]::Round($_.FreeSpace/1GB,1);usedPct=[math]::Round(($_.Size-$_.FreeSpace)/$_.Size*100,1)}})}catch{}
try{$r.procs=@((Get-Process|Sort CPU -Desc|Select -First %d)|ForEach-Object{@{pid=$_.Id;name=[string]$_.Name;cpu=[math]::Round($_.CPU,1);memMB=[math]::Round($_.WS/1MB,0)}})}catch{}
`, topN)
	// 扩展指标(按需,每段独立 try/catch)
	if want("diskio") {
		s += "try{$pdR=Get-Counter '\\PhysicalDisk(*)\\Disk Read Bytes/sec';$pdW=Get-Counter '\\PhysicalDisk(*)\\Disk Write Bytes/sec';$pdQ=Get-Counter '\\PhysicalDisk(*)\\Current Disk Queue Length';$dio=@();for($i=0;$i -lt $pdR.Count;$i++){if($pdR[$i].InstanceName -ne '_total'){$dio+=@{device=$pdR[$i].InstanceName;readMB=[math]::Round($pdR[$i].CookedValue/1MB,1);writeMB=[math]::Round($pdW[$i].CookedValue/1MB,1);queue=[math]::Round($pdQ[$i].CookedValue,0)}}};$r.disk_io=$dio}catch{}\n"
	}
	if want("netio") || want("net") {
		s += "try{$nR=Get-Counter '\\Network Interface(*)\\Bytes Received/sec';$nT=Get-Counter '\\Network Interface(*)\\Bytes Sent/sec';$nio=@();for($i=0;$i -lt $nR.Count;$i++){$nio+=@{iface=$nR[$i].InstanceName;rxKB=[math]::Round($nR[$i].CookedValue/1KB,0);txKB=[math]::Round($nT[$i].CookedValue/1KB,0)}};$r.net_io=$nio}catch{}\n"
		s += "try{$r.tcp_est=[int](Get-Counter '\\TCPv4\\Connections Established').CounterSamples.CookedValue}catch{}\n"
	}
	if want("net") {
		s += "try{$lp=Get-NetTCPConnection -State Listen;$r.tcp_lis=@($lp|Measure-Object).Count;$r.ports=@($lp|Select -First 20|ForEach-Object{@{port=$_.LocalPort;addr=$_.LocalAddress;pid=$_.OwningProcess}})}catch{}\n"
	}
	if want("gpu") {
		s += "try{$r.gpus=@((Get-CimInstance Win32_VideoController)|ForEach-Object{@{name=[string]$_.Name;driver=[string]$_.DriverVersion;memTotalMB=[int]($_.AdapterRAM/1MB)}})}catch{}\n"
	}
	if want("svc") {
		s += "try{$r.unhealthy_svc=@((Get-Service|Where{$_.StartType -eq 'Automatic' -and $_.Status -ne 'Running'}|Select -First 10)|ForEach-Object{@{name=[string]$_.Name;status=[string]$_.Status;startType=[string]$_.StartType}})}catch{}\n"
	}
	s += "$j=$r|ConvertTo-Json -Compress -Depth 4;Write-Output ('<<<JSON>>>'+$j+'<<<END>>>')"
	return s
}

// parseSample 解析完整指标 JSON 到 sample(基础+扩展统一解析)。
func parseSample(out string, sample *proto.MonitorSample) {
	if out == "" {
		return
	}
	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return
	}
	sample.Uptime = int64(getFloat(raw, "uptime"))
	sample.MemTotalMB = int64(getFloat(raw, "memTotal"))
	sample.MemFreeMB = int64(getFloat(raw, "memFree"))
	if sample.MemTotalMB > 0 {
		sample.MemUsedPct = float64(sample.MemTotalMB-sample.MemFreeMB) / float64(sample.MemTotalMB) * 100
	}
	sample.CPUUsage = getFloat(raw, "cpu")
	sample.CPUQueueLen = getFloat(raw, "cpu_queue")
	sample.CPUCtxSwitch = int64(getFloat(raw, "cpu_ctx"))
	sample.MemCommittedMB = int64(getFloat(raw, "mem_committed"))
	sample.MemCacheMB = int64(getFloat(raw, "mem_cache"))
	sample.TCPEstablished = int(getFloat(raw, "tcp_est"))
	sample.TCPListen = int(getFloat(raw, "tcp_lis"))
	sample.CPUPerCore = parseFloatArray(raw["cpu_per_core"])
	sample.Disks = parseDisks(raw["disks"])
	sample.DiskIO = parseDiskIO(raw["disk_io"])
	sample.NetIO = parseNetIO(raw["net_io"])
	sample.TopProcesses = parseProcs(raw["procs"])
	sample.GPUs = parseGPUs(raw["gpus"])
	sample.UnhealthyServices = parseSvcHealth(raw["unhealthy_svc"])
	sample.Ports = parsePorts(raw["ports"])
}

// parseFloatArray 解析 []float64(来自 PowerShell 数组)。
func parseFloatArray(v interface{}) []float64 {
	arr, ok := v.([]interface{})
	if !ok {
		return nil
	}
	out := make([]float64, 0, len(arr))
	for _, x := range arr {
		if f, ok := x.(float64); ok {
			out = append(out, f)
		}
	}
	return out
}

// parseDisks 解析磁盘容量数组。
func parseDisks(v interface{}) []proto.DiskInfo {
	arr, ok := v.([]interface{})
	if !ok {
		// 单个对象(只有 1 个盘时 PowerShell 不返回数组)
		if m, ok := v.(map[string]interface{}); ok {
			arr = []interface{}{m}
		} else {
			return nil
		}
	}
	out := make([]proto.DiskInfo, 0, len(arr))
	for _, x := range arr {
		if m, ok := x.(map[string]interface{}); ok {
			out = append(out, proto.DiskInfo{
				Device:  getString(m, "dev"),
				TotalGB: getFloat(m, "totalGB"),
				FreeGB:  getFloat(m, "freeGB"),
				UsedPct: getFloat(m, "usedPct"),
			})
		}
	}
	return out
}

// parseDiskIO 解析磁盘 IO 数组。
func parseDiskIO(v interface{}) []proto.DiskIOInfo {
	arr, ok := v.([]interface{})
	if !ok {
		if m, ok := v.(map[string]interface{}); ok {
			arr = []interface{}{m}
		} else {
			return nil
		}
	}
	out := make([]proto.DiskIOInfo, 0, len(arr))
	for _, x := range arr {
		if m, ok := x.(map[string]interface{}); ok {
			out = append(out, proto.DiskIOInfo{
				Device:      getString(m, "device"),
				ReadMBps:    getFloat(m, "readMB"),
				WriteMBps:   getFloat(m, "writeMB"),
				QueueLength: getFloat(m, "queue"),
			})
		}
	}
	return out
}

// parseNetIO 解析网络 IO 数组。
func parseNetIO(v interface{}) []proto.NetIOInfo {
	arr, ok := v.([]interface{})
	if !ok {
		if m, ok := v.(map[string]interface{}); ok {
			arr = []interface{}{m}
		} else {
			return nil
		}
	}
	out := make([]proto.NetIOInfo, 0, len(arr))
	for _, x := range arr {
		if m, ok := x.(map[string]interface{}); ok {
			out = append(out, proto.NetIOInfo{
				Interface: getString(m, "iface"),
				RxKBps:    getFloat(m, "rxKB"),
				TxKBps:    getFloat(m, "txKB"),
			})
		}
	}
	return out
}

// parseProcs 解析进程数组。
func parseProcs(v interface{}) []proto.ProcInfo {
	arr, ok := v.([]interface{})
	if !ok {
		if m, ok := v.(map[string]interface{}); ok {
			arr = []interface{}{m}
		} else {
			return nil
		}
	}
	out := make([]proto.ProcInfo, 0, len(arr))
	for _, x := range arr {
		if m, ok := x.(map[string]interface{}); ok {
			out = append(out, proto.ProcInfo{
				PID:     int32(getFloat(m, "pid")),
				Name:    getString(m, "name"),
				CPU:     getFloat(m, "cpu"),
				MemMB:   getFloat(m, "memMB"),
				Threads: int(getFloat(m, "threads")),
			})
		}
	}
	return out
}

// parseGPUs 解析 GPU 数组。
func parseGPUs(v interface{}) []proto.GPUInfo {
	arr, ok := v.([]interface{})
	if !ok {
		if m, ok := v.(map[string]interface{}); ok {
			arr = []interface{}{m}
		} else {
			return nil
		}
	}
	out := make([]proto.GPUInfo, 0, len(arr))
	for _, x := range arr {
		if m, ok := x.(map[string]interface{}); ok {
			out = append(out, proto.GPUInfo{
				Name:       getString(m, "name"),
				Driver:     getString(m, "driver"),
				MemTotalMB: int64(getFloat(m, "memTotalMB")),
			})
		}
	}
	return out
}

// parseSvcHealth 解析服务健康数组。
func parseSvcHealth(v interface{}) []proto.SvcHealth {
	arr, ok := v.([]interface{})
	if !ok {
		if m, ok := v.(map[string]interface{}); ok {
			arr = []interface{}{m}
		} else {
			return nil
		}
	}
	out := make([]proto.SvcHealth, 0, len(arr))
	for _, x := range arr {
		if m, ok := x.(map[string]interface{}); ok {
			out = append(out, proto.SvcHealth{
				Name:      getString(m, "name"),
				Status:    getString(m, "status"),
				StartType: getString(m, "startType"),
			})
		}
	}
	return out
}

// parsePorts 解析端口数组。
func parsePorts(v interface{}) []proto.PortInfo {
	arr, ok := v.([]interface{})
	if !ok {
		if m, ok := v.(map[string]interface{}); ok {
			arr = []interface{}{m}
		} else {
			return nil
		}
	}
	out := make([]proto.PortInfo, 0, len(arr))
	for _, x := range arr {
		if m, ok := x.(map[string]interface{}); ok {
			out = append(out, proto.PortInfo{
				Port:    int(getFloat(m, "port")),
				Address: getString(m, "addr"),
				PID:     int32(getFloat(m, "pid")),
				Proto:   "tcp",
			})
		}
	}
	return out
}

// runPowerShellShort 执行 PowerShell 脚本,返回输出。
// 超时 15s。
// 用 EncodedCommand + Output()(只读 stdout)。
// 脚本末尾用 <<<JSON>>>...<<<END>>> 标记包裹 JSON 输出,Go 侧提取。
func runPowerShellShort(script string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	encoded := encodePowerShellCommand(script)
	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-EncodedCommand", encoded)
	setProcAttr(cmd)
	out, _ := cmd.Output()
	s := string(out)
	// 提取 <<<JSON>>> 标记间内容
	startMark := "<<<JSON>>>"
	endMark := "<<<END>>>"
	startIdx := strings.Index(s, startMark)
	if startIdx >= 0 {
		jsonStart := startIdx + len(startMark)
		endIdx := strings.Index(s[jsonStart:], endMark)
		if endIdx >= 0 {
			return strings.TrimSpace(s[jsonStart : jsonStart+endIdx])
		}
		return strings.TrimSpace(s[jsonStart:])
	}
	// 兜底:CLIXML 编码时,JSON 可能在 <S> 标签里
	if strings.Contains(s, "#< CLIXML") {
		// 尝试从 CLIXML 提取明文 JSON(CLIXML 把字符串编码在 <S S="...">...</S> 里)
		idx := strings.Index(s, "{")
		if idx >= 0 {
			last := strings.LastIndex(s, "}")
			if last > idx {
				return s[idx : last+1]
			}
		}
	}
	return strings.TrimSpace(s)
}

// 辅助:从 map[string]interface{} 安全取值。
func getString(m map[string]interface{}, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

func getFloat(m map[string]interface{}, k string) float64 {
	if v, ok := m[k].(float64); ok {
		return v
	}
	return 0
}
