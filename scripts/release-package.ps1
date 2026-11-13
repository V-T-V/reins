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
#   reins-agent-linux-<ver>.tar.gz   Linux Agent 被控端包:reins-agent(amd64) + systemd unit
#                                    + AGENT-LINUX-SETUP.md(含能力分账)+ config.example.json
#   SHA256SUMS.txt                   全部产物的 SHA256 校验和(sha256sum -c 兼容格式,小写十六进制)
#
# 每个包自包含:客户拿到单个文件 + 一页说明即可从零部署,不需要回仓库找任何东西。
# 发布纪律:打包完成后用 scripts/release-tag.ps1 生成 release notes 并打 annotated tag
# (tag 由所有者手动 -Apply 触发,脚本默认 dry-run 不执行)。

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
Write-Host "[1/7] 版本: $Version  输出: $OutDir" -ForegroundColor Green

# --- 打包前测试(可 -SkipTest 跳过;禁止把测试红的构建发出去) ---
if (-not $SkipTest) {
    Write-Host "[2/7] 打包前全量测试 go test ./... (SkipTest 可跳过)" -ForegroundColor Green
    go test ./... -count=1
    if ($LASTEXITCODE -ne 0) { Write-Host "[ERROR] 测试未通过,拒绝打包" -ForegroundColor Red; exit 1 }
} else {
    Write-Host "[2/7] 跳过测试(-SkipTest)" -ForegroundColor Yellow
}

# --- 编译 ---
Write-Host "[3/7] 编译 Windows 组件(amd64, CGO=0)" -ForegroundColor Green
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
Write-Host "[4/7] 组装包结构" -ForegroundColor Green
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
Write-Host "[5/7] 压缩" -ForegroundColor Green
$ServerZip = Join-Path $OutDir "reins-server-windows-$Version.zip"
$AgentZip  = Join-Path $OutDir "reins-agent-windows-$Version.zip"
Compress-Archive -Path "$ServerStage\*" -DestinationPath $ServerZip -Force
Compress-Archive -Path "$AgentStage\*"  -DestinationPath $AgentZip  -Force

