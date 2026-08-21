$ErrorActionPreference = "Stop"

function Invoke-NativeChecked {
  param(
    [string]$Command,
    [string[]]$Arguments = @()
  )
  & $Command @Arguments
  if ($LASTEXITCODE -ne 0) {
    throw "$Command failed with exit code $LASTEXITCODE"
  }
}

Write-Output "devtools-smoke-ok"
$Computer = Get-ComputerInfo
$InstallationType = (Get-ItemProperty -LiteralPath "HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion").InstallationType
if (
  $Computer.OsName -notmatch "Windows Server 2022" -or
  $Computer.OsBuildNumber -ne "20348" -or
  $InstallationType -ne "Server"
) {
  throw "Windows Server 2022 Desktop Experience is required, found $($Computer.OsName) build $($Computer.OsBuildNumber) installation type $InstallationType"
}
$Computer | Select-Object OsName, OsVersion, OsBuildNumber | Format-List
Invoke-NativeChecked "git" @("--version")
$GhVersion = Invoke-NativeChecked "gh" @("--version")
$GhVersion | Select-Object -First 1
Invoke-NativeChecked "jq" @("--version")
$RgVersion = Invoke-NativeChecked "rg" @("--version")
$RgVersion | Select-Object -First 1
Invoke-NativeChecked "fd" @("--version")
Invoke-NativeChecked "python" @("--version")
Invoke-NativeChecked "node" @("--version")
$nodeMajor = [int](Invoke-NativeChecked "node" @("-p", "process.versions.node.split('.')[0]"))
if ($nodeMajor -lt 24) {
  throw "Node.js 24 or newer is required, found major $nodeMajor"
}
Invoke-NativeChecked "npm" @("--version")
Invoke-NativeChecked "corepack" @("--version")
Invoke-NativeChecked "pnpm" @("--version")
Invoke-NativeChecked "trufflehog" @("--no-update", "--version")
Invoke-NativeChecked "docker" @("--version")
Invoke-NativeChecked "docker" @("version")
Invoke-NativeChecked "docker" @(
  "image",
  "inspect",
  "mcr.microsoft.com/windows/servercore:ltsc2022"
) | Out-Null
