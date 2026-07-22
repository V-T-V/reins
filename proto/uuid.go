package proto

import "github.com/google/uuid"

// uuidNewString 返回一个 UUIDv4 字符串。
//
// 单独抽成变量，便于在测试中替换为确定性生成器。
var uuidNewString = func() string {
	return uuid.NewString()
}
