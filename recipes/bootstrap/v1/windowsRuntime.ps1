
# Native machine prerequisite only; repository runtimes and package managers remain caller-owned.
function Get-CrabboxWindowsRuntimeInterop {
  return @'
using System;
using System.ComponentModel;
using System.Runtime.InteropServices;
public static class CrabboxWindowsRuntimeNative {
  [DllImport("kernel32.dll", SetLastError = true)]
  static extern bool IsWow64Process2(IntPtr process, out ushort processMachine, out ushort nativeMachine);
  [DllImport("kernel32.dll")]
  static extern IntPtr GetCurrentProcess();
  [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
  static extern IntPtr LoadLibraryEx(string path, IntPtr file, uint flags);
  [DllImport("kernel32.dll")]
  static extern bool FreeLibrary(IntPtr module);
  public static ushort[] Host() {
    ushort process, native;
    if (!IsWow64Process2(GetCurrentProcess(), out process, out native))
      throw new Win32Exception(Marshal.GetLastWin32Error());
    return new ushort[] { process, native, (ushort)IntPtr.Size };
  }
  public static void Load(string name) {
    string path = System.IO.Path.Combine(Environment.SystemDirectory, name);
    // Resolve imports and run DLL initialization, searching only the native system directory.
    IntPtr module = LoadLibraryEx(path, IntPtr.Zero, 0x00000800);
    if (module == IntPtr.Zero) throw new Win32Exception(Marshal.GetLastWin32Error(), name + " cannot load");
    FreeLibrary(module);
  }
}
'@
}

function Get-CrabboxWindowsRuntimeHost {
  try {
    if (-not ('CrabboxWindowsRuntimeNative' -as [type])) {
      Add-Type -TypeDefinition (Get-CrabboxWindowsRuntimeInterop) -ErrorAction Stop
    }
    return [CrabboxWindowsRuntimeNative]::Host()
  } catch {
    throw "Cannot determine native Windows runtime architecture using IsWow64Process2; use native 64-bit PowerShell on supported Windows: $_"
  }
}

function Get-CrabboxWindowsRuntimeArchitecture {
  $hostMachine = @(Get-CrabboxWindowsRuntimeHost)
  if ($hostMachine.Count -ne 3 -or $hostMachine[0] -ne 0 -or $hostMachine[2] -ne 8) {
    throw "Windows runtime bootstrap rejects WOW64, 32-bit, or emulated PowerShell; rerun in native AMD64 or ARM64 PowerShell matching the OS."
  }
  switch ($hostMachine[1]) {
    0x8664 { return 'AMD64' }
    0xAA64 { return 'ARM64' }
    default { throw "Unsupported native Windows runtime architecture: $($hostMachine[1]); use AMD64 or ARM64 Windows and matching native PowerShell." }
  }
}

function Test-CrabboxWindowsRuntime([ValidateSet('AMD64', 'ARM64')][string]$Architecture) {
  # Each probe starts a fresh matching PowerShell: failed loads cannot poison the post-install probe.
  $interop = Get-CrabboxWindowsRuntimeInterop
  $probe = @"
`$ErrorActionPreference = 'Stop'
try {
  Add-Type -TypeDefinition @'
$interop
'@
  `$hostMachine = [CrabboxWindowsRuntimeNative]::Host()
  `$expected = if ('$Architecture' -eq 'AMD64') { 0x8664 } else { 0xAA64 }
  if (`$hostMachine[0] -ne 0 -or `$hostMachine[1] -ne `$expected -or `$hostMachine[2] -ne 8) {
    throw 'Runtime probe child must match the native OS architecture'
  }
} catch { [Console]::Error.WriteLine(`$_); exit 2 }
try {
  `$dlls = @('vcruntime140.dll', 'msvcp140.dll')
  # The separate EH continuation runtime is an x64 requirement, not an ARM64 DLL.
  if ('$Architecture' -eq 'AMD64') { `$dlls += 'vcruntime140_1.dll' }
  foreach (`$dll in `$dlls) { [CrabboxWindowsRuntimeNative]::Load(`$dll) }
} catch { [Console]::Error.WriteLine(`$_); exit 1 }
exit 0
"@
  $start = New-Object Diagnostics.ProcessStartInfo
  $start.FileName = (Get-Process -Id $PID).Path
  $start.Arguments = '-NoLogo -NoProfile -NonInteractive -EncodedCommand ' + [Convert]::ToBase64String([Text.Encoding]::Unicode.GetBytes($probe))
  $start.UseShellExecute = $false
  $start.CreateNoWindow = $true
  $start.RedirectStandardError = $true
  $start.RedirectStandardOutput = $true
  $process = New-Object Diagnostics.Process
  $process.StartInfo = $start
  try {
    if (-not $process.Start()) { throw 'Windows runtime probe did not start' }
    if (-not $process.WaitForExit(60000)) {
      $process.Kill()
      throw 'Windows runtime probe timed out'
    }
    $detail = $process.StandardError.ReadToEnd() + $process.StandardOutput.ReadToEnd()
    if ($process.ExitCode -eq 0) { return $true }
    if ($process.ExitCode -ne 1) { throw "Windows runtime probe failed (exit $($process.ExitCode)): $detail" }
    Write-Host "Native $Architecture VC++ runtime is missing or unloadable: $detail"
    return $false
  } finally { $process.Dispose() }
}

