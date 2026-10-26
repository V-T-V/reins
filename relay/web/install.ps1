# ╔══════════════════════════════════════════════════════════════╗
# ║  reins Agent 一行命令远程部署 — Windows(无脑化版)           ║
# ║  在目标 Win10 机器上执行一行命令即可自动下载+安装+注册服务     ║
# ╚══════════════════════════════════════════════════════════════╝
#
# 用法(在目标机器执行):
#
#   # 方式1(推荐·零参数全自动):管理员 PowerShell 一行命令。
#   # Relay 在 /install.ps1 服务时已把地址烘焙进脚本,无需任何参数与交互;
#   # 非管理员窗口运行会自动弹 UAC 提权后重跑;装完自动在 Relay 侧确认上线。
#   # Relay 配置了 -install-token(部署专用 Token)时,Token 也已烘焙,同样零参数。
#   # 双保险去 BOM:服务端保留 BOM 供 PS 5.1 文件执行;字符串路径先剥 U+FEFF,
#   # 再剥 Latin-1 误读的 ï»¿(nginx 未带 charset 时 irm 按 Latin-1 解码,否则
#   # 第 1 行变幻影命令,param() 不再是首语句会报"赋值表达式无效")。
#   iex (((irm 'https://203.0.113.99/rmm/install.ps1') -replace '^\uFEFF') -replace '^\u00EF\u00BB\u00BF')
#
#   #   · Relay 仅配主 Token(未配 -install-token)时,补一个参数:
#   & ([scriptblock]::Create((((irm 'https://203.0.113.99/rmm/install.ps1') -replace '^\uFEFF') -replace '^\u00EF\u00BB\u00BF'))) -Token 'xxx'
#   #   · 或先设环境变量再零参数运行:  $env:REINS_TOKEN = 'xxx'
#
#   # 方式2:显式参数(先下载脚本再跑;烘焙地址存在时参数全部可省)
#   irm 'https://203.0.113.99/rmm/install.ps1' -OutFile install.ps1
#   .\install.ps1 -Server 'https://203.0.113.99/rmm' -Relay 'wss://203.0.113.99/rmm/agent' -Token 'xxx'
#   #   注:下载的文件带 BOM,PS 5.1/7 直接运行均可;PS 7 也可 . (irm ... -OutFile tmp; . tmp) 等
#
#   # 方式3:前台调试(不装服务,无需管理员)
#   .\install.ps1 -Foreground
#
# 卸载:
#   .\install.ps1 -Uninstall
#
# 特点:
#   - 无源码依赖、无 GitHub 依赖(二进制从 Relay 的 /dl/ 端点拉取)
#   - 地址自动烘焙 + Token 自动烘焙/探测 + 管理员自动提权 + 上线自动确认
#   - 完整的失败排查提示

param(
    [string]$Server = "",
    [string]$Relay = "",
    [string]$Token = "",
    [string]$AgentId = "",
    [string[]]$Tags = @(),
    [string]$InstallDir = "C:\Program Files\reins",
    [string]$Workspace = "C:\reins\workspace",
    [switch]$Foreground,
    [switch]$Uninstall,
    # M1 主从架构:支持装 master/slave 角色
    [ValidateSet("", "master", "slave")]
    [string]$Role = "",
    [string]$SlaveExeDir = "",    # master 专属:从的 exe 目录
    [string]$SlaveAgentId = "",   # master 专属:从的 agent_id
    [string]$Version = "v4.3.3"
)

$ErrorActionPreference = "Stop"
$ProgressPreference = 'SilentlyContinue'

# === 烘焙默认值 ===
# Relay 在 /install.ps1 服务时注入本机外部地址与部署专用 Token(如已配置
# -install-token),使零参数 iex 全自动部署——地址与 Token 都无需中间传递。
# 占位符未被替换(静态副本/直接读源码)时视为空,走交互兜底。
$BakedServer = "__REINS_SERVER_URL__"
$BakedRelay  = "__REINS_RELAY_URL__"
$BakedToken  = "__REINS_TOKEN__"
if ($BakedServer -match '^__.*__$') { $BakedServer = "" }
if ($BakedRelay  -match '^__.*__$') { $BakedRelay  = "" }
if ($BakedToken  -match '^__.*__$') { $BakedToken  = "" }

