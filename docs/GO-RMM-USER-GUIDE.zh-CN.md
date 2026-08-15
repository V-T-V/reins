# go-rmm 完整使用与 Token 管理指南

> 适用代码目录：`D:\M_X_M\go-rmm`  
> 校对日期：2026-08-13  
> 本文以当前代码和 `go-rmm.exe --help` 为准。仓库中的 README、AGENTS 与现有二进制存在版本号漂移时，以实际部署二进制的 `version` 输出和 API 行为为准。

## 1. 系统是什么

go-rmm 由三个核心组件组成：

```text
操作端 CLI / Web 控制台
          │ HTTPS / SSE
          ▼
       Relay 中继
          ▲
          │ Agent 主动建立反向 WebSocket / WSS
          │
   Windows Agent 服务
```

- **Relay**：保存任务、管理在线 Agent、转发命令和文件、提供 Web 控制台与 API。
- **Agent**：安装在被控 Windows 主机，主动连接 Relay，因此不要求被控机开放入站端口。
- **CLI**：运维人员使用的命令行工具，调用 Relay API。
- **Web 控制台**：浏览器管理界面，推荐使用独立控制台密码登录，不在浏览器保存 Relay Token。

默认生产拓扑是“一台 Relay + 多台 Agent”。Agent 必须能出站访问 Relay 的 WSS 地址。

## 2. 四类凭据不要混用

| 凭据 | 用途 | 配置位置 | 生命周期 |
|---|---|---|---|
| Relay Token | CLI Bearer 鉴权；非 mTLS Agent 连接鉴权 | Relay `-token` / `GO_RMM_RELAY_TOKEN`；CLI `--token` / `GO_RMM_TOKEN`；Agent `config.json` | 长期，需主动轮换 |
| Console Password | 浏览器登录并换取 HttpOnly Session Cookie | Relay `-console-password` / `GO_RMM_CONSOLE_PASSWORD` | 长期，但不传给 Agent |
| Metrics Token | Prometheus `/metrics` 专用 | Relay `-metrics-token` / `GO_RMM_METRICS_TOKEN` | 长期，建议与主 Token 分离 |
| Enrollment Token | mTLS Agent 首次申请客户端证书 | 管理员调用 `agent enroll-token` 临时生成 | 一次性，默认 3600 秒 |

重要结论：

- 普通部署中，Agent 与 CLI 使用的是同一个 **Relay Token**。
- Web 控制台应使用 **Console Password** 登录，而不是把 Relay Token 放到 URL、localStorage 或网页脚本。
- `agent enroll-token` 生成的是 mTLS 首次注册令牌，不是普通 Agent 的长期连接 Token。
- Relay Token 为空且非 `-dev` 时，Relay 会自动生成并保存；生产不要显式使用空 Token。

## 3. Relay Token 如何获取

### 3.1 自动生成 Token 的权威位置

当 Relay 启动时没有设置 `-token` 或 `GO_RMM_RELAY_TOKEN`，且不是 `-dev` 模式，程序会生成 16 个随机字节并编码为 32 位十六进制字符串，保存在：

```text
<Relay 可执行文件目录>\data\.token
```

Linux 示例；此方法会把 Token 打印到当前终端：

```bash
pid=$(systemctl show -p MainPID --value go-rmm-relay)
exe=$(readlink -f "/proc/$pid/exe")
sudo cat "$(dirname "$exe")/data/.token"
```

如果安装在 `/opt/go-rmm/go-rmm-relay`，通常是：

```bash
sudo cat /opt/go-rmm/data/.token
```

Windows Relay 示例：

```powershell
$proc = Get-CimInstance Win32_Process -Filter "Name='go-rmm-relay.exe'"
$relayExe = [string]$proc.ExecutablePath
Get-Content -LiteralPath (Join-Path (Split-Path $relayExe -Parent) 'data\.token') -Raw
```

### 3.2 显式 Token 优先，`.token` 可能不是当前值

配置优先级是：

```text
命令行 -token > GO_RMM_RELAY_TOKEN > 自动生成的 data/.token
```

如果服务使用了 `-token` 或环境变量，必须从服务配置中读取；此时 `.token` 文件可能是旧值。

