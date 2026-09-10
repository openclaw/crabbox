param([string]$Launcher = (Join-Path $PSScriptRoot "start-windows-detached-process.ps1"))
$ErrorActionPreference = "Stop"
$stage = Join-Path $env:TEMP ("crabbox-detach-test " + [Guid]::NewGuid().ToString("N"))
$childPid = $null
try {
  New-Item -ItemType Directory -Path $stage | Out-Null
  $source = @'
$ErrorActionPreference = 'Stop'
[pscustomobject]@{pid=$PID; cwd=$PWD.Path; marker=$env:CRABBOX_DETACH_TEST_MARKER; text=$args[0]} | ConvertTo-Json | Set-Content -LiteralPath 'result.json' -Encoding UTF8
Start-Sleep 30
'@
  $child = Join-Path $stage "child with spaces.ps1"
  [IO.File]::WriteAllText($child, $source, [Text.UTF8Encoding]::new($true))
  $env:CRABBOX_DETACH_TEST_MARKER = "inherited-fixture"
  $childPid = & $Launcher -FilePath powershell.exe -ArgumentList ('-NoProfile -ExecutionPolicy Bypass -File "' + $child + '" "two words"') -WorkingDirectory $stage
  if ($childPid -isnot [int] -or $childPid -le 0) { throw "launcher did not return one PID" }
  $resultFile = Join-Path $stage "result.json"
  $deadline = (Get-Date).AddSeconds(20)
  while (-not (Test-Path -LiteralPath $resultFile)) {
    if ((Get-Date) -gt $deadline) { throw "detached child did not write its result" }
    Start-Sleep -Milliseconds 100
  }
  $result = Get-Content -Raw -LiteralPath $resultFile | ConvertFrom-Json
  if ($result.pid -ne $childPid -or $result.cwd -ne $stage -or $result.marker -ne "inherited-fixture" -or $result.text -ne "two words") { throw "PID, cwd, environment or argument transfer failed" }
  $null = Get-Process -Id $childPid
  foreach ($case in @("missing-executable", "missing-directory")) {
    $rejected = $false
    try {
      if ($case -eq "missing-executable") { & $Launcher -FilePath (Join-Path $stage "missing.exe") }
      else { & $Launcher -FilePath powershell.exe -WorkingDirectory (Join-Path $stage "missing") }
    } catch { $rejected = $true }
    if (-not $rejected) { throw "launcher accepted $case" }
  }
  Write-Output "PASS detached launcher: PID, quoted paths/arguments, cwd, inherited environment, invalid inputs"
} finally {
  if ($childPid) { Stop-Process -Id $childPid -Force -ErrorAction SilentlyContinue }
  Remove-Item Env:CRABBOX_DETACH_TEST_MARKER -ErrorAction SilentlyContinue
  Remove-Item -LiteralPath $stage -Recurse -Force -ErrorAction SilentlyContinue
}
