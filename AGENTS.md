# go-rmm · AGENTS.md

## 项目内容(What)

Go 实现的**企业级零信任远程访问平台**(v4.3.3)——通过反向 WebSocket 中继,让 CLI / Web 控制台远程驱动 Agent 执行全维度运维操作。对标 Teleport / Tailscale。

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
- 命令策略默认黑名单 best-effort(可绕过);可选 allow-only 白名单(Relay -policy-allow / Agent policy_allow,交集语义黑名单兜底)
- 文件传输/终端 best-effort(非 at-least-once,靠 SHA256/录屏兜底)

## 目标(Goal)

- **G1**:Agent 注册为 Windows 服务,开机自启、崩溃自恢复(OnFailure=restart),经反向 WS 上线到 Relay。
- **G2**:CLI/Web 下发任务 → Relay 路由 → Agent 执行 → 结果实时回流(SSE/轮询)。
- **G3**:9 类任务执行器 + 51 CLI 命令 + 42 API 端点,覆盖诊断/执行/传输/部署/配置/监控/管理/守护/安全。
- **G4**:水平扩展就绪(TaskStore interface + Redis 后端 + Pub/Sub 事件联邦 + Agent 多 Relay 故障切换)。
- **G5**(v4.3.3):批量任务分发(一对多/聚合/重试/进度跟踪)+ 多租户组织/角色策略/配额 + 审计 SIEM 导出。
- **成功标准**:169 Go 文件 / 31.8K 行 / 398 测试 + 22 基准 / 端到端全路径验证通过(rounds 1-10)。

## 架构假设(4 项显式假设)

1. **单 Relay 默认**:多 Relay 需 Redis 共享状态(Store/Registry/EventBus 三层抽象化已就绪)。
2. **Agent 主动外连**:Relay 不主动连 Agent,Agent 反向 WS。端口转发隧道复用此连接。
3. **PowerShell 为 Windows 核心执行器**:-EncodedCommand(UTF16LE),命令策略黑名单 + Agent 侧二次校验。
4. **bbolt 单文件为默认持久化**:Redis 后端可选(TaskStore interface 切换)。

## 项目结构(v4.3.3)

