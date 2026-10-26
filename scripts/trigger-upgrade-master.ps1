$r = Invoke-CimMethod -ClassName Win32_Process -MethodName Create -Arguments @{ CommandLine = 'powershell -NoProfile -NonInteractive -ExecutionPolicy Bypass -File C:\reins\master-workspace\upgrade-master.ps1' }
Write-Output ("PID=" + $r.ProcessId + " RC=" + $r.ReturnValue)
