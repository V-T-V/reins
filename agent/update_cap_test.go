package main

import (
	"testing"

	"github.com/QiuShichang/go-rmm/proto"
)

// TestRegisterExecutorsUpdateCapability update 执行器注册回归:
// slave(含未配置 role)的 Dispatcher 能力必须含 update——缺失导致 Relay
// agentSupports 拒收 update 任务,自更新通道被自己的能力校验锁死
// (2026-08-18 生产实证:注册代码在灾难重建时丢失)。
func TestRegisterExecutorsUpdateCapability(t *testing.T) {
	for _, role := range []string{"", "slave"} {
		cfg := Config{Role: role}
		d := registerExecutors(cfg)
		types := d.Types()
		found := false
		for _, ty := range types {
			if ty == proto.TaskUpdate {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("role=%q 的 Dispatcher 应注册 update 能力,实际: %v", role, types)
		}
	}

	// master 不注册:主永不更新,Relay 不应把 update 路由给 master
	d := registerExecutors(Config{Role: "master"})
	for _, ty := range d.Types() {
		if ty == proto.TaskUpdate {
			t.Fatalf("master 不应注册 update 能力(主永不更新),实际: %v", d.Types())
		}
	}
}
