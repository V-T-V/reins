package taskstore

// 2026 租户加固 · 存储层断言。两个实现(bbolt / 内存)逐条对拍——
// 租户过滤与归属绑定不允许只修一份(首版就漏了内存版:bbolt 滤了租户、内存版照旧全量返回)。

import (
	"path/filepath"
	"sync"
	"testing"

	"github.com/V-T-V/reins/proto"
)

// tenantFixture 三个租户各一条任务 + 一条无租户的旧记录。
func tenantFixture(s TaskStore) {
	save := func(id, tenant string) {
		_ = s.SaveTask(StoredTask{
			Request: proto.TaskRequest{TaskID: id, Type: "powershell", AgentID: "a-" + id},
			Status:  StatusPending,
			Tenant:  tenant,
		})
	}
	save("t-acme-1", "acme")
	save("t-acme-2", "acme")
	save("t-globex-1", "globex")
	save("t-legacy", "") // 加固前的旧记录:无租户字段
}

// taskIDs 取任务 ID 集合(顺序无关)。
func taskIDs(tasks []StoredTask) map[string]bool {
	out := make(map[string]bool, len(tasks))
	for _, t := range tasks {
		out[t.Request.TaskID] = true
	}
	return out
}

func sameSet(got map[string]bool, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for _, w := range want {
		if !got[w] {
			return false
		}
	}
	return true
}

func keys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	return out
}

// assertTenantBehaviors 对给定实现逐条断言租户过滤与绑定语义。
func assertTenantBehaviors(t *testing.T, s TaskStore, name string) {
	t.Helper()
	tenantFixture(s)

	cases := []struct {
		tenant string
		want   []string
	}{
		{"", []string{"t-acme-1", "t-acme-2", "t-globex-1", "t-legacy"}}, // admin 全量
		{"acme", []string{"t-acme-1", "t-acme-2"}},
		{"globex", []string{"t-globex-1"}},
		{"nobody", nil},
	}
	for _, c := range cases {
		got, err := s.ListTasksQuery(TaskQuery{Tenant: c.tenant})
		if err != nil {
			t.Fatalf("%s: ListTasksQuery(tenant=%q): %v", name, c.tenant, err)
		}
		if !sameSet(taskIDs(got), c.want) {
			t.Errorf("%s: tenant=%q 得到 %v,期望 %v", name, c.tenant, keys(taskIDs(got)), c.want)
		}
	}
	// 租户过滤与状态索引路径叠加:走 tasks_by_status 也必须带租户约束
	got, err := s.ListTasksQuery(TaskQuery{Tenant: "acme", Statuses: []string{StatusPending}})
	if err != nil || !sameSet(taskIDs(got), []string{"t-acme-1", "t-acme-2"}) {
		t.Errorf("%s: 租户+状态组合过滤 = %v, %v;期望恰好 acme 的 2 条", name, keys(taskIDs(got)), err)
	}

	// 绑定:insert-if-absent
	if bound, err := s.BindAgentTenant("agent-x", "acme"); err != nil || bound != "acme" {
		t.Fatalf("%s: 首次绑定 = %q, %v", name, bound, err)
	}
	if tn, ok := s.AgentTenant("agent-x"); !ok || tn != "acme" {
		t.Fatalf("%s: AgentTenant = %q,%v", name, tn, ok)
	}
	if bound, err := s.BindAgentTenant("agent-x", "globex"); err != nil || bound != "acme" {
		t.Errorf("%s: 重复绑定不得覆盖:得到 %q, %v,期望 acme", name, bound, err)
	}
	if tn, _ := s.AgentTenant("agent-x"); tn != "acme" {
		t.Errorf("%s: 覆盖后归属被改写为 %q", name, tn)
	}
	if _, ok := s.AgentTenant("never-bound"); ok {
		t.Errorf("%s: 未绑定的 Agent 不应存在", name)
	}
	if _, err := s.BindAgentTenant("", "acme"); err == nil {
		t.Errorf("%s: 空 agent_id 应报错", name)
	}
}

func TestTenantQueryAndBindingBolt(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	assertTenantBehaviors(t, s, "bbolt")
}

func TestTenantQueryAndBindingMemory(t *testing.T) {
	assertTenantBehaviors(t, NewMemoryStore(), "memory")
}

// TestTenantFilterEquivalentAcrossStores 两个实现对同一份数据的租户过滤结果逐条一致
// (内存模式与持久化模式下的隔离语义不得分叉)。
func TestTenantFilterEquivalentAcrossStores(t *testing.T) {
	boltS, err := OpenStore(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer boltS.Close()
	memS := NewMemoryStore()
	tenantFixture(boltS)
	tenantFixture(memS)
	for _, tenant := range []string{"", "acme", "globex", "nobody"} {
		b, err := boltS.ListTasksQuery(TaskQuery{Tenant: tenant})
		if err != nil {
			t.Fatal(err)
		}
		m, _ := memS.ListTasksQuery(TaskQuery{Tenant: tenant})
		if !sameSet(taskIDs(b), keys(taskIDs(m))) {
			t.Errorf("tenant=%q: bbolt=%v memory=%v(两实现不一致)", tenant, keys(taskIDs(b)), keys(taskIDs(m)))
		}
	}
}

// TestAgentBindingPersistsAcrossReopen 绑定写盘,重开库后仍在(重启不清空归属)。
func TestAgentBindingPersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	s1, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s1.BindAgentTenant("kiosk-01", "acme"); err != nil {
		s1.Close()
		t.Fatal(err)
	}
	s1.Close()

	s2, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if tn, ok := s2.AgentTenant("kiosk-01"); !ok || tn != "acme" {
		t.Fatalf("重开后绑定丢失: %q,%v", tn, ok)
	}
}

// TestAgentBindingConcurrentFirstRegistration 并发的两个租户首次注册同一 agentID:
// insert-if-absent 保证只有一个成功,另一个拿到先到者的绑定(不得后到覆盖)。
func TestAgentBindingConcurrentFirstRegistration(t *testing.T) {
	boltS, err := OpenStore(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer boltS.Close()
	for name, s := range map[string]TaskStore{"bbolt": boltS, "memory": NewMemoryStore()} {
		var mu sync.Mutex
		results := map[string]string{}
		var wg sync.WaitGroup
		for _, tenant := range []string{"acme", "globex"} {
			wg.Add(1)
			go func(tn string) {
				defer wg.Done()
				bound, err := s.BindAgentTenant("shared-01", tn)
				if err != nil {
					t.Errorf("%s: 绑定失败: %v", name, err)
				}
				mu.Lock()
				results[tn] = bound
				mu.Unlock()
			}(tenant)
		}
		wg.Wait()
		final, ok := s.AgentTenant("shared-01")
		if !ok || (final != "acme" && final != "globex") {
			t.Fatalf("%s: 并发绑定结果异常: %q,%v", name, final, ok)
		}
		loser := "acme"
		if final == "acme" {
			loser = "globex"
		}
		if results[final] != final || results[loser] != final {
			t.Errorf("%s: 先到者 %q 应成立、后到者 %q 应拿回先到者的绑定;得到 acme→%q globex→%q",
				name, final, loser, results["acme"], results["globex"])
		}
	}
}
