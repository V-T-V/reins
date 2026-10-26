# reins Agent 一键部署脚本 — Windows
#
# 用法(管理员 PowerShell):
#   # 方式1: 直接运行
#   .\scripts\deploy-agent-windows.ps1 -RelayUrl "ws://your-relay:7780/agent"
#
#   # 方式2: 带 token
#   .\scripts\deploy-agent-windows.ps1 -RelayUrl "ws://your-relay:7780/agent" -Token "your-secret"
#
#   # 方式3: 一行命令(远程下载运行)
#   iex (New-Object Net.WebClient).DownloadString('https://raw.githubusercontent.com/V-T-V/reins/main/scripts/deploy-agent-windows.ps1')
#
# 功能:
#   1. 下载/编译 reins-agent.exe
#   2. 生成 config.json(RelayURL/Token/Tags)
#   3. 注册为 Windows 服务(开机自启 + 崩溃恢复)
#   4. 启动服务 + 验证

param(
    [Parameter(Mandatory=$true)]
    [string]$RelayUrl,

    [string]$Token = "",
    [string]$AgentId = "",
    [string]$InstallDir = "C:\Program Files\reins",
    [string]$Workspace = "C:\reins\workspace",
    [string[]]$Tags = @(),
    [string]$Version = "v3.9.0"
)

$ErrorActionPreference = "Stop"

# === 检查管理员 ===
if (-not ([Security.Principal.WindowsPrincipal] [Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    Write-Host "[ERROR] 需要管理员权限运行" -ForegroundColor Red
    Write-Host "  右键 PowerShell → 以管理员身份运行" -ForegroundColor Yellow
    exit 1
}

Write-Host "[INFO] reins Agent 部署脚本 v$Version" -ForegroundColor Green

# === 创建目录 ===
$DataDir = Join-Path $InstallDir "data"
New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
New-Item -ItemType Directory -Force -Path $DataDir | Out-Null
New-Item -ItemType Directory -Force -Path $Workspace | Out-Null
Write-Host "[INFO] 安装目录: $InstallDir"

# === 获取/编译二进制 ===
$AgentExe = Join-Path $InstallDir "reins-agent.exe"
$ConfigFile = Join-Path $InstallDir "config.json"

if (Test-Path "D:\M_X_M\reins") {
    # 本地开发环境:从源码编译
    Write-Host "[INFO] 检测到本地源码,编译中..."
    Push-Location "D:\M_X_M\reins"
    $env:CGO_ENABLED = "0"
    go build -ldflags "-s -w -X main.version=$Version" -o $AgentExe ./agent/
    Pop-Location
} elseif (Test-Path $AgentExe) {
    Write-Host "[INFO] 二进制已存在: $AgentExe"
} else {
    # 下载
    Write-Host "[INFO] 下载 reins-agent.exe..."
    $arch = if ([Environment]::Is64BitOperatingSystem) { "amd64" } else { "amd64" }
    $url = "https://github.com/V-T-V/reins/releases/download/$Version/reins-agent-windows-$arch.exe"
    try {
        Invoke-WebRequest -Uri $url -OutFile $AgentExe -UseBasicParsing
    } catch {
        Write-Host "[ERROR] 下载失败: $_" -ForegroundColor Red
        Write-Host "  请手动上传 reins-agent.exe 到: $InstallDir" -ForegroundColor Yellow
        exit 1
    }
}

# === 生成 config.json ===
$config = @{
    relay_url = $RelayUrl
    workspace = $Workspace
}
if ($Token) { $config.token = $Token }
if ($AgentId) { $config.agent_id = $AgentId }
if ($Tags.Count -gt 0) {
    $tagMap = @{}
    foreach ($t in $Tags) {
        $parts = $t -split "=", 2
        if ($parts.Count -eq 2) { $tagMap[$parts[0]] = $parts[1] }
    }
    $config.tags = $tagMap
}

$config | ConvertTo-Json -Depth 3 | Out-File -FilePath $ConfigFile -Encoding UTF8
Write-Host "[INFO] 配置文件: $ConfigFile"
Write-Host "  Relay: $RelayUrl"
if ($Token) { Write-Host "  Token: $($Token.Substring(0,4))***" }
if ($Tags) { Write-Host "  Tags: $($Tags -join ', ')" }

# === 安装服务 ===
$ServiceName = "reins-agent"

# 停止旧服务(如果存在)
if (Get-Service -Name $ServiceName -ErrorAction SilentlyContinue) {
    Write-Host "[INFO] 停止旧服务..."
    Stop-Service -Name $ServiceName -Force -ErrorAction SilentlyContinue
    Start-Sleep -Seconds 2
    sc.exe delete $ServiceName | Out-Null
    Start-Sleep -Seconds 1
}

Write-Host "[INFO] 安装 Windows 服务..."
& $AgentExe install -relay $RelayUrl $(if ($Token) { "-token $Token" })

Write-Host "[INFO] 启动服务..."
& $AgentExe start

Start-Sleep -Seconds 3

# === 验证 ===
$status = & $AgentExe status
Write-Host "[INFO] 服务状态: $status"

if ($status -match "Running") {
    Write-Host ""
    Write-Host "[SUCCESS] reins Agent 部署成功!" -ForegroundColor Green
    Write-Host ""
    Write-Host "  服务名: $ServiceName"
    Write-Host "  二进制: $AgentExe"
    Write-Host "  配置:   $ConfigFile"
    Write-Host "  工作目录: $Workspace"
    Write-Host "  Relay:  $RelayUrl"
    Write-Host ""
    Write-Host "  管理命令:"
    Write-Host "    停止: net stop $ServiceName"
    Write-Host "    启动: net start $ServiceName"
    Write-Host "    状态: sc.exe query $ServiceName"
    Write-Host "    卸载: & '$AgentExe' uninstall"
    Write-Host ""
    Write-Host "  在 Relay 端验证:"
    Write-Host "    curl http://relay-ip:7780/api/agents"
} else {
    Write-Host "[WARN] 服务可能未正常启动,检查:" -ForegroundColor Yellow
    Write-Host "  & '$AgentExe' run -relay $RelayUrl  (前台调试)"
}
