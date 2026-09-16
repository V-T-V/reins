# Changelog

格式遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)。版本单一真相源：git tag。

## [v4.7.0] — 2026-09-16 内网直连传输(Phase 1) + 分片绝对路径修复

### Phase 1 内网直连传输(吞吐数量级提升)
- **架构**:CLI ↔ Agent 字节流不再绕道云端 Relay——Agent 配置 `xfer_listen`(如 `:7783`,默认关)后监听本网段直传端口,CLI 自动探测直连可达性(Relay 只做信令/授权面),千兆内网从几 MB/s(出口上行瓶颈)提到线速;不可达/未开启时**静默回落**中继路径,零配置成本。
- **票据授权(零信任姿态不变)**:Relay 新端点 `POST /api/transfer/direct` 签发一次性票据(32B 随机 secret,明文只回 CLI,SHA256 hash 经控制 WS 推给 Agent)——60s 过期、路径(含 .ftpart 分片前缀)+方向绑定、用后即焚、恒时比对、consume 失败也消费防在线穷举;Agent 侧 begin 再签发 data_token 防会话劫持。
- **Agent 直连端点**:自签 ECDSA P256 TLS(票据不落明文)+ `/xfer/probe|begin|data|fetch` 四端点;`begin` 复用 `HandleInit`(路径策略/续传偏移/ALREADY_COMPLETE 快路径全套中继语义);`SendDownloadData` 重构抽出 `streamDownload` 共核(限速/分片/进度/计数两路同语义)。
- **CLI 直连优先**:`file upload/download` 单发与分片每片先试直连;`--no-direct`/`GO_RMM_NO_DIRECT` 显式关闭;直连熔断(首个 probe 失败/Agent 未开启即进程级禁用,后续分片零探测开销);路径策略 403 映射 blocked 终态不回落(中继同样会被拒)。
- **本地三组件 E2E 五场景实证**:5MB 单发直连 0.6s / 60MB 绝对路径分片(4 路并发+PowerShell 合并)5.3s SHA 一致 / 60MB 直连下载字节逐位一致 / `--no-direct` 中继 / 黑洞直连地址静默回落。

### 分片上传绝对路径修复(sess_8d55d252 复盘)
- **根因**:列目录/合并的 PowerShell 胶水假设 remotePath 为相对路径,绝对路径时 `Join-Path` 拼出 `dir\C:\...` 非法路径(PS 5.1 实证),回退 CWD=`C:\Windows\system32` 后必然 `MERGE_FAIL notfound`,断点续传同病根失效(会话实证 3 轮重传 3×159MB 全废)。
- **修复**:按绝对/相对分流(绝对路径直接 `-LiteralPath` 完整路径,跳过 workspace 自省;`remoteIsAbs` 兼容 Windows 盘符——CLI 可能跑在 Linux 控制 Windows Agent);解析前缀用 base name;`MERGE_FAIL` 判不可重试;含 Windows 真实 PowerShell 执行闭环测试(含 `"$($base).ftpart"` 插值陷阱锚——裸 `$base.ftpart` 会被 PS 解析为属性访问变空串)。

### E2E 逮住的 6 处真 bug(同轮修复)
- CLI `ds.begin`/Agent `handleBegin` 双端 UnixNano transferID 并发撞车(4 worker 同纳秒 → sessions/dataTk 相互覆盖 → data_token 401 + pathInUse 泄漏级联"路径被占用"),双端改 uuid。
- 直连失败后 section 已部分消费未回卷,中继收到残缺流(`Seek(0)` 回卷)。
- 直连分片成功误用 `continue` 留在重试循环(每片白传 3 次,改 `break`)。
- `--no-direct` 布尔 flag 被 `stripValueFlags` 当值型 flag 吞掉后参(纯 token 过滤)。
- 悬空会话(begin 后 data 永不到达)pathInUse 永久泄漏(`sweepIdleSessions` 2min 空闲清扫)。
- `handleData` EOF 后死循环读 body(上传成功但永不回响应,goroutine dump 定位)。

### 测试
+26 测(分片绝对路径 9/proto 票据 2/relay 票据端点 E2E 4/agent 直连 5/CLI 直连 5),总数 496→522,12 包全绿。

## [Unreleased] — 2026-08 深度推进（22 轮迭代重建）

