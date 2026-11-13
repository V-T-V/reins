package main

// GET /api/tasks 过滤/分页增强测试(status 索引路径 + agent/type 过滤 + offset 翻页)。

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/V-T-V/reins/proto"
)

// seedTasks 向 store 写 N 条任务(交错 status/agent/type)。
func seedTasks(t *testing.T, s TaskStore, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		status := StatusCompleted
		if i%3 == 0 {
			status = StatusFailed
		}
		agent := "agent-a"
		if i%2 == 1 {
			agent = "agent-b"
		}
		typ := "powershell"
		if i%4 == 0 {
			typ = "script"
		}
		task := StoredTask{
			Request: proto.TaskRequest{
				TaskID:  "t-seed-" + string(rune('a'+i)),
				Type:    typ,
				AgentID: agent,
			},
			Status:    status,
			CreatedAt: int64(1000 + i), // 升序时间,验证降序输出
		}
		if err := s.SaveTask(task); err != nil {
			t.Fatal(err)
		}
	}
}

func TestListTasksFilterPaging(t *testing.T) {
	r, srv := newTestRelay(t)
	defer srv.Close()
	seedTasks(t, r.store, 10)

	get := func(q string) struct {
		Tasks  []StoredTask `json:"tasks"`
		Count  int          `json:"count"`
		Total  int          `json:"total"`
		Offset int          `json:"offset"`
	} {
		resp, err := http.Get(srv.URL + "/api/tasks" + q)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("status = %d (q=%s)", resp.StatusCode, q)
		}
		var out struct {
			Tasks  []StoredTask `json:"tasks"`
			Count  int          `json:"count"`
			Total  int          `json:"total"`
			Offset int          `json:"offset"`
		}
		json.NewDecoder(resp.Body).Decode(&out)
		return out
	}

	// 1) 默认:total=10 新在前(CreatedAt 降序)
	out := get("")
	if out.Total != 10 || out.Count != 10 {
		t.Errorf("默认 total=%d count=%d, 期望 10/10", out.Total, out.Count)
	}
	if len(out.Tasks) >= 2 && out.Tasks[0].CreatedAt < out.Tasks[1].CreatedAt {
		t.Errorf("排序错误: 应新的在前")
	}

	// 2) status 过滤(索引路径): i%3==0 → failed 有 i=0,3,6,9 → 4 条
	out = get("?status=failed")
	if out.Total != 4 {
		t.Errorf("status=failed total=%d, 期望 4", out.Total)
	}
	for _, tk := range out.Tasks {
		if tk.Status != StatusFailed {
			t.Errorf("status 过滤泄漏: %+v", tk.Status)
		}
	}

	// 3) 多值 status + agent 组合
	out = get("?status=completed,failed&agent_id=agent-b")
	if out.Total != 5 { // i 奇数 → agent-b,共 5 条
		t.Errorf("组合过滤 total=%d, 期望 5", out.Total)
	}
	for _, tk := range out.Tasks {
		if tk.Request.AgentID != "agent-b" {
			t.Errorf("agent 过滤泄漏: %s", tk.Request.AgentID)
		}
	}

	// 4) type 过滤:i%4==0 → script 有 i=0,4,8 → 3 条
	out = get("?type=script")
	if out.Total != 3 {
		t.Errorf("type=script total=%d, 期望 3", out.Total)
	}

	// 5) 分页:limit=3 offset=3,且页间无重叠、页内降序
	page1 := get("?limit=3")
	page2 := get("?limit=3&offset=3")
	if page1.Count != 3 || page2.Count != 3 || page1.Total != 10 || page2.Offset != 3 {
		t.Errorf("分页元数据错误: p1=%d/%d p2=%d offset=%d", page1.Count, page1.Total, page2.Count, page2.Offset)
	}
	if page1.Tasks[0].Request.TaskID == page2.Tasks[0].Request.TaskID {
		t.Errorf("页间重叠")
	}
	if page2.Tasks[0].CreatedAt > page1.Tasks[0].CreatedAt {
		t.Errorf("翻页方向错误: 第 2 页不应比第 1 页新")
	}

	// 6) offset 越界:空页不 panic
	out = get("?offset=999")
	if out.Count != 0 || out.Total != 10 {
		t.Errorf("越界 offset count=%d total=%d, 期望 0/10", out.Count, out.Total)
	}
}
