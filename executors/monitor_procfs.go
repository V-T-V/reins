//go:build linux

// Package executors — Linux 监测采集(/proc,P1-4 Linux Agent)。
//
// 无外部依赖,纯 /proc 文件解析:
//   - CPU: /proc/stat 两次采样差分(200ms)得总使用率+每核
//   - 内存: /proc/meminfo(MemTotal/MemAvailable)
//   - 磁盘容量: /proc/mounts 物理文件系统挂载点 × statfs
//   - 负载/uptime: /proc/loadavg、/proc/uptime
//   - Top 进程: /proc/[pid]/stat 累计 CPU 时间(自启动平均占用)+ RSS
//   - 网络 IO: /proc/net/dev 两次采样差分(速率)
//   - TCP: /proc/net/tcp(01=ESTABLISHED,0A=LISTEN)
//
// GPU/服务健康/事件日志为 Windows 扩展维度,Linux 暂缺(留待 systemd/journalctl 步)。
package executors

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/V-T-V/reins/proto"
)

// procSampleInterval CPU/NetIO 差分采样间隔。
const procSampleInterval = 200 * time.Millisecond

// collectProcFS Linux /proc 采集入口。
func (e *MonitorExecutor) collectProcFS(_ []string, topN int) proto.MonitorSample {
	sample := proto.MonitorSample{Timestamp: time.Now().Unix()}

	// 需差分的指标:先取 t0,等待,再取 t1
	cpu0 := readCPUStat()
	net0 := readNetDev()
	if cpu0 != nil && net0 != nil {
		time.Sleep(procSampleInterval)
	}
	cpu1 := readCPUStat()
	net1 := readNetDev()
	if elapsed := procSampleInterval.Seconds(); elapsed > 0 {
		if cpu0 != nil && cpu1 != nil {
			sample.CPUUsage = cpuUsagePct(cpu0, cpu1, -1)
			for i := range cpu1.cores {
				sample.CPUPerCore = append(sample.CPUPerCore, cpuUsagePct(cpu0, cpu1, i))
			}
		}
		if net0 != nil && net1 != nil {
			for iface, d1 := range net1 {
				if d0, ok := net0[iface]; ok && iface != "lo" {
					rxKB := float64(d1.rx-d0.rx) / 1024 / elapsed
					txKB := float64(d1.tx-d0.tx) / 1024 / elapsed
					if rxKB >= 0 && txKB >= 0 {
						sample.NetIO = append(sample.NetIO, proto.NetIOInfo{
							Interface: iface, RxKBps: rxKB, TxKBps: txKB,
							RxErrs: d1.rxErrs - d0.rxErrs, TxErrs: d1.txErrs - d0.txErrs,
						})
					}
				}
			}
		}
	}

	readMeminfo(&sample)
	readLoadavg(&sample)
	readUptime(&sample)
	sample.Disks = readDisks()
	sample.TCPEstablished, sample.TCPListen = readTCPCounts()
	sample.TopProcesses = readTopProcs(topN)
	return sample
}

// cpuStat /proc/stat 的 cpu 行快照(总+每核)。
type cpuStat struct {
	total uint64 // 用户态+内核+闲置等全部 jiffies
	idle  uint64 // idle+iowait
	cores []struct{ total, idle uint64 }
}

// readCPUStat 解析 /proc/stat。失败返回 nil。
func readCPUStat() *cpuStat {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return nil
	}
	defer f.Close()
	out := &cpuStat{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "cpu") {
			continue
		}
		fields := strings.Fields(line)
		// cpu 行: cpu user nice system idle iowait irq softirq steal ...
		var total, idle uint64
		for i, v := range fields[1:] {
			n, _ := strconv.ParseUint(v, 10, 64)
			total += n
			if i == 3 || i == 4 { // idle + iowait
				idle += n
			}
		}
		if fields[0] == "cpu" {
			out.total, out.idle = total, idle
		} else {
			out.cores = append(out.cores, struct{ total, idle uint64 }{total, idle})
		}
	}
	return out
}

