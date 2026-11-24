// Package main — 审计日志查询端点(控制台审计浏览面板消费)。
//
// 与 /api/audit/export(SIEM 机器格式导出)互补:query 面向人(控制台 UI),
// 返回结构化 JSON + 过滤 + 分页。
//
// GET /api/audit/query?event=task_submitted&agent_id=agent-1&since=24h&offset=0&limit=50
package main

import (
	"bytes"
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
	Tenant  string   // 2026 租户加固:非空=只看该租户的记录(旧记录无租户 → 不可见,fail-closed);空=admin 全量
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
	_, id, err := r.authenticateCtx(req)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, errMap(err))
		return
	}
	if r.audit == nil {
		writeJSON(w, http.StatusServiceUnavailable, errMap(fmt.Errorf("审计未启用")))
		return
	}

	// 解析过滤参数(2026 租户加固:租户范围取服务端绑定身份,不可由请求参数声明)
	f := auditQueryFilter{Tenant: id.Tenant}
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

	// 读文件 + 过滤(单遍扫描:性能轮 2026-11-14,见 queryAuditFile)
	matched := queryAuditFile(r.audit.Path(), f)

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
// 性能轮 2026-11-14:splitLines 零切分行替代 string 转换+逐行二次拷贝。
func readAuditFile(path string) []auditRecord {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	lines := splitLines(data)
	out := make([]auditRecord, 0, len(lines))
	for _, line := range lines {
		if len(line) == 0 {
			continue
		}
		var rec auditRecord
		if json.Unmarshal(line, &rec) == nil {
			out = append(out, rec)
		}
	}
	return out
}

// filterAuditEntries 按条件过滤(纯函数,单测口径保留;handler 热路径走 queryAuditFile)。
// 匹配数超过 auditQueryCap 截断。
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
		if f.Tenant != "" && e.Tenant != f.Tenant {
			continue
		}
		out = append(out, e)
		if len(out) >= auditQueryCap {
			break
		}
	}
	return out
}

// queryAuditFile 单遍完成"读文件+过滤"(性能轮 2026-11-14)。
//
// 旧路径 readAuditFile+filterAuditEntries 三宗罪(20000 条基准 280k allocs/57MB):
//  1. strings.Split(string(data)) 先整文件转 string(全量拷贝),每行再 []byte(line)
//     二次拷贝——同一份字节被复制 3 遍
//  2. 无过滤条件也全量 unmarshal——checkValid+decode 占实测 CPU ~45%
//  3. append 无预分配,20000 条翻倍重分配
//
// 优化(过滤语义与 filterAuditEntries 逐位等价,含 auditQueryCap 截断):
//   - splitLines(audit.go,VerifyChain 同款)零切分行,不发生拷贝
//   - 必要条件子串预筛:精确匹配 agent_id/event 的行,其 JSON 文本必含该值子串
//     (仅对 JSON 安全串生效,见 jsonSafePlain);子串不命中直接跳过整行 unmarshal
//   - 结果按行数预分配
func queryAuditFile(path string, f auditQueryFilter) []auditRecord {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	lines := splitLines(data)

	var eventSet map[string]bool
	var eventNeedles [][]byte
	if len(f.Events) > 0 {
		eventSet = make(map[string]bool, len(f.Events))
		allSafe := true // 任一值 JSON 不安全即整体禁用预筛(等价性测试逮住:混合安全/不安全值时,不安全值的行必不含安全 needle)
		for _, e := range f.Events {
			eventSet[e] = true
			if !jsonSafePlain(e) {
				allSafe = false
			}
		}
		if allSafe {
			for _, e := range f.Events {
				eventNeedles = append(eventNeedles, []byte(e))
			}
		}
	}
	var agentNeedle []byte
	if f.AgentID != "" && jsonSafePlain(f.AgentID) {
		agentNeedle = []byte(f.AgentID)
	}
	var tenantNeedle []byte
	if f.Tenant != "" && jsonSafePlain(f.Tenant) {
		tenantNeedle = []byte(f.Tenant)
	}

	out := make([]auditRecord, 0, len(lines))
	for _, line := range lines {
		if len(line) == 0 {
			continue
		}
		// 预筛(必要条件):子串不命中必不匹配,免 unmarshal
		if agentNeedle != nil && !bytes.Contains(line, agentNeedle) {
			continue
		}
		if len(eventNeedles) > 0 && !containsAnyBytes(line, eventNeedles) {
			continue
		}
		if tenantNeedle != nil && !bytes.Contains(line, tenantNeedle) {
			continue
		}
		var rec auditRecord
		if json.Unmarshal(line, &rec) != nil {
			continue // 坏行跳过(与 readAuditFile 一致)
		}
		// 精确过滤(预筛只是必要条件,这里才是判据)
		if f.SinceMs != 0 && rec.TS < f.SinceMs {
			continue
		}
		if len(eventSet) > 0 && !eventSet[rec.Event] {
			continue
		}
		if f.AgentID != "" && rec.AgentID != f.AgentID {
			continue
		}
		if f.Tenant != "" && rec.Tenant != f.Tenant {
			continue
		}
		out = append(out, rec)
		if len(out) >= auditQueryCap {
			break
		}
	}
	return out
}

// jsonSafePlain 判断 s 的 JSON 编码是否与原串逐字节相同
// (可打印 ASCII 且不含 '"' 与 '\\')。仅此时"行内必含 s 子串"才成立——
// 含转义字符的值(如 quote/反斜杠/非 ASCII)编码后会变形,子串预筛会漏匹配,
// 必须回退全量 unmarshal(正确性优先,性能其次)。
func jsonSafePlain(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c > 0x7e || c == '"' || c == '\\' {
			return false
		}
	}
	return true
}

// containsAnyBytes line 含任意一个 needle 即 true。
func containsAnyBytes(line []byte, needles [][]byte) bool {
	for _, n := range needles {
		if bytes.Contains(line, n) {
			return true
		}
	}
	return false
}
