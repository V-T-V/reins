@echo off
echo === cleanup qwen3 leftovers ===
rmdir /s /q "D:\LocalVoiceAssistant\.venv-qwen3-tts" >nul 2>&1
rmdir /s /q "D:\LocalVoiceAssistant\models\tts\Qwen3-TTS-12Hz-0.6B-CustomVoice" >nul 2>&1
del /q "D:\LocalVoiceAssistant\run_worker.bat" >nul 2>&1
del /q "D:\LocalVoiceAssistant\scripts\setup-qwen3-tts.ps1" >nul 2>&1
del /q "D:\LocalVoiceAssistant\scripts\diagnose-qwen3-tts.ps1" >nul 2>&1
copy /Y C:\reins\slave-workspace\start-all.ps1 D:\LocalVoiceAssistant\scripts\start-all.ps1 >nul
copy /Y C:\reins\slave-workspace\lva_health_check.py D:\LocalVoiceAssistant\scripts\lva_health_check.py >nul
echo === verify ===
if exist "D:\LocalVoiceAssistant\.venv-qwen3-tts" (echo VENV-STILL-EXISTS) else (echo venv-deleted)
if exist "D:\LocalVoiceAssistant\models\tts\Qwen3-TTS-12Hz-0.6B-CustomVoice" (echo MODEL-STILL-EXISTS) else (echo model-deleted)
if exist "D:\LocalVoiceAssistant\run_worker.bat" (echo BAT-STILL-EXISTS) else (echo bat-deleted)
dir D:\LocalVoiceAssistant\scripts\start-all.ps1 D:\LocalVoiceAssistant\scripts\lva_health_check.py | findstr ps1
echo === done ===
