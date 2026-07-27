// Package proto — 二进制 WS 帧编解码(文件传输 + 隧道复用)。
//
// M3 修复:原 buildBinaryFrame 在 agent/filetransfer.go 和
// buildRelayBinaryFrame 在 relay/router.go 各定义一份,逻辑相同但各自维护。
// 移到 proto/frame.go 统一定义,三端共用,消除协议一致性隐患。
//
// 阶段 1.2 扩展:支持多通道类型(文件 + 隧道),100% 向后兼容。
//
// 帧格式(文件传输,旧):
//
//	[id_len u8][id][isLast u8][data]
//	首字节 = id_len,范围 0-255。旧格式 id_len 实际 ≤ 64(transferID 长度),
//	所以首字节 < 0x40。我们用 ≥ 0x80 作为新格式标记。
//
// 帧格式(隧道,新):
//
//	[0x80][chanType u8][id_len u8][id][flags u8][data]
//	首字节 0x80 = 新格式标记; chanType: 0x10=隧道数据, 0x11=隧道控制
package proto

import "fmt"

// 通道类型常量(新格式 chanType 字段)。
const (
	ChanTypeTunnelData = 0x10 // 隧道数据帧
	ChanTypeTunnelCtrl = 0x11 // 隧道控制帧
)

// newFrameMarker 新格式首字节标记(≥ 0x80 与旧格式 id_len 区分)。
const newFrameMarker = 0x80

// BuildBinaryFrame 构造文件传输二进制 WS 帧(旧格式,向后兼容)。
func BuildBinaryFrame(transferID string, isLast bool, data []byte) []byte {
	idBytes := []byte(transferID)
	if len(idBytes) > 255 {
		idBytes = idBytes[:255]
	}
	frame := make([]byte, 0, 1+len(idBytes)+1+len(data))
	frame = append(frame, byte(len(idBytes)))
	frame = append(frame, idBytes...)
	if isLast {
		frame = append(frame, 1)
	} else {
		frame = append(frame, 0)
	}
	frame = append(frame, data...)
	return frame
}

// ParseBinaryFrame 解析二进制 WS 帧,返回 transferID + isLast + data。
// 自动识别新旧格式(首字节 ≥ 0x80 为新格式隧道帧,否则旧格式文件帧)。
//
// 旧格式:[id_len][id][isLast][data] → (transferID, isLast, data, false)
// 新格式:[0x80][chanType][id_len][id][flags][data] → 拆为通用字段
func ParseBinaryFrame(frame []byte) (transferID string, isLast bool, data []byte, err error) {
	if len(frame) < 2 {
		return "", false, nil, fmt.Errorf("帧太短")
	}
	// 阶段 1.2:检测新格式(隧道)
	// 用 IsTunnelFrame(含 chanType 合法性检查)避免长 ID 误判
	if IsTunnelFrame(frame) {
		// 新格式隧道帧,走 parseTunnelFrameRaw
		tid, _, flags, payload, e := parseTunnelFrameRaw(frame)
		return tid, flags != 0, payload, e
	}
	// 旧格式文件帧
	idLen := int(frame[0])
	if len(frame) < 1+idLen+1 {
		return "", false, nil, fmt.Errorf("帧不完整")
	}
	transferID = string(frame[1 : 1+idLen])
	isLast = frame[1+idLen] != 0
	data = frame[2+idLen:]
	return transferID, isLast, data, nil
}

// IsTunnelFrame 判断二进制帧是否为隧道帧(新格式)。
// 判断逻辑:首字节 ≥ 0x80(标记) **且** 第二字节是合法 chanType(0x10/0x11)。
// 这避免长 ID(≥128 字节)被误判为隧道帧。
func IsTunnelFrame(frame []byte) bool {
	if len(frame) < 2 {
		return false
	}
	if frame[0]&newFrameMarker == 0 {
		return false
	}
	// 第二字节必须是合法 chanType
	return frame[1] == ChanTypeTunnelData || frame[1] == ChanTypeTunnelCtrl
}

// BuildTunnelFrame 构造隧道二进制帧。
// chanType: ChanTypeTunnelData / ChanTypeTunnelCtrl
// id: tunnelID
// flags: 数据帧=0, 控制帧可携带控制码
// data: 载荷
func BuildTunnelFrame(chanType byte, id string, flags byte, data []byte) []byte {
	idBytes := []byte(id)
	if len(idBytes) > 255 {
		idBytes = idBytes[:255]
	}
	frame := make([]byte, 0, 1+1+1+len(idBytes)+1+len(data))
	frame = append(frame, newFrameMarker) // 0x80 新格式标记
	frame = append(frame, chanType)
	frame = append(frame, byte(len(idBytes)))
	frame = append(frame, idBytes...)
	frame = append(frame, flags)
	frame = append(frame, data...)
	return frame
}

// ParseTunnelFrame 解析隧道帧,返回 tunnelID + chanType + flags + data。
func ParseTunnelFrame(frame []byte) (tunnelID string, chanType, flags byte, data []byte, err error) {
	if len(frame) < 5 {
		return "", 0, 0, nil, fmt.Errorf("隧道帧太短")
	}
	if frame[0]&newFrameMarker == 0 {
		return "", 0, 0, nil, fmt.Errorf("不是隧道帧")
	}
	return parseTunnelFrameRaw(frame)
}

// parseTunnelFrameRaw 内部解析,返回 tunnelID + chanType + flags + data。
func parseTunnelFrameRaw(frame []byte) (tunnelID string, chanType, flags byte, data []byte, err error) {
	// [0x80][chanType][id_len][id][flags][data]
	if len(frame) < 5 {
		return "", 0, 0, nil, fmt.Errorf("隧道帧太短")
	}
	chanType = frame[1]
	idLen := int(frame[2])
	if len(frame) < 3+idLen+1 {
		return "", 0, 0, nil, fmt.Errorf("隧道帧不完整")
	}
	tunnelID = string(frame[3 : 3+idLen])
	flags = frame[3+idLen]
	data = frame[4+idLen:]
	return tunnelID, chanType, flags, data, nil
}
