// Package main — XL6 智能调度负载接线测试(monitor 采样回流 → SelectBestAgent 评分)。
//
// 覆盖:
//   - parseMonitorLoad:monitor progress Message 的 JSON 段提取
//     (真实格式/嵌套字段/零值省略字段/无 JSON/悬空 '{'/'}' 在 '{' 前/坏 JSON)
//   - Registry.UpdateLoad/LoadOf:未知 Agent/无采样/读写往返/覆盖更新
//   - SelectBestAgent 负载评分:monitor 采样驱动选低载 Agent/
//     在途任务权重压过负载差/TTL 过期采样被无视(宁用无数据不用陈旧数据)
//   - 真 WS E2E:Agent 发 monitor progress 帧 → Registry 负载缓存更新;
//     非 monitor step / 无 JSON 消息不触碰缓存
package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/QiuShichang/reins/proto"
)

// TestParseMonitorLoad —— 纯函数:Message JSON 段提取与全部拒绝路径。
func TestParseMonitorLoad(t *testing.T) {
	cases := []struct {
		name             string
		msg              string
		wantCPU, wantMem float64
		wantOK           bool
	}{
		{
			// executors/monitor.go report() 的真实上报格式:msg + " | " + JSON。
			name:    "真实 monitor 格式",
			msg:     `采样 #3 | {"ts":1726000000,"cpu_usage":42.5,"mem_used_pct":61.25}`,
			wantCPU: 42.5, wantMem: 61.25, wantOK: true,
		},
		{
			name:    "嵌套数组字段不干扰提取",
			msg:     `采集开始 | {"ts":1,"cpu_usage":7.5,"mem_used_pct":33,"procs":[{"pid":4,"name":"System"}],"disks":[{"device":"C:","total_gb":100}]}`,
			wantCPU: 7.5, wantMem: 33, wantOK: true,
		},
		{
			// omitempty:采集失败时 cpu/mem 可能整体缺失 → 解析成功但值为 0。
			name: "零值省略字段 ok=true 值为 0", msg: `x | {"ts":1}`,
			wantCPU: 0, wantMem: 0, wantOK: true,
		},
		{name: "无 JSON", msg: "任务执行中", wantOK: false},
		{name: "悬掉 { 无 }", msg: "prefix { dangling", wantOK: false},
		{name: "} 在 { 之前", msg: "} and then {", wantOK: false},
		{name: "坏 JSON", msg: "x | {not json}", wantOK: false},
		{name: "空串", msg: "", wantOK: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cpu, mem, ok := parseMonitorLoad(tc.msg)
			if ok != tc.wantOK {
				t.Fatalf("ok: want %v, got %v", tc.wantOK, ok)
			}
			if ok && (cpu != tc.wantCPU || mem != tc.wantMem) {
				t.Fatalf("want (%v,%v), got (%v,%v)", tc.wantCPU, tc.wantMem, cpu, mem)
			}
		})
	}
}

// TestRegistryUpdateLoadLoadOf —— 负载缓存读写契约。
func TestRegistryUpdateLoadLoadOf(t *testing.T) {
	r := smartRelay()
	regAgent(r, "a1", []string{"powershell"}, nil)

	// 未注册 Agent:拒绝写。
	if r.agents.UpdateLoad("ghost", 10, 20) {
		t.Fatal("未注册 Agent UpdateLoad 应返回 false")
	}
	// 已注册但从未采样:ok=false,seen 零值。
	if _, _, seen, ok := r.agents.LoadOf("a1"); ok || !seen.IsZero() {
		t.Fatalf("无采样应 ok=false 且 seen 零值,得到 ok=%v seen=%v", ok, seen)
	}
	// 未注册 Agent:读也 false。
	if _, _, _, ok := r.agents.LoadOf("ghost"); ok {
		t.Fatal("未注册 Agent LoadOf 应返回 false")
	}
	// 写入 → 读回,seen 为最近时刻。
	if !r.agents.UpdateLoad("a1", 37.5, 62.5) {
		t.Fatal("已注册 Agent UpdateLoad 应返回 true")
	}
	cpu, mem, seen, ok := r.agents.LoadOf("a1")
	if !ok || cpu != 37.5 || mem != 62.5 {
		t.Fatalf("want (37.5,62.5,true), got (%v,%v,%v)", cpu, mem, ok)
	}
	if seen.IsZero() || time.Since(seen) > 5*time.Second {
		t.Fatalf("seen 应为最近时刻,得到 %v", seen)
	}
	// 覆盖更新:last write wins。
	r.agents.UpdateLoad("a1", 80, 90)
	cpu, mem, _, _ = r.agents.LoadOf("a1")
	if cpu != 80 || mem != 90 {
		t.Fatalf("覆盖更新后 want (80,90), got (%v,%v)", cpu, mem)
	}
}

