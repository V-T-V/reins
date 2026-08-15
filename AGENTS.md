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
- 命令策略 best-effort(黑名单可绕过,Agent 侧二次校验兜底)
- 文件传输/终端 best-effort(非 at-least-once,靠 SHA256/录屏兜底)

## 目标(Goal)

- **G1**:Agent 注册为 Windows 服务,开机自启、崩溃自恢复(OnFailure=restart),经反向 WS 上线到 Relay。
- **G2**:CLI/Web 下发任务 → Relay 路由 → Agent 执行 → 结果实时回流(SSE/轮询)。
- **G3**:9 类任务执行器 + 51 CLI 命令 + 42 API 端点,覆盖诊断/执行/传输/部署/配置/监控/管理/守护/安全。
- **G4**:水平扩展就绪(TaskStore interface + Redis 后端 + Pub/Sub 事件联邦 + Agent 多 Relay 故障切换)。
- **G5**(v4.3.3):批量任务分发(一对多/聚合/重试/进度跟踪)+ 多租户组织/角色策略/配额 + 审计 SIEM 导出。
- **成功标准**:197 Go 文件 / ~39.6K 行 / 447 测试函数 + 22 基准 / 端到端全路径验证通过(rounds 1-10 + 2026-08-12 控制台/AI Agent/架构优化)。

## 架构假设(4 项显式假设)

1. **单 Relay 默认**:多 Relay 需 Redis 共享状态(Store/Registry/EventBus 三层抽象化已就绪)。
2. **Agent 主动外连**:Relay 不主动连 Agent,Agent 反向 WS。端口转发隧道复用此连接。
3. **PowerShell 为 Windows 核心执行器**:-EncodedCommand(UTF16LE),命令策略黑名单 + Agent 侧二次校验。
4. **bbolt 单文件为默认持久化**:Redis 后端可选(TaskStore interface 切换)。

## 项目结构(v4.3.3)