// cpuUsagePct 两次快照差分的使用率;core=-1 为总使用率。
func cpuUsagePct(a, b *cpuStat, core int) float64 {
	var dt, di uint64
	if core < 0 {
		dt, di = b.total-a.total, b.idle-a.idle
	} else {
		if core >= len(a.cores) || core >= len(b.cores) {
			return 0
		}
		dt = b.cores[core].total - a.cores[core].total
		di = b.cores[core].idle - a.cores[core].idle
	}
	if dt == 0 {
		return 0
	}
	pct := float64(dt-di) / float64(dt) * 100
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	return pct
}

// readMeminfo 解析 /proc/meminfo → MemTotalMB/MemFreeMB/MemUsedPct。
func readMeminfo(s *proto.MonitorSample) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return
	}
	defer f.Close()
	var totalKB, availKB int64
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		v, _ := strconv.ParseInt(fields[1], 10, 64)
		switch fields[0] {
		case "MemTotal:":
			totalKB = v
		case "MemAvailable:":
			availKB = v
		}
	}
	if totalKB > 0 {
		s.MemTotalMB = totalKB / 1024
		s.MemFreeMB = availKB / 1024
		s.MemUsedPct = float64(totalKB-availKB) / float64(totalKB) * 100
	}
}

// readLoadavg 解析 /proc/loadavg。
func readLoadavg(s *proto.MonitorSample) {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return
	}
	fields := strings.Fields(string(data))
	if len(fields) >= 3 {
		s.LoadAvg1, _ = strconv.ParseFloat(fields[0], 64)
		s.LoadAvg5, _ = strconv.ParseFloat(fields[1], 64)
		s.LoadAvg15, _ = strconv.ParseFloat(fields[2], 64)
	}
}

// readUptime 解析 /proc/uptime(秒)。
func readUptime(s *proto.MonitorSample) {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return
	}
	fields := strings.Fields(string(data))
	if len(fields) >= 1 {
		v, _ := strconv.ParseFloat(fields[0], 64)
		s.Uptime = int64(v)
	}
}

// readDisks /proc/mounts 物理文件系统挂载点 × statfs。
func readDisks() []proto.DiskInfo {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return nil
	}
	defer f.Close()
	physical := map[string]bool{"ext4": true, "ext3": true, "ext2": true, "xfs": true,
		"btrfs": true, "zfs": true, "f2fs": true, "overlay": true}
	seen := map[string]bool{}
	var out []proto.DiskInfo
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 {
			continue
		}
		dev, mount, fstype := fields[0], fields[1], fields[2]
		if !physical[fstype] || seen[mount] || strings.HasPrefix(dev, "/dev/loop") {
			continue
		}
		var st unix.Statfs_t
		if err := unix.Statfs(mount, &st); err != nil {
			continue
		}
		seen[mount] = true
		total := float64(st.Blocks) * float64(st.Bsize) / 1e9
		free := float64(st.Bavail) * float64(st.Bsize) / 1e9
		var pct float64
		if total > 0 {
			pct = (total - free) / total * 100
		}
		out = append(out, proto.DiskInfo{Device: mount, TotalGB: total, FreeGB: free, UsedPct: pct})
	}
	return out
}

