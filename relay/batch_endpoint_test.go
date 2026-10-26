package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/V-T-V/reins/proto"
)

// TestBatchProgressSSEDone 已完成批次的 SSE 流应立即收到 done 事件并关闭。
func TestBatchProgressSSEDone(t *testing.T) {
	relay := NewRelay(Config{Token: "t"})
	prog := newBatchProgress("b-done", 1)
	prog.markSucceeded("a-1", "t1", 1)
	br := BatchResult{BatchID: "b-done", Targets: []TargetResult{{AgentID: "a-1", TaskID: "t1", Success: true}}}
	tb := &trackedBatch{progress: prog, result: &br, done: make(chan struct{}), createdAt: time.Now()}
	close(tb.done)
	relay.batches.Store("b-done", tb)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/batch/{id}/progress", relay.handleBatchProgressSSE)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/batch/b-done/progress", nil)
	req.Header.Set("Authorization", "Bearer t")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(data), "event: done") {
		t.Fatalf("已完成批次 SSE 应收到 done 事件,实际: %s", data)
	}
	if !strings.Contains(string(data), `"success_rate"`) {
		t.Fatalf("done 事件应含聚合成功率,实际: %s", data)
	}
}

// TestBatchEndpointLifecycle 异步批次端点全生命周期:
// 提交(202+batch_id) → 轮询快照(running→finished) → SSE 收到 done 事件。
// 用真 Relay + 注册 2 个带 tag 的假 Agent;dispatchTask 对假 conn 投递
// 失败或成功取决于 wsConn——此处直接验证注册表/快照/SSE 的编排层,
// 投递结果无论如何,批次必然终态(成功或失败都是终态)。
func TestBatchEndpointLifecycle(t *testing.T) {
	relay := NewRelay(Config{Token: "t"})
	relay.agents.Register(proto.Register{AgentID: "a-1", Tags: map[string]string{"env": "prod"}}, nil)
	relay.agents.Register(proto.Register{AgentID: "a-2", Tags: map[string]string{"env": "prod"}}, nil)

	// 经真 ServeMux 路由(PathValue 只有 mux 匹配 {id} 通配后才填充)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/batch", relay.handleBatchStart)
	mux.HandleFunc("GET /api/batch/{id}", relay.handleBatchStatus)
	mux.HandleFunc("GET /api/batch/{id}/progress", relay.handleBatchProgressSSE)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := srv.Client()
	post := func(body string, withAuth bool) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/batch", strings.NewReader(body))
		if withAuth {
			req.Header.Set("Authorization", "Bearer t")
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("请求失败: %v", err)
		}
		return resp
	}

	// 1. 提交(重试 1 次,假 conn=nil → dispatchForBatch 报 agent 离线 → 失败/重试后终态)
	body := `{"tag":"env=prod","type":"powershell","payload":{"command":"Get-Date"},"timeout":10,"max_retries":1}`
	resp := post(body, true)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("提交应返回 202,实际 %d", resp.StatusCode)
	}
	var started struct {
		BatchID string `json:"batch_id"`
		Total   int    `json:"total"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&started); err != nil {
		t.Fatalf("解析提交响应失败: %v", err)
	}
	resp.Body.Close()
	if started.BatchID == "" || started.Total != 2 {
		t.Fatalf("batch_id/total 不符: %+v", started)
	}

	// 2. 轮询直到终态(conn=nil 投递失败,重试 1 次后 finished)
	deadline := time.Now().Add(10 * time.Second)
	var snap map[string]any
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/batch/"+started.BatchID, nil)
		req.Header.Set("Authorization", "Bearer t")
		r, err := client.Do(req)
		if err != nil {
			t.Fatalf("请求失败: %v", err)
		}
		if r.StatusCode != http.StatusOK {
			t.Fatalf("快照应 200,实际 %d", r.StatusCode)
		}
		snap = map[string]any{}
		err = json.NewDecoder(r.Body).Decode(&snap)
		r.Body.Close()
		if err != nil {
			t.Fatalf("解析快照失败: %v", err)
		}
		if snap["status"] == "finished" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if snap["status"] != "finished" {
		t.Fatalf("批次应在重试后到达终态,实际 %v", snap["status"])
	}
	if failed, _ := snap["failed"].(float64); failed != 2 {
		t.Fatalf("假 conn(离线)投递应全失败,实际 failed=%v", snap["failed"])
	}

	// 3. 404:不存在/已过期
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/batch/b-nope", nil)
	req.Header.Set("Authorization", "Bearer t")
	r, _ := client.Do(req)
	r.Body.Close()
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("未知批次应 404,实际 %d", r.StatusCode)
	}

	// 4. 鉴权失败
	if r := post(body, false); r.StatusCode != http.StatusUnauthorized {
		r.Body.Close()
		t.Fatalf("无鉴权应 401,实际 %d", r.StatusCode)
	} else {
		r.Body.Close()
	}

	// 5. 策略拦截(高危命令)
	badBody := `{"tag":"env=prod","type":"powershell","payload":{"command":"Format-Volume"},"timeout":10}`
	if r := post(badBody, true); r.StatusCode != http.StatusForbidden {
		r.Body.Close()
		t.Fatalf("高危命令应 403,实际 %d", r.StatusCode)
	} else {
		r.Body.Close()
	}
}

// TestDispatchWithRetryTrackedInjectedProgress 引擎拆分回归:
// 外部注入的 progress 在分发过程中即可读到实时状态(异步端点的关键不变量)。
func TestDispatchWithRetryTrackedInjectedProgress(t *testing.T) {
	var calls atomic.Int32
	d := newBatchDispatcher(func(ctx context.Context, agentID string, task proto.TaskRequest) (string, error) {
		calls.Add(1)
		if agentID == "slow" {
			time.Sleep(150 * time.Millisecond)
		}
		return task.TaskID, nil
	})
	progress := newBatchProgress("b-test", 2)
	done := make(chan BatchResult, 1)
	go func() {
		done <- d.dispatchWithRetryTracked(context.Background(), BatchRequest{
			BatchID:  "b-test",
			AgentIDs: []string{"fast", "slow"},
			Type:     proto.TaskPowerShell,
			Payload:  []byte(`{"command":"x"}`),
		}, progress)
	}()
	// slow 还在跑时,progress 应已能看到 running 状态
	time.Sleep(60 * time.Millisecond)
	snap := progress.Snapshot()
	if snap.Dispatched < 1 {
		t.Fatalf("分发中应能从注入的 progress 读到实时状态: %+v", snap)
	}
	br := <-done
	if len(br.Targets) != 2 || br.SuccessRate() != 1 {
		t.Fatalf("聚合结果不符: %+v", br)
	}
	if !progress.IsDone() {
		t.Fatal("完成后 IsDone 应为 true")
	}
	if calls.Load() != 2 {
		t.Fatalf("无失败不应重试,实际投递 %d 次", calls.Load())
	}
}

// TestBatchGCFreesExpired 完成 1h 后的批次被懒清理。
func TestBatchGCFreesExpired(t *testing.T) {
	relay := NewRelay(Config{})
	tb := &trackedBatch{progress: newBatchProgress("b-old", 1), done: make(chan struct{}), createdAt: time.Now().Add(-2 * time.Hour)}
	close(tb.done)
	relay.batches.Store("b-old", tb)
	keep := &trackedBatch{progress: newBatchProgress("b-new", 1), done: make(chan struct{}), createdAt: time.Now()}
	relay.batches.Store("b-new", keep)

	relay.gcBatches()
	if _, ok := relay.lookupBatch("b-old"); ok {
		t.Fatal("过期批次应被清理")
	}
	if _, ok := relay.lookupBatch("b-new"); !ok {
		t.Fatal("未过期批次应保留")
	}

}
