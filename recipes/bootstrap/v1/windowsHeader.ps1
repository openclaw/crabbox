
$ErrorActionPreference = "Stop"
$ProgressPreference = "SilentlyContinue"
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
function Retry($ScriptBlock) {
  for ($i = 1; $i -le 8; $i++) {
    try { & $ScriptBlock; return }
    catch {
      if ($i -eq 8) { throw }
      Start-Sleep -Seconds ($i * 5)
    }
  }
}
function Assert-CrabboxFileSHA256([string]$Path, [string]$Expected) {
  if (-not (Test-Path -LiteralPath $Path)) { throw "downloaded artifact is missing: $Path" }
  $actual = (Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash.ToLowerInvariant()
  if ($actual -ne $Expected.ToLowerInvariant()) {
    Remove-Item -Force -LiteralPath $Path -ErrorAction SilentlyContinue
    throw "SHA-256 mismatch for downloaded artifact: $Path"
  }
}
function New-CrabboxPassword {
  $bytes = New-Object byte[] 18
  $rng = [Security.Cryptography.RandomNumberGenerator]::Create()
  try { $rng.GetBytes($bytes) } finally { $rng.Dispose() }
  return "Cb1!" + [Convert]::ToBase64String($bytes).Substring(0, 18)
}
function Resolve-CrabboxOpenSSHCommand([string]$Name) {
  foreach ($root in @($openSSHInstallRoot, $openSSHSystemRoot)) {
    $candidate = Join-Path $root $Name
    if (Test-Path -LiteralPath $candidate) { return $candidate }
  }
  $command = Get-Command $Name -ErrorAction SilentlyContinue
  if ($command -and $command.Source) { return $command.Source }
  throw "OpenSSH command $Name was not found"
}
$user = {{user}}
$publicKey = {{publicKey}}
$workRoot = {{workRoot}}
$sshPorts = {{ports}}
$base = "C:\ProgramData\crabbox"
$setupCompletePath = Join-Path $base "setup-complete"
$openSSHZip = "$env:TEMP\OpenSSH-Win64.zip"
$openSSHInstallRoot = "C:\Program Files\OpenSSH"
$openSSHSystemRoot = Join-Path $env:WINDIR "System32\OpenSSH"
$gitInstaller = "$env:TEMP\Git-2.52.0-64-bit.exe"
New-Item -ItemType Directory -Force -Path $base, $workRoot | Out-Null
New-Item -Path "HKLM:\SYSTEM\CurrentControlSet\Control\Network\NewNetworkWindowOff" -Force | Out-Null
Set-ItemProperty -Path "HKLM:\SOFTWARE\Microsoft\ServerManager" -Name DoNotOpenServerManagerAtLogon -Type DWord -Value 1 -ErrorAction SilentlyContinue
