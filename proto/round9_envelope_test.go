package proto

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestNewEnvelopeFieldsPopulated 构造后必填字段(Version/Type/From/ID/TS)填充。
func TestNewEnvelopeFieldsPopulated(t *testing.T) {
	env, err := NewEnvelope(TypeHeartbeat, "agent-1", "relay", Heartbeat{AgentID: "a", Seq: 1})
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	if env.Version != Version {
		t.Errorf("Version=%d 期望 %d", env.Version, Version)
	}
	if env.Type != TypeHeartbeat {
		t.Errorf("Type=%q 期望 %q", env.Type, TypeHeartbeat)
	}
	if env.From != "agent-1" || env.To != "relay" {
		t.Errorf("From/To = %q/%q", env.From, env.To)
	}
	if env.ID == "" {
		t.Error("ID 不应为空")
	}
	if env.TS == 0 {
		t.Error("TS 不应为 0")
	}
	if env.TS > time.Now().UnixMilli()+1000 {
		t.Error("TS 不应是未来时间")
	}
}

// TestNewEnvelopeUniqueIDs 连续构造,每个 Envelope ID 互不相同。
func TestNewEnvelopeUniqueIDs(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		env, _ := NewEnvelope(TypeHeartbeat, "a", "r", nil)
		if seen[env.ID] {
			t.Fatalf("ID 重复: %s", env.ID)
		}
		seen[env.ID] = true
	}
}

// TestNewEnvelopeNilPayloadNoPayload payload=nil 时 Payload 字段为空。
func TestNewEnvelopeNilPayloadNoPayload(t *testing.T) {
	env, err := NewEnvelope(TypeHeartbeat, "a", "r", nil)
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	if len(env.Payload) != 0 {
		t.Errorf("nil payload 时 Payload 应空, 得到 %q", env.Payload)
	}
}

// TestNewEnvelopeUnmarshallablePayloadErrors 无法序列化的 payload 返回错误。
func TestNewEnvelopeUnmarshallablePayloadErrors(t *testing.T) {
	// channel 无法 JSON 序列化
	_, err := NewEnvelope(TypeHeartbeat, "a", "r", make(chan int))
	if err == nil {
		t.Fatal("不可序列化的 payload 应返回错误")
	}
}

// TestEnvelopeDecodeEmptyPayloadReturnsNil 空 Payload 的 Decode 不报错(nil)。
func TestEnvelopeDecodeEmptyPayloadReturnsNil(t *testing.T) {
	env := Envelope{}
	var hb Heartbeat
	if err := env.Decode(&hb); err != nil {
		t.Fatalf("空 Payload Decode 应 nil, got %v", err)
	}
}

// TestEnvelopeDecodeMalformedPayloadErrors 畸形 JSON 载荷解码报错。
// 注:Go json 对结构体字段不匹配是宽容的(忽略未知字段),故错误路径用畸形 JSON 触发。
func TestEnvelopeDecodeMalformedPayloadErrors(t *testing.T) {
	env := Envelope{Payload: json.RawMessage(`{not valid json`)}
	var tr TaskResult
	if err := env.Decode(&tr); err == nil {
		t.Error("畸形 JSON 载荷应报错")
	}
}

// TestEnvelopeDecodeStructTolerance 验证 Go JSON 的宽容解码:
// PowerShellTask 载荷解为 TaskResult 不报错(忽略未知字段),结果为零值。
// 这是协议层的实际行为契约——调用方必须按 Type 选正确类型。
func TestEnvelopeDecodeStructTolerance(t *testing.T) {
	env, _ := NewEnvelope(TypeTaskRequest, "a", "r", PowerShellTask{Command: "x"})
	var tr TaskResult
	if err := env.Decode(&tr); err != nil {
		t.Fatalf("宽容解码不应报错: %v", err)
	}
	// Command 字段不属于 TaskResult → 被忽略,tr 保持零值
	if tr.TaskID != "" || tr.Success {
		t.Errorf("不匹配字段应被忽略,tr 应零值: %+v", tr)
	}
}

// TestHeartbeatOmitemptyBackwardCompat V1 计数器 omitempty:零值不上报,
// 旧 Relay 解码不感知(向后兼容核心不变量)。
func TestHeartbeatOmitemptyBackwardCompat(t *testing.T) {
	old := Heartbeat{AgentID: "a", Seq: 1} // 全部计数器零值
	raw, err := json.Marshal(old)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(raw)
	for _, key := range []string{`"ub"`, `"db"`, `"uc"`, `"dc"`, `"fc"`} {
		if strings.Contains(s, key) {
			t.Errorf("零值计数器 %s 应被 omitempty 省略, json=%s", key, s)
		}
	}
	// 新版带计数器,旧 Relay 解码:多余字段忽略(不报错)
	full := Heartbeat{AgentID: "a", Seq: 1, UploadBytes: 100, FailedCount: 3}
	fullRaw, _ := json.Marshal(full)
	var decoded Heartbeat
	if err := json.Unmarshal(fullRaw, &decoded); err != nil {
		t.Fatalf("反向解码: %v", err)
	}
	if decoded.UploadBytes != 100 || decoded.FailedCount != 3 {
		t.Errorf("计数器往返不一致: %+v", decoded)
	}
}

