package proto

import "encoding/json"

// 命令提取注册表(建议 A / P0-2 修复)。
//
// 原实现 relay/policy.go 与 executors/command_policy.go 各自 switch 枚举 powershell/script
// 提取命令文本,是两份逻辑相同的副本——加新命令类任务需同步改两处,漏改 = 该类型命令
// 不经高危拦截 = 安全洞。现收敛为单点注册表:Relay/Agent 的 CommandPolicy.CheckPayload
// 统一调 ExtractCommand。
//
// 默认注册放在 proto 包(而非 executors init),因为命令提取是"协议层 payload 结构"的知识
// (PowerShellTask.Command / ScriptTask.Content 定义在本包),且 Relay 进程不 import executors,
// 若靠 executors init 注册则 Relay 侧注册表为空 = 安全策略失效。proto 自注册让 Relay/Agent
// 都开箱可用。新增命令类任务:在此 init 追加一行 RegisterCommandExtractor 即可。

// commandExtractors 任务类型 → 命令文本提取器。
var commandExtractors = map[string]func([]byte) string{}

// RegisterCommandExtractor 注册某任务类型的命令文本提取器(用于高危拦截)。
func RegisterCommandExtractor(taskType string, fn func([]byte) string) {
	commandExtractors[taskType] = fn
}

// ExtractCommand 从 payload 提取命令文本(若该类型注册了提取器);否则返回空串(非命令类)。
func ExtractCommand(taskType string, payload []byte) string {
	fn, ok := commandExtractors[taskType]
	if !ok {
		return ""
	}
	return fn(payload)
}

// IsCommandTask 判断某类型是否注册了命令提取器(即命令类任务,需过安全策略)。
func IsCommandTask(taskType string) bool {
	_, ok := commandExtractors[taskType]
	return ok
}

func init() {
	// 内置命令类任务的默认提取器(基于本包 payload 结构定义)。
	RegisterCommandExtractor(TaskPowerShell, func(p []byte) string {
		var t struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal(p, &t)
		return t.Command
	})
	RegisterCommandExtractor(TaskScript, func(p []byte) string {
		var t struct {
			Content string `json:"content"`
		}
		_ = json.Unmarshal(p, &t)
		return t.Content
	})
}
