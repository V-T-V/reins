//go:build linux

package executors

import (
	"testing"
)

// TestCollectProcFS Linux /proc 采集的真实环境验证(CI Linux runner 执行)。
func TestCollectProcFS(t *testing.T) {
	e := NewMonitor()
	s := e.collectProcFS([]string{"cpu", "mem", "disk", "proc"}, 5)

	if s.Timestamp == 0 {
		t.Fatal("时间戳不应为空")
	}
	if s.CPUUsage < 0 || s.CPUUsage > 100 {
		t.Fatalf("CPU 使用率应在 0-100,实际 %v", s.CPUUsage)
	}
	if s.MemTotalMB <= 0 {
		t.Fatalf("内存总量应为正,实际 %d MB", s.MemTotalMB)
	}
	if s.MemUsedPct <= 0 || s.MemUsedPct > 100 {
		t.Fatalf("内存使用率应在 (0,100],实际 %v", s.MemUsedPct)
	}
	if s.Uptime <= 0 {
		t.Fatalf("uptime 应为正,实际 %d", s.Uptime)
	}
	if s.LoadAvg1 < 0 || s.LoadAvg5 < 0 || s.LoadAvg15 < 0 {
		t.Fatalf("负载不应为负: %v/%v/%v", s.LoadAvg1, s.LoadAvg5, s.LoadAvg15)
	}
	if len(s.Disks) == 0 {
		t.Fatal("应至少采集到一个物理文件系统挂载点")
	}
	for _, d := range s.Disks {
		if d.TotalGB <= 0 || d.FreeGB < 0 || d.UsedPct < 0 || d.UsedPct > 100 {
			t.Fatalf("磁盘数据异常: %+v", d)
		}
	}
	if len(s.TopProcesses) == 0 {
		t.Fatal("应至少返回一个进程")
	}
	for _, p := range s.TopProcesses {
		if p.PID <= 0 || p.Name == "" || p.CPU < 0 || p.CPU > 100 {
			t.Fatalf("进程数据异常: %+v", p)
		}
	}
}

// TestReadNetDev /proc/net/dev 解析:回环口应存在。
func TestReadNetDev(t *testing.T) {
	devs := readNetDev()
	if devs == nil {
		t.Fatal("/proc/net/dev 应可读")
	}
	if _, ok := devs["lo"]; !ok {
		t.Fatalf("应含回环口 lo: %v", devs)
	}
}

// TestReadTCPCounts /proc/net/tcp 统计非负。
func TestReadTCPCounts(t *testing.T) {
	est, listen := readTCPCounts()
	if est < 0 || listen < 0 {
		t.Fatalf("TCP 计数不应为负: est=%d listen=%d", est, listen)
	}
}
