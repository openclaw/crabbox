$ErrorActionPreference = 'Stop'
if ($env:GITHUB_ACTIONS -ne 'true' -or $env:RUNNER_OS -ne 'Windows') { throw 'Requires a disposable native Windows CI runner' }
$root = (Get-Location).Path
$base = '88cec9219d8ef52137b7e6f7047342e2926c9055'
$baseDir = Join-Path $env:RUNNER_TEMP 'crabbox-windows-retry-red'
& git fetch --no-tags origin $base
if ($LASTEXITCODE -ne 0) { throw 'Cannot fetch exact red control' }
& git worktree add --detach $baseDir $base
if ($LASTEXITCODE -ne 0) { throw 'Cannot create isolated red control' }
try {
 Copy-Item (Join-Path $root 'internal/cli/coordinator_read_retry_windows_test.go') (Join-Path $baseDir 'internal/cli/coordinator_read_retry_windows_test.go')
 Push-Location $baseDir
 try {
  $events = @(& go test ./internal/cli -run '^TestCoordinatorReadRetryWindowsConnectionErrors$' -count=1 -json)
  $code = $LASTEXITCODE
 } finally { Pop-Location }
 $events | Tee-Object -FilePath (Join-Path $env:RUNNER_TEMP 'crabbox-windows-retry-red.jsonl') | Write-Host
 if ($code -eq 0) { throw 'Exact88 unexpectedly accepted actual Windows socket retry errors' }
 $parsed = @($events | ForEach-Object { $_ | ConvertFrom-Json })
 foreach ($name in @('wsarecv_aborted','wsarecv_reset')) {
  $required = 'TestCoordinatorReadRetryWindowsConnectionErrors/' + $name
  if (-not ($parsed | Where-Object { $_.Action -eq 'fail' -and $_.Test -eq $required })) { throw "Red control did not exercise $required" }
 }
} finally {
 & git worktree remove --force $baseDir
 if ($LASTEXITCODE -ne 0) { throw 'Cannot clean task-owned red control' }
}
& go test ./internal/cli -run '^TestCoordinatorReadRetryWindowsConnectionErrors$' -count=1 -v
if ($LASTEXITCODE -ne 0) { throw 'Native Windows candidate retry classification failed' }
