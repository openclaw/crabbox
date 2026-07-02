$ErrorActionPreference = "Stop"
$ProgressPreference = "SilentlyContinue"
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

$DefaultNodeVersion = "24.11.1"
$DefaultNodeSHA256 = "208ba5ca1dab0b330f457909e0797de340c40b34ddf2edf40d26f382f733297e"
$NodeVersion = $env:CRABBOX_WINDOWS_NODE_VERSION
if (-not $NodeVersion) { $NodeVersion = $DefaultNodeVersion }
$NodeSHA256 = $env:CRABBOX_WINDOWS_NODE_SHA256
if (-not $NodeSHA256) {
  if ($NodeVersion -ne $DefaultNodeVersion) {
    throw "CRABBOX_WINDOWS_NODE_SHA256 is required when CRABBOX_WINDOWS_NODE_VERSION overrides $DefaultNodeVersion"
  }
  $NodeSHA256 = $DefaultNodeSHA256
}
if ($NodeSHA256 -notmatch "^[0-9a-fA-F]{64}$") {
  throw "CRABBOX_WINDOWS_NODE_SHA256 must be a 64-character hex digest"
}
$PnpmVersion = $env:CRABBOX_WINDOWS_PNPM_VERSION
if (-not $PnpmVersion) { $PnpmVersion = "11.1.0" }
$InstallDocker = $env:CRABBOX_WINDOWS_INSTALL_DOCKER
if (-not $InstallDocker) { $InstallDocker = "1" }
$DefaultDockerVersion = "29.5.1"
$DefaultDockerSHA256 = "7008d54da30461fa745d4539beb87d3d14dd38c7ab0110657720526e16f5f2d3"
$DockerVersion = $env:CRABBOX_WINDOWS_DOCKER_VERSION
if (-not $DockerVersion) { $DockerVersion = $DefaultDockerVersion }
$DockerSHA256 = $env:CRABBOX_WINDOWS_DOCKER_SHA256
if ($InstallDocker -eq "1") {
  if (-not $DockerSHA256) {
    if ($DockerVersion -ne $DefaultDockerVersion) {
      throw "CRABBOX_WINDOWS_DOCKER_SHA256 is required when CRABBOX_WINDOWS_DOCKER_VERSION overrides $DefaultDockerVersion"
    }
    $DockerSHA256 = $DefaultDockerSHA256
  }
  if ($DockerSHA256 -notmatch "^[0-9a-fA-F]{64}$") {
    throw "CRABBOX_WINDOWS_DOCKER_SHA256 must be a 64-character hex digest"
  }
}
$DockerImages = $env:CRABBOX_WINDOWS_DOCKER_IMAGES
if (-not $DockerImages) { $DockerImages = "mcr.microsoft.com/windows/servercore:ltsc2022" }
$RebootMarker = "C:\ProgramData\crabbox\image-prep-reboot-required"
$ChocolateyPackageURL = $env:CRABBOX_WINDOWS_CHOCO_PACKAGE_URL
if (-not $ChocolateyPackageURL) { $ChocolateyPackageURL = "https://community.chocolatey.org/api/v2/package/chocolatey/2.7.3" }
$ChocolateyPackageSHA256 = $env:CRABBOX_WINDOWS_CHOCO_PACKAGE_SHA256
if (-not $ChocolateyPackageSHA256) { $ChocolateyPackageSHA256 = "40778cc59245b3eb6ea5147aeef5bea5d577419e5abce22a224189740dc16db5" }

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

function Refresh-SessionPath {
  $machinePath = [Environment]::GetEnvironmentVariable("Path", "Machine")
  $userPath = [Environment]::GetEnvironmentVariable("Path", "User")
  $paths = @($machinePath, $userPath) | Where-Object { $_ }
  if ($paths.Count -gt 0) {
    $env:Path = ($paths -join ";")
  }
  Add-MachinePath "C:\ProgramData\chocolatey\bin"
  Add-MachinePath "C:\Program Files\Git\cmd"
  Add-MachinePath "C:\Python313"
  Add-MachinePath "C:\Python313\Scripts"
  Add-MachinePath "C:\Program Files\nodejs"
  Add-MachinePath "C:\ProgramData\crabbox\pnpm"
  Add-MachinePath "C:\Program Files\docker"
}