// readTopProcs 遍历 /proc/[pid],按累计 CPU 时间(自启动平均占用)排序取 topN。
func readTopProcs(topN int) []proto.ProcInfo {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	clockTicks := 100.0 // Linux 默认 USER_HZ;boot 后恒定
	uptime := 1.0
	if data, err := os.ReadFile("/proc/uptime"); err == nil {
		if fields := strings.Fields(string(data)); len(fields) >= 1 {
			uptime, _ = strconv.ParseFloat(fields[0], 64)
		}
	}
	procs := make([]proto.ProcInfo, 0, 64)
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue // 非数字目录
		}
		statBytes, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "stat"))
		if err != nil {
			continue
		}
		// /proc/pid/stat: pid (comm) state ppid ... utime(14) stime(15) ... threads(20) ...
		// comm 可能含空格/括号——从最后一个 ')' 后切分,字段下标相对 ')' 后:
		// fields[0]=state utime=fields[11] stime=fields[12] threads=fields[17]。
		stat := string(statBytes)
		idx := strings.LastIndex(stat, ")")
		if idx < 0 {
			continue
		}
		fields := strings.Fields(stat[idx+1:])
		if len(fields) < 20 {
			continue
		}
		// fields[0]=state;utime=fields[11],stime=fields[12],threads=fields[17](相对 ')' 后)
		utime, _ := strconv.ParseFloat(fields[11], 64)
		stime, _ := strconv.ParseFloat(fields[12], 64)
		threads, _ := strconv.Atoi(fields[17])
		procCPUSeconds := (utime + stime) / clockTicks
		name, rssMB := readProcNameRss(pid)
		var cpuPct float64
		if uptime > 0 {
			cpuPct = procCPUSeconds / uptime * 100
		}
		procs = append(procs, proto.ProcInfo{
			PID: int32(pid), Name: name, CPU: cpuPct, MemMB: rssMB, Threads: threads,
		})
	}
	sort.SliceStable(procs, func(i, j int) bool {
		if procs[i].CPU != procs[j].CPU {
			return procs[i].CPU > procs[j].CPU
		}
		return procs[i].MemMB > procs[j].MemMB
	})
	if len(procs) > topN {
		procs = procs[:topN]
	}
	return procs
}

// readProcNameRss 读 /proc/pid/status 的 Name 与 VmRSS。
func readProcNameRss(pid int) (string, float64) {
	f, err := os.Open(filepath.Join("/proc", strconv.Itoa(pid), "status"))
	if err != nil {
		return strconv.Itoa(pid), 0
	}
	defer f.Close()
	name := ""
	var rssKB float64
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "Name:"):
			name = strings.TrimSpace(strings.TrimPrefix(line, "Name:"))
		case strings.HasPrefix(line, "VmRSS:"):
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				rssKB, _ = strconv.ParseFloat(fields[1], 64)
			}
		}
		if name != "" && rssKB > 0 {
			break
		}
	}
	if name == "" {
		name = strconv.Itoa(pid)
	}
	return name, rssKB / 1024
}

// netDevCounters /proc/net/dev 单接口计数。
type netDevCounters struct {
	rx, tx, rxErrs, txErrs int64
}

// readNetDev 解析 /proc/net/dev。
func readNetDev() map[string]netDevCounters {
	f, err := os.Open("/proc/net/dev")
	if err != nil {
		return nil
	}
	defer f.Close()
	out := map[string]netDevCounters{}
	sc := bufio.NewScanner(f)
	for i := 0; sc.Scan(); i++ {
		if i < 2 { // 头两行是表头
			continue
		}
		parts := strings.SplitN(sc.Text(), ":", 2)
		if len(parts) != 2 {
			continue
		}
		iface := strings.TrimSpace(parts[0])
		fields := strings.Fields(parts[1])
		if len(fields) < 9 {
			continue
		}
		rx, _ := strconv.ParseInt(fields[0], 10, 64)
		rxErrs, _ := strconv.ParseInt(fields[2], 10, 64)
		tx, _ := strconv.ParseInt(fields[8], 10, 64)
		txErrs, _ := strconv.ParseInt(fields[10], 10, 64)
		out[iface] = netDevCounters{rx: rx, tx: tx, rxErrs: rxErrs, txErrs: txErrs}
	}
	return out
}

// readTCPCounts 统计 /proc/net/tcp 连接状态。
func readTCPCounts() (established, listen int) {
	f, err := os.Open("/proc/net/tcp")
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for i := 0; sc.Scan(); i++ {
		if i == 0 { // 表头
			continue
		}
		fields := strings.Fields(sc.Text())
		if len(fields) < 4 {
			continue
		}
		switch fields[3] {
		case "01":
			established++
		case "0A":
			listen++
		}
	}
	return established, listen
}
