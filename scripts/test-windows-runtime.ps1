# Run on native Windows PowerShell 5.1 or PowerShell 7; no Pester, downloads, or installation.
[CmdletBinding()]
param([switch]$TestExtractionOnly)
$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
$recipe = Join-Path $PSScriptRoot 'windows-runtime.generated.ps1'

function Assert-True($Condition, [string]$Message) {
  if (-not $Condition) { throw $Message }
}

function Get-RequiredFragmentMarker([string]$Source, [string]$Marker) {
  $index = $Source.IndexOf($Marker, [StringComparison]::Ordinal)
  Assert-True ($index -ge 0) "Missing harness fragment marker: $Marker"
  Assert-True ($index -eq $Source.LastIndexOf($Marker, [StringComparison]::Ordinal)) "Duplicate harness fragment marker: $Marker"
  return $index
}

function Get-RuntimeTestFragments([string]$Core, [string]$Finalize, [string]$Desktop) {
  # Windows checkouts may use CRLF; normalize before matching or joining fragments.
  $Core = $Core.Replace("`r`n", "`n")
  $Finalize = $Finalize.Replace("`r`n", "`n")
  $Desktop = $Desktop.Replace("`r`n", "`n")
  $runtimeCall = "`nEnsure-CrabboxWindowsRuntime`n"
  $call = Get-RequiredFragmentMarker $Core $runtimeCall
  $clear = Get-RequiredFragmentMarker $Core 'Remove-Item -LiteralPath $setupCompletePath -Force -ErrorAction Stop'
  Assert-True ($clear -lt $call) 'Readiness invalidation must precede the runtime gate'
  $ready = 'Set-Content -NoNewline -Encoding ASCII -Path $setupCompletePath'
  $null = Get-RequiredFragmentMarker $Finalize $ready
  $completion = Get-RequiredFragmentMarker $Desktop 'if (-not (Test-Path -LiteralPath $setupCompletePath))'
  $desktopReady = Get-RequiredFragmentMarker $Desktop $ready
  Assert-True ($completion -lt $desktopReady) 'Desktop completion must include the readiness marker'
  return [pscustomobject]@{
    Gate = $Core.Substring(0, $call + $runtimeCall.Length)
    Finalize = $Finalize
    Completion = $Desktop.Substring($completion)
  }
}

$core = Get-Content -Raw -LiteralPath (Join-Path $root 'recipes/bootstrap/v1/windowsCore.ps1')
$finalize = Get-Content -Raw -LiteralPath (Join-Path $root 'recipes/bootstrap/v1/windowsFinalize.ps1')
$desktop = Get-Content -Raw -LiteralPath (Join-Path $root 'recipes/bootstrap/v1/windowsDesktop.ps1')
$fragments = Get-RuntimeTestFragments $core $finalize $desktop

# Run these regressions both locally (-TestExtractionOnly) and in the native harness.
$expectedGate = @'

