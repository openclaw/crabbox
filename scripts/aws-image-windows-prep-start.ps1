$dir = 'C:\ProgramData\crabbox'
$runner = Join-Path $dir 'image-prep-runner.ps1'
$script = Join-Path $dir 'image-prep.ps1'
$log = Join-Path $dir 'image-prep.log'
$exitFile = Join-Path $dir 'image-prep.exit'
$done = Join-Path $dir 'image-prep.done'
$failed = Join-Path $dir 'image-prep.failed'
Remove-Item -Force $log,$exitFile,$done,$failed -ErrorAction SilentlyContinue
@'
$dir = 'C:\ProgramData\crabbox'
$script = Join-Path $dir 'image-prep.ps1'
$log = Join-Path $dir 'image-prep.log'
$exitFile = Join-Path $dir 'image-prep.exit'
$done = Join-Path $dir 'image-prep.done'
$failed = Join-Path $dir 'image-prep.failed'
$ErrorActionPreference = 'Continue'
Remove-Item -Force $exitFile,$done,$failed -ErrorAction SilentlyContinue
& powershell -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File $script *>&1 | Tee-Object -FilePath $log
$code = $LASTEXITCODE
if ($null -eq $code) { $code = 0 }
Set-Content -Path $exitFile -Value $code
if ($code -eq 0) {
  Set-Content -Path $done -Value 'ok'
} else {
  Set-Content -Path $failed -Value $code
}
exit $code
'@ | Set-Content -Path $runner -Encoding UTF8
Unregister-ScheduledTask -TaskName 'CrabboxImagePrep' -Confirm:$false -ErrorAction SilentlyContinue
$action = New-ScheduledTaskAction -Execute 'powershell.exe' -Argument ('-NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File "{0}"' -f $runner)
$principal = New-ScheduledTaskPrincipal -UserId 'SYSTEM' -RunLevel Highest
$settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -ExecutionTimeLimit (New-TimeSpan -Hours 2)
Register-ScheduledTask -TaskName 'CrabboxImagePrep' -Action $action -Principal $principal -Settings $settings -Force | Out-Null
Start-ScheduledTask -TaskName 'CrabboxImagePrep'
Write-Output 'crabbox-prep-started'