// TestHeartbeatRoundTrip 零值与满值心跳的 Envelope 往返。
func TestHeartbeatRoundTrip(t *testing.T) {
	hb := Heartbeat{AgentID: "h", Seq: 42, UploadBytes: 1024}
	env, _ := NewEnvelope(TypeHeartbeat, "h", "relay", hb)
	var got Heartbeat
	if err := env.Decode(&got); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.AgentID != "h" || got.Seq != 42 || got.UploadBytes != 1024 {
		t.Errorf("往返不一致: %+v", got)
	}
}

// TestRegisterTagsRoundTrip Register.Tags 地图往返(多租户/调度依赖)。
func TestRegisterTagsRoundTrip(t *testing.T) {
	reg := Register{
		AgentID: "r1", Hostname: "h", OS: "windows/amd64", Version: "v1",
		Capabilities: []string{TaskPowerShell, TaskFile},
		Tags:         map[string]string{"env": "prod", "power_allowed": "true"},
	}
	env, _ := NewEnvelope(TypeRegister, "r1", "relay", reg)
	var got Register
	if err := env.Decode(&got); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.Tags["env"] != "prod" || got.Tags["power_allowed"] != "true" {
		t.Errorf("Tags 往返不一致: %+v", got.Tags)
	}
	if len(got.Capabilities) != 2 {
		t.Errorf("Capabilities 长度=%d 期望 2", len(got.Capabilities))
	}
}

// TestShellInputOutputRoundTrip shell 会话消息往返。
func TestShellInputOutputRoundTrip(t *testing.T) {
	si := ShellInput{SessionID: "s1", DataB64: "bHMgLWxhCg=="}
	env, _ := NewEnvelope(TypeShellInput, "console", "agent", si)
	var got ShellInput
	if err := env.Decode(&got); err != nil {
		t.Fatalf("Decode ShellInput: %v", err)
	}
	if got.SessionID != "s1" || got.DataB64 != "bHMgLWxhCg==" {
		t.Errorf("ShellInput 往返不一致: %+v", got)
	}
}

// TestErrorPayloadRoundTrip 错误载荷往返。
func TestErrorPayloadRoundTrip(t *testing.T) {
	ep := ErrorPayload{Code: "agent_busy", Message: "max-tasks reached"}
	env, _ := NewEnvelope("error", "agent", "relay", ep)
	var got ErrorPayload
	if err := env.Decode(&got); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.Code != "agent_busy" || got.Message != "max-tasks reached" {
		t.Errorf("ErrorPayload 往返不一致: %+v", got)
	}
}

// TestTaskNackRoundTrip Nack(拒绝执行)往返。
func TestTaskNackRoundTrip(t *testing.T) {
	nack := TaskNack{TaskID: "t1", Reason: "pool_full"}
	env, _ := NewEnvelope(TypeTaskNack, "agent", "relay", nack)
	var got TaskNack
	if err := env.Decode(&got); err != nil {
		t.Fatalf("Decode TaskNack: %v", err)
	}
	if got.TaskID != "t1" || got.Reason != "pool_full" {
		t.Errorf("TaskNack 往返不一致: %+v", got)
	}
}

// TestFileTransferInitRoundTrip 文件传输初始化往返(含 SHA256)。
func TestFileTransferInitRoundTrip(t *testing.T) {
	init := FileTransferInit{
		TransferID: "xfer-1", Direction: "upload",
		Path: "C:/tmp/f.txt", Size: 12345, SHA256: "abc",
	}
	env, _ := NewEnvelope(TypeFileTransferInit, "console", "agent", init)
	var got FileTransferInit
	if err := env.Decode(&got); err != nil {
		t.Fatalf("Decode FileTransferInit: %v", err)
	}
	if got.TransferID != "xfer-1" || got.Size != 12345 || got.SHA256 != "abc" {
		t.Errorf("FileTransferInit 往返不一致: %+v", got)
	}
}

// TestEnvelopeJSONRoundTripPreservesFields 完整 Envelope JSON 序列化/反序列化保留全部字段。
func TestEnvelopeJSONRoundTripPreservesFields(t *testing.T) {
	env, _ := NewEnvelope(TypeTaskResult, "agent-9", "relay",
		TaskResult{TaskID: "task-9", Success: true, Stdout: "out"})
	env.ReplyTo = "req-9"
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded Envelope
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.ID != env.ID || decoded.ReplyTo != "req-9" || decoded.From != "agent-9" {
		t.Errorf("字段丢失: ID=%q ReplyTo=%q From=%q", decoded.ID, decoded.ReplyTo, decoded.From)
	}
	var tr TaskResult
	if err := decoded.Decode(&tr); err != nil || tr.TaskID != "task-9" || !tr.Success {
		t.Errorf("载荷丢失: %+v err=%v", tr, err)
	}
}
