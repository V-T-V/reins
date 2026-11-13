# reins 版本化打包脚本 —— 产出客户交付 zip(tar.gz)
#
# 用法(在仓库根目录,需 Go 工具链):
#   powershell -File scripts\release-package.ps1                    # 版本取 git describe(退回 dev-<日期>)
#   powershell -File scripts\release-package.ps1 -Version v4.9.3    # 显式版本
#   powershell -File scripts\release-package.ps1 -SkipLinux         # 只打 Windows 包(默认也打)
#
# 产物(dist/release/<version>/):
#   reins-server-windows-<ver>.zip   Relay 服务器包:reins-relay.exe + reins.exe(CLI) + reins-gencert.exe
#                                    + start-relay.ps1(防火墙/服务注册样例) + INSTALL.md + config 说明
#   reins-agent-windows-<ver>.zip    Agent 被控端包:reins-agent.exe + service-install.ps1(装/卸/升级)
#                                    + config.example.json + AGENT-SETUP.md
#   reins-relay-linux-<ver>.tar.gz   Linux Relay 包:reins-relay(amd64) + systemd unit + 部署说明
#
# 每个包自包含:客户拿到单个文件 + 一页说明即可从零部署,不需要回仓库找任何东西。

param(
    [string]$Version = "",
    [switch]$SkipLinux,
    [switch]$SkipTest       # 跳过打包前 go test ./...(默认跑,防把红的构建打进包里)
)

$ErrorActionPreference = "Stop"
$RepoRoot = Split-Path -Parent $PSScriptRoot
Set-Location $RepoRoot

# --- 版本号:参数 > git describe > dev-日期 ---
if (-not $Version) {
    $Version = git describe --tags --always --dirty 2>$null
    if (-not $Version -or $LASTEXITCODE -ne 0) { $Version = "" }
}
if (-not $Version) { $Version = "dev-$(Get-Date -Format yyyyMMdd-HHmm)" }
$OutDir = Join-Path $RepoRoot "dist\release\$Version"
Write-Host "[1/6] 版本: $Version  输出: $OutDir" -ForegroundColor Green

# --- 打包前测试(可 -SkipTest 跳过;禁止把测试红的构建发出去) ---
if (-not $SkipTest) {
    Write-Host "[2/6] 打包前全量测试 go test ./... (SkipTest 可跳过)" -ForegroundColor Green
    go test ./... -count=1
    if ($LASTEXITCODE -ne 0) { Write-Host "[ERROR] 测试未通过,拒绝打包" -ForegroundColor Red; exit 1 }
} else {
    Write-Host "[2/6] 跳过测试(-SkipTest)" -ForegroundColor Yellow
}

# --- 编译 ---
Write-Host "[3/6] 编译 Windows 组件(amd64, CGO=0)" -ForegroundColor Green
$env:GOOS = "windows"; $env:GOARCH = "amd64"; $env:CGO_ENABLED = "0"
$LdFlags = "-s -w -X main.version=$Version"
$BuildDir = Join-Path $OutDir "build"
New-Item -ItemType Directory -Force -Path $BuildDir | Out-Null
$builds = @(
    @{ Out = "$BuildDir\reins-relay.exe";   Pkg = ".\relay" },
    @{ Out = "$BuildDir\reins.exe";         Pkg = ".\cmd\reins" },
    @{ Out = "$BuildDir\reins-agent.exe";   Pkg = ".\agent" },
    @{ Out = "$BuildDir\reins-gencert.exe"; Pkg = ".\cmd\gencert" }
)
foreach ($b in $builds) {
    go build -ldflags $LdFlags -o $b.Out $b.Pkg
    if ($LASTEXITCODE -ne 0) { Write-Host "[ERROR] 编译失败: $($b.Pkg)" -ForegroundColor Red; exit 1 }
}