function Assert-CrabboxWindowsRuntimeAdministrator {
  $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
  $principal = New-Object Security.Principal.WindowsPrincipal($identity)
  if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw 'Missing Windows VC++ runtime requires elevated managed bootstrap; rerun as Administrator. BYO hosts must be prepared by their operator.'
  }
}

function New-CrabboxWindowsRuntimeStage {
  $path = Join-Path ([Environment]::GetFolderPath('Windows')) ('Temp\crabbox-vc-runtime-' + [Guid]::NewGuid().ToString('N'))
  if (Test-Path -LiteralPath $path) { throw 'Windows runtime staging path already exists' }
  $acl = New-Object Security.AccessControl.DirectorySecurity
  $acl.SetAccessRuleProtection($true, $false)
  $administrators = New-Object Security.Principal.SecurityIdentifier('S-1-5-32-544')
  $acl.SetOwner($administrators)
  foreach ($sid in @('S-1-5-32-544', 'S-1-5-18')) {
    $identity = New-Object Security.Principal.SecurityIdentifier($sid)
    $rule = New-Object Security.AccessControl.FileSystemAccessRule($identity, 'FullControl', 'ContainerInherit,ObjectInherit', 'None', 'Allow')
    $acl.AddAccessRule($rule)
  }
  $directory = New-Object IO.DirectoryInfo($path)
  # Create with the private ACL, including on PowerShell 7's .NET runtime.
  if ($PSVersionTable.PSEdition -eq 'Desktop') { $directory.Create($acl) }
  else { [IO.FileSystemAclExtensions]::Create($directory, $acl) }
  try {
    $actual = Get-Acl -LiteralPath $path
    $rules = @($actual.GetAccessRules($true, $true, [Security.Principal.SecurityIdentifier]))
    if (-not $actual.AreAccessRulesProtected -or $actual.GetOwner([Security.Principal.SecurityIdentifier]).Value -ne 'S-1-5-32-544' -or
        $rules.Count -ne 2 -or ((Get-Item -LiteralPath $path).Attributes -band [IO.FileAttributes]::ReparsePoint)) {
      throw 'Windows runtime staging ACL is not private'
    }
    foreach ($sid in @('S-1-5-32-544', 'S-1-5-18')) {
      $matches = @($rules | Where-Object { $_.IdentityReference.Value -eq $sid -and $_.AccessControlType -eq 'Allow' -and
        $_.FileSystemRights -eq 'FullControl' -and $_.InheritanceFlags -eq 'ContainerInherit,ObjectInherit' -and
        $_.PropagationFlags -eq 'None' -and -not $_.IsInherited })
      if ($matches.Count -ne 1) { throw 'Windows runtime staging ACL is not private' }
    }
    return $path
  } catch {
    Remove-Item -LiteralPath $path -Recurse -Force
    throw
  }
}

function Assert-CrabboxWindowsRuntimeArtifact([string]$Path, [string]$ExpectedSHA256) {
  if ((Get-FileHash -LiteralPath $Path -Algorithm SHA256 -ErrorAction Stop).Hash -ine $ExpectedSHA256) {
    throw 'Windows VC++ runtime installer SHA-256 mismatch'
  }
  $signature = Get-AuthenticodeSignature -LiteralPath $Path -ErrorAction Stop
  if ($signature.Status -ne 'Valid' -or $null -eq $signature.SignerCertificate) {
    throw 'Windows VC++ runtime installer requires Valid Authenticode and a Microsoft signer certificate'
  }
  $expected = @{ CN = 'Microsoft Corporation'; O = 'Microsoft Corporation'; L = 'Redmond'; S = 'Washington'; C = 'US' }
  $subject = @{}
  foreach ($line in ($signature.SignerCertificate.SubjectName.Decode([Security.Cryptography.X509Certificates.X500DistinguishedNameFlags]::UseNewLines) -split '\r?\n')) {
    if (-not $line.Trim()) { continue }
    $parts = $line -split '=', 2
    if ($parts.Count -ne 2) { throw 'Unexpected Windows VC++ runtime signer identity' }
    $key = $parts[0].Trim().ToUpperInvariant()
    if ($key -eq 'ST') { $key = 'S' }
    $value = $parts[1].Trim()
    if ($value.StartsWith('"') -and $value.EndsWith('"')) { $value = $value.Substring(1, $value.Length - 2) }
    if (-not $expected.ContainsKey($key) -or $subject.ContainsKey($key) -or
        -not [string]::Equals($value, $expected[$key], [StringComparison]::OrdinalIgnoreCase)) {
      throw 'Unexpected Windows VC++ runtime signer identity; exact Microsoft Corporation publisher required'
    }
    $subject[$key] = $value
  }
  if ($subject.Count -ne $expected.Count) { throw 'Incomplete Windows VC++ runtime Microsoft signer identity' }
}

