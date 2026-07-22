# go-rmm 本地三组件联调脚本（PowerShell）
#
# 用法：
#   .\scripts\demo-local.ps1              # 编译并启动三组件
#   .\scripts\demo-local.ps1 -NoBuild     # 跳过编译，直接启动
#
# 启动后浏览器打开 http://localhost:7781
# 按 Ctrl+C 停止全部。

param([switch]$NoBuild)

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

if (-not $NoBuild) {
    Write-Host "==> 编译三组件..." -ForegroundColor Cyan
    go build -o bin/go-rmm-relay.exe ./relay/
    go build -o bin/go-rmm-agent.exe ./agent/
    go build -o bin/go-rmm-console.exe ./console/
    Write-Host "编译完成" -ForegroundColor Green
}

$procs = @()

# Relay
Write-Host "==> 启动 Relay (:17780)" -ForegroundColor Cyan
$procs += Start-Process -PassThru -FilePath "$root\bin\go-rmm-relay.exe" -ArgumentList "-addr,:17780,-dev" -NoNewWindow

Start-Sleep -Milliseconds 500

# Agent
Write-Host "==> 启动 Agent (连 Relay)" -ForegroundColor Cyan
$procs += Start-Process -PassThru -FilePath "$root\bin\go-rmm-agent.exe" -ArgumentList "run,-relay,ws://127.0.0.1:17780/agent,-id,agent-local" -NoNewWindow

Start-Sleep -Milliseconds 500

# Console
Write-Host "==> 启动 Console (:7781)" -ForegroundColor Cyan
$procs += Start-Process -PassThru -FilePath "$root\bin\go-rmm-console.exe" -ArgumentList "-addr,:7781,-relay,http://127.0.0.1:17780" -NoNewWindow

Write-Host ""
Write-Host "三组件已启动。浏览器打开: http://localhost:7781" -ForegroundColor Green
Write-Host "按 Ctrl+C 停止全部..." -ForegroundColor Yellow

try {
    Wait-Event -Timeout ([int]::MaxValue)
} finally {
    Write-Host "`n==> 停止所有组件..." -ForegroundColor Cyan
    $procs | ForEach-Object {
        try { $_.Kill() } catch {}
    }
}
