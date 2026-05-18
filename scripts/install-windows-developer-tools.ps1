$ErrorActionPreference = "Stop"
$ProgressPreference = "SilentlyContinue"
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

$NodeVersion = $env:CRABBOX_WINDOWS_NODE_VERSION
if (-not $NodeVersion) { $NodeVersion = "24.11.1" }
$PnpmVersion = $env:CRABBOX_WINDOWS_PNPM_VERSION
if (-not $PnpmVersion) { $PnpmVersion = "11.1.0" }
$DockerImages = $env:CRABBOX_WINDOWS_DOCKER_IMAGES
if (-not $DockerImages) { $DockerImages = "mcr.microsoft.com/windows/servercore:ltsc2022" }
$InstallDocker = $env:CRABBOX_WINDOWS_INSTALL_DOCKER
if (-not $InstallDocker) { $InstallDocker = "1" }
$RebootMarker = "C:\ProgramData\crabbox\image-prep-reboot-required"

function Write-Log {
  param([string]$Message)
  Write-Host "windows-tools: $Message"
}

function Retry {
  param([scriptblock]$ScriptBlock)
  for ($i = 1; $i -le 8; $i++) {
    try {
      & $ScriptBlock
      return
    } catch {
      if ($i -eq 8) { throw }
      Start-Sleep -Seconds ($i * 5)
    }
  }
}

function Add-MachinePath {
  param([string]$Path)
  $machinePath = [Environment]::GetEnvironmentVariable("Path", "Machine")
  if ($machinePath -notlike "*$Path*") {
    [Environment]::SetEnvironmentVariable("Path", "$machinePath;$Path", "Machine")
  }
  if ($env:Path -notlike "*$Path*") {
    $env:Path = "$env:Path;$Path"
  }
}

function Install-Chocolatey {
  if (Get-Command choco.exe -ErrorAction SilentlyContinue) { return }
  Write-Log "installing Chocolatey"
  Set-ExecutionPolicy Bypass -Scope Process -Force
  Invoke-Expression ((New-Object Net.WebClient).DownloadString("https://community.chocolatey.org/install.ps1"))
  Add-MachinePath "C:\ProgramData\chocolatey\bin"
}

function Install-ChocoPackage {
  param([string[]]$Packages)
  Retry { choco install -y --no-progress @Packages }
}

function Install-Node {
  if (Get-Command node.exe -ErrorAction SilentlyContinue) {
    $current = (& node --version).TrimStart("v")
    if ($current -eq $NodeVersion) { return }
  }
  $arch = "x64"
  $msi = Join-Path $env:TEMP "node-v$NodeVersion-$arch.msi"
  $url = "https://nodejs.org/dist/v$NodeVersion/node-v$NodeVersion-$arch.msi"
  Write-Log "installing Node $NodeVersion"
  Retry { Invoke-WebRequest -Uri $url -OutFile $msi -UseBasicParsing }
  Start-Process -FilePath "msiexec.exe" -ArgumentList "/i", $msi, "/qn", "/norestart" -Wait
  Add-MachinePath "C:\Program Files\nodejs"
}

function Enable-CorepackPnpm {
  Write-Log "activating pnpm $PnpmVersion"
  & corepack enable
  & corepack prepare "pnpm@$PnpmVersion" --activate
}

function Install-DockerEngine {
  if ($InstallDocker -ne "1") { return }
  Write-Log "installing Windows container support and Docker Engine"
  $restartRequired = $false
  $feature = Get-WindowsFeature -Name Containers -ErrorAction SilentlyContinue
  if ($feature -and -not $feature.Installed) {
    $result = Install-WindowsFeature -Name Containers
    $result | Out-Host
    if ($result.RestartNeeded -and $result.RestartNeeded -ne "No") {
      $restartRequired = $true
    }
  }
  if ($restartRequired) {
    New-Item -ItemType Directory -Force -Path (Split-Path $RebootMarker) | Out-Null
    Set-Content -Path $RebootMarker -Value "Containers feature requires reboot before Docker Engine installation"
    Write-Log "Docker Engine will be installed after reboot"
    return
  }
  if (-not (Get-PackageProvider -Name NuGet -ErrorAction SilentlyContinue)) {
    Install-PackageProvider -Name NuGet -MinimumVersion 2.8.5.201 -Force | Out-Null
  }
  if (-not (Get-Module -ListAvailable -Name DockerMsftProvider)) {
    Install-Module -Name DockerMsftProvider -Repository PSGallery -Force | Out-Null
  }
  if (-not (Get-Service -Name docker -ErrorAction SilentlyContinue)) {
    Install-Package -Name docker -ProviderName DockerMsftProvider -Force | Out-Null
  }
  Set-Service -Name docker -StartupType Automatic
  try {
    Start-Service docker
    & docker version
  } catch {
    if (-not $restartRequired) { throw }
    New-Item -ItemType Directory -Force -Path (Split-Path $RebootMarker) | Out-Null
    Set-Content -Path $RebootMarker -Value "Containers feature requires reboot before Docker service verification"
    Write-Log "Docker service will be verified after reboot"
    return
  }
  if (Test-Path $RebootMarker) {
    Remove-Item -Force $RebootMarker
  }
  foreach ($image in ($DockerImages -split "\s+")) {
    if (-not $image) { continue }
    try {
      Retry { & docker pull $image }
    } catch {
      Write-Log "docker pull failed for $image; continuing"
    }
  }
}

function Prepare-Caches {
  $dirs = @(
    "C:\ProgramData\crabbox\cache",
    "C:\ProgramData\crabbox\cache\npm",
    "C:\ProgramData\crabbox\cache\pnpm",
    "C:\ProgramData\crabbox\cache\corepack",
    "C:\ProgramData\crabbox\cache\docker"
  )
  foreach ($dir in $dirs) {
    New-Item -ItemType Directory -Force -Path $dir | Out-Null
  }
  [Environment]::SetEnvironmentVariable("npm_config_cache", "C:\ProgramData\crabbox\cache\npm", "Machine")
  [Environment]::SetEnvironmentVariable("PNPM_HOME", "C:\ProgramData\crabbox\pnpm", "Machine")
  Add-MachinePath "C:\ProgramData\crabbox\pnpm"
}

function Disable-FirstBootNoise {
  Set-ItemProperty -Path "HKLM:\SOFTWARE\Microsoft\ServerManager" -Name DoNotOpenServerManagerAtLogon -Type DWord -Value 1 -ErrorAction SilentlyContinue
  New-Item -Path "HKLM:\SYSTEM\CurrentControlSet\Control\Network\NewNetworkWindowOff" -Force | Out-Null
}

Install-Chocolatey
Install-ChocoPackage @(
  "git",
  "git-lfs",
  "gh",
  "jq",
  "yq",
  "ripgrep",
  "fd",
  "fzf",
  "7zip",
  "python313",
  "curl",
  "wget",
  "openssh",
  "vcredist-all"
)
Install-Node
Enable-CorepackPnpm
Install-DockerEngine
Prepare-Caches
Disable-FirstBootNoise

Write-Log "versions"
Get-ComputerInfo | Select-Object OsName, OsVersion, OsBuildNumber | Format-List
git --version
gh --version | Select-Object -First 1
jq --version
rg --version | Select-Object -First 1
fd --version
python --version
node --version
npm --version
corepack --version
pnpm --version
if (Get-Command docker.exe -ErrorAction SilentlyContinue) {
  docker --version
}

Write-Log "complete"
