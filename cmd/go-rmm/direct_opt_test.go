package main

// v4.7.2 直连层优化测试:共享 client 单例(连接复用前提)+ probe 快速失败。

import (
	"testing"
	"time"
)

func TestDirectClientSharedSingleton(t *testing.T) {
	a := directHTTPClient()
	b := directHTTPClient()
	if a != b {
		t.Error("直连 client 应为进程级单例(连接复用的前提)")
	}
}

func TestProbeUnreachableFastFail(t *testing.T) {
	resetDirectState(t)
	ds := &directSession{ticketID: "t", secret: "s", addrs: []string{"10.255.255.1:19999"}}
	start := time.Now()
	err := ds.probe("p", "upload")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("黑洞地址应失败")
	}
	if elapsed > 3*time.Second {
		t.Errorf("单地址探测应在 1.5s 超时内完成: %v", elapsed)
	}
	if !directDisabled.Load() {
		t.Error("全部地址不可达应置位熔断")
	}
}
