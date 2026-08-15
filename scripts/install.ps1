# ╔══════════════════════════════════════════════════════════╗
# ║  go-rmm Windows 安装器（从 GitHub Release 下载二进制）      ║
# ╚══════════════════════════════════════════════════════════╝
#
# 用法（管理员 PowerShell）:
#   .\scripts\install.ps1                          # 安装最新版 Relay
#   .\scripts\install.ps1 -Version v4.4.0          # 指定版本
#   .\scripts\install.ps1 -Component agent         # 安装 Agent（agent|relay|cli）
#   .\scripts\install.ps1 -Component agent -Service   # 安装 Agent 并注册 Windows 服务
param(
    [string]$Owner = "V-T-V",
    [string]$Repo = "go-rmm",
    [string]$Version = "latest",
    [ValidateSet("relay", "agent", "cli")][string]$Component = "relay",
    [switch]$Service,
    [string]$InstallDir = "$env:ProgramFiles\go-rmm"
)
$ErrorActionPreference = "Stop"

# latest → 实际版本号
if ($Version -eq "latest") {
    $rel = Invoke-RestMethod "https://api.github.com/repos/$Owner/$Repo/releases/latest"
    $Version = $rel.tag_name
    if (-not $Version) { throw "无法解析最新版本（检查网络或指定 -Version）" }
}

$Binary = "go-rmm-$Component.exe"
$Archive = "go-rmm-${Component}_${Version}_windows_amd64.zip"
$Url = "https://github.com/$Owner/$Repo/releases/download/$Version/$Archive"
$Tmp = Join-Path $env:TEMP "go-rmm-install-$(Get-Random)"
New-Item -ItemType Directory -Force -Path $Tmp | Out-Null

Write-Host "↓ 下载 $Url" -ForegroundColor Cyan
$ZipPath = Join-Path $Tmp $Archive
Invoke-WebRequest -Uri $Url -OutFile $ZipPath -UseBasicParsing
Expand-Archive -Path $ZipPath -DestinationPath $Tmp -Force

if (-not (Test-Path (Join-Path $Tmp $Binary))) {
    throw "解压后未找到 $Binary（archive 结构可能已变）"
}

New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
Move-Item -Force (Join-Path $Tmp $Binary) (Join-Path $InstallDir $Binary)

# 更新系统 PATH
$path = [Environment]::GetEnvironmentVariable("Path", "Machine")
if ($path -notlike "*$InstallDir*") {
    [Environment]::SetEnvironmentVariable("Path", "$path;$InstallDir", "Machine")
    Write-Host "✅ PATH 已更新（重开终端生效）" -ForegroundColor Green
}
Write-Host "✅ 已安装 $InstallDir\$Binary ($Version)" -ForegroundColor Green

# 可选：注册 Windows 服务（agent，使用 go-rmm-agent 自带 service 子命令）
if ($Service -and $Component -eq "agent") {
    $exe = Join-Path $InstallDir $Binary
    & $exe service install
    Write-Host "✅ 已注册 Windows 服务（go-rmm-agent service install）" -ForegroundColor Green
}

Remove-Item -Recurse -Force $Tmp
Write-Host ""
Write-Host "下一步:" -ForegroundColor Cyan
Write-Host "  $Binary -h"
