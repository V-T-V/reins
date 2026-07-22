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

- **完成度**：**P0 完成（约 90%）**——三组件全链路打通，6 类执行器实现，端到端联调通过
- **代码规模**：32 个 Go 文件，约 3500 行（不含生成的 go.sum）；9 个测试全绿
- **已落地**：
  - **协议层**（`proto/`）：Envelope 信封 + 9 种消息类型 + 6 种任务载荷，纯 Go struct 零依赖
  - **Relay 中继**（`relay/`）：`coder/websocket` WS 连接池 + Agent 注册表（conn 匹配保护）+ REST API（`GET /api/agents`、`POST /api/tasks`）+ SSE 实时事件流（`GET /api/events`）+ 心跳超时清理
  - **Agent**（`agent/`）：`kardianos/service` 双模式（install/uninstall/start/stop/run/status）+ 反向 WS 连接 + 15s 心跳 + 指数退避重连（1s→30s 封顶）+ 任务分发框架
  - **6 类执行器**（`executors/`）：powershell（强制 UTF-8）/script（BOM 处理）/file（base64 往返）/screenshot/guiclick（看-判-点-验循环）/browse（chromedp，build tag 隔离）
  - **Win32 封装**（`win32/`）：Go syscall 替代 computer-use-rtc/win32.cs——PrintWindow+PW_RENDERFULLCONTENT（截 Chromium）+ SendInput（绝对坐标归一化）+ 窗口枚举/查找/激活；非 Windows 平台 stub 保证跨平台编译
  - **视觉识别**（`vision/`）：移植 computer-use-rtc/vision.mjs——GLM-5-Turbo OpenAI 兼容 API + data URL 内联 + 正则容错提取 JSON
  - **Web 控制台**（`console/`）：无状态反向代理 + `embed.FS` 内嵌单页 + 原生 JS 零构建（Agent 列表/任务表单动态渲染/结果流/截图预览）
- **端到端验证**：浏览器 → Console(7781) → 反向代理 → Relay(17780) → WS 路由 → Agent PowerShell 执行 → SSE 结果回流，中文无损
- **已知限制**：
  - 文件任务 P0 限 10MB（WS 帧限制，P1 改分块流）
  - browse 执行器需 Chrome 环境（默认 stub，`-tags browse` 编译启用）
  - guiclick 需 GLM API key（未配置则该能力不上报）
  - TLS/P1：Relay 当前仅 HTTP，公网部署需前置 TLS 或用 Tailscale

## 技术栈与架构

- **语言**：Go 1.22+（开发机 go1.25.6，已验证）
- **核心依赖**（3 个生产依赖 + 1 个可选）：
  - `github.com/kardianos/service v1.3.0` —— Windows 服务注册/启停
  - `github.com/coder/websocket v1.8.15` —— 反向长连接（nhooyr 的官方继任者，gorilla 已归档）
  - `github.com/google/uuid v1.6.0` —— 消息/任务 ID
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