// TestSelectBestAgentMonitorLoadScoring —— monitor 采样驱动评分(XL6 核心)。
func TestSelectBestAgentMonitorLoadScoring(t *testing.T) {
	t.Run("采样负载驱动选择", func(t *testing.T) {
		r := smartRelay()
		regAgent(r, "hot", []string{"powershell"}, nil)
		regAgent(r, "cool", []string{"powershell"}, nil)
		// hot 分 = 90*0.4+80*0.3 = 60,cool 分 = 5*0.4+10*0.3 = 5 → 必选 cool。
		r.agents.UpdateLoad("hot", 90, 80)
		r.agents.UpdateLoad("cool", 5, 10)
		got, err := r.SelectBestAgent("", "powershell")
		if err != nil {
			t.Fatal(err)
		}
		if got != "cool" {
			t.Fatalf("低载 Agent 应胜出: want cool, got %s", got)
		}
	})

	t.Run("在途任务权重压过负载差", func(t *testing.T) {
		r := smartRelay()
		regAgent(r, "loaded", []string{"powershell"}, nil)
		regAgent(r, "busy", []string{"powershell"}, nil)
		// busy 1 个在途任务(10 分) vs loaded 重载采样(60 分):busy 胜。
		r.agents.UpdateLoad("loaded", 90, 80)
		r.agents.IncInFlight("busy")
		got, err := r.SelectBestAgent("", "powershell")
		if err != nil {
			t.Fatal(err)
		}
		if got != "busy" {
			t.Fatalf("任务权重应最高: want busy(1 在途), got %s", got)
		}
	})

	t.Run("TTL 过期采样被无视", func(t *testing.T) {
		r := smartRelay()
		regAgent(r, "stale-hot", []string{"powershell"}, nil)
		regAgent(r, "fresh-cool", []string{"powershell"}, nil)
		r.agents.UpdateLoad("stale-hot", 95, 90)
		r.agents.UpdateLoad("fresh-cool", 50, 40)
		// 直接回拨采样时刻越过 loadSampleTTL(UpdateLoad 恒记 now,
		// 时钟不可注入,同包测试直接改内部字段是唯一确定性手段)。
		r.agents.mu.Lock()
		r.agents.agents["stale-hot"].loadSeen = time.Now().Add(-3 * time.Minute)
		r.agents.mu.Unlock()
		// stale-hot 采样过期 → 评分回 0;fresh-cool = 50*0.4+40*0.3 = 32。
		// 若陈旧数据被误用(95*0.4+90*0.3=65),stale-hot 必输——过期即无视。
		got, err := r.SelectBestAgent("", "powershell")
		if err != nil {
			t.Fatal(err)
		}
		if got != "stale-hot" {
			t.Fatalf("过期采样应被无视(评分回 0): want stale-hot, got %s", got)
		}
	})
}

