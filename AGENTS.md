# go-rmm · AGENTS.md

## 项目内容（What）

Go 实现的**远程自动化执行操作平台**——通过反向 WebSocket 中继，让 Web 控制台远程驱动 Windows Agent 执行 6 类任务。

三组件架构：

```
Web 控制台(浏览器) ──HTTP/SSE──► Relay 中继服务器 ◄──反向WS长连接── Windows Agent(kardianos服务)
                                      │                                  ├ powershell  执行 PowerShell 命令
                                      │                                  ├ script      执行下发脚本
                                      │                                  ├ file        文件上传/下载
                                      │                                  ├ screenshot  PrintWindow 截图
                                      │                                  ├ browse      chromedp 浏览器自动化
                                      │                                  └ guiclick    视觉模型+SendInput 点击
                                      └──────── 任务路由 + 结果回流 ──────┘
```

**做**：远程下发任务、穿透 NAT（Agent 主动外连）、Windows 服务化常驻、6 类执行器、Web 控制台。
**不做**：监控告警（P1+）、多 Agent 编排调度（P2）、Linux 受控端（架构预留，P0 仅 Windows）、自主决策 Agent（与 agentloop 解耦，P2 可选接入）。

## 目标（Goal）

- **G1**：Agent 注册为 Windows 服务，开机自启、崩溃自恢复，经反向 WS 上线到 Relay。
- **G2**：控制台浏览器下发任务 → Relay 路由 → Agent 执行 → 结果实时回流（SSE）。
- **G3**：6 类任务执行器可用，稳定性优先级：`接口调用 > 命令执行 > UI 控件识别 > 图片识别 > 坐标点击`。
- **成功标准**：三组件单二进制部署，`make all` 一次编译产出，本地三端联调跑通"PowerShell 命令下发→结果回显"闭环。

## 当前情况（Status）

- **完成度**：**P6 大文件传输完成**——5GB 级二进制流式传输，stream-through 中继，SHA256 校验
- **代码规模**：66 个 Go 文件，约 10000 行；18 个测试文件全绿；46 个 commit
- **P0 基础**（已稳定）：
  - **协议层**（`proto/`）：Envelope 信封 + 13 种消息类型 + 6 种任务载荷 + 幂等键 + type 校验
  - **Relay 中继**（`relay/`）：WS 连接池 + REST API + SSE 事件流 + 心跳超时清理（Sweep 主动关连接）
  - **Agent**（`agent/`）：服务化双模式 + 反向 WS + 心跳 + 指数退避重连 + worker pool 并发限流
  - **6 类执行器**（`executors/`）：powershell/script/file/screenshot/guiclick/browse
  - **Win32 封装**（`win32/`）+ **视觉识别**（`vision/`）+ **Web 控制台**（`console/`）
- **P1 升级**（工业级）：
  - **安全性**：mTLS 双向证书认证（Relay 内置 CA 自签根 + 矾 Agent 短期证书 + CRL 撤销）+ 文件路径穿越防护（白名单+黑名单）+ 命令安全策略（15 类高危命令拦截）+ token 常量时间比较 + 审计日志（append-only JSONL）
  - **可靠性**：bbolt 任务持久化（Relay 重启不丢任务）+ Agent 本地结果缓存重投 + 任务 ACK + 幂等执行（防重投重复）+ 任务取消
  - **并发安全**：Agent 并发写 WS 串行化（wsConn writeMu）+ Dispatcher 加锁 + Sweep 主动关连接防泄漏
  - **可观测性**：metrics 端点（`/api/metrics`：在线 agent/任务计数/uptime/goroutines）+ 任务历史查询（`/api/tasks`）
  - **功能增强**：标签批量下发（`POST /api/tasks/batch`）+ 任务取消（`POST /api/tasks/cancel`）+ 并发限流（worker pool，默认 4）
- **P2 深度优化**（at-least-once 真正成立）：
  - **致命漏洞修复**：批量下发绕过命令策略（D1，安全洞）+ 任务卡 dispatched 永不重投（D2，reaper 周期回退）+ CRL 重启丢失（D3，ca-state.json 持久化）+ 结果队列半开连接丢结果（D4，peek+逐条commit）+ pool 满丢任务无 NACK（D4，TrySubmit+task_nack）
  - **状态机**：终态（completed/failed/cancelled）锁死不可变更（D5，防 cancel/result 竞态漂移）+ store 并发关停安全（db==nil 检查）
  - **前端**：SSE 主动重连（D6）+ metrics 面板 + 取消按钮 + 重连补拉近期结果
  - **可观测性**：EventBus 慢消费者丢弃计数（D7）+ metrics 扩展至 13 项（cancelled/acked/nacked/reaped/events_dropped）+ 深度 healthz（检查 store 可用性）
  - **清理**：删除死代码 minGo122 + handleListAgents 双次 Snapshot 合并
