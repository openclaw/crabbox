# Install Crabbox on Windows

The supported Windows client setup uses the native `crabbox.exe` from the
latest GitHub Release, native Windows OpenSSH for Crabbox control commands,
and one of two matched transfer toolchains:

1. A default WSL2 distribution with native Linux `rsync` and OpenSSH. This is
   the preferred path.
2. On x64 only, no WSL: MSYS2 `rsync.exe` and the sibling MSYS2 `ssh.exe`
   from the exact same directory.

Native Windows Git, the Windows OpenSSH Client capability (`ssh.exe` and
`ssh-keygen.exe`), and `curl.exe` are required for both paths.

The native Crabbox release installer below supports both amd64 and arm64
archives. The no-WSL MSYS2 transport has been live-proven and is supported on
x64 Windows only; use the preferred WSL2 path on Windows ARM64.

Crabbox invokes `wsl.exe` without naming a distribution, so it probes the
default WSL distribution. This guide configures Ubuntu as that default, but
Ubuntu is not a code requirement: another distribution works when it is the
default and provides native Linux `rsync` and `ssh` commands.

The transport selection documented here requires current `main` or Crabbox
v0.42.1 and newer. Crabbox v0.42.0 and older can silently select an
incompatible Windows SSH pairing.

Crabbox never pairs MSYS2 or Cygwin rsync with the native System32 OpenSSH
client. That combination is unsupported: rsync can exit while its SSH child
remains connected. Crabbox prefers native WSL tools when both are available;
without WSL, it resolves `rsync.exe` from `PATH` and fails closed unless a
regular `ssh.exe` exists beside it. Direct control commands separately use
`%WINDIR%\System32\OpenSSH\ssh.exe` when present, ignoring an MSYS2 SSH that
appears earlier on `PATH`.

## Install the native Crabbox executable

