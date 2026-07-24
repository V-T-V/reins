# go-rmm

> Go 实现的远程自动化执行操作平台——通过反向 WebSocket 中继，让 Web 控制台远程驱动 Windows Agent 执行 6 类任务。

## 这是什么

一个三组件的远程操作平台，解决"如何远程自动化操控另一台 Windows"的问题：

| 组件 | 产出 | 职责 |
|------|------|------|
| **Relay** | `go-rmm-relay` | 公网/局域网中继服务器，管理 Agent 连接池，路由任务与结果 |
| **Agent** | `go-rmm-agent.exe` | Windows 常驻服务，反向连接 Relay，执行任务 |
| **Console** | ~~独立进程~~ | 已合并进 Relay（N1），浏览器直接访问 Relay 端口 |

```
浏览器 ──HTTP/SSE──► Relay(内嵌Web UI) ◄──反向WS── Windows Agent(服务化)
                                          ├─ powershell/script/file/screenshot
                                          ├─ browse/guiclick
                                          ├─ shell（交互终端）
                                          └─ transfer（5GB大文件传输）
```

## 快速部署（零配置）

```bash
# Linux 服务端（1行，零参数零token）
./go-rmm-relay-linux

# Windows 被控端（管理员，1行，只填IP）
go-rmm-agent.exe install -relay 192.168.1.100

# 浏览器打开
http://192.168.1.100:7780
```

## 为什么用 Go

- **单文件部署**：编译成单二进制，无运行时依赖（vs Node.js 需 node + node_modules）
- **服务化原生**：`kardianos/service` 一行注册为 Windows 服务，开机自启（vs Python 需 pywin32）
- **交叉编译**：`GOOS=windows go build` 一条命令产出 Windows 可执行文件
- **并发模型**：goroutine 天然适合"管理多 Agent 连接 + 并发执行任务"

Go 做**控制平面**（通信/调度/服务化），数据平面（具体点击/识别）委托给更强工具：PowerShell 调 Win32、chromedp 调浏览器、云端视觉模型。

## 快速开始

```bash
# Linux 服务端（零参数零token）
./go-rmm-relay-linux

# Windows 被控端（管理员，只填IP）
go-rmm-agent.exe install -relay 192.168.1.100

# 浏览器打开
http://192.168.1.100:7780
```

## 生产部署（Linux 服务端 + Windows Agent）

```bash
# ① Linux 服务器
scp go-rmm-relay-linux user@server:/opt/
ssh user@server "chmod +x /opt/go-rmm-relay-linux && /opt/go-rmm-relay-linux"

# 可选：注册 systemd 开机自启
cat > /etc/systemd/system/go-rmm-relay.service << 'EOF'
[Unit]
Description=go-rmm Relay
After=network.target
[Service]
ExecStart=/opt/go-rmm-relay-linux
Restart=always
[Install]
WantedBy=multi-user.target
EOF
systemctl enable --now go-rmm-relay
```

```powershell
# ② Windows 被控机器（管理员 PowerShell）
.\go-rmm-agent.exe install -relay 服务器IP
# 自动安装为服务 + 启动 + 连接

# 管理
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

## 状态

P7 完成（零配置部署：Relay 内嵌 Web UI + 裸 IP 自动补全 + 安装引导页）。详见 [`AGENTS.md` 当前情况](./AGENTS.md#当前情况status)。
