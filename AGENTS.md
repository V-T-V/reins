# go-rmm · AGENTS.md

## 项目内容(What)

Go 实现的**企业级零信任远程访问平台**(v4.6.0)——通过反向 WebSocket 中继,让 CLI / Web 控制台远程驱动 Agent 执行全维度运维操作。对标 Teleport / Tailscale。

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
| 传输 | file upload·download / sync push·pull / tunnel -L | 文件传输(内网直连优先+中继回落) + 目录同步 + 端口转发 |
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
- 传输 Complete 终态帧 at-least-once(resultQueue 补发);progress/终端输出 best-effort(录屏兜底)

## 目标(Goal)

- **G1**:Agent 注册为 Windows 服务,开机自启、崩溃自恢复(OnFailure=restart),经反向 WS 上线到 Relay。
- **G2**:CLI/Web 下发任务 → Relay 路由 → Agent 执行 → 结果实时回流(SSE/轮询)。
- **G3**:9 类任务执行器 + 51 CLI 命令 + 42 API 端点,覆盖诊断/执行/传输/部署/配置/监控/管理/守护/安全。
- **G4**:水平扩展就绪(TaskStore interface + Redis 后端 + Pub/Sub 事件联邦 + Agent 多 Relay 故障切换)。
- **G5**(v4.3.3):批量任务分发(一对多/聚合/重试/进度跟踪)+ 多租户组织/角色策略/配额 + 审计 SIEM 导出。
- **G6**(v4.5.0→v4.6.0):批量任务编排 DAG——`internal/dagflow` 调度核心(依赖建模+并发上限+失败级联跳过+ContinueOnError 软依赖,纯逻辑零依赖、确定性可测);v4.6.0 接入 relay 编排端点(POST /api/dag 提交/GET 列表与逐节点状态快照,策略逐节点校验+审计+TTL 注册表)与 CLI(`go-rmm dag apply/list/status`),节点执行体复用既有任务投递链路(store 持久化+WS dispatch+终态轮询)。
- **成功标准**:231 Go 文件 / 42K 行 / 496 测试 + 22 基准 / 端到端全路径验证通过(rounds 1-10 + 2026-09-03 rules 门禁冒烟 + 2026-09-14 webhook/tls_watcher/智能调度负载收口)。

## 架构假设(4 项显式假设)

1. **单 Relay 默认**:多 Relay 需 Redis 共享状态(Store/Registry/EventBus 三层抽象化已就绪)。
2. **Agent 主动外连**:Relay 不主动连 Agent,Agent 反向 WS。端口转发隧道复用此连接。
3. **PowerShell 为 Windows 核心执行器**:-EncodedCommand(UTF16LE),命令策略黑名单 + Agent 侧二次校验。
4. **bbolt 单文件为默认持久化**:Redis 后端可选(TaskStore interface 切换)。

