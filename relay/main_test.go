package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/QiuShichang/go-rmm/proto"
)

// TestRelayEndToEnd 验证完整闭环：
//
//	Agent WS 连接 → register → 控制台 SSE 订阅 → 控制台 POST 任务
//	→ Relay 转发给 Agent → Agent 回 task_result → SSE 推给控制台
func TestRelayEndToEnd(t *testing.T) {
	relay := NewRelay(Config{Dev: true})
	mux := http.NewServeMux()
	relay.registerHTTP(mux)
	mux.HandleFunc("/agent", relay.handleAgentWS)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// 1. 控制台先订阅 SSE（拿到 agent_online/task_result 事件）
	sseURL := strings.Replace(srv.URL+"/api/events", "http://", "http://", 1)
	sseReq, _ := http.NewRequestWithContext(ctx, "GET", sseURL, nil)
	sseResp, err := http.DefaultClient.Do(sseReq)
	if err != nil {
		t.Fatalf("SSE 连接失败: %v", err)
	}
	defer sseResp.Body.Close()

	// 读 helper：用 bufio.Scanner 逐行读取，按空行分块，匹配 event 类型
	scanner := bufio.NewScanner(sseResp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	readSSE := func(wantEvent string, timeout time.Duration) (string, error) {
		done := make(chan struct {
			data string
			err  error
		}, 1)
		go func() {
			currentEvent := ""
			dataLine := ""
			for scanner.Scan() {
				line := scanner.Text()
				if line == "" {
					// 空行 = 事件结束
					if currentEvent == wantEvent && dataLine != "" {
						done <- struct {
							data string
							err  error
						}{dataLine, nil}
						return
					}
					currentEvent = ""
					dataLine = ""
					continue
				}
				if strings.HasPrefix(line, "event: ") {
					currentEvent = strings.TrimPrefix(line, "event: ")
				} else if strings.HasPrefix(line, "data: ") {
					dataLine = strings.TrimPrefix(line, "data: ")
				}
			}
			done <- struct {
				data string
				err  error
			}{"", fmt.Errorf("SSE 流结束: %w", scanner.Err())}
		}()
		select {
		case r := <-done:
			return r.data, r.err
		case <-time.After(timeout):
			return "", fmt.Errorf("等待事件 %s 超时", wantEvent)
		}
	}

	// 先消费 hello 事件
	if _, err := readSSE("hello", 2*time.Second); err != nil {
		t.Fatalf("未收到 hello: %v", err)
	}

	// 2. Agent 连接并 register
	agentURL := strings.Replace(srv.URL+"/agent", "http://", "ws://", 1)
	ac, _, err := websocket.Dial(ctx, agentURL, nil)
	if err != nil {
		t.Fatalf("Agent WS 连接失败: %v", err)
	}
	defer ac.CloseNow()
	ac.SetReadLimit(1 << 20)

	regEnv, _ := proto.NewEnvelope(proto.TypeRegister, "agent-test", "relay", proto.Register{
		AgentID:      "agent-test",
		Hostname:     "testhost",
		OS:           "linux/amd64",
		Capabilities: []string{proto.TaskPowerShell, proto.TaskScreenshot},
	})
	regBytes, _ := json.Marshal(regEnv)
	if err := ac.Write(ctx, websocket.MessageText, regBytes); err != nil {
		t.Fatalf("发送 register 失败: %v", err)
	}

	// 3. 控制台应收到 agent_online
	if data, err := readSSE(proto.TypeAgentOnline, 2*time.Second); err != nil {
		t.Fatalf("未收到 agent_online: %v", err)
	} else if !strings.Contains(data, "agent-test") {
		t.Fatalf("agent_online 数据异常: %s", data)
	}

	// 4. 控制台查询 agents 列表
	agentsResp, err := http.Get(srv.URL + "/api/agents")
	if err != nil {
		t.Fatalf("查询 agents 失败: %v", err)
	}
	var listResp struct {
		Count int                `json:"count"`
		Agents []proto.Register  `json:"agents"`
	}
	json.NewDecoder(agentsResp.Body).Decode(&listResp)
	agentsResp.Body.Close()
	if listResp.Count != 1 || listResp.Agents[0].AgentID != "agent-test" {
		t.Fatalf("agents 列表异常: %+v", listResp)
	}

	// 5. 启动 Agent 读 goroutine：收到任务后立即返回成功结果
	go func() {
		for {
			_, data, err := ac.Read(ctx)
			if err != nil {
				return
			}
			var env proto.Envelope
			if json.Unmarshal(data, &env) == nil && env.Type == proto.TypeTaskRequest {
				var task proto.TaskRequest
				env.Decode(&task)
				// 模拟执行：回一个成功结果
				result := proto.TaskResult{
					TaskID:   task.TaskID,
					Success:  true,
					Stdout:   "hello-from-agent",
					ExitCode: 0,
				}
				resEnv, _ := proto.NewEnvelope(proto.TypeTaskResult, "agent-test", "relay", result)
				resBytes, _ := json.Marshal(resEnv)
				ac.Write(ctx, websocket.MessageText, resBytes)
			}
		}
	}()

	// 6. 控制台下发任务
	taskBody, _ := json.Marshal(proto.TaskRequest{
		TaskID:  "task-001",
		Type:    proto.TaskPowerShell,
		AgentID: "agent-test",
		Payload: json.RawMessage(`{"command":"echo hi"}`),
	})
	postResp, err := http.Post(srv.URL+"/api/tasks", "application/json", bytes.NewReader(taskBody))
	if err != nil {
		t.Fatalf("下发任务失败: %v", err)
	}
	if postResp.StatusCode != http.StatusAccepted {
		t.Fatalf("期望 202，得到 %d", postResp.StatusCode)
	}
	postResp.Body.Close()

	// 7. 控制台应经 SSE 收到 task_result
	if data, err := readSSE(proto.TypeTaskResult, 2*time.Second); err != nil {
		t.Fatalf("未收到 task_result: %v", err)
	} else if !strings.Contains(data, "hello-from-agent") {
		t.Fatalf("task_result 数据异常: %s", data)
	}
}

// TestRegistrySweep 验证心跳超时清理。
func TestRegistrySweep(t *testing.T) {
	r := NewRegistry()
	r.Register(proto.Register{AgentID: "a1"}, &wsConn{id: "c1"})

	// 等待后用极小阈值清理，确保超过阈值
	time.Sleep(3 * time.Millisecond)
	if len(r.Sweep(1 * time.Millisecond)) != 1 {
		t.Fatal("首次清理应移除 a1")
	}
	if r.Conn("a1") != nil {
		t.Fatal("清理后 a1 应不存在")
	}

	// 重新注册并 Touch，确认不被误清
	r.Register(proto.Register{AgentID: "a2"}, &wsConn{id: "c2"})
	r.Touch("a2", 1) // Touch 刷新 lastBeat
	if len(r.Sweep(time.Second)) != 0 {
		t.Fatal("刚心跳的 a2 不应被清理")
	}
}

// TestRegistryUnregisterGuard 验证 Unregister 的 conn 匹配保护（防旧连接断开误删新连接）。
func TestRegistryUnregisterGuard(t *testing.T) {
	r := NewRegistry()
	oldConn := &wsConn{id: "old"}
	newConn := &wsConn{id: "new"}
	r.Register(proto.Register{AgentID: "a1"}, oldConn)
	// 模拟重连：新连接覆盖
	r.Register(proto.Register{AgentID: "a1"}, newConn)

	// 旧连接断开调用 Unregister，不应移除新连接
	if r.Unregister("a1", oldConn) {
		t.Fatal("旧连接不应成功 Unregister（已被新连接覆盖）")
	}
	if r.Conn("a1") == nil || r.Conn("a1").id != "new" {
		t.Fatal("新连接应仍在线")
	}

	// 新连接断开才能移除
	if !r.Unregister("a1", newConn) {
		t.Fatal("新连接应成功 Unregister")
	}
}
