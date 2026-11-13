# reins Agent Windows 服务管理器(随 agent zip 分发)
#
# 包装 reins-agent.exe 自带的 kardianos 服务管理(install/uninstall/start/stop/status),
# 补齐客户交付三件事:安装目录规范(C:\Program Files\reins)、升级(备份→替换→启动→验证→回滚)、
# 卸载残留清理。
#
# 用法(管理员 PowerShell,在解压后的 reins-agent 目录内):
#   .\service-install.ps1 install -Relay 192.168.1.10:7780 -Token <Token>            # 装服务+启动
#   .\service-install.ps1 install -Relay 192.168.1.10 -Token <Token> -Tags env=prod  # 打标签(批量分发用)
#   .\service-install.ps1 status                                                      # 服务状态
#   .\service-install.ps1 stop / start / restart                                      # 常规控制
#   .\service-install.ps1 upgrade -NewExe C:\Downloads\reins-agent.exe                # 升级(自动备份/回滚)
#   .\service-install.ps1 rollback                                                    # 回滚到上一版
#   .\service-install.ps1 uninstall                                                   # 卸载+清残留
#
# 服务特性(由 reins-agent 内建):开机自启(Automatic)、崩溃自动重启(OnFailure=restart)。
param(
    [Parameter(Position = 0, Mandatory = $true)]
    [ValidateSet("install", "uninstall", "start", "stop", "restart", "status", "upgrade", "rollback")]
    [string]$Action,

    [string]$Relay = "",
    [string]$Token = "",
    [string]$AgentId = "",
    [string[]]$Tags = @(),
    [string]$InstallDir = "$env:ProgramFiles\reins",
    [string]$NewExe = "",           # upgrade: 新版 exe 路径
    [switch]$KeepWorkspace          # uninstall: 保留 workspace 数据
)

$ErrorActionPreference = "Stop"

function Require-Admin {
    $isAdmin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
    if (-not $isAdmin) { Write-Host "[ERROR] 需要管理员权限(右键 PowerShell → 以管理员身份运行)" -ForegroundColor Red; exit 1 }
}

# zip 内自带的 exe(脚本同目录)
$SrcExe = Join-Path $PSScriptRoot "reins-agent.exe"
$TargetExe = Join-Path $InstallDir "reins-agent.exe"

function Copy-AgentBinary {
    if (-not (Test-Path $SrcExe)) { Write-Host "[ERROR] 脚本同目录缺少 reins-agent.exe" -ForegroundColor Red; exit 1 }
    New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
    Copy-Item $SrcExe $TargetExe -Force
    Write-Host "[OK] 二进制就位: $TargetExe"
}

