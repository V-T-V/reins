param(
  [ValidateSet('ollama','mock')][string]$LLM = 'ollama',
  [string]$Model = 'qwen3.5:2b',
  [ValidateSet('real','mock')][string]$Speech = 'real',
  [string]$ASRModelPath = '',
  [ValidateSet('auto','cpu','cuda')][string]$ASRDevice = 'auto',
  [ValidateSet('sapi','edge','qwen3','cosyvoice')][string]$TTSBackend = 'cosyvoice',
  [string]$TTSVoice = '',
  [ValidateRange(-10,10)][int]$TTSRate = -1,
  [ValidateRange(0,100)][int]$TTSVolume = 100,
  [string]$TTSModelPath = 'models/tts/CosyVoice2-0.5B',
  [ValidateSet('auto','cpu','cuda')][string]$TTSDevice = 'auto',
  [ValidateSet('auto','bfloat16','float16','float32')][string]$TTSDType = 'auto',
  [string]$TTSSpeaker = 'Serena',
  [string]$TTSInstruct = '用自然、亲切、专业的语气回答，语速适中，停顿自然。',
  [ValidateSet('none','sapi')][string]$TTSFallbackBackend = 'sapi',
  [ValidateRange(32,500)][int]$TTSChunkRunes = 96,
  [switch]$OpenBrowser
)
$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

function Wait-Http {
  param(
    [string]$Url,
    [int]$Seconds = 20
  )
  for ($i = 0; $i -lt $Seconds; $i++) {
    try { Invoke-RestMethod -Uri $Url -TimeoutSec 2 | Out-Null; return $true } catch { Start-Sleep -Seconds 1 }
  }
  return $false
}

function Show-SpeechHealth {
  param([object]$Health)
  if (-not $Health -or [string]$Health.backend -ne 'real' -or -not $Health.tts) { return }
  $tts = $Health.tts
  $requested = [string]$tts.requested_backend
  $effective = [string]$tts.effective_backend
  $device = [string]$tts.effective_device
  $dtype = [string]$tts.effective_dtype
  $speaker = [string]$tts.speaker
  $loadMs = [string]$tts.load_elapsed_ms
  Write-Host ("speech TTS: requested={0} effective={1} device={2} dtype={3} speaker={4} load_ms={5}" -f $requested, $effective, $device, $dtype, $speaker, $loadMs) -ForegroundColor Cyan
  if ($requested -eq 'cosyvoice' -and $effective -eq 'sapi') {
    $reason = [string]$tts.fallback_reason
    if ([string]::IsNullOrWhiteSpace($reason)) { $reason = 'unknown reason' }
    Write-Warning ("CosyVoice-TTS fell back to SAPI: {0}" -f $reason)
  }
}

function Get-LogTail {
  param(
    [string]$Path,
    [int]$Lines = 20
  )
  if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) { return '(no output)' }
  try {
    $content = Get-Content -LiteralPath $Path -Tail $Lines -ErrorAction Stop
    if ($null -eq $content) { return '(empty)' }
    return (($content | ForEach-Object { [string]$_ }) -join [Environment]::NewLine)
  } catch {
    return "(unable to read log: $($_.Exception.Message))"
  }
}

function Get-AppLogSummary {
  param(
    [string]$StdoutPath,
    [string]$StderrPath
  )
  $stdoutTail = Get-LogTail -Path $StdoutPath
  $stderrTail = Get-LogTail -Path $StderrPath
  return "stdout log: $StdoutPath`n$stdoutTail`nstderr log: $StderrPath`n$stderrTail"
}

function Wait-AppHealth {
  param(
    [System.Diagnostics.Process]$Process,
    [string]$Url,
    [int]$Seconds,
    [string]$StdoutPath,
    [string]$StderrPath
  )
  $deadline = (Get-Date).AddSeconds([Math]::Max(1, $Seconds))
  while ((Get-Date) -lt $deadline) {
    $Process.Refresh()
    if ($Process.HasExited) {
      $exitCode = $Process.ExitCode
      $logs = Get-AppLogSummary -StdoutPath $StdoutPath -StderrPath $StderrPath
      throw "local-voice-assistant exited before health succeeded (exit code $exitCode).`n$logs"
    }
    try {
      Invoke-RestMethod -UseBasicParsing -Uri $Url -TimeoutSec 2 | Out-Null
      return $true
    } catch { }
    $Process.Refresh()
    if ($Process.HasExited) {
      $exitCode = $Process.ExitCode
      $logs = Get-AppLogSummary -StdoutPath $StdoutPath -StderrPath $StderrPath
      throw "local-voice-assistant exited before health succeeded (exit code $exitCode).`n$logs"
    }
    $remainingMilliseconds = [int]([Math]::Min(1000, [Math]::Max(1, (($deadline - (Get-Date)).TotalMilliseconds))))
    Start-Sleep -Milliseconds $remainingMilliseconds
  }
  $Process.Refresh()
  if ($Process.HasExited) {
    $exitCode = $Process.ExitCode
    $logs = Get-AppLogSummary -StdoutPath $StdoutPath -StderrPath $StderrPath
    throw "local-voice-assistant exited before health succeeded (exit code $exitCode).`n$logs"
  }
  return $false
}

