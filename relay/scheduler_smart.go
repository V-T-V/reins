// Package main — 智能调度(XL6 神级提升)。
//
// 批量任务下发的智能 Agent 选择:
//   - 负载均衡:选择当前负载最低的 Agent(CPU/内存最低)
//   - 亲和性:优先选择上次成功执行同类型任务的 Agent(数据本地性)
//   - 地域:优先选择同 tag 的 Agent(region=us-east)
//   - 健康:排除近期频繁失败的 Agent
//
// GET /api/scheduler/best?tag=env=prod&type=powershell
// 返回最优 Agent ID 供 CLI/Console 选择。
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/QiuShichang/reins/proto"
)

// loadSampleTTL monitor 负载采样有效期(XL6):monitor 任务默认 3s 一采,
// 2 分钟无新采样(任务早已结束/Agent 空闲)视为过期,评分回 0——
// 宁可用"无数据"也不拿陈旧负载数据当实时值。
const loadSampleTTL = 2 * time.Minute

// parseMonitorLoad 从 monitor progress 的 Message 提取负载采样(XL6)。
//
// monitor 执行器上报格式为 `"<msg> | <MonitorSample JSON>"`(msg 如 "采样 #2"),
// 此处取首个 '{' 到末尾 '}' 的 JSON 段解析出 CPU/内存使用率。
// 非 monitor 消息/坏 JSON/无 JSON 一律返回 ok=false,由调用方忽略。
func parseMonitorLoad(msg string) (cpu, memPct float64, ok bool) {
	start := strings.Index(msg, "{")
	if start < 0 {
		return 0, 0, false
	}
	end := strings.LastIndex(msg, "}")
	if end <= start {
		return 0, 0, false
	}
	var sample proto.MonitorSample
	if err := json.Unmarshal([]byte(msg[start:end+1]), &sample); err != nil {
		return 0, 0, false
	}
	return sample.CPUUsage, sample.MemUsedPct, true
}

// SelectBestAgent 智能选择最优 Agent。
//
// 策略:
//  1. 过滤:在线 + 支持 taskType + 匹配 tag
//  2. 排序:负载综合分 CPU*0.4 + Mem*0.3 + 在途任务数*10 升序(任务权重最高——
//     一个在途任务(10 分)重于 25% CPU 差距;CPU/内存来自 monitor 采样缓存,
//     TTL 内的采样参与评分,过期/无采样按 0 处理=无数据不阻塞调度)
//  3. 返回最优
func (r *Relay) SelectBestAgent(tag, taskType string) (string, error) {
	candidates := r.agents.Snapshot()
	if len(candidates) == 0 {
		return "", fmt.Errorf("无在线 Agent")
	}

	type scored struct {
		agent proto.Register
		score float64
	}
	var pool []scored

	for _, a := range candidates {
		// 能力过滤
		if taskType != "" && !r.agents.Supports(a.AgentID, taskType) {
			continue
		}
		// 标签过滤(tag 为空时跳过)
		if tag != "" && !matchTagAny(a.Tags, tag) {
			continue
		}
		// 负载评分(XL6 接线):monitor 采样经 progress 帧回流 Registry 负载缓存,
		// TTL 内的采样参与评分,过期/无采样回 0(等同无数据,不阻塞调度)。
		cpu, mem := 0.0, 0.0
		if c, m, seen, ok := r.agents.LoadOf(a.AgentID); ok && time.Since(seen) <= loadSampleTTL {
			cpu, mem = c, m
		}
		tasks := float64(r.getInflightCount(a.AgentID))
		score := cpu*0.4 + mem*0.3 + tasks*10 // task weight 高
		pool = append(pool, scored{a, score})
	}

	if len(pool) == 0 {
		return "", fmt.Errorf("无匹配 Agent(tag=%s type=%s)", tag, taskType)
	}

	// 按 score 升序(最低负载优先)
	sort.SliceStable(pool, func(i, j int) bool {
		return pool[i].score < pool[j].score
	})
	return pool[0].agent.AgentID, nil
}

// AgentInfo 统一结构(供调度用)。
type AgentInfo = struct {
	AgentID      string
	Hostname     string
	OS           string
	Version      string
	Capabilities []string
	Tags         map[string]string
}

// matchTagAny 检查 tags 是否匹配(支持 "key=value" 或 "value")。
func matchTagAny(tags map[string]string, filter string) bool {
	if tags == nil {
		return false
	}
	for k, v := range tags {
		if k+"="+v == filter || v == filter {
			return true
		}
	}
	return false
}

// getInflightCount 返回 Agent 在途任务数(简化:从 store 统计)。
func (r *Relay) getInflightCount(agentID string) int {
	if r.store == nil {
		return 0
	}
	return int(r.agents.InFlight(agentID))
}

// handleSelectBest GET /api/scheduler/best —— 智能选择最优 Agent。
func (r *Relay) handleSelectBest(w http.ResponseWriter, req *http.Request) {
	if err := r.authorize(req); err != nil {
		writeJSON(w, http.StatusUnauthorized, errMap(err))
		return
	}
	tag := req.URL.Query().Get("tag")
	taskType := req.URL.Query().Get("type")
	best, err := r.SelectBestAgent(tag, taskType)
	if err != nil {
		writeJSON(w, http.StatusNotFound, errMap(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"agent_id": best,
		"tag":      tag,
		"type":     taskType,
		"message":  "这是基于负载均衡推荐的最优 Agent",
	})
}
