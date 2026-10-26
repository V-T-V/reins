// Package client — SSE 事件流订阅(O4:替代轮询,实时接收事件)。
//
// 让 AI Agent / CLI 实时订阅 Relay 事件(agent 上线/任务进度/结果/输出)。
// 内部用 net/http 长连接 + 手写 SSE 解析,零依赖。
package client

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/V-T-V/reins/proto"
)

// Event SSE 事件。
type Event struct {
	Type string          // event: 后的值(如 task_progress / task_result / agent_online)
	Data json.RawMessage // data: 后的 JSON
}

// EventHandler 事件回调。
type EventHandler func(event Event)

// SubscribeEvents 订阅 SSE 事件流(阻塞直到 ctx 取消或连接断开)。
//
// 用法:
//
//	c := client.New("http://relay:7780", "token")
//	c.SubscribeEvents(ctx, func(ev client.Event) {
//	    if ev.Type == "task_result" {
//	        var result proto.TaskResult
//	        json.Unmarshal(ev.Data, &result)
//	        fmt.Println(result.Stdout)
//	    }
//	})
func (c *Client) SubscribeEvents(ctx context.Context, handler EventHandler) error {
	u := c.baseURL + "/api/events"
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("Accept", "text/event-stream")
	// 用无超时的 client(流式)
	streamClient := &http.Client{Transport: c.http.Transport}
	resp, err := streamClient.Do(req)
	if err != nil {
		return fmt.Errorf("SSE 连接失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("SSE HTTP %d", resp.StatusCode)
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 256*1024), 256*1024)
	var eventType string
	var dataBuf strings.Builder

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		line := scanner.Text()
		if line == "" {
			// 空行 = 事件边界
			if dataBuf.Len() > 0 {
				handler(Event{
					Type: eventType,
					Data: json.RawMessage(dataBuf.String()),
				})
			}
			eventType = ""
			dataBuf.Reset()
			continue
		}
		if strings.HasPrefix(line, "event:") {
			eventType = strings.TrimSpace(line[6:])
		} else if strings.HasPrefix(line, "data:") {
			if dataBuf.Len() > 0 {
				dataBuf.WriteByte('\n')
			}
			dataBuf.WriteString(strings.TrimSpace(line[5:]))
		}
	}
	return scanner.Err()
}

// SubscribeTaskResult 订阅指定 task 的结果(阻塞直到拿到 task_result 或超时)。
//
// 用法:
//
//	result, err := c.SubscribeTaskResult(ctx, "task-123", 60*time.Second)
func (c *Client) SubscribeTaskResult(ctx context.Context, taskID string, timeout <-chan time.Time) (*proto.TaskResult, error) {
	resultCh := make(chan *proto.TaskResult, 1)
	errCh := make(chan error, 1)

	subCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	go func() {
		err := c.SubscribeEvents(subCtx, func(ev Event) {
			if ev.Type != "task_result" {
				return
			}
			var outer struct {
				Data json.RawMessage `json:"data"`
			}
			if json.Unmarshal(ev.Data, &outer) != nil {
				return
			}
			var tr struct {
				TaskID string `json:"task_id"`
			}
			json.Unmarshal(outer.Data, &tr)
			if tr.TaskID != taskID {
				return
			}
			var result proto.TaskResult
			json.Unmarshal(outer.Data, &result)
			resultCh <- &result
			cancel()
		})
		if err != nil {
			errCh <- err
		}
	}()

	select {
	case result := <-resultCh:
		return result, nil
	case err := <-errCh:
		return nil, err
	case <-timeout:
		return nil, fmt.Errorf("等待 task_result 超时")
	}
}