# --- 组装 server 包 ---
Write-Host "[4/6] 组装包结构" -ForegroundColor Green
$ServerStage = Join-Path $OutDir "stage\reins-server"
New-Item -ItemType Directory -Force -Path $ServerStage | Out-Null
Copy-Item "$BuildDir\reins-relay.exe"  $ServerStage
Copy-Item "$BuildDir\reins.exe"        $ServerStage
Copy-Item "$BuildDir\reins-gencert.exe" $ServerStage
Copy-Item docs\INSTALL.md $ServerStage\INSTALL.md
Copy-Item scripts\start-relay.ps1 $ServerStage\start-relay.ps1

# --- 组装 agent 包 ---
$AgentStage = Join-Path $OutDir "stage\reins-agent"
New-Item -ItemType Directory -Force -Path $AgentStage | Out-Null
Copy-Item "$BuildDir\reins-agent.exe" $AgentStage
Copy-Item scripts\service-install.ps1 $AgentStage\service-install.ps1
Copy-Item config.example.json $AgentStage\config.example.json
Copy-Item docs\AGENT-SETUP.md $AgentStage\AGENT-SETUP.md

# --- 压 zip ---
Write-Host "[5/6] 压缩" -ForegroundColor Green
$ServerZip = Join-Path $OutDir "reins-server-windows-$Version.zip"
$AgentZip  = Join-Path $OutDir "reins-agent-windows-$Version.zip"
Compress-Archive -Path "$ServerStage\*" -DestinationPath $ServerZip -Force
Compress-Archive -Path "$AgentStage\*"  -DestinationPath $AgentZip  -Force

# --- Linux relay 包(交叉编译;Windows 10+ 自带 bsdtar) ---
if (-not $SkipLinux) {
    $env:GOOS = "linux"; $env:GOARCH = "amd64"
    go build -ldflags $LdFlags -o "$BuildDir\reins-relay-linux" .\relay
    if ($LASTEXITCODE -ne 0) { Write-Host "[ERROR] linux relay 编译失败" -ForegroundColor Red; exit 1 }
    $LinuxStage = Join-Path $OutDir "stage\reins-relay-linux"
    New-Item -ItemType Directory -Force -Path $LinuxStage | Out-Null
    Copy-Item "$BuildDir\reins-relay-linux" "$LinuxStage\reins-relay"
    Copy-Item deploy\reins-relay.service "$LinuxStage\reins-relay.service"
    Copy-Item scripts\deploy-relay-linux.sh "$LinuxStage\deploy-relay-linux.sh"
    $LinuxTar = Join-Path $OutDir "reins-relay-linux-$Version.tar.gz"
    # Windows 下必须用系统自带 bsdtar:Git Bash 的 GNU tar 把 "D:\..." 的冒号解析为远程主机(host:file 语法)
    $tarExe = Join-Path $env:SystemRoot "System32\tar.exe"
    if (Test-Path $tarExe) {
        & $tarExe -czf $LinuxTar -C $LinuxStage reins-relay reins-relay.service deploy-relay-linux.sh
    } else {
        tar -czf $LinuxTar -C $LinuxStage reins-relay reins-relay.service deploy-relay-linux.sh
    }
    if ($LASTEXITCODE -ne 0) { Write-Host "[WARN] tar 打包失败(可忽略,zip 包已完成);或用 -SkipLinux" -ForegroundColor Yellow }
}

$env:GOOS = ""; $env:GOARCH = ""

# --- 汇总 ---
Write-Host "[6/6] 完成。产物清单:" -ForegroundColor Green
Get-ChildItem $OutDir -File | Where-Object { $_.Extension -in ".zip", ".gz" } | ForEach-Object {
    Write-Host ("  {0}  {1:N1} MB" -f $_.Name, ($_.Length / 1MB))
}
Write-Host "清 单:$OutDir"
Write-Host "下一步:校验 zip 内容后分发。安装说明已随包(INSTALL.md / AGENT-SETUP.md)。"