# 服务名按 Role 推导
$ServiceName = switch ($Role) {
    "master" { "reins-master" }
    "slave"  { "reins-slave" }
    default  { "reins-agent" }
}

# 主从模式默认目录
if ($Role -eq "master" -and $InstallDir -eq "C:\Program Files\reins") {
    $InstallDir = "C:\Program Files\reins-master"
    $Workspace = "C:\reins\master-workspace"
}
if ($Role -eq "slave" -and $InstallDir -eq "C:\Program Files\reins") {
    $InstallDir = "C:\Program Files\reins-slave"
    $Workspace = "C:\reins\slave-workspace"
}

# 前台模式(无需管理员):默认装到用户目录
if ($Foreground -and $InstallDir -eq "C:\Program Files\reins") {
    $InstallDir = Join-Path $env:LOCALAPPDATA "reins"
}

# === 辅助函数 ===
function Write-Step($msg) { Write-Host "[步骤] $msg" -ForegroundColor Cyan }
function Write-OK($msg)   { Write-Host "[OK]    $msg" -ForegroundColor Green }
function Write-Err($msg)  { Write-Host "[错误]  $msg" -ForegroundColor Red }
function Write-Info($msg) { Write-Host "[信息]  $msg" -ForegroundColor Yellow }
function Quote-PSVal($s)  { if ($s) { "'" + ($s -replace "'", "''") + "'" } else { "''" } }

# Get-HTTPStatus 探测 URL 的 HTTP 状态码;网络层错误(连不上/超时/DNS)返回 -1。
function Get-HTTPStatus([string]$Url, [hashtable]$Headers) {
    try {
        Invoke-WebRequest -Uri $Url -Headers $Headers -UseBasicParsing -TimeoutSec 15 | Out-Null
        return 200
    } catch {
        $resp = $_.Exception.Response
        if ($resp) { try { return [int]$resp.StatusCode } catch { return -1 } }
        return -1
    }
}

# Wait-AgentOnline 轮询 Relay /api/agents,直到本机 Agent 上线或超时。
# 匹配规则:精确 agent_id(-AgentId 指定时) > hostname > agent-<hostname>-* 前缀。
function Wait-AgentOnline([string]$ApiBase, [string]$Tok, [string]$WantId, [int]$TimeoutSec) {
    $h = @{}
    if ($Tok) { $h["Authorization"] = "Bearer $Tok" }
    $hostPrefix = "agent-" + $env:COMPUTERNAME + "-"
    $deadline = (Get-Date).AddSeconds($TimeoutSec)
    while ((Get-Date) -lt $deadline) {
        try {
            $resp = Invoke-RestMethod -Uri "$ApiBase/api/agents" -Headers $h -TimeoutSec 10
            $hit = @($resp.agents | Where-Object {
                ($WantId -and $_.agent_id -eq $WantId) -or
                (-not $WantId -and ($_.hostname -eq $env:COMPUTERNAME -or $_.agent_id -like "$hostPrefix*"))
            })
            if ($hit.Count -gt 0) { return @{ online = $true; agents = $hit } }
        } catch {
            $code = 0
            if ($_.Exception.Response) { try { $code = [int]$_.Exception.Response.StatusCode } catch {} }
            if ($code -eq 401) { return @{ online = $false; err = "unauthorized" } }
        }
        Start-Sleep -Seconds 5
    }
    return @{ online = $false }
}

