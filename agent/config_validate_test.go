package main

import (
	"testing"
	"time"
)

func TestConfigValidateOK(t *testing.T) {
	c := Config{
		RelayURL:  "ws://relay:7780/agent",
		AgentID:   "agent-test-1",
		Heartbeat: 15 * time.Second,
	}
	if err := c.Validate(); err != nil {
		t.Errorf("有效配置应通过: %v", err)
	}
}

func TestConfigValidateEmptyRelay(t *testing.T) {
	c := Config{AgentID: "a1"}
	if err := c.Validate(); err == nil {
		t.Error("空 relay_url 应报错")
	}
}

func TestConfigValidateBadProtocol(t *testing.T) {
	c := Config{RelayURL: "http://relay:7780", AgentID: "a1"}
	if err := c.Validate(); err == nil {
		t.Error("http:// 开头应报错")
	}
}

func TestConfigValidateEmptyAgentID(t *testing.T) {
	c := Config{RelayURL: "ws://relay:7780/agent"}
	if err := c.Validate(); err == nil {
		t.Error("空 agent_id 应报错")
	}
}

func TestConfigValidateShortHeartbeat(t *testing.T) {
	c := Config{
		RelayURL:  "ws://relay:7780/agent",
		AgentID:   "a1",
		Heartbeat: 2 * time.Second,
	}
	if err := c.Validate(); err == nil {
		t.Error("heartbeat 2s 应报错")
	}
}

func TestConfigValidateZeroHeartbeatOK(t *testing.T) {
	c := Config{
		RelayURL:  "ws://relay:7780/agent",
		AgentID:   "a1",
		Heartbeat: 0, // 0 表示用默认值,应通过
	}
	if err := c.Validate(); err != nil {
		t.Errorf("heartbeat=0 应通过(用默认): %v", err)
	}
}

func TestConfigValidateNegativeMaxTasks(t *testing.T) {
	c := Config{
		RelayURL:           "ws://relay:7780/agent",
		AgentID:            "a1",
		MaxConcurrentTasks: -1,
	}
	if err := c.Validate(); err == nil {
		t.Error("负数 max_concurrent_tasks 应报错")
	}
}

func TestDefaultConfig(t *testing.T) {
	c := DefaultConfig()
	if c.Heartbeat != 15*time.Second {
		t.Errorf("Heartbeat = %v", c.Heartbeat)
	}
	if c.MaxConcurrentTasks != 4 {
		t.Errorf("MaxConcurrentTasks = %d", c.MaxConcurrentTasks)
	}
	if !c.AllowAllRoots {
		t.Error("AllowAllRoots 默认应为 true")
	}
}

func TestConfigValidateWSS(t *testing.T) {
	c := Config{RelayURL: "wss://relay:7780/agent", AgentID: "a1"}
	if err := c.Validate(); err != nil {
		t.Errorf("wss:// 应通过: %v", err)
	}
}

func TestExeDir(t *testing.T) {
	dir := exeDir()
	if dir == "" {
		t.Error("exeDir 不应为空")
	}
}
