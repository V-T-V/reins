# go-rmm

> Go 实现的远程自动化执行操作平台——通过反向 WebSocket 中继，让 Web 控制台远程驱动 Windows Agent 执行 6 类任务。

## 这是什么

一个三组件的远程操作平台，解决"如何远程自动化操控另一台 Windows"的问题：

| 组件 | 产出 | 职责 |
|------|------|------|
| **Relay** | `go-rmm-relay` | 公网/局域网中继服务器，管理 Agent 连接池，路由任务与结果 |
| **Agent** | `go-rmm-agent.exe` | Windows 常驻服务，反向连接 Relay，执行下发的 6 类任务 |
| **Console** | `go-rmm-console` | Web 控制台，浏览器下发任务、查看实时结果 |

```
浏览器控制台 ──HTTP/SSE──► Relay ◄──反向WS── Windows Agent(服务化)
                                      ├─ powershell  执行 PowerShell 命令
                                      ├─ script      执行下发脚本
                                      ├─ file        文件上传/下载
                                      ├─ screenshot  窗口/全屏截图
                                      ├─ browse      chromedp 浏览器自动化
                                      └─ guiclick    视觉模型+SendInput 桌面点击
```

## 为什么用 Go

- **单文件部署**：编译成单二进制，无运行时依赖（vs Node.js 需 node + node_modules）
- **服务化原生**：`kardianos/service` 一行注册为 Windows 服务，开机自启（vs Python 需 pywin32）
- **交叉编译**：`GOOS=windows go build` 一条命令产出 Windows 可执行文件
- **并发模型**：goroutine 天然适合"管理多 Agent 连接 + 并发执行任务"

Go 做**控制平面**（通信/调度/服务化），数据平面（具体点击/识别）委托给更强工具：PowerShell 调 Win32、chromedp 调浏览器、云端视觉模型。

## 快速开始

```bash
# 编译全部三组件
make all

# 终端1：启动中继服务器
bin/go-rmm-relay -addr :7780

# 终端2：Agent 前台调试模式（自动连本地 Relay）
bin/go-rmm-agent.exe run -relay ws://127.0.0.1:7780/agent

# 终端3：Web 控制台
bin/go-rmm-console -addr :7781 -relay http://127.0.0.1:7780
# 浏览器打开 http://localhost:7781
```

## 生产部署

```powershell
# 被控 Windows：安装 Agent 为服务
.\go-rmm-agent.exe install -relay wss://relay.example.com/agent -token <your-token>
.\go-rmm-agent.exe start

# 查看状态/卸载
.\go-rmm-agent.exe status
.\go-rmm-agent.exe stop
.\go-rmm-agent.exe uninstall
```

## 任务协议

所有消息用统一的 `Envelope` 信封，详见 [`proto/message.go`](./proto/message.go)。任务下发示例：

```json
{
  "task_id": "t-001",
  "type": "powershell",
  "agent_id": "agent-win01",
  "payload": { "command": "Get-Process | Select-Object -First 5" },
  "timeout": 30
}
```

## 技术栈

| 层 | 选型 |
|----|------|
| WebSocket | `coder/websocket`（nhooyr 继任者） |
| 服务化 | `kardianos/service` |
| Win32 API | `golang.org/x/sys/windows` |
| 浏览器自动化 | `chromedp`（build tag 隔离） |
| Web | 标准库 `net/http` + `embed.FS` + 原生 JS |

## 文档

- [`DEPLOYMENT.md`](./DEPLOYMENT.md) —— **生产部署运维指南**（内网/公网/mTLS/服务化/故障排查）
- [`BENCHMARK.md`](./BENCHMARK.md) —— **性能基线**（关键路径 benchmark + 优化记录 + 回归门禁）
- [`AGENTS.md`](./AGENTS.md) —— 完整架构与开发约定
- [`proto/message.go`](./proto/message.go) —— 通信协议定义

## 状态

P4 完成（6 轮优化：基础→安全可靠→漏洞修复→收尾加固→性能→一致性修复）。详见 [`AGENTS.md` 当前情况](./AGENTS.md#当前情况status)。
