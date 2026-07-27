package proto

import (
	"encoding/json"
	"testing"
)

func TestNewEnvelope(t *testing.T) {
	env, err := NewEnvelope(TypeTaskRequest, "agent-1", "relay", nil)
	if err != nil {
		t.Fatalf("NewEnvelope 失败: %v", err)
	}
	if env.Type != TypeTaskRequest {
		t.Errorf("Type = %s, 期望 task_request", env.Type)
	}
	if env.From != "agent-1" {
		t.Errorf("From = %s", env.From)
	}
	if env.To != "relay" {
		t.Errorf("To = %s", env.To)
	}
	if env.Version != Version {
		t.Errorf("Version = %d, 期望 %d", env.Version, Version)
	}
	if env.ID == "" {
		t.Error("ID 不应为空")
	}
	if env.TS == 0 {
		t.Error("TS 不应为 0")
	}
}

func TestNewEnvelopeWithPayload(t *testing.T) {
	task := TaskRequest{TaskID: "t1", Type: TaskPowerShell}
	env, err := NewEnvelope(TypeTaskRequest, "relay", "agent-1", task)
	if err != nil {
		t.Fatalf("NewEnvelope 失败: %v", err)
	}
	if len(env.Payload) == 0 {
		t.Error("Payload 不应为空")
	}
	var decoded TaskRequest
	if err := env.Decode(&decoded); err != nil {
		t.Fatalf("Decode 失败: %v", err)
	}
	if decoded.TaskID != "t1" {
		t.Errorf("TaskID = %s, 期望 t1", decoded.TaskID)
	}
}

func TestEnvelopeDecode(t *testing.T) {
	// 空 payload
	env := Envelope{}
	var r Register
	if err := env.Decode(&r); err != nil {
		t.Errorf("空 payload Decode 应返回 nil,得到: %v", err)
	}
	// 有 payload
	env.Payload = json.RawMessage(`{"agent_id":"a1","hostname":"host"}`)
	if err := env.Decode(&r); err != nil {
		t.Fatalf("Decode 失败: %v", err)
	}
	if r.AgentID != "a1" {
		t.Errorf("AgentID = %s", r.AgentID)
	}
	if r.Hostname != "host" {
		t.Errorf("Hostname = %s", r.Hostname)
	}
	// 无效 JSON
	env.Payload = json.RawMessage(`{invalid}`)
	if err := env.Decode(&r); err == nil {
		t.Error("无效 JSON 应返回 error")
	}
}

func TestIsValidVersion(t *testing.T) {
	env := Envelope{Version: Version}
	if !env.IsValidVersion() {
		t.Error("当前版本应有效")
	}
	env.Version = 2
	if env.IsValidVersion() {
		t.Error("版本 2 应无效")
	}
	env.Version = 0
	if env.IsValidVersion() {
		t.Error("版本 0 应无效")
	}
}

func TestIsValidTaskType(t *testing.T) {
	validTypes := []string{
		TaskPowerShell, TaskScript, TaskFile, TaskScreenshot,
		TaskBrowse, TaskGUIClick, TaskUpdate, TaskPower, TaskMonitor,
	}
	for _, tt := range validTypes {
		if !IsValidTaskType(tt) {
			t.Errorf("IsValidTaskType(%s) = false, 期望 true", tt)
		}
	}
	invalidTypes := []string{"", "unknown", "powershell2", "EXEC"}
	for _, tt := range invalidTypes {
		if IsValidTaskType(tt) {
			t.Errorf("IsValidTaskType(%s) = true, 期望 false", tt)
		}
	}
}

func TestIsTerminalStatus(t *testing.T) {
	terminal := []string{StatusCompleted, StatusFailed, StatusCancelled}
	for _, s := range terminal {
		if !IsTerminalStatus(s) {
			t.Errorf("IsTerminalStatus(%s) = false, 期望 true", s)
		}
	}
	nonTerminal := []string{StatusPending, StatusDispatched, StatusAcknowledged, ""}
	for _, s := range nonTerminal {
		if IsTerminalStatus(s) {
			t.Errorf("IsTerminalStatus(%s) = true, 期望 false", s)
		}
	}
}

// === 二进制帧测试 ===

