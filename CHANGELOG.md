# Changelog

格式遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)。版本单一真相源：git tag。

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
