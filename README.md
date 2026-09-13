# go-rmm

> 企业级零信任远程访问 + 自动化运维平台 — Go 实现

[![Go](https://img.shields.io/badge/Go-1.25+-00ADD8?logo=go)](https://go.dev)
[![Version](https://img.shields.io/badge/version-4.3.0-blue)]()
[![License](https://img.shields.io/badge/license-MIT-green)]()
[![Tests](https://img.shields.io/badge/tests-309-brightgreen)]()
[![CLI](https://img.shields.io/badge/CLI-51%20commands-blue)]()
[![API](https://img.shields.io/badge/API-42%20endpoints-blue)]()

远程控制 Windows/Linux 主机:命令执行、文件传输、交互式 Shell、端口转发、多步编排、系统监控、告警、自更新。通过反向 WebSocket 中继穿透 NAT,支持一对多批量分发、多租户隔离与审计防篡改。

---

## 目录

- [架构总览](#架构总览)
- [核心特性](#核心特性)
- [快速开始](#快速开始)
- [部署](#部署)
- [配置](#配置)
- [安全](#安全)
- [多租户与 RBAC](#多租户与-rbac)
- [批量任务分发](#批量任务分发)
- [项目规则门禁(.go-rmm/rules.md)](#项目规则门禁go-rmmrulesmd)
- [API](#api)
- [AI Agent 接入](#ai-agent-接入)
- [指标](#指标)
- [文档](#文档)

---

## 架构总览

```
                        ┌─────────────────────────────────────────────────────────────┐
                        │                     Relay 中继服务器                          │
   CLI / AI SDK         │  HTTP REST (42 端点) + 内嵌 Web 控制台 + SSE 事件流          │
   ┌──────────┐  HTTPS  │  ┌────────────┐  ┌──────────┐  ┌──────────┐  ┌──────────┐   │
   │ go-rmm   │────────►│  │  Router    │─►│ Policy   │─►│  RBAC    │─►│  Audit   │   │
   │ (51 cmd) │         │  │  (REST)    │  │ (黑名单) │  │ (多租户) │  │(hash链) │   │
   └──────────┘         │  └─────┬──────┘  └──────────┘  └──────────┘  └──────────┘   │
                        │        │ dispatch                                              │
   Web 控制台           │  ┌─────▼──────────────────────────────────────────────────┐  │
   (内嵌前端)           │  │            EventBus + BatchDispatcher(批量分发)         │  │
                        │  │  TaskStore(bbolt/Redis) + Reaper(超时回收) + Scheduler│  │
                        │  └──────────────────────┬────────────────────────────────┘  │
                        │        反向 WS 长连接    │ 智能调度(负载均衡选最优 Agent)    │
                        │  mTLS + 内置 CA + CRL   │ Prometheus /metrics + pprof        │
                        └────────────────────────┼─────────────────────────────────────┘
                                                 │ 反向 WebSocket(穿透 NAT)
            ┌────────────────────────────────────┼────────────────────────────────────┐
            │                                    │                                    │
       ┌────▼─────┐                        ┌────▼─────┐                        ┌────▼─────┐
       │  Agent 1 │                        │  Agent 2 │                        │  Agent N │
       │ Windows  │                        │ Windows  │                        │  Linux   │
       │ 服务     │                        │ 服务     │                        │  (预留)  │
       ├──────────┤                        ├──────────┤                        ├──────────┤
       │powershell│                        │powershell│                        │ script   │
       │ script   │                        │ file     │                        │ file     │
       │ file     │                        │ power    │                        │ monitor  │
       │screenshot│                        │ update   │                        └──────────┘
       │ browse   │                        └──────────┘
       │ guiclick │      9 类任务执行器 / Agent 自更新(rename+copy+SCM 重启) / Session 录屏
       │ monitor  │
       └──────────┘
```

**数据流**:CLI/Web → HTTP REST → Relay 路由 → 命令策略校验 → RBAC 多租户鉴权 → 审计 hash chain → EventBus 投递 → 反向 WS → Agent 执行 → 结果回流(SSE/轮询)。

**水平扩展**:多 Relay 通过 Redis 共享 `TaskStore` + Pub/Sub 事件联邦,Agent 多 Relay 故障切换。

---

## 核心特性

### 远程操作(15 命令)
| 能力 | 命令 |
|------|------|
| 命令执行 | `task run -a <agent> -c "Get-Date"` |
| 流式执行 | `task stream -a <agent> -c "ping -t 8.8.8.8"` (SSE 实时 stdout) |
| 多 Agent 并行 | `exec -a a1,a2,a3 -c "hostname"` |
| 交互式 Shell | `shell open -a <agent>` (asciinema 录屏) |
| 文件传输 | `file upload/download` (SHA256 + 断点续传 + 并发分片) |
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
| **多租户 RBAC** | `tenant:role:secret` token,组织/角色/权限/配额;组织 bbolt 持久化(重启不丢),禁用/删除即失效 |
| **审计防篡改** | hash chain + SIEM 导出(CEF/Syslog/JSON) |
| **批量分发** | 一对多分发 + 结果聚合 + 指数退避重试 + 实时进度;异步 REST 端点 + SSE 进度流 + CLI `batch start/status/watch` |
| **命令策略白名单** | 默认 32 条黑名单;可选 allow-only 白名单(Relay `-policy-allow` / Agent `policy_allow`,交集语义黑名单兜底) |
| **Linux Agent** | script `lang=sh` + monitor `/proc` 全维采集;CLI 15 组运维命令平台方言化(logs/sysinfo/service/proc/net/find/config/diagnose/sync/watch/snapshot/tag 等) |
| **Secret 管理** | 凭据 TTL+一次性,不落 audit |
| **Session 录屏** | asciinema v2,可回放 |
| **可观测性** | slog + Prometheus + pprof;Agent 本地 `127.0.0.1:7781/metrics`(xfer 计数/会话/重连/uptime) |
| **Webhook 告警** | Slack/钉钉/企业微信 |
| **水平扩展** | TaskStore interface + Redis + Pub/Sub |
| **AI Agent SDK** | `client.New()` → 45+ 方法 + 流式执行 |

---

## 快速开始

```bash
# 1. 服务端(一台机器)——零配置启动,自动生成 token
./go-rmm-relay
# → 浏览器打开 http://localhost:7780(内嵌 Web 控制台)

# 2. 被控端(每台目标机)——复制一行命令即可
./go-rmm-agent run -relay ws://server:7780/agent -token <token>

# 3. 操作(你的电脑)
go-rmm --relay http://server:7780 --token <token> agent list
go-rmm --relay http://server:7780 --token <token> task run -a <agent> -c "hostname"
go-rmm --relay http://server:7780 --token <token> monitor top -a <agent>
```

> 零配置:Relay 首次启动若未设 `-token` 会自动生成一个并打印;Agent 也可用 `GET /api/bootstrap` 拿到一行 PowerShell 安装命令(`iex (irm ...)`),复制粘贴到目标机即可。

---

## 部署

| 方式 | 命令 |
|------|------|
| Linux | `./scripts/quickstart.sh server` |
| Windows | `.\scripts\quickstart.ps1` |
| Docker | `docker build -f Dockerfile.relay -t go-rmm . && docker run -p 7780:7780 go-rmm` |
| K8s | `helm install rmm deploy/helm/go-rmm` |
| 服务器 | `./scripts/deploy-to-server.sh` |
| Nginx 反代 | 见 `nginx-rmm.conf`(子路径部署 + TLS 终端) |

详细操作见 [DEPLOYMENT.md](./DEPLOYMENT.md)。生产建议:Nginx/Traefik 做 TLS 终端,Relay 后端;Agent 通过 `wss://` 连接;多 Relay 共享 Redis。

### Agent 自更新(公网链路用 --path 引用模式)

```bash
# 标准流程:分片上传新二进制 → 引用模式推送(零内联,公网必用——内联 7MB+
# base64 单帧 WS 写在公网链路必超时)
go-rmm file upload -a <agent> go-rmm-agent.exe "C:\go-rmm\slave-workspace\go-rmm-agent-new.exe"
go-rmm update push -a <agent> --version v1.2.3 --path "C:\go-rmm\slave-workspace\go-rmm-agent-new.exe"

# Agent 侧:SHA256 校验 → .bak 备份 → 两步 rename(运行中 exe 先移走再顶名,
# Windows 顶名到被映射路径必 Access denied)→ SCM 重启;健康自检失败自动回滚
```

### Linux Agent(85%)

```bash
# Linux 被控端:同一二进制,systemd 服务或前台运行
./go-rmm-agent run -relay ws://relay:7780/agent -token <token>

# 下发 shell 脚本(lang=sh,临时文件执行+进程树管理)
go-rmm task run -a <agent> -t script -s deploy.sh          # 本地脚本文件直传
go-rmm -o json task run -a <agent> -t script -f payload.json

# 运维命令自动平台方言化(CLI 查询 agent os 字段选 PowerShell 或 sh):
go-rmm sysinfo -a <agent>          # /proc+df 手拼同构 JSON
go-rmm service list -a <agent>     # systemctl list-units
go-rmm proc list -a <agent>        # ps --sort=-pcpu
go-rmm logs tail -a <agent> /var/log/syslog   # tail -f
go-rmm net ports -a <agent>        # ss -tlnp
go-rmm monitor top -a <agent>      # /proc 全维(CPU 差分/Top 进程/NetIO 速率)
```

CI 三平台(windows/linux/darwin)编译门禁;workflow/playbook 的命令内容由作者写,方言自定义。

---

## 配置

Relay 配置优先级:**命令行 flag > 环境变量 > 默认值**。

| Flag | 环境变量 | 默认 | 说明 |
|------|----------|------|------|
| `-addr` | `GO_RMM_RELAY_ADDR` | `:7780` | HTTP 监听地址 |
| `-token` | `GO_RMM_RELAY_TOKEN` | (自动生成) | Agent/Console 鉴权 token;空则不校验 |
| `-dev` | `GO_RMM_RELAY_DEV` | off | 开发模式(详细日志、放宽校验) |
| `-mtls` | `GO_RMM_RELAY_MTLS` | off | 启用 mTLS(需内置 CA + Agent 客户端证书) |
| `-tls-cert` | `GO_RMM_RELAY_TLS_CERT` | — | Relay TLS 证书文件 |
| `-tls-key` | `GO_RMM_RELAY_TLS_KEY` | — | Relay TLS 私钥文件 |
- `-external-url` | `GO_RMM_EXTERNAL_URL` | — | 外部访问地址(经反代/TLS 终端时用),如 `https://host/rmm`
| `-policy-allow` | `GO_RMM_POLICY_ALLOW` | (黑名单模式) | 命令白名单(逗号分隔正则);非空则 allow-only |

配置校验(`Config.Validate()`):`addr` 不能为空;mTLS 模式必须配证书;token 长度建议 ≥8。

CLI 全局 flag:`--relay`(默认 `http://127.0.0.1:7780`)、`--token`、`--output text|json`、`--insecure`(跳过自签证书校验)、`--cacert`。

---

## 安全

| 层 | 机制 |
|----|------|
| **传输** | 反向 WS(wss://)+ mTLS 双向认证 + 内置 CA + CRL 吊销 |
| **鉴权** | `tenant:role:secret` token;空 token 配置 = 不校验(仅 dev) |
| **授权** | RBAC 三角色(admin/operator/viewer)+ 细粒度 action |
| **命令策略** | 黑名单拦截高危命令(Format-Volume 等),批量下发也校验;可选 allow-only 白名单(交集语义) |
| **审计** | hash chain 防篡改 + 异步落盘 + SIEM 导出(CEF/Syslog) |
| **Secret** | 引用式凭据(TTL + 一次性),不落 audit |
| **隔离** | 租户只能看自己的 Agent(tags["tenant"] 过滤) |

> 深度防御:黑名单为 best-effort(可绕过),Agent 侧二次校验兜底;需要收紧时启用白名单——
> Relay `-policy-allow '^Get-Service,^node '`(或 `GO_RMM_POLICY_ALLOW` env,逗号分隔正则)、
> Agent config `policy_allow: ["^systemctl status"]`。白名单非空时命令必须命中其一且不碰黑名单。

---

## 多租户与 RBAC

token 格式(向后兼容):

| 格式 | 含义 |
|------|------|
| `secret123` | 旧格式,无租户,全权限(admin) |
| `tenant:role:secret` | 租户隔离 + 角色,如 `acme:operator:s3cr3t` |

**角色**:
- `admin` — 全权限(默认,旧 token 等效)
- `operator` — 可下发任务/管理 Agent,不可 revoke/电源
- `viewer` — 只读(查看 agents/tasks/metrics)

**组织(Organization)**:每个租户一个组织实体,含名称、Agent 配额(`Quota`,0=不限)、启停(`Enabled`,禁用后该租户所有操作被拒)。

**隔离规则**:`tenant=acme` 的 token 只能看到 `Tags["tenant"]="acme"` 的 Agent;旧 token(无 tenant)看全部。

**可配置策略**:`RolePolicy` 可在启动时从配置注入角色→action 映射,也可运行期 `GrantRole` 动态加权限(替代硬编码)。

---

## 批量任务分发

一对多任务下发 + 结果聚合 + 失败重试 + 实时进度(`relay/batchdispatch.go`)。

**异步端点(推荐,公网/大规模)**:

```bash
# 提交异步批次(202 返回 batch_id,带指数退避重试)
curl -X POST $RELAY/api/batch -H "Authorization: Bearer $TOKEN"   -d '{"tag":"env=prod","type":"powershell","payload":{"command":"Get-Date"},"max_retries":2}'

# 进度快照(轮询):total/succeeded/failed/percent/eta/各目标状态机
curl $RELAY/api/batch/<batch_id> -H "Authorization: Bearer $TOKEN"

# SSE 实时进度(每秒一帧,终态发 done 事件)
curl -N $RELAY/api/batch/<batch_id>/progress -H "Authorization: Bearer $TOKEN"

# CLI 等价
go-rmm batch start --tag env=prod -c "Get-Date" --retries 2
go-rmm batch watch <batch_id>          # 实时跟踪到终态
```

**同步端点(轻量场景)**:`POST /api/tasks/batch`
```json
{"tag": "env=prod", "type": "powershell", "payload": {...}, "timeout": 30}
```
按标签匹配在线 Agent,并行投递,返回 `{dispatched, offline, task_ids}`。

**BatchDispatcher(增强 API)**:

| 能力 | 说明 |
|------|------|
| 一对多分发 | 并发投递所有目标,`dispatchFn` 抽象便于注入 mock |
| 结果聚合 | `BatchResult` 含每目标成功/失败/输出,`SuccessRate()`/`Succeeded()`/`Failed()` |
| 指数退避重试 | 失败目标按 `100ms * 2^n`(封顶 1s)重投,`MaxRetries` 可配 |
| 幂等 | 已成功目标不重投;taskID 含 `batchID-agentID-attempt` 去重 |
| 实时进度 | `BatchProgress` 线程安全快照:`Total/Dispatched/Succeeded/Failed/Pending/Percent/ETA` |

**进度状态机**:`pending → running → (succeeded | failed)`,重试时 `failed → running → ...`。

```go
d := newBatchDispatcher(dispatchFn)
req := BatchRequest{BatchID: "b1", AgentIDs: ids, MaxRetries: 3}
br, progress := d.DispatchWithRetryTracked(ctx, req)
// 分发期间可并发调 progress.Snapshot() 拿实时进度
fmt.Printf("进度 %.0f%%, 成功 %d, 失败 %d\n",
    progress.Snapshot().Percent, progress.Snapshot().Succeeded, progress.Snapshot().Failed)
```

### DAG 编排核心(internal/dagflow,v4.5.0)

批量分发解决"一批 Agent 同时跑一个任务";多阶段运维流程(备份 → 升级 → 健康检查 → 失败回滚)需要**依赖编排**——`internal/dagflow` 提供纯逻辑、零依赖、确定性可测的 DAG 调度核心:

```go
g := &dagflow.Graph{
    MaxConcurrency: 3,
    Nodes: []dagflow.Node{
        {ID: "backup"},
        {ID: "upgrade", Deps: []string{"backup"}},
        {ID: "health",  Deps: []string{"upgrade"}},
        {ID: "alert",   Deps: []string{"upgrade"}, ContinueOnError: true}, // 上游失败也照跑
    },
}
if err := g.Validate(); err != nil { /* 环/悬空依赖/自依赖,报错点名肇事节点 */ }
res := g.Run(ctx) // res.Nodes 逐节点 succeeded/failed/skipped + res.StartOrder
```

| 能力 | 语义 |
|------|------|
| 图校验 | 重复 ID/悬空依赖/自依赖/重复依赖/Kahn 判环,环上节点点名报告 |
| 并发上限 | 就绪集合按 ID 字典序取前 `MaxConcurrency` 个启动;`<=0` 串行 |
| 失败级联 | 节点失败后传递下游整枝 `skipped`(从未启动),防半升级状态继续跑 |
| 软依赖 | 节点标 `ContinueOnError` = 自身依赖全部软化:上游成败不限,但仍须等其终态(拓扑顺序保留) |
| 确定性 | 串行模式启动序列完全可复现(同输入同序列);并发模式每轮派发批内按 ID 序,启动/跳过集合只由结果决定 |
| 取消语义 | ctx 取消:未启动节点按 `skipped` 收尾;带环非法图不死锁不 panic,环上节点 `skipped` 收尾 |

---

## 文件传输

大文件传输走**并发分片 + 断点续传**,经 nginx 反代不再依赖单连接超时,跨 NAT 稳定可靠。

### 能力概览

| 特性 | 说明 |
|------|------|
| 并发分片 | `>50MB` 自动切 10MB/片 × 4 路并发(可调),重叠 RTT 提速 |
| 断点续传 | 中断重跑自动跳过已传分片(按文件名+大小校验) |
| 流式低内存 | SectionReader 边读边传,每 worker ~64KB(非整片缓冲) |
| SHA256 双边校验 | Agent 算 hash 回传 + CLI 本地比对,合并后整文件校验 |
| 进度 ETA | 瞬时速率(滑动窗口)+ 剩余时间预估 |
| path 互斥 | 同 path 并发上传自动拒绝(防双写 `.part` 致 SHA256 不匹配) |
| 已满快路径 | `.part` 已满时直接 rename + complete,不等 sender 跳字节 |
| 孤儿分片 sweep | Agent 每 30 分钟清理 2 小时前的残留 `.ftpart*` |
| 死连接 watchdog | 半开连接(NAT 超时)2×心跳无收包则强制重连,秒级恢复 |
| 能力协商 | 分片下载需 Agent `xfer_offset` 能力,旧 Agent 显式拒绝 |

### 用法

```bash
# 上传(大文件自动分片)
go-rmm file upload -a <agent> <local> <remote>
go-rmm file upload -a <agent> --concurrency 8 --chunk-size 20M --chunk-retry 5 <local> <remote>

# 下载(大文件自动分片)
go-rmm file download -a <agent> <remote> <local>
go-rmm file download -a <agent> --concurrency 8 <remote> <local>
```

| flag | 默认 | 说明 |
|------|------|------|
| `--concurrency` | 4 | 并发上传/下载路数 |
| `--chunk-size` | 10M | 每片大小(上限 128M,防 OOM) |
| `--chunk-retry` | 3 | 每片失败重试次数(指数退避 1s/2s/4s) |

### 实测性能

| 场景 | 文件 | 速率 | 说明 |
|------|------|------|------|
| 本机(loopback) | 500MB | ~30 MB/s | 磁盘瓶颈,并发无明显优势(RTT≈0) |
| 跨网络(家庭上行) | 1GB | ~0.54 MB/s | 带宽硬瓶颈,并发提速 ~22% |

> 注:并发收益在**高延迟链路**(卫星/跨境/移动网络)最明显;家庭上行带宽瓶颈下收益有限。

---

## API

42 个 REST 端点(`relay/router.go`),完整契约见 [deploy/openapi.yaml](./deploy/openapi.yaml)。分类:

| 类别 | 代表端点 |
|------|----------|
| 任务 | `POST /api/tasks`、`GET /api/tasks/:id`、`POST /api/tasks/batch`、`POST /api/tasks/:id/cancel` |
| Agent | `GET /api/agents`、`GET /api/bootstrap`、`POST /api/agents/revoke` |
| 文件 | `POST /api/files/upload`、`GET /api/files/download` |
| 监控 | `GET /api/sysinfo/:agent`、`GET /api/snapshot/:agent` |
| 事件 | `GET /api/events`(SSE:agent上线/任务结果/策略拦截) |
| 安全 | `GET /api/audit`、`GET /api/audit/export`、`POST /api/power` |
| 运维 | `GET /metrics`(Prometheus)、`GET /debug/pprof/*` |

---

## 项目规则门禁(.go-rmm/rules.md)

被部署项目可在**项目根**放 `.go-rmm/rules.md` 写定制运维规则——AI/自动化用 go-rmm 操作该项目前必须先加载规则并遵守。规则随项目仓库走,部署同步到远端。

### 规则文件示例

```markdown
# loan-kiosk-platform 运维规则

1. 勿在营业时段(9:00-18:00)杀壳进程 YxLoan.exe——用户可能正在使用
2. 部署镜像同步必须排除 runtime/ node_modules/ .env(远端运行态数据)
3. 重启壳用 Register-ScheduledTask(-LogonType Interactive 落桌面会话),
   不要用 schtasks /create(被命令策略拦截)
4. 健康检查 http://localhost:3100/health 连续 3 次通过才算部署成功
5. 中文关键词经命令传输会乱码,用 [char]0x8D39 构造
```

### 工作流(AI 视角)

```bash
# ① 加载规则:读全文 + 自动缓存 ack(规则内容指纹)
go-rmm rules read -a agent-xxx            # 自动发现全部规则
go-rmm rules read -a agent-xxx -p D:\proj # 只读指定项目

# ② 操作:task run / exec / deploy 自动携带缓存的 ack
go-rmm task run -a agent-xxx -c "..."     # 上一步的 ack 已自动附上

# ③ 规则变更 → 旧 ack 失效,任务被拒并指引重新 read(强制读新规则)
```

### 机制分层

| 层 | 行为 | 生效条件 |
|----|------|----------|
| 软提醒 | CLI 当前目录有 `.go-rmm/rules.md` 时,变更类命令把规则全文打印到 stderr(AI 上下文可见)并以本地指纹为 ack | 始终(本地模式) |
| 硬门禁 | Agent 拒绝无 ack / 过期 ack 的变更类任务(powershell/script/browse/guiclick/update/power),拒绝信息不回显指纹(防跳过阅读直接重放) | agent 配置 `rules_gate: true` |

- ack 语义:规则内容 SHA256 前 16 hex。多项目时接受组合指纹或任一单项目指纹(AI 读了自己项目的规则即可操作,不被同机其他项目卡死)
- 发现范围:`allowed_roots` + `workspace`(深度 2),30s 缓存——规则文件改了半分钟内生效
- 豁免:`file`(读规则必经)/`screenshot`/`monitor`(只读观测)
- 已知边界:`sync`/`file upload` 走流式传输通道(非任务协议),不在门禁内;`task batch`/tag 批量路径暂不携带 ack

### 一行命令部署模板

```bash
go-rmm deploy init myapp                       # basic: 上传→停→启→健康检查
go-rmm deploy init myapp --template zip-node   # zip 包→镜像同步(排除表)→npm→重启计划任务→健康检查
```

zip-node 模板固化了真实会话验证 5 轮的部署模式(robocopy 退出码 0-7 归一化 / npmmirror / 长步骤超时这些坑都已写对)。playbook 步骤支持 `timeout`(npm install 类长步骤默认 60s 会中途杀树,必须调大)。

---

## AI Agent 接入

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

---

## 指标

| 项目 | 数值 |
|------|------|
| Go 文件 | 221 |
| 代码行 | 40,900+ |
| CLI 命令 | 52 |
| API 端点 | 42 |
| SDK 方法 | 45+ |
| 测试用例 | 460 |
| 基准测试 | 22 |
| 任务类型 | 9 |
| 性能优化 | 8 项 (55x~1940x) |

---

## 文档
- [部署指南](./DEPLOYMENT.md)
- [性能基线](./BENCHMARK.md)
- [API 文档](./deploy/openapi.yaml)
- [架构详解](./AGENTS.md)

## 📜 License: MIT