// TestMonitorProgressFeedsSchedulerE2E —— 真 WS 全链路:
// Agent 上报 monitor progress → serveAgent 提取采样 → Registry 负载缓存更新;
// 非 monitor step 与无 JSON 的 monitor 消息不触碰缓存。
func TestMonitorProgressFeedsSchedulerE2E(t *testing.T) {
	relay := NewRelay(Config{Dev: true, Addr: ":0"})
	mux := http.NewServeMux()
	relay.registerHTTP(mux)
	mux.HandleFunc("/agent", relay.handleAgentWS)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	agentURL := strings.Replace(srv.URL+"/agent", "http://", "ws://", 1)

	conn, _, err := websocket.Dial(ctx, agentURL, nil) //nolint:bodyclose // 成功时 resp.Body 为 nil
	if err != nil {
		t.Fatalf("agent 连接失败: %v", err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 20)

	regEnv, _ := proto.NewEnvelope(proto.TypeRegister, "load-agent", "relay", proto.Register{
		AgentID: "load-agent", Hostname: "load-host", OS: "linux/amd64",
		Capabilities: []string{proto.TaskMonitor},
	})
	if err := conn.Write(ctx, websocket.MessageText, mustJSON(regEnv)); err != nil {
		t.Fatal(err)
	}

	// 排空读循环(heartbeat_ack 等)。
	go func() {
		for {
			if _, _, rerr := conn.Read(ctx); rerr != nil {
				return
			}
		}
	}()

	// 等 agent 上线。
	deadline := time.Now().Add(3 * time.Second)
	for len(relay.agents.Snapshot()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("agent 未上线")
		}
		time.Sleep(10 * time.Millisecond)
	}

	sendProgress := func(step, message string) {
		t.Helper()
		env, err := proto.NewEnvelope(proto.TypeTaskProgress, "load-agent", "relay",
			proto.TaskProgress{TaskID: "t1", Step: step, Message: message, Percent: 10})
		if err != nil {
			t.Fatal(err)
		}
		if err := conn.Write(ctx, websocket.MessageText, mustJSON(env)); err != nil {
			t.Fatal(err)
		}
	}

	// ① monitor 采样帧 → 负载缓存更新。
	sendProgress("monitor", `采样 #5 | {"ts":1,"cpu_usage":66.5,"mem_used_pct":41.25}`)
	got := false
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cpu, mem, _, ok := relay.agents.LoadOf("load-agent"); ok && cpu == 66.5 && mem == 41.25 {
			got = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !got {
		cpu, mem, _, ok := relay.agents.LoadOf("load-agent")
		t.Fatalf("monitor progress 应回流负载缓存: ok=%v cpu=%v mem=%v", ok, cpu, mem)
	}

	// ② 非 monitor step(即使带 JSON)不触碰缓存。
	sendProgress("powershell", `中 | {"cpu_usage":1,"mem_used_pct":2}`)
	time.Sleep(150 * time.Millisecond)
	if cpu, mem, _, ok := relay.agents.LoadOf("load-agent"); !ok || cpu != 66.5 || mem != 41.25 {
		t.Fatalf("非 monitor step 不应更新负载: ok=%v cpu=%v mem=%v", ok, cpu, mem)
	}

	// ③ monitor step 但无 JSON → 解析失败静默忽略,缓存不变。
	sendProgress("monitor", "采样 #6 | (无 JSON)")
	time.Sleep(150 * time.Millisecond)
	if cpu, mem, _, ok := relay.agents.LoadOf("load-agent"); !ok || cpu != 66.5 || mem != 41.25 {
		t.Fatalf("无 JSON monitor 消息不应更新负载: ok=%v cpu=%v mem=%v", ok, cpu, mem)
	}

	// 采样已入缓存 → 调度评分可用(端到端闭环锚点)。
	best, err := relay.SelectBestAgent("", proto.TaskMonitor)
	if err != nil {
		t.Fatal(err)
	}
	if best != "load-agent" {
		t.Fatalf("唯一 monitor Agent 应被选中,得到 %s", best)
	}
}
