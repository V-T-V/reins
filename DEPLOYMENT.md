# go-rmm 生产级部署指南

> v3.8.1 · 从零到生产:架构规划 → 安装 → SSL → 安全加固 → 监控 → 日常运维

---

## 目录

1. [架构规划](#1-架构规划)
2. [环境要求](#2-环境要求)
3. [服务端部署(Relay)](#3-服务端部署relay)
4. [Agent 部署(被控端)](#4-agent-部署被控端)
5. [操作端配置(CLI / SDK)](#5-操作端配置cli--sdk)
6. [Nginx + HTTPS(SSL)](#6-nginx--httpsssl)
7. [安全加固](#7-安全加固)
8. [监控与告警](#8-监控与告警)
9. [日常运维](#9-日常运维)
10. [故障排查](#10-故障排查)
11. [备份与恢复](#11-备份与恢复)
12. [升级与回滚](#12-升级与回滚)

---

## 1. 架构规划

### 推荐架构(单 Relay + N Agent)

```
                        ┌─────────────────────────────────┐
                        │       运维人员 / AI Agent        │
                        │   CLI / SDK / Web UI            │
                        └───────────┬─────────────────────┘
                                    │ HTTPS / WSS
                        ┌───────────▼─────────────────────┐
                        │        Nginx (443 SSL)          │
                        │   https://your-domain.com/rmm/  │
                        └───────────┬─────────────────────┘
                                    │ HTTP 127.0.0.1:7780
                        ┌───────────▼─────────────────────┐
                        │     go-rmm Relay (systemd)      │
                        │  • bbolt 持久化(tasks.db)      │
                        │  • 审计 hash chain(audit.log)  │
                        │  • Prometheus /metrics          │
                        │  • CA + mTLS(可选)             │
                        └───┬─────────┬─────────┬─────────┘
                            │         │         │
                     ┌──────▼──┐ ┌───▼────┐ ┌───▼────┐
                     │ Agent 1 │ │ Agent 2│ │Agent N │
                     │ Windows │ │ Linux  │ │Windows │
                     └─────────┘ └────────┘ └────────┘
```

### 规模建议

| Agent 数 | Relay 配置 | 部署模式 |
|----------|------------|----------|
| 1-50 | 2C4G | 单 Relay + bbolt |
| 50-200 | 4C8G | 单 Relay + bbolt + Nginx |
| 200-1000 | 4C8G × 2 | 多 Relay + Redis + Nginx LB |
| 1000+ | K8s 集群 | Helm + Redis + HPA |

---

## 2. 环境要求

### 服务端(Relay)

| 项目 | 要求 |
|------|------|
| OS | Linux(Ubuntu 22.04+/CentOS 8+)或 Windows Server |
| 架构 | amd64 / arm64 |
| Go | 1.21+(编译用)或下载预编译二进制 |
| 内存 | ≥ 512MB(推荐 2GB+) |
| 端口 | 7780 — 或经 Nginx 代理到 443 |
| 依赖 | 无(纯 Go 静态二进制) |

### Agent(被控端)

| 项目 | 要求 |
|------|------|
| OS | Windows 10/11/Server 2016+ 或 Linux |
| 内存 | ≥ 128MB |
| 网络 | 能出站访问 Relay 7780(WSS) |

---

## 3. 服务端部署(Relay)

### 方式 A:一键脚本

```bash
curl -fsSL https://your-domain.com/rmm/q.sh | bash
# 或从源码
./scripts/quickstart.sh server
```

### 方式 B:手动部署

```bash
# 编译
git clone https://github.com/QiuShichang/go-rmm.git && cd go-rmm
CGO_ENABLED=0 go build -ldflags "-s -w" -o /opt/go-rmm/go-rmm-relay ./relay/

# systemd 服务
cat > /etc/systemd/system/go-rmm-relay.service << 'EOF'
[Unit]
Description=go-rmm Relay
After=network-online.target
Wants=network-online.target
[Service]
Type=simple
ExecStart=/opt/go-rmm/go-rmm-relay -addr 127.0.0.1:7780
WorkingDirectory=/opt/go-rmm
Restart=always
RestartSec=3
LimitNOFILE=65536
NoNewPrivileges=true
ProtectSystem=strict
ReadWritePaths=/opt/go-rmm/data
PrivateTmp=true
[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable --now go-rmm-relay

# 验证
curl http://127.0.0.1:7780/healthz
# → {"relay":"ok","store":"ok"}
```

### 方式 C:Docker

```bash
docker build -f Dockerfile.relay -t go-rmm-relay .
docker run -d --name rmm -p 7780:7780 -v rmm-data:/data --restart unless-stopped go-rmm-relay
```

### 方式 D:Kubernetes

```bash
helm install rmm deploy/helm/go-rmm --set relay.token=$(openssl rand -hex 16)
```

---

## 4. Agent 部署(被控端)

### Windows

```powershell
# 管理员 PowerShell
.\go-rmm-agent.exe install -relay wss://your-domain.com/rmm/agent -token <TOKEN>
.\go-rmm-agent.exe start
.\go-rmm-agent.exe status
```

### Linux

```bash
cat > /etc/systemd/system/go-rmm-agent.service << 'EOF'
[Unit]
Description=go-rmm Agent
After=network-online.target
[Service]
Type=simple
ExecStart=/usr/local/bin/go-rmm-agent run -relay wss://your-domain.com/rmm/agent -token <TOKEN>
Restart=always
RestartSec=5
[Install]
WantedBy=multi-user.target
EOF
systemctl enable --now go-rmm-agent
```

### 标签配置(批量管理)

```json
{
    "relay_url": "wss://your-domain.com/rmm/agent",
    "token": "<TOKEN>",
    "tags": {"env": "prod", "role": "web", "power_allowed": "true"},
    "allow_all_roots": false,
    "allowed_roots": ["C:\\app\\workspace"]
}
```

---

## 5. 操作端配置

### CLI

```bash
export GO_RMM_RELAY=https://your-domain.com/rmm
export GO_RMM_TOKEN=<TOKEN>

go-rmm agent list
go-rmm task run -a win-prod-01 -c "Get-Date"
go-rmm task stream -a win-prod-01 -c "ping -t 8.8.8.8"
go-rmm events
go-rmm sysinfo -a win-prod-01
go-rmm monitor top -a win-prod-01
```

### SDK(AI Agent 接入)

```go
c := client.New("https://your-domain.com/rmm", "<TOKEN>")
tools, _ := c.ListTools()
stdout, _ := c.RunCommand("win-prod-01", "Get-Date", 10)
result, _ := c.StreamCommand(ctx, "win-prod-01", "ping 8.8.8.8", 30, handler)
```

---

## 6. Nginx + HTTPS

```nginx
server {
    listen 443 ssl http2;
    server_name your-domain.com;
    ssl_certificate /etc/letsencrypt/live/your-domain.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/your-domain.com/privkey.pem;

    location /rmm/ {
        proxy_pass http://127.0.0.1:7780/;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_buffering off;
        proxy_read_timeout 86400s;
        proxy_request_buffering off;
        client_max_body_size 0;
    }
}
```

```bash
# Let's Encrypt
certbot --nginx -d your-domain.com

# 验证
curl -k https://your-domain.com/rmm/healthz
```

---

## 7. 安全加固

| 层 | 措施 | 命令 |
|----|------|------|
| 鉴权 | Token(必须) | `-token $(openssl rand -hex 32)` |
| RBAC | 多租户角色 | Token: `tenant:operator:secret` |
| 电源 | 标签白名单 | Agent tags: `power_allowed=true` |
| 文件 | 路径白名单 | `allow_all_roots: false` |
| 传输 | mTLS(可选) | `-mtls -tls-cert cert.pem` |
| 审计 | hash chain | 自动(SIEM 导出 CEF) |
| 凭据 | Secret 管理 | `POST /api/secrets`(TTL+一次性) |
| 命令 | 策略拦截 | 自动(Format-Volume/rm -rf 拦截) |
| 录屏 | Session 记录 | 自动(asciinema v2) |

---

## 8. 监控与告警

### Prometheus

```yaml
scrape_configs:
  - job_name: go-rmm
    static_configs: [{targets: ['relay:7780']}]
    metrics_path: /metrics
```

### Webhook 告警

```bash
export GO_RMM_WEBHOOKS="https://hooks.slack.com/xxx"
# 自动触发:任务失败/Agent离线/策略拦截/电源操作
```

### 健康检查

```bash
# crontab
*/1 * * * * curl -sf http://127.0.0.1:7780/healthz || systemctl restart go-rmm-relay
```

---

## 9. 日常运维

```bash
journalctl -u go-rmm-relay -f       # Relay 日志
journalctl -u go-rmm-agent -f       # Agent 日志
go-rmm events                        # 实时事件流
go-rmm task list --status failed     # 查失败任务
go tool pprof http://relay:7780/debug/pprof/heap  # 内存分析
```

---

## 10. 故障排查

| 症状 | 排查 |
|------|------|
| Agent 连不上 | `ping relay` → `telnet relay 7780` → 检查 token |
| Nginx 404 | `proxy_pass` 尾部必须有 `/` |
| WSS 连接失败 | HTTPS 下 Agent 必须用 `wss://` 而非 `ws://` |
| 任务卡 pending | Agent 是否在线 → `go-rmm agent list` |
| 中文乱码 | 已用 EncodedCommand(UTF16LE)解决 |

---

## 11. 备份与恢复

```bash
# 备份(Relay 停机后)
tar czf /backup/rmm-$(date +%Y%m%d).tar.gz /opt/go-rmm/data/

# 恢复
systemctl stop go-rmm-relay
tar xzf /backup/rmm-20260101.tar.gz -C /
systemctl start go-rmm-relay
```

---

## 12. 升级与回滚

### Agent 自更新(远程)

```bash
go-rmm update upload -f agent-new.exe --version v4.0
go-rmm update push -a win-prod-01 --version v4.0
# 自动 rename+copy+重启+60s 健康回滚
```

### Relay 升级

```bash
cd go-rmm && git pull
go build -o /opt/go-rmm/go-rmm-relay ./relay/
systemctl restart go-rmm-relay
```

### 回滚

```bash
# Relay:换旧二进制重启
# Agent:go-rmm update rollback -a <agent>
```

---

## 生产 Checklist

```
□ Token 已设置(非空)
□ systemd 已启用(开机自启)
□ Nginx HTTPS 已配置
□ 防火墙:7780 仅本机
□ 数据目录权限 0600
□ 备份脚本已配(crontab)
□ Agent 注册为服务
□ Tags 已配(env/role)
□ power_allowed 仅授权必要机器
□ allowed_roots 已限制
□ Prometheus 采集已配
□ Webhook 告警已配
□ 健康检查脚本已配
```
