# reins-relay 启动器(随 server zip 分发)
#
# 用法(管理员 PowerShell,在解压后的 reins-server 目录内):
#   .\start-relay.ps1 -Token <你的Token>                      # 前台运行(快速试用,Ctrl+C 停)
#   .\start-relay.ps1 -Token <你的Token> -Mode boot           # 注册开机自启(计划任务,SYSTEM,隐藏窗口,日志写文件)
#   .\start-relay.ps1 -Mode uninstall                         # 移除开机自启
#   .\start-relay.ps1 -Token <你的Token> -UseTLS              # 生成自签证书并以 HTTPS(8443) 启动
#
# 数据落盘位置:与 reins-relay.exe 同目录的 data\(tasks.db/audit.log/CA/Agent 二进制仓库)。
# 端口默认 7780(HTTP);-UseTLS 时 8443(自签)。修改端口用 -Port。
param(
    [Parameter(Mandatory = $false)]
    [string]$Token = "",
    [int]$Port = 7780,
    [ValidateSet("foreground", "boot", "uninstall")]
    [string]$Mode = "foreground",
    [switch]$UseTLS,
    [string]$TaskName = "reins-relay"
)

$ErrorActionPreference = "Stop"
$Here = Split-Path -Parent $MyInvocation.MyCommand.Path
$Exe = Join-Path $Here "reins-relay.exe"
if (-not (Test-Path $Exe)) { Write-Host "[ERROR] 找不到 reins-relay.exe(须在本脚本同目录运行)" -ForegroundColor Red; exit 1 }

function Open-Firewall([int]$p) {
    $rule = "reins-relay-$p"
    if (-not (Get-NetFirewallRule -Name $rule -ErrorAction SilentlyContinue)) {
        New-NetFirewallRule -Name $rule -DisplayName "reins relay $p" -Direction Inbound -Protocol TCP -LocalPort $p -Action Allow | Out-Null
        Write-Host "[OK] 防火墙入站规则已放行 TCP/$p"
    }
}

function Require-Admin {
    $isAdmin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
    if (-not $isAdmin) { Write-Host "[ERROR] 需要管理员权限(右键 PowerShell → 以管理员身份运行)" -ForegroundColor Red; exit 1 }
}

# --- 组装 relay 参数 ---
$relayArgs = @("-addr", ":$Port")
if ($Token) { $relayArgs += @("-token", $Token) }
if ($UseTLS) {
    $certDir = Join-Path $Here "certs"
    New-Item -ItemType Directory -Force -Path $certDir | Out-Null
    $crt = Join-Path $certDir "relay.crt"; $key = Join-Path $certDir "relay.key"
    if (-not (Test-Path $crt) -or -not (Test-Path $key)) {
        Write-Host "[INFO] 生成自签证书(825 天,含本机全部 IP;公网部署请换正规 CA)"
        & (Join-Path $Here "reins-gencert.exe") -out-cert $crt -out-key $key
        if ($LASTEXITCODE -ne 0) { Write-Host "[ERROR] 证书生成失败" -ForegroundColor Red; exit 1 }
    }
    $relayArgs += @("-mtls", "-tls-cert", $crt, "-tls-key", $key)
}

switch ($Mode) {
    "uninstall" {
        Require-Admin
        $t = Get-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue
        if ($t) {
            Stop-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue
            Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false
            Write-Host "[OK] 开机自启已移除"
        } else { Write-Host "[INFO] 未注册开机自启" }
        return
    }
    "boot" {
        Require-Admin
        Open-Firewall $Port
        $dataDir = Join-Path $Here "data"
        New-Item -ItemType Directory -Force -Path $dataDir | Out-Null
        $log = Join-Path $dataDir "relay.log"
        $argLine = ('-WindowStyle Hidden -Command "& ''{0}'' {1} *>> ''{2}''"' -f $Exe, (($relayArgs | ForEach-Object { "'$_'" }) -join " "), $log)
        $action = New-ScheduledTaskAction -Execute "powershell.exe" -Argument $argLine
        $trigger = New-ScheduledTaskTrigger -AtStartup
        $settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -RestartCount 3 -RestartInterval (New-TimeSpan -Minutes 1) -ExecutionTimeLimit (New-TimeSpan -Days 3650)
        $principal = New-ScheduledTaskPrincipal -UserId "SYSTEM" -RunLevel Highest
        if (Get-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue) {
            Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false
        }
        Register-ScheduledTask -TaskName $TaskName -Action $action -Trigger $trigger -Settings $settings -Principal $principal | Out-Null
        Start-ScheduledTask -TaskName $TaskName
        Write-Host "[OK] reins-relay 已注册开机自启并启动(计划任务 $TaskName,崩溃 1 分钟内自动重启,最多 3 次)"
        $scheme = if ($UseTLS) { "https" } else { "http" }
        Write-Host "     控制台: $scheme://<本机IP>:$Port/   日志: $log"
        return
    }
    default {
        Open-Firewall $Port
        $scheme = if ($UseTLS) { "https" } else { "http" }
        Write-Host "[INFO] 前台启动 $scheme://0.0.0.0:$Port  (Ctrl+C 停止;生产用 -Mode boot)"
        & $Exe @relayArgs
    }
}
