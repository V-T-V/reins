# go-rmm Agent Windows 服务安装脚本
#
# 用法（管理员 PowerShell）：
#   .\scripts\install-agent.ps1 -RelayUrl "ws://relay-host:7780/agent" -Token "your-token"
#   .\scripts\install-agent.ps1 -RelayUrl "ws://relay-host:7780/agent" -GlmApiKey "your-glm-key"
#
# 脚本会：
#   1. 生成 config.json（含 AgentID/RelayURL/Token/GLM配置）
#   2. 调用 go-rmm-agent.exe install 注册为 Windows 服务
#   3. 启动服务

param(
    [Parameter(Mandatory=$true)]
    [string]$RelayUrl,
    [string]$Token = "",
    [string]$GlmApiKey = "",
    [string]$GlmModel = "glm-5-turbo",
    [string]$GlmBaseUrl = "https://open.bigmodel.cn/api/paas/v4",
    [string]$AgentId = ""  # 留空自动生成
)

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

# 检查管理员权限
$isAdmin = ([Security.Principal.WindowsPrincipal] [Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
if (-not $isAdmin) {
    Write-Warning "建议以管理员身份运行，否则服务安装可能失败"
}

# 确认 agent 二进制存在
$agentExe = "$root\bin\go-rmm-agent.exe"
if (-not (Test-Path $agentExe)) {
    Write-Host "==> 未找到 agent 二进制，开始编译..." -ForegroundColor Cyan
    go build -o $agentExe ./agent/
}

# 生成配置
if (-not $AgentId) {
    $hostname = $env:COMPUTERNAME
    $short = (New-Guid).ToString().Substring(0,8)
    $AgentId = "agent-$hostname-$short"
}

$config = @{
    agent_id  = $AgentId
    relay_url = $RelayUrl
    token     = $Token
    tags      = @{ installed_by = $env:USERNAME; install_time = (Get-Date -Format "o") }
}
if ($GlmApiKey) {
    $config.glm = @{
        api_key  = $GlmApiKey
        model    = $GlmModel
        base_url = $GlmBaseUrl
    }
}

$configPath = "$root\bin\config.json"
$config | ConvertTo-Json -Depth 5 | Out-File -FilePath $configPath -Encoding utf8
Write-Host "==> 配置已写入 $configPath" -ForegroundColor Green
Write-Host "    AgentID: $AgentId"
Write-Host "    Relay:   $RelayUrl"
if ($GlmApiKey) { Write-Host "    GLM:     已配置 (guiclick 可用)" }

# 安装服务
Write-Host "==> 安装 Windows 服务..." -ForegroundColor Cyan
& $agentExe install
if ($LASTEXITCODE -ne 0) {
    Write-Error "服务安装失败"
    exit 1
}

Write-Host ""
Write-Host "安装完成！" -ForegroundColor Green
Write-Host "  启动:   $agentExe start"
Write-Host "  停止:   $agentExe stop"
Write-Host "  状态:   $agentExe status"
Write-Host "  卸载:   $agentExe uninstall"
Write-Host "  日志:   查看服务控制管理器 / 事件查看器"