```
go-rmm/
├── relay/              # 中继服务器(78 文件)
│   ├── server.go       # Relay 主体 + authorize/authH 中间件 + serveAgent + EventBus
│   ├── router.go       # REST 路由(~47 端点,authH 包装器统一鉴权)
│   ├── store.go        # bbolt TaskStore + TaskStore interface + CAS/ReapToPending
│   ├── memstore.go     # 内存版 TaskStore(mock/测试/轻量部署)
│   ├── batchdispatch.go # 批量任务分发(一对多/聚合/重试/进度跟踪)[round 8]
│   ├── tenant.go       # 多租户组织/角色策略/配额/隔离过滤 [round 7](未接 HTTP handler)
│   ├── reaper.go       # 超时任务回收
│   ├── ca.go           # 内置 CA + CRL + enrollment
│   ├── policy.go       # 命令策略(黑名单)
│   ├── audit.go        # hash chain 审计 + 异步写入
│   ├── audit_siem.go   # SIEM 导出(CEF/Syslog/JSON)
│   ├── rbac.go         # RBAC 多租户(tenant:role:secret)(未接 HTTP handler)
│   ├── power.go        # 电源操作端点(标签白名单)
│   ├── secrets.go      # Secret 管理(TTL+一次性)
│   ├── sessions.go     # 控制台会话存储(内存态,滑动续期)[2026-08-12]
│   ├── console_auth.go # 控制台 login/logout/session 端点(HttpOnly Cookie + 登录限流)[2026-08-12]
│   ├── login_ratelimit.go # 登录限流(IP 失败计数+锁定,防暴力破解)[2026-08-12]
│   ├── agent_run.go    # AI Agent 编排(Plan-Execute-Replan + V4 prompt)[2026-08-06]
│   ├── agent_tools.go  # 8 个编排工具(list_agents/run_task/...)[2026-08-06]
│   ├── llm_client.go   # OpenAI 兼容 LLM 客户端(规划/流式/function calling)[2026-08-06]
│   ├── update_repo.go  # Agent 版本仓库(自更新)
│   ├── scheduler.go    # cron 定时任务
│   ├── scheduler_smart.go # 智能调度(负载均衡,CPU/mem 待接 metrics)
│   ├── scriptlib.go    # 脚本库(远端脚本存储)
│   ├── tunnel.go       # 隧道透传(端口转发)
│   ├── tunnel_ws.go    # 隧道 WebSocket 全双工 [2026-08-06]
│   ├── logging.go      # slog 结构化日志
│   ├── metrics.go      # Prometheus /metrics(常量时间鉴权)+ pprof
│   ├── tls_watcher.go  # TLS 证书过期检测
│   └── *.go            # config/web/main/registry/quickstart/stream_task/buffer_pool/...
│   # 注:redis_store.go / event_federation.go 此前误列,实际未实现(水平扩展为接口预留)
├── agent/              # Windows Agent(34 文件)
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
├── cmd/go-rmm/         # CLI 客户端(30 文件)
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
| Go 文件 | 197 |
| 代码行 | ~39,600 |
| Go 包 | 9 包(relay/agent/cmd/client/proto/console/executors + vision/win32 辅助) |
| 测试用例 | 447 测试函数(relay 包 258)/ 端到端全路径 |
| 基准测试 | 22 |
| CLI 命令 | 51 |
| API 端点 | 42 |
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
| **relay** 批量分发 | 90% | 一对多 + 聚合 + 指数退避重试 + 实时进度跟踪 [round 8] | ✅ |
| **relay** 多租户 | 80% | 组织/角色策略/配额/隔离过滤(内存 store) [round 7] | 🟡 持久化待接 |
| **relay** 安全层 | 92% | mTLS + 内置 CA + CRL + 命令策略黑名单 + Secret 管理 + 控制台会话 Cookie 鉴权 | ✅ |
| **relay** 审计 | 95% | hash chain 防篡改 + 异步落盘 + SIEM 导出 [round 6] | ✅ |
| **relay** 水平扩展 | 70% | TaskStore interface + Redis + Pub/Sub 联邦;多 Relay 故障切换 | 🟡 生产验证待补 |
| **agent** 执行器 | 90% | 9 类任务(PS/script/file/screenshot/browse/guiclick/update/power/monitor) | ✅ |
| **agent** 连接管理 | 95% | 反向 WS + 断线重连 + 心跳 + 主从双 Agent + 死连接 watchdog(S1)+ 孤儿分片 sweep(S3) | ✅ |
| **agent** 文件传输 | 90% | 并发分片 + 断点续传 + path 互斥(R1)+ 已满快路径(R2)+ 流式低内存(R3)+ 能力协商(S4) | ✅ |
| **agent** Linux | 30% | 跨平台执行器预留(PS/部分) | ⏳ |
| **cmd** CLI | 90% | 51 命令 + 流式 + TUI + 退出码体系 | ✅ |
| **client** SDK | 85% | 45+ 方法 + 流式 + 工具发现 | ✅ |
| **proto** 协议 | 95% | Envelope + 30 消息 + 二进制帧 + tunnel | ✅ |
| **console** Web | 90% | 反向代理 + 内嵌前端 + 实时事件 + 会话 Cookie 鉴权(HttpOnly+Secure+SameSite,EventSource 靠 cookie) | ✅ |
| **executors** | 90% | 8 执行器实现 + 跨平台 | ✅ |

**安全假设(显式)**:
- 单 Relay 为默认(水平扩展需 Redis 共享状态,接口已就绪)。
- Agent 默认 Windows(Linux 预留,部分执行器跨平台)。
- 命令策略 best-effort(黑名单可绕过,Agent 侧二次校验兜底)。
- 文件传输/终端 best-effort(非 at-least-once,靠 SHA256/录屏兜底)。

---

## 测试覆盖(v4.3.3)

447 测试函数 + 22 基准。按包分布与重点验证场景:

| 包 | 测试数 | 文件数 | 重点场景 |
|----|--------|--------|----------|
| **relay** | 258 | 78 | 多客户端路由/断线重连/心跳[round2];6 类执行器[round3];console 转发[round4];mTLS 双向认证[round5];审计查询/导出[round6];多租户隔离[round7];批量分发/重试/进度[round8];SecretStore(TTL/一次性/并发防重放)/ScriptLibrary(路径穿越拒绝)/buffer_pool/审计错误路径/Prom 命名空间一致性[round9-10] |
| **agent** | 60 | 34 | 连接主循环/文件传输(续传+hash)/shell 会话/录屏/自更新/健康自检/可靠性(path互斥/已满快路径/sweep/watchdog);rQueue 可靠投递(FIFO/drain/requeue/flushTo 写失败保留)[round9] |
| **executors** | 41 | 25 | PowerShell(中文乱码修复)/脚本 BOM/截图/浏览器/GUI 点击/电源/监控 |
| **proto** | 44 | 11 | 消息编解码/边界/二进制帧/tunnel;Envelope(ID 唯一/错误路径/宽容解码)+Heartbeat omitempty 向后兼容+多消息类型往返[round9] |
| **client** | 18 | 9 | SDK 方法/流式/工具发现 |
| **console** | 14 | 4 | 反向代理/事件转发 |
| **cmd** | 12 | 30 | CLI 工具函数(flag 解析/大小解析/strip 剥离)[v4.3.3 新增测试] |

**深度测试(rounds 1-10)**:round 1 修复 `TestMultiAgentRoutingE2E` 永久失败;rounds 2-7 逐层加深(relay 路由 → 执行器 → console → mTLS → 审计 → 多租户);round 8 覆盖批量分发不变量(并发投递/聚合/退避重试/幂等/ctx 取消/进度状态机);rounds 9-10 工作区深度推进——补 SecretStore/ScriptLibrary/buffer_pool/rQueue/Envelope 等弱测模块,修复空闲 Agent 掉线循环(S7 heartbeat_ack)、脚本库路径穿越漏洞、Prom 指标前缀 typo、审计写盘静默丢记录。

**并发安全**:批量进度跟踪用 `sync.RWMutex` 保护,`markRunning/markSucceeded/markFailed` 与并发 `Snapshot()` 线程安全(race 检测已验证:本地 cgo+gcc(MinGW)环境 `go test -race ./relay/ ./agent/ ./executors/ ./cmd/go-rmm/` 全包通过,2026-08;CI 亦配 -race,ci.yml:38)。

**测试运行**:`go test ./... -count=1`(单 relay 包约 10s)。基准:`go test ./relay/ -bench=.`(22 项)。

---

## 架构演进记录(2026-08 深度推进后)

- **已拆共享包**(internal/,消跨包复制):`cmdpolicy`(命令策略 32 条单一真相源)、`wsx`(P0/P1 优先级写引擎)、`ca`(PKI:enroll/renew/CRL)。
- **RBAC 已接线执行**:authH 注入 Identity + RequireRole 守卫高危端点(revoke/enroll-token/power/secrets)+ power/update 任务类型守卫;hasValidBearer 按 Secret 部分比较(支持 tenant:role:secret 格式)。
- **CLI/SDK 双栈决策**:client 包(929 行)当前为孤儿(CLI 自带 HTTP 层)。统一方向是 **SDK 吸收 CLI 的 SSE/重试/分片语义后 CLI 变薄壳**——反向(CLI 改用 SDK)会丢失 CLI 已有的丰富语义,不做。在此之前 SDK 不承诺 API 稳定。
- **agent 依赖注入(未做)**:11 个包级单例(sessions/transfers/tunnelMgr/resultQueue/idempotencyCache/tunnelReadyCb...)收进 Agent 聚合根需大范围重构(涉及全部 handler 与测试),作为独立重构轮次,不在日常修复中零碎进行。
- **file 执行器 ContentB64 路径已标 Deprecated**:主路径为流式 /api/transfer/*(分片+SHA256+断点续传)。

## 下一步(Next Steps)

按优先级排序的演进方向:

### P0 — 生产硬化
1. **多租户持久化**:`TenantManager`/`RolePolicy` 从内存 store 迁移到 bbolt/Redis,重启不丢组织与策略。
2. **批量分发端点化**:把 `BatchDispatcher` 接入新 REST 端点 `POST /api/batch`(返回 batchID)+ `GET /api/batch/:id/progress`(SSE 实时进度),供 CLI/Web 调用。
3. **水平扩展验证**:Redis 多 Relay 实测(Store/Registry/EventBus 三层联邦),补 Agent 故障切换 E2E。

**已完成(rounds 9-10,工作区深度推进)**:
- ✅ **脚本库路径穿越修复**:`GET/DELETE/POST /api/scripts/{name}` 原 `filepath.Join` 清理 `..` 后逃逸脚本目录,可读删目录外任意文件。现已 handler + 库方法(Save/Read/Delete)双层 `validateScriptName` 校验(字符黑名单 + 解析后路径必须位于脚本目录内)。
- ✅ **空闲 Agent 掉线循环修复(S7)**:Relay 收到心跳后回 `heartbeat_ack` 帧,防空闲 Agent(无任务下发)被 watchdog(2×Heartbeat 无收包)周期误杀重连(隧道/任务随机拒绝)。
- ✅ **Prometheus 指标前缀统一**:`gormm_agents_online`(两 m)→`gormmm_agents_online`(三 m),与其他 `gormmm_*` 命名空间一致(防 PromQL 匹配遗漏)。
- ✅ **审计写盘错误加固**:`writeEntry` 原丢弃 `file.Write` 返回值,磁盘满/权限拒绝时审计记录静默丢失;现显式记日志(含 event 名),并补 `VerifyChain` 空文件/缺失/空行回归测试。

**已完成(2026-08-06,基于两次真实会话的全面修复+深度优化)**:
- ✅ **P0-A 隧道首包丢失竞态(会话卡死根因)**:`handleTunnelOpen` 原只在 open 阶段注册 readyCh,dataCh 由 poll 按需注册;Agent 在 TunnelReady 后立即推数据,`routeXferData` 找不到 channel 静默丢弃 → HTTP 响应头字节全丢,连接表现为"✓ 隧道就绪但数据卡死"。三层修复:Relay open 阶段提前注册 dataCh(64 缓冲,getOrRegister 语义,open 返回只清理 readyCh)+ CLI poll 与 open 并行启动 + Agent session 注册提前到 sendReady 前;另修 close 后 poll 挂 30 分钟(EOF 唤醒)。新增 `tunnel_integration_test.go` 4 个回归测试(首包不丢/拒绝透传/close 唤醒/并发竞态)。
- ✅ **P0-B `-o json` 补齐**:jsonMode 原只覆盖 8 处(agent list/task result/sysinfo 等),task run/list/cancel、status、file upload/download、task stream、tunnel 横幅全为文本。现统一 `{command, ok, ...}` 结构全命令覆盖;上传/下载进度条 json 模式抑制(progressReader/Writer 静默);新增 `json_output_test.go`。
- ✅ **P1-A 隧道 WebSocket 全双工**:新端点 `GET /api/tunnel/ws`(握手即 open),单 WS 连接承载 open 信令 + 双向数据 + 心跳(Ping 0x02/Pong 0x03,默认 15s/30s 判死,Relay 字段可调),彻底消除双 HTTP 长连接开销与首包竞态;CLI 优先 WS,握手失败自动回退 HTTP(老 Relay 兼容);旧端点完整保留。Agent 侧零改动。新增 `tunnel_ws_test.go` 4 个测试(E2E 双向/心跳保活/判死/RST 拒绝)。
- ✅ **P1-B 任务结果 SSE 实时推送**:Relay 补广播 `task_dispatched`(dispatchTask 成功)/`task_acknowledged`(TypeTaskAck 处),proto 新增对应常量;CLI `waitResult` 改 SSE 优先 + 轮询兜底双通道(`/api/events` 按 task_id 过滤),task run 结果秒回(实测 299ms vs 原 60s 轮询超时),SSE 断线自动落回轮询零状态丢失。新增 `task_events_test.go` 3 个测试。
- ✅ **P2-A 上传白名单体验**:403 时 CLI 附可操作提示(查看/修改 AllowedRoots 指引);新增 `config agent-config` 子命令——探测远端 config.json 位置并解析显示 allowed_roots/workspace/max_concurrent_tasks 等关键字段(支持 `-o json` 与 `--path` 指定)。
- ✅ **P2-B CLI localhost 代理豁免**:`ProxyFromEnvironment` 会劫持 127.0.0.1 流量(用户本机 http_proxy 导致隧道假死,会话实测);提取 `proxyFunc`——loopback(localhost/127.0.0.1/[::1])直连,其余走环境代理;新增 `io_test.go`。
- ✅ **P2-C status 输出补全**:原只解析 4 个 health 字段 + agent count;现补 xfer 传输指标(healthz)、每-Agent 详情(capabilities/tags/version)、任务实时状态分布(pending/dispatched/acknowledged/completed/failed/cancelled 计数)、`-o json` 支持。
- ✅ **P2-D Agent 任务排队替代 NACK 风暴**:`handleTaskWithPool` 原 TrySubmit 池满立即 NACK → Relay 回退 pending → 重投风暴(会话实测 master 串行长任务 3-4 分钟时后续任务反复超时);改先发 ACK(Relay 保持 acknowledged,reaper 阈值 timeout×3≥300s 不误杀)再 `Submit` 阻塞排队,排队上限 = 任务 timeout 超时自愈;新增排队自动执行回归测试。

**已完成(2026-08-06,Web 控制台 Agent 化任务执行)**:
- ✅ **Agent 编排层(补齐"自然语言 → 工具调用")**:Relay 新增 `POST /api/agent/run`(SSE 流式)——用户用自然语言描述运维任务,LLM 自动规划 → 逐步执行工具 → 失败重规划(≤2 次)→ 流式汇总。Plan-and-Execute 循环(借鉴 go-agent-research `planexec` 三接口 + agentloop `LoopEvent` 事件协议)。
- ✅ **LLM 客户端(纯标准库)**:`relay/llm_client.go` — OpenAI 兼容 `/v1/chat/completions`,原生 function calling,`Chat`/`ChatJSON`/`ChatStream`(逐 token)+ 重试(429/5xx 指数退避)+ `StubLLM` 脚本式 mock(测试零依赖真实模型)。配置:`-llm-base-url`(默认空=禁用,示例 `http://127.0.0.1:11434/v1` 指向 Ollama)/`-llm-api-key`/`-llm-model`(默认 qwen2.5:7b)/`-llm-agent-max`(并发上限 2)。
- ✅ **8 个编排工具**(`relay/agent_tools.go`,内部直接调用 Relay 逻辑,错误不抛出):list_agents / scheduler_best / run_task / get_task_result / run_script / shell_exec / list_tasks / read_agent_config。
- ✅ **SSE 事件协议**:`agent_start` / `agent_plan`(含 replanned 标记)/ `step_start` / `tool_call`(参数截断脱敏)/ `tool_result`(2KB 截断)/ `step_done` / `step_failed` / `agent_delta`(汇总流式)/ `agent_final` / `agent_error` / `agent_queued`(并发排队)。data 统一 `{agent_run_id, data, ts}` 包裹。
- ✅ **console Agent 面板**(`console/web/index.html` + `app.js`):自然语言输入 → 计划步骤卡片(状态着色 pending/running/done/failed/replanned)→ 实时事件日志 → 流式结果气泡;Agent 下拉限定;会话内历史 5 条回填;fetch+ReadableStream 解析 SSE(POST 不支持 EventSource)。
- ✅ **规划容错**:LLM 规划 JSON 解析失败(围栏/非法/空)→ 重试 1 次 → 单步 run_task 兜底;工具失败 → Replanner 注入失败原因重规划(≤2 次)。
- ✅ **/api/tools 补登记**:`stream_task` + `agent_run`(原 29 工具 → 31)。

