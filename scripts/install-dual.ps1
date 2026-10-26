# ╔══════════════════════════════════════════════════════════════╗
# ║  reins 主从双 Agent 部署脚本 (M1 架构)                       ║
# ║  装 master + slave 两个服务,主稳定管从,从可热更新             ║
# ╚══════════════════════════════════════════════════════════════╝
#
# 用法(管理员 PowerShell):
#   .\install-dual.ps1 -Server 'https://203.0.113.99/rmm' -Relay 'wss://203.0.113.99/rmm/agent' -Token 'xxx'
#
# 安装内容:
#   C:\Program Files\reins-master\  主(稳定,永不更新)
#     - reins-master.exe
#     - config.json (role=master, slave_exe_dir/slave_agent_id)
#     - data\slave-state.json (看门狗状态)
#   C:\Program Files\reins-slave\   从(可热更新)
#     - reins-slave.exe
#     - config.json (role=slave)
#
# 两个 Windows 服务:
#   reins-master  (Automatic + OnFailure=restart + 看门狗监控从)
#   reins-slave   (Automatic + OnFailure=restart)
#
# 主救援从的机制:
#   1. 本地看门狗:主每30s检查从服务,挂了sc.exe start拉起 + .bak回滚
#   2. Relay通道:主经POST /api/updates/apply给从推版本(复用现有机制)
#   3. CLI:reins rescue restart/rollback/update -a <agent-id>

param(
    [Parameter(Mandatory=$true)]
    [string]$Server,
    [Parameter(Mandatory=$true)]
    [string]$Relay,
    [Parameter(Mandatory=$true)]
    [string]$Token,
    [string]$MasterDir = "C:\Program Files\reins-master",
    [string]$SlaveDir  = "C:\Program Files\reins-slave",
    [string]$MasterWorkspace = "C:\reins\master-workspace",
    [string]$SlaveWorkspace  = "C:\reins\slave-workspace",
    [string]$Version = "v4.2.0"
)

$ErrorActionPreference = "Stop"