## 项目结构(v4.6.0)

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
│   ├── deploy_init.go  # deploy init 模板生成器(basic/zip-node 会话验证模式) [v4.4.0]
│   ├── rules.go        # rules read 命令 + rules_ack 解析/缓存/本地 .go-rmm 加载 [v4.4.0]
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
│   ├── dag.go          # dag apply/list/status CLI(DAG 编排提交与跟踪) [v4.6.0]
│   └── io.go           # HTTP client/slog/CLIXML 过滤
├── client/             # Go SDK(第三方可 import)
├── proto/              # 协议层(Envelope + 30 消息 + 二进制帧 + tunnel)
├── executors/          # 执行器(powershell/script/file/screenshot/browse/guiclick/power/monitor)
│   └── rulesgate.go    # 项目规则门禁:.go-rmm/rules.md 发现+指纹+Dispatch 校验 [v4.4.0]
├── internal/dagflow/   # DAG 编排调度核心(依赖图+并发上限+失败级联,纯逻辑零依赖) [v4.5.0]
├── console/            # Web 控制台(反向代理 + 内嵌前端)
├── deploy/             # 部署配置(Helm/OpenAPI/systemd)
├── Dockerfile.relay    # Relay Docker 镜像
├── Dockerfile.agent    # Agent Docker 镜像
├── .goreleaser.yaml    # 跨平台发布
├── .golangci.yml       # 代码检查
├── .github/workflows/  # CI/CD
└── Makefile            # 世界级构建系统(15 targets)
```

## 指标(v4.6.0)

| 项目 | 数值 |
|------|------|
| Go 文件 | 231 |
| 代码行 | 42,000+ |
| Go 包 | 7 核心包(relay/agent/cmd/client/proto/console/executors) |
| 测试用例 | 535 |
| 基准测试 | 22 |
| CLI 命令 | 52 |
| API 端点 | 45 |
| 任务类型 | 9 |
| 基础设施产物 | 10+(Docker×2/Helm/OpenAPI/goreleaser/CI/systemd/golangci) |
| 神级提升 | 50+ |

---

## 模块完成度(v4.6.0)

每个核心包的功能完成度与成熟度。✅ 生产就绪 / 🟡 骨架/接口先行 / ⏳ 预留未实现。

| 模块 | 完成度 | 关键能力 | 状态 |
|------|--------|----------|------|
| **relay** 路由层 | 95% | 42 REST 端点 + SSE 事件流 + 鉴权链 | ✅ |
| **relay** 可观测性 | 90% | /api/metrics(JSON)+ /metrics(Prom)+ xfer 传输指标(M1)+ pprof + webhook 告警 5 事件全接线[2026-09-14] | ✅ |
| **relay** 任务投递 | 95% | WS 转发 + CAS 存储 + Reaper 超时回收 + 智能调度(monitor 采样负载回流评分,TTL 过期回 0)[2026-09-14] | ✅ |
| **relay** 批量分发 | 95% | 一对多 + 聚合 + 指数退避重试 + 实时进度 + REST 异步端点(POST /api/batch + SSE 进度) [round 8+2026-08-18] | ✅ |
| **relay** DAG 编排 | 95% | POST /api/dag 提交(Validate+逐节点策略校验)+ GET 列表/逐节点状态快照;节点=agent_id/tag 扇出投递+终态轮询;失败级联跳过对下游零投递 [v4.6.0] | ✅ |
| **relay** 多租户 | 90% | 组织/角色策略/配额/隔离过滤 + bbolt 持久化(tenants bucket,重启恢复) + authorize 接入 OrgEnabled fail-closed [2026-08-18] | ✅ |
| **relay** 安全层 | 90% | mTLS + 内置 CA + CRL + 命令策略黑名单(离线缓存路径同拦,P1-7[2026-09-14]) + Secret 管理 | ✅ |
| **relay** 审计 | 95% | hash chain 防篡改 + 异步落盘 + SIEM 导出 [round 6] | ✅ |
| **relay** 水平扩展 | 70% | TaskStore interface + Redis + Pub/Sub 联邦;多 Relay 故障切换 | 🟡 生产验证待补 |
| **agent** 执行器 | 90% | 9 类任务(PS/script/file/screenshot/browse/guiclick/update/power/monitor) | ✅ |
| **agent** 连接管理 | 95% | 反向 WS + 断线重连 + 心跳 + 主从双 Agent + 死连接 watchdog(S1)+ 孤儿分片 sweep(S3)+ **自动更新轮询**(auto_update/download_url,复用健康回滚安全网)[2026-09-17] | ✅ |
| **agent** 文件传输 | 95% | 并发分片 + 断点续传 + path 互斥(R1)+ 已满快路径(R2)+ 流式低内存(R3)+ 能力协商(S4)+ per-chunk SHA256 收完即校验(损坏当场删 .part 拒绝)[2026-09-13] + **内网直连监听**(xfer_listen/自签 TLS/票据一次性+data_token/悬空会话 2min 清扫)[2026-09-16] | ✅ |
| **agent** Linux | 85% | script lang=sh + monitor /proc + CLI 命令族平台方言化全覆盖(logs/sysinfo/service/proc/net/find/config/diagnose/compare/sync/watch/snapshot/tag,双方言一次直达;workflow/playbook/exec 为用户内容型)+ 三平台编译门禁(P1-4 七步);Linux 实机部署验证待补 | 🟡 |
| **cmd** CLI | 90% | 51 命令 + 流式 + TUI + 退出码体系 | ✅ |
| **client** SDK | 85% | 45+ 方法 + 流式 + 工具发现 | ✅ |
| **proto** 协议 | 95% | Envelope + 30 消息 + 二进制帧 + tunnel | ✅ |
| **console** Web | 80% | 反向代理 + 内嵌前端 + 实时事件 | 🟡 |
| **executors** | 90% | 8 执行器实现 + 跨平台 | ✅ |
| **internal/dagflow** | 95% | DAG 编排核心(Validate 图校验+Run 并发上限调度+失败级联+软依赖,串行模式启动序列完全确定) | ✅ v4.6.0 已接 relay 编排端点+CLI dag 子命令 |

**安全假设(显式)**:
- 单 Relay 为默认(水平扩展需 Redis 共享状态,接口已就绪)。
- Agent 默认 Windows(Linux 预留,部分执行器跨平台)。
- 命令策略默认黑名单 best-effort(可绕过);可选 allow-only 白名单(Relay -policy-allow / Agent policy_allow,交集语义黑名单兜底)。
- 传输 Complete 终态帧 at-least-once(resultQueue 补发);progress/终端输出 best-effort(录屏兜底)。

---

## 测试覆盖(v4.6.0)

535 测试 + 22 基准(2026-09-17 自动更新链路后口径,`go test ./... -list '.*'` 实测)。按包分布与重点验证场景:

| 包 | 测试数 | 文件数 | 重点场景 |
|----|--------|--------|----------|
| **relay** | 240 | 66 | 多客户端路由/断线重连/心跳[round2];6 类执行器[round3];console 转发[round4];mTLS 双向认证[round5];审计查询/导出[round6];多租户隔离[round7];批量分发/重试/进度[round8];SecretStore(TTL/一次性/并发防重放)/ScriptLibrary(路径穿越拒绝)/buffer_pool/审计错误路径/Prom 命名空间一致性[round9-10];transfer upload sha256 透传 E2E(真 WS fake Agent 全链路)[2026-09-13];webhook 收口(5 事件接线+flag/env 配置+通知体结构+真 handler 403/失败 result/证书撤销 E2E)+离线缓存策略绕过回归(P1-7)[2026-09-14];tls_watcher(CheckCertExpiry 正常/过期/三错误路径+CertDaysLeftMetric 双口径+LoadCertForRelay)[2026-09-14];智能调度负载接线(monitor 采样回流评分/TTL 过期无视/真 WS progress E2E)[2026-09-14] |
| **agent** | 82 | 33 | 连接主循环/文件传输(续传+hash)/shell 会话/录屏/自更新/健康自检/可靠性(path互斥/已满快路径/sweep/watchdog);rQueue 可靠投递(FIFO/drain/requeue/flushTo 写失败保留)[round9];单片 SHA 校验防线(匹配成功 rename/不匹配删 .part 防续传死循环)[2026-09-13] |
| **executors** | 57 | 25 | PowerShell(中文乱码修复)/脚本 BOM/截图/浏览器/GUI 点击/电源/监控;rules 门禁(发现去重/指纹组合/Dispatcher 校验/op=rules/变更失效闭环)[v4.4.0] |
| **proto** | 46 | 9 | 消息编解码/边界/二进制帧/tunnel;Envelope(ID 唯一/错误路径/宽容解码)+Heartbeat omitempty 向后兼容+多消息类型往返[round9] |
| **internal/wsx** | 5 | 1 | 优先级写引擎(P0 控制帧不被 P1 数据饿死——阻塞 socket 写+积压构造确定性验证/写错误经 Done 回传/ctx 契约两段分层断言(调用方 ctx 已取消=确定性 ctx.Err;writer ctx 取消=只锚定不挂死,nil/err 均合法——原"必须报错"断言与"可能迟到但不会丢"设计语义冲突,调度偏置下稳定翻车,2026-09-14 修订)/Start 幂等)[2026-09-12]
| **internal/dagflow** | 13 | 1 | 图校验 8 类非法图逐一拒绝+环点名;串行启动序列 50 次重跑逐位一致;并发峰值恰守上限 3;失败级联(health/watch 跳过+alert 软依赖照跑+跳过者从未启动);软依赖仍须等终态;ctx 取消 skipped 收尾;带环图不死锁 [v4.5.0];v4.6.0 relay 端到端:3 节点失败级联剧场(start_order 确定 prep→deploy/verify 零投递)+软依赖照跑+策略拦截 403+提交期校验 |
| **client** | 18 | 9 | SDK 方法/流式/工具发现 |
| **console** | 14 | 4 | 反向代理/事件转发 |
| **cmd** | 43 | 31 | CLI 工具函数(flag 解析/大小解析/strip 剥离)[v4.3.3];rules read E2E/ack 缓存/-s 防呆/deploy 模板[v4.4.0];per-chunk SHA256(computeSectionSHA 段 hash/分片 query 下发/非分片预计算 E2E/Agent 拒绝冒泡)[2026-09-13] |

**深度测试(rounds 1-10)**:round 1 修复 `TestMultiAgentRoutingE2E` 永久失败;rounds 2-7 逐层加深(relay 路由 → 执行器 → console → mTLS → 审计 → 多租户);round 8 覆盖批量分发不变量(并发投递/聚合/退避重试/幂等/ctx 取消/进度状态机);rounds 9-10 工作区深度推进——补 SecretStore/ScriptLibrary/buffer_pool/rQueue/Envelope 等弱测模块,修复空闲 Agent 掉线循环(S7 heartbeat_ack)、脚本库路径穿越漏洞、Prom 指标前缀 typo、审计写盘静默丢记录。

**并发安全**:批量进度跟踪用 `sync.RWMutex` 保护,`markRunning/markSucceeded/markFailed` 与并发 `Snapshot()` 线程安全(race 检测需 cgo/gcc,本机未启用,靠互斥锁设计保证)。

**测试运行**:`go test ./... -count=1`(单 relay 包约 10s)。基准:`go test ./relay/ -bench=.`(22 项)。

**测试密度补强(2026-09-12)**:internal/wsx 从零测试到 5 项(优先级写引擎核心语义);relay 补 scheduler_smart 负载均衡/标签过滤/端点与 update_repo 版本仓库 13 项。**逮住真 bug**:`UpdateRepo.List` 版本排序用纯字典序——`v2.0.0 > v10.0.0`,"最新在前"承诺被违反(上传 v10 后 CLI 列表仍把 v2 当最新);修复为数值感知比较(compareVersions,数字段按数值比,时间戳自动版本天然兼容)。

**per-chunk SHA256 强制(2026-09-13,P2-11 上传侧收口)**:+8 测(cmd 5/relay 1/agent 2)。**实现点修正**:原预估"需协议改动(滚动升级兼容性风险)",实测 `sha256` query → `file_xfer_init.SHA256` → Agent `expectedSHA256` 收完即比对 通道自 J2 起就存在,只是 CLI 上传路径从未接线——分片 `uploadSingleChunk` 与非分片 `fileUpload` 都只做事后比对(彼时损坏文件已被 Agent rename 成正式文件留在目标位置)。接线后修复真实痛点:断点续传 `checkCompletedChunks` 只查大小,损坏片被永久跳过 → 整文件重试 2 次全败。首个 transfer 端点 E2E(httptest relay + 真 WS fake Agent,init 透传断言+二进制帧累积+complete 回传)。

**webhook 告警收口 + P1-7 离线缓存策略洞(2026-09-14)**:+16 测(relay 11 webhook + 5 tls_watcher),总数 492。①**webhook 半成品收口**:5 个文档承诺事件此前只有 agent_offline 一个接线点,task_failed/policy_blocked/power_executed/agent_revoked 全是零触发点的空壳(notify 函数存在但无人调用);现 6 处接线(任务失败 result 分支/单任务+批量+DAG 三处策略拦截/power 下发成功/证书撤销),配置补 `-webhooks` flag(flag > `GO_RMM_WEBHOOKS` env,与其它配置同优先级序),删 `WebhookConfig.Enabled` 死字段,通知体{source,event,timestamp,data}经 httptest sink 确定性验证+三个真 handler E2E(403 拦截/真 WS fake Agent 回失败 result/真 CA revoke)。②**P1-7 安全洞(webhook E2E 构造离线目标时意外暴露)**:`handlePostTask` 离线缓存分支在命令策略校验**之前** return 202——高危命令发给离线 Agent 被缓存,Agent 上线 `dispatchPendingTasks` 补投时无二次校验,命令直达 Agent(与 P1-1 批量路径绕过同构);修复=策略校验挪到离线分支之前(校验不依赖在线态),离线缓存任务天然合法,回归测试锚定"离线目标高危命令必须 403 不得 202 缓存"。③**tls_watcher 零测试模块补测**(5 测):自签证书 NotAfter 可控生成器,DaysLeft 截断语义锚定(36h→1/-48h→-2/**72h 恰整天边界因毫秒延迟抖到 2——改 84h 半整天窗口**),三错误路径(缺文件/非 PEM/坏 DER)+CertDaysLeftMetric 双口径一致+错误 -1。④**wsx TestContextCancel flaky 修复**:原断言"writer ctx 取消后写必须报错"与设计语义("可能迟到但不会丢"——取消后已入队帧允许被迟到消费成功写出)冲突,cancel 与 writer 消费 controlCh 的 select 竞态+loopback 写 fast path 必成功,调度偏置下稳定返回 nil 翻车;修订为两段分层:调用方 ctx 已取消(未 Start)=确定性 `context.Canceled` 断言,writer ctx 取消=只锚定"不挂死"契约,5 连跑稳定。

**智能调度负载接线(XL6 收口,2026-09-14)**:+4 测(relay 225→229),总数 496。`SelectBestAgent` 的负载评分自始是空壳——`cpu := 0.0 // TODO: 从 metrics 取`,CPU/内存恒 0,评分实际只剩在途任务数,"负载均衡选最优 Agent"的文档承诺名不副实。现接线:**monitor 执行器的 progress 帧回流**(`serveAgent` 收 `Step=="monitor"` 的 TaskProgress → `parseMonitorLoad` 从 `"<msg> | <MonitorSample JSON>"` 消息体提取 CPU/内存使用率 → `Registry.UpdateLoad` 负载缓存)→ `SelectBestAgent` 评分取 TTL 内(2 分钟)采样参与 `cpu*0.4+mem*0.3+任务数*10`,**过期/无采样评分回 0**——宁可用"无数据"也不拿陈旧负载数据当实时值(时钟不可注入,TTL 过期测试同包直改 `loadSeen` 回拨,是唯一确定性手段)。顺带:删 `AgentLoad` 死结构体(零引用)、修 SelectBestAgent 文档注释里与实现不符的评分公式(`TaskCount*0.3` → 实际 `任务数*10`)。测试:parseMonitorLoad 八分支(真实格式/嵌套字段/omitempty 零值 ok=true/无 JSON/悬掉 `{`/`}` 在 `{` 前/坏 JSON/空串)+UpdateLoad/LoadOf 契约+评分三锚(采样驱动选择/1 个在途任务(10 分)压过 60 分负载差/TTL 过期采样被无视使陈旧重载者反超胜出)+真 WS E2E(monitor 帧回流 LoadOf 断言/非 monitor step 与无 JSON 消息不触碰缓存/采样入缓存后 SelectBestAgent 闭环)。