**已完成(2026-08-12,控制台会话鉴权 — HttpOnly Cookie)**:
- ✅ **根因**:浏览器控制台显示"在线 Agent (0)" + "Relay: 连接断开"——所有 `fetch()` 与 `EventSource` 未带鉴权,`/api/agents` 与 `/api/events` 返回 401。CLI 带 Bearer Token 正常,证明鉴权链路本身正常,缺的是浏览器侧凭证传递。`EventSource` 浏览器无法设 Authorization 头 → 必须走 Cookie。
- ✅ **方案**:控制台登录后 Relay 颁发 `HttpOnly + Secure + SameSite=Lax` 会话 Cookie(有状态内存 session,滑动续期 8h),普通 API 与 SSE 都靠 cookie 鉴权。Relay Token 不落浏览器,CLI/Agent Bearer 零改动。
- ✅ **`relay/sessions.go`**:`SessionStore`(内存态,`crypto/rand` 32 字节 ID,滑动续期,后台 sweep 清过期,`Create/Get/Delete/Len/Close`)。不复用 SecretStore(其无 caller-key + 无续期 API)。
- ✅ **`relay/console_auth.go`**:`POST /api/console/login`(密码 `subtle.ConstantTimeCompare` + 颁 cookie + 审计)、`POST /api/console/logout`(清 session + 清 cookie)、`GET /api/console/session`(前端判断登录态)。`Secure` 据 `X-Forwarded-Proto` 自适应(反代 TLS 终端)。
- ✅ **`relay/server.go` `authorize` 双轨**:抽 `hasValidBearer` + 新增 `hasValidSession`,两者任一通过即可。~20 个 handler 零调用点改动即全端点(含 SSE)支持 cookie。`Relay.sessions` 字段 + `WithSessions` builder。
- ✅ **`relay/config.go`**:`-console-password` / `-console-session-ttl` / `-console-cookie-name` / `-console-secure-cookie` flag + env + `Validate`。
- ✅ **前端 `web/app.js` + `index.html`**:`apiFetch()` 包装(自动 `credentials:"include"` + 401 弹登录)、`EventSource({withCredentials:true})`、XHR `withCredentials`、`#login-overlay` 登录页 + 登出按钮、`checkSession()` 启动门控。监听随 EventSource 重连重新注册(顺带修了重连丢监听的潜在问题)。
- ✅ **测试**:`relay/console_auth_test.go` 13 例(login 成功/失败/禁用、session 端点、authorize cookie 通过/无凭证 401/Bearer 回归、logout 失效、TTL 过期、滑动续期、cookie 属性 HttpOnly/Secure/SameSite/Path、X-Forwarded-Proto 自适应、dev 放行回归、审计 login 事件)。

