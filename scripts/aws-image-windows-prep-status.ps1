$dir = 'C:\ProgramData\crabbox'
$log = Join-Path $dir 'image-prep.log'
$exitFile = Join-Path $dir 'image-prep.exit'
$done = Join-Path $dir 'image-prep.done'
$failed = Join-Path $dir 'image-prep.failed'
if (Test-Path $done) {
  Write-Output 'crabbox-prep-done'
  if (Test-Path $exitFile) { Get-Content $exitFile }
  if (Test-Path $log) { Get-Content $log -Tail 80 }
  exit 0
}
if (Test-Path $failed) {
  Write-Output 'crabbox-prep-failed'
  if (Test-Path $exitFile) { Get-Content $exitFile }
  if (Test-Path $log) { Get-Content $log -Tail 120 }
  exit 0
}
$task = Get-ScheduledTask -TaskName 'CrabboxImagePrep' -ErrorAction SilentlyContinue
if ($task) {
  $info = Get-ScheduledTaskInfo -TaskName 'CrabboxImagePrep' -ErrorAction SilentlyContinue
  if ($info) {
    Write-Output ("crabbox-prep-state={0} result={1}" -f $task.State,$info.LastTaskResult)
  } else {
    Write-Output ("crabbox-prep-state={0}" -f $task.State)
  }
}
if (Test-Path $log) { Get-Content $log -Tail 30 }
Write-Output 'crabbox-prep-running'
exit 0