function Assert-FileSHA256 {
  param(
    [string]$Path,
    [string]$Expected,
    [string]$Name
  )
  if (-not $Expected -or $Expected -notmatch "^[0-9a-fA-F]{64}$") {
    throw "$Name SHA256 must be a 64-character hex digest"
  }
  $actual = (Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash.ToLowerInvariant()
  $want = $Expected.ToLowerInvariant()
  if ($actual -ne $want) {
    throw "$Name SHA256 mismatch: got $actual want $want"
  }
}

function Install-VerifiedChocolateyPackage {
  param(
    [string]$Url,
    [string]$SHA256
  )
  $workDir = Join-Path $env:TEMP ("crabbox-chocolatey-" + [Guid]::NewGuid().ToString("N"))
  $package = Join-Path $workDir "chocolatey.zip"
  $extractDir = Join-Path $workDir "package"
  try {
    New-Item -ItemType Directory -Force -Path $workDir | Out-Null
    Retry { Invoke-WebRequest -Uri $Url -OutFile $package -UseBasicParsing }
    Assert-FileSHA256 -Path $package -Expected $SHA256 -Name "Chocolatey package"
    Expand-Archive -LiteralPath $package -DestinationPath $extractDir -Force

    $installScript = Join-Path $extractDir "tools\chocolateyInstall.ps1"
    if (-not (Test-Path -LiteralPath $installScript -PathType Leaf)) {
      throw "Chocolatey package is missing tools\chocolateyInstall.ps1"
    }

    & $installScript

    $installRoot = [Environment]::GetEnvironmentVariable("ChocolateyInstall", "Machine")
    if (-not $installRoot) { $installRoot = "C:\ProgramData\chocolatey" }
    $chocoExe = Join-Path $installRoot "bin\choco.exe"
    if (-not (Test-Path -LiteralPath $chocoExe -PathType Leaf)) {
      throw "Chocolatey package installation did not create $chocoExe"
    }

    $packageDir = Join-Path $installRoot "lib\chocolatey"
    New-Item -ItemType Directory -Force -Path $packageDir | Out-Null
    Copy-Item -LiteralPath $package -Destination (Join-Path $packageDir "chocolatey.nupkg") -Force
  } finally {
    Remove-Item -Recurse -Force -LiteralPath $workDir -ErrorAction SilentlyContinue
  }
}

function Install-Chocolatey {
  if (Get-Command choco.exe -ErrorAction SilentlyContinue) { return }
  Write-Log "installing Chocolatey"
  Set-ExecutionPolicy Bypass -Scope Process -Force
  Install-VerifiedChocolateyPackage -Url $ChocolateyPackageURL -SHA256 $ChocolateyPackageSHA256
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
  $workDir = Join-Path $env:TEMP ("crabbox-node-" + [Guid]::NewGuid().ToString("N"))
  $msi = Join-Path $workDir "node-v$NodeVersion-$arch.msi"
  $url = "https://nodejs.org/dist/v$NodeVersion/node-v$NodeVersion-$arch.msi"
  Write-Log "installing Node $NodeVersion"
  try {
    New-Item -ItemType Directory -Force -Path $workDir | Out-Null
    Retry { Invoke-WebRequest -Uri $url -OutFile $msi -UseBasicParsing }
    Assert-FileSHA256 -Path $msi -Expected $NodeSHA256 -Name "Node MSI"
    Write-Log "verified Node $NodeVersion $arch MSI SHA256 $($NodeSHA256.ToLowerInvariant())"
    Start-Process -FilePath "msiexec.exe" -ArgumentList "/i", $msi, "/qn", "/norestart" -Wait
  } finally {
    Remove-Item -Recurse -Force -LiteralPath $workDir -ErrorAction SilentlyContinue
  }
  Add-MachinePath "C:\Program Files\nodejs"
}

function Enable-CorepackPnpm {
  Write-Log "activating pnpm $PnpmVersion"
  & corepack enable
  & corepack prepare "pnpm@$PnpmVersion" --activate
}

function Install-StaticDockerEngine {
  $dockerBin = Join-Path $env:ProgramFiles "docker"
  $dockerd = Join-Path $dockerBin "dockerd.exe"
  $dockerExe = Join-Path $dockerBin "docker.exe"
  $dockerService = Get-Service -Name docker -ErrorAction SilentlyContinue
  # A service-less directory is not installation proof; refresh it from the verified archive before registration.
  if (-not (Test-Path $dockerd) -or -not (Test-Path $dockerExe) -or -not $dockerService) {
    $workDir = Join-Path $env:TEMP ("crabbox-docker-" + [Guid]::NewGuid().ToString("N"))
    $zip = Join-Path $workDir "docker-$DockerVersion.zip"
    $url = "https://download.docker.com/win/static/stable/x86_64/docker-$DockerVersion.zip"
    Write-Log "installing Docker Engine $DockerVersion"
    try {
      New-Item -ItemType Directory -Force -Path $workDir | Out-Null
      Retry { Invoke-WebRequest -Uri $url -OutFile $zip -UseBasicParsing }
      Assert-FileSHA256 -Path $zip -Expected $DockerSHA256 -Name "Docker Engine archive"
      Write-Log "verified Docker Engine $DockerVersion x86_64 archive SHA256 $($DockerSHA256.ToLowerInvariant())"
      Expand-Archive -LiteralPath $zip -DestinationPath $env:ProgramFiles -Force
    } finally {
      Remove-Item -Recurse -Force -LiteralPath $workDir -ErrorAction SilentlyContinue
    }
  }
  Add-MachinePath $dockerBin
  Refresh-SessionPath
  if (-not $dockerService) {
    & $dockerd --register-service
  }
}

function Install-DockerEngine {
  if ($InstallDocker -ne "1") { return }
  Write-Log "installing Windows container support and Docker Engine"
  $restartRequired = $false
  $feature = Get-WindowsFeature -Name Containers -ErrorAction SilentlyContinue
  if ($feature -and -not $feature.Installed) {
    New-Item -ItemType Directory -Force -Path (Split-Path $RebootMarker) | Out-Null
    Set-Content -Path $RebootMarker -Value "Containers feature installation may interrupt SSH and requires a reboot before Docker Engine installation"
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
  Install-StaticDockerEngine
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

function Reset-EC2LaunchForImage {
  $ec2Launch = @(
    "C:\Program Files\Amazon\EC2Launch\EC2Launch.exe",
    "C:\ProgramData\Amazon\EC2Launch\EC2Launch.exe"
  ) | Where-Object { Test-Path $_ } | Select-Object -First 1
  if ($ec2Launch) {
    Write-Log "resetting EC2Launch v2 state for AMI boot"
    & $ec2Launch reset --block
    if ($LASTEXITCODE -ne 0) {
      & $ec2Launch reset
    }
    if ($LASTEXITCODE -ne 0) {
      throw "EC2Launch reset failed with exit code $LASTEXITCODE"
    }
  } else {
    $initialize = "C:\ProgramData\Amazon\EC2-Windows\Launch\Scripts\InitializeInstance.ps1"
    if (Test-Path $initialize) {
      Write-Log "scheduling EC2Launch v1 initialize for AMI boot"
      & powershell -NoLogo -NoProfile -ExecutionPolicy Bypass -File $initialize -Schedule
    }
  }
  Remove-Item -Force "C:\ProgramData\crabbox\setup-complete" -ErrorAction SilentlyContinue
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
  "vcredist-all"
)
Refresh-SessionPath
Install-Node
Refresh-SessionPath
Enable-CorepackPnpm
Install-DockerEngine
Prepare-Caches
Disable-FirstBootNoise
Refresh-SessionPath

if (Test-Path $RebootMarker) {
  Write-Log "reboot required before final verification"
  exit 0
}

Reset-EC2LaunchForImage

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
