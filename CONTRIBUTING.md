# 贡献指南

## 开发环境
- Go 1.25+

## 常用命令
| 命令 | 作用 |
|------|------|
| `make all` | 编译三组件到 bin/ |
| `go test ./... -count=1` | 全部测试 |
| `go test -race ./...` | 并发安全（需 cgo/gcc） |
| `make lint` / `make vuln` | golangci-lint / govulncheck |

## 架构导航
- `internal/` 共享域包：`ca`（PKI）/`cmdpolicy`（命令策略 32 条）/`taskstore`（存储+状态机+CAS）/`wsx`（P0/P1 优先级写引擎）——改这些会影响 relay+agent 双侧。
- `proto/` 协议内核：Envelope + 二进制帧 + ExtractCommand 注册表。加新命令类任务在 `proto/payload_policy.go` init 注册提取器（单点，双端自动生效）。
- `relay/` 服务端；`agent/` 被控端（写经 wsx 优先级引擎）；`executors/` 执行器（Registry 注册）。

## 约定
- 并发改动必须过 `go test -race`。
- 高危端点需 RequireRole 守卫；高危任务类型需 CanPerform 检查。
- 文件落盘用 temp+rename（参考 agent/enroll.go atomicWriteFile）。
- 提交信息简洁中文，前缀 feat/fix/test/docs/refactor/ci/chore；能力演进登记 CHANGELOG.md。