---

## 下一步(Next Steps)

按优先级排序的演进方向:

### P0 — 生产硬化

**已完成(v4.4.0, 2026-09-03, sess_610cef8e loan-kiosk 部署会话复盘)**:
- ✅ **项目规则门禁(.go-rmm/rules.md)全链路**:被部署项目在项目根放 `.go-rmm/rules.md` 写定制运维规则(勿营业时段杀壳/部署排除表/重启走哪个 cmdlet 等),AI/自动化操作前必须先加载——把"部署会话里人肉反复交代的注意事项"变成机器可执行的约束。四层实现:① `proto.TaskRequest.RulesAck` 字段(relay 结构体 re-marshal 全链路透传,含持久化重投);② `executors/rulesgate.go`——发现(allowed_roots+workspace 深度 2,跳系统目录,30s TTL 缓存)+指纹(SHA256 前 16 hex,单文件=自身/多文件=组合;门禁接受组合或任一单项目指纹——AI 读了本项目规则即可操作,不被同机其他项目卡死)+`Dispatcher.Dispatch` 校验(与命令策略同为 Agent 侧最后防线;拒绝信息**不回显指纹**防跳过阅读直接重放);③ file 任务新 op=`rules`(rules read 后端,指定 -p 目录或自动发现,FilePolicy 越界拒绝);④ CLI `rules read`(读全文+缓存 ack 到用户缓存目录)+ `task run/stream/exec/deploy` 自动携带(本地 CWD 有 `.go-rmm/rules.md` 时规则全文打 stderr 进 AI 上下文+本地指纹为 ack;显式 `--rules-ack` 优先;`--no-rules` 跳过)。豁免 file/screenshot/monitor(读规则必经+只读观测,防鸡生蛋)。Agent config `rules_gate: true` 开启,默认关(向后兼容)。
- ✅ **真二进制三组件端到端实证**(relay+agent 前台真跑,Windows):①未读规则执行被拒(错误指引 rules read)→②rules read 打印全文+缓存 ack→③重试自动携带通过→④改规则文件→旧 ack 被拒(TTL 30s 后)→⑤重读新 ack 通过→⑥CLI 在项目目录内操作时本地规则自动打印到 stderr+本地 ack 通过。冒烟现场逮住并修复**多根交叉发现重复 bug**(allowed_roots 的 projects 根与自动追加的 exeDir 以深度 2 覆盖同一 .go-rmm,发现结果只对根去重没对文件去重——文件级去重后 ack 组合才稳定)。
- ✅ **file 任务 AllowedRoots 装配断线修复**:config 注释承诺 allowed_roots "file 任务+大文件传输共用",实际只接线了传输引擎(transfers sharedPolicy),RegisterAll 构造的 FileExecutor AllowedRoots 恒空=所有 file 任务 fail-closed 拒绝。现 `Options.AllowedRoots` 注入+`resolveAllowedRoots` 纯函数三态(显式>整盘兜底>nil),file 任务/流式传输/规则发现三个消费方同源。
- ✅ **部署会话痛点四连修**:①cmdpolicy hints 补齐——`schtasks /create` 指引 Register-ScheduledTask(-LogonType Interactive 落桌面会话)、`Stop-Computer`/`Restart-Computer -Force` 指引 `go-rmm power shutdown/restart`(平台原生通道+标签白名单+审计),全部是会话里被拦后手工摸索出的正解;②`-s` 路径防呆——参数是路径形态(盘符/根/点斜杠前缀+单行)但 os.Stat 失败时硬错误,不再把整条路径字符串当脚本文本发远端吃 ParserError(Temp 被清理场景);③deploy PlayStep `timeout` 字段(exec 默认 60s/script 120s 可覆盖——npm install 常超 60s 被中途杀树);④`deploy init <name> [--template basic|zip-node]` 模板生成——zip-node 固化会话验证 5 轮的部署模式(robocopy 退出码 0-7 归一化/npmmirror/排除表/Restart-ScheduledTask/健康检查 retry),`reorderInitArgs` 处理 Go flag 在位置参数后停止解析的坑(`deploy init myapp --force` 的 --force 会被静默吞)。
- 测试 +18(rulesgate 7/CLI rules 9/hints 1/roots 1,含规则变更失效闭环与 httptest E2E),全量 `go test ./...` 12 包全绿,linux/darwin 跨平台编译门禁通过。

