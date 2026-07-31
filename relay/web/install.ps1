# ╔══════════════════════════════════════════════════════════════╗
# ║  go-rmm Agent 一行命令远程部署 — Windows                      ║
# ║  在目标 Win10 机器上执行一行命令即可自动下载+安装+注册服务     ║
# ╚══════════════════════════════════════════════════════════════╝
#
# 用法(管理员 PowerShell,在目标机器执行):
#
#   # 方式1: 一行命令(推荐,从 Relay 拉取脚本+二进制)
#   # 注意:不能用 iex(...)-Param,因为 iex 不接受命名参数。
#   # 必须用 [scriptblock]::Create() 编译后用 & 调用,才能传参。
#   & ([scriptblock]::Create((irm 'https://203.0.113.99/rmm/install.ps1'))) -Server 'https://203.0.113.99/rmm' -Relay 'wss://203.0.113.99/rmm/agent' -Token 'xxx'
#
#   # 方式2: 显式参数(先下载脚本再跑)
#   irm 'https://203.0.113.99/rmm/install.ps1' -OutFile install.ps1
#   .\install.ps1 -Server 'https://203.0.113.99/rmm' -Relay 'wss://203.0.113.99/rmm/agent' -Token 'xxx'
#
#   # 方式3: 交互式(不传参数,会提示输入)
#   & ([scriptblock]::Create((irm 'https://203.0.113.99/rmm/install.ps1')))
#
#   # 方式4: 前台调试(不装服务,无需管理员)
#   .\install.ps1 -Server 'https://R/rmm' -Relay 'wss://R/rmm/agent' -Foreground
#
# 卸载:
#   .\install.ps1 -Uninstall
#
# 特点:
#   - 无源码依赖、无 GitHub 依赖(二进制从 Relay 的 /dl/ 端点拉取)
#   - 自动下载、自动注册服务、自动启动
#   - 完整的失败排查提示

param(
    [string]$Server = "",
    [string]$Relay = "",
    [string]$Token = "",
    [string]$AgentId = "",
    [string[]]$Tags = @(),
    [string]$InstallDir = "C:\Program Files\go-rmm",
    [string]$Workspace = "C:\go-rmm\workspace",
    [switch]$Foreground,
    [switch]$Uninstall,
    # M1 主从架构:支持装 master/slave 角色
    [ValidateSet("", "master", "slave")]
    [string]$Role = "",
    [string]$SlaveExeDir = "",    # master 专属:从的 exe 目录
    [string]$SlaveAgentId = "",   # master 专属:从的 agent_id
    [string]$Version = "v4.2.0"
)

$ErrorActionPreference = "Stop"
# 服务名按 Role 推导
$ServiceName = switch ($Role) {
    "master" { "go-rmm-master" }
    "slave"  { "go-rmm-slave" }
    default  { "go-rmm-agent" }
}

# 主从模式默认目录
if ($Role -eq "master" -and $InstallDir -eq "C:\Program Files\go-rmm") {
    $InstallDir = "C:\Program Files\go-rmm-master"
    $Workspace = "C:\go-rmm\master-workspace"
}
if ($Role -eq "slave" -and $InstallDir -eq "C:\Program Files\go-rmm") {
    $InstallDir = "C:\Program Files\go-rmm-slave"
    $Workspace = "C:\go-rmm\slave-workspace"
}

# 前台模式(无需管理员):默认装到用户目录
if ($Foreground -and $InstallDir -eq "C:\Program Files\go-rmm") {
    $InstallDir = Join-Path $env:LOCALAPPDATA "go-rmm"
}

# === 辅助函数 ===
function Write-Step($msg) { Write-Host "[步骤] $msg" -ForegroundColor Cyan }
function Write-OK($msg)   { Write-Host "[OK]    $msg" -ForegroundColor Green }
function Write-Err($msg)  { Write-Host "[错误]  $msg" -ForegroundColor Red }
function Write-Info($msg) { Write-Host "[信息]  $msg" -ForegroundColor Yellow }

# === 卸载模式 ===
if ($Uninstall) {
    Write-Host "go-rmm Agent 卸载" -ForegroundColor Cyan
    $exePath = Join-Path $InstallDir "go-rmm-agent.exe"
    if ($InstallDir -eq "C:\Program Files\go-rmm") {
        foreach ($p in @("$env:LOCALAPPDATA\go-rmm\go-rmm-agent.exe", "C:\Program Files\go-rmm\go-rmm-agent.exe")) {
            if (Test-Path $p) { $exePath = $p; break }
        }
    }
    if (Test-Path $exePath) {
        try {
            Stop-Service -Name $ServiceName -Force -ErrorAction SilentlyContinue
            & $exePath uninstall
            Write-OK "服务已卸载"
        } catch { Write-Info "服务未注册或已卸载" }
    } else {
        Write-Info "未找到 go-rmm-agent.exe(可能本机未安装)"
    }
    exit 0
}