# === 卸载模式 ===
if ($Uninstall) {
    Write-Host "reins Agent 卸载" -ForegroundColor Cyan
    $exePath = Join-Path $InstallDir "reins-agent.exe"
    if ($InstallDir -eq "C:\Program Files\reins") {
        foreach ($p in @("$env:LOCALAPPDATA\reins\reins-agent.exe", "C:\Program Files\reins\reins-agent.exe")) {
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
        Write-Info "未找到 reins-agent.exe(可能本机未安装)"
    }
    exit 0
}

# === 检查管理员权限(服务模式) ===
# 无脑化:非管理员窗口自动弹 UAC 提权,用烘焙地址重新下载脚本在新窗口重跑。
if (-not $Foreground) {
    $isAdmin = ([Security.Principal.WindowsPrincipal] [Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
    if (-not $isAdmin) {
        $elevBase = $BakedServer
        if (-not $elevBase -and $Server) { $elevBase = ($Server -replace '/+$', '') }
        if ($elevBase -and [Environment]::UserInteractive) {
            Write-Info "安装服务需要管理员权限,正在弹出 UAC 提权后自动重跑..."
            $scriptUrl = ($elevBase -replace '/+$', '') + "/install.ps1"
            # 双保险去 BOM:先剥 U+FEFF,再剥 Latin-1 误读的 ï»¿(nginx 无 charset 时)
            $inner = "iex (((irm $(Quote-PSVal $scriptUrl)) -replace '^\uFEFF') -replace '^\u00EF\u00BB\u00BF')"
            $extra = ""
            if ($Token)        { $extra += " -Token $(Quote-PSVal $Token)" }
            if ($AgentId)      { $extra += " -AgentId $(Quote-PSVal $AgentId)" }
            if ($Tags.Count -gt 0) { $extra += " -Tags " + (($Tags | ForEach-Object { Quote-PSVal $_ }) -join ",") }
            if ($Role)         { $extra += " -Role $(Quote-PSVal $Role)" }
            if ($SlaveExeDir)  { $extra += " -SlaveExeDir $(Quote-PSVal $SlaveExeDir)" }
            if ($SlaveAgentId) { $extra += " -SlaveAgentId $(Quote-PSVal $SlaveAgentId)" }
            if ($extra) { $inner = "& ([scriptblock]::Create((((irm $(Quote-PSVal $scriptUrl)) -replace '^\uFEFF') -replace '^\u00EF\u00BB\u00BF')))" + $extra }
            $elevScript = Join-Path $env:TEMP "reins-elevate-$(Get-Random).ps1"
            $inner += "; `$null = Read-Host '部署结束,按回车关闭'; Remove-Item -LiteralPath $(Quote-PSVal $elevScript) -ErrorAction SilentlyContinue"
            [System.IO.File]::WriteAllText($elevScript, $inner, (New-Object System.Text.UTF8Encoding $true))
            try {
                Start-Process powershell.exe -Verb RunAs -ArgumentList @('-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', ('"' + $elevScript + '"'))
                Write-OK "已在新管理员窗口继续部署,请在 UAC 弹窗确认后查看新窗口输出"
            } catch {
                Write-Err "UAC 提权被取消: $_"
                Write-Info "请右键 PowerShell → 以管理员身份运行,再执行一次安装命令"
            }
            exit 0
        }
        Write-Err "安装服务需要管理员权限"
        Write-Info "请右键 PowerShell → 以管理员身份运行"
        Write-Info "或加 -Foreground 参数前台调试(无需管理员)"
        exit 1
    }
}

# === 推导 Server / Relay ===
# 优先级: 显式参数 > 烘焙值(Relay 注入) > 从另一项推导 > 交互兜底(仅静态副本场景)
if (-not $Server) {
    if ($BakedServer) {
        $Server = $BakedServer
    } elseif ($Relay) {
        # 从 Relay URL 推导 Server: ws://host:7780/agent → http://host:7780
        $Server = $Relay -replace '^wss?://', 'http' -replace '/agent$', ''
        if ($Relay -match '^wss://') { $Server = $Server -replace '^http://', 'https://' }
    } else {
        # 交互兜底(仅当脚本未经 Relay 服务且未传任何参数)
        Write-Host ""
        Write-Host "reins Agent 部署向导" -ForegroundColor Cyan
        $Server = Read-Host "请输入 Relay 地址(如 http://203.0.113.99:7780)"
        if (-not $Server) { Write-Err "Server 地址必填"; exit 1 }
    }
}
if (-not $Relay) {
    if ($BakedRelay -and $Server -eq $BakedServer) {
        $Relay = $BakedRelay
    } else {
        # 从 Server 推导 Relay
        $scheme = "ws"
        if ($Server -match '^https://') { $scheme = "wss" }
        $hostPort = $Server -replace '^https?://', ''
        $Relay = "${scheme}://${hostPort}/agent"
    }
}

# Token 优先级: 显式参数 > 环境变量 REINS_TOKEN > 烘焙值(Relay -install-token 注入) > 留空(dev)。
# 若 Relay 实际启用了鉴权,由下方 API 预检发现 401 后再补问,不做无谓提问。
if (-not $Token) { $Token = $env:REINS_TOKEN }
if (-not $Token) { $Token = $BakedToken }

# === 横幅 ===
Write-Host ""
Write-Host "═══════════════════════════════════════════════════════════════"
Write-Host " reins Agent $Version 远程部署" -ForegroundColor Cyan
Write-Host "═══════════════════════════════════════════════════════════════"
Write-Host "  Server:    $Server"
Write-Host "  Relay:     $Relay"
if ($Token) { Write-Host "  Token:     $($Token.Substring(0, [Math]::Min(6, $Token.Length)))***" } else { Write-Host "  Token:     (dev 模式,免 token)" }
Write-Host "  安装目录:  $InstallDir"
Write-Host "  工作目录:  $Workspace"
if ($AgentId) { Write-Host "  Agent ID:  $AgentId" }
if ($Tags)    { Write-Host "  标签:      $($Tags -join ', ')" }
if ($Foreground) { Write-Host "  模式:      前台运行(不装服务)" } else { Write-Host "  模式:      Windows 服务(开机自启)" }
Write-Host "═══════════════════════════════════════════════════════════════"
Write-Host ""

# === 连通性预检(TCP) ===
Write-Step "连通性预检..."
# 从 Server URL 提取 host(依次去 scheme、路径、端口)
# 例: https://203.0.113.99/rmm → 203.0.113.99
#     http://host:8080/agent → host
$testHost = $Server -replace '^https?://', ''   # 去 scheme
$testHost = $testHost -replace '/.*$', ''        # 去路径
$testHost = $testHost -replace ':.*$', ''        # 去端口
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

# === 连通性预检(HTTP API + Token 探测) ===
# 200 = dev 模式或 Token 有效;401 = 需要鉴权;-1 = API 不可达(仅警告,TCP 已通过)
if (-not $Foreground) {
    Write-Step "探测 Relay API 鉴权..."
    $probeHeaders = @{}
    if ($Token) { $probeHeaders["Authorization"] = "Bearer $Token" }
    $status = Get-HTTPStatus -Url "$Server/api/agents" -Headers $probeHeaders
    if ($status -eq 401) {
        if (-not $Token) {
            if (-not [Environment]::UserInteractive) {
                Write-Err "Relay 启用了鉴权,需要 Token:请加 -Token 参数或设 `$env:REINS_TOKEN 后重跑"
                exit 1
            }
            $Token = Read-Host "Relay 启用了鉴权,请输入 Token"
            if (-not $Token) { Write-Err "Token 必填"; exit 1 }
            $probeHeaders["Authorization"] = "Bearer $Token"
            $status = Get-HTTPStatus -Url "$Server/api/agents" -Headers $probeHeaders
            if ($status -eq 401) {
                Write-Info "输入的 Token 未获 REST 认可,以 Agent 实际连接为准"
            } else {
                Write-OK "Token 验证通过"
            }
        } else {
            # 部署专用 Token(-install-token)只授权 Agent 注册与 /api/agents;
            # 若连 /api/agents 也 401(如仅有 WS 权限或 Token 有误),不阻断安装,
            # 由 Agent 实际连接与最终上线确认环节裁决。
            Write-Info "Token 无 REST 查询权限(可能为部署专用 Token 或有误),以 Agent 实际连接为准"
        }
    } elseif ($status -eq 200) {
        Write-OK "API 可达$(if ($Token) { ',Token 有效' })"
    } else {
        Write-Info "API 预检未通过(状态 $status),继续尝试安装(以 Agent 实际连接为准)"
    }
}

# === 创建目录 ===
Write-Step "创建目录..."
New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
New-Item -ItemType Directory -Force -Path $Workspace  | Out-Null
$DataDir = Join-Path $InstallDir "data"
New-Item -ItemType Directory -Force -Path $DataDir    | Out-Null

# === 下载二进制 ===
# M1:exe 名按 Role 区分(master/slave/默认),但都从同一个 dl/reins-agent.exe 下载
$exeName = switch ($Role) {
    "master" { "reins-master.exe" }
    "slave"  { "reins-slave.exe" }
    default  { "reins-agent.exe" }
}
$dstExe = Join-Path $InstallDir $exeName
$dlUrl = "$Server/dl/reins-agent.exe"

Write-Step "下载 Agent 二进制: $dlUrl"
# 增加重试(大文件首次下载可能慢)
$dlOk = $false
for ($i = 1; $i -le 3; $i++) {
    try {
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

# === 本地验证(用 sc.exe 查询,最可靠) ===
$svcStatus = (sc.exe query $ServiceName 2>$null | Select-String "STATE" | Select-Object -First 1)
Write-Step "服务状态: $svcStatus"

# 兼容中英文:Running / 运行中 / RUNNING
$isRunning = $svcStatus -match "RUNNING|运行中" -or (& $dstExe status 2>$null) -match "Running=True"

# === 远端验证:等待 Agent 经 Relay 上线 ===
$onlineInfo = $null
if ($isRunning) {
    Write-Step "等待 Agent 在 Relay 注册上线(最长 60s)..."
    $onlineInfo = Wait-AgentOnline -ApiBase $Server -Tok $Token -WantId $AgentId -TimeoutSec 60
    if ($onlineInfo.online) {
        foreach ($a in $onlineInfo.agents) {
            Write-OK "已上线: $($a.agent_id) ($($a.os), v$($a.version))"
        }
    } elseif ($onlineInfo.err -eq "unauthorized") {
        Write-Info "Token 无权查询 /api/agents,跳过上线自动确认(服务已运行)"
    } else {
        Write-Info "60s 内未在 Relay 观察到本机 Agent(可能仍在连接中)"
    }
}

if ($isRunning -and $onlineInfo -and $onlineInfo.online) {
    Write-Host ""
    Write-Host "═══════════════════════════════════════════════════════════════"
    Write-Host " ✅  reins Agent 部署成功(已在 Relay 上线确认)!" -ForegroundColor Green
    Write-Host "═══════════════════════════════════════════════════════════════"
    Write-Host "  服务名:    $ServiceName"
    Write-Host "  Agent ID:  $($onlineInfo.agents[0].agent_id)"
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
    Write-Host "   reins --relay $Server agent list"
    Write-Host "═══════════════════════════════════════════════════════════════"
} elseif ($isRunning) {
    Write-Host ""
    Write-Host "⚠️  服务已运行,但未完成上线确认(不影响服务本身),排查步骤:" -ForegroundColor Yellow
    Write-Info "1. 稍后手动核对: reins --relay $Server agent list"
    Write-Info "2. 前台调试:     .\install.ps1 -Server $Server -Foreground"
    Write-Info "3. 事件日志:     Get-EventLog -LogName Application -Source reins-agent -Newest 20"
    Write-Info "4. 网络连通:     Test-NetConnection $testHost -Port $testPort"
    Write-Info "5. 若 Relay 启用了鉴权,确认 Token 正确后重跑本命令"
} else {
    Write-Host ""
    Write-Host "⚠️  服务已安装但状态异常,排查步骤:" -ForegroundColor Yellow
    Write-Info "1. 前台调试: .\install.ps1 -Server $Server -Relay $Relay -Foreground"
    Write-Info "2. 事件日志: Get-EventLog -LogName Application -Source reins-agent -Newest 20"
    Write-Info "3. 网络连通: Test-NetConnection $testHost -Port $testPort"
}