1. **水平扩展验证**:Agent 故障切换 E2E ✅(2026-08-19)——TestAgentFailoverBetweenRelays(双 fakeRelay:主注册→宕机→1s 退避切备用→稳定无抖动)+ TestBuildRelayCandidates(W1 候选去重/主优先);顺带修 Heartbeat 零值 NewTicker panic 兜底。Redis 三层联邦(Store/EventBus)实测仍待环境(本机无 Docker/Redis)。

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
- ✅ **一行部署无脑化(2026-08-25)**:`/install.ps1` 从静态文件改为动态模板——Relay 服务脚本时把 ExternalURL(或请求 Host/X-Forwarded-Proto 推导)烘焙进默认值,目标机器 `iex (irm ...)` 零参数全自动(免交互/非管理员自动 UAC 提权重跑/401 自动探测 Token 补问/装完轮询 /api/agents 确认上线);bootstrap 一行命令与 Web 控制台安装引导同步简化;另增 `-install-token` 部署专用 Token——服务时烘焙进脚本使 Token 也零传递,权限收窄为 Agent 注册 + GET /api/agents(其余 REST 401),主 Token 绝不烘焙。编码契约(PS 5.1 双坑实测):响应保留 UTF-8 BOM(无 BOM 文件被 PS 5.1 按 ANSI 误读,UTF-8 中文多字节会吃掉引号致解析崩溃),一行命令统一双保险去 BOM——先 `-replace '^FEFF'` 再 `-replace '^00EF00BB00BF'`(案例:nginx 静态服务 .ps1 无 charset 时 PS 5.1 irm 按 Latin-1 解码,BOM 变 ï»¿ 糊在第 1 行注释前→幻影命令→param() 不再是首语句报"赋值表达式无效";实测线上 203.0.113.99 即此故障,`/rmm/install.ps1` 被 nginx 直接静态服务(Content-Type: application/x-powershell 无 charset)且为旧版副本,须反代到 Relay 或加 charset utf-8)

