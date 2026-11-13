# reins Agent(Linux)安装指南 · 一页

> 适用于 `reins-agent-linux-<版本>.tar.gz`(linux/amd64)。前提:已有一台跑起来的 reins-relay(见 server 包 INSTALL.md)。
> Windows 被控端请用 `reins-agent-windows-<版本>.zip`(能力全集见其 AGENT-SETUP.md)。

## 能力分账(Linux 上什么能用/什么不能用)

reins 的执行器按平台注册(`executors/register.go`):Agent 注册时只声明本机真实可用的
能力,Relay 据此路由——**Linux 上不可用的任务类型不会下发给它,不会出现"接了再失败"**。
下表是 linux/amd64 构建的真实注册表:

| 能力 | Linux 可用 | 说明 |
|---|---|---|
| `script`(lang=sh) | ✅ | 跨平台脚本执行。CLI 会按目标 OS 自动选 sh(`task run -t script -s x.sh`);`lang=ps1/bat` 在 Linux 被拒绝 |
| `file` | ✅ | 文件读写/删除/列目录,受 `allowed_roots` 边界(fail-closed 语义与 Windows 一致) |
| `browse` | ⚠️ stub | 默认构建为占位实现(返回"不可用");真实 chromedp 需 `-tags browse` 自行编译,Windows 同 |
| `monitor` | ✅ | 流式系统监测,Linux 走 `/proc`(Windows 走 PowerShell) |
| `update` | ✅ | 自更新(SHA256 校验+备份+两步 rename+健康回滚;systemd 重启拉起新版)。仅 slave 角色 |
| 交互式 `shell` | ✅ | 会话默认 `bash`(Windows 为 powershell.exe);纯管道回显,无 PTY 特性(Tab 补全/方向键同 Windows 旧形态) |
| 大文件传输 | ✅ | upload/download/sync push/pull、分片续传(xfer_offset)、限速、内网直连数据面——全跨平台 |
| mTLS/注册/审计 | ✅ | 反向 WS、每设备证书、规则门禁(.reins/rules.md)、幂等、限流——全跨平台 |
| `powershell` | ❌ | 仅 Windows(未注册) |
| `screenshot` | ❌ | 依赖 Win32 API(PrintWindow/BitBlt),仅 Windows(未注册) |
| `guiclick` | ❌ | Windows GUI 自动化 + GLM 视觉,仅 Windows(未注册) |
| `power` | ❌ | **执行器代码内有 Linux 分支(shutdown/poweroff/reboot)但注册仅 Windows——本轮如实标注,不硬编**;Linux 需要电源操作走 `script`(sh)或未来接线(一行注册改动+测试,另立一轮) |
| watchdog(master 角色) | ❌ | 非 Windows 平台禁用(用于守护 Windows slave 进程树,语义不适用) |
| 进程树击杀 | ✅ 等价 | Windows 用 Job Object;Linux 用进程组 SIGKILL(`job_other.go`),超时/取消杀整棵树的语义等价 |
| 服务安装 | ✅ | `reins-agent install` 内建 systemd 注册(kardianos/service);或用本包 unit 文件(下文,推荐) |

一句话:**命令/脚本/文件/传输/监测/自更新/交互 shell 全量可用;Windows 桌面面(screenshot/
guiclick/powershell/power)不可用——那正是装 Windows Agent 的理由。**

## 安装(被控 Linux 机器)

```bash
# 1. 解压(建议固定路径,unit 文件默认指向这里)
sudo mkdir -p /opt/reins-agent
sudo tar -xzf reins-agent-linux-<版本>.tar.gz -C /opt/reins-agent
sudo chmod +x /opt/reins-agent/reins-agent   # 防御:包内已是 0755,重打包/传输丢位时兜底

# 2. 写配置(最小可用)
sudo tee /opt/reins-agent/config.json >/dev/null <<'EOF'
{
  "relay_url": "ws://<Relay地址>:7780/agent",
  "token": "<Token>",
  "allow_all_roots": false,
  "allowed_roots": ["/opt/reins-agent/workspace"],
  "workspace": "/opt/reins-agent/workspace",
  "tags": { "env": "prod" }
}
EOF

# 3. 装 systemd 服务并启动
sudo cp /opt/reins-agent/reins-agent-linux.service /etc/systemd/system/reins-agent.service
sudo systemctl daemon-reload
sudo systemctl enable --now reins-agent

# 4. 看日志确认连上
journalctl -u reins-agent -f
```

Relay 控制台左侧 Agent 列表应出现本机(OS 列 `linux/amd64`,能力列只有上表 ✅ 项)。

## 日常操作

| 操作 | 命令 |
|---|---|
| 状态/停止/启动 | `sudo systemctl status / stop / start reins-agent` |
| 升级 | 停服务 → 覆盖 `/opt/reins-agent/reins-agent` → 起服务;或走内置 `update` 任务通道 |
| 卸载 | `sudo systemctl disable --now reins-agent && sudo rm /etc/systemd/system/reins-agent.service && sudo rm -rf /opt/reins-agent` |
| 前台调试 | `/opt/reins-agent/reins-agent run -relay ws://<地址>:7780/agent -token <Token>`(直看日志,Ctrl+C 退) |

## 验证边界(如实声明)

本包在 Windows 构建机上交叉编译(GOOS=linux GOARCH=amd64, CGO=0)产出。构建成功、
SHA256 校验、以及 WSL2(Ubuntu-24.04)真机 loopback 冒烟已做;**未在裸机 Linux 发行版
矩阵(Debian/RHEL/ARM 等)上做端到端验证**。已知敏感点:

- systemd unit 以 `Type=simple` 直接跑二进制(无参数=服务模式),与 kardianos 内建注册等价;
  若用 `reins-agent install` 自动注册,unit 路径/名称以它生成的为准。
- 裸机差异(SELinux、防火墙、非 bash 默认 shell)不在本包验证范围,遇到按常规排查:
  `journalctl -u reins-agent` + 前台 `run` 模式。

## 校验包完整性

```bash
sha256sum -c SHA256SUMS.txt reins-agent-linux-<版本>.tar.gz   # 或对目录内全部条目
```