$appPath = Join-Path $root 'bin\local-voice-assistant.exe'
if (-not (Test-Path -LiteralPath $appPath)) { $appPath = Join-Path $root 'local-voice-assistant.exe' }
$mockWorkerPath = Join-Path $root 'bin\mock-speech-worker.exe'
if (-not (Test-Path -LiteralPath $mockWorkerPath)) { $mockWorkerPath = Join-Path $root 'runtime\speech\mock-speech-worker.exe' }
if (-not (Test-Path -LiteralPath $appPath) -or -not (Test-Path -LiteralPath $mockWorkerPath)) {
  if (-not (Get-Command go -ErrorAction SilentlyContinue)) { throw 'Built binaries were not found and Go is not installed. Build the package first.' }
  & '.\scripts\build.ps1' -Profile dev -Version 0.1.0-dev
  $appPath = Join-Path $root 'bin\local-voice-assistant.exe'
  $mockWorkerPath = Join-Path $root 'bin\mock-speech-worker.exe'
}

$ollama = Get-Command ollama -ErrorAction SilentlyContinue
if ($LLM -eq 'ollama') {
  if (-not $ollama) { throw 'Ollama executable was not found. Install Ollama first.' }
  if (-not (Wait-Http 'http://127.0.0.1:11434/api/tags' 3)) {
    Start-Process -FilePath $ollama.Source -ArgumentList 'serve' -WindowStyle Hidden | Out-Null
    if (-not (Wait-Http 'http://127.0.0.1:11434/api/tags' 30)) { throw 'Ollama did not start within 30 seconds.' }
  }
  $tags = Invoke-RestMethod 'http://127.0.0.1:11434/api/tags'
  if (-not ($tags.models | Where-Object { $_.name -eq $Model })) {
    throw "Ollama model '$Model' is not installed. Run: ollama pull $Model"
  }
}