### P1 — 能力补全
4. **Linux Agent**:补齐 Linux 执行器(systemd 服务/包管理/journalctl),打通跨平台。
5. ~~任务 at-least-once~~:✅ 大部分达成——任务结果(rQueue peek+commit 逐条/重连补发)+ 传输 Complete 终态帧(sendCompleteReliably 先入队再 flush,断线丢失会让 Console 挂 30 分钟超时的场景消除);progress/ready 中间帧与终端输出维持 best-effort(丢失无碍/录屏兜底)。

### P2 — 可观测/体验
7. ~~OpenAPI 同步~~:✅ 已完成——39 路径全量覆盖(46 method 级端点,含 shell/tunnel/secrets/scripts/audit/enroll/events/scheduler/bootstrap/tools);typed client 生成可选做。
8. **Web 控制台深化**:批量分发进度可视化 ✅(2026-08-19)——控制台新增批量面板(按 tag 提交异步批次,进度条+成功/失败/待投计数+ETA+各目标状态网格+终态成功率,轮询 1s,复用 /api/batch×3 端点);租户/组织管理 UI、审计回放仍待做。
9. ~~race 检测 CI~~:✅ 已完成——`ci.yml` 的 Test step 即 `go test -cover -race`(3563a3c 落地,本机与 CI 双验证零警告)。
10. ~~Agent Prometheus 指标端点~~:✅ 已完成——`127.0.0.1:7781/metrics`(仅本机回环,metrics_addr 可改/off 禁用),xfer 五计数器+sessions_active gauge+reconnects_total+uptime+build_info,gormmm_ 三 m 命名空间统一。
11. ~~文件传输 per-chunk SHA256 强制~~:✅ 已完成(2026-09-13,上传侧收口)——**实现点修正:零协议改动**。原预估"需协议改动(滚动升级兼容性风险)",实测 `sha256` query → `file_xfer_init.SHA256` → Agent `expectedSHA256` 收完即比对 的通道自 J2 stream-through 起就存在,只是 CLI 上传路径没接线(分片 `uploadSingleChunk` 与非分片 `fileUpload` 都只在事后比对响应回传 hash——彼时损坏文件已被 Agent rename 成正式文件留在目标位置)。现:①分片上传每片发送前 `computeSectionSHA256`(SectionReader 流式,~64KB 内存,代价是该片多读一遍磁盘)随 query 下发,损坏当场失败重传——修复真实痛点:断点续传 `checkCompletedChunks` 只查大小,损坏片(大小恰好匹配)被永久跳过 → 整文件重试 2 次全败只能人工清远端分片;②非分片路径预计算整文件 hash 进 query(管道模式 `-` stdin 单遍流无法预计算,维持事后比对兜底);③agent 侧 mismatch 路径删 `.part`(既有行为,本次补直接测试)。下载侧未动(HTTP 响应为纯 octet-stream,Agent 算的分片 hash 无回传通道,改响应协议风险大——合并后整文件校验继续兜底)。磁盘位腐烂(传输后落盘损坏)仍靠整文件校验兜底,诚实边界。
12. ~~Relay 速率限制~~:✅ 已完成(实现点修正为 Agent 发送侧,更贴近痛点)——`transfer_rate_limit_mb` config(MB/s,0=不限)→ TransferEngine 发送前令牌桶节流(手写零依赖,突发额度 1s 满速,ctx 取消即返);防并发下载吃满 Agent 上行挤死心跳 WS。