func TestBuildBinaryFrame(t *testing.T) {
	frame := BuildBinaryFrame("xfer-123", false, []byte("hello"))
	if len(frame) < 10 {
		t.Fatalf("帧太短: %d", len(frame))
	}
	// 首字节是 id_len
	if frame[0] != byte(len("xfer-123")) {
		t.Errorf("id_len = %d, 期望 %d", frame[0], len("xfer-123"))
	}
}

func TestParseBinaryFrame(t *testing.T) {
	orig := []byte("test data payload")
	frame := BuildBinaryFrame("transfer-1", true, orig)
	tid, isLast, data, err := ParseBinaryFrame(frame)
	if err != nil {
		t.Fatalf("ParseBinaryFrame 失败: %v", err)
	}
	if tid != "transfer-1" {
		t.Errorf("transferID = %s", tid)
	}
	if !isLast {
		t.Error("isLast 应为 true")
	}
	if string(data) != string(orig) {
		t.Errorf("data = %q, 期望 %q", string(data), string(orig))
	}
}

func TestParseBinaryFrameEmpty(t *testing.T) {
	// 空数据帧
	frame := BuildBinaryFrame("t1", false, nil)
	tid, isLast, data, err := ParseBinaryFrame(frame)
	if err != nil {
		t.Fatalf("Parse 失败: %v", err)
	}
	if tid != "t1" {
		t.Errorf("tid = %s", tid)
	}
	if isLast {
		t.Error("isLast 应为 false")
	}
	if len(data) != 0 {
		t.Errorf("data 应为空,得到 %d 字节", len(data))
	}
}

func TestParseBinaryFrameInvalid(t *testing.T) {
	// 太短
	_, _, _, err := ParseBinaryFrame([]byte{0x01})
	if err == nil {
		t.Error("太短的帧应报错")
	}
	// 帧不完整
	_, _, _, err = ParseBinaryFrame([]byte{0x05, 0x41, 0x42}) // 声称 id_len=5 但只有 2 字节
	if err == nil {
		t.Error("不完整的帧应报错")
	}
}

// === 隧道帧测试 ===

func TestBuildTunnelFrame(t *testing.T) {
	frame := BuildTunnelFrame(ChanTypeTunnelData, "tun-abc", 0, []byte("data"))
	if !IsTunnelFrame(frame) {
		t.Error("IsTunnelFrame 应为 true")
	}
}

func TestParseTunnelFrame(t *testing.T) {
	orig := []byte("tunnel payload")
	frame := BuildTunnelFrame(ChanTypeTunnelData, "tun-1", 0, orig)
	tid, chanType, flags, data, err := ParseTunnelFrame(frame)
	if err != nil {
		t.Fatalf("ParseTunnelFrame 失败: %v", err)
	}
	if tid != "tun-1" {
		t.Errorf("tunnelID = %s", tid)
	}
	if chanType != ChanTypeTunnelData {
		t.Errorf("chanType = %d", chanType)
	}
	if flags != 0 {
		t.Errorf("flags = %d", flags)
	}
	if string(data) != string(orig) {
		t.Errorf("data = %q", string(data))
	}
}

func TestParseBinaryFrameOnTunnel(t *testing.T) {
	// ParseBinaryFrame 应能处理隧道帧(向后兼容)
	frame := BuildTunnelFrame(ChanTypeTunnelData, "tun-x", 1, []byte("hi"))
	tid, isLast, data, err := ParseBinaryFrame(frame)
	if err != nil {
		t.Fatalf("ParseBinaryFrame 对隧道帧失败: %v", err)
	}
	if tid != "tun-x" {
		t.Errorf("tid = %s", tid)
	}
	if !isLast {
		t.Error("isLast 应为 true(flags=1)")
	}
	if string(data) != "hi" {
		t.Errorf("data = %q", string(data))
	}
}

func TestIsTunnelFrame(t *testing.T) {
	// 文件帧不是隧道帧
	fileFrame := BuildBinaryFrame("xfer-1", false, []byte("x"))
	if IsTunnelFrame(fileFrame) {
		t.Error("文件帧不应是隧道帧")
	}
	// 隧道帧
	tunnelFrame := BuildTunnelFrame(ChanTypeTunnelData, "t1", 0, nil)
	if !IsTunnelFrame(tunnelFrame) {
		t.Error("隧道帧应识别为隧道帧")
	}
}