### 稳定性（S0 修复）
- **mTLS 证书续签字塔（W7）**：新增 `/api/renew`（mTLS 客户端证书自鉴权 + `SignAgentCSRNoRevoke` 不撤销旧证不断连）+ Agent 后台 `certRenewLoop`（每小时检查）+ 证书三件套原子写（temp+rename）。原续签复用一次性 token → 24h 必 401 → Agent 永久离线。
- **服务模式自更新激活（U1）**：成功路径补 `os.Exit(0)` 触发 SCM 重启。原实现新二进制永不生效（所有服务模式自更新都是假象）。
- **更新原子替换（U2）**：备份改 copy（exe 保留原位），rename 失败/断电不再"砖"。
- **ca-state fail-closed（CRL1）**：状态文件损坏拒绝启动（原静默清空撤销集→已撤销 Agent 重新放行）+ saveState 原子写。

### 连接可靠性（W1-W5）
- **多 Relay 故障切换（W1）**：候选列表轮询（主→备用→…→回主）。原 relayIdx 仅在主 URL 为空时生效，故障切换形同虚设。
- **CAS 原子化（W2）**：`CASDispatchToPending` 单事务完成 pending→dispatched+Attempt++，dispatchTask 删 SaveTask 裸覆盖（原快任务 completed 可被覆盖回 dispatched→重复执行）。
- **panic 隔离（W3）**：recvLoop 全 handler safeCall + pool recover（原单任务 panic 崩整个 Agent）。
- **隧道/下载写超时（W4）**：SetWriteDeadline + 下载写 15s（原无限持锁拖垮心跳）。
- **退避重置（W5）**：健康连接(≥60s)断开后 backoff 归位（原永不重置，抖动后每次断线等 30s）。

### 弱网韧性
- **写优先级双侧（A1/A2）**：wsConn 从无优先级 writeMu 重构为 P0(control:心跳/ack)/P1(data) 双 channel + 单 writer goroutine + done 同步——弱网大传输不再饿死心跳致反复重连。
- watchdog 2×→3×、排队超时兜底、resultQueue 硬上限 1000、幂等 TTL 1h→2h。

### 安全
- **RBAC 执行（短1）**：RequireRole 守卫高危端点（revoke/enroll-token/power/secrets）+ power/update 任务类型守卫 + authorize 按 Secret 部分比较（支持 tenant:role:secret；原 viewer 格式 token 连鉴权都过不了）。
- **browse SSRF 防护（H3）**、token query 移除（H4）、登录限流 XFF 白名单（M1）、审计丢弃可观测（M3+audit_dropped 指标）。
- **命令策略统一（短2）**：internal/cmdpolicy 32 条单一真相源（原 Relay 15 条/Agent 32 条漂移）。

### 深层风险
- **离线任务契约（H-2）**：离线+曾注册+不支持→400；从未上线/支持→202 缓存（原 API 400 但落库→上线补投→非幂等命令延迟执行）。
- **in-flight 计数（H2/H-3）**：atomic ±1 + CAS 钳 0（防负漂移→调度失衡）。
- **isLast 写错检查（H-5）**：上传/下载终态帧写失败立即收尾（原悬挂至 30 分钟）。
- **关停顺序（H-1）**：Agent 连接先于 store 关闭。

### 架构
- **internal/ 共享域包**：ca（PKI）/cmdpolicy（命令策略）/taskstore（存储域+状态机+CAS+Batch）/wsx（优先级写引擎）——relay main 包减重 ~1400 行。
- **ExtractCommand 注册表**（proto 层）：命令提取单点化，加新命令类零遗漏。
- **update 纳入 Dispatcher**：能力协商修复+统一分发。
- **db.Batch**（taskstore 全部写事务）。

### 可观测性
- 任务延迟直方图（gormmm_task_duration_ms histogram）、tasks_acked/nacked、audit_dropped 进 /metrics；转发透传 env.Payload（省 re-marshal）。

### 修复（其他）
- PowerShell exit code（try/catch 前缀 + -NonInteractive + TestMain 预热）、GetCronRule O(1)、H-4 payload 透传、make dist 所需 install 脚本、quickstart 去硬编码、openapi 重复键。

### 部署与治理
- docker-compose（Relay+可选 Agent）、.env.example、config.example.json、release.yml（ghcr+goreleaser）、install.sh/ps1、LICENSE、CONTRIBUTING。

## [4.5.0] — relay 架构梳理 v4.5.0

## [历史] — 4.x 及之前
反向 WebSocket 中继穿透 NAT + Windows Agent 服务化 + 9 类任务执行器；
mTLS + 内置 CA + CRL + RBAC + hash chain 审计 + SIEM 导出；
Web 控制台 + Go SDK + Helm chart + goreleaser。