---

## 已知坑位(踩坑实录)

- **Windows 下 Git Bash 直跑 go-rmm.exe 偶发 0xC0000142 崩溃(2026-08-31,sess_610cef8e)**:症状为进程零输出直接退出,退出码 3221225794(=0xC0000142 STATUS_DLL_INIT_FAILED),`agent list`/`task run` 全崩,sleep 8 重试无效,次日自愈。**非代码缺陷**——该 exe 静态导入仅 kernel32.dll,崩溃发生在 loader 阶段(Go runtime 尚未启动),同一二进制(8/19 构建)前后均正常;定位为系统环境瞬态故障(desktop heap/会话资源耗尽或 AV 注入 DLL 初始化失败一类,当日机器同时出现文件被其他进程锁定、`rm: Device or resource busy` 等高负载征兆)。**规避**:`powershell -NoProfile -Command "& 'D:\...\go-rmm.exe' <args>"` 包装调用(换了一条进程创建路径,当日实证恢复;注意时间自愈与包装效果的混杂,下次复现时先做三方对照再下结论)。**复现时的三步定位法**:① 同一 bash 里试 `cmd //c go-rmm.exe version` 和 powershell 包装——隔离"Git Bash 创建进程特有"vs"系统级";② 试 `cmd //c ver`/`ping -n 1 127.0.0.1`——若同样 0xC0000142 则是系统级(desktop heap/AV),与 go-rmm 无关;③ 查事件查看器 Application 日志 0xC0000142 记录与 Defender 操作日志。**连带坑**:`-s` 参数语义是"本地文件路径,os.Stat 失败则原样当脚本内容发远端"(`cmd/go-rmm/main.go` uniqueNano 附近),PowerShell 里路径须用反斜杠;Temp 日期切换会清脚本,用前先 `ls` 确认存在。
- **退出码 1 ≠ CLI 崩溃**:输出含「任务结果 成功: false / 退出码: 1 / 耗时」+ `go-rmm: 任务执行失败` → 是远端脚本失败(常见为内嵌 JS 双引号/`$` 未转义的 ParserError,stderr 可能 GBK 乱码"锟斤拷"),go-rmm 进程本身正常;无结果块且本地零输出退出才是 CLI 自身问题。远端脚本含引号/`$` 用 here-string `@'...'@` 或 base64;中文关键词用 `[char]0x8D37` 构造防编码损坏。