// === 消息类型常量完整性 ===

func TestMessageTypeConstants(t *testing.T) {
	// 确保关键类型常量不为空
	types := []string{
		TypeRegister, TypeHeartbeat, TypeTaskRequest, TypeTaskResult,
		TypeTaskProgress, TypeTaskAck, TypeTaskCancel, TypeTaskNack,
		TypeShellOpen, TypeShellInput, TypeShellOutput, TypeShellClose,
		TypeFileTransferInit, TypeFileTransferReady, TypeFileTransferComplete,
		TypeTunnelOpen, TypeTunnelReady, TypeTunnelClose,
	}
	for _, tt := range types {
		if tt == "" {
			t.Error("消息类型常量不应为空")
		}
	}
}

func TestTaskRequestSerialization(t *testing.T) {
	orig := TaskRequest{
		TaskID:         "task-123",
		Type:           TaskPowerShell,
		AgentID:        "agent-1",
		Payload:        json.RawMessage(`{"command":"Get-Date"}`),
		Timeout:        60,
		IdempotencyKey: "idem-key",
		Attempt:        3,
	}
	data, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("Marshal 失败: %v", err)
	}
	var decoded TaskRequest
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal 失败: %v", err)
	}
	if decoded.TaskID != orig.TaskID {
		t.Errorf("TaskID = %s", decoded.TaskID)
	}
	if decoded.IdempotencyKey != orig.IdempotencyKey {
		t.Errorf("IdempotencyKey = %s", decoded.IdempotencyKey)
	}
	if decoded.Attempt != orig.Attempt {
		t.Errorf("Attempt = %d", decoded.Attempt)
	}
}

func TestMonitorSampleSerialization(t *testing.T) {
	orig := MonitorSample{
		Timestamp:    1234567890,
		CPUUsage:     45.2,
		MemTotalMB:   32768,
		MemFreeMB:    8192,
		MemUsedPct:   75.0,
		CPUPerCore:   []float64{10, 20, 30, 40},
		DiskIO:       []DiskIOInfo{{Device: "C:", ReadMBps: 1.5, WriteMBps: 0.5}},
		TopProcesses: []ProcInfo{{PID: 1234, Name: "chrome", CPU: 50, MemMB: 200}},
	}
	data, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("Marshal 失败: %v", err)
	}
	var decoded MonitorSample
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal 失败: %v", err)
	}
	if decoded.CPUUsage != orig.CPUUsage {
		t.Errorf("CPUUsage = %f", decoded.CPUUsage)
	}
	if len(decoded.CPUPerCore) != 4 {
		t.Errorf("CPUPerCore len = %d", len(decoded.CPUPerCore))
	}
	if len(decoded.DiskIO) != 1 || decoded.DiskIO[0].Device != "C:" {
		t.Errorf("DiskIO 解析错误")
	}
}

func TestPowerTaskSerialization(t *testing.T) {
	orig := PowerTask{Op: "shutdown", Force: true, Delay: 30}
	data, _ := json.Marshal(orig)
	var decoded PowerTask
	json.Unmarshal(data, &decoded)
	if decoded.Op != "shutdown" || !decoded.Force || decoded.Delay != 30 {
		t.Errorf("PowerTask 往返不匹配: %+v", decoded)
	}
}

func TestUpdateTaskSerialization(t *testing.T) {
	// 两种模式
	pathMode := UpdateTask{BinaryPath: "/tmp/agent.new", SHA256: "abc", Version: "v1.0"}
	b64Mode := UpdateTask{BinaryB64: "base64data", SHA256: "def", Version: "v2.0", Force: true}
	
	data1, _ := json.Marshal(pathMode)
	data2, _ := json.Marshal(b64Mode)
	
	var d1, d2 UpdateTask
	json.Unmarshal(data1, &d1)
	json.Unmarshal(data2, &d2)
	
	if d1.BinaryPath != "/tmp/agent.new" {
		t.Errorf("BinaryPath 往返失败")
	}
	if d2.BinaryB64 != "base64data" || !d2.Force {
		t.Errorf("BinaryB64 往返失败")
	}
}