Run this block in ordinary, non-elevated PowerShell. It detects amd64 versus
arm64, queries the latest stable
[Crabbox GitHub Release](https://github.com/openclaw/crabbox/releases),
downloads the matching archive and `checksums.txt`, verifies SHA-256 before
extraction, and adds the install directory to your user `PATH`.

```powershell
$ErrorActionPreference = "Stop"
$minimumVersion = [version]"0.42.1"

$osArchitecture = [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString().ToLowerInvariant()
$architecture = switch ($osArchitecture) {
    "x64"   { "amd64" }
    "arm64" { "arm64" }
    default { throw "Unsupported Windows architecture: $osArchitecture" }
}

$headers = @{
    Accept = "application/vnd.github+json"
    "User-Agent" = "Crabbox-Windows-Installer"
}
$release = Invoke-RestMethod `
    -Uri "https://api.github.com/repos/openclaw/crabbox/releases/latest" `
    -Headers $headers
if ($release.draft -or $release.prerelease) {
    throw "The GitHub latest-release endpoint returned a non-stable release."
}

$version = $release.tag_name -replace '^v', ''
$stableVersion = [version]$version
if ($stableVersion -lt $minimumVersion) {
    throw "Latest stable Crabbox release v$version is older than required v$minimumVersion. Use a build from current main or wait for Crabbox v$minimumVersion or newer."
}

$archiveName = "crabbox_${version}_windows_${architecture}.zip"
$archiveAsset = $release.assets | Where-Object { $_.name -eq $archiveName } | Select-Object -First 1
$checksumsAsset = $release.assets | Where-Object { $_.name -eq "checksums.txt" } | Select-Object -First 1
if ($null -eq $archiveAsset -or $null -eq $checksumsAsset) {
    throw "Release assets are missing $archiveName or checksums.txt."
}

$downloadDir = Join-Path ([IO.Path]::GetTempPath()) ("crabbox-install-" + [guid]::NewGuid())
$archivePath = Join-Path $downloadDir $archiveName
$checksumsPath = Join-Path $downloadDir "checksums.txt"
$installDir = Join-Path $env:LOCALAPPDATA "Programs\Crabbox"
New-Item -ItemType Directory -Path $downloadDir -Force | Out-Null

try {
    Invoke-WebRequest -Uri $archiveAsset.browser_download_url -OutFile $archivePath
    Invoke-WebRequest -Uri $checksumsAsset.browser_download_url -OutFile $checksumsPath

    $checksumPattern = '\s+\*?' + [regex]::Escape($archiveName) + '$'
    $checksumLine = Get-Content $checksumsPath |
        Where-Object { $_ -match $checksumPattern } |
        Select-Object -First 1
    if (-not $checksumLine) {
        throw "No checksum found for $archiveName."
    }
    $expectedHash = ($checksumLine.Trim() -split '\s+')[0]
    $actualHash = (Get-FileHash -Algorithm SHA256 -Path $archivePath).Hash
    if ($actualHash -ine $expectedHash) {
        throw "SHA-256 mismatch for $archiveName."
    }

    New-Item -ItemType Directory -Path $installDir -Force | Out-Null
    Expand-Archive -Path $archivePath -DestinationPath $installDir -Force
} finally {
    Remove-Item -LiteralPath $downloadDir -Recurse -Force -ErrorAction SilentlyContinue
}

$userPath = [Environment]::GetEnvironmentVariable("Path", "User")
$userPathParts = @($userPath -split ';' | Where-Object { $_ })
if ($userPathParts -notcontains $installDir) {
    $updatedUserPath = (@($userPathParts) + $installDir) -join ';'
    [Environment]::SetEnvironmentVariable("Path", $updatedUserPath, "User")
}
if (($env:Path -split ';') -notcontains $installDir) {
    $env:Path = "$installDir;$env:Path"
}
```

## Install Windows prerequisites

Git for Windows can be installed from an ordinary PowerShell session:

```powershell
winget install --id Git.Git --exact --source winget
```

Windows 11 includes `curl.exe`. Install the Windows OpenSSH Client capability
from an **elevated PowerShell** session; this also supplies `ssh-keygen.exe`:

```powershell
$openSSH = Get-WindowsCapability -Online -Name "OpenSSH.Client*"
if ($openSSH.State -ne "Installed") {
    Add-WindowsCapability -Online -Name "OpenSSH.Client~~~~0.0.1.0"
}
```

Close and reopen PowerShell after installation so the updated application and
user paths are visible.

## Install WSL2 transport tools

Install WSL2 with Ubuntu from an **elevated PowerShell** session. Windows may
require a restart before Ubuntu completes its first-launch setup.

```powershell
wsl.exe --install --distribution Ubuntu
```

After Ubuntu has initialized, make it the default distribution from an
ordinary, non-elevated PowerShell session. This must match Crabbox's
unqualified `wsl.exe` probe.

```powershell
wsl.exe --set-default Ubuntu
```

Then install Ubuntu's native Linux transfer tools from PowerShell. Ubuntu may
prompt for the Linux user's password for `sudo`.

```powershell
wsl.exe --distribution Ubuntu -- bash -lc 'sudo apt-get update && sudo apt-get install -y rsync openssh-client'
```

If you prefer another WSL distribution, set that distribution as the default
instead and install its native `rsync` and OpenSSH client packages there.

## Install no-WSL transfer tools with MSYS2

Skip this section when using the WSL2 path above. In an **elevated PowerShell**
session on x64 Windows, download the official maintained MSYS2 self-extracting
base archive, verify the exact digest exercised by the live proof, extract it
to `C:\msys64`, and install both transfer packages. This path intentionally
does not depend on Winget, which was unavailable on the clean AWS Windows image
used for the live proof.

```powershell
$ErrorActionPreference = "Stop"
$msysRoot = "C:\msys64"
$msysBin = Join-Path $msysRoot "usr\bin"
$msysBash = Join-Path $msysBin "bash.exe"
$msysURL = "https://github.com/msys2/msys2-installer/releases/download/2026-06-11/msys2-base-x86_64-20260611.sfx.exe"
$expectedMSYSHash = "c105946e64e08f099ac0e4647461ce762b95333ad211777666476a9a41451d65"
$downloadDir = Join-Path ([IO.Path]::GetTempPath()) ("crabbox-msys2-" + [guid]::NewGuid())
$msysSFX = Join-Path $downloadDir "msys2-base-x86_64-20260611.sfx.exe"

New-Item -ItemType Directory -Path $downloadDir -Force | Out-Null
try {
    Invoke-WebRequest -Uri $msysURL -OutFile $msysSFX
    $actualMSYSHash = (Get-FileHash -Algorithm SHA256 -Path $msysSFX).Hash
    if ($actualMSYSHash -ine $expectedMSYSHash) {
        throw "MSYS2 SHA-256 mismatch; do not bypass checksum verification."
    }

    $extract = Start-Process -FilePath $msysSFX `
        -ArgumentList "-y", "-oC:\" `
        -Wait -PassThru -NoNewWindow
    if ($extract.ExitCode -ne 0) { throw "MSYS2 extraction failed." }
} finally {
    Remove-Item -LiteralPath $downloadDir -Recurse -Force -ErrorAction SilentlyContinue
}

if (-not (Test-Path -LiteralPath $msysBash -PathType Leaf)) {
    throw "MSYS2 bash was not installed at $msysBash."
}

& $msysBash -lc 'pacman -Sy --needed --noconfirm rsync openssh'
if ($LASTEXITCODE -ne 0) { throw "MSYS2 rsync/OpenSSH installation failed." }

$requiredTransferTools = @(
    (Join-Path $msysBin "rsync.exe")
    (Join-Path $msysBin "ssh.exe")
)
foreach ($tool in $requiredTransferTools) {
    if (-not (Test-Path -LiteralPath $tool -PathType Leaf)) {
        throw "Required no-WSL transfer tool is missing: $tool"
    }
}