Linux systemd：

```bash
sudo systemctl cat go-rmm-relay
sudo systemctl show go-rmm-relay -p ExecStart -p Environment --no-pager
```

Docker：

```bash
docker inspect go-rmm-relay \
  --format '{{range .Config.Env}}{{println .}}{{end}}' \
  | grep '^GO_RMM_RELAY_TOKEN='
```

Kubernetes 的 Token 应来自 Secret，而不是 Pod 日志。先找 Deployment 引用的 Secret，再读取对应键：

```bash
kubectl -n <namespace> get deploy <relay-deployment> -o yaml
kubectl -n <namespace> get secret <secret-name> -o jsonpath='{.data.GO_RMM_RELAY_TOKEN}' | base64 -d
```

### 3.3 已经持有有效凭据时，通过 Bootstrap 核对

`GET /api/bootstrap` 需要先通过 Relay 鉴权。它可用于核对外部 URL、Agent WSS 地址和当前 Token，但不能绕过鉴权“找回”未知 Token。

PowerShell：

```powershell
$relay = 'https://203.0.113.99/rmm'
$token = '<已有的有效 Relay Token>'
$headers = @{ Authorization = "Bearer $token" }
$bootstrap = Invoke-RestMethod "$relay/api/bootstrap" -Headers $headers
$bootstrap | Select-Object server_url, relay_url, agent_binary_ready
# 明确需要显示时才执行：$bootstrap.token
# 一键安装命令会包含 Token：$bootstrap.one_line_cmd
```

CLI：

```powershell
.\go-rmm.exe --relay https://203.0.113.99/rmm --token '<TOKEN>' bootstrap
```

### 3.4 mTLS 一次性注册 Token

前提：Relay 必须启用 `-mtls`，内置 CA 初始化成功，并且调用方持有管理员 Relay Token。

当前实现还有一个必须正视的首次注册限制：`-mtls` 会让 Relay 的同一个 HTTPS 监听器对所有请求强制要求客户端证书，但 `/api/enroll` 又承担“尚无证书的 Agent 首次换证”职责。因此，全新环境不能只靠下面两条命令完成从零注册。现阶段生产有三种选择：

1. 继续使用 Nginx TLS/WSS + 强 Relay Token，这是当前公网环境采用的路径；
2. 通过安全的带外方式预置 Agent 客户端证书与 CA；
3. 在代码中拆分独立、严格限流的一次性 Enrollment 监听器后，再启用完整 mTLS 自动注册。

下面命令仅适用于已经解决上述引导条件、且管理员客户端本身能够访问 mTLS Relay 的环境：

```powershell
$env:GO_RMM_RELAY = 'https://relay.example.com/rmm'
$env:GO_RMM_TOKEN = '<管理员 Relay Token>'
.\go-rmm.exe agent enroll-token agent-win-prod-01
```

输出的注册 Token：

- 绑定指定 Agent ID；
- 默认 3600 秒过期；
- 成功换取证书后立即失效；
- 不能作为 CLI 长期 Token 使用。

目标机首次注册示例：

```powershell
.\go-rmm-agent.exe run `
  -relay 'wss://relay.example.com/rmm/agent' `
  -id 'agent-win-prod-01' `
  -mtls `
  -token '<一次性 Enrollment Token>'
```

## 4. 构建与安装

### 4.1 从源码构建

```powershell
Set-Location D:\M_X_M\go-rmm
$env:CGO_ENABLED = '0'
go build -trimpath -ldflags '-s -w' -o bin\go-rmm.exe .\cmd\go-rmm
go build -trimpath -ldflags '-s -w' -o bin\go-rmm-agent.exe .\agent
go build -trimpath -ldflags '-s -w' -o bin\go-rmm-relay.exe .\relay
go test ./...
```

生产构建应使用 `-X main.version=<版本>` 注入版本号：

```powershell
$version = 'v4.5.0-prod.1'
go build -trimpath -ldflags "-s -w -X main.version=$version" -o bin\go-rmm.exe .\cmd\go-rmm
go build -trimpath -ldflags "-s -w -X main.version=$version" -o bin\go-rmm-agent.exe .\agent
go build -trimpath -ldflags "-s -w -X main.version=$version" -o bin\go-rmm-relay.exe .\relay
```

