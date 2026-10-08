param([string]$OutputDirectory = '')
$ErrorActionPreference = 'Stop'
if (-not $IsWindows -and $env:OS -ne 'Windows_NT') { throw 'This lane requires Windows' }
if (-not $OutputDirectory) {
  $base = if ($env:RUNNER_TEMP) { $env:RUNNER_TEMP } else { [IO.Path]::GetTempPath() }
  $OutputDirectory = Join-Path $base ('crabbox-sftp-' + [Guid]::NewGuid().ToString('N'))
}
$OutputDirectory = [IO.Path]::GetFullPath($OutputDirectory)
if (Test-Path -LiteralPath $OutputDirectory) { throw 'SFTP lane output directory must be new' }
New-Item -ItemType Directory -Path $OutputDirectory | Out-Null
$archive = Join-Path $OutputDirectory 'openssh.zip'
$url = 'https://github.com/PowerShell/Win32-OpenSSH/releases/download/10.0.0.0p2-Preview/OpenSSH-Win64.zip'
curl.exe --fail --location --retry 2 --connect-timeout 30 --max-time 180 --silent --show-error --output $archive $url
if ($LASTEXITCODE -ne 0) { throw 'OpenSSH fixture download failed' }
$hash = (Get-FileHash -LiteralPath $archive -Algorithm SHA256).Hash.ToLowerInvariant()
if ($hash -ne '23f50f3458c4c5d0b12217c6a5ddfde0137210a30fa870e98b29827f7b43aba5') { throw 'OpenSSH fixture digest mismatch' }
$zip = [IO.Compression.ZipFile]::OpenRead($archive)
$bytes = [int64]0
try {
  if ($zip.Entries.Count -gt 1000) { throw 'Too many OpenSSH archive entries' }
  foreach ($entry in $zip.Entries) {
    $name = $entry.FullName.Replace('/', '\')
    if ([IO.Path]::IsPathRooted($name) -or $name.Contains(':') -or ($name.Split('\') -contains '..')) { throw 'Unsafe archive entry' }
    if ((($entry.ExternalAttributes -shr 16) -band 0xF000) -eq 0xA000) { throw 'Archive links are not permitted' }
    $bytes += $entry.Length
    if ($bytes -gt 64MB) { throw 'OpenSSH fixture expansion exceeds 64 MiB' }
  }
} finally { $zip.Dispose() }
$tools = Join-Path $OutputDirectory 'tools'
[IO.Compression.ZipFile]::ExtractToDirectory($archive, $tools)
$env:CRABBOX_TEST_OPENSSH_DIR = Join-Path $tools 'OpenSSH-Win64'
$env:CRABBOX_TEST_REQUIRE_SFTP = '1'
$env:TEMP = Join-Path $OutputDirectory 'tmp'
$env:TMP = $env:TEMP
New-Item -ItemType Directory -Path $env:TEMP | Out-Null
$required = @('TestWindowsSFTPPathContract','TestWindowsSFTPMetadataOutputIsBoundedThroughCopy','TestWindowsSFTPBehavior','TestWindowsSFTPStreamedLargeFile','TestWindowsFileSnapshotIdentity','TestWindowsFileSnapshotRejectsReplacedIdentity','TestWindowsFileSnapshotRejectsSymlinkReplacement','TestWindowsFileSnapshotManagedTransferPath')

# Each lane needs a real Windows host, so none of them can run in the Linux
# suite. A lane that loses a required case is a failure, never a quiet skip.
function Invoke-NativeLane([string]$Package, [string]$Pattern, [string[]]$Required, [string]$LogName) {
  $events = @(go test $Package -run $Pattern -count=1 -timeout=15m -json)
  $code = $LASTEXITCODE
  $log = Join-Path $OutputDirectory $LogName
  [IO.File]::WriteAllText($log, [string]::Join("`n", [string[]]$events) + "`n", [Text.UTF8Encoding]::new($false))
  $events | ForEach-Object { Write-Host $_ }
  if ($code -ne 0) { exit $code }
  $records = @($events | ForEach-Object { $_ | ConvertFrom-Json })
  foreach ($name in $Required) {
    foreach ($action in @('run','pass')) {
      if (-not ($records | Where-Object { $_.Test -eq $name -and $_.Action -eq $action })) { throw "$name did not $action" }
    }
  }
  if ($records | Where-Object { $_.Action -eq 'skip' }) { throw "Required lane $Package skipped a case" }
  Write-Host "Verified $Package lane: $log"
}

Invoke-NativeLane '.\internal\cli' '^TestWindows(SFTP|FileSnapshot)' $required 'tests.jsonl'
Invoke-NativeLane '.\internal\providers\hyperv' '^TestGuestCredentialSurvivesCorePollutedModulePath$' @('TestGuestCredentialSurvivesCorePollutedModulePath') 'hyperv-credential.jsonl'
