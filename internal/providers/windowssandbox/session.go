package windowssandbox

import "strings"

func stringsForSandboxScript(script string) string {
	script = strings.ReplaceAll(script, "__SANDBOX_PROCESS_NAMES__", windowsSandboxProcessNames)
	script = strings.ReplaceAll(script, "__SANDBOX_NAMES_QUOTED__", strings.ReplaceAll(windowsSandboxProcessNames, ",", "','"))
	return strings.ReplaceAll(script, "\n", "\r\n")
}

// The identity file and this helper are host-only: neither lives in a mapped
// directory. Keep host cleanup and cancellation fallback on the same path.
const sandboxSessionFunctions = `
function Get-SandboxProcesses {
  Get-Process -Name __SANDBOX_PROCESS_NAMES__ -ErrorAction SilentlyContinue
}

function Open-SandboxProcess($Identity) {
  $candidate = Get-Process -Id $Identity.Id -ErrorAction SilentlyContinue
  if ($null -eq $candidate) { return }
  $retain = $false
  try {
    # Pin the handle before checking creation time; Kill must use this object.
    $null = $candidate.Handle
    if ($candidate.StartTime.ToUniversalTime().Ticks -ne $Identity.StartTimeTicks) { return }
    if (-not $candidate.HasExited) { $retain = $true; return $candidate }
  } catch {
    Write-Warning ('Cannot verify Windows Sandbox process {0}; leaving it running.' -f $Identity.Id)
  } finally {
    if (-not $retain) { $candidate.Dispose() }
  }
}

function Save-SandboxIdentities {
  $json = ConvertTo-Json -InputObject @($script:identities) -Compress
  Set-Content -LiteralPath ($IdentityPath + '.tmp') -Value $json -Encoding ASCII
  Move-Item -LiteralPath ($IdentityPath + '.tmp') -Destination $IdentityPath -Force
}

function Add-SandboxIdentity($Process, [bool]$Stop) {
  $null = $Process.Handle
  $script:identities += [pscustomobject]@{
    Id = $Process.Id
    StartTimeTicks = $Process.StartTime.ToUniversalTime().Ticks
    Stop = $Stop
  }
  Save-SandboxIdentities
}

function Update-SandboxIdentities {
  try {
    $snapshot = @(Get-CimInstance -ClassName Win32_Process -ErrorAction Stop)
    do {
      $added = $false
      foreach ($parentIdentity in @($script:identities)) {
        $parent = Open-SandboxProcess $parentIdentity
        if ($null -eq $parent) { continue }
        try {
          foreach ($row in $snapshot) {
            if ($row.ParentProcessId -ne $parent.Id -or $null -eq $row.CreationDate) { continue }
            $child = Get-Process -Id $row.ProcessId -ErrorAction SilentlyContinue
            if ($null -eq $child) { continue }
            try {
              $null = $child.Handle
              $ticks = $child.StartTime.ToUniversalTime().Ticks
              # CIM timestamps have microsecond precision; reject a reused child PID.
              if (($ticks - $ticks % 10) -ne $row.CreationDate.ToUniversalTime().Ticks) { continue }
              if ($ticks -lt $parentIdentity.StartTimeTicks -or $parent.HasExited) { continue }
              $known = @($script:identities | Where-Object { $_.Id -eq $child.Id -and $_.StartTimeTicks -eq $ticks })
              if ($known.Count -gt 0) { continue }
              Add-SandboxIdentity $child ($child.ProcessName -in @('__SANDBOX_NAMES_QUOTED__'))
              $added = $true
            } finally { $child.Dispose() }
          }
        } finally { $parent.Dispose() }
      }
    } while ($added)
  } catch {
    if (-not $script:ownershipWarning) {
      Write-Warning 'Cannot confirm Windows Sandbox launcher handoff; unverified processes will be left running.'
      $script:ownershipWarning = $true
    }
  }
}

function Get-OwnedSandboxProcesses {
  foreach ($identity in $script:identities) {
    if ($identity.Stop) { Open-SandboxProcess $identity }
  }
}

function Report-UnconfirmedSandbox {
  Write-Warning 'Windows Sandbox cleanup ownership could not be confirmed; leaving unverified processes and preserving the host workspace. Close the sandbox window manually.'
  Set-Content -LiteralPath ($IdentityPath + '.unconfirmed') -Value 'cleanup unconfirmed' -Encoding ASCII
}

function Report-UnownedSandboxProcesses {
  foreach ($candidate in @(Get-SandboxProcesses)) {
    try {
      $null = $candidate.Handle
      $ticks = $candidate.StartTime.ToUniversalTime().Ticks
      $known = @($script:identities | Where-Object { $_.Stop -and $_.Id -eq $candidate.Id -and $_.StartTimeTicks -eq $ticks })
      if ($known.Count -eq 0) { Report-UnconfirmedSandbox; return }
    } catch { Report-UnconfirmedSandbox; return }
    finally { $candidate.Dispose() }
  }
}

function Stop-SandboxSession {
  # Descendants first, so terminating the launcher does not hide a handoff.
  $owned = @(Get-OwnedSandboxProcesses)
  [array]::Reverse($owned)
  foreach ($candidate in $owned) {
    try {
      $candidate.Kill()
      if (-not $candidate.WaitForExit(2000)) { Report-UnconfirmedSandbox }
    } catch { Report-UnconfirmedSandbox }
    finally { $candidate.Dispose() }
  }
  Report-UnownedSandboxProcesses
}

function Wait-SandboxSession([int]$TimeoutSeconds) {
  $waitDeadline = (Get-Date).AddSeconds($TimeoutSeconds)
  while ($true) {
    $running = @(Get-OwnedSandboxProcesses)
    foreach ($candidate in $running) { $candidate.Dispose() }
    if ($running.Count -eq 0) { break }
    Update-SandboxIdentities
    if ((Get-Date) -gt $waitDeadline) {
      Stop-SandboxSession
      return
    }
    Start-Sleep -Milliseconds 500
  }
  Report-UnownedSandboxProcesses
}
`

func sandboxSessionScript() string {
	return stringsForSandboxScript(sandboxSessionFunctions)
}

func canceledSandboxScript(identityPath string) string {
	return sandboxSessionScript() + `
$IdentityPath = ` + psSingleQuote(identityPath) + `
$script:identities = @()
try {
  $script:identities = @(Get-Content -LiteralPath $IdentityPath -Raw -ErrorAction Stop | ConvertFrom-Json -ErrorAction Stop)
  if ($script:identities.Count -eq 0) { throw 'No captured identity' }
  Stop-SandboxSession
} catch {
  Report-UnconfirmedSandbox
}
`
}
