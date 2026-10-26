package proto

// Phase 1 内网直连传输票据协议测试:
//   - XferTicketGrant Envelope 往返(Relay→Agent 控制帧)
//   - Register.XferAddrs omitempty 向后兼容(旧 Agent 不填,序列化无该字段)

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestXferTicketGrantRoundTrip(t *testing.T) {
	grant := XferTicketGrant{
		TicketID:   "tkt-1234",
		SecretHash: "abcdef0123456789",
		Path:       `C:\reins\slave-workspace\big.tar`,
		Direction:  "upload",
		ExpiryUnix: time.Now().Add(60 * time.Second).Unix(),
	}
	env, err := NewEnvelope(TypeXferTicket, "relay", "agent-1", grant)
	if err != nil {
		t.Fatal(err)
	}
	if env.Type != TypeXferTicket {
		t.Fatalf("type = %q, want %q", env.Type, TypeXferTicket)
	}
	var got XferTicketGrant
	if err := env.Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got != grant {
		t.Errorf("往返不一致:\n got %+v\nwant %+v", got, grant)
	}
}

func TestRegisterXferAddrsOmitEmpty(t *testing.T) {
	// 旧 Agent:不填 XferAddrs → 序列化产物不含 xfer_addrs 字段(向后兼容锚)
	old := Register{AgentID: "a1", Hostname: "h", Capabilities: []string{"file"}}
	b, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "xfer_addrs") {
		t.Errorf("零值 XferAddrs 不应序列化: %s", b)
	}
	var back Register
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.XferAddrs) != 0 {
		t.Errorf("往返后 XferAddrs 应为空,实际 %v", back.XferAddrs)
	}

	// 新 Agent:填直连地址 → 往返保真
	fresh := Register{AgentID: "a2", XferAddrs: []string{"192.168.1.5:7783", "10.0.0.7:7783"}}
	b2, _ := json.Marshal(fresh)
	var back2 Register
	if err := json.Unmarshal(b2, &back2); err != nil {
		t.Fatal(err)
	}
	if len(back2.XferAddrs) != 2 || back2.XferAddrs[0] != "192.168.1.5:7783" {
		t.Errorf("XferAddrs 往返失真: %v", back2.XferAddrs)
	}
}