- **P3 收尾加固**（可靠性声明无懈可击）：
  - **状态机原子性**：ACK 路径改走 UpdateTaskStatus（B1，防 cancel 后被 ACK 改回）+ UpdateTaskStatus 合并进单写事务（B2，bbolt 写事务串行保证读-校验-写原子，防 reaper 与 result 并发覆盖终态）
  - **maxAttempts 兜底**：MaxTaskAttempts=5，超过转 failed（W1，防 poison task 无限重投）
  - **enroll token 过期**：加 createdAt + 1h TTL 校验（W4，对齐前端 expires_in:3600，修复"撒谎 bug"）
  - **reaper 周期**：30s→15s（W2，降低短任务回退延迟）
  - **端到端护栏**：3 条核心链路 E2E 测试——reaper 回退闭环 / NACK 链路 / 状态机 vs reaper 交互（W3）
- **Perf 性能优化**（数据驱动）：
  - **状态索引**：ListTasksByStatus 走 bucketTasksByStatus 索引（F1，55x 延迟提升）
  - **命令策略预过滤**：PolicyCheck 用 strings.Contains 快速跳过正则（F2，28x 延迟提升）
  - **bbolt fsync**：评估后不改（8ms 是可靠性保证，125 写/s 远超 RMM 需求）
  - **性能基线**：BENCHMARK.md + 9 个 benchmark
- **P4 全局一致性修复**（文档与实现对齐）：
  - **mTLS 接线**：Agent 侧 enroll/mTLS 整条链接入（G1/A1，原为死代码）+ enroll URL 转换修复（G1/A3）
  - **CRL 校验**：Relay mTLS 加 VerifyPeerCertificate 钩子 + 撤销 API（G2/A2，原撤销完全失效）
  - **协议清理**：TypeEnrollResp/TypeError 标注为 HTTP/预留（G3/A4）
- **P5 功能扩展**（从"能执行"到"好用"）：
  - **文件分块传输**（H1）：chunk_upload/chunk_download，突破 10MB 限制（1MB/块）
  - **定时任务**（H2）：robfig/cron 调度引擎，规则持久化到 bbolt，REST API 管理
  - **交互式终端**（H3/H5）：方案C 协议（shell_open/input/output/close）+ SessionManager + REST + 前端
    - Agent 持久 shell 进程，stdout/stderr 经 WS 流式推送
    - 会话独立于任务状态机，连接断开自动清理
- **P6 大文件传输**（5GB 级 stream-through）：
  - **二进制 WS 帧**：去掉 base64 膨胀，直接传原始字节（省 33% 带宽 + CPU）
  - **stream-through 中继**：Console→Relay→Agent 管道直连，Relay 仅 256KB 缓冲不落盘
  - **SHA256 增量校验**：io.MultiWriter 流过即算，完成时比对
  - **断点续传**：.part 临时文件 + resume_offset，同 transfer_id 重协商
  - **进度推送**：Agent 周期 progress → SSE → Console（500ms 限流 + 速度估算）
  - **绕过 bbolt**：不经 handlePostTask/SaveTask，独立 REST 端点直接 WS 转发
- **已知限制**：
  - 文件任务限 10MB（WS 帧，P2 改分块流）
  - browse 执行器需 Chrome 环境（`-tags browse` 编译启用）
  - mTLS 需手动配置（`-mtls` + TLS 证书），默认向后兼容 ws+token
  - race detector 需 CGO（CI 环境装 gcc 后 `make test-race`）

## 技术栈与架构

- **语言**：Go 1.22+（开发机 go1.25.6，已验证）
- **核心依赖**（4 个生产依赖 + 1 个可选）：
  - `github.com/kardianos/service v1.3.0` —— Windows 服务注册/启停
  - `github.com/coder/websocket v1.8.15` —— 反向长连接（nhooyr 的官方继任者，gorilla 已归档）
  - `github.com/google/uuid v1.6.0` —— 消息/任务 ID
  - `go.etcd.io/bbolt v1.5.0` —— 任务持久化（P1 新增）
  - `github.com/chromedp/chromedp` —— browse 执行器（**可选**，build tag `browse` 隔离，默认不编译）
  - `golang.org/x/sys` —— Win32 API（PrintWindow/SendInput，替代 computer-use-rtc/win32.cs）
- **零依赖原则**：无 Web 框架（标准库 `net/http`）、无 ORM、无前端构建（原生 JS + `embed.FS`）

目录树：

