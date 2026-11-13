# reins Agent 安装指南(被控 Windows 机器 · 一页)

> 适用于 `reins-agent-windows-<版本>.zip`。前提:已有一台跑起来的 reins-relay(见 server 包 INSTALL.md)。

## 安装(管理员 PowerShell,在解压目录内)

```powershell
.\service-install.ps1 install -Relay <Relay地址> -Token <Token> [-Tags env=prod,role=web]
```

- `-Relay` 支持简写(`192.168.1.10` 自动补全为 `ws://192.168.1.10:7780/agent`)或完整 `ws://host:7780/agent`。
- `-Tags` 可选,`key=value` 形式,供控制台批量分发按标签选择目标。
- 安装内容:二进制 → `C:\Program Files\reins\`,注册 Windows 服务(**开机自启 + 崩溃自动重启**),配置写入同目录 `config.json`,立即启动。

## 验证

```powershell
.\service-install.ps1 status        # 服务状态
```

Relay 控制台左侧 Agent 列表应出现本机(带主机名/能力标签/在线时长)。

## 日常操作

| 操作 | 命令 |
|---|---|
| 停止/启动/重启 | `.\service-install.ps1 stop / start / restart` |
| 升级到新版 | `.\service-install.ps1 upgrade -NewExe C:\Downloads\reins-agent.exe`(备份→替换→探活→失败自动回滚) |
| 回滚上一版 | `.\service-install.ps1 rollback` |
| 卸载(含清理) | `.\service-install.ps1 uninstall`(保留工作区数据加 `-KeepWorkspace`) |

## 手动方式(不想要包装脚本)

reins-agent.exe 自带服务管理,与包装脚本等价:

```powershell
reins-agent.exe install -relay <地址> -token <Token>
reins-agent.exe start / stop / status / uninstall
reins-agent.exe run -relay <地址> -token <Token>    # 前台调试模式
```

## 配置(config.json,高级)

服务模式从 exe 同目录 `config.json` 加载。完整字段说明见仓库 `config.example.json`
(zip 内已附带)。常用:

```jsonc
{
  "relay_url": "ws://192.168.1.10:7780/agent",
  "token": "reins-prod-2026",
  "tags": { "env": "prod" },
  "allow_all_roots": false,              // 生产建议关,配合 allowed_roots 最小权限
  "allowed_roots": ["C:\\reins\\workspace", "D:\\apps"],
  "workspace": "C:\\reins\\workspace"
}
```

改完配置执行 `.\service-install.ps1 restart` 生效。

## 故障排查

- 服务起不来:事件查看器 → Windows 日志 → Application,来源 reins-agent。
- 连不上 Relay:确认地址/Token;`reins-agent.exe run -relay ...` 前台模式直看日志。
- Agent 在线但任务被拒:检查 `allowed_roots` 是否覆盖目标路径(安全默认 fail-closed)。
