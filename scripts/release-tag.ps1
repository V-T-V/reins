# reins 发布 tag 纪律脚本 —— CHANGELOG 未发布节 → release notes → annotated tag
#
# 纪律(为什么有这个脚本):
#   - 版本单一真相源 = git tag(CHANGELOG 开头语),但 tag 至今靠手敲——欠账③。
#   - release notes 从 CHANGELOG [未发布] 节生成,不另写一份(两处维护必漂移);
#     产物 SHA256 校验和随 notes 一起入 tag message,发布与校验凭据同源。
#
# 用法(仓库根目录):
#   powershell -File scripts\release-tag.ps1 -Version v4.9.4           # dry-run:只生成并打印 notes,不动 git
#   powershell -File scripts\release-tag.ps1 -Version v4.9.4 -Apply    # 所有者确认后:真正打 annotated tag
#
# 铁律:
#   - 默认 dry-run,绝不执行任何 git 写操作;tag 只能由所有者加 -Apply 手动触发。
#   - 即使 -Apply 也只打本地 tag,绝不 push(push 永远是所有者的手动动作)。
#   - 不修改 CHANGELOG([未发布]→[版本] 的头部改名由所有者提交时顺手做,脚本只提醒)。
#
# 前提:先跑 scripts\release-package.ps1 产出 dist/release/<version>/(含 SHA256SUMS.txt)。

param(
    [Parameter(Mandatory = $true)]
    [string]$Version,          # tag 名,如 v4.9.4(须 v 开头 semver;也用作产物目录名)
    [string]$ArtifactsDir = "",# 默认 dist/release/<Version>;没有产物也能出 notes(无校验和段,给黄牌)
    [switch]$Apply             # 加上才真正 git tag -a;默认 dry-run
)

$ErrorActionPreference = "Stop"
$RepoRoot = Split-Path -Parent $PSScriptRoot
Set-Location $RepoRoot

# --- 版本号形状校验(v + semver,允许 -rc.1 / -ui 之类后缀) ---
if ($Version -notmatch '^v\d+\.\d+\.\d+([-.][0-9A-Za-z.]+)?$') {
    Write-Host "[ERROR] 版本号形状不对: '$Version'(要 v 开头 semver,如 v4.9.4 / v4.9.4-rc.1)" -ForegroundColor Red
    exit 1
}

# --- tag 冲突检查 ---
$existing = git tag -l $Version
if ($existing) {
    Write-Host "[ERROR] tag $Version 已存在(指向 $(git rev-parse -q --short $Version))。发新版本请换版本号。" -ForegroundColor Red
    exit 1
}

# --- 工作区状态(诚实提醒,不阻断) ---
$dirty = git status --porcelain
if ($dirty) {
    Write-Host "[WARN] 工作区有未提交改动——tag 只会包含已提交的内容:" -ForegroundColor Yellow
    $dirty | Select-Object -First 10 | ForEach-Object { Write-Host "    $_" -ForegroundColor Yellow }
    Write-Host "      (确认这些改动是否应先提交再打 tag)" -ForegroundColor Yellow
}

# --- 解析 CHANGELOG [未发布] 节 ---
$changelog = Join-Path $RepoRoot "CHANGELOG.md"
$lines = [System.IO.File]::ReadAllLines($changelog)
$inSection = $false
$body = @()
foreach ($ln in $lines) {
    if ($ln -match '^## \[未发布\]') { $inSection = $true; continue }
    if ($inSection -and $ln -match '^## \[') { break }
    if ($inSection) { $body += $ln }
}
if (-not $body -or -not ($body | Where-Object { $_.Trim() -ne "" })) {
    Write-Host "[ERROR] CHANGELOG.md 里没找到有内容的 '## [未发布]' 节——先补 CHANGELOG 再谈发布。" -ForegroundColor Red
    exit 1
}
# 掐掉节首尾空行
while ($body.Count -gt 0 -and $body[0].Trim() -eq "") { $body = $body[1..($body.Count - 1)] }
while ($body.Count -gt 0 -and $body[-1].Trim() -eq "") { $body = $body[0..($body.Count - 2)] }

# --- 产物目录与校验和 ---
if (-not $ArtifactsDir) { $ArtifactsDir = Join-Path $RepoRoot "dist\release\$Version" }
$sumsPath = Join-Path $ArtifactsDir "SHA256SUMS.txt"
$sumsBlock = ""
if (Test-Path $sumsPath) {
    $sumsBlock = ([System.IO.File]::ReadAllText($sumsPath)).TrimEnd()
} else {
    Write-Host "[WARN] $sumsPath 不存在——release notes 将不带校验和。先跑 release-package.ps1,或用 -ArtifactsDir 指对目录。" -ForegroundColor Yellow
}

# --- 组装 release notes ---
$commit = git rev-parse --short HEAD
$date = Get-Date -Format yyyy-MM-dd
$notes = @()
$notes += "reins $Version($date)"
$notes += ""
$notes += $body
if ($sumsBlock) {
    $notes += ""
    $notes += "## 产物校验和(SHA256)"
    $notes += ""
    $notes += "与产物同目录的 SHA256SUMS.txt 逐字节一致;校验命令:`sha256sum -c SHA256SUMS.txt`(Windows: Get-FileHash,见 INSTALL.md 第零步)。"
    $notes += ""
    $notes += '```'
    $notes += $sumsBlock
    $notes += '```'
}
$notes += ""
$notes += "----"
$notes += "commit: $commit  |  tag: $Version(annotated)"

$notesFile = Join-Path $ArtifactsDir "RELEASE-NOTES-$Version.md"
New-Item -ItemType Directory -Force -Path $ArtifactsDir | Out-Null
# UTF-8 带 BOM:git show tag 时 Windows 侧工具(记事本/类型重定向)不乱码
[System.IO.File]::WriteAllLines($notesFile, [string[]]$notes, [System.Text.UTF8Encoding]::new($true))

# --- 输出 ---
Write-Host ""
Write-Host "================ release notes($notesFile)================" -ForegroundColor Green
$notes | ForEach-Object { Write-Host $_ }
Write-Host "===========================================================" -ForegroundColor Green
Write-Host ""
if ($Apply) {
    git tag -a $Version -F $notesFile
    if ($LASTEXITCODE -ne 0) { Write-Host "[ERROR] git tag 失败" -ForegroundColor Red; exit 1 }
    Write-Host "[OK] 已打本地 annotated tag: $Version -> $(git rev-parse -q --short $Version)" -ForegroundColor Green
    Write-Host "     未 push。push 由所有者手动执行: git push origin $Version"
} else {
    Write-Host "[DRY-RUN] 未执行任何 git 写操作。打 tag 请所有者手动确认后执行:" -ForegroundColor Cyan
    Write-Host "    powershell -File scripts\release-tag.ps1 -Version $Version -Apply" -ForegroundColor Cyan
}
Write-Host ""
Write-Host "发布收尾清单(所有者手动):"
Write-Host "  1. CHANGELOG.md 头部 '## [未发布] — ...' 改名为 '## [$Version] — $date' 并提交(版本真相源=tag,CHANGELOG 只是账本)"
Write-Host "  2. (可选)推送: git push origin $Version"
Write-Host "  3. 分发产物 + SHA256SUMS.txt + RELEASE-NOTES-$Version.md 三件套"
