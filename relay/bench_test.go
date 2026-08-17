package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/QiuShichang/go-rmm/internal/ca"
	"github.com/QiuShichang/go-rmm/proto"
)

// BenchmarkSaveTask bbolt SaveTask 吞吐（任务持久化核心路径）。
func BenchmarkSaveTask(b *testing.B) {
	dir := b.TempDir()
	s, _ := OpenStore(filepath.Join(dir, "bench.db"))
	defer s.Close()

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		task := proto.TaskRequest{
			TaskID:  fmt.Sprintf("bench-%d", i),
			Type:    proto.TaskPowerShell,
			AgentID: "bench-agent",
			Payload: json.RawMessage(`{"command":"Get-Process"}`),
		}
		s.SaveTask(StoredTask{Request: task, Status: StatusPending})
	}
}

// BenchmarkUpdateTaskStatus 状态机更新吞吐（写事务内读-校验-写）。
func BenchmarkUpdateTaskStatus(b *testing.B) {
	dir := b.TempDir()
	s, _ := OpenStore(filepath.Join(dir, "bench-sm.db"))
	defer s.Close()

	task := proto.TaskRequest{TaskID: "sm-bench", Type: proto.TaskPowerShell, AgentID: "a"}
	s.SaveTask(StoredTask{Request: task, Status: StatusPending})

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		target := StatusDispatched
		if i%2 == 0 {
			target = StatusPending
		}
		s.UpdateTaskStatus("sm-bench", target)
	}
}

// BenchmarkGetTask 读取吞吐。
func BenchmarkGetTask(b *testing.B) {
	dir := b.TempDir()
	s, _ := OpenStore(filepath.Join(dir, "bench-get.db"))
	defer s.Close()

	task := proto.TaskRequest{TaskID: "get-bench", Type: proto.TaskPowerShell, AgentID: "a"}
	s.SaveTask(StoredTask{Request: task, Status: StatusCompleted})

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		s.GetTask("get-bench")
	}
}

// BenchmarkListTasksByStatus 按状态查询（全表扫描）。
func BenchmarkListTasksByStatus(b *testing.B) {
	dir := b.TempDir()
	s, _ := OpenStore(filepath.Join(dir, "bench-list.db"))
	defer s.Close()

	for i := 0; i < 100; i++ {
		task := proto.TaskRequest{TaskID: fmt.Sprintf("list-%d", i), Type: proto.TaskPowerShell, AgentID: "a"}
		s.SaveTask(StoredTask{Request: task, Status: StatusCompleted})
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		s.ListTasksByStatus(StatusCompleted)
	}
}

// BenchmarkListTasksByStatusLarge 1000 任务时的扫描性能（模拟生产规模）。
func BenchmarkListTasksByStatusLarge(b *testing.B) {
	dir := b.TempDir()
	s, _ := OpenStore(filepath.Join(dir, "bench-large.db"))
	defer s.Close()

	for i := 0; i < 1000; i++ {
		task := proto.TaskRequest{TaskID: fmt.Sprintf("large-%d", i), Type: proto.TaskPowerShell, AgentID: "a"}
		s.SaveTask(StoredTask{Request: task, Status: StatusCompleted})
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		s.ListTasksByStatus(StatusCompleted)
	}
}

// BenchmarkListTasksByStatusRealistic 真实生产场景：
// 10000 个 completed 任务 + 仅 10 个 dispatched（reaper 实际查询的场景）。
// 验证 F1 索引优化：O(匹配数=10) 而非 O(全表=10010)。
func BenchmarkListTasksByStatusRealistic(b *testing.B) {
	dir := b.TempDir()
	s, _ := OpenStore(filepath.Join(dir, "bench-real.db"))
	defer s.Close()

	// 10000 个历史 completed
	for i := 0; i < 10000; i++ {
		task := proto.TaskRequest{TaskID: fmt.Sprintf("hist-%d", i), Type: proto.TaskPowerShell, AgentID: "a"}
		s.SaveTask(StoredTask{Request: task, Status: StatusCompleted})
	}
	// 10 个在途 dispatched（reaper 实际查的）
	for i := 0; i < 10; i++ {
		task := proto.TaskRequest{TaskID: fmt.Sprintf("disp-%d", i), Type: proto.TaskPowerShell, AgentID: "a"}
		s.SaveTask(StoredTask{Request: task, Status: StatusDispatched})
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		// reaper 实际查询：只找在途的少数
		s.ListTasksByStatus(StatusDispatched, StatusAcknowledged)
	}
}