```
go-rmm/
├── relay/              # 中继服务器(59 文件)
│   ├── server.go       # Relay 主体 + serveAgent + xferSession + EventBus
│   ├── router.go       # REST 路由(42 端点)
│   ├── store.go        # bbolt TaskStore + TaskStore interface + CAS/ReapToPending
│   ├── memstore.go     # 内存版 TaskStore(mock/测试/轻量部署)
│   ├── redis_store.go  # Redis 版 TaskStore(水平扩展)
│   ├── batchdispatch.go # 批量任务分发(一对多/聚合/重试/进度跟踪)[round 8]
│   ├── tenant.go       # 多租户组织/角色策略/配额/隔离过滤 [round 7]
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

## 指标(v4.3.3)

| 项目 | 数值 |
|------|------|
| Go 文件 | 169 |
| 代码行 | 31,800+ |
| Go 包 | 7 核心包(relay/agent/cmd/client/proto/console/executors) |
| 测试用例 | 398 |
| 基准测试 | 22 |
| CLI 命令 | 51 |
| API 端点 | 45 |
| 任务类型 | 9 |
| 基础设施产物 | 10+(Docker×2/Helm/OpenAPI/goreleaser/CI/systemd/golangci) |
| 神级提升 | 50+ |

---

## 模块完成度(v4.3.3)

每个核心包的功能完成度与成熟度。✅ 生产就绪 / 🟡 骨架/接口先行 / ⏳ 预留未实现。

| 模块 | 完成度 | 关键能力 | 状态 |
|------|--------|----------|------|
| **relay** 路由层 | 95% | 42 REST 端点 + SSE 事件流 + 鉴权链 | ✅ |
| **relay** 可观测性 | 90% | /api/metrics(JSON)+ /metrics(Prom)+ xfer 传输指标(M1)+ pprof | ✅ |
| **relay** 任务投递 | 95% | WS 转发 + CAS 存储 + Reaper 超时回收 + 智能调度 | ✅ |
| **relay** 批量分发 | 95% | 一对多 + 聚合 + 指数退避重试 + 实时进度 + REST 异步端点(POST /api/batch + SSE 进度) [round 8+2026-08-18] | ✅ |
| **relay** 多租户 | 90% | 组织/角色策略/配额/隔离过滤 + bbolt 持久化(tenants bucket,重启恢复) + authorize 接入 OrgEnabled fail-closed [2026-08-18] | ✅ |
| **relay** 安全层 | 90% | mTLS + 内置 CA + CRL + 命令策略黑名单 + Secret 管理 | ✅ |
| **relay** 审计 | 95% | hash chain 防篡改 + 异步落盘 + SIEM 导出 [round 6] | ✅ |
| **relay** 水平扩展 | 70% | TaskStore interface + Redis + Pub/Sub 联邦;多 Relay 故障切换 | 🟡 生产验证待补 |
| **agent** 执行器 | 90% | 9 类任务(PS/script/file/screenshot/browse/guiclick/update/power/monitor) | ✅ |
| **agent** 连接管理 | 95% | 反向 WS + 断线重连 + 心跳 + 主从双 Agent + 死连接 watchdog(S1)+ 孤儿分片 sweep(S3) | ✅ |
| **agent** 文件传输 | 90% | 并发分片 + 断点续传 + path 互斥(R1)+ 已满快路径(R2)+ 流式低内存(R3)+ 能力协商(S4) | ✅ |
| **agent** Linux | 70% | script lang=sh + monitor /proc 全维采集 + CLI 运维命令族平台方言化(logs/sysinfo/service/proc/net,双方言一次直达)+ 三平台编译门禁(P1-4 四步);journalctl 深度日志/Linux 实机部署验证待补 | 🟡 |
| **cmd** CLI | 90% | 51 命令 + 流式 + TUI + 退出码体系 | ✅ |
| **client** SDK | 85% | 45+ 方法 + 流式 + 工具发现 | ✅ |
| **proto** 协议 | 95% | Envelope + 30 消息 + 二进制帧 + tunnel | ✅ |
| **console** Web | 80% | 反向代理 + 内嵌前端 + 实时事件 | 🟡 |
| **executors** | 90% | 8 执行器实现 + 跨平台 | ✅ |

**安全假设(显式)**:
- 单 Relay 为默认(水平扩展需 Redis 共享状态,接口已就绪)。
- Agent 默认 Windows(Linux 预留,部分执行器跨平台)。
- 命令策略默认黑名单 best-effort(可绕过);可选 allow-only 白名单(Relay -policy-allow / Agent policy_allow,交集语义黑名单兜底)。
- 文件传输/终端 best-effort(非 at-least-once,靠 SHA256/录屏兜底)。

---

## 测试覆盖(v4.3.3)

398 测试 + 22 基准。按包分布与重点验证场景:

| 包 | 测试数 | 文件数 | 重点场景 |
|----|--------|--------|----------|
| **relay** | 198 | 60 | 多客户端路由/断线重连/心跳[round2];6 类执行器[round3];console 转发[round4];mTLS 双向认证[round5];审计查询/导出[round6];多租户隔离[round7];批量分发/重试/进度[round8];SecretStore(TTL/一次性/并发防重放)/ScriptLibrary(路径穿越拒绝)/buffer_pool/审计错误路径/Prom 命名空间一致性[round9-10] |
| **agent** | 59 | 32 | 连接主循环/文件传输(续传+hash)/shell 会话/录屏/自更新/健康自检/可靠性(path互斥/已满快路径/sweep/watchdog);rQueue 可靠投递(FIFO/drain/requeue/flushTo 写失败保留)[round9] |
| **executors** | 40 | 24 | PowerShell(中文乱码修复)/脚本 BOM/截图/浏览器/GUI 点击/电源/监控 |
| **proto** | 44 | 9 | 消息编解码/边界/二进制帧/tunnel;Envelope(ID 唯一/错误路径/宽容解码)+Heartbeat omitempty 向后兼容+多消息类型往返[round9] |
| **client** | 18 | 9 | SDK 方法/流式/工具发现 |
| **console** | 14 | 4 | 反向代理/事件转发 |
| **cmd** | 12 | 28 | CLI 工具函数(flag 解析/大小解析/strip 剥离)[v4.3.3 新增测试] |

**深度测试(rounds 1-10)**:round 1 修复 `TestMultiAgentRoutingE2E` 永久失败;rounds 2-7 逐层加深(relay 路由 → 执行器 → console → mTLS → 审计 → 多租户);round 8 覆盖批量分发不变量(并发投递/聚合/退避重试/幂等/ctx 取消/进度状态机);rounds 9-10 工作区深度推进——补 SecretStore/ScriptLibrary/buffer_pool/rQueue/Envelope 等弱测模块,修复空闲 Agent 掉线循环(S7 heartbeat_ack)、脚本库路径穿越漏洞、Prom 指标前缀 typo、审计写盘静默丢记录。

**并发安全**:批量进度跟踪用 `sync.RWMutex` 保护,`markRunning/markSucceeded/markFailed` 与并发 `Snapshot()` 线程安全(race 检测需 cgo/gcc,本机未启用,靠互斥锁设计保证)。

**测试运行**:`go test ./... -count=1`(单 relay 包约 10s)。基准:`go test ./relay/ -bench=.`(22 项)。

---

## 下一步(Next Steps)

按优先级排序的演进方向:

### P0 — 生产硬化
1. **水平扩展验证**:Redis 多 Relay 实测(Store/Registry/EventBus 三层联邦),补 Agent 故障切换 E2E。

**已完成(rounds 9-10,工作区深度推进)**:
- ✅ **脚本库路径穿越修复**:`GET/DELETE/POST /api/scripts/{name}` 原 `filepath.Join` 清理 `..` 后逃逸脚本目录,可读删目录外任意文件。现已 handler + 库方法(Save/Read/Delete)双层 `validateScriptName` 校验(字符黑名单 + 解析后路径必须位于脚本目录内)。
- ✅ **空闲 Agent 掉线循环修复(S7)**:Relay 收到心跳后回 `heartbeat_ack` 帧,防空闲 Agent(无任务下发)被 watchdog(2×Heartbeat 无收包)周期误杀重连(隧道/任务随机拒绝)。
- ✅ **Prometheus 指标前缀统一**:`gormm_agents_online`(两 m)→`gormmm_agents_online`(三 m),与其他 `gormmm_*` 命名空间一致(防 PromQL 匹配遗漏)。
- ✅ **审计写盘错误加固**:`writeEntry` 原丢弃 `file.Write` 返回值,磁盘满/权限拒绝时审计记录静默丢失;现显式记日志(含 event 名),并补 `VerifyChain` 空文件/缺失/空行回归测试。

**已完成(2026-08-17,使用会话实证查漏补缺)**:
基于 7 个真实使用会话(passport-service 发布/voice-assistant 部署/local-demo-platform 更新/local-ai demo/realtime-digital-human 验证/tmp 服务/控制台运维,2026-08-05~08-17)的痛点证据,修复 4 个高频实战缺陷:
- ✅ **CLI 默认幂等键掺单调纳秒戳**(4 会话实证):原纯内容 hash 使"重跑同一条命令"命中 Agent 端幂等缓存(2h TTL)回放旧结果,运维被迫手工 `--idem "key-$(date +%s)"` 绕过。现 `defaultIdemKey`=内容 hash 指纹+严格递增纳秒戳(Windows time.Now 同毫秒同值以 CAS 循环兜底);同进程内 apiPostRetry 复用同一 key,网络重试安全语义不变。
- ✅ **Windows Job Object 杀进程树**(3 会话实证,passport-service 僵尸 uvicorn 占 8090 事故根因):超时原只杀直接子进程 powershell.exe,孙进程存活为僵尸占端口、健康检查打旧进程假通过。现 powershell/script 执行器经 `runManaged` 纳入 Job Object(KILL_ON_JOB_CLOSE + ctx 取消主动 TerminateJobObject),Linux 侧对负 pgid 发 SIGKILL;实测(Windows)ping 孙进程随超时整树终止。
- ✅ **`-t <数字>` 误用提示**:`-t 30` 原报"未知任务类型"无引导,现提示"-t 是任务类型,设置超时请用 -timeout <秒>"。
- ✅ **script 执行器补 `-NonInteractive` + usage 发现性强化**(5 会话全部手工绕路"写 ps1→BOM 规范化→upload→-File"):`-s <本地文件>` 本就自动 BOM+UTF-8 头+远端临时文件执行,但无人知晓;usage/flag 描述现明确"复杂命令请写本地 .ps1 后用 -s 执行,规避内联转义与编码问题"。
- ✅ **`-o json` 补齐 task run 终态与 task list**(2026-08-06 会话"-o json 无效"实证):jsonMode 自 v4.1.0 存在且覆盖 task result/agent list/net/ops/diagnose 等 7 处,但最高频的 `task run` 终态仍输出人类可读文本块、`task list` 仅表格。现 task run 终态/`--no-wait`/离线缓存三路径输出 `{task_id,status,result}` 纯 JSON(等待前缀与轮询点静默,stdout 无污染),task list 输出 `{tasks,count,enabled}`;httptest mock Relay 端到端断言 stdout 可直接 `json.Unmarshal`。
- ✅ **输出流纪律 + 版本一致性**(2026-08-18):`file download` 写文件路径的 `✅ 下载完成` 从 stderr 挪到 stdout(管道模式保留 stderr——stdout 是下载数据流),修复成功提示被 PS 5.1 `$ErrorActionPreference='Stop'` 误判为错误(passport-service 发布会话实证);printUsage 标头硬编码 `v3.9` 改为动态 `version` 变量(ldflags 注入,与 `version` 子命令一致);`checkRoots` 空白名单拒绝错误补配置指引(指向 agent `allowed_roots`/`allow_all_roots`,2026-08-17 会话"未配置允许根目录"无从下手实证)。
- ✅ **多租户持久化 + 鉴权接入**(2026-08-18,原 P0-1):`TenantManager` 注入 `*taskstore.Store`(与任务/cron 共用 tasks.db 的 `tenants` bucket),组织创建/启停/删除写盘、启动恢复,重启不丢组织与配额;`authorize` 链挂 `OrgEnabled` 校验——组织禁用/删除后该租户 token 立即失效(fail-closed)。**行为变更**:`tenant:role:secret` 语法的 token 现在要求组织已创建且启用(不存在=拒绝);纯 secret 部署(空 tenant)不受影响,现网向后兼容。`RolePolicy` 仍为内存态(角色策略为启动期配置,无运行时变更路径,暂无持久化需求)。
- ✅ **命令策略白名单**(2026-08-18,原 P1-6):`cmdpolicy.Policy` 增 `SetAllowlist`(正则,自动 `(?i)`)——非空时 allow-only **交集语义**(须命中白名单且不碰黑名单;白名单写宽如 `^Get-` 也有 NTUSER.DAT/凭据导出等黑名单兜底)。配置入口:Relay `-policy-allow` flag / `GO_RMM_POLICY_ALLOW` env(逗号分隔),Agent config `policy_allow` 数组(Dispatcher.SetAllowlist,Agent 强制底线即使 Relay 被绕过也生效);非法正则启动 fail-fast。EncodedCommand 拦截错误附 script 任务(`-s` 本地脚本文件)指引——使用会话实证用户手动 `-EncodedCommand` 规避转义被拦后无从知晓正解。默认不配置 = 纯黑名单,行为不变。
- ✅ **批量分发端点化**(2026-08-18,原 P0-1):round 8 的 BatchDispatcher 引擎(一对多/聚合/指数退避重试/实时进度)此前仅测试在用,现暴露为异步 REST:`POST /api/batch`(202 返回 batch_id,鉴权+命令策略校验+tag 匹配与 tasks/batch 同款)/`GET /api/batch/{id}`(进度快照:总/成/败/待/百分比/ETA/各目标状态)/`GET /api/batch/{id}/progress`(SSE 每秒推送,终态发 done 事件含成功率)。引擎拆出可注入 progress 的 `dispatchWithRetryTracked` 主体——goroutine 启动前查询端点即持有跟踪器;批次注册表 `sync.Map` + 完成后 1h TTL 懒清理。CLI 新增 `batch start/status/watch`(-o json 兼容)。同步版 `POST /api/tasks/batch` 保留(轻量场景)。
- ✅ **update 能力注册 + online_since 补齐**(2026-08-18 部署实战暴露):①`updateExecutor` 类型存在但注册代码在灾难重建时丢失——Agent caps 恒不含 update,Relay 能力校验拒收 `task run -t update`,自更新通道被自己的校验锁死(鸡生蛋)。现 slave(含未配置 role)注册,master 不注册(主永不更新);②`/api/agents` 漏填 `online_since`(Registry 私有 `registeredAt` 未输出,CLI "在线自"恒显"—"),补 `Registry.OnlineSince`。

### P1 — 能力补全
4. **Linux Agent**:补齐 Linux 执行器(systemd 服务/包管理/journalctl),打通跨平台。
5. **任务 at-least-once**:文件传输/终端结果可靠投递(当前 best-effort),减少 SHA256/录屏兜底依赖。

### P2 — 可观测/体验
7. **OpenAPI 同步**:`deploy/openapi.yaml` 已增量同步 2026-08-18 新端点(batch ×3 / updates apply binary_path / agents online_since+xfer_stats),存量端点补全至 45+ 与 typed client 生成仍待做。
8. **Web 控制台深化**:批量分发进度可视化、租户/组织管理 UI、审计回放。
9. ~~race 检测 CI~~:✅ 已完成——`ci.yml` 的 Test step 即 `go test -cover -race`(3563a3c 落地,本机与 CI 双验证零警告)。
10. ~~Agent Prometheus 指标端点~~:✅ 已完成——`127.0.0.1:7781/metrics`(仅本机回环,metrics_addr 可改/off 禁用),xfer 五计数器+sessions_active gauge+reconnects_total+uptime+build_info,gormmm_ 三 m 命名空间统一。
11. **文件传输 per-chunk SHA256 强制**:当前单片 `.ftpartNN` 无 hash 校验(合并后整文件校验兜底),加 per-chunk 强制可提前发现损坏,但需协议改动(滚动升级兼容性风险)。
12. **Relay 速率限制**:按 agentID 维度令牌桶(`golang.org/x/time/rate`),防并发上传吃满 Agent 上行带宽影响心跳。

---

## 进入子项目前先读

- `README.md` — 快速开始 + 架构图 + 能力矩阵 + 部署/配置/安全/多租户/批量分发
- `DEPLOYMENT.md` — 生产部署操作指南
- `deploy/openapi.yaml` — API 契约
- `proto/message.go` — 协议层(所有消息类型)
- `relay/router.go` — REST API 路由(入口)
- `relay/batchdispatch.go` — 批量分发系统(round 8)
- `relay/tenant.go` + `relay/rbac.go` — 多租户与 RBAC(round 7)
- `agent/connection.go` — Agent 主循环(入口)
- `cmd/go-rmm/main.go` — CLI 路由(入口)
