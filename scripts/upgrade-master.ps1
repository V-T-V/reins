$ErrorActionPreference = 'Continue'
$src = 'C:\go-rmm\master-workspace\go-rmm-agent-new.exe'
$dst = 'C:\Program Files\go-rmm-master\go-rmm-master.exe'
$bak = "$dst.bak-manual"
$want = '6d34e0c924b7f9237235730beab6c4cc29573b9f81763b4153543655f7ffb850'

Stop-Service go-rmm-master -Force
Start-Sleep -Seconds 3
Copy-Item $dst $bak -Force
Copy-Item $src $dst -Force
$sha = (Get-FileHash $dst -Algorithm SHA256).Hash.ToLower()
if ($sha -ne $want) {
    # SHA 校验失败:回滚旧版并拉起,避免留在半部署状态
    Copy-Item $bak $dst -Force
    Start-Service go-rmm-master
    exit 1
}
Start-Service go-rmm-master
