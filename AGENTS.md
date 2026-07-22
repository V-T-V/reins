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

- **完成度**：P0 开发中（~15%，骨架+协议层完成，三组件实现中）
- **已落地**：`proto/message.go` 协议层（Envelope/TaskRequest/TaskResult 等 9 个类型 + 6 种任务载荷）、go.mod 依赖骨架
- **待落地**：Relay 中继服务器、Agent 服务化与反向连接、6 个执行器、Web 控制台
- **已知限制**（预设）：文件任务 P0 限 10MB（WS 帧限制）；browse 执行器需 Chrome 环境；guiclick 需 GLM API key

## 技术栈与架构

- **语言**：Go 1.22+（开发机 go1.25.6）
- **核心依赖**：
  - `github.com/kardianos/service` —— Windows 服务注册/启停
  - `github.com/coder/websocket` —— 反向长连接（nhooyr 的官方继任者，gorilla 已归档）
  - `github.com/google/uuid` —— 消息/任务 ID
  - `github.com/chromedp/chromedp` —— browse 执行器（build tag `browse` 隔离）
  - `golang.org/x/sys/windows` —— Win32 API（PrintWindow/SendInput，替代 computer-use-rtc/win32.cs）
- **零依赖原则**：无 Web 框架（标准库 `net/http`）、无 ORM、无前端构建（原生 JS + `embed.FS`）

目录树：

```
go-rmm/
├── proto/          # 三组件共享协议（Envelope 信封 + 6 种任务载荷）
├── relay/          # 组件1：中继服务器（go-rmm-relay）
├── agent/          # 组件2：Windows Agent（go-rmm-agent.exe）
├── console/        # 组件3：Web 控制台（go-rmm-console）
├── executors/      # 6 类任务执行器
├── win32/          # Win32 API 封装（PrintWindow/SendInput）
├── vision/         # GLM 视觉识别客户端（移植 computer-use-rtc/vision.mjs）
├── cmd/            # 公共 CLI 工具
└── scripts/        # 安装/联调脚本
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
