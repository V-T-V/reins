# go-rmm 部署运维指南

> 从零到生产：单机调试 → 内网部署 → 跨公网 mTLS 安全部署。

## 目录

- [快速开始（本地三端联调）](#快速开始本地三端联调)
- [场景一：内网部署（Tailscale + Token）](#场景一内网部署tailscale--token)
- [场景二：公网部署（mTLS + CA）](#场景二公网部署mtls--ca)
- [Agent 服务化管理](#agent-服务化管理)
- [Console 控制台使用](#console-控制台使用)
- [运维操作](#运维操作)
- [故障排查](#故障排查)
- [数据目录与备份](#数据目录与备份)

---

## 快速开始（本地三端联调）

三个终端，30 秒跑通：

```bash
# 编译
make all

# 终端 1：Relay（中继服务器）
./bin/go-rmm-relay -addr :7780 -dev

# 终端 2：Agent（前台调试）
./bin/go-rmm-agent run -relay ws://127.0.0.1:7780/agent -id my-agent

# 终端 3：Console（Web 控制台）
./bin/go-rmm-console -addr :7781 -relay http://127.0.0.1:7780
```

浏览器打开 `http://localhost:7781`，左侧看到 Agent，下发 PowerShell 任务，实时看结果。

或用一键脚本：

```powershell
.\scripts\demo-local.ps1
```

---

## 场景一：内网部署（Tailscale + Token）

适合：个人/小团队，几台机器在同一虚拟内网。零自建公网服务器。

### 架构

```
你的电脑(Tailscale 100.x.x.x)
    │
    ├─ Console :7781 ──► Relay :7780 ──WS──► Agent(办公机 100.x.x.y)
    │                                          └─ Agent(家里机 100.x.x.z)
    └─ 浏览器
```

### 步骤

#### 1. 所有机器安装 Tailscale

```bash
# 各机器安装后登录同一账号
tailscale up
# 记录各机 Tailscale IP（100.x.x.x）
tailscale ip -4
```

#### 2. 选一台机器跑 Relay + Console

```bash
# Relay（绑定 Tailscale IP，设置 token）
./go-rmm-relay -addr 100.x.x.relay:7780 -token "$(openssl rand -hex 16)"

# Console
./go-rmm-console -addr 100.x.x.relay:7781 -relay http://100.x.x.relay:7780
```

#### 3. 被控机器安装 Agent

```powershell
# Windows（管理员 PowerShell）
.\go-rmm-agent.exe install `
    -relay ws://100.x.x.relay:7780/agent `
    -token <你的token>
.\go-rmm-agent.exe start
```

#### 4. 访问

浏览器打开 `http://100.x.x.relay:7781`，用 token 鉴权。

> **安全提示**：Tailscale 自身加密，但 Relay 与 Agent 间流量在 Tailscale 隧道内仍是明文。
> 若需端到端加密，用场景二的 mTLS。

---

## 场景二：公网部署（mTLS + CA）

适合：跨公网控制，或对安全有严格要求。Relay 暴露公网，全链路 mTLS。

### 架构

```
                        ┌─────────────────────┐
   Agent(各处) ──wss──► │  Relay(公网服务器)    │ ◄──https── Console / 浏览器
   (客户端证书)          │  -mtls + TLS 证书    │
                        │  内置 CA 自动管理     │
                        └─────────────────────┘
```

### 步骤

#### 1. 服务器准备 TLS 证书

Relay 需要服务器 TLS 证书（让 Agent/Console 校验 Relay 身份）。两种方式：

```bash
# 方式 A：Let's Encrypt（有域名）
certbot certonly --standalone -d relay.example.com

# 方式 B：自签（无域名，Agent 需跳过 ServerName 校验或导入 CA）
openssl req -x509 -newkey rsa:4096 -keyout relay.key -out relay.crt -days 365 -nodes \
    -subj "/CN=relay.example.com" -addext "subjectAltName=DNS:relay.example.com"
```

#### 2. 启动 Relay（mTLS 模式）

```bash
./go-rmm-relay \
    -addr :7780 \
    -mtls \
    -tls-cert /path/to/relay.crt \
    -tls-key /path/to/relay.key
```

首次启动会自动生成内置 CA：
```
data/ca-root.crt    # CA 根证书（10年有效）
data/ca-root.key    # CA 根私钥（0600 权限，妥善保管！）
data/ca-state.json  # CRL 状态（已签发/已撤销）
```

#### 3. 为每台 Agent 签发证书（enrollment 流程）

```bash
# 在服务器上：为 agent-prod-01 生成一次性注册令牌（1小时有效）
curl -X POST http://localhost:7780/api/enroll-token \
    -H "Content-Type: application/json" \
    -d '{"agent_id": "agent-prod-01"}'
# 返回: {"agent_id":"agent-prod-01","token":"a1b2c3...","expires_in":3600}
```

#### 4. Agent 首次注册（在被控机器上）

```powershell
# Agent 用 token 自动生成密钥 + 换取客户端证书
.\go-rmm-agent.exe run `
    -relay wss://relay.example.com:7780/agent `
    -id agent-prod-01 `
    -token <enrollment-token>
# 首次连接后自动完成 enroll，证书落盘 data/agent.crt + data/agent.key
# 之后注册为服务
.\go-rmm-agent.exe install -relay wss://relay.example.com:7780/agent -id agent-prod-01
.\go-rmm-agent.exe start
```

#### 5. 撤销 Agent（如有失陷）

```bash
# 在服务器上调用（需扩展 API，或直接操作 CA）
# 当前版本：RevokeAgent 会把证书加入 CRL，mTLS 握手时拒绝
# 撤销状态持久化到 ca-state.json，Relay 重启不丢失
```

---

## Agent 服务化管理

### Windows 服务

```powershell
# 安装（需管理员）
.\go-rmm-agent.exe install -relay ws://relay:7780/agent -token <token>

# 管理
.\go-rmm-agent.exe start       # 启动
.\go-rmm-agent.exe stop        # 停止
.\go-rmm-agent.exe status      # 状态
.\go-rmm-agent.exe uninstall   # 卸载

# 前台调试（不注册服务）
.\go-rmm-agent.exe run -relay ws://relay:7780/agent
```

### Linux systemd

```bash
# kardianos/service 自动生成 systemd unit
./go-rmm-agent install -relay ws://relay:7780/agent -token <token>
sudo systemctl start go-rmm-agent
sudo systemctl status go-rmm-agent
journalctl -u go-rmm-agent -f    # 看日志
```

### 配置文件

服务模式从 `config.json`（与二进制同目录）加载：

```json
{
  "agent_id": "agent-prod-01",
  "relay_url": "wss://relay.example.com:7780/agent",
  "token": "your-token",
  "heartbeat": 15000000000,
  "max_concurrent_tasks": 4,
  "tags": {
    "env": "prod",
    "role": "web"
  },
  "glm": {
    "api_key": "your-glm-key",
    "model": "glm-5-turbo",
    "base_url": "https://open.bigmodel.cn/api/paas/v4"
  }
}
```

> 权限自动设为 0600（含 token/glm key 等敏感信息）。
> `tags` 用于批量下发（如 `env=prod`）。
> `glm.api_key` 配置后 guiclick（视觉点击）能力才启用。

---

## Console 控制台使用

### 页面功能

| 区域 | 功能 |
|------|------|
| **左侧 Agent 列表** | 在线状态、capabilities、tags |
| **下发任务** | 选 Agent → 选类型 → 填参数 → 下发 |
| **运行指标** | 13 项 metrics 实时刷新（5s） |
| **实时结果流** | SSE 推送，含取消按钮 |
| **截图预览** | screenshot/guiclick 结果内联展示 |

### 任务类型速查

| 类型 | 必填参数 | 用途 |
|------|---------|------|
| `powershell` | command | 执行 PowerShell 命令 |
| `script` | lang, content | 下发 .ps1/.bat 脚本 |
| `file` | op, path | 上传(upload)/下载(download)文件 |
| `screenshot` | target(可选) | 窗口/全屏截图 |
| `browse` | url, actions | headless 浏览器自动化（需 `-tags browse` 编译） |
| `guiclick` | intent | 视觉驱动桌面点击（需 GLM key） |

### 批量下发

```bash
# 按标签批量（如所有 env=prod 的 Agent）
curl -X POST http://relay:7780/api/tasks/batch \
    -H "Content-Type: application/json" \
    -d '{
        "tag": "env=prod",
        "type": "powershell",
        "payload": {"command": "Get-Process | Select-Object -First 3"},
        "timeout": 30
    }'
```

### 取消任务

```bash
curl -X POST http://relay:7780/api/tasks/cancel \
    -d '{"task_id": "task-xxx", "agent_id": "agent-prod-01"}'
```

---

## 运维操作

### REST API 速查

| 方法 | 路径 | 用途 |
|------|------|------|
| GET | `/healthz` | 深度健康检查（含 store 状态） |
| GET | `/api/agents` | 在线 Agent 列表 |
| GET | `/api/tasks?limit=N` | 任务历史 |
| GET | `/api/tasks/{id}` | 单任务详情 + 结果 |
| POST | `/api/tasks` | 下发任务 |
| POST | `/api/tasks/batch` | 按标签批量下发 |
| POST | `/api/tasks/cancel` | 取消任务 |
| GET | `/api/metrics` | 13 项运行指标 |
| GET | `/api/events` | SSE 实时事件流 |
| POST | `/api/enroll-token` | 生成注册令牌（mTLS） |
| POST | `/api/enroll` | Agent 换取证书（mTLS） |

### metrics 解读

```json
{
  "agents_online": 3,          // 当前在线 Agent 数
  "tasks_submitted": 150,      // 累计提交
  "tasks_dispatched": 148,     // 累计投递
  "tasks_completed": 140,      // 成功完成
  "tasks_failed": 5,           // 失败（含 maxAttempts）
  "tasks_blocked": 2,          // 命令策略拦截
  "tasks_cancelled": 1,        // 被取消
  "tasks_acked": 146,          // Agent ACK 数
  "tasks_nacked": 0,           // Agent NACK（pool 满）
  "tasks_reaped": 3,           // reaper 回退数（若持续增长说明 Agent 频繁超时）
  "events_dropped": 0,         // SSE 丢弃（若增长说明 Console 处理慢）
  "uptime_seconds": 86400,
  "goroutines": 18
}
```

**告警建议**：
- `tasks_failed / tasks_completed` 持续 > 10%：检查任务载荷或 Agent 环境
- `tasks_reaped` 持续增长：Agent 频繁超时，检查网络/负载
- `events_dropped` > 0：Console 消费慢，考虑增加订阅缓冲

### 审计日志

位于 `data/audit.log`（JSON Lines，只追加）：

```bash
# 查看最近 20 条审计
tail -20 data/audit.log | jq .

# 统计各类事件
cat data/audit.log | jq -r .event | sort | uniq -c

# 查找被拦截的任务
grep task_blocked data/audit.log | jq .
```

---

## 故障排查

### Agent 连不上 Relay

```bash
# 1. 检查网络连通
telnet relay.example.com 7780

# 2. 前台模式看详细日志
./go-rmm-agent run -relay ws://relay:7780/agent -token xxx
# 常见错误：
#   "connection refused" → Relay 未启动或防火墙
#   "handshake error" → mTLS 证书问题
#   "unauthorized" → token 错误

# 3. 检查 Relay 侧 healthz
curl http://relay:7780/healthz
```

### 任务一直 pending 不执行

```bash
# 1. Agent 是否在线？
curl http://relay:7780/api/agents | jq .

# 2. 任务状态？
curl http://relay:7780/api/tasks/<task_id> | jq .task.status
# pending = Agent 离线或 pool 满，等 reaper 补投
# dispatched = 已投递等结果
# 若长期 pending 且 Agent 在线 → 检查 metrics tasks_nacked

# 3. 查审计
grep <task_id> data/audit.log
```

### 任务结果不返回

```bash
# 1. Agent 日志看是否有 "任务结果回流失败"
# 2. 可能是 WS 写超时 → Agent 会缓存结果，重连后补发
# 3. 查 metrics events_dropped 是否增长（Console 侧丢事件，但结果在 store 里）
curl http://relay:7780/api/tasks/<task_id> | jq .result
```

### mTLS 证书过期

Agent 客户端证书有效期 24 小时，Agent 会自动续签（enroll.go 的 needRenew 逻辑）。
若续签失败（如 Relay 不可达超 24h）：

```powershell
# 删除旧证书，重新 enroll
del data\agent.crt data\agent.key
.\go-rmm-agent.exe run -relay wss://relay:7780/agent -id <agent-id> -token <new-enrollment-token>
```

---

## 数据目录与备份

### Relay 数据目录（`data/`）

| 文件 | 用途 | 备份策略 |
|------|------|---------|
| `tasks.db` | bbolt 任务持久化 | 定期备份（含在途任务状态） |
| `audit.log` | 审计日志（append-only） | 归档到 SIEM/对象存储 |
| `ca-root.crt` | CA 根证书 | 离线备份（10年有效） |
| `ca-root.key` | CA 根私钥 | **最高安全**，离线冷备（泄露=全系统失陷） |
| `ca-state.json` | CRL 状态（已签发/撤销） | 随 tasks.db 一起备份 |

### Agent 数据目录（`data/`）

| 文件 | 用途 |
|------|------|
| `agent.crt` | 客户端证书（24h，自动续签） |
| `agent.key` | 客户端私钥（0600） |
| `ca.crt` | Relay CA 证书（校验 Relay 身份） |

### 备份命令

```bash
# Relay 数据备份
tar czf relay-data-$(date +%F).tar.gz data/

# 迁移 Relay 到新服务器
# 1. 停旧 Relay
# 2. 复制 data/ 到新服务器
# 3. 启动新 Relay（自动恢复 tasks/ca/audit）
# 4. Agent 会自动重连（反向连接，Relay 地址不变则无感）
```

---

## 安全清单

部署前逐项确认：

- [ ] Relay 不直接暴露 3389(RDP)/22(SSH)/5985(WinRM) 到公网
- [ ] 使用 Tailscale 组网或 mTLS 加密
- [ ] token 已设置（非空），且未硬编码在脚本仓库
- [ ] config.json 权限 0600
- [ ] ca-root.key 已离线备份，服务器上权限 0600
- [ ] 命令策略已启用（默认启用，检查 metrics tasks_blocked > 0）
- [ ] 审计日志已接入收集
- [ ] healthz 已接入监控告警
- [ ] Agent MaxConcurrentTasks 已按机器负载调整（默认 4）
- [ ] guiclick 的 GLM key 仅在需要的 Agent 配置（非全局）