# === 检查管理员权限(服务模式) ===
if (-not $Foreground) {
    $isAdmin = ([Security.Principal.WindowsPrincipal] [Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
    if (-not $isAdmin) {
        Write-Err "安装服务需要管理员权限"
        Write-Info "请右键 PowerShell → 以管理员身份运行"
        Write-Info "或加 -Foreground 参数前台调试(无需管理员)"
        exit 1
    }
}

# === 推导 Server / Relay ===
# 当通过 & ([scriptblock]::Create((irm ...))) 执行时,脚本无 $PSScriptRoot,需用 -Server 显式指定
if (-not $Server) {
    if ($Relay) {
        # 从 Relay URL 推导 Server: ws://host:7780/agent → http://host:7780
        $Server = $Relay -replace '^wss?://','http' -replace '/agent$',''
        if ($Relay -match '^wss://') { $Server = $Server -replace '^http://','https://' }
    } else {
        # 交互式
        Write-Host ""
        Write-Host "go-rmm Agent 部署向导" -ForegroundColor Cyan
        $Server = Read-Host "请输入 Relay 地址(如 http://203.0.113.99:7780)"
        if (-not $Server) { Write-Err "Server 地址必填"; exit 1 }
    }
}
if (-not $Relay) {
    # 从 Server 推导 Relay
    $scheme = "ws"
    if ($Server -match '^https://') { $scheme = "wss" }
    $hostPort = $Server -replace '^https?://',''
    $Relay = "${scheme}://${hostPort}/agent"
}

if (-not $Token -and -not $Foreground) {
    $Token = Read-Host "请输入 Token(直接回车=dev 模式免 token)"
}

# === 横幅 ===
Write-Host ""
Write-Host "═══════════════════════════════════════════════════════════════"
Write-Host " go-rmm Agent $Version 远程部署" -ForegroundColor Cyan
Write-Host "═══════════════════════════════════════════════════════════════"
Write-Host "  Server:    $Server"
Write-Host "  Relay:     $Relay"
if ($Token) { Write-Host "  Token:     $($Token.Substring(0,[Math]::Min(6,$Token.Length)))***" } else { Write-Host "  Token:     (dev 模式,免 token)" }
Write-Host "  安装目录:  $InstallDir"
Write-Host "  工作目录:  $Workspace"
if ($AgentId) { Write-Host "  Agent ID:  $AgentId" }
if ($Tags)    { Write-Host "  标签:      $($Tags -join ', ')" }
if ($Foreground) { Write-Host "  模式:      前台运行(不装服务)" } else { Write-Host "  模式:      Windows 服务(开机自启)" }
Write-Host "═══════════════════════════════════════════════════════════════"
Write-Host ""

# === 连通性预检 ===
Write-Step "连通性预检..."
# 从 Server URL 提取 host(依次去 scheme、路径、端口)
# 例: https://203.0.113.99/rmm → 203.0.113.99
#     http://host:8080/agent → host
$testHost = $Server -replace '^https?://',''   # 去 scheme
$testHost = $testHost -replace '/.*$',''        # 去路径
$testHost = $testHost -replace ':.*$',''        # 去端口
$testPort = if ($Server -match ':(\d+)') { [int]$Matches[1] } else { if ($Server -match '^https://') { 443 } else { 80 } }
$tcp = New-Object Net.Sockets.TcpClient
try {
    $tcp.Connect($testHost, $testPort)
    Write-OK "网络可达 $testHost`:$testPort"
    $tcp.Close()
} catch {
    Write-Err "无法连接 $testHost`:$testPort : $_"
    Write-Info "排查: 1) Relay 是否启动 2) 防火墙/安全组是否放行 3) IP 是否正确"
    exit 1
}

# === 创建目录 ===
Write-Step "创建目录..."
New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
New-Item -ItemType Directory -Force -Path $Workspace  | Out-Null
$DataDir = Join-Path $InstallDir "data"
New-Item -ItemType Directory -Force -Path $DataDir    | Out-Null

# === 下载二进制 ===
# M1:exe 名按 Role 区分(master/slave/默认),但都从同一个 dl/go-rmm-agent.exe 下载
$exeName = switch ($Role) {
    "master" { "go-rmm-master.exe" }
    "slave"  { "go-rmm-slave.exe" }
    default  { "go-rmm-agent.exe" }
}
$dstExe = Join-Path $InstallDir $exeName
$dlUrl = "$Server/dl/go-rmm-agent.exe"

Write-Step "下载 Agent 二进制: $dlUrl"
# 增加重试(大文件首次下载可能慢)
$dlOk = $false
for ($i = 1; $i -le 3; $i++) {
    try {
        $ProgressPreference = 'SilentlyContinue'
        Invoke-WebRequest -Uri $dlUrl -OutFile $dstExe -UseBasicParsing -TimeoutSec 120
        $dlOk = $true
        break
    } catch {
        Write-Info "下载重试 $i/3: $_"
        Start-Sleep -Seconds 2
    }
}
if (-not $dlOk) {
    Write-Err "下载失败,请检查:"
    Write-Info "  1. 在浏览器打开 $dlUrl 能否下载"
    Write-Info "  2. Relay 版本是否 ≥ v4.1(支持 /dl/ 端点)"
    Write-Info "  3. 若手动有 agent.exe,直接拷到 $dstExe 后重跑"
    exit 1
}
$size = (Get-Item $dstExe).Length
Write-OK "下载完成: $([math]::Round($size/1MB,1)) MB"

# === 生成 config.json(无 BOM UTF-8) ===
Write-Step "生成配置文件 config.json..."
$config = [ordered]@{
    relay_url = $Relay
    workspace = $Workspace
    allow_all_roots = $false
}
if ($Token)   { $config.token = $Token }
if ($AgentId) { $config.agent_id = $AgentId }
if ($Tags.Count -gt 0) {
    $tagMap = @{}
    foreach ($t in $Tags) {
        $parts = $t -split "=", 2
        if ($parts.Count -eq 2) { $tagMap[$parts[0]] = $parts[1] }
    }
    $config.tags = $tagMap
}
# M1 主从架构:写入 role + master 专属配置
if ($Role) {
    $config.role = $Role
    $config.service_name = $ServiceName
}
if ($Role -eq "master") {
    if ($SlaveExeDir)    { $config.slave_exe_dir = $SlaveExeDir }
    if ($SlaveAgentId)   { $config.slave_agent_id = $SlaveAgentId }
}
$configFile = Join-Path $InstallDir "config.json"
$json = $config | ConvertTo-Json -Depth 3
[System.IO.File]::WriteAllText($configFile, $json, (New-Object System.Text.UTF8Encoding $false))

# === 前台模式:直接启动 ===
if ($Foreground) {
    Write-Step "前台启动(调试模式,Ctrl+C 退出)..."
    $fgArgs = @("run", "-relay", $Relay)
    if ($Token) { $fgArgs += @("-token", $Token) }
    if ($Role)  { $fgArgs += @("-role", $Role) }
    & $dstExe @fgArgs
    exit 0
}

# === 服务模式:停旧→装新→启动 ===
if (Get-Service -Name $ServiceName -ErrorAction SilentlyContinue) {
    Write-Step "停止并卸载旧服务..."
    try {
        Stop-Service -Name $ServiceName -Force -ErrorAction SilentlyContinue
        Start-Sleep -Seconds 2
        & $dstExe uninstall 2>&1 | Out-Null
        Start-Sleep -Seconds 1
    } catch {}
}

Write-Step "安装 Windows 服务(开机自启 + 崩溃恢复)..."
# install 子命令会自动启动服务,无需再单独调 start
$installArgs = @("install", "-relay", $Relay)
if ($Token)   { $installArgs += @("-token", $Token) }
if ($AgentId) { $installArgs += @("-id", $AgentId) }
if ($Role)    { $installArgs += @("-role", $Role) }
& $dstExe @installArgs

Start-Sleep -Seconds 4

# === 验证(用 sc.exe 查询,最可靠) ===
$svcStatus = (sc.exe query $ServiceName 2>$null | Select-String "STATE" | Select-Object -First 1)
Write-Step "服务状态: $svcStatus"

# 兼容中英文:Running / 运行中 / RUNNING
$isRunning = $svcStatus -match "RUNNING|运行中" -or (& $dstExe status 2>$null) -match "Running=True"

if ($isRunning) {
    Write-Host ""
    Write-Host "═══════════════════════════════════════════════════════════════"
    Write-Host " ✅  go-rmm Agent 部署成功!" -ForegroundColor Green
    Write-Host "═══════════════════════════════════════════════════════════════"
    Write-Host "  服务名:    $ServiceName"
    Write-Host "  二进制:    $dstExe"
    Write-Host "  配置:      $configFile"
    Write-Host "  工作目录:  $Workspace"
    Write-Host "  Relay:     $Relay"
    Write-Host ""
    Write-Host " 管理命令:" -ForegroundColor Yellow
    Write-Host "   启动:     net start $ServiceName"
    Write-Host "   停止:     net stop $ServiceName"
    Write-Host "   重启:     Restart-Service $ServiceName"
    Write-Host "   状态:     Get-Service $ServiceName"
    Write-Host "   卸载:     .\install.ps1 -Uninstall -Server $Server"
    Write-Host ""
    Write-Host " 在控制端验证:" -ForegroundColor Yellow
    Write-Host "   go-rmm --relay $Server agent list"
    Write-Host "═══════════════════════════════════════════════════════════════"
} else {
    Write-Host ""
    Write-Host "⚠️  服务已安装但状态异常,排查步骤:" -ForegroundColor Yellow
    Write-Info "1. 前台调试: .\install.ps1 -Server $Server -Relay $Relay -Foreground"
    Write-Info "2. 事件日志: Get-EventLog -LogName Application -Source go-rmm-agent -Newest 20"
    Write-Info "3. 网络连通: Test-NetConnection $testHost -Port $testPort"
}