### 4.2 Relay 最小生产启动

优先通过服务环境注入密钥，避免 Token 出现在 shell history 和进程参数中：

```bash
export GO_RMM_RELAY_ADDR='127.0.0.1:7780'
export GO_RMM_RELAY_TOKEN='<强随机 Relay Token>'
export GO_RMM_CONSOLE_PASSWORD='<独立控制台密码>'
export GO_RMM_EXTERNAL_URL='https://relay.example.com/rmm'
exec /opt/go-rmm/go-rmm-relay
```

生成 32 字节随机 Token：

```bash
openssl rand -hex 32
```

PowerShell 等价方式：

```powershell
$bytes = [byte[]]::new(32)
[Security.Cryptography.RandomNumberGenerator]::Fill($bytes)
$token = -join ($bytes | ForEach-Object { $_.ToString('x2') })
$token
```

### 4.3 Nginx 子路径反向代理

外部地址为 `https://relay.example.com/rmm/` 时，需要同时代理 HTTP、SSE 和 WebSocket：

```nginx
location /rmm/ {
    proxy_pass http://127.0.0.1:7780/;
    proxy_http_version 1.1;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection "upgrade";
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_buffering off;
    proxy_request_buffering off;
    proxy_read_timeout 86400s;
    client_max_body_size 0;
}
```

Relay 必须配置最终外部地址：

```text
-external-url https://relay.example.com/rmm
```

## 5. Windows Agent 部署

### 5.1 获取一键安装命令

在安全的管理员终端中调用 Bootstrap：

```powershell
$relay = 'https://relay.example.com/rmm'
$token = '<Relay Token>'
$info = Invoke-RestMethod "$relay/api/bootstrap" -Headers @{Authorization="Bearer $token"}
if (-not $info.agent_binary_ready) { throw 'Relay 尚未托管 Agent 二进制' }
$info.one_line_cmd
```

复制输出的一行命令，在目标 Windows 主机的管理员 PowerShell 中执行。该命令含 Token，不应发到群聊、工单或公开日志。

### 5.2 手工安装服务

```powershell
$install = 'C:\Program Files\go-rmm'
New-Item -ItemType Directory -Force $install | Out-Null
Copy-Item .\go-rmm-agent.exe "$install\go-rmm-agent.exe"

& "$install\go-rmm-agent.exe" install `
  -relay 'wss://relay.example.com/rmm/agent' `
  -token '<Relay Token>' `
  -id 'agent-win-prod-01'

& "$install\go-rmm-agent.exe" start
& "$install\go-rmm-agent.exe" status
```

Agent 服务配置文件位于可执行文件同目录：

```text
C:\Program Files\go-rmm\config.json
```

生产建议配置：

```json
{
  "agent_id": "agent-win-prod-01",
  "relay_url": "wss://relay.example.com/rmm/agent",
  "token": "<Relay Token>",
  "tags": {
    "env": "prod",
    "role": "application"
  },
  "heartbeat": 15000000000,
  "workspace": "C:\\go-rmm\\workspace",
  "allow_all_roots": false,
  "allowed_roots": [
    "C:\\go-rmm\\workspace",
    "D:\\LocalVoiceAssistant"
  ],
  "max_concurrent_tasks": 4,
  "role": "slave",
  "service_name": "go-rmm-slave"
}
```

注意：Go 的 `time.Duration` JSON 默认使用纳秒整数；15 秒是 `15000000000`。如果由安装脚本生成配置，以脚本结果为准。

### 5.3 前台诊断

服务无法上线时，先停止服务，再以前台模式查看真实错误：

```powershell
& 'C:\Program Files\go-rmm\go-rmm-agent.exe' stop
& 'C:\Program Files\go-rmm\go-rmm-agent.exe' run `
  -relay 'wss://relay.example.com/rmm/agent' `
  -token '<Relay Token>' `
  -id 'agent-win-prod-01'
```

## 6. CLI 配置

全局参数：

