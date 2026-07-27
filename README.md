# go-rmm

> 企业级零信任远程访问 + 自动化运维平台 — Go 实现

[![Go](https://img.shields.io/badge/Go-1.25+-00ADD8?logo=go)](https://go.dev)
[![Version](https://img.shields.io/badge/version-4.1.0-blue)]()
[![License](https://img.shields.io/badge/license-MIT-green)]()
[![Tests](https://img.shields.io/badge/tests-216-brightgreen)]()
[![CLI](https://img.shields.io/badge/CLI-51%20commands-blue)]()
[![Coverage](https://img.shields.io/badge/proto%20coverage-91.3%25-brightgreen)]()

远程控制 Windows/Linux 主机:命令执行、文件传输、交互式 Shell、端口转发、多步编排、系统监控、告警、自更新。

## ✨ 核心特性

### 远程操作(15 命令)
| 能力 | 命令 |
|------|------|
| 命令执行 | `task run -a <agent> -c "Get-Date"` |
| 流式执行 | `task stream -a <agent> -c "ping -t 8.8.8.8"` (SSE 实时 stdout) |
| 多 Agent 并行 | `exec -a a1,a2,a3 -c "hostname"` |
| 交互式 Shell | `shell open -a <agent>` (asciinema 录屏) |
| 文件传输 | `file upload/download` (SHA256 + 断点续传) |
| 目录同步 | `sync push/pull` |
| 端口转发 | `tunnel -L 3306:db:3306` (SSH -L) |

### 自动化编排(6 命令)
| 能力 | 命令 |
|------|------|
| 多步部署 | `deploy run playbook.json` |
| 批量部署 | `deploy batch pb.json --tag env=prod` |
| 工作流 | `workflow run wf.json` (条件+变量提取+on_failure) |
| 告警规则 | `alert add --agent X --metric cpu --gt 90` |
| 健康巡检 | `health-check --tag prod --schedule "*/5 * * * *"` |
| 进程守护 | `watch add --svc MyAppService` |

### 多 Agent 分析(5 命令)
| 能力 | 命令 |
|------|------|
| 拓扑视图 | `topology` (按 tag 分组) |
| 批量诊断 | `diagnose --tag prod` (并行体检+告警) |
| 差异对比 | `compare agent-1 agent-2` |
| 清单导出 | `inventory --format csv` |
| 历史统计 | `history -n 100` (成功率+分布) |

### 系统监控(7 命令)
| 能力 | 命令 |
|------|------|
| 系统体检 | `sysinfo -a <agent>` |
| 实时监控 | `monitor top` (glances TUI: CPU/Mem/Disk/Net/GPU) |
| 系统快照 | `snapshot take` + `snapshot diff` |
| 实时事件 | `events` (SSE: agent上线/任务结果/策略拦截) |
| 网络诊断 | `net ping/dns/trace/ports/check` |
| 日志聚合 | `logs tail/read/search` |
| 文件搜索 | `find name/grep/stat/du` |

### 企业级能力
| 能力 | 说明 |
|------|------|
| **多租户 RBAC** | `tenant:role:secret` token 格式 |
| **审计防篡改** | hash chain + SIEM 导出(CEF/Syslog) |
| **Secret 管理** | 凭据 TTL+一次性,不落 audit |
| **Session 录屏** | asciinema v2,可回放 |
| **可观测性** | slog + Prometheus + pprof |
| **Webhook 告警** | Slack/钉钉/企业微信 |
| **水平扩展** | TaskStore interface + Redis + Pub/Sub |
| **AI Agent SDK** | `client.New()` → 45+ 方法 + 流式执行 |

## 🚀 快速开始

```bash
# 1. 服务端(一台机器)
./go-rmm-relay                    # 零配置启动,自动生成 token
# → 浏览器打开 http://localhost:7780

# 2. 被控端(每台目标机)
./go-rmm-agent run -relay ws://server:7780/agent -token <token>

# 3. 操作(你的电脑)
go-rmm --relay http://server:7780 --token <token> agent list
go-rmm --relay http://server:7780 --token <token> task run -a <agent> -c "hostname"
go-rmm --relay http://server:7780 --token <token> monitor top -a <agent>
```

## 📦 部署

| 方式 | 命令 |
|------|------|
| Linux | `./scripts/quickstart.sh server` |
| Windows | `.\scripts\quickstart.ps1` |
| Docker | `docker build -f Dockerfile.relay -t go-rmm . && docker run -p 7780:7780 go-rmm` |
| K8s | `helm install rmm deploy/helm/go-rmm` |
| 服务器 | `./scripts/deploy-to-server.sh` |

## 🔌 AI Agent 接入

```go
import "github.com/QiuShichang/go-rmm/client"

c := client.New("https://relay:7780", "token")

// 发现工具(33+ 个)
tools, _ := c.ListTools()

// 执行命令
stdout, _ := c.RunCommand("agent-1", "Get-Date", 10)

// 流式执行(实时 stdout)
result, _ := c.StreamCommand(ctx, "agent-1", "ping 8.8.8.8", 30, func(event string, data json.RawMessage) {
    fmt.Println(event, string(data))
})

// 系统体检
info, _ := c.Sysinfo("agent-1")
```

## 📊 指标

| 项目 | 数值 |
|------|------|
| Go 文件 | 154 |
| 代码行 | 28,000+ |
| CLI 命令 | 51 |
| API 端点 | 42 |
| SDK 方法 | 45+ |
| 测试用例 | 216 |
| 基准测试 | 22 |
| 性能优化 | 8 项 (55x~1940x) |

## 📄 文档
- [部署指南](./DEPLOYMENT.md)
- [性能基线](./BENCHMARK.md)
- [API 文档](./deploy/openapi.yaml)
- [架构详解](./AGENTS.md)

## 📜 License: MIT
