# go-rmm 一行安装脚本 — Windows
#
# 用法(管理员 PowerShell):
#   iex (New-Object Net.WebClient).DownloadString('http://your-server:7780/install.ps1')
#
# 或本地:
#   .\scripts\install.ps1
#   .\scripts\install.ps1 -Server "http://relay:7780"
#   .\scripts\install.ps1 -InstallAsService -Relay "ws://relay:7780/agent" -Token "xxx"
#
# 功能:
#   1. 下载 go-rmm 三合一包(relay + agent + cli)
#   2. 安装到 C:\Program Files\go-rmm\
#   3. 添加到系统 PATH
#   4. 可选:启动 Relay / 注册 Agent 服务
param(
    [string]$Server = "",
    [string]$InstallDir = "C:\Program Files\go-rmm",
    [string]$Version = "v3.5.0",
    [switch]$InstallAsService,
    [string]$Relay = "",
    [string]$Token = ""
)

$ErrorActionPreference = "Stop"
$BinDir = Join-Path $InstallDir "bin"

Write-Host "go-rmm $Version 安装程序" -ForegroundColor Cyan
Write-Host ""

# === 检测架构 ===
$arch = if ([Environment]::Is64BitOperatingSystem) { "amd64" } else { "amd64" }
Write-Host "[INFO] 系统: windows/$arch"

# === 创建目录 ===
New-Item -ItemType Directory -Force -Path $BinDir | Out-Null
Write-Host "[INFO] 安装目录: $InstallDir"

# === 获取二进制 ===
$needBuild = $false

# 尝试从源码编译(如果有 Go + 源码)
if (Test-Path "D:\M_X_M\go-rmm") {
    Write-Host "[INFO] 检测到本地源码,编译中..."
    Push-Location "D:\M_X_M\go-rmm"
    $env:CGO_ENABLED = "0"
    go build -ldflags "-s -w -X main.version=$Version" -o "$BinDir\go-rmm-relay.exe" ./relay/
    go build -ldflags "-s -w -X main.version=$Version" -o "$BinDir\go-rmm-agent.exe" ./agent/
    go build -ldflags "-s -w -X main.version=$Version" -o "$BinDir\go-rmm.exe" ./cmd/go-rmm/
    Pop-Location
    Write-Host "[INFO] 编译完成"
}
# 尝试从服务器下载
elseif ($Server) {
    Write-Host "[INFO] 从 $Server 下载..."
    foreach ($bin in @("go-rmm-relay", "go-rmm-agent", "go-rmm")) {
        $url = "$Server/dl/$bin-windows-$arch.exe"
        try {
            Invoke-WebRequest -Uri $url -OutFile "$BinDir\$bin.exe" -UseBasicParsing
            Write-Host "  [OK] $bin"
        } catch {
            Write-Host "  [WARN] $bin 下载失败" -ForegroundColor Yellow
        }
    }
}
else {
    Write-Host "[ERROR] 无源码且未指定 -Server" -ForegroundColor Red
    Write-Host "  用法: .\install.ps1 -Server http://your-relay:7780" -ForegroundColor Yellow
    exit 1
}

# === 添加到 PATH ===
$currentPath = [Environment]::GetEnvironmentVariable("Path", "Machine")
if ($currentPath -notlike "*$BinDir*") {
    [Environment]::SetEnvironmentVariable("Path", "$currentPath;$BinDir", "Machine")
    Write-Host "[INFO] PATH 已更新(重新打开终端生效)"
}
$env:Path += ";$BinDir"

# === 生成配置(零配置) ===
$ConfigFile = Join-Path $InstallDir "go-rmm.json"
if (-not (Test-Path $ConfigFile)) {
    if (-not $Token) {
        $bytes = New-Object byte[] 16
        ([Security.Cryptography.RandomNumberGenerator]::Create()).GetBytes($bytes)
        $Token = -join ($bytes | ForEach-Object { $_.ToString("x2") })
    }
    @{ token = $Token; addr = ":7780" } | ConvertTo-Json | Out-File -FilePath $ConfigFile -Encoding UTF8
    Write-Host "[INFO] Token: $Token" -ForegroundColor Green
}

# === 安装服务(可选) ===
if ($InstallAsService -and $Relay) {
    Write-Host "[INFO] 注册 Agent 服务..."
    & "$BinDir\go-rmm-agent.exe" install -relay $Relay -token $Token
    & "$BinDir\go-rmm-agent.exe" start
}

# === 完成 ===
Write-Host ""
Write-Host "go-rmm $Version 安装完成!" -ForegroundColor Green
Write-Host ""
Write-Host "服务端: go-rmm-relay -addr :7780"
Write-Host "Agent:  go-rmm-agent run -relay ws://<relay-ip>:7780/agent -token <token>"
Write-Host "CLI:    go-rmm --relay http://<relay-ip>:7780 --token <token> agent list"
Write-Host ""