## 已知坑位(踩坑实录)

- **Windows Defender 拦"未签名新文件 rename 顶替运行中服务 exe"**(2026-09-17,4060 生产更新实证):`os.Rename(新文件 → 服务exe名)` 确定性 Access denied(4 次复现,与账户/ACL 无关——LocalSystem+全控+写测试通过),但同操作回滚(rename 原文件归位)与改名到其它名字、CopyItem 到同目录全部畅通。**绕法(生产验证过的 copy 舞步)**:脚本任务里 `Copy-Item exe→.bak` → `Rename exe→.exe.old-running` → `Copy-Item 新→exe` → 写 `.update-pending` 标记 → `Register-ScheduledTask+Start-ScheduledTask` 延迟重启(分离 powershell 子进程在服务会话内不执行,schtasks /create 又被命令策略拦——按拦截提示走原生 cmdlet)。根治=构建签名。dev 机(iZt7)无此拦截,标准 push --path 直接过——**同构机器行为不同,更新失败先做 rename/copy 差分测试再选路径**。
- **线上 relay 的 unit 文件与运行参数漂移**:`-install-token` 在运行进程有、unit 文件没有,systemctl restart 会静默丢参数——替换二进制前 diff `ps` 与 unit 的 ExecStart,以运行态为准补齐再重启(2026-09-17 实操踩到)。

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
