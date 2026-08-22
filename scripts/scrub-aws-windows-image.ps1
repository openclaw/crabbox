$ErrorActionPreference = "Stop"

$Root = if ($env:CRABBOX_SCRUB_ROOT) { $env:CRABBOX_SCRUB_ROOT } else { "C:\" }
Set-Location -LiteralPath $Root
$Removed = [ordered]@{
  authorizedKeys = 0
  cloudInitState = 0
  credentials = 0
  hostIdentity = 0
  prepArtifacts = 0
  shellHistory = 0
  sshHostKeys = 0
  workspaces = 0
}

function Remove-ScrubPath {
  param(
    [string]$Relative,
    [string]$Category
  )
  $Candidate = Join-Path $Root $Relative
  if (Test-Path -LiteralPath $Candidate) {
    Remove-Item -LiteralPath $Candidate -Recurse -Force
    $Removed[$Category]++
  }
}

function Remove-ScrubPrefix {
  param(
    [string]$Relative,
    [string]$Prefix,
    [string]$Category
  )
  $Directory = Join-Path $Root $Relative
  if (-not (Test-Path -LiteralPath $Directory -PathType Container)) {
    return
  }
  foreach ($Entry in Get-ChildItem -LiteralPath $Directory -Force) {
    if ($Entry.Name.StartsWith($Prefix, [StringComparison]::Ordinal)) {
      Remove-Item -LiteralPath $Entry.FullName -Recurse -Force
      $Removed[$Category]++
    }
  }
}

function Test-ScrubPrefix {
  param(
    [string]$Relative,
    [string]$Prefix
  )
  $Directory = Join-Path $Root $Relative
  if (-not (Test-Path -LiteralPath $Directory -PathType Container)) {
    return $false
  }
  return $null -ne (Get-ChildItem -LiteralPath $Directory -Force | Where-Object {
    $_.Name.StartsWith($Prefix, [StringComparison]::Ordinal)
  } | Select-Object -First 1)
}

function Remove-UserSSHState {
  param([string]$Relative)
  $Directory = Join-Path $Root $Relative
  if (-not (Test-Path -LiteralPath $Directory)) {
    return
  }
  $Entry = Get-Item -LiteralPath $Directory -Force
  if (($Entry.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
    Remove-Item -LiteralPath $Directory -Force
    $Removed.credentials++
    return
  }
  $AuthorizedKeys = Join-Path $Directory "authorized_keys"
  if (Test-Path -LiteralPath $AuthorizedKeys) {
    Remove-Item -LiteralPath $AuthorizedKeys -Force
    $Removed.authorizedKeys++
  }
  Remove-Item -LiteralPath $Directory -Recurse -Force
  $Removed.credentials++
}

function Get-ScrubChildEntry {
  param(
    [string]$Parent,
    [string]$Name
  )
  return Get-ChildItem -LiteralPath $Parent -Force | Where-Object {
    [string]::Equals($_.Name, $Name, [StringComparison]::OrdinalIgnoreCase)
  } | Select-Object -First 1
}

function Test-TrustedScrubDirectory {
  param([string]$Relative)
  $Current = $Root
  foreach ($Part in $Relative.Split("\")) {
    $Entry = Get-ScrubChildEntry $Current $Part
    if ($null -eq $Entry -or
      ($Entry.Attributes -band [IO.FileAttributes]::Directory) -eq 0 -or
      ($Entry.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
      return $false
    }
    $Current = $Entry.FullName
  }
  return $true
}

function Test-CompatibilityProfileLink {
  param([IO.FileSystemInfo]$Entry)
  $ExpectedRelative = switch ($Entry.Name) {
    "All Users" { "ProgramData" }
    "Default User" { "Users\Default" }
    default { return $false }
  }
  if (-not (Test-TrustedScrubDirectory $ExpectedRelative)) {
    return $false
  }
  $Targets = @($Entry.Target)
  if ($Targets.Count -ne 1 -or [string]::IsNullOrWhiteSpace([string]$Targets[0])) {
    return $false
  }
  $Target = [string]$Targets[0]
  if (-not [IO.Path]::IsPathRooted($Target)) {
    $Target = Join-Path $Entry.Parent.FullName $Target
  }
  $Actual = [IO.Path]::GetFullPath($Target).TrimEnd("\")
  $Expected = [IO.Path]::GetFullPath((Join-Path $Root $ExpectedRelative)).TrimEnd("\")
  return [string]::Equals($Actual, $Expected, [StringComparison]::OrdinalIgnoreCase)
}

$UsersRoot = Join-Path $Root "Users"
$UsersRootEntry = Get-ScrubChildEntry $Root "Users"
$LinkedUsersRoot = $null -ne $UsersRootEntry -and
  (($UsersRootEntry.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0)
$UserEntries = if ($null -ne $UsersRootEntry -and -not $LinkedUsersRoot) {
  @(Get-ChildItem -LiteralPath $UsersRoot -Directory -Force)
} else {
  @()
}
$Users = @($UserEntries | Where-Object {
    ($_.Attributes -band [IO.FileAttributes]::ReparsePoint) -eq 0
})
$ReparseProfiles = @($UserEntries | Where-Object {
  ($_.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0 -and
  -not (Test-CompatibilityProfileLink $_)
})

Remove-ScrubPath "ProgramData\ssh\administrators_authorized_keys" "authorizedKeys"
Remove-ScrubPrefix "ProgramData\ssh" "ssh_host_" "sshHostKeys"
foreach ($User in $Users) {
  Remove-UserSSHState "Users\$($User.Name)\.ssh"
  Remove-ScrubPath "Users\$($User.Name)\AppData\Roaming\Microsoft\Windows\PowerShell\PSReadLine\ConsoleHost_history.txt" "shellHistory"
  Remove-ScrubPath "Users\$($User.Name)\.aws" "credentials"
  Remove-ScrubPath "Users\$($User.Name)\.config\gh" "credentials"
}

Remove-ScrubPath "ProgramData\Amazon\EC2Launch\state\previous-state.json" "hostIdentity"
foreach ($Relative in @(
  "ProgramData\Amazon\EC2Launch\log",
  "ProgramData\Amazon\EC2Launch\state",
  "ProgramData\Amazon\EC2-Windows\Launch\Log",
  "ProgramData\Amazon\EC2-Windows\Launch\State"
)) {
  Remove-ScrubPath $Relative "cloudInitState"
}
foreach ($Relative in @(
  "ProgramData\crabbox\credentials",
  "ProgramData\crabbox\auth.json",
  "ProgramData\crabbox\vnc.password",
  "ProgramData\crabbox\vnc.pass",
  "ProgramData\crabbox\windows.password",
  "ProgramData\crabbox\windows.username"
)) {
  Remove-ScrubPath $Relative "credentials"
}
$TightVNCRegistryPaths = @(
  "HKLM:\Software\TightVNC\Server",
  "HKLM:\Software\WOW6432Node\TightVNC\Server"
)
$TightVNCPasswordNames = @("Password", "ControlPassword")
foreach ($RegistryPath in $TightVNCRegistryPaths) {
  if (-not (Test-Path -LiteralPath $RegistryPath)) {
    continue
  }
  foreach ($Name in $TightVNCPasswordNames) {
    $Properties = Get-ItemProperty -LiteralPath $RegistryPath
    if ($Properties.PSObject.Properties.Name -contains $Name) {
      Remove-ItemProperty -LiteralPath $RegistryPath -Name $Name -Force
      $Removed.credentials++
    }
  }
}
foreach ($Relative in @(
  "workspace",
  "workspaces",
  "crabbox",
  "ProgramData\crabbox\workspaces"
)) {
  Remove-ScrubPath $Relative "workspaces"
}
Remove-ScrubPrefix "ProgramData\crabbox" "image-prep" "prepArtifacts"
$PrepTask = Get-ScheduledTask -TaskName "CrabboxImagePrep" -ErrorAction SilentlyContinue
if ($null -ne $PrepTask) {
  Unregister-ScheduledTask -TaskName "CrabboxImagePrep" -Confirm:$false
  $Removed.prepArtifacts++
}

$Findings = [Collections.Generic.HashSet[string]]::new([StringComparer]::Ordinal)
function Add-ScrubFinding {
  param(
    [bool]$Condition,
    [string]$Category
  )
  if ($Condition) {
    [void]$Findings.Add($Category)
  }
}

Add-ScrubFinding (Test-Path -LiteralPath (Join-Path $Root "ProgramData\ssh\administrators_authorized_keys")) "authorizedKeys"
Add-ScrubFinding (Test-ScrubPrefix "ProgramData\ssh" "ssh_host_") "sshHostKeys"
Add-ScrubFinding (Test-Path -LiteralPath (Join-Path $Root "ProgramData\Amazon\EC2Launch\state")) "cloudInitState"
Add-ScrubFinding (Test-Path -LiteralPath (Join-Path $Root "ProgramData\Amazon\EC2Launch\log")) "cloudInitState"
Add-ScrubFinding (Test-Path -LiteralPath (Join-Path $Root "ProgramData\Amazon\EC2-Windows\Launch\State")) "cloudInitState"
Add-ScrubFinding (Test-Path -LiteralPath (Join-Path $Root "ProgramData\Amazon\EC2-Windows\Launch\Log")) "cloudInitState"
Add-ScrubFinding (Test-Path -LiteralPath (Join-Path $Root "ProgramData\Amazon\EC2Launch\state\previous-state.json")) "hostIdentity"
Add-ScrubFinding (Test-Path -LiteralPath (Join-Path $Root "ProgramData\crabbox\credentials")) "credentials"
Add-ScrubFinding (Test-Path -LiteralPath (Join-Path $Root "ProgramData\crabbox\auth.json")) "credentials"
Add-ScrubFinding (Test-Path -LiteralPath (Join-Path $Root "ProgramData\crabbox\vnc.password")) "credentials"
Add-ScrubFinding (Test-Path -LiteralPath (Join-Path $Root "ProgramData\crabbox\vnc.pass")) "credentials"
Add-ScrubFinding (Test-Path -LiteralPath (Join-Path $Root "ProgramData\crabbox\windows.password")) "credentials"
Add-ScrubFinding (Test-Path -LiteralPath (Join-Path $Root "ProgramData\crabbox\windows.username")) "credentials"
foreach ($RegistryPath in $TightVNCRegistryPaths) {
  if (-not (Test-Path -LiteralPath $RegistryPath)) {
    continue
  }
  $Properties = Get-ItemProperty -LiteralPath $RegistryPath
  foreach ($Name in $TightVNCPasswordNames) {
    Add-ScrubFinding ($Properties.PSObject.Properties.Name -contains $Name) "credentials"
  }
}
Add-ScrubFinding (Test-Path -LiteralPath (Join-Path $Root "workspace")) "workspaces"
Add-ScrubFinding (Test-Path -LiteralPath (Join-Path $Root "workspaces")) "workspaces"
Add-ScrubFinding (Test-Path -LiteralPath (Join-Path $Root "crabbox")) "workspaces"
Add-ScrubFinding (Test-Path -LiteralPath (Join-Path $Root "ProgramData\crabbox\workspaces")) "workspaces"
Add-ScrubFinding (Test-ScrubPrefix "ProgramData\crabbox" "image-prep") "prepArtifacts"
Add-ScrubFinding ($null -ne (Get-ScheduledTask -TaskName "CrabboxImagePrep" -ErrorAction SilentlyContinue)) "prepArtifacts"
Add-ScrubFinding ($LinkedUsersRoot -or $ReparseProfiles.Count -gt 0) "credentials"
foreach ($User in $Users) {
  Add-ScrubFinding (Test-Path -LiteralPath (Join-Path $Root "Users\$($User.Name)\.ssh")) "credentials"
  Add-ScrubFinding (Test-Path -LiteralPath (Join-Path $Root "Users\$($User.Name)\AppData\Roaming\Microsoft\Windows\PowerShell\PSReadLine\ConsoleHost_history.txt")) "shellHistory"
  Add-ScrubFinding (Test-Path -LiteralPath (Join-Path $Root "Users\$($User.Name)\.aws")) "credentials"
  Add-ScrubFinding (Test-Path -LiteralPath (Join-Path $Root "Users\$($User.Name)\.config\gh")) "credentials"
}

$Evidence = [ordered]@{
  schema = "crabbox-aws-image-scrub/v1"
  target = "windows"
  removed = $Removed
  findings = @($Findings | Sort-Object)
}
$EvidenceJSON = $Evidence | ConvertTo-Json -Depth 5 -Compress
$Result = $EvidenceJSON | & node -e @'
const { createHash } = require("node:crypto");
let text = "";
process.stdin.setEncoding("utf8");
process.stdin.on("data", (chunk) => { text += chunk; });
process.stdin.on("end", () => {
  const canonical = (value) => {
    if (Array.isArray(value)) return `[${value.map(canonical).join(",")}]`;
    if (value && typeof value === "object") {
      return `{${Object.keys(value).sort().map((key) => `${JSON.stringify(key)}:${canonical(value[key])}`).join(",")}}`;
    }
    return JSON.stringify(value);
  };
  const evidence = JSON.parse(text);
  const digest = `sha256:${createHash("sha256").update(canonical(evidence)).digest("hex")}`;
  process.stdout.write(`${canonical({ ...evidence, evidenceDigest: digest })}\n`);
});
'@
if ($LASTEXITCODE -ne 0) {
  exit $LASTEXITCODE
}
Write-Output $Result
