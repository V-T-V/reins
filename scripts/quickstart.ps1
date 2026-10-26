# ╔══════════════════════════════════════════════════════════╗
# ║  reins 极简安装 — Windows(一行命令,3 步完成)            ║
# ╚══════════════════════════════════════════════════════════╝
#
# 用法(管理员 PowerShell):
#   .\scripts\quickstart.ps1                              # 服务端(本机)
#   .\scripts\quickstart.ps1 -Mode agent -Relay ws://relay:7780/agent -Token xxx
#   .\scripts\quickstart.ps1 -Mode cli                    # 仅 CLI
#
# 服务端一行(远程):
#   iex (irm http://your-server:7780/q.ps1)
param(
    [string]$Mode = "server",
    [string]$Relay = "",
    [string]$Token = "",
    [string]$Dir = "$env:USERPROFILE\.reins",
    [string]$Version = "v3.9.0"
)

$ErrorActionPreference = "Stop"
$Bin = Join-Path $Dir "bin"
New-Item -ItemType Directory -Force -Path $Bin | Out-Null

function Get-Token {
    $tokenFile = Join-Path $Dir "token"
    if (Test-Path $tokenFile) { return Get-Content $tokenFile }
    $bytes = New-Object byte[] 16
    [System.Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($bytes)
    $t = -join ($bytes | ForEach-Object { $_.ToString("x2") })
    Set-Content -Path $tokenFile -Value $t -Encoding UTF8
    return $t
}

# === 编译 ===
Write-Host "reins $Version" -ForegroundColor Cyan

if (Test-Path "D:\M_X_M\reins\go.mod") {
    Write-Host "[编译] 从源码..." -ForegroundColor Green
    Push-Location "D:\M_X_M\reins"
    $env:CGO_ENABLED = "0"
    go build -ldflags "-s -w -X main.version=$Version" -o "$Bin\reins-relay.exe" ./relay/
    go build -ldflags "-s -w -X main.version=$Version" -o "$Bin\reins-agent.exe" ./agent/
    go build -ldflags "-s -w -X main.version=$Version" -o "$Bin\reins.exe" ./cmd/reins/
    Pop-Location
} else {
    Write-Host "[错误] 需要在源码目录运行,或指定下载地址" -ForegroundColor Red
    exit 1
}

# PATH
$currentPath = [Environment]::GetEnvironmentVariable("Path", "User")
if ($currentPath -notlike "*$Bin*") {
    [Environment]::SetEnvironmentVariable("Path", "$currentPath;$Bin", "User")
    Write-Host "[PATH] 已更新(重开终端生效)" -ForegroundColor Green
}
$env:Path += ";$Bin"

$IP = (Get-NetIPAddress -AddressFamily IPv4 | Where-Object { $_.InterfaceAlias -notmatch 'Loopback' -and $_.IPAddress -notmatch '^169' } | Select-Object -First 1).IPAddress
if (-not $IP) { $IP = "127.0.0.1" }

if ($Mode -eq "server" -or $Mode -eq "serve") {
    # === 服务端 ===
    if (-not $Token) { $Token = Get-Token }

    Write-Host "[启动] Relay..." -ForegroundColor Green
    Start-Process -FilePath "$Bin\reins-relay.exe" -ArgumentList "-addr", ":7780", "-token", $Token -WindowStyle Minimized
    Start-Sleep 3

    Write-Host ""
    Write-Host "reins 服务端已就绪!" -ForegroundColor Green
    Write-Host ""
    Write-Host "  Web UI:  http://$IP`:7780"
    Write-Host "  Token:   $Token"
    Write-Host ""
    Write-Host "  在被控端执行:" -ForegroundColor Yellow
    Write-Host "    reins-agent.exe run -relay ws://$IP`:7780/agent -token $Token"
    Write-Host ""
    Write-Host "  在操作端执行:" -ForegroundColor Yellow
    Write-Host "    reins --relay http://$IP`:7780 --token $Token agent list"
    Write-Host ""

} elseif ($Mode -eq "agent") {
    # === Agent ===
    if (-not $Relay) { Write-Host "需要 -Relay 参数" -ForegroundColor Red; exit 1 }
    if (-not $Token) { $Token = Get-Token }

    Write-Host "[启动] Agent..." -ForegroundColor Green
    Start-Process -FilePath "$Bin\reins-agent.exe" -ArgumentList "run", "-relay", $Relay, "-token", $Token -WindowStyle Minimized
    Start-Sleep 2

    Write-Host "Agent 已连接!" -ForegroundColor Green

} elseif ($Mode -eq "cli") {
    Write-Host "CLI 就绪!" -ForegroundColor Green
    Write-Host "  reins --relay http://<relay-ip>:7780 --token <token> agent list"
}