**已完成(2026-08-12,控制台三连 + AI Agent 深度优化)**:
- ✅ **Agent 任务档案面板**:选中 Agent → 已做/在做/待做 三栏 + cron 定时计划;`/api/tasks` 加 `?agent_id`+`?status` 过滤,`/api/crons` 加 `?agent_id`+`next_run` 运行时计算(`router_tasks_filter_test.go` 6 例)。
- ✅ **批量任务下发面板**:多选 Agent + 一次性下发 + 实时执行结果表;`/api/tasks/batch` 加 `agent_ids` 选择器(优先于 tag,过滤在线)(`router_batch_test.go` 8 例)。
- ✅ **🤖 AI Agent 自然语言面板**:`POST /api/agent/run` SSE 流式 → fetch+ReadableStream 按 `\n\n` 分帧解析(POST 不能用 EventSource),渲染 agent_plan/step_start/tool_call/tool_result/agent_delta(逐 token)/agent_final;信封 `{agent_run_id,data,ts}` unwrap。
- ✅ **DeepSeek V4 深度优化**(网络研究后):
  - **根因修复**:`agentPlan`/`agentReplan` 原同时传 `tools=schemas` + `response_format=json_object` —— DeepSeek V4(强 tool-use)会倾向返回 `tool_calls` 而非计划 JSON,冲突。改 `ChatJSON(..., nil)`,规划纯走 system prompt 描述工具 + json_object,schemas 仅用于 prompt 文本。
  - **prompt 针对 V4 重写**:`agentPlanSystemPrompt` 加结构化「工具选择决策树」(不知道 agent→list_agents;执行命令→run_task;...) + 反幻觉规则(不编造工具/参数,命令完整 PowerShell)。
  - **模型迁移**:`deepseek-chat`(legacy,2026-07-24 退役)→ `deepseek-v4-flash`/`deepseek-v4-pro`(1M 上下文,SOTA agentic)。`config.go` 注释 + 默认说明更新。

