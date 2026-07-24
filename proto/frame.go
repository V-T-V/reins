// Package proto — 二进制 WS 帧编解码（文件传输用）。
//
// M3 修复：原 buildBinaryFrame 在 agent/filetransfer.go 和
// buildRelayBinaryFrame 在 relay/router.go 各定义一份，逻辑相同但各自维护。
// 移到 proto/frame.go 统一定义，三端共用，消除协议一致性隐患。
//
// 帧格式：[transferID_len u8][transferID][isLast u8][data]
package proto

import "fmt"

// BuildBinaryFrame 构造二进制 WS 帧。
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

// ParseBinaryFrame 解析二进制 WS 帧，返回 transferID + isLast + data。
func ParseBinaryFrame(frame []byte) (transferID string, isLast bool, data []byte, err error) {
	if len(frame) < 2 {
		return "", false, nil, fmt.Errorf("帧太短")
	}
	idLen := int(frame[0])
	if len(frame) < 1+idLen+1 {
		return "", false, nil, fmt.Errorf("帧不完整")
	}
	transferID = string(frame[1 : 1+idLen])
	isLast = frame[1+idLen] != 0
	data = frame[2+idLen:]
	return transferID, isLast, data, nil
}
