// Package main — DAG 编排端点(v4.6,接入 internal/dagflow 调度核心)。
//
// 批量分发(P0-1)只会"同时对一批 Agent 发同一个任务";实际运维流程往往是
// 多阶段的:"备份 → 升级 → 健康检查 → 失败回滚"。本文件把 dagflow 的
// DAG 调度核心暴露为异步 REST 端点:
//
//	POST /api/dag      → 定义+提交 DAG(先 Validate,通过后异步执行),202 返回 dag_id
//	GET  /api/dag      → 编排列表(最近 50 条)
//	GET  /api/dag/{id} → 单次编排的状态快照(逐节点状态/错误/task_ids)
//
// 节点执行体:每个节点解析目标(agent_id 直指或 tag 扇出),复用既有任务
// 投递链路(store 持久化 + WS dispatchTask),然后轮询任务终态
// (completed/failed/cancelled)——节点成败完全由 Agent 回流的真实任务结果
// 决定,与 goroutine 调度解耦(dagflow 的确定性语义)。下游节点在失败
// 上游后由调度核心级联跳过(未投递任何任务)。
// 无 agent_id/tag 的节点为纯占位(汇聚/阶段分隔),立即成功。
//
// 注册表带 TTL 懒清理(与 batch 同款,完成 1h 后可回收)。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/V-T-V/reins/internal/dagflow"
	"github.com/V-T-V/reins/proto"
)

// dagTTL 已完成编排的注册表保留时长。
const dagTTL = time.Hour

// dagPollInterval 节点任务终态轮询粒度。
const dagPollInterval = 100 * time.Millisecond

// dagDispatchGrace 投递后等待终态的额外宽限(超出节点 timeout 的余量)。
const dagDispatchGrace = 15 * time.Second

// dagNodeStatus 单节点的实时状态(供快照视图)。
type dagNodeStatus struct {
	ID        string   `json:"id"`
	Status    string   `json:"status"`               // running/succeeded/failed/skipped
	Err       string   `json:"err,omitempty"`        // failed 时的原因文本
	Targets   []string `json:"targets,omitempty"`    // 解析后的目标 Agent
	TaskIDs   []string `json:"task_ids,omitempty"`   // 已投递的任务 ID
	Continue  bool     `json:"continue_on_error"`    // 软依赖标记
	StartedAt int64    `json:"started_at,omitempty"` // Unix 毫秒,未启动为 0
}

// trackedDAG 注册表中一次编排的条目。
type trackedDAG struct {
	id        string
	name      string
	createdAt time.Time
	done      chan struct{}
	finished  atomic.Bool

	mu     sync.Mutex
	nodes  []dagNodeStatus // 按 ID 字典序
	result *dagflow.Result
}

// markRunning/markFinished 更新节点实时状态(执行闭包内调用)。
func (td *trackedDAG) markRunning(st *dagNodeStatus) {
	td.mu.Lock()
	defer td.mu.Unlock()
	st.StartedAt = time.Now().UnixMilli()
	st.Status = "running"
}

func (td *trackedDAG) markFinished(st *dagNodeStatus, err error) {
	td.mu.Lock()
	defer td.mu.Unlock()
	if err != nil {
		st.Status = "failed"
		st.Err = err.Error()
	} else {
		st.Status = "succeeded"
	}
}

// recordTaskIDs 投递成功后回填任务 ID(供状态查询/结果追溯)。
func (td *trackedDAG) recordTaskIDs(st *dagNodeStatus, ids []string) {
	td.mu.Lock()
	defer td.mu.Unlock()
	st.TaskIDs = append(st.TaskIDs, ids...)
}

// dagSubmitNode 提交体中的节点定义。
type dagSubmitNode struct {
	ID              string          `json:"id"`
	Deps            []string        `json:"deps"`
	AgentID         string          `json:"agent_id"`
	Tag             string          `json:"tag"`
	Type            string          `json:"type"`
	Payload         json.RawMessage `json:"payload"`
	Timeout         int             `json:"timeout"` // 单任务超时秒数
	ContinueOnError bool            `json:"continue_on_error"`
}

