# go-rmm · AGENTS.md

## 项目内容(What)

Go 实现的**企业级零信任远程访问平台**(v3.4)——通过反向 WebSocket 中继,让 CLI / Web 控制台远程驱动 Agent 执行全维度运维操作。对标 Teleport / Tailscale。

### 三组件架构

```
CLI (38 命令)  ──HTTP/SSE──►  Relay 中继服务器  ◄──反向WS长连接──  Agent (Windows 服务)
Web UI (内嵌)                  (40 API 端点)                        ├ powershell   PowerShell -EncodedCommand
SDK (Go client)                ├ bbolt/Redis 持久化                  ├ script       脚本执行(BOM)
                               ├ Prometheus /metrics + pprof          ├ file         文件传输(SHA256)
                               ├ mTLS + CA + CRL                      ├ screenshot   PrintWindow 截图
                               ├ RBAC 多租户(admin/op/viewer)         ├ browse       chromedp 浏览器自动化
                               ├ 审计 hash chain + SIEM 导出           ├ guiclick     视觉模型+SendInput
                               ├ Secret 管理(引用式,TTL+一次性)        ├ update       自更新(rename+copy+SCM重启)
                               ├ 脚本库(远端脚本存储+复用)             ├ power        电源操作(shutdown/restart)
                               └ 智能调度(负载均衡选最优 Agent)       └ monitor      流式系统监测(glances TUI)
```

### 核心 CLI 能力(38 命令,8 大分区)

| 分区 | 命令 | 说明 |
|------|------|------|
| 诊断 | status / sysinfo / net ping·dns·trace·ports·check / logs tail·read·search | 远端网络诊断 + 日志聚合 |
| 执行 | task run·batch·list·result·cancel / exec -a/--tag / shell open | 命令执行 + 多 Agent 并行 + 交互式终端 |
| 传输 | file upload·download / sync push·pull / tunnel -L | 文件传输 + 目录同步 + 端口转发 |
| 部署 | deploy run·validate / update upload·push·rollback | 多步编排 + Agent 自更新 |
| 配置 | config get·set·backup·diff / find name·grep·stat·du | 配置管理 + 文件搜索 |
| 监控 | monitor top·cpu·disk·net·gpu / snapshot take·diff | 全维度资源监控 + 系统快照 |
| 管理 | agent list·revoke·enroll / service list·start·stop / proc list·kill / power shutdown·restart | 服务/进程/电源管理 |
| 守护 | watch add·list·remove·check / cron list·add·delete | 进程守护(自动重启) + 定时任务 |

### 做 / 不做

**做**:远程任务执行、穿透 NAT、文件传输、端口转发、部署编排、自更新、系统监控、服务/进程/电源管理、审计防篡改、RBAC 多租户、Secret 管理、Session 录屏、水平扩展(Redis+Pub/Sub)、SDK。

**不做(明确假设)**:
- 单 Relay 为默认(水平扩展需 Redis 共享状态,已预留 TaskStore interface)
- Agent 默认 Windows(Linux 预留,部分执行器跨平台)
- 命令策略 best-effort(黑名单可绕过,Agent 侧二次校验兜底)
- 文件传输/终端 best-effort(非 at-least-once,靠 SHA256/录屏兜底)

## 目标(Goal)

- **G1**:Agent 注册为 Windows 服务,开机自启、崩溃自恢复(OnFailure=restart),经反向 WS 上线到 Relay。
- **G2**:CLI/Web 下发任务 → Relay 路由 → Agent 执行 → 结果实时回流(SSE/轮询)。
- **G3**:9 类任务执行器 + 38 CLI 命令 + 40 API 端点,覆盖诊断/执行/传输/部署/配置/监控/管理/守护/安全。
- **G4**:水平扩展就绪(TaskStore interface + Redis 后端 + Pub/Sub 事件联邦 + Agent 多 Relay 故障切换)。
- **成功标准**:128 Go 文件 / 23K 行 / 183 测试 / 端到端全路径验证通过。

## 架构假设(4 项显式假设)

1. **单 Relay 默认**:多 Relay 需 Redis 共享状态(Store/Registry/EventBus 三层抽象化已就绪)。
2. **Agent 主动外连**:Relay 不主动连 Agent,Agent 反向 WS。端口转发隧道复用此连接。
3. **PowerShell 为 Windows 核心执行器**:-EncodedCommand(UTF16LE),命令策略黑名单 + Agent 侧二次校验。
4. **bbolt 单文件为默认持久化**:Redis 后端可选(TaskStore interface 切换)。

## 项目结构(v3.4)