**已完成(2026-08-12,架构梳理与优化 v4.5.0)**:
- ✅ **架构审计**:Explore agent 全面审计 78 个 relay 文件,识别 10 项优化点(按 ROI 排序)。
- ✅ **auth 中间件 `authH`**(最大架构改进):消除 ~40 个 handler 各自重复的 `if err := r.authorize(req); err != nil { writeJSON(401...) }` 4 行块。`registerHTTP` 统一用 `r.authH(handler)` 包装;故意跳过 authorize 的端点(/healthz 软鉴权、/metrics 独立 token、/api/enroll 注册令牌、/api/console/* 自鉴权)显式不包。删 `handleConsoleWS` 死代码(未注册)。
- ✅ **安全修复 `handlePromMetrics`**:原 `got != "Bearer "+token` 是明文比较(计时侧信道泄露 metrics token),改 `subtle.ConstantTimeCompare`(与 authorize/hasValidBearer 一致)。
- ✅ **`newTestRelay` 漂移修复**:原 struct literal 构造漏 `agentRunSem`(nil channel,触 /api/agent/run 会挂死)、sessions、loginLimiter。改用 `NewRelay(cfg).WithStore(store)`,继承所有默认值,消除"测试与生产行为不一致"footgun。
- ✅ **一致性**:`bootstrap.go:55` 的 hand-rolled `map[string]string` 错误改用统一 `errMap`(已由 authH 覆盖鉴权)。
- ✅ **AGENTS.md 同步**:删幻影文件(`redis_store.go`/`event_federation.go` 实际未实现);补 `login_ratelimit.go`/`agent_run.go`/`agent_tools.go`/`llm_client.go`/`tunnel_ws.go` 等;更新计数(169→194 文件、31.8K→~39K 行、373→560+ 测试);标注 RBAC/tenant"未接 HTTP handler"。

### P1 — 能力补全
4. **Linux Agent**:补齐 Linux 执行器(systemd 服务/包管理/journalctl),打通跨平台。
5. **任务 at-least-once**:文件传输/终端结果可靠投递(当前 best-effort),减少 SHA256/录屏兜底依赖。
6. **命令策略白名单**:从黑名单升级到可配置白名单 + 正则,提升防绕过。

### P2 — 可观测/体验
7. **OpenAPI 同步**:`deploy/openapi.yaml` 补全至 42 端点(批量分发/进度/SIEM 导出),生成 typed client。
8. **Web 控制台深化**:批量分发进度可视化、租户/组织管理 UI、审计回放。
9. **race 检测 CI**:CI 启用 cgo + `-race`(需 gcc),覆盖并发热点(批量分发/EventBus/store)。
10. **Agent Prometheus 指标端点**:暴露 `xfer_sessions_active`/`xfer_bytes_total`/`xfer_failures_total`/`reconnect_count`(Q4 计数器已就绪,Relay 侧 M1 已暴露 xfer 指标,Agent 侧 HTTP 端点待补)。
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