// handleDAGSubmit POST /api/dag —— 定义+提交 DAG 编排。
//
// 请求体: {"name":"upgrade-flow","max_concurrency":2,"nodes":[
//
//	{"id":"backup","tag":"env=prod","type":"powershell","payload":{...},"timeout":60},
//	{"id":"upgrade","deps":["backup"],"tag":"env=prod",...},
//	{"id":"alert","deps":["upgrade"],"continue_on_error":true,...}
//
// ]}
// 响应 202: {"dag_id":"dag-...","node_count":N}
func (r *Relay) handleDAGSubmit(w http.ResponseWriter, req *http.Request) {
	if err := r.authorize(req); err != nil {
		writeJSON(w, http.StatusUnauthorized, errMap(err))
		return
	}
	var body struct {
		Name           string          `json:"name"`
		MaxConcurrency int             `json:"max_concurrency"`
		Nodes          []dagSubmitNode `json:"nodes"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("请求体解析失败: %w", err)))
		return
	}
	if body.Name == "" {
		body.Name = "unnamed"
	}
	if len(body.Nodes) == 0 {
		writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("nodes 不能为空")))
		return
	}

	// 每个节点提交期校验:任务类型 + 策略(编排是策略绕过的高危面,逐节点检查)。
	for i := range body.Nodes {
		n := &body.Nodes[i]
		if n.AgentID == "" && n.Tag == "" {
			continue // 纯占位节点,无任务可检
		}
		if n.Type == "" || !proto.IsValidTaskType(n.Type) {
			writeJSON(w, http.StatusBadRequest, errMap(fmt.Errorf("节点 %q 缺少合法 type", n.ID)))
			return
		}
		if err := r.policy.CheckCommandPayload(n.Type, n.Payload); err != nil {
			log.Printf("[policy] DAG 节点被拦截 name=%s node=%s type=%s: %v", body.Name, n.ID, n.Type, err)
			metrics.Inc("blocked")
			if r.audit != nil {
				r.audit.Log(AuditEntry{Event: "task_blocked", Type: n.Type, Detail: fmt.Sprintf("dag:%s/%s %s", body.Name, n.ID, err.Error())})
			}
			notifyPolicyBlocked(n.AgentID, n.Type, fmt.Sprintf("dag:%s/%s %s", body.Name, n.ID, err.Error())) // F11:webhook 告警
			writeJSON(w, http.StatusForbidden, errMap(err))
			return
		}
	}

	// 构造调度图并 Validate(Kahn 判环,非法图在提交期拒绝)。
	graph := dagflow.Graph{MaxConcurrency: body.MaxConcurrency}
	for i := range body.Nodes {
		sn := &body.Nodes[i]
		graph.Nodes = append(graph.Nodes, dagflow.Node{ID: sn.ID, Deps: sn.Deps, ContinueOnError: sn.ContinueOnError})
	}
	if err := graph.Validate(); err != nil {
		if r.audit != nil {
			r.audit.Log(AuditEntry{Event: "dag_rejected", Detail: body.Name + " " + err.Error()})
		}
		writeJSON(w, http.StatusBadRequest, errMap(err))
		return
	}

	dagID := fmt.Sprintf("dag-%d-%s", time.Now().UnixNano(), randSuffix(4))
	td := &trackedDAG{id: dagID, name: body.Name, createdAt: time.Now(), done: make(chan struct{})}
	// 快照视图节点(按 ID 字典序)与执行闭包共享同一份 dagNodeStatus。
	statuses := make(map[string]*dagNodeStatus, len(body.Nodes))
	for i := range body.Nodes {
		sn := &body.Nodes[i]
		st := &dagNodeStatus{ID: sn.ID, Continue: sn.ContinueOnError, Targets: r.dagResolveTargets(sn)}
		statuses[sn.ID] = st
		td.nodes = append(td.nodes, *st)
	}
	sort.SliceStable(td.nodes, func(i, j int) bool { return td.nodes[i].ID < td.nodes[j].ID })
	// 修正共享指针:sort 拷贝后 statuses 里的指针须指向 td.nodes 的元素,
	// 否则闭包更新的是切片外的副本。重新指向排序后的槽位。
	for i := range td.nodes {
		statuses[td.nodes[i].ID] = &td.nodes[i]
	}
	r.dags.Store(dagID, td)
	r.gcDAGs()

	// 异步执行:节点 Action = 目标投递 + 轮询终态,成败完全由 Agent 回流决定。
	run := dagflow.Graph{MaxConcurrency: body.MaxConcurrency}
	for i := range body.Nodes {
		sn := &body.Nodes[i]
		st := statuses[sn.ID]
		run.Nodes = append(run.Nodes, dagflow.Node{
			ID: sn.ID, Deps: sn.Deps, ContinueOnError: sn.ContinueOnError,
			Action: func(ctx context.Context) error {
				td.markRunning(st)
				err := r.executeDAGNode(ctx, td, sn, st)
				td.markFinished(st, err)
				return err
			},
		})
	}
	go func() {
		td.result = run.Run(context.Background())
		// 用调度核心的权威终态回填快照(skipped 节点从未执行 Action,
		// 实时字段不会更新;失败原因保留 Action 记录的文本)。
		td.mu.Lock()
		for i := range td.nodes {
			for _, nr := range td.result.Nodes {
				if nr.ID == td.nodes[i].ID {
					td.nodes[i].Status = string(nr.Status)
					if nr.Err != "" {
						td.nodes[i].Err = nr.Err
					}
					break
				}
			}
		}
		td.mu.Unlock()
		td.finished.Store(true)
		close(td.done)
		log.Printf("[dag] 编排 %s(%s) 结束: 成功 %d / 失败 %d / 跳过 %d",
			dagID, body.Name, td.result.Succeeded, td.result.Failed, td.result.Skipped)
	}()

	if r.audit != nil {
		r.audit.Log(AuditEntry{Event: "dag_submitted", Detail: fmt.Sprintf("%s nodes=%d concurrency=%d", body.Name, len(body.Nodes), body.MaxConcurrency)})
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"dag_id":     dagID,
		"name":       body.Name,
		"node_count": len(body.Nodes),
		"status":     "/api/dag/" + dagID,
	})
}

// dagResolveTargets 解析节点目标:agent_id 直指优先,否则按 tag 扇出。
// 无目标(占位节点)返回 nil。
func (r *Relay) dagResolveTargets(n *dagSubmitNode) []string {
	if n.AgentID != "" {
		return []string{n.AgentID}
	}
	if n.Tag == "" {
		return nil
	}
	ids := r.agents.FindByTag(n.Tag)
	sort.Strings(ids)
	return ids
}

// executeDAGNode 节点执行体:对全部目标投递任务并轮询终态。
//
// 投递:store 持久化(pending) → dispatchTask(WS);投递失败即节点失败
// (任务标 failed 终态,防 reaper 重投)。
// 轮询:100ms 粒度查 store,全部 completed → 成功;任一 failed/cancelled
// → 失败;超过 timeout+宽限仍非终态 → 失败(防无限等待)。
func (r *Relay) executeDAGNode(ctx context.Context, td *trackedDAG, sn *dagSubmitNode, st *dagNodeStatus) error {
	if len(st.Targets) == 0 {
		return nil // 纯占位节点
	}
	var (
		taskIDs []string
		failed  []string
	)
	for _, aid := range st.Targets {
		taskID := fmt.Sprintf("dag-%s-%s-%s-%s", td.id, sn.ID, aid, randSuffix(4))
		task := proto.TaskRequest{TaskID: taskID, Type: sn.Type, AgentID: aid, Payload: sn.Payload, Timeout: sn.Timeout}
		now := time.Now().UnixMilli()
		if r.store != nil {
			_ = r.store.SaveTask(StoredTask{Request: task, Status: StatusPending, CreatedAt: now, UpdatedAt: now})
		}
		conn := r.agents.Conn(aid)
		if conn == nil {
			if r.store != nil {
				_ = r.store.UpdateTaskStatus(taskID, StatusFailed)
			}
			failed = append(failed, fmt.Sprintf("%s: agent 离线", aid))
			continue
		}
		metrics.Inc("submitted")
		if err := r.dispatchTask(conn, task); err != nil {
			if r.store != nil {
				_ = r.store.UpdateTaskStatus(taskID, StatusFailed)
			}
			failed = append(failed, fmt.Sprintf("%s: %v", aid, err))
			continue
		}
		taskIDs = append(taskIDs, taskID)
		if r.audit != nil {
			r.audit.Log(AuditEntry{Event: "task_submitted", AgentID: aid, TaskID: taskID, Type: sn.Type, Detail: "dag:" + td.id + "/" + sn.ID})
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("投递失败: %v", failed)
	}
	td.recordTaskIDs(st, taskIDs)

	// 轮询终态:deadline = 单任务超时 + 投递宽限。
	deadline := time.Time{}
	if sn.Timeout > 0 {
		deadline = time.Now().Add(time.Duration(sn.Timeout)*time.Second + dagDispatchGrace)
	}
	for {
		allDone, err := r.pollDAGTasks(taskIDs)
		if err != nil {
			return err
		}
		if allDone {
			return nil
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			return fmt.Errorf("等待任务终态超时(> %ds+宽限): %v", sn.Timeout, taskIDs)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("编排上下文取消")
		case <-time.After(dagPollInterval):
		}
	}
}

// pollDAGTasks 检查任务终态:全部 completed → (true,nil);
// 任一 failed/cancelled → (false, 点名错误);否则 (false,nil) 继续等。
func (r *Relay) pollDAGTasks(taskIDs []string) (bool, error) {
	if r.store == nil {
		return true, nil // 无持久化时无从判定——视为完成(测试/降级模式)
	}
	completed := 0
	for _, id := range taskIDs {
		t, err := r.store.GetTask(id)
		if err != nil || t == nil {
			continue // 尚未可见(写入路径异步),继续等
		}
		switch t.Status {
		case StatusCompleted:
			completed++
		case StatusFailed:
			return false, fmt.Errorf("任务 %s 执行失败", id)
		case StatusCancelled:
			return false, fmt.Errorf("任务 %s 被取消", id)
		}
	}
	return completed == len(taskIDs), nil
}

// handleDAGList GET /api/dag —— 编排列表(最近 50 条,新在前)。
func (r *Relay) handleDAGList(w http.ResponseWriter, req *http.Request) {
	if err := r.authorize(req); err != nil {
		writeJSON(w, http.StatusUnauthorized, errMap(err))
		return
	}
	type entry struct {
		ID    string `json:"dag_id"`
		Name  string `json:"name"`
		State string `json:"state"` // running/finished
	}
	var list []entry
	r.dags.Range(func(k, v any) bool {
		td := v.(*trackedDAG)
		state := "running"
		if td.finished.Load() {
			state = "finished"
		}
		list = append(list, entry{ID: td.id, Name: td.name, State: state})
		return true
	})
	sort.SliceStable(list, func(i, j int) bool { return list[i].ID > list[j].ID })
	if len(list) > 50 {
		list = list[:50]
	}
	writeJSON(w, http.StatusOK, map[string]any{"dags": list, "count": len(list)})
}

// handleDAGStatus GET /api/dag/{id} —— 单次编排快照(逐节点状态)。
func (r *Relay) handleDAGStatus(w http.ResponseWriter, req *http.Request) {
	if err := r.authorize(req); err != nil {
		writeJSON(w, http.StatusUnauthorized, errMap(err))
		return
	}
	v, ok := r.dags.Load(req.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, errMap(fmt.Errorf("编排不存在或已过期")))
		return
	}
	writeJSON(w, http.StatusOK, dagStatusView(v.(*trackedDAG)))
}

// dagStatusView 编排的对外视图。
func dagStatusView(td *trackedDAG) map[string]any {
	td.mu.Lock()
	nodes := make([]dagNodeStatus, len(td.nodes))
	copy(nodes, td.nodes)
	td.mu.Unlock()

	out := map[string]any{
		"dag_id":     td.id,
		"name":       td.name,
		"created_at": td.createdAt.UnixMilli(),
		"nodes":      nodes,
	}
	if td.finished.Load() && td.result != nil {
		out["status"] = "finished"
		out["start_order"] = td.result.StartOrder
		out["succeeded"] = td.result.Succeeded
		out["failed"] = td.result.Failed
		out["skipped"] = td.result.Skipped
		out["success"] = td.result.Success()
	} else {
		out["status"] = "running"
	}
	return out
}

// gcDAGs 懒清理过期编排(完成超过 dagTTL)。
func (r *Relay) gcDAGs() {
	now := time.Now()
	r.dags.Range(func(k, v any) bool {
		td := v.(*trackedDAG)
		if td.finished.Load() && now.Sub(td.createdAt) > dagTTL {
			r.dags.Delete(k)
		}
		return true
	})
}