| 参数 | 环境变量 | 说明 |
|---|---|---|
| `--relay` | `GO_RMM_RELAY` | Relay HTTP/HTTPS 根地址 |
| `--token` | `GO_RMM_TOKEN` | Relay Bearer Token |
| `-o json` / `--output json` | `GO_RMM_OUTPUT` | 机器可读 JSON |
| `--insecure` | — | 跳过 TLS 校验，仅临时诊断 |
| `--cacert <path>` | — | 指定可信自签 CA，优先于 `--insecure` |

PowerShell 当前会话配置：

```powershell
$env:GO_RMM_RELAY = 'https://203.0.113.99/rmm'
$env:GO_RMM_TOKEN = '<Relay Token>'
.\go-rmm.exe status
.\go-rmm.exe agent list
```

不要优先使用 `setx GO_RMM_TOKEN ...`，因为它会把长期 Token 持久化到用户环境。CI/CD 应使用流水线 Secret，交互式运维应只设置当前进程环境变量。

退出当前 PowerShell 前清理：

```powershell
Remove-Item Env:GO_RMM_TOKEN -ErrorAction SilentlyContinue
```

CLI 退出码：

| 退出码 | 含义 |
|---:|---|
| 0 | 成功 |
| 1 | 通用错误 |
| 2 | 远程任务失败 |
| 3 | 超时 |
| 4 | 资源未找到 |
| 5 | 被安全策略拦截 |

## 7. 日常使用命令

以下命令默认已设置 `GO_RMM_RELAY` 与 `GO_RMM_TOKEN`。

### 7.1 状态与 Agent

```powershell
.\go-rmm.exe status
.\go-rmm.exe -o json agent list
.\go-rmm.exe sysinfo -a agent-win-prod-01
.\go-rmm.exe topology
.\go-rmm.exe diagnose --tag env=prod
.\go-rmm.exe compare agent-win-prod-01 agent-win-prod-02
```

### 7.2 下发命令

```powershell
# 普通 PowerShell 任务
.\go-rmm.exe task run -a agent-win-prod-01 -c 'Get-Date'

# 指定超时
.\go-rmm.exe task run -a agent-win-prod-01 -c 'Get-Process' -timeout 120

# SSE 实时输出
.\go-rmm.exe task stream -a agent-win-prod-01 -c 'ping -t 127.0.0.1'

# 多 Agent 并行
.\go-rmm.exe exec -a agent-01,agent-02 -c 'hostname'

# 交互式终端
.\go-rmm.exe shell open -a agent-win-prod-01
```

生产脚本应使用 JSON 输出并检查退出码：

```powershell
$result = .\go-rmm.exe -o json task run -a agent-win-prod-01 -c 'Get-Date'
if ($LASTEXITCODE -ne 0) { throw "远程任务失败：$result" }
$result | ConvertFrom-Json
```

### 7.3 文件和目录

```powershell
# 上传：本地 → Agent
.\go-rmm.exe file upload -a agent-win-prod-01 .\package.zip 'C:\go-rmm\workspace\package.zip'

# 下载：Agent → 本地
.\go-rmm.exe file download -a agent-win-prod-01 'C:\go-rmm\workspace\result.log' .\result.log

# 目录同步
.\go-rmm.exe sync push -a agent-win-prod-01 .\release 'C:\go-rmm\workspace\release'
.\go-rmm.exe sync pull -a agent-win-prod-01 'C:\go-rmm\workspace\logs' .\logs
```

Agent 会按 `allowed_roots` 检查路径。遇到 403 时：

```powershell
.\go-rmm.exe config agent-config -a agent-win-prod-01
```

不要为了省事把生产 Agent 长期设置为 `allow_all_roots=true`。

### 7.4 端口转发

把本机 `13306` 转发到 Agent 所能访问的 `db.internal:3306`：

```powershell
.\go-rmm.exe tunnel -L 13306:db.internal:3306 -a agent-win-prod-01
```

CLI 优先使用 WebSocket 全双工隧道，握手不兼容时回退到旧 HTTP 通道。保持当前终端运行；Ctrl+C 关闭隧道。

### 7.5 诊断、日志和搜索