$speechHealth = $null
try { $speechHealth = Invoke-RestMethod 'http://127.0.0.1:18081/health' -TimeoutSec 2 } catch { }
$actualDevice = $null
$actualComputeType = $null
$speechListener = Get-NetTCPConnection -LocalPort 18081 -State Listen -ErrorAction SilentlyContinue
if ($speechHealth -and $speechHealth.backend -ne $(if ($Speech -eq 'real') { 'real' } else { 'mock' })) {
  throw "Speech worker at 127.0.0.1:18081 is '$($speechHealth.backend)', but '$Speech' was requested. Run scripts/stop-all.ps1 first."
}
if ($speechHealth -and $Speech -eq 'real') {
  $runningTTSBackend = [string]$speechHealth.tts.requested_backend
  if ([string]::IsNullOrWhiteSpace($runningTTSBackend) -or $runningTTSBackend -ne $TTSBackend) {
    throw "Speech worker at 127.0.0.1:18081 uses TTS backend '$runningTTSBackend', but '$TTSBackend' was requested. Run scripts/stop-all.ps1 first."
  }
  Show-SpeechHealth -Health $speechHealth
}
if (-not $speechHealth -and $speechListener) {
  throw 'Port 18081 is already occupied by a process that is not a healthy local speech worker.'
}
  if (-not $speechHealth) {
  if ($Speech -eq 'real') {
    $python = Get-Command python -ErrorAction SilentlyContinue
    if ($TTSBackend -eq 'cosyvoice') {
      $cosyPython = Join-Path $root '.venv-cosyvoice\Scripts\python.exe'
      if (Test-Path -LiteralPath $cosyPython -PathType Leaf) {
        $python = [pscustomobject]@{ Source = (Resolve-Path -LiteralPath $cosyPython).Path }
      }
    }
    if (-not $python) { throw 'Python executable was not found for the real speech worker.' }
    if (-not $ASRModelPath) {
      $localModel = Join-Path $root 'models\asr\whisper-small'
      $referenceModel = Join-Path $root '..\..\..\M_X_M\realtime-digital-human\models\whisper-small'
      if (Test-Path -LiteralPath $localModel) { $ASRModelPath = (Resolve-Path $localModel).Path }
      elseif (Test-Path -LiteralPath $referenceModel) { $ASRModelPath = (Resolve-Path $referenceModel).Path }
      else { throw 'Whisper model not found. Set -ASRModelPath to a local faster-whisper snapshot.' }
    }
    $actualDevice = $ASRDevice.ToLowerInvariant()
    $computeType = 'int8'
    if ($ASRDevice -eq 'auto') {
      $actualDevice = 'cpu'
      try {
        $cudaDevices = (& $python.Source -c "import ctranslate2; print(ctranslate2.get_cuda_device_count())").Trim()
        if ([int]$cudaDevices -gt 0) { $actualDevice = 'cuda'; $computeType = 'int8_float16' }
      } catch { }
    } elseif ($ASRDevice -eq 'cuda') { $computeType = 'int8_float16' }
    $actualComputeType = $computeType
    $workerScript = Join-Path $root 'cmd\real-speech-worker\worker.py'
    if (-not (Test-Path -LiteralPath $workerScript)) { $workerScript = Join-Path $root 'runtime\speech\real-speech-worker.py' }
    if (-not (Test-Path -LiteralPath $workerScript)) { throw 'Real Speech Worker script was not found.' }
    $workerArgs = @($workerScript, '--address','127.0.0.1:18081', '--asr-model',$ASRModelPath, '--asr-device',$actualDevice, '--asr-compute-type',$computeType, '--tts-backend',$TTSBackend, '--tts-voice',$TTSVoice, '--tts-rate',$TTSRate, '--tts-volume',$TTSVolume, '--tts-model-path',$TTSModelPath, '--tts-device',$TTSDevice, '--tts-dtype',$TTSDType, '--tts-speaker',$TTSSpeaker, '--tts-instruct',$TTSInstruct, '--tts-fallback-backend',$TTSFallbackBackend, '--sample-rate','16000')
    if ($TTSBackend -eq 'cosyvoice') {
      $workerArgs += @('--tts-prompt-wav',(Join-Path $root 'asset\zero_shot_prompt.wav'), '--tts-prompt-text','希望你以后能够做的比我还好呦。')
    }
    $worker = Start-Process -FilePath $python.Source -ArgumentList $workerArgs -WorkingDirectory $root -WindowStyle Hidden -PassThru
    if (-not (Wait-Http 'http://127.0.0.1:18081/health' 300)) { throw 'Real Speech Worker did not start within 300 seconds.' }
    try {
      $speechHealth = Invoke-RestMethod 'http://127.0.0.1:18081/health' -TimeoutSec 5
    } catch {
      throw "Real Speech Worker health could not be read after startup: $($_.Exception.Message)"
    }
    if ([string]$speechHealth.backend -ne 'real') {
      throw "Speech worker health reported backend '$($speechHealth.backend)' after starting a real worker."
    }
    Show-SpeechHealth -Health $speechHealth
  } else {
    $worker = Start-Process -FilePath $mockWorkerPath -ArgumentList '--address','127.0.0.1:18081' -WorkingDirectory $root -WindowStyle Hidden -PassThru
    if (-not (Wait-Http 'http://127.0.0.1:18081/health' 10)) { throw 'Mock Speech Worker did not start.' }
    try { $speechHealth = Invoke-RestMethod 'http://127.0.0.1:18081/health' -TimeoutSec 5 } catch { throw "Mock Speech Worker health could not be read after startup: $($_.Exception.Message)" }
  }
} else { $worker = $null }

if ($Speech -eq 'real') {
  if ($speechHealth) {
    $reportedDevice = [string]$speechHealth.asr.device
    if ($reportedDevice -match '^(?i:cpu|cuda)$') {
      $actualDevice = $reportedDevice.ToLowerInvariant()
    } elseif ($ASRDevice -eq 'cuda') {
      $actualDevice = 'cuda'
    } else {
      $actualDevice = 'cpu'
    }
    $reportedComputeType = [string]$speechHealth.asr.compute_type
    if ($reportedComputeType.Trim()) { $actualComputeType = $reportedComputeType }
  }
} elseif (-not $actualDevice) {
  $actualDevice = 'cpu'
  $actualComputeType = 'int8'
}