function Get-CrabboxWindowsRuntimeBoot {
  return (Get-CimInstance -ClassName Win32_OperatingSystem -ErrorAction Stop).LastBootUpTime.ToUniversalTime().Ticks.ToString()
}

function Get-CrabboxWindowsRuntimePendingBoot {
  $path = 'HKLM:\SOFTWARE\Crabbox\Bootstrap\WindowsRuntime'
  if (Test-Path -LiteralPath $path) {
    $state = Get-ItemProperty -LiteralPath $path -ErrorAction Stop
    if ($state.PSObject.Properties['PendingBoot']) { return $state.PendingBoot }
  }
}

function Set-CrabboxWindowsRuntimePendingBoot([string]$Boot) {
  $path = 'HKLM:\SOFTWARE\Crabbox\Bootstrap\WindowsRuntime'
  if ($Boot) {
    New-Item -Path $path -Force -ErrorAction Stop | Out-Null
    New-ItemProperty -LiteralPath $path -Name PendingBoot -Value $Boot -PropertyType String -Force -ErrorAction Stop | Out-Null
  } else {
    Remove-ItemProperty -LiteralPath $path -Name PendingBoot -ErrorAction Stop
  }
}

function Ensure-CrabboxWindowsRuntime {
  $ErrorActionPreference = 'Stop'
  $architecture = Get-CrabboxWindowsRuntimeArchitecture
  $pendingBoot = Get-CrabboxWindowsRuntimePendingBoot
  if ($pendingBoot) {
    if ($pendingBoot -notmatch '^\d+$') {
      throw 'Invalid Windows VC++ runtime pending-boot state; inspect the interrupted installation and repair HKLM:\SOFTWARE\Crabbox\Bootstrap\WindowsRuntime PendingBoot before retry. Bootstrap is not ready.'
    }
    if ($pendingBoot -eq (Get-CrabboxWindowsRuntimeBoot)) {
      throw 'Windows VC++ runtime reboot required after a pending or interrupted installation; reboot outside bootstrap, then retry. Bootstrap is not ready.'
    }
    Set-CrabboxWindowsRuntimePendingBoot ''
  }
  if (Test-CrabboxWindowsRuntime $architecture) { return }
  Assert-CrabboxWindowsRuntimeAdministrator
  if ($architecture -eq 'AMD64') {
    $url = {{ps:windowsVCRuntimeX64URL}}
    $sha256 = {{ps:windowsVCRuntimeX64SHA256}}
  } else {
    $url = {{ps:windowsVCRuntimeARM64URL}}
    $sha256 = {{ps:windowsVCRuntimeARM64SHA256}}
  }
  $stage = New-CrabboxWindowsRuntimeStage
  try {
    $installer = Join-Path $stage 'vc_redist.exe'
    [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
    for ($attempt = 1; $attempt -le 3; $attempt++) {
      try {
        Invoke-WebRequest -Uri $url -OutFile $installer -UseBasicParsing -TimeoutSec 120 -ErrorAction Stop
        break
      } catch {
        if ($attempt -eq 3) { throw }
        Start-Sleep -Seconds ($attempt * 5)
      }
    }
    Assert-CrabboxWindowsRuntimeArtifact $installer $sha256
    # Persist before execution so an interrupted install cannot turn a retry into false readiness.
    Set-CrabboxWindowsRuntimePendingBoot (Get-CrabboxWindowsRuntimeBoot)
    $install = Start-Process -FilePath $installer -ArgumentList '/install','/quiet','/norestart' -Wait -PassThru
    switch ($install.ExitCode) {
      0 { }
      3010 { throw 'Windows VC++ runtime installer exit 3010: reboot required; reboot outside bootstrap, then retry. Bootstrap is not ready.' }
      1641 { throw 'Windows VC++ runtime installer exit 1641: unexpected restart initiated despite /norestart; bootstrap failed. Retry only after reboot.' }
      default { throw "Windows VC++ runtime installer failed with exit $($install.ExitCode); inspect installer failure, reboot, then retry bootstrap." }
    }
    if (-not (Test-CrabboxWindowsRuntime $architecture)) {
      throw "Windows VC++ runtime post-install load check failed for $architecture; bootstrap is not ready. Inspect the runtime, reboot, then retry."
    }
    Set-CrabboxWindowsRuntimePendingBoot ''
  } finally {
    Remove-Item -LiteralPath $stage -Recurse -Force
  }
}