```
go-rmm/
├── relay/              # 中继服务器(15 文件)
│   ├── server.go       # Relay 主体 + serveAgent + xferSession + EventBus
│   ├── router.go       # REST 路由(40 端点)
│   ├── store.go        # bbolt TaskStore + TaskStore interface + CAS/ReapToPending
│   ├── memstore.go     # 内存版 TaskStore(mock/测试/轻量部署)
│   ├── redis_store.go  # Redis 版 TaskStore(水平扩展)
│   ├── reaper.go       # 超时任务回收
│   ├── ca.go           # 内置 CA + CRL + enrollment
│   ├── policy.go       # 命令策略(黑名单)
│   ├── audit.go        # hash chain 审计 + 异步写入
│   ├── audit_siem.go   # SIEM 导出(CEF/Syslog/JSON)
│   ├── rbac.go         # RBAC 多租户(tenant:role:secret)
│   ├── power.go        # 电源操作端点(标签白名单)
│   ├── secrets.go      # Secret 管理(TTL+一次性)
│   ├── update_repo.go  # Agent 版本仓库(自更新)
│   ├── scheduler.go    # cron 定时任务
│   ├── scheduler_smart.go # 智能调度(负载均衡)
│   ├── scriptlib.go    # 脚本库(远端脚本存储)
│   ├── tunnel.go       # 隧道透传(端口转发)
│   ├── event_federation.go # Redis Pub/Sub 事件联邦
│   ├── logging.go      # slog 结构化日志
│   ├── metrics.go      # Prometheus /metrics + pprof
│   ├── tls_watcher.go  # TLS 证书过期检测
│   └── *.go            # config/web/main/registry
├── agent/              # Windows Agent(18 文件)
│   ├── connection.go   # 主循环 + recvLoop + WS 连接管理
│   ├── filetransfer.go # 大文件传输引擎(SHA256+续传)
│   ├── session.go      # 交互式 shell 会话(PTY)
│   ├── recording.go    # Session 录屏(asciinema v2)
│   ├── tunnel.go       # 隧道管理(端口转发)
│   ├── updater.go      # 自更新(rename+copy+重启)
│   ├── health.go       # 健康自检+自动回滚
│   ├── enroll.go       # mTLS 证书申请
│   ├── service.go      # kardianos 服务管理
│   ├── config.go       # 配置(Validate)
│   └── ...
├── cmd/go-rmm/         # CLI 客户端(18 文件)
│   ├── main.go         # 路由 + task/file/shell + printUsage
│   ├── commands.go     # status/cron/agent revoke/enroll/task batch
│   ├── deploy.go       # deploy playbook 引擎
│   ├── tunnel.go       # CLI 隧道(-L 端口转发)
│   ├── update.go       # CLI update(upload/list/push/rollback)
│   ├── ops.go          # sysinfo/service/proc/power/monitor(TUI)
│   ├── net.go          # 网络诊断(ping/dns/trace/ports/check)
│   ├── logs.go         # 日志聚合(tail/read/list/search)
│   ├── sync.go         # 目录同步(push/pull)
│   ├── find.go         # 文件查找(name/grep/stat/du)
│   ├── configmgmt.go   # 配置管理(get/set/backup/diff)
│   ├── snapshot.go     # 系统快照(take/list/diff)
│   ├── exec.go         # 批量并行执行(多 Agent + 结果汇总)
│   ├── watch.go        # 进程守护(自动重启)
│   ├── completion.go   # Shell 补全(bash/zsh/pwsh/fish)
│   ├── exit.go         # 退出码体系(0-5)
│   └── io.go           # HTTP client/slog/CLIXML 过滤
├── client/             # Go SDK(第三方可 import)
├── proto/              # 协议层(Envelope + 30 消息 + 二进制帧 + tunnel)
├── executors/          # 执行器(powershell/script/file/screenshot/browse/guiclick/power/monitor)
├── console/            # Web 控制台(反向代理 + 内嵌前端)
├── deploy/             # 部署配置(Helm/OpenAPI/systemd)
├── Dockerfile.relay    # Relay Docker 镜像
├── Dockerfile.agent    # Agent Docker 镜像
├── .goreleaser.yaml    # 跨平台发布
├── .golangci.yml       # 代码检查
├── .github/workflows/  # CI/CD
└── Makefile            # 世界级构建系统(15 targets)
```

## 指标(v3.4)

| 项目 | 数值 |
|------|------|
| Go 文件 | 128 |
| 代码行 | 23,121 |
| Go 包 | 9 |
| 测试用例 | 183 |
| CLI 命令 | 38 |
| API 端点 | 40 |
| 任务类型 | 9 |
| 基础设施产物 | 10+(Docker×2/Helm/OpenAPI/goreleaser/CI/systemd/golangci) |
| 神级提升 | 50+ |

## 进入子项目前先读

- `README.md` — 快速开始 + 能力矩阵 + 部署指南
- `DEPLOYMENT.md` — 生产部署操作指南
- `deploy/openapi.yaml` — API 契约(36 端点)
- `proto/message.go` — 协议层(所有消息类型)
- `relay/router.go` — REST API 路由(入口)
- `agent/connection.go` — Agent 主循环(入口)
- `cmd/go-rmm/main.go` — CLI 路由(入口)
