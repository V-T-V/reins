package proto

// 帧解析器 fuzz(面向远端输入的安全面):
// ParseBinaryFrame/ParseTunnelFrame 消费来自网络的原始字节——畸形输入
// 不得 panic/越界,只能返回错误;合法输入往返恒等。
// `go test -run Fuzz`(种子语料)/`go test -fuzz FuzzParseBinaryFrame -fuzztime 60s`(深度)。

import (
	"encoding/json"
	"testing"
)

func FuzzParseBinaryFrame(f *testing.F) {
	// 种子:正常帧 / 边界(空 ID/超长 ID/截断/空载荷/isLast)
	f.Add([]byte{3, 'a', 'b', 'c', 1})                          // 正常 isLast 空 data
	f.Add(append([]byte{2, 'x', 'y', 0}, []byte("payload")...)) // 正常带 data
	f.Add([]byte{0, 1})                                         // 空 transferID
	f.Add([]byte{})                                             // 空帧
	f.Add([]byte{255})                                          // 长度声明后截断
	f.Add([]byte{1})                                            // 只有长度
	f.Add(append([]byte{255, 'z'}, make([]byte, 4096)...))      // 超长 ID
	f.Add([]byte{1, 'a', 0})                                    // 最小合法
	f.Add([]byte{4, 'a', 'b', 'c', 'd', 2})                     // 非法 isLast 值(宽容)
	f.Fuzz(func(t *testing.T, raw []byte) {
		id, isLast, data, err := ParseBinaryFrame(raw)
		if err != nil {
			return // 畸形输入:必须干净报错,不 panic
		}
		// 隧道帧走隧道格式解析(结果含 chanType),不适用文件帧往返不变量
		if IsTunnelFrame(raw) {
			return
		}
		// 合法文件帧:往返恒等(Build→Parse 逐位复现)
		rebuilt := BuildBinaryFrame(id, isLast, data)
		id2, isLast2, data2, err2 := ParseBinaryFrame(rebuilt)
		if err2 != nil {
			t.Fatalf("重建帧解析失败: %v (raw=%x id=%q)", err2, rebuilt, id)
		}
		if id2 != id || isLast2 != isLast || string(data2) != string(data) {
			t.Fatalf("往返失真: (%q,%v,%d字节) → (%q,%v,%d字节)",
				id, isLast, len(data), id2, isLast2, len(data2))
		}
	})
}

func FuzzParseTunnelFrame(f *testing.F) {
	f.Add([]byte{0x54, 0x4e, 0x01, 0x00, 3, 'a', 'b', 'c'}) // 隧道帧魔数 TN
	f.Add([]byte{0x54, 0x4e})                               // 只有魔数
	f.Add([]byte{})                                         // 空输入
	f.Add([]byte{0x54, 0x4e, 0x01})                         // 类型截断
	f.Add([]byte{0x00, 0x00})                               // 非魔数前缀
	f.Fuzz(func(t *testing.T, raw []byte) {
		tunnelID, chanType, flags, data, err := ParseTunnelFrame(raw)
		if err != nil {
			return // 畸形输入干净报错,不 panic
		}
		// 合法隧道帧:往返恒等(BuildTunnelFrame→ParseTunnelFrame)
		rebuilt := BuildTunnelFrame(chanType, tunnelID, flags, data)
		tid2, ct2, fl2, d2, err2 := ParseTunnelFrame(rebuilt)
		if err2 != nil {
			t.Fatalf("重建隧道帧解析失败: %v (raw=%x)", err2, rebuilt)
		}
		if tid2 != tunnelID || ct2 != chanType || fl2 != flags || string(d2) != string(data) {
			t.Fatalf("隧道往返失真: (%q,%#x,%#x,%d字节) → (%q,%#x,%#x,%d字节)",
				tunnelID, chanType, flags, len(data), tid2, ct2, fl2, len(d2))
		}
	})
}

func FuzzEnvelopeDecode(f *testing.F) {
	// Envelope 宽容解码:畸形 JSON 载荷不炸(旧版/恶意 Agent 可能发任意字节)
	f.Add([]byte(`{"v":1,"type":"task_result","payload":{"task_id":"x"}}`))
	f.Add([]byte(`{"v":1,"type":"xfer_ticket","payload":{"ticket_id":"t","secret_hash":"h","path":"p","direction":"upload","expiry_unix":1}}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`not json`))
	f.Add([]byte(`{"v":1,"type":"task_request","payload":[1,2,3]}`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		var env Envelope
		_ = json.Unmarshal(raw, &env)
		var v any
		_ = env.Decode(&v)
		_ = env.IsValidVersion()
	})
}
