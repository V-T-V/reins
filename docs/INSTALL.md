# reins 快速安装指南(从零到能用 · 一页)

> 适用于 `reins-server-windows-<版本>.zip`。被控端安装见同目录分发的两个 Agent 包:
> Windows 机器用 `reins-agent-windows-<版本>.zip`(内含 AGENT-SETUP.md);
> Linux 机器用 `reins-agent-linux-<版本>.tar.gz`(内含 AGENT-LINUX-SETUP.md,含能力分账)。

## 架构 30 秒

```
你的浏览器 ──HTTP──► reins-relay(服务器,本包) ◄──反向 WS 长连接── reins-agent(被控 Windows 机器)
                                                            ◄──反向 WS 长连接── reins-agent(被控 Linux 机器)
```

Relay 是唯一需要暴露端口的组件;Agent 主动外连,被控机器不需要任何入站端口。
Web 控制台内嵌在 Relay 里(浏览器打开即用),CLI(`reins.exe`)也在本包。

## 第零步:校验产物完整性(拿到包后第一件事)

每个发布目录都带 `SHA256SUMS.txt`(全部 zip/tar.gz 的 SHA256,小写十六进制,
`sha256sum -c` 兼容格式)。分发/接收任何一环都先校验,防传输损坏与掉包:

```powershell
# Windows(PowerShell,与 SHA256SUMS.txt 逐行比对)
Get-Content .\SHA256SUMS.txt | ForEach-Object {
    $hash, $name = $_ -split '  ', 2
    $actual = (Get-FileHash -Algorithm SHA256 $name).Hash.ToLower()
    if ($actual -eq $hash) { "OK    $name" } else { "FAIL  $name" }
}

# Windows(无 PowerShell 的环境)
certutil -hashfile reins-server-windows-<版本>.zip SHA256

# Linux / macOS / WSL
sha256sum -c SHA256SUMS.txt          # 校验目录内全部条目
sha256sum -c SHA256SUMS.txt reins-agent-linux-<版本>.tar.gz   # 只校单个
```

任一 `FAIL` 即停止分发/安装,重新获取。

## 第一步:起 Relay(2 分钟)

1. 解压 zip 到任意目录(建议 `C:\Program Files\reins-server`),管理员 PowerShell 进入该目录。
2. 想一个鉴权 Token(自定义字符串,如 `reins-prod-2026`),然后:

```powershell
# 快速试用(前台运行,Ctrl+C 停)
.\start-relay.ps1 -Token reins-prod-2026

# 正式部署(开机自启 + 崩溃 1 分钟内自动重启,日志 data\relay.log)
.\start-relay.ps1 -Token reins-prod-2026 -Mode boot

# 内网 HTTPS(自签证书自动生成,825 天)
.\start-relay.ps1 -Token reins-prod-2026 -Mode boot -UseTLS
```

3. 浏览器打开 `http://<本机IP>:7780/`(UseTLS 则 `https://<本机IP>:8443/`)——看到 reins 控制台即成功。
   首次会显示"部署向导",照着把 Agent 装起来即可。

## 第二步:装 Agent(每台被控机器,1 分钟)

在每台被控 Windows 机器上,解压 `reins-agent-windows-<版本>.zip`,管理员 PowerShell:

```powershell
.\service-install.ps1 install -Relay <Relay的IP>:7780 -Token reins-prod-2026 -Tags env=prod
```

装完即:Windows 服务注册(开机自启 + 崩溃自动重启)+ 启动 + 反向连接 Relay。
刷新控制台,左侧 Agent 列表应出现该机器(含主机名/系统/能力/标签/在线时长)。

> 更省事的替代:控制台"部署向导"里的一行命令(`iex (irm http://<relay>/install.ps1)`)——
> 前提是 Relay 的 `dl\` 目录里放了 `reins-agent.exe`(zip 里已带,拷过去即可)。

## 第三步:验证与日常使用

| 做什么 | 在哪 |
|---|---|
| 下发第一条命令 | 控制台「任务执行」页 → 选 Agent → powershell → `Get-Process \| Select -First 5` |
| 看历史任务/结果(截图/输出按类型渲染) | 控制台「任务历史」页(可按状态/类型/Agent 筛选) |
| 看谁在什么时候做了什么 | 控制台「审计日志」页(hash chain 防篡改,可导出 SIEM) |
| 一对多批量执行 | 「任务执行」页「批量分发」(按 tag,如 `env=prod`) |
| CLI 侧同样能力 | `.\reins.exe -relay http://<IP>:7780 -token <Token> agent list` / `task run ...` |

## 升级 / 卸载

```powershell
# Relay:直接覆盖 reins-relay.exe 后重启计划任务
.\start-relay.ps1 -Mode uninstall   # 移除自启
.\start-relay.ps1 -Token <Token> -Mode boot   # 重新注册(新版本)

# Agent(在被控机):
.\service-install.ps1 upgrade -NewExe C:\Downloads\reins-agent.exe   # 备份→替换→验证→失败自动回滚
.\service-install.ps1 rollback                                       # 回滚上一版
.\service-install.ps1 uninstall                                      # 卸载并清理
```

## 数据与安全须知

- **数据位置**:全部状态在 Relay 目录的 `data\`(tasks.db / audit.log / CA / 脚本库)。备份这个目录=备份全部。
- **Token 即边界**:Token 泄漏=控制权泄漏。生产建议 `-policy-allow` 命令白名单 + 定期轮换 Token + Agent 侧 `allowed_roots` 限目录。
- **HTTPS**:内网用 `-UseTLS`(自签);公网请置于反向代理(nginx/caddy)后用正规 CA 证书。
- **自签证书续期**:`.\reins-gencert.exe -days 825 -force`(Relay 自带证书过期监控告警)。
- **防火墙**:start-relay.ps1 已自动放行端口;跨网段请自行确认路由可达。

## 常见问题

| 症状 | 处置 |
|---|---|
| 控制台打不开 | `sc query`/任务计划程序里看 reins-relay 是否在跑;看 `data\relay.log` |
| Agent 装了但列表没有 | 被控机 `sc query reins-agent` + 事件查看器 Application 日志;确认 Token/地址一致 |
| 命令被拦截(task_blocked) | 命中命令策略黑名单——审计日志页可看拦截详情;改用平台原生命令(如 `reins power shutdown`) |
| 升级后服务起不来 | `service-install.ps1 rollback`;看事件查看器 reins-agent 日志 |