```
go-rmm/
├── proto/          # 三组件共享协议（Envelope 信封 + 6 种任务载荷，零外部依赖）
├── relay/          # 组件1：中继服务器（WS连接池/REST/SSE/心跳清理）
├── agent/          # 组件2：Windows Agent（服务化/反向连接/重连/分发）
├── console/        # 组件3：Web 控制台（反向代理+embed.FS+原生JS单页）
├── executors/      # 6 类执行器 + Executor 接口 + Dispatcher 路由
│   ├── registry.go         # Executor 接口 + Dispatcher
│   ├── register.go         # RegisterAll 能力探测注册
│   ├── powershell.go       # UTF-8 强制 / exit code / 超时
│   ├── script.go           # 临时文件 + UTF-8 BOM
│   ├── file.go             # base64 上传下载（10MB 限）
│   ├── screenshot.go       # 调 win32.CaptureWindow
│   ├── guiclick.go         # 看-判-点-验 agentic 循环
│   └── browse*.go          # chromedp（build tag browse 隔离）
├── win32/          # Win32 API 封装（Go syscall 替代 win32.cs）
│   ├── win32.go            # 跨平台常量与类型
│   ├── *_windows.go        # Windows 真实实现（PrintWindow/SendInput/枚举）
│   └── stub_other.go       # 非 Windows stub
├── vision/         # GLM 视觉识别客户端（移植 vision.mjs）
└── scripts/        # install-agent.ps1 / demo-local.ps1
```

## 如何运行

```bash
# 同步依赖
make tidy

# 编译全部三组件到 bin/
make all

# 本地联调（三个终端）
make run-relay                    # 终端1：Relay 监听 :7780
make run-agent                    # 终端2：Agent 前台模式连本地 Relay
make console && bin/go-rmm-console # 终端3：控制台 http://localhost:7781

# Agent 安装为 Windows 服务（生产部署）
bin/go-rmm-agent.exe install -relay ws://your-relay:7780/agent
bin/go-rmm-agent.exe start

# 测试与检查
make test
make vet
```

## 关键约定

- **协议单一信封**：所有 WS/HTTP 消息用 `proto.Envelope` 包装，`Type` 字段区分，Payload 延迟解码（Relay 仅路由不关心载荷）。
- **Agent 主动外连**：穿透 NAT 的唯一方式是 Agent 发起反向 WS，Relay 不主动连 Agent。
- **服务化双模式**：`go-rmm-agent.exe install/start/stop/uninstall` 由 SCM 管理；`run` 子命令前台调试。
- **UTF-8 强制**：调 PowerShell 时前置 `[Console]::OutputEncoding=[Text.Encoding]::UTF8`；写 .ps1 含中文需 BOM。
- **PrintWindow 标志位**：截 Electron/Chromium 必须用 `PW_RENDERFULLCONTENT=0x2`，否则黑屏。
- **稳定性降级链**：每个执行器优先用最稳的方式，失败再降级（如 PrintWindow 失败降级 CopyFromScreen）。

## 与其他项目的关系

- **复用 `computer-use-rtc`**：`vision/glm.go` 移植 `vision.mjs` 的 GLM OpenAI 兼容调用与正则提取 JSON；`win32/` 用 Go syscall 替代 `win32.cs`（PrintWindow/SendInput 等价实现）；`executors/guiclick.go` 复用 `cu.mjs` 的"看-判-点-验"循环范式。**不直接调 PowerShell 脚本**，改 Go 原生实现以摆脱 BOM/编码/进程开销。
- **不耦合 `agentloop`/`cogent`**：P0 是确定性任务执行平台。P2 可选接入 agentloop 作为"自主决策层"，让平台升级为 computer-use agent。
- **工作区首个 Go 项目**：本工作区此前全为 TS/Python/Dart/Lean/Godot，go-rmm 是 Go 技术栈的开篇。

## 技术选型理由（关键决策）

| 决策点 | 选择 | 理由 |
|--------|------|------|
| WS 库 | `coder/websocket` | nhooyr 已废弃重定向至此，gorilla 已归档；coder 是当前唯一活跃维护且 Go 官方背书 |
| Web 框架 | 无（标准库） | 保持单二进制部署优势，RMM 场景路由简单无需框架 |
| 服务化 | `kardianos/service` | Go 社区事实标准，跨平台，API 极简 |
| 浏览器自动化 | `chromedp`（build tag 隔离） | 事实标准、纯 Go 无外部依赖；build tag 让无 Chrome 环境可跳过编译 |
| 计划任务 | 暂不做（P1） | P0 六种任务已覆盖核心，scheduled 留 P1 |
| 前端 | 原生 JS + embed.FS | 无构建步骤，单二进制内嵌，控制台与 Relay 可合并部署 |