```powershell
.\go-rmm.exe net ping -a agent-win-prod-01 8.8.8.8
.\go-rmm.exe net dns -a agent-win-prod-01 example.com
.\go-rmm.exe net trace -a agent-win-prod-01 example.com
.\go-rmm.exe net ports -a agent-win-prod-01

.\go-rmm.exe logs tail -a agent-win-prod-01 'D:\app\logs\app.log'
.\go-rmm.exe find name -a agent-win-prod-01 '*.log'
.\go-rmm.exe find grep -a agent-win-prod-01 'ERROR'
```

不同版本的小命令参数可能变化；以 `go-rmm <分组> <子命令> --help` 和当前 `--help` 为准。

### 7.6 服务、进程、电源与守护

```powershell
.\go-rmm.exe service list -a agent-win-prod-01
.\go-rmm.exe service start -a agent-win-prod-01 MyService
.\go-rmm.exe service stop -a agent-win-prod-01 MyService
.\go-rmm.exe service restart -a agent-win-prod-01 MyService

.\go-rmm.exe proc list -a agent-win-prod-01 --sort cpu -n 20
.\go-rmm.exe proc kill -a agent-win-prod-01 1234
.\go-rmm.exe proc kill -a agent-win-prod-01 notepad

.\go-rmm.exe watch add -a agent-win-prod-01 --svc MyService
.\go-rmm.exe watch list -a agent-win-prod-01
```

关机/重启属于高风险操作，并受标签与策略限制：

```powershell
.\go-rmm.exe power restart -a agent-win-prod-01 --yes
```

### 7.7 监控、事件与快照

```powershell
.\go-rmm.exe monitor top -a agent-win-prod-01
.\go-rmm.exe monitor gpu -a agent-win-prod-01
.\go-rmm.exe events
.\go-rmm.exe snapshot take -a agent-win-prod-01
.\go-rmm.exe inventory --format csv
.\go-rmm.exe history -n 100
```

### 7.8 部署编排

先校验，再执行：

```powershell
.\go-rmm.exe deploy validate .\deploy.json
.\go-rmm.exe deploy run .\deploy.json
```

部署原则：

1. 上传到 Agent 的 `workspace`；
2. 校验 SHA-256；
3. 创建独立备份；
4. 执行精确覆盖；
5. 启动并验证健康；
6. 失败自动回滚；
7. 使用幂等键避免重复执行。

### 7.9 主从救援

双 Agent 模式下，Master 保持稳定，Slave 承担业务和更新：

```powershell
.\go-rmm.exe rescue status -a <master-agent-id>
.\go-rmm.exe rescue restart -a <master-agent-id>
.\go-rmm.exe rescue rollback -a <master-agent-id>
.\go-rmm.exe rescue update -a <slave-agent-id> -v <version>
```

Master 的 `config.json` 必须正确配置 `role=master`、`slave_exe_dir` 和 `slave_agent_id`。

## 8. Web 控制台

访问：

```text
https://relay.example.com/rmm/
```

推荐配置：

```text
GO_RMM_CONSOLE_PASSWORD=<独立强密码>
GO_RMM_CONSOLE_SESSION_TTL=8h
```

登录成功后 Relay 颁发 HttpOnly、SameSite Session Cookie。浏览器 API 和 SSE 都使用 Cookie，不需要也不应该把 Relay Token 写入网页。

控制台显示“在线 Agent 0”时，按顺序检查：

1. 浏览器是否已经登录，而不是 API 返回 401；
2. CLI `agent list` 是否同样为 0；
3. Agent Windows 服务是否 Running；
4. Agent `config.json` 的 `relay_url` 是否为 `/rmm/agent`；
5. Token 是否与 Relay 当前 Token 完全一致；
6. Nginx 是否转发 WebSocket Upgrade/Connection；
7. 主机时间、TLS 证书与 DNS 是否正常；
8. 前台运行 Agent 查看 401、TLS 或网络错误。

## 9. API 调用

Bearer 方式：

```powershell
$relay = 'https://relay.example.com/rmm'
$headers = @{ Authorization = 'Bearer <Relay Token>' }
Invoke-RestMethod "$relay/api/agents" -Headers $headers
```

curl：