// BenchmarkEventBusBroadcast SSE 广播扇出（10 订阅者 × M 事件）。
func BenchmarkEventBusBroadcast(b *testing.B) {
	bus := NewEventBus()
	for i := 0; i < 10; i++ {
		bus.Subscribe(256)
	}

	ev := Event{Kind: proto.TypeTaskResult, AgentID: "a", TS: time.Now().UnixMilli()}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		bus.Broadcast(ev)
	}
}

// BenchmarkEventBusBroadcastLarge 大扇出（100 订阅者，模拟大规模 Console）。
func BenchmarkEventBusBroadcastLarge(b *testing.B) {
	bus := NewEventBus()
	for i := 0; i < 100; i++ {
		bus.Subscribe(256)
	}

	ev := Event{Kind: proto.TypeTaskResult, AgentID: "a", TS: time.Now().UnixMilli()}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		bus.Broadcast(ev)
	}
}

// BenchmarkEnvelopeMarshal 协议信封序列化（1KB stdout）。
func BenchmarkEnvelopeMarshal(b *testing.B) {
	result := proto.TaskResult{
		TaskID:   "bench-marshal",
		Success:  true,
		Stdout:   strings.Repeat("x", 1024),
		ExitCode: 0,
		Duration: 500,
	}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = proto.NewEnvelope(proto.TypeTaskResult, "agent", "relay", result)
	}
}

// BenchmarkEnvelopeMarshalLarge 大结果序列化（100KB，模拟截图 base64）。
func BenchmarkEnvelopeMarshalLarge(b *testing.B) {
	result := proto.TaskResult{
		TaskID:   "bench-large-marshal",
		Success:  true,
		Stdout:   strings.Repeat("x", 100*1024), // 100KB
		ExitCode: 0,
		Duration: 500,
	}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = proto.NewEnvelope(proto.TypeTaskResult, "agent", "relay", result)
	}
}

// BenchmarkPolicyCheck 命令策略校验（每条任务必经）。
func BenchmarkPolicyCheck(b *testing.B) {
	p := DefaultCommandPolicy()
	cmd := "Get-Process | Select-Object Name,Id -First 5 | Format-Table -AutoSize"
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		p.CheckCommand(cmd)
	}
}

// BenchmarkCASignAgentCSR CA 签发证书吞吐。
func BenchmarkCASignAgentCSR(b *testing.B) {
	dir := b.TempDir()
	authority, _ := ca.LoadOrCreateCA(dir)

	// 预生成 CSR（不计时）
	key := genBenchmarkKey()
	csrPEM := genBenchmarkCSR(b, key, "bench-sign")

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		authority.SignAgentCSR(csrPEM, fmt.Sprintf("bench-%d", i), time.Hour)
	}
}

// genBenchmarkKey 生成 ECDSA 测试密钥。
func genBenchmarkKey() *ecdsaPrivateKey {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	return k
}

type ecdsaPrivateKey = ecdsa.PrivateKey

// genBenchmarkCSR 生成测试 CSR PEM。
func genBenchmarkCSR(b *testing.B, key *ecdsaPrivateKey, agentID string) []byte {
	csrTmpl := &x509.CertificateRequest{
		Subject:  pkix.Name{CommonName: agentID},
		DNSNames: []string{agentID},
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, csrTmpl, key)
	if err != nil {
		b.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})
}