$userPath = [Environment]::GetEnvironmentVariable("Path", "User")
$userPathParts = @($userPath -split ';' | Where-Object { $_ })
if ($userPathParts -notcontains $msysBin) {
    [Environment]::SetEnvironmentVariable(
        "Path",
        (@($msysBin) + $userPathParts) -join ';',
        "User"
    )
}
if (($env:Path -split ';') -notcontains $msysBin) {
    $env:Path = "$msysBin;$env:Path"
}
```

The URL and digest above pin the immutable MSYS2 2026-06-11 base archive.
The digest verifies that archive, while `pacman` installs the current package
versions rather than pinning them.

This deliberately places `C:\msys64\usr\bin\rsync.exe` and its sibling
`ssh.exe` together on `PATH`. Crabbox binds native rsync's `-e` remote shell to
that exact sibling executable, including when the directory contains spaces.
It still uses System32 OpenSSH for direct control, probes, connect, and tunnel
commands. Do not remove the sibling MSYS2 `ssh.exe` or substitute System32
OpenSSH in rsync configuration.

The x64 live proof installed MSYS2 rsync 3.5.0 and OpenSSH 10.5p1, and that
tuple completed a real 64 MiB Windows-to-Linux sync. Those package versions
describe the proven run; the `pacman` command above does not pin future installs
to identical versions. The no-WSL tuple has not been live-proven on Windows
ARM64, so use WSL2 there.

## Verify the installation

Open a fresh, ordinary PowerShell session and verify the shared native tools:

```powershell
Get-Command crabbox.exe, git.exe, ssh-keygen.exe, curl.exe
Test-Path -LiteralPath "$env:WINDIR\System32\OpenSSH\ssh.exe" -PathType Leaf
crabbox --version
crabbox doctor
```

For WSL2, run Crabbox's unqualified default-distribution probe. Both results
must be Linux paths such as `/usr/bin/rsync` and `/usr/bin/ssh`, never `.exe`
files or paths below `/mnt/<drive>` or `/mnt/host/<drive>`:

```powershell
wsl.exe sh -c 'command -v rsync || exit 1; command -v ssh || exit 1'
```

For no-WSL MSYS2 on x64, verify that PATH-selected rsync and its required
sibling are the exact two files installed together. `Get-Command ssh.exe` may
report the MSYS2 binary; Crabbox still selects System32 OpenSSH for direct
control.

```powershell
$expectedRsync = "C:\msys64\usr\bin\rsync.exe"
$expectedSiblingSSH = "C:\msys64\usr\bin\ssh.exe"
$rsync = (Get-Command rsync.exe -CommandType Application).Source
$siblingSSH = Join-Path (Split-Path -Parent $rsync) "ssh.exe"
$rsync
$siblingSSH
if ($rsync -ine $expectedRsync -or $siblingSSH -ine $expectedSiblingSSH) {
    throw "PATH-selected rsync.exe and its sibling ssh.exe are not the verified C:\msys64\usr\bin pair."
}
foreach ($tool in @($expectedRsync, $expectedSiblingSSH)) {
    if (-not (Test-Path -LiteralPath $tool -PathType Leaf)) {
        throw "Required no-WSL transfer tool is missing: $tool"
    }
}
```

A provider-neutral `crabbox doctor` can report `no provider selected`. That is
expected until provider or broker configuration is added and is separate from
local installation readiness. Select and configure a provider before running a
remote lifecycle command.

## Optional Docker Desktop smoke

If Docker Desktop is already installed and running, this secretless local
container smoke creates and commits a fixture in a temporary repository, syncs
that repository through the selected Windows transfer transport, verifies the
fixture in the container, and removes both the container and local repository:

```powershell
docker info
if ($LASTEXITCODE -ne 0) {
    throw "Docker Desktop is not ready."
}

$smokeRepo = Join-Path ([IO.Path]::GetTempPath()) ("crabbox-windows-sync-" + [guid]::NewGuid())
New-Item -ItemType Directory -Path $smokeRepo | Out-Null

try {
    Set-Content -LiteralPath (Join-Path $smokeRepo "fixture.txt") -Value "crabbox-windows-sync-ok" -NoNewline
    Push-Location $smokeRepo
    try {
        git init
        if ($LASTEXITCODE -ne 0) { throw "git init failed." }

        git add fixture.txt
        if ($LASTEXITCODE -ne 0) { throw "git add failed." }

        git -c 'user.name=Crabbox Sync Test' -c 'user.email=crabbox-sync-test@example.invalid' commit -m 'test: add sync fixture'
        if ($LASTEXITCODE -ne 0) { throw "git commit failed." }

        crabbox run --provider local-container --local-container-runtime docker --stop-after always -- sh -lc 'test "$(cat fixture.txt)" = "crabbox-windows-sync-ok"'
        if ($LASTEXITCODE -ne 0) { throw "Crabbox repository sync smoke failed." }
    } finally {
        Pop-Location
    }
} finally {
    Remove-Item -LiteralPath $smokeRepo -Recurse -Force -ErrorAction SilentlyContinue
}
```

See [Getting Started](getting-started.md) when you are ready to configure a
provider or broker and run a project workload.