# --- Linux 包(交叉编译;tar 用 scripts/tarpack.go 打包) ---
# agent 包能力分账(screenshot/guiclick/powershell/power 仅 Windows)见
# docs/AGENT-LINUX-SETUP.md——执行器按平台注册,Linux 只声明真实可用的能力。
# 诚实边界:Windows 构建机只验证"编译成功+校验和",二进制运行验证靠 WSL 冒烟,
# 未做裸机 Linux 发行版矩阵的端到端验证(SETUP 文档已声明)。
# tar 打包不用宿主 tar:System32 bsdtar 不支持 --mode(所有条目 0644,二进制解压
# 后无执行位);Git Bash GNU tar 把 "D:\..." 解析为远程主机(host:file 语法)。
# tarpack.go 纯标准库,模式位定死(二进制/.sh 0755,其余 0644)。
if (-not $SkipLinux) {
    $env:GOOS = "linux"; $env:GOARCH = "amd64"
    go build -ldflags $LdFlags -o "$BuildDir\reins-relay-linux" .\relay
    if ($LASTEXITCODE -ne 0) { Write-Host "[ERROR] linux relay 编译失败" -ForegroundColor Red; exit 1 }
    go build -ldflags $LdFlags -o "$BuildDir\reins-agent-linux" .\agent
    if ($LASTEXITCODE -ne 0) { Write-Host "[ERROR] linux agent 编译失败" -ForegroundColor Red; exit 1 }
    # go run 用宿主工具链——GOOS 必须先还原,否则 tarpack 被编成 linux 二进制在 Windows 上跑不起来
    $env:GOOS = ""; $env:GOARCH = ""

    # Linux relay 包
    $LinuxStage = Join-Path $OutDir "stage\reins-relay-linux"
    New-Item -ItemType Directory -Force -Path $LinuxStage | Out-Null
    Copy-Item "$BuildDir\reins-relay-linux" "$LinuxStage\reins-relay"
    Copy-Item deploy\reins-relay.service "$LinuxStage\reins-relay.service"
    Copy-Item scripts\deploy-relay-linux.sh "$LinuxStage\deploy-relay-linux.sh"
    $LinuxTar = Join-Path $OutDir "reins-relay-linux-$Version.tar.gz"
    go run scripts\tarpack.go -out $LinuxTar -C $LinuxStage reins-relay reins-relay.service deploy-relay-linux.sh
    if ($LASTEXITCODE -ne 0) { Write-Host "[ERROR] linux relay tar 打包失败" -ForegroundColor Red; exit 1 }

    # Linux agent 包
    $LinuxAgentStage = Join-Path $OutDir "stage\reins-agent-linux"
    New-Item -ItemType Directory -Force -Path $LinuxAgentStage | Out-Null
    Copy-Item "$BuildDir\reins-agent-linux" "$LinuxAgentStage\reins-agent"
    Copy-Item deploy\reins-agent-linux.service "$LinuxAgentStage\reins-agent-linux.service"
    Copy-Item docs\AGENT-LINUX-SETUP.md "$LinuxAgentStage\AGENT-LINUX-SETUP.md"
    Copy-Item config.example.json "$LinuxAgentStage\config.example.json"
    $LinuxAgentTar = Join-Path $OutDir "reins-agent-linux-$Version.tar.gz"
    go run scripts\tarpack.go -out $LinuxAgentTar -C $LinuxAgentStage reins-agent reins-agent-linux.service AGENT-LINUX-SETUP.md config.example.json
    if ($LASTEXITCODE -ne 0) { Write-Host "[ERROR] linux agent tar 打包失败" -ForegroundColor Red; exit 1 }
} else {
    $env:GOOS = ""; $env:GOARCH = ""
}

# --- SHA256 校验和(所有产物;sha256sum -c 兼容:小写十六进制+两个空格+文件名) ---
Write-Host "[6/7] 生成 SHA256SUMS.txt" -ForegroundColor Green
$artifacts = Get-ChildItem $OutDir -File | Where-Object { $_.Extension -in ".zip", ".gz" } | Sort-Object Name
if (-not $artifacts) { Write-Host "[ERROR] 没有可校验的产物" -ForegroundColor Red; exit 1 }
$sumLines = foreach ($f in $artifacts) {
    $hash = (Get-FileHash -Algorithm SHA256 -LiteralPath $f.FullName).Hash.ToLower()
    "{0}  {1}" -f $hash, $f.Name
}
$sumsFile = Join-Path $OutDir "SHA256SUMS.txt"
# 纯 ASCII + LF 行尾:WriteAllLines 在 Windows 出 CRLF,GNU sha256sum -c 不剥 \r 会全体
# "FAILED open or read"(2026-11-13 实测逮住)——用 WriteAllText 显式 LF。
$sumText = ($sumLines -join "`n") + "`n"
[System.IO.File]::WriteAllText($sumsFile, $sumText, [System.Text.Encoding]::ASCII)
$sumLines | ForEach-Object { Write-Host "  $_" }
Write-Host "  -> $sumsFile"

# --- 汇总 ---
Write-Host "[7/7] 完成。产物清单:" -ForegroundColor Green
Get-ChildItem $OutDir -File | Where-Object { $_.Extension -in ".zip", ".gz", ".txt" } | ForEach-Object {
    Write-Host ("  {0}  {1:N1} MB" -f $_.Name, ($_.Length / 1MB))
}
Write-Host "清 单:$OutDir"
Write-Host "下一步:1) 抽产物解压验收 2) 校验和随产物分发(SHA256SUMS.txt,sha256sum -c / Get-FileHash)"
Write-Host "        3) 打 tag 出 release notes: powershell -File scripts\release-tag.ps1 -Version <tag>(默认 dry-run,所有者加 -Apply 才真打)"
