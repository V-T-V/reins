# reins Windows 安装器（从 GitHub Release 下载二进制）
#
# 用法（管理员 PowerShell）:
#   .\scripts\install.ps1                          # 最新版 Relay
#   .\scripts\install.ps1 -Component agent         # relay|agent|cli
param(
    [string]$Owner = "V-T-V",
    [string]$Repo = "reins",
    [string]$Version = "latest",
    [ValidateSet("relay", "agent", "cli")][string]$Component = "relay",
    [string]$InstallDir = "$env:ProgramFiles\reins"
)
$ErrorActionPreference = "Stop"

if ($Version -eq "latest") {
    $rel = Invoke-RestMethod "https://api.github.com/repos/$Owner/$Repo/releases/latest"
    $Version = $rel.tag_name
}

$Binary = "reins-$Component.exe"
$Archive = "reins-${Component}_${Version}_windows_amd64.zip"
$Url = "https://github.com/$Owner/$Repo/releases/download/$Version/$Archive"
$Tmp = Join-Path $env:TEMP "reins-install-$(Get-Random)"

Write-Host "下载 $Url"
New-Item -ItemType Directory -Force -Path $Tmp | Out-Null
$ZipPath = Join-Path $Tmp $Archive
Invoke-WebRequest -Uri $Url -OutFile $ZipPath -UseBasicParsing
Expand-Archive -Path $ZipPath -DestinationPath $Tmp -Force

New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
Move-Item -Force (Join-Path $Tmp $Binary) (Join-Path $InstallDir $Binary)
Write-Host "已安装 $InstallDir\$Binary ($Version)"
Remove-Item -Recurse -Force $Tmp