$crabboxSetupWasComplete = Test-Path -LiteralPath $setupCompletePath
if ($crabboxSetupWasComplete) {
  Remove-Item -LiteralPath $setupCompletePath -Force -ErrorAction Stop
}
Ensure-CrabboxWindowsRuntime
'@
$expectedGate = $expectedGate.Replace("`r`n", "`n") + "`n"
foreach ($lineEnding in @("`n", "`r`n")) {
  $variantCore = $core.Replace("`r`n", "`n").Replace("`n", $lineEnding)
  $variantFinalize = $finalize.Replace("`r`n", "`n").Replace("`n", $lineEnding)
  $variantDesktop = $desktop.Replace("`r`n", "`n").Replace("`n", $lineEnding)
  $actual = Get-RuntimeTestFragments $variantCore $variantFinalize $variantDesktop
  Assert-True ($actual.Gate -ceq $expectedGate) 'Line endings changed the exact runtime gate'
  Assert-True ($actual.Finalize -ceq $finalize.Replace("`r`n", "`n")) 'Line endings changed finalization'
  Assert-True ($actual.Completion -ceq $fragments.Completion) 'Line endings changed desktop completion'
  foreach ($invalid in @(
    @{ Core = $variantCore.Replace('Ensure-CrabboxWindowsRuntime', 'Missing-RuntimeCall'); Finalize = $variantFinalize; Desktop = $variantDesktop; Error = 'Missing harness fragment marker' },
    @{ Core = $variantCore.Replace('Remove-Item -LiteralPath $setupCompletePath', 'Missing-ReadinessInvalidation'); Finalize = $variantFinalize; Desktop = $variantDesktop; Error = 'Missing harness fragment marker' },
    @{ Core = $variantCore; Finalize = $variantFinalize.Replace('Set-Content', 'Missing-ReadinessWrite'); Desktop = $variantDesktop; Error = 'Missing harness fragment marker' },
    @{ Core = $variantCore; Finalize = $variantFinalize; Desktop = $variantDesktop.Replace('if (-not (Test-Path -LiteralPath $setupCompletePath))', 'if ($true)'); Error = 'Missing harness fragment marker' },
    @{ Core = $variantCore; Finalize = $variantFinalize; Desktop = $variantDesktop.Replace('Set-Content -NoNewline -Encoding ASCII -Path $setupCompletePath', 'Missing-DesktopReadiness'); Error = 'Missing harness fragment marker' },
    @{ Core = ($variantCore + $lineEnding + 'Ensure-CrabboxWindowsRuntime' + $lineEnding); Finalize = $variantFinalize; Desktop = $variantDesktop; Error = 'Duplicate harness fragment marker' },
    @{ Core = ($lineEnding + 'Ensure-CrabboxWindowsRuntime' + $lineEnding + $variantCore.Replace('Ensure-CrabboxWindowsRuntime', 'Missing-RuntimeCall')); Finalize = $variantFinalize; Desktop = $variantDesktop; Error = 'Readiness invalidation must precede' }
  )) {
    $failure = ''
    try { $null = Get-RuntimeTestFragments $invalid.Core $invalid.Finalize $invalid.Desktop } catch { $failure = $_.ToString() }
    Assert-True ($failure -match $invalid.Error) "Malformed fragment did not fail closed: $failure"
  }
}
Write-Host 'PASS LF/CRLF fragment extraction and missing, duplicate, or misordered markers'
if ($TestExtractionOnly) { return }