# === 检查管理员 ===
$isAdmin = ([Security.Principal.WindowsPrincipal] [Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
if (-not $isAdmin) {
    Write-Host "[错误] 需要管理员权限(装两个 Windows 服务)" -ForegroundColor Red
    exit 1
}

Write-Host ""
Write-Host "═══════════════════════════════════════════════════════════════"
Write-Host " reins 主从双 Agent 部署 $Version" -ForegroundColor Cyan
Write-Host "═══════════════════════════════════════════════════════════════"
Write-Host "  Server:    $Server"
Write-Host "  Relay:     $Relay"
Write-Host "  主目录:    $MasterDir (稳定,永不更新)"
Write-Host "  从目录:    $SlaveDir  (可热更新)"
Write-Host "═══════════════════════════════════════════════════════════════"
Write-Host ""

# === 1. 下载二进制 ===
Write-Host "[1/6] 下载 Agent 二进制..." -ForegroundColor Cyan
$dlUrl = "$Server/dl/reins-agent.exe"
$ProgressPreference = 'SilentlyContinue'

# 主二进制
$masterExe = Join-Path $MasterDir "reins-master.exe"
New-Item -ItemType Directory -Force -Path $MasterDir | Out-Null
Invoke-WebRequest -Uri $dlUrl -OutFile $masterExe -UseBasicParsing -TimeoutSec 120

# 从二进制(同一份二进制,不同名字+不同目录+不同 config)
$slaveExe = Join-Path $SlaveDir "reins-slave.exe"
New-Item -ItemType Directory -Force -Path $SlaveDir | Out-Null
Invoke-WebRequest -Uri $dlUrl -OutFile $slaveExe -UseBasicParsing -TimeoutSec 120

Write-Host "[OK]   下载完成: 主 $([math]::Round((Get-Item $masterExe).Length/1MB,1))MB, 从 $([math]::Round((Get-Item $slaveExe).Length/1MB,1))MB" -ForegroundColor Green

# === 2. 先装从(主需要知道从的 agent_id) ===
Write-Host ""
Write-Host "[2/6] 配置并启动从 Agent..." -ForegroundColor Cyan

# 生成从 config(先不设 agent_id,让从自动生成)
$slaveCfg = [ordered]@{
    relay_url = $Relay
    token = $Token
    role = "slave"
    workspace = $SlaveWorkspace
    allow_all_roots = $false
}
$slaveCfgFile = Join-Path $SlaveDir "config.json"
$json = $slaveCfg | ConvertTo-Json -Depth 3
[System.IO.File]::WriteAllText($slaveCfgFile, $json, (New-Object System.Text.UTF8Encoding $false))

# 停旧的 + 装新的
if (Get-Service -Name "reins-slave" -ErrorAction SilentlyContinue) {
    Stop-Service "reins-slave" -Force -ErrorAction SilentlyContinue
    & $slaveExe uninstall 2>&1 | Out-Null
}
New-Item -ItemType Directory -Force -Path $SlaveWorkspace | Out-Null
& $slaveExe install -relay $Relay -token $Token -role slave
Start-Sleep 5

# 读从的 agent_id(从自动生成并持久化到 config.json)
$slaveCfgData = Get-Content $slaveCfgFile -Raw | ConvertFrom-Json
$slaveAgentID = $slaveCfgData.agent_id
Write-Host "[OK]   从 Agent ID: $slaveAgentID" -ForegroundColor Green

# === 3. 装主 ===
Write-Host ""
Write-Host "[3/6] 配置并启动主 Agent..." -ForegroundColor Cyan

$masterCfg = [ordered]@{
    relay_url = $Relay
    token = $Token
    role = "master"
    workspace = $MasterWorkspace
    allow_all_roots = $false
    slave_exe_dir = $SlaveDir
    slave_agent_id = $slaveAgentID
}
$masterCfgFile = Join-Path $MasterDir "config.json"
$json = $masterCfg | ConvertTo-Json -Depth 3
[System.IO.File]::WriteAllText($masterCfgFile, $json, (New-Object System.Text.UTF8Encoding $false))

if (Get-Service -Name "reins-master" -ErrorAction SilentlyContinue) {
    Stop-Service "reins-master" -Force -ErrorAction SilentlyContinue
    & $masterExe uninstall 2>&1 | Out-Null
}
New-Item -ItemType Directory -Force -Path $MasterWorkspace | Out-Null
& $masterExe install -relay $Relay -token $Token -role master
Start-Sleep 5

$masterCfgData = Get-Content $masterCfgFile -Raw | ConvertFrom-Json
$masterAgentID = $masterCfgData.agent_id
Write-Host "[OK]   主 Agent ID: $masterAgentID" -ForegroundColor Green

# === 4. 验证两个服务 ===
Write-Host ""
Write-Host "[4/6] 验证服务状态..." -ForegroundColor Cyan
$masterSvc = Get-Service "reins-master" -ErrorAction SilentlyContinue
$slaveSvc = Get-Service "reins-slave" -ErrorAction SilentlyContinue
if ($masterSvc.Status -eq "Running") {
    Write-Host "[OK]   reins-master: Running" -ForegroundColor Green
} else {
    Write-Host "[WARN] reins-master: $($masterSvc.Status)" -ForegroundColor Yellow
}
if ($slaveSvc.Status -eq "Running") {
    Write-Host "[OK]   reins-slave:  Running" -ForegroundColor Green
} else {
    Write-Host "[WARN] reins-slave:  $($slaveSvc.Status)" -ForegroundColor Yellow
}

# === 5. 等待上线 ===
Write-Host ""
Write-Host "[5/6] 等待 Agent 连接 Relay(15秒)..." -ForegroundColor Cyan
Start-Sleep 15

# === 6. 汇总 ===
Write-Host ""
Write-Host "═══════════════════════════════════════════════════════════════"
Write-Host " ✅  主从双 Agent 部署成功!" -ForegroundColor Green
Write-Host "═══════════════════════════════════════════════════════════════"
Write-Host "  主 Agent: $masterAgentID"
Write-Host "    服务:   reins-master (永不更新,管从)"
Write-Host "    目录:   $MasterDir"
Write-Host "  从 Agent: $slaveAgentID"
Write-Host "    服务:   reins-slave  (可热更新,承担业务)"
Write-Host "    目录:   $SlaveDir"
Write-Host ""
Write-Host " 看门狗:主每30s检查从,挂了自动sc start + .bak回滚" -ForegroundColor Yellow
Write-Host ""
Write-Host " 在控制端验证:" -ForegroundColor Yellow
Write-Host "   reins --relay $Server --token '***' agent list"
Write-Host "   (应看到两个 Agent,Tags 含 role=master 和 role=slave)"
Write-Host ""
Write-Host " 救援命令:" -ForegroundColor Yellow
Write-Host "   reins rescue status   -a $masterAgentID   # 查从的状态"
Write-Host "   reins rescue restart  -a $masterAgentID   # 重启从"
Write-Host "   reins rescue rollback -a $masterAgentID   # .bak 回滚从"
Write-Host "   reins rescue update   -a $slaveAgentID -v v4.2.0  # 推版本救援"
Write-Host "═══════════════════════════════════════════════════════════════"