```bash
curl -H "Authorization: Bearer $GO_RMM_TOKEN" \
  "$GO_RMM_RELAY/api/agents"
```

Token 也可通过 `?token=` 查询参数传递，但 URL 容易进入代理日志、浏览器历史和监控系统，生产禁止使用；仅 WebSocket 客户端兼容路径可能需要它。

## 10. Token 轮换

当前实现的主 Bearer Token 是单值校验，不支持新旧 Token 并行接受。因此轮换需要维护窗口，不能宣称无缝滚动。

推荐流程：

1. 生成新的 32 字节随机 Token；
2. 备份 Relay 服务配置和 Agent `config.json`；
3. 把所有非 mTLS Agent 的配置文件更新为新 Token，但暂不依赖旧连接执行后续步骤；
4. 配置新的 `GO_RMM_RELAY_TOKEN`；
5. 重启 Relay；
6. 重启全部 Agent，使其重新读取配置；
7. 用新 Token 执行 `status`、`agent list` 和测试任务；
8. 确认旧 Token 返回 401；
9. 清理终端历史、临时脚本、CI 日志中的旧 Token。

如果 Agent 数量多，应先通过 Master/本地计划任务准备重启兜底，避免修改 Token 后失去远程控制通道。

## 11. 安全基线

- Relay 只监听 `127.0.0.1:7780`，公网由 Nginx/Traefik 提供 HTTPS/WSS。
- Relay Token、Console Password、Metrics Token 三者不同。
- Token 至少使用 32 随机字节；不要使用手机号、主机名或普通密码。
- 禁止在 Git、README、部署 zip、截图、工单和聊天群中保存真实 Token。
- Agent `config.json` 含敏感信息，应限制为 Administrators 和 SYSTEM 可读。
- 生产 Agent 设置 `allow_all_roots=false` 并使用最小 `allowed_roots`。
- `/metrics` 使用独立低权限 Token或 Nginx IP 白名单；`pprof` 默认关闭。
- 高危电源、更新和进程操作使用标签、RBAC 和审计。
- 定期备份 Relay `data/tasks.db`、`data/audit.log`、CA/CRL 与 Agent 版本仓库。
- 任何已经发到聊天、日志或命令行截图中的 Token 都应视为已暴露并安排轮换。

## 12. 故障排查速查

### Relay 可达但 Agent 为 0

```powershell
.\go-rmm.exe -o json status
.\go-rmm.exe -o json agent list
Get-Service go-rmm-agent,go-rmm-slave,go-rmm-master -ErrorAction SilentlyContinue
```

Agent 前台日志重点看：

- `401`：Token 不一致；
- `x509`：证书链、主机名或系统时间异常；
- `websocket`：反代未升级连接或路径错误；
- `connection refused`：Relay/Nginx 未监听；
- `context deadline exceeded`：网络、防火墙、代理或 Relay 过载。

### CLI 401

```powershell
"Relay=$env:GO_RMM_RELAY"
"TokenLength=$($env:GO_RMM_TOKEN.Length)"
```

不要直接打印 Token。确认没有前后空格、换行、引号，也不要把 Console Password 当成 Relay Token。

### 文件上传 403

检查 Agent `allowed_roots` 和 `workspace`，不要直接关闭路径保护：

```powershell
.\go-rmm.exe config agent-config -a <agent-id>
```

### 隧道已就绪但无数据

1. 更新到含 tunnel 首包竞态修复的 Relay/CLI/Agent；
2. 确认目标地址是从 Agent 视角可达；
3. 检查本机代理是否错误代理 `127.0.0.1`；
4. 检查 Nginx 的 WebSocket 与长连接超时。

## 13. 当前生产地址模板

当前环境使用：

```powershell
$env:GO_RMM_RELAY = 'https://203.0.113.99/rmm'
$env:GO_RMM_TOKEN = '<从安全渠道获取的当前 Token>'

.\go-rmm.exe status
.\go-rmm.exe agent list
```

Agent 地址：

```text
wss://203.0.113.99/rmm/agent
```

真实 Token 不写入本指南。获取后只在当前终端、受控 Secret 管理器或 Agent ACL 保护的 `config.json` 中使用。
