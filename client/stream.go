// Package client — 流式任务执行(F3:实时输出,非轮询)。
//
// StreamExec 下发任务到 POST /api/tasks/stream,SSE 实时接收进度+结果。
// 适合长任务(monitor/build/deploy)或需要实时输出的场景。
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/QiuShichang/go-rmm/proto"
)

// StreamHandler 流式事件回调。
type StreamHandler func(event string, data json.RawMessage)

// StreamExec 流式执行任务(实时 SSE 推送进度+结果)。
//
// 用法:
//
//	c.StreamExec(ctx, taskReq, func(event string, data json.RawMessage) {
//	    if event == "task_progress" {
//	        fmt.Println("进度:", string(data))
//	    }
//	    if event == "task_result" {
//	        var r proto.TaskResult
//	        json.Unmarshal(data, &r)
//	        fmt.Println(r.Stdout)
//	    }
//	})
func (c *Client) StreamExec(ctx context.Context, task proto.TaskRequest, handler StreamHandler) (*proto.TaskResult, error) {
	body, _ := json.Marshal(task)
	u := c.baseURL + "/api/tasks/stream"
	req, err := http.NewRequestWithContext(ctx, "POST", u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("Accept", "text/event-stream")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("stream 连接失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("stream HTTP %d", resp.StatusCode)
	}

	scanner := newBufioScanner2(resp.Body)
	var finalResult *proto.TaskResult

	for {
		select {
		case <-ctx.Done():
			return finalResult, ctx.Err()
		default:
		}
		ev, err := parseSSE2(scanner)
		if err != nil {
			return finalResult, nil
		}
		if handler != nil {
			handler(ev.event, ev.data)
		}
		// task_result 是终态
		if ev.event == "task_result" {
			var outer struct {
				Data json.RawMessage `json:"data"`
			}
			json.Unmarshal(ev.data, &outer)
			var r proto.TaskResult
			json.Unmarshal(outer.Data, &r)
			finalResult = &r
			return finalResult, nil
		}
	}
}

// StreamCommand 快捷方式:流式执行 PowerShell 命令。
func (c *Client) StreamCommand(ctx context.Context, agentID, command string, timeout int, handler StreamHandler) (*proto.TaskResult, error) {
	payload, _ := json.Marshal(map[string]string{"command": command})
	task := proto.TaskRequest{
		TaskID:  fmt.Sprintf("sdk-stream-%d", time.Now().UnixNano()),
		Type:    "powershell",
		AgentID: agentID,
		Payload: payload,
		Timeout: timeout,
	}
	return c.StreamExec(ctx, task, handler)
}

// === 内部 SSE 解析(避免与 cmd/go-rmm 的 newSSEScanner 冲突)===

type sseEvent2 struct {
	event string
	data  []byte
}

func newBufioScanner2(data interface{ Read(p []byte) (int, error) }) interface {
	Read(p []byte) (int, error)
} {
	return data
}

func parseSSE2(reader interface{ Read(p []byte) (int, error) }) (*sseEvent2, error) {
	buf := make([]byte, 256*1024)
	var event string
	var dataBuf []byte

	for {
		line := readLine2(reader, buf)
		if len(line) == 0 {
			// 空行 = 事件边界
			if len(dataBuf) > 0 || event != "" {
				return &sseEvent2{event: event, data: dataBuf}, nil
			}
			continue
		}
		s := string(line)
		if len(s) > 6 && s[:6] == "event:" {
			event = trimSpaces(s[6:])
		} else if len(s) > 5 && s[:5] == "data:" {
			if len(dataBuf) > 0 {
				dataBuf = append(dataBuf, '\n')
			}
			dataBuf = append(dataBuf, []byte(trimSpaces(s[5:]))...)
		}
	}
}

func readLine2(reader interface{ Read(p []byte) (int, error) }, buf []byte) []byte {
	// 简化:每次读一行
	var line []byte
	small := make([]byte, 1)
	for {
		n, err := reader.Read(small)
		if n == 0 || err != nil {
			return line
		}
		if small[0] == '\n' {
			return trimCR(line)
		}
		line = append(line, small[0])
		if len(line) > 256*1024 {
			return line // 防溢出
		}
	}
}

func trimCR(b []byte) []byte {
	if len(b) > 0 && b[len(b)-1] == '\r' {
		return b[:len(b)-1]
	}
	return b
}

func trimSpaces(s string) string {
	start := 0
	end := len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}