$gate = $fragments.Gate
$finalize = $fragments.Finalize
$completion = $fragments.Completion
$scratch = Join-Path ([IO.Path]::GetTempPath()) ('crabbox-runtime-test-' + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $scratch | Out-Null

function Invoke-RuntimeCase([string]$Name, [hashtable]$Options) {
  . $recipe
  $caseState = @{
    Host = @(0, 0x8664, 8); Probes = @($false, $true); ProbeCount = 0
    Downloads = 0; Installs = 0; ExitCode = 0; DownloadFailures = 0
    Signature = 'Valid'; Certificate = $true; BadHash = $false
    Subject = 'CN=Microsoft Corporation, O=Microsoft Corporation, L=Redmond, S=Washington, C=US'
    Pending = ''; Boot = '100'; Admin = $true; StageFailure = $false
    ExpectedError = ''; ExpectedInstalls = 0; ExpectedDownloads = 0; Repeat = 1
    Events = (New-Object 'Collections.Generic.List[string]')
    Path = (Join-Path $scratch $Name)
  }
  foreach ($key in $Options.Keys) { $caseState[$key] = $Options[$key] }
  $setupCompletePath = Join-Path $scratch ($Name + '-ready')
  Set-Content -LiteralPath $setupCompletePath -Value 'stale readiness'

  function Get-CrabboxWindowsRuntimeHost {
    $caseState.Events.Add('host')
    return $caseState.Host
  }
  function Test-CrabboxWindowsRuntime([string]$Architecture) {
    $caseState.Events.Add('probe:' + $Architecture)
    $index = $caseState.ProbeCount
    $caseState.ProbeCount++
    if ($caseState.Probes[$index] -is [string] -and $caseState.Probes[$index] -eq 'error') { throw 'probe infrastructure failure' }
    return $caseState.Probes[$index]
  }
  function Assert-CrabboxWindowsRuntimeAdministrator {
    $caseState.Events.Add('admin')
    if (-not $caseState.Admin) { throw 'requires elevated managed bootstrap' }
  }
  function Get-CrabboxWindowsRuntimeBoot { return $caseState.Boot }
  function Get-CrabboxWindowsRuntimePendingBoot { return $caseState.Pending }
  function Set-CrabboxWindowsRuntimePendingBoot([string]$Boot) {
    $caseState.Events.Add('pending:' + $Boot)
    $caseState.Pending = $Boot
  }
  function New-CrabboxWindowsRuntimeStage {
    $caseState.Events.Add('stage')
    if ($caseState.StageFailure) { throw 'Windows runtime staging ACL is not private' }
    New-Item -ItemType Directory -Path $caseState.Path | Out-Null
    return $caseState.Path
  }
  function Invoke-WebRequest {
    [CmdletBinding()]
    param($Uri, $OutFile, [switch]$UseBasicParsing, $TimeoutSec)
    $caseState.Events.Add('download')
    $caseState.Downloads++
    $suffix = if ($caseState.Host[1] -eq 0xAA64) { '/VC_redist.arm64.exe' } else { '/VC_redist.x64.exe' }
    Assert-True ($Uri.EndsWith($suffix)) 'wrong architecture installer'
    Assert-True ($UseBasicParsing -and $TimeoutSec -eq 120) 'unbounded download'
    if ($caseState.Downloads -le $caseState.DownloadFailures) { throw 'download failed' }
    Set-Content -LiteralPath $OutFile -Value 'synthetic installer; never execute'
  }
  function Start-Sleep { param($Seconds) $caseState.Events.Add('sleep') }
  function Get-FileHash {
    [CmdletBinding()]
    param($LiteralPath, $Algorithm)
    $caseState.Events.Add('hash')
    Assert-True ($Algorithm -eq 'SHA256' -and (Test-Path -LiteralPath $LiteralPath)) 'hash must cover downloaded file'
    # Read the canonical pin rather than duplicating it in the test harness.
    $artifacts = Get-Content -Raw -LiteralPath (Join-Path $root 'recipes/bootstrap/v1/artifacts.json') | ConvertFrom-Json
    $pin = if ($caseState.Host[1] -eq 0xAA64) { $artifacts.artifacts.windowsVCRuntimeARM64.SHA256 } else { $artifacts.artifacts.windowsVCRuntimeX64.SHA256 }
    if ($caseState.BadHash) { $pin = '0' * 64 }
    return [pscustomobject]@{ Hash = $pin }
  }
  function Get-AuthenticodeSignature {
    [CmdletBinding()]
    param($LiteralPath)
    $caseState.Events.Add('signature')
    $certificate = $null
    if ($caseState.Certificate) {
      $certificate = [pscustomobject]@{ SubjectName = (New-Object Security.Cryptography.X509Certificates.X500DistinguishedName($caseState.Subject)) }
    }
    return [pscustomobject]@{ Status = $caseState.Signature; SignerCertificate = $certificate }
  }
  function Start-Process {
    param($FilePath, $ArgumentList, [switch]$Wait, [switch]$PassThru)
    Assert-True ($caseState.Events.Contains('hash') -and $caseState.Events.Contains('signature')) 'execution before verification'
    Assert-True ($caseState.Pending -eq $caseState.Boot) 'execution without durable pending marker'
    Assert-True ($Wait -and $PassThru -and ($ArgumentList -join ' ') -eq '/install /quiet /norestart') 'wrong installer flags'
    Assert-True ($FilePath -eq (Join-Path $caseState.Path 'vc_redist.exe')) 'execution outside private stage'
    $caseState.Events.Add('install')
    $caseState.Installs++
    if ([string]$caseState.ExitCode -eq 'throw') { throw 'installer launch interrupted' }
    return [pscustomobject]@{ ExitCode = $caseState.ExitCode }
  }
  function git { }
  function tar { }
  function Restart-Service { param($Name, [switch]$Force) }

  $failure = ''
  try {
    for ($repeat = 0; $repeat -lt $caseState.Repeat; $repeat++) {
      & ([scriptblock]::Create($gate + $finalize))
    }
  } catch { $failure = $_.ToString() }
  Assert-True ($caseState.Events.Contains('host')) "$Name skipped the runtime helper"
  if ($caseState.ExpectedError) {
    Assert-True ($failure -match $caseState.ExpectedError) "$Name expected $($caseState.ExpectedError), got: $failure"
    Assert-True (-not (Test-Path -LiteralPath $setupCompletePath)) "$Name retained or wrote readiness after failure"
  } else {
    Assert-True (-not $failure) "$Name failed: $failure"
    Assert-True (Test-Path -LiteralPath $setupCompletePath) "$Name did not reach readiness"
  }
  Assert-True ($caseState.Installs -eq $caseState.ExpectedInstalls) "$Name installer count: $($caseState.Installs)"
  Assert-True ($caseState.Downloads -eq $caseState.ExpectedDownloads) "$Name download count: $($caseState.Downloads)"
  Assert-True (-not (Test-Path -LiteralPath $caseState.Path)) "$Name retained helper staging"
  if ($caseState.Installs -gt 0 -and $failure) {
    # Immediate retry must remain blocked even if the DLLs now load.
    $caseState.Probes = @($true); $caseState.ProbeCount = 0
    $retryFailure = ''
    try { Ensure-CrabboxWindowsRuntime } catch { $retryFailure = $_.ToString() }
    Assert-True ($retryFailure -match 'reboot required') "$Name allowed retry before reboot"
    Assert-True ($caseState.ProbeCount -eq 0) "$Name probed through pending reboot gate"
    # A new boot permits an idempotent probe without reinstalling.
    $caseState.Boot = '200'
    Ensure-CrabboxWindowsRuntime
    Assert-True (-not $caseState.Pending) "$Name did not clear old boot marker"
  }
  Write-Host "PASS $Name"
}

try {
  foreach ($path in @($recipe, $PSCommandPath)) {
    $tokens = $null; $errors = $null
    [void][Management.Automation.Language.Parser]::ParseFile($path, [ref]$tokens, [ref]$errors)
    Assert-True ($errors.Count -eq 0) ($errors | Out-String)
  }
  # Exercise actual native APIs and child-process isolation before replacing OS boundaries.
  . $recipe
  $architecture = Get-CrabboxWindowsRuntimeArchitecture
  [CrabboxWindowsRuntimeNative]::Load('kernel32.dll')
  $loadFailed = $false
  try { [CrabboxWindowsRuntimeNative]::Load('crabbox-nonexistent-runtime-fixture.dll') } catch { $loadFailed = $true }
  Assert-True $loadFailed 'native DLL probe accepted a missing DLL'
  $healthy = Test-CrabboxWindowsRuntime $architecture
  Assert-True ($healthy -is [bool]) 'native runtime probe did not return one boolean'
  Write-Host "Native $architecture runtime loadability before mocked cases: $healthy (no install performed)"
  $principal = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())
  if ($principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    $stage = New-CrabboxWindowsRuntimeStage
    try { Assert-True (Test-Path -LiteralPath $stage) 'private staging was not created' }
    finally { Remove-Item -LiteralPath $stage -Recurse -Force }
    Write-Host 'PASS native private staging creation and ACL verification'
  } else {
    Write-Host 'SKIP native staging ACL smoke (requires elevation; mocked ACL failure still tested)'
  }

  Invoke-RuntimeCase 'healthy-idempotent' @{ Probes = @($true, $true); Repeat = 2 }
  Invoke-RuntimeCase 'missing-amd64' @{ ExpectedInstalls = 1; ExpectedDownloads = 1 }
  Invoke-RuntimeCase 'missing-arm64' @{ Host = @(0, 0xAA64, 8); ExpectedInstalls = 1; ExpectedDownloads = 1 }
  Invoke-RuntimeCase 'wrong-hash' @{ BadHash = $true; ExpectedDownloads = 1; ExpectedError = 'SHA-256 mismatch' }
  foreach ($signature in @('NotSigned', 'HashMismatch', 'NotTrusted', 'UnknownError')) {
    Invoke-RuntimeCase "signature-$signature" @{ Signature = $signature; ExpectedDownloads = 1; ExpectedError = 'Valid Authenticode' }
  }
  Invoke-RuntimeCase 'missing-certificate' @{ Certificate = $false; ExpectedDownloads = 1; ExpectedError = 'signer certificate' }
  foreach ($subject in @(
    'CN=Microsoft Corporation impostor, O=Microsoft Corporation, L=Redmond, S=Washington, C=US',
    'CN=Microsoft Corporation, O=Other Corporation, L=Redmond, S=Washington, C=US',
    'CN=Microsoft Corporation, O=Microsoft Corporation, L=Redmond, S=Washington, C=CA',
    'CN=Microsoft Corporation',
    'CN=Microsoft Corporation, CN=Microsoft Corporation, O=Microsoft Corporation, L=Redmond, S=Washington, C=US'
  )) {
    Invoke-RuntimeCase ('wrong-publisher-' + [Guid]::NewGuid().ToString('N')) @{ Subject = $subject; ExpectedDownloads = 1; ExpectedError = 'signer identity' }
  }
  Invoke-RuntimeCase 'reordered-publisher' @{ Subject = 'C=US, S=Washington, L=Redmond, O="Microsoft Corporation", CN="Microsoft Corporation"'; ExpectedInstalls = 1; ExpectedDownloads = 1 }
  Invoke-RuntimeCase 'wow64' @{ Host = @(0x14c, 0x8664, 4); ExpectedError = 'native AMD64 or ARM64' }
  Invoke-RuntimeCase 'x64-emulated-arm64' @{ Host = @(0x8664, 0xAA64, 8); ExpectedError = 'native AMD64 or ARM64' }
  Invoke-RuntimeCase 'native-x86' @{ Host = @(0, 0x14c, 4); ExpectedError = 'native AMD64 or ARM64' }
  Invoke-RuntimeCase 'unsupported-architecture' @{ Host = @(0, 0x200, 8); ExpectedError = 'Unsupported native' }
  Invoke-RuntimeCase 'non-admin' @{ Admin = $false; ExpectedError = 'requires elevated' }
  Invoke-RuntimeCase 'bad-stage-acl' @{ StageFailure = $true; ExpectedError = 'ACL is not private' }
  Invoke-RuntimeCase 'download-retry' @{ DownloadFailures = 2; ExpectedInstalls = 1; ExpectedDownloads = 3 }
  Invoke-RuntimeCase 'download-exhausted' @{ DownloadFailures = 3; ExpectedDownloads = 3; ExpectedError = 'download failed' }
  Invoke-RuntimeCase 'probe-infrastructure' @{ Probes = @('error'); ExpectedError = 'probe infrastructure' }
  Invoke-RuntimeCase 'postcondition-failed' @{ Probes = @($false, $false); ExpectedInstalls = 1; ExpectedDownloads = 1; ExpectedError = 'post-install load check failed' }
  foreach ($code in @(3010, 1641, 1603, 1638)) {
    Invoke-RuntimeCase "installer-$code" @{ ExitCode = $code; ExpectedInstalls = 1; ExpectedDownloads = 1; ExpectedError = "exit $code" }
  }
  Invoke-RuntimeCase 'installer-interrupted' @{ ExitCode = 'throw'; ExpectedInstalls = 1; ExpectedDownloads = 1; ExpectedError = 'launch interrupted' }
  Invoke-RuntimeCase 'pending-reboot-healthy' @{ Pending = '100'; Probes = @($true); ExpectedError = 'reboot required' }
  Invoke-RuntimeCase 'pending-marker-invalid' @{ Pending = 'invalid'; Probes = @($true); ExpectedError = 'Invalid.*pending-boot state' }
  Invoke-RuntimeCase 'after-reboot-healthy' @{ Pending = '50'; Probes = @($true) }

  # Runtime invalidation must preserve the existing desktop first-setup reboot decision.
  foreach ($wasComplete in @($true, $false)) {
    & {
      $setupCompletePath = Join-Path $scratch ('desktop-' + $wasComplete)
      $crabboxSetupWasComplete = $wasComplete
      function Restart-Service { param($Name) }
      function Restart-Computer { param([switch]$Force) throw 'desktop-first-setup-reboot' }
      $failure = ''
      try { & ([scriptblock]::Create($completion)) } catch { $failure = $_.ToString() }
      Assert-True (Test-Path -LiteralPath $setupCompletePath) 'desktop completion marker missing'
      if ($wasComplete) { Assert-True (-not $failure) 'existing desktop requested another reboot' }
      else { Assert-True ($failure -eq 'desktop-first-setup-reboot') 'first desktop setup lost its reboot' }
    }
  }
  Write-Host 'PASS desktop readiness invalidation preserves first-setup reboot policy'
} finally {
  Remove-Item -LiteralPath $scratch -Recurse -Force
}
Write-Host 'Windows runtime behavioral harness passed; publisher/download/installer boundaries were mocked.'