switch ($Action) {
    "install" {
        Require-Admin
        if (-not $Relay) { Write-Host "[ERROR] install 需要 -Relay <地址>(IP[:端口] 或 ws://host:7780/agent)" -ForegroundColor Red; exit 1 }
        Copy-AgentBinary
        # 装服务(kardianos:Automatic 启动 + OnFailure=restart 崩溃自愈)
        $installArgs = @("install", "-relay", $Relay)
        if ($Token)   { $installArgs += @("-token", $Token) }
        if ($AgentId) { $installArgs += @("-id", $AgentId) }
        & $TargetExe @installArgs
        if ($LASTEXITCODE -ne 0) { Write-Host "[ERROR] 服务注册失败" -ForegroundColor Red; exit 1 }
        # 标签写进 config.json(reins-agent install 已持久化配置,这里原地补 tags)
        if ($Tags.Count -gt 0) {
            $cfgPath = Join-Path $InstallDir "config.json"
            if (Test-Path $cfgPath) {
                $cfg = Get-Content $cfgPath -Raw | ConvertFrom-Json
                if (-not $cfg.tags) { $cfg | Add-Member -NotePropertyName tags -NotePropertyValue ([pscustomobject]@{}) }
                foreach ($t in $Tags) {
                    $kv = $t -split "=", 2
                    if ($kv.Count -eq 2) { $cfg.tags | Add-Member -NotePropertyName $kv[0] -NotePropertyValue $kv[1] -Force }
                }
                $cfg | ConvertTo-Json -Depth 10 | Set-Content $cfgPath -Encoding UTF8
                Write-Host "[OK] 标签已写入 config.json: $($Tags -join ', ')"
            }
        }
        & $TargetExe start
        Write-Host "[OK] 服务已安装并启动(开机自启 + 崩溃自动重启)"
        Write-Host "     验证: reins-relay 控制台应出现本机 Agent;或 sc query reins-agent"
    }

    "uninstall" {
        Require-Admin
        if (Test-Path $TargetExe) {
            & $TargetExe stop 2>$null
            & $TargetExe uninstall
        } else { Write-Host "[INFO] 未发现已安装的 reins-agent" }
        # 清残留:卸载只移除服务注册,二进制/配置/数据留待确认
        if (Test-Path $InstallDir) {
            $dataPath = Join-Path $InstallDir "data"
            if (-not $KeepWorkspace) {
                Remove-Item $InstallDir -Recurse -Force -ErrorAction SilentlyContinue
                Write-Host "[OK] 已清理 $InstallDir (含配置与数据;保留数据请加 -KeepWorkspace)"
            } else {
                Write-Host "[OK] 已保留 $InstallDir(-KeepWorkspace)"
            }
        }
    }

    "start"   { Require-Admin; & $TargetExe start;   Write-Host "[OK] 已启动" }
    "stop"    { Require-Admin; & $TargetExe stop;    Write-Host "[OK] 已停止" }
    "restart" { Require-Admin; & $TargetExe stop;    Start-Sleep -Seconds 2; & $TargetExe start; Write-Host "[OK] 已重启" }
    "status"  { & $TargetExe status }

    "upgrade" {
        Require-Admin
        if (-not $NewExe -or -not (Test-Path $NewExe)) { Write-Host "[ERROR] upgrade 需要 -NewExe <新版exe路径>" -ForegroundColor Red; exit 1 }
        if (-not (Test-Path $TargetExe)) { Write-Host "[ERROR] 尚未安装,请先 install" -ForegroundColor Red; exit 1 }
        # 停服务 → 备份 → 替换 → 启动 → 探活 → 失败自动回滚
        Write-Host "[1/5] 停止服务"
        & $TargetExe stop 2>$null
        Start-Sleep -Seconds 2
        $backup = "$TargetExe.bak"
        Write-Host "[2/5] 备份当前版本 → $backup"
        Copy-Item $TargetExe $backup -Force
        Write-Host "[3/5] 替换二进制"
        Copy-Item $NewExe $TargetExe -Force
        Write-Host "[4/5] 启动并验证(等待 8s 后查服务状态)"
        & $TargetExe start
        Start-Sleep -Seconds 8
        $svc = Get-Service -Name "reins-agent" -ErrorAction SilentlyContinue
        if ($svc -and $svc.Status -eq "Running") {
            Write-Host "[5/5] ✓ 升级成功: $((Get-Item $TargetExe).VersionInfo.FileVersion) 服务运行中(回滚可用 rollback)"
        } else {
            Write-Host "[5/5] ✗ 升级后服务未运行,自动回滚" -ForegroundColor Red
            Copy-Item $backup $TargetExe -Force
            & $TargetExe start
            Write-Host "      已回滚到旧版本。请检查新版兼容性(事件查看器→Application→reins-agent 日志)"
            exit 1
        }
    }

    "rollback" {
        Require-Admin
        $backup = "$TargetExe.bak"
        if (-not (Test-Path $backup)) { Write-Host "[ERROR] 没有备份(.bak)——rollback 仅在 upgrade 之后可用" -ForegroundColor Red; exit 1 }
        & $TargetExe stop 2>$null
        Start-Sleep -Seconds 2
        Copy-Item $backup $TargetExe -Force
        & $TargetExe start
        Write-Host "[OK] 已回滚到上一版本并启动"
    }
}
