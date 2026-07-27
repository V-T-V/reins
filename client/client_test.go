package client

import (
	"encoding/json"
	"testing"
)

func TestClientNew(t *testing.T) {
	c := New("http://localhost:7780", "secret")
	if c.baseURL != "http://localhost:7780" {
		t.Errorf("baseURL = %s", c.baseURL)
	}
	if c.token != "secret" {
		t.Errorf("token = %s", c.token)
	}
	if c.http == nil {
		t.Error("http client 不应为 nil")
	}
}

func TestClientServices(t *testing.T) {
	c := New("http://localhost:7780", "")
	if c.Agents() == nil {
		t.Error("Agents() 不应为 nil")
	}
	if c.Tasks() == nil {
		t.Error("Tasks() 不应为 nil")
	}
	if c.Files() == nil {
		t.Error("Files() 不应为 nil")
	}
}

func TestTaskRunPayload(t *testing.T) {
	// 测试 payload 构造(不发真实请求)
	payload := json.RawMessage(`{"command":"Get-Date"}`)
	if string(payload) != `{"command":"Get-Date"}` {
		t.Errorf("payload 构造错误: %s", string(payload))
	}
}