$configTemplate = if ($LLM -eq 'ollama') { Join-Path $root 'config.ollama.json' } elseif ($Speech -eq 'real') { Join-Path $root 'config.real-speech.json' } else { Join-Path $root 'config.example.json' }
if (-not (Test-Path -LiteralPath $configTemplate)) { throw "Config template not found: $configTemplate" }
$launchConfig = Join-Path $root '.runtime-launch.config.json'
$launchObject = Get-Content -LiteralPath $configTemplate -Raw | ConvertFrom-Json
$launchObject.llm.backend = $LLM
$launchObject.llm.start_process = $false
if ($LLM -eq 'ollama') { $launchObject.llm.model = $Model; $launchObject.llm.base_url = 'http://127.0.0.1:11434' }
$launchObject.speech.backend = 'worker'
$launchObject.speech.start_process = $false
$launchObject.speech.address = '127.0.0.1:18081'
$launchObject.speech.executable = 'python'
$launchObject.speech.tts_backend = $TTSBackend
if ($actualDevice) { $launchObject.speech.asr_device = $actualDevice }
if ($actualComputeType) { $launchObject.speech.asr_compute_type = $actualComputeType }
$launchObject.speech.tts_voice = $TTSVoice
$launchObject.speech.tts_rate = $TTSRate
$launchObject.speech.tts_volume = $TTSVolume
$launchObject.speech.tts_model_path = $TTSModelPath
$launchObject.speech.tts_device = $TTSDevice
$launchObject.speech.tts_dtype = $TTSDType
$launchObject.speech.tts_speaker = $TTSSpeaker
$launchObject.speech.tts_instruct = $TTSInstruct
$launchObject.speech.tts_fallback_backend = $TTSFallbackBackend
$launchObject.speech.tts_chunk_runes = $TTSChunkRunes
$runtimeLogDir = Join-Path $root 'data'
if (-not (Test-Path -LiteralPath $runtimeLogDir -PathType Container)) {
  New-Item -ItemType Directory -Path $runtimeLogDir -Force | Out-Null
}
$stdoutLog = Join-Path $runtimeLogDir 'runtime-launch.stdout.log'
$stderrLog = Join-Path $runtimeLogDir 'runtime-launch.stderr.log'
$utf8NoBom = New-Object -TypeName System.Text.UTF8Encoding -ArgumentList $false
$launchJson = $launchObject | ConvertTo-Json -Depth 20
[System.IO.File]::WriteAllText($launchConfig, $launchJson, $utf8NoBom)
$appPath = (Resolve-Path -LiteralPath $appPath).Path
$listener = Get-NetTCPConnection -LocalPort 8080 -State Listen -ErrorAction SilentlyContinue
foreach ($item in @($listener)) {
  $owner = Get-Process -Id $item.OwningProcess -ErrorAction SilentlyContinue
  if ($owner -and $owner.Path -eq $appPath) { Stop-Process -Id $owner.Id -Force }
}
[System.IO.File]::WriteAllText($stdoutLog, '', $utf8NoBom)
[System.IO.File]::WriteAllText($stderrLog, '', $utf8NoBom)
$app = Start-Process -FilePath $appPath -ArgumentList '-config',$launchConfig -WorkingDirectory $root -WindowStyle Hidden -RedirectStandardOutput $stdoutLog -RedirectStandardError $stderrLog -PassThru
if (-not (Wait-AppHealth -Process $app -Url 'http://127.0.0.1:8080/api/health' -Seconds 15 -StdoutPath $stdoutLog -StderrPath $stderrLog)) {
  $logs = Get-AppLogSummary -StdoutPath $stdoutLog -StderrPath $stderrLog
  throw "local-voice-assistant did not become healthy within 15 seconds.`n$logs"
}

Write-Host "local-voice-assistant: http://127.0.0.1:8080/ (PID $($app.Id))" -ForegroundColor Green
if ($worker) { Write-Host "$Speech speech worker: 127.0.0.1:18081 (PID $($worker.Id))" -ForegroundColor Green }
if ($LLM -eq 'ollama') { Write-Host "ollama: http://127.0.0.1:11434 (model $Model)" -ForegroundColor Green }
if ($OpenBrowser) { Start-Process 'http://127.0.0.1:8080/' }
