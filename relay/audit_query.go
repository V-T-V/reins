// Package main — 审计日志查询端点(控制台审计浏览面板消费)。
//
// 与 /api/audit/export(SIEM 机器格式导出)互补:query 面向人(控制台 UI),
// 返回结构化 JSON + 过滤 + 分页。
//
// GET /api/audit/query?event=task_submitted&agent_id=agent-1&since=24h&offset=0&limit=50
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// auditQueryCap 单次查询扫描的匹配记录上限(防超大审计文件撑爆内存;
// 正常运维场景远达不到,超限时 total 如实反映截断值)。
const auditQueryCap = 20000

// auditQueryFilter 查询过滤条件(纯数据,便于单元测试)。
type auditQueryFilter struct {
	Events  []string // 精确匹配(event 支持逗号分隔多值);空=不过滤
	AgentID string   // 精确匹配;空=不过滤
	SinceMs int64    // 只看 ts >= now-SinceMs;0=不限
}

// handleAuditQuery GET /api/audit/query —— 审计日志 JSON 查询(过滤+分页)。
//
// 参数:
//
//	event=task_submitted   精确匹配,逗号分隔多值(task_submitted,task_failed)
//	agent_id=agent-1       精确匹配
//	since=24h              只看最近(Go duration,默认 24h;0s=不限)
//	offset=0&limit=50      分页(默认 0/50,limit 上限 500)
//
// 响应: {"entries":[…含 prev_hash/this_hash], "count":N, "total":M, "offset":O, "limit":L}
// 排序: 新的在前(浏览器第一页看最新;与 export 的文件序旧→新相反)。
func (r *Relay) handleAuditQuery(w http.ResponseWriter, req *http.Request) {
	if err := r.authorize(req); err != nil {
		writeJSON(w, http.StatusUnauthorized, errMap(err))
		return
	}
	if r.audit == nil {
		writeJSON(w, http.StatusServiceUnavailable, errMap(fmt.Errorf("审计未启用")))
		return
	}

	// 解析过滤参数
	f := auditQueryFilter{}
	if ev := req.URL.Query().Get("event"); ev != "" {
		for _, s := range strings.Split(ev, ",") {
			if s = strings.TrimSpace(s); s != "" {
				f.Events = append(f.Events, s)
			}
		}
	}
	f.AgentID = req.URL.Query().Get("agent_id")

	sinceMs := int64(0)
	if s := req.URL.Query().Get("since"); s != "" {
		if d, err := time.ParseDuration(s); err == nil {
			sinceMs = time.Now().Add(-d).UnixMilli()
		} else if ms, err := strconv.ParseInt(s, 10, 64); err == nil {
			// 允许直接传毫秒时间戳(下界)
			sinceMs = ms
		}
	}
	f.SinceMs = sinceMs

	offset := 0
	if o := req.URL.Query().Get("offset"); o != "" {
		if n, err := strconv.Atoi(o); err == nil && n > 0 {
			offset = n
		}
	}
	limit := 50
	if l := req.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}

	// 读文件 + 过滤(纯函数,可单测)
	entries := readAuditFile(r.audit.Path())
	matched := filterAuditEntries(entries, f)

	// 新的在前
	sort.SliceStable(matched, func(i, j int) bool {
		return matched[i].TS > matched[j].TS
	})

	total := len(matched)
	start := offset
	if start > total {
		start = total
	}
	end := start + limit
	if end > total {
		end = total
	}
	// 空页保持 [] 而非 null(前端/脚本消费友好)
	page := matched[start:end]
	if page == nil {
		page = []auditRecord{}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"entries": page,
		"count":   len(page),
		"total":   total,
		"offset":  offset,
		"limit":   limit,
	})
}

// readAuditFile 读整个审计文件(JSON Lines),坏行跳过。
// 文件不存在返回 nil(审计刚启用还没落盘是正常态)。
func readAuditFile(path string) []auditRecord {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []auditRecord
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec auditRecord
		if json.Unmarshal([]byte(line), &rec) == nil {
			out = append(out, rec)
		}
	}
	return out
}

// filterAuditEntries 按条件过滤(纯函数)。匹配数超过 auditQueryCap 截断。
func filterAuditEntries(entries []auditRecord, f auditQueryFilter) []auditRecord {
	eventSet := make(map[string]bool, len(f.Events))
	for _, e := range f.Events {
		eventSet[e] = true
	}
	var out []auditRecord
	for _, e := range entries {
		if f.SinceMs != 0 && e.TS < f.SinceMs {
			continue
		}
		if len(eventSet) > 0 && !eventSet[e.Event] {
			continue
		}
		if f.AgentID != "" && e.AgentID != f.AgentID {
			continue
		}
		out = append(out, e)
		if len(out) >= auditQueryCap {
			break
		}
	}
	return out
}
