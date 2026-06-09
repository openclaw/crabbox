package cli

import (
	"fmt"
	"os"
	"strings"
)

const (
	tightVNCMSIURL        = "https://www.tightvnc.com/download/2.8.85/tightvnc-2.8.85-gpl-setup-64bit.msi"
	gitForWindowsSetupURL = "https://github.com/git-for-windows/git/releases/download/v2.52.0.windows.1/Git-2.52.0-64-bit.exe"
	openSSHWin64ZipURL    = "https://github.com/PowerShell/Win32-OpenSSH/releases/download/v9.8.3.0p2-Preview/OpenSSH-Win64.zip"
	ubuntuWSLRootFSURL    = "https://cloud-images.ubuntu.com/wsl/releases/24.04/current/ubuntu-noble-wsl-amd64-wsl.rootfs.tar.gz"
)

func awsUserData(cfg Config, publicKey string) string {
	switch cfg.TargetOS {
	case targetWindows:
		return windowsUserData(cfg, publicKey)
	case targetMacOS:
		return macOSUserData(cfg, publicKey)
	default:
		return cloudInit(cfg, publicKey)
	}
}

func cloudInit(cfg Config, publicKey string) string {
	portLines := ""
	for _, port := range sshPortCandidates(cfg.SSHPort, cfg.SSHFallbackPorts) {
		portLines += fmt.Sprintf("      Port %s\n", port)
	}
	readyChecks := cloudInitOptionalReadyChecks(cfg)
	writeFiles := cloudInitOptionalWriteFiles(cfg)
	bootstrap := cloudInitOptionalBootstrap(cfg)
	return fmt.Sprintf(`#cloud-config
package_update: false
package_upgrade: false
users:
  - name: %[1]s
    groups: sudo
    shell: /bin/bash
    sudo: ['ALL=(ALL) NOPASSWD:ALL']
    ssh_authorized_keys:
      - %[2]s
write_files:
  - path: /etc/ssh/sshd_config.d/99-crabbox-port.conf
    permissions: '0644'
    content: |
%[4]s
      PasswordAuthentication no
  - path: /usr/local/bin/crabbox-ready
    permissions: '0755'
    content: |
      #!/usr/bin/env bash
      set -euo pipefail
      git --version
      rsync --version >/dev/null
      curl --version >/dev/null
      jq --version >/dev/null
      test -f /var/lib/crabbox/bootstrapped
      test -w %[3]s
%[5]s
%[6]s
runcmd:
  - |
    bash -euxo pipefail <<'BOOT'
    export DEBIAN_FRONTEND=noninteractive
    cat >/etc/apt/apt.conf.d/80-crabbox-retries <<'APT'
    Acquire::Retries "8";
    Acquire::http::Timeout "30";
    Acquire::https::Timeout "30";
    APT
    retry() {
      n=1
      until "$@"; do
        if [ "$n" -ge 8 ]; then
          return 1
        fi
        sleep $((n * 5))
        n=$((n + 1))
      done
    }
    retry apt-get update
    retry apt-get install -y --no-install-recommends openssh-server ca-certificates curl git rsync jq
    mkdir -p %[3]s /var/cache/crabbox/pnpm /var/cache/crabbox/npm
    chown -R %[1]s:%[1]s %[3]s /var/cache/crabbox
    install -d /var/lib/crabbox
    systemctl enable ssh || true
    timeout 30s systemctl restart ssh || timeout 30s systemctl restart ssh.socket || true
%[7]s
    touch /var/lib/crabbox/bootstrapped
    crabbox-ready
    BOOT
`, cfg.SSHUser, publicKey, cfg.WorkRoot, portLines, readyChecks, writeFiles, bootstrap)
}

func CloudInitUserData(cfg Config, publicKey string) string {
	return cloudInit(cfg, publicKey)
}

func windowsUserData(cfg Config, publicKey string) string {
	_ = cfg
	_ = publicKey
	return `version: 1.1
tasks:
- task: enableOpenSsh
`
}

func windowsBootstrapHeaderPowerShell(cfg Config, publicKey, workRoot string) string {
	return `
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
function New-CrabboxPassword {
  $bytes = New-Object byte[] 18
  $rng = [Security.Cryptography.RandomNumberGenerator]::Create()
  try { $rng.GetBytes($bytes) } finally { $rng.Dispose() }
  return "Cb1!" + [Convert]::ToBase64String($bytes).Substring(0, 18)
}
$user = ` + psQuote(cfg.SSHUser) + `
$publicKey = ` + psQuote(publicKey) + `
$workRoot = ` + psQuote(workRoot) + `
$sshPorts = ` + windowsSSHPortsPowerShell(cfg) + `
$base = "C:\ProgramData\crabbox"
$setupCompletePath = Join-Path $base "setup-complete"
$openSSHZip = "$env:TEMP\OpenSSH-Win64.zip"
$gitInstaller = "$env:TEMP\Git-2.52.0-64-bit.exe"
New-Item -ItemType Directory -Force -Path $base, $workRoot | Out-Null
New-Item -Path "HKLM:\SYSTEM\CurrentControlSet\Control\Network\NewNetworkWindowOff" -Force | Out-Null
Set-ItemProperty -Path "HKLM:\SOFTWARE\Microsoft\ServerManager" -Name DoNotOpenServerManagerAtLogon -Type DWord -Value 1 -ErrorAction SilentlyContinue
`
}

func windowsBootstrapCorePowerShell() string {
	return `
if (-not (Test-Path -LiteralPath $passwordPath)) {
  New-CrabboxPassword | Set-Content -NoNewline -Encoding ASCII -Path $passwordPath
}
$userPassword = (Get-Content -Raw -Path $passwordPath).Trim()
if ($userPassword.Length -lt 12 -or $userPassword -notmatch '[A-Z]' -or $userPassword -notmatch '[a-z]' -or $userPassword -notmatch '[0-9]' -or $userPassword -notmatch '[^A-Za-z0-9]') {
  $userPassword = New-CrabboxPassword
  Set-Content -NoNewline -Encoding ASCII -Path $passwordPath -Value $userPassword
}
$secure = ConvertTo-SecureString $userPassword -AsPlainText -Force
if (-not (Get-LocalUser -Name $user -ErrorAction SilentlyContinue)) {
  New-LocalUser -Name $user -Password $secure -PasswordNeverExpires -AccountNeverExpires | Out-Null
} else {
  Set-LocalUser -Name $user -Password $secure -PasswordNeverExpires $true
}
Add-LocalGroupMember -Group "Administrators" -Member $user -ErrorAction SilentlyContinue
Set-Content -NoNewline -Encoding ASCII -Path $usernamePath -Value $user
if ($passwordMirrorPath) {
  Set-Content -NoNewline -Encoding ASCII -Path $passwordMirrorPath -Value $userPassword
}
$userSID = (Get-LocalUser -Name $user).SID.Value
icacls.exe $workRoot /grant "*${userSID}:(OI)(CI)F" | Out-Null
$userSSHDir = Join-Path (Join-Path "C:\Users" $user) ".ssh"
$userAuthorizedKeys = Join-Path $userSSHDir "authorized_keys"
New-Item -ItemType Directory -Force -Path $userSSHDir | Out-Null
Set-Content -Encoding ASCII -Path $userAuthorizedKeys -Value $publicKey
icacls.exe $userSSHDir /inheritance:r /grant "*${userSID}:F" /grant "*S-1-5-32-544:F" /grant "*S-1-5-18:F" | Out-Null
icacls.exe $userAuthorizedKeys /inheritance:r /grant "*${userSID}:F" /grant "*S-1-5-32-544:F" /grant "*S-1-5-18:F" | Out-Null
if (-not (Get-Service -Name sshd -ErrorAction SilentlyContinue)) {
  Retry { Invoke-WebRequest -Uri ` + psQuote(openSSHWin64ZipURL) + ` -OutFile $openSSHZip -UseBasicParsing }
  Remove-Item -Recurse -Force "C:\Program Files\OpenSSH" -ErrorAction SilentlyContinue
  Expand-Archive -LiteralPath $openSSHZip -DestinationPath "C:\Program Files" -Force
  if (Test-Path -LiteralPath "C:\Program Files\OpenSSH-Win64") {
    Rename-Item -LiteralPath "C:\Program Files\OpenSSH-Win64" -NewName "OpenSSH" -Force
  }
  & "C:\Program Files\OpenSSH\install-sshd.ps1"
}
New-Item -ItemType Directory -Force -Path "$env:ProgramData\ssh" | Out-Null
Set-Content -Encoding ASCII -Path "$env:ProgramData\ssh\administrators_authorized_keys" -Value $publicKey
icacls.exe "$env:ProgramData\ssh\administrators_authorized_keys" /inheritance:r /grant "*S-1-5-32-544:F" /grant "*S-1-5-18:F" | Out-Null
$sshdConfigPath = "$env:ProgramData\ssh\sshd_config"
$sshdConfig = ""
if (Test-Path -LiteralPath $sshdConfigPath) {
  $sshdConfig = Get-Content -Raw -LiteralPath $sshdConfigPath
}
$globalLines = @()
$matchLines = @()
$inMatch = $false
foreach ($line in ($sshdConfig -split "\r?\n")) {
  if ($line -match '^\s*Match\s+') { $inMatch = $true }
  if (-not $inMatch -and $line -match '^\s*Port\s+\d+\s*$') { continue }
  if ($enforceKeyAuth -and -not $inMatch -and $line -match '^\s*(PasswordAuthentication|PubkeyAuthentication)\s+') { continue }
  if ($inMatch) { $matchLines += $line } else { $globalLines += $line }
}
foreach ($port in $sshPorts) { $globalLines += "Port $port" }
if ($enforceKeyAuth) {
  $globalLines += "PubkeyAuthentication yes"
  $globalLines += "PasswordAuthentication no"
}
if (($matchLines -join [Environment]::NewLine) -notmatch '(?im)^\s*Match\s+Group\s+administrators\b') {
  $matchLines += "Match Group administrators"
  $matchLines += "       AuthorizedKeysFile __PROGRAMDATA__/ssh/administrators_authorized_keys"
}
Set-Content -Encoding ASCII -LiteralPath $sshdConfigPath -Value (($globalLines + $matchLines) -join [Environment]::NewLine)
foreach ($port in $sshPorts) {
  $ruleName = "crabbox-sshd-$port"
  if (-not (Get-NetFirewallRule -Name $ruleName -ErrorAction SilentlyContinue)) {
    New-NetFirewallRule -Name $ruleName -DisplayName "Crabbox OpenSSH $port" -Enabled True -Direction Inbound -Protocol TCP -Action Allow -LocalPort $port | Out-Null
  }
}
Set-Service -Name sshd -StartupType Automatic
Start-Service sshd
if (-not (Test-Path -LiteralPath "C:\Program Files\Git\cmd\git.exe")) {
  Retry { Invoke-WebRequest -Uri ` + psQuote(gitForWindowsSetupURL) + ` -OutFile $gitInstaller -UseBasicParsing }
  Start-Process -FilePath $gitInstaller -ArgumentList "/VERYSILENT","/NORESTART","/NOCANCEL","/SP-" -Wait
}
$machinePath = [Environment]::GetEnvironmentVariable("Path", "Machine")
foreach ($path in @("C:\Program Files\OpenSSH", "C:\Program Files\Git\cmd", "C:\Program Files\Git\usr\bin")) {
  if ($machinePath -notlike "*$path*") { $machinePath = "$machinePath;$path" }
  if ($env:Path -notlike "*$path*") { $env:Path = "$env:Path;$path" }
}
[Environment]::SetEnvironmentVariable("Path", $machinePath, "Machine")
`
}

func windowsBootstrapPowerShell(cfg Config, publicKey string) string {
	script := windowsBootstrapHeaderPowerShell(cfg, publicKey, windowsBootstrapWorkRoot(cfg)) +
		windowsManagedCorePreludePowerShell(cfg) +
		windowsBootstrapCorePowerShell()
	if cfg.WindowsMode == windowsModeWSL2 {
		return script + windowsWSL2BootstrapPowerShell(cfg)
	}
	if cfg.Desktop {
		return script + windowsDesktopBootstrapPowerShell()
	}
	return script + windowsCoreBootstrapFinalizePowerShell()
}

func windowsBootstrapWorkRoot(cfg Config) string {
	if cfg.WindowsMode == windowsModeWSL2 {
		return defaultWindowsWorkRoot
	}
	if cfg.WorkRoot != "" {
		return cfg.WorkRoot
	}
	return defaultWindowsWorkRoot
}

func windowsWSLWorkRoot(cfg Config) string {
	if cfg.WorkRoot != "" {
		return cfg.WorkRoot
	}
	return defaultPOSIXWorkRoot
}

func windowsManagedCorePreludePowerShell(cfg Config) string {
	if cfg.WindowsMode == windowsModeNormal && cfg.Desktop {
		return `
	$vncPasswordPath = "C:\ProgramData\crabbox\vnc.password"
	$windowsUsernamePath = "C:\ProgramData\crabbox\windows.username"
	$windowsPasswordPath = "C:\ProgramData\crabbox\windows.password"
	$passwordPath = $vncPasswordPath
	$usernamePath = $windowsUsernamePath
	$passwordMirrorPath = $windowsPasswordPath
	$enforceKeyAuth = $false
	$userVNCStartupPath = "C:\ProgramData\crabbox\start-user-vnc.ps1"
	$userVNCStartupCommandPath = Join-Path (Join-Path (Join-Path "C:\Users" $user) "AppData\Roaming\Microsoft\Windows\Start Menu\Programs\Startup") "crabbox-user-vnc.cmd"
	$tightVNCInstaller = "$env:TEMP\tightvnc-2.8.85-gpl-setup-64bit.msi"
	`
	}
	return `
	$windowsUsernamePath = "C:\ProgramData\crabbox\windows.username"
	$windowsPasswordPath = "C:\ProgramData\crabbox\windows.password"
	$passwordPath = $windowsPasswordPath
	$usernamePath = $windowsUsernamePath
	$passwordMirrorPath = $null
	$enforceKeyAuth = $false
	`
}

func windowsWSL2BootstrapPowerShell(cfg Config) string {
	workRoot := windowsWSLWorkRoot(cfg)
	return `
	$wslDistro = "Crabbox"
	$wslRoot = "C:\ProgramData\crabbox\wsl\Crabbox"
	$wslRootfs = "C:\ProgramData\crabbox\wsl\ubuntu-noble-wsl-amd64.rootfs.tar.gz"
	$wslRootfsDownload = "$wslRootfs.download"
	$wslRootfsMinBytes = 100 * 1024 * 1024
	$wslSetup = "C:\ProgramData\crabbox\wsl\linux-setup.sh"
	$wslFeaturesMarker = "C:\ProgramData\crabbox\wsl-features-rebooted"
	$wslKernelMarker = "C:\ProgramData\crabbox\wsl-kernel-rebooted"
	function Restart-CrabboxBootstrap($MarkerPath) {
	  Set-Content -NoNewline -Encoding ASCII -Path $MarkerPath -Value (Get-Date).ToString("o")
	  Restart-Computer -Force
	  exit 0
	}
	$needsFeatureReboot = $false
	foreach ($feature in @("Microsoft-Windows-Subsystem-Linux", "VirtualMachinePlatform", "HypervisorPlatform")) {
	  $state = (Get-WindowsOptionalFeature -Online -FeatureName $feature -ErrorAction SilentlyContinue).State
	  if ($state -ne "Enabled") {
	    dism.exe /online /enable-feature /featurename:$feature /all /norestart | Out-Host
	    if ($LASTEXITCODE -ne 0 -and $LASTEXITCODE -ne 3010) { throw "enable $feature failed with exit $LASTEXITCODE" }
	    $needsFeatureReboot = $true
	  }
	}
	bcdedit.exe /set hypervisorlaunchtype auto | Out-Host
	if ($LASTEXITCODE -ne 0) { throw "bcdedit hypervisorlaunchtype failed with exit $LASTEXITCODE" }
	if ($needsFeatureReboot -and -not (Test-Path -LiteralPath $wslFeaturesMarker)) {
	  Restart-CrabboxBootstrap $wslFeaturesMarker
	}
	if (-not (Test-Path -LiteralPath $wslKernelMarker)) {
	  wsl.exe --update --web-download | Out-Host
	  if ($LASTEXITCODE -ne 0) { throw "wsl --update --web-download failed with exit $LASTEXITCODE" }
	  Restart-CrabboxBootstrap $wslKernelMarker
	}
	wsl.exe --set-default-version 2 | Out-Host
	if ($LASTEXITCODE -ne 0) { throw "wsl --set-default-version 2 failed with exit $LASTEXITCODE" }
	$distros = (wsl.exe --list --quiet 2>$null) -join [Environment]::NewLine
	if ($distros -notmatch "(?m)^$([Regex]::Escape($wslDistro))$") {
	  New-Item -ItemType Directory -Force -Path (Split-Path -Parent $wslRoot), $wslRoot | Out-Null
	  if ((Test-Path -LiteralPath $wslRootfs) -and ((Get-Item -LiteralPath $wslRootfs).Length -lt $wslRootfsMinBytes)) {
	    Remove-Item -Force -LiteralPath $wslRootfs
	  }
	  if (-not (Test-Path -LiteralPath $wslRootfs)) {
	    Remove-Item -Force -LiteralPath $wslRootfsDownload -ErrorAction SilentlyContinue
	    Retry {
	      $expectedLength = 0
	      try {
	        $head = Invoke-WebRequest -Uri ` + psQuote(ubuntuWSLRootFSURL) + ` -Method Head -UseBasicParsing
	        if ($head.Headers.ContainsKey("Content-Length")) {
	          [void][Int64]::TryParse(($head.Headers["Content-Length"] | Select-Object -First 1), [ref]$expectedLength)
	        }
	      } catch {
	        $expectedLength = 0
	      }
	      if (Get-Command curl.exe -ErrorAction SilentlyContinue) {
	        & curl.exe -fL --retry 8 --retry-delay 5 --connect-timeout 30 --speed-time 30 --speed-limit 1024 -o $wslRootfsDownload ` + psQuote(ubuntuWSLRootFSURL) + `
	        if ($LASTEXITCODE -ne 0) { throw "download WSL rootfs failed with exit $LASTEXITCODE" }
	      } else {
	        Invoke-WebRequest -Uri ` + psQuote(ubuntuWSLRootFSURL) + ` -OutFile $wslRootfsDownload -UseBasicParsing
	      }
	      $actualLength = (Get-Item -LiteralPath $wslRootfsDownload).Length
	      if ($actualLength -lt $wslRootfsMinBytes) { throw "downloaded WSL rootfs is incomplete" }
	      if ($expectedLength -gt 0 -and $actualLength -ne $expectedLength) {
	        throw "downloaded WSL rootfs is incomplete: $actualLength of $expectedLength bytes"
	      }
	    }
	    Move-Item -Force -LiteralPath $wslRootfsDownload -Destination $wslRootfs
	  }
	  wsl.exe --import $wslDistro $wslRoot $wslRootfs --version 2 | Out-Host
	  if ($LASTEXITCODE -ne 0) { throw "wsl --import failed with exit $LASTEXITCODE" }
	  wsl.exe --set-default $wslDistro | Out-Host
	  if ($LASTEXITCODE -ne 0) { throw "wsl --set-default failed with exit $LASTEXITCODE" }
	}
	$linuxSetup = @'
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
mkdir -p ` + shellQuote(workRoot) + ` /var/cache/crabbox/pnpm /var/cache/crabbox/npm /var/lib/crabbox
cat >/etc/apt/apt.conf.d/80-crabbox-retries <<'APT'
Acquire::Retries "8";
Acquire::http::Timeout "30";
Acquire::https::Timeout "30";
APT
rm -rf /var/lib/apt/lists/*
apt-get update
apt-get install -y --no-install-recommends ca-certificates curl git rsync jq
if [ -d /proc/sys/fs/binfmt_misc ]; then
  if [ ! -e /proc/sys/fs/binfmt_misc/register ]; then
    mount -t binfmt_misc binfmt_misc /proc/sys/fs/binfmt_misc 2>/dev/null || true
  fi
  if [ -e /proc/sys/fs/binfmt_misc/status ] && ! grep -qx enabled /proc/sys/fs/binfmt_misc/status; then
    printf '1' >/proc/sys/fs/binfmt_misc/status 2>/dev/null || true
  fi
  if [ ! -e /proc/sys/fs/binfmt_misc/WSLInterop ]; then
    test -w /proc/sys/fs/binfmt_misc/register
    printf '%s' ':WSLInterop:M::MZ::/init:PF' >/proc/sys/fs/binfmt_misc/register
  fi
fi
cat >/usr/local/bin/crabbox-ready <<'READY'
#!/usr/bin/env bash
set -euo pipefail
git --version >/dev/null
rsync --version >/dev/null
curl --version >/dev/null
jq --version >/dev/null
test -e /proc/sys/fs/binfmt_misc/WSLInterop
test -w ` + shellQuote(workRoot) + `
READY
chmod 0755 /usr/local/bin/crabbox-ready
touch /var/lib/crabbox/bootstrapped
crabbox-ready
'@
	$linuxSetup = $linuxSetup.Replace(([string][char]13 + [string][char]10), ([string][char]10))
	[IO.File]::WriteAllText($wslSetup, $linuxSetup, (New-Object Text.UTF8Encoding($false)))
	wsl.exe -d $wslDistro --user root --exec bash /mnt/c/ProgramData/crabbox/wsl/linux-setup.sh
	if ($LASTEXITCODE -ne 0) { throw "WSL setup failed with exit $LASTEXITCODE" }
	Set-Content -NoNewline -Encoding ASCII -Path $setupCompletePath -Value (Get-Date).ToString("o")
	Restart-Service sshd -Force
	`
}

func windowsDesktopBootstrapPowerShell() string {
	return `
	if (-not (Test-Path -LiteralPath "C:\Program Files\TightVNC\tvnserver.exe")) {
	  Retry { Invoke-WebRequest -Uri ` + psQuote(tightVNCMSIURL) + ` -OutFile $tightVNCInstaller -UseBasicParsing }
	  $vncPassword = Get-Content -Raw -Path $vncPasswordPath
	  Start-Process -FilePath msiexec.exe -ArgumentList @(
    "/i", $tightVNCInstaller, "/quiet", "/norestart",
    "ADDLOCAL=Server",
    "SERVER_REGISTER_AS_SERVICE=1",
    "SERVER_ADD_FIREWALL_EXCEPTION=0",
    "SET_USEVNCAUTHENTICATION=1", "VALUE_OF_USEVNCAUTHENTICATION=1",
    "SET_PASSWORD=1", "VALUE_OF_PASSWORD=$vncPassword",
    "SET_USECONTROLAUTHENTICATION=1", "VALUE_OF_USECONTROLAUTHENTICATION=1",
    "SET_CONTROLPASSWORD=1", "VALUE_OF_CONTROLPASSWORD=$vncPassword",
    "SET_ALLOWLOOPBACK=1", "VALUE_OF_ALLOWLOOPBACK=1",
    "SET_ACCEPTHTTPCONNECTIONS=1", "VALUE_OF_ACCEPTHTTPCONNECTIONS=0"
  ) -Wait
}
$userVNCStartup = @'
$ErrorActionPreference = "SilentlyContinue"
$base = "C:\ProgramData\crabbox"
$password = (Get-Content -Raw -LiteralPath (Join-Path $base "vnc.password")).Trim()
$serverKey = "HKCU:\Software\TightVNC\Server"
$serviceKey = "HKLM:\Software\TightVNC\Server"
$serviceConfig = Get-ItemProperty -Path $serviceKey -ErrorAction SilentlyContinue
function Set-TightVNCBinaryValue($Name) {
  $hex = ""
  if ($serviceConfig -and $serviceConfig.$Name) {
    $bytes = [byte[]]$serviceConfig.$Name
    if ($bytes -and $bytes.Length -gt 0) {
      $hex = -join ($bytes | ForEach-Object { $_.ToString("X2") })
    }
  }
  if ($hex) {
    & reg.exe add "HKCU\Software\TightVNC\Server" /v $Name /t REG_BINARY /d $hex /f | Out-Null
  }
}
New-Item -Force -Path $serverKey | Out-Null
New-ItemProperty -Force -Path $serverKey -Name UseVncAuthentication -PropertyType DWord -Value 1 | Out-Null
Set-TightVNCBinaryValue "Password"
New-ItemProperty -Force -Path $serverKey -Name UseControlAuthentication -PropertyType DWord -Value 1 | Out-Null
Set-TightVNCBinaryValue "ControlPassword"
New-ItemProperty -Force -Path $serverKey -Name AllowLoopback -PropertyType DWord -Value 1 | Out-Null
New-ItemProperty -Force -Path $serverKey -Name AcceptHttpConnections -PropertyType DWord -Value 0 | Out-Null
$exe = "C:\Program Files\TightVNC\tvnserver.exe"
Get-Process tvnserver -ErrorAction SilentlyContinue | Where-Object { $_.SessionId -eq (Get-Process -Id $PID).SessionId } | Stop-Process -Force -ErrorAction SilentlyContinue
Start-Sleep -Milliseconds 500
Start-Process -FilePath $exe -ArgumentList "-run" -WindowStyle Minimized
'@
Set-Content -Encoding UTF8 -LiteralPath $userVNCStartupPath -Value $userVNCStartup
New-Item -ItemType Directory -Force -Path (Split-Path -Parent $userVNCStartupCommandPath) | Out-Null
Set-Content -Encoding ASCII -LiteralPath $userVNCStartupCommandPath -Value ('@echo off' + [Environment]::NewLine + 'powershell.exe -NoProfile -WindowStyle Hidden -ExecutionPolicy Bypass -File "' + $userVNCStartupPath + '"' + [Environment]::NewLine)
$startupTask = "CrabboxUserVNC"
cmd.exe /c "schtasks.exe /Delete /TN $startupTask /F 2>NUL" | Out-Null
schtasks.exe /Create /TN $startupTask /SC ONLOGON /TR "powershell.exe -NoProfile -WindowStyle Hidden -ExecutionPolicy Bypass -File $userVNCStartupPath" /RU $user /IT /F | Out-Null
Get-Service -Name tvnserver -ErrorAction SilentlyContinue | Set-Service -StartupType Disabled
Stop-Service -Name tvnserver -Force -ErrorAction SilentlyContinue
$winlogon = "HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion\Winlogon"
Set-ItemProperty -Path $winlogon -Name AutoAdminLogon -Value "1" -Type String
Set-ItemProperty -Path $winlogon -Name ForceAutoLogon -Value "1" -Type String
Set-ItemProperty -Path $winlogon -Name DefaultUserName -Value $user -Type String
Set-ItemProperty -Path $winlogon -Name DefaultPassword -Value $userPassword -Type String
if (-not (Test-Path -LiteralPath $setupCompletePath)) {
  Set-Content -NoNewline -Encoding ASCII -Path $setupCompletePath -Value (Get-Date).ToString("o")
	  Restart-Computer -Force
	  exit 0
	}
Restart-Service sshd
	`
}

func windowsCoreBootstrapFinalizePowerShell() string {
	return `
	git --version | Out-Null
	tar --version | Out-Null
	Set-Content -NoNewline -Encoding ASCII -Path $setupCompletePath -Value (Get-Date).ToString("o")
	Restart-Service sshd -Force
	`
}

func azureWindowsBootstrapPowerShell(cfg Config, publicKey string) string {
	workRoot := windowsBootstrapWorkRoot(cfg)
	setupComplete := `Set-Content -NoNewline -Encoding ASCII -Path $setupCompletePath -Value (Get-Date).ToString("o")`
	if cfg.Desktop {
		setupComplete = ""
	}
	return windowsBootstrapHeaderPowerShell(cfg, publicKey, workRoot) + `
$passwordPath = Join-Path $base "windows.password"
$usernamePath = Join-Path $base "windows.username"
$passwordMirrorPath = $null
$enforceKeyAuth = $true
` + windowsBootstrapCorePowerShell() + `
git --version | Out-Null
tar --version | Out-Null
` + setupComplete + `
Restart-Service sshd -Force
`
}

func windowsSSHPortsPowerShell(cfg Config) string {
	ports := sshPortCandidates(cfg.SSHPort, cfg.SSHFallbackPorts)
	quoted := make([]string, 0, len(ports))
	for _, port := range ports {
		quoted = append(quoted, psQuote(port))
	}
	return "@(" + strings.Join(quoted, ", ") + ")"
}

func macOSUserData(cfg Config, publicKey string) string {
	workRoot := cfg.WorkRoot
	if workRoot == "" {
		workRoot = defaultMacOSWorkRoot
	}
	quotedPorts := make([]string, 0, len(sshPortCandidates(cfg.SSHPort, cfg.SSHFallbackPorts)))
	for _, port := range sshPortCandidates(cfg.SSHPort, cfg.SSHFallbackPorts) {
		quotedPorts = append(quotedPorts, shellQuote(port))
	}
	sshPortsShell := strings.Join(quotedPorts, " ")
	return `#!/bin/bash
set -euxo pipefail
crabbox_user=` + shellQuote(cfg.SSHUser) + `
crabbox_work_root=` + shellQuote(workRoot) + `
crabbox_public_key=` + shellQuote(publicKey) + `
crabbox_ssh_ports=(` + sshPortsShell + `)
id "$crabbox_user" >/dev/null
install -d -m 0755 "$crabbox_work_root" /var/db/crabbox
chown -R "$crabbox_user":staff "$crabbox_work_root"
user_home="$(dscl . -read "/Users/$crabbox_user" NFSHomeDirectory 2>/dev/null | awk '{print $2; exit}')"
if [ -z "$user_home" ]; then
  user_home="/Users/$crabbox_user"
fi
install -d -m 0700 -o "$crabbox_user" -g staff "$user_home/.ssh"
authorized_keys="$user_home/.ssh/authorized_keys"
touch "$authorized_keys"
if [ -n "$crabbox_public_key" ] && ! grep -qxF "$crabbox_public_key" "$authorized_keys"; then
  printf '%s\n' "$crabbox_public_key" >>"$authorized_keys"
fi
chown "$crabbox_user":staff "$user_home/.ssh" "$authorized_keys"
chmod 0700 "$user_home/.ssh"
chmod 0600 "$authorized_keys"
sshd_config=/etc/ssh/sshd_config
touch "$sshd_config"
tmp_config="$(mktemp)"
awk '
  /^# crabbox ssh ports begin$/ { skip=1; next }
  /^# crabbox ssh ports end$/ { skip=0; next }
  !skip { print }
' "$sshd_config" >"$tmp_config"
cat "$tmp_config" >"$sshd_config"
rm -f "$tmp_config"
{
  printf '%s\n' '# crabbox ssh ports begin'
  for port in "${crabbox_ssh_ports[@]}"; do
    printf 'Port %s\n' "$port"
  done
  printf '%s\n' 'PubkeyAuthentication yes'
  printf '%s\n' 'PasswordAuthentication no'
  printf '%s\n' '# crabbox ssh ports end'
} >>"$sshd_config"
/usr/sbin/sshd -t -f "$sshd_config"
systemsetup -setremotelogin on || true
launchctl enable system/com.openssh.sshd || true
launchctl load -w /System/Library/LaunchDaemons/ssh.plist || true
launchctl kickstart -k system/com.openssh.sshd || true
if [ ! -s /var/db/crabbox/vnc.password ]; then
  set +o pipefail
  pw="$(LC_ALL=C tr -dc 'A-Za-z0-9' </dev/urandom | head -c 16)"
  set -o pipefail
  if [ "${#pw}" -ne 16 ]; then
    echo "failed to generate vnc password" >&2
    exit 1
  fi
  printf '%s\n' "$pw" >/var/db/crabbox/vnc.password
  dscl . -passwd /Users/` + shellQuote(cfg.SSHUser) + ` "$pw"
fi
chmod 0600 /var/db/crabbox/vnc.password
launchctl enable system/com.apple.screensharing || true
launchctl load -w /System/Library/LaunchDaemons/com.apple.screensharing.plist || true
launchctl kickstart -k system/com.apple.screensharing || true
cat >/usr/local/bin/crabbox-ready <<'READY'
#!/bin/bash
set -euo pipefail
rsync --version >/dev/null
curl --version >/dev/null
test -w ` + shellQuote(workRoot) + `
ssh_ready=0
for port in ` + sshPortsShell + `; do
  if nc -z 127.0.0.1 "$port"; then
    ssh_ready=1
    break
  fi
done
test "$ssh_ready" -eq 1
nc -z 127.0.0.1 5900
READY
chmod 0755 /usr/local/bin/crabbox-ready
/usr/local/bin/crabbox-ready
`
}

func cloudInitOptionalReadyChecks(cfg Config) string {
	var b strings.Builder
	if cfg.Tailscale.Enabled {
		b.WriteString("      test -s /var/lib/crabbox/tailscale-ipv4\n")
		b.WriteString("      grep -Eq '^100\\.' /var/lib/crabbox/tailscale-ipv4\n")
	}
	if cfg.Desktop {
		if isWaylandDesktopEnv(cfg.DesktopEnv) {
			b.WriteString("      systemctl is-active --quiet crabbox-desktop.service\n")
			b.WriteString("      systemctl is-active --quiet crabbox-wayvnc.service\n")
		} else {
			b.WriteString("      systemctl is-active --quiet crabbox-xvfb.service\n")
			b.WriteString("      systemctl is-active --quiet crabbox-desktop.service\n")
			b.WriteString("      systemctl is-active --quiet crabbox-desktop-session.service\n")
			b.WriteString("      systemctl is-active --quiet crabbox-x11vnc.service\n")
		}
		b.WriteString("      ss -ltn | grep -q '127.0.0.1:5900'\n")
	}
	if cfg.Browser {
		b.WriteString("      test -s /var/lib/crabbox/browser.env\n")
		b.WriteString("      . /var/lib/crabbox/browser.env\n")
		b.WriteString("      test -x \"$BROWSER\"\n")
		b.WriteString("      \"$BROWSER\" --version >/dev/null\n")
	}
	if cfg.Code {
		b.WriteString("      test -x /usr/local/bin/code-server\n")
		b.WriteString("      /usr/local/bin/code-server --version >/dev/null\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func cloudInitOptionalWriteFiles(cfg Config) string {
	var parts []string
	if cfg.Provider == "gcp" {
		parts = append(parts, cloudInitGCPExpiryGuardFiles())
	}
	if cfg.Desktop && isWaylandDesktopEnv(cfg.DesktopEnv) {
		parts = append(parts, cloudInitWaylandDesktopWriteFiles(normalizedDesktopEnv(cfg.DesktopEnv)))
	} else if cfg.Desktop {
		parts = append(parts, `  - path: /etc/systemd/system/crabbox-xvfb.service
    permissions: '0644'
    content: |
      [Unit]
      Description=Crabbox Xvfb display
      After=network.target

      [Service]
      User=crabbox
      ExecStart=/usr/bin/Xvfb :99 -screen 0 1920x1080x24 -nolisten tcp -ac
      Restart=always

      [Install]
      WantedBy=multi-user.target
  - path: /usr/local/bin/crabbox-configure-desktop-theme
    permissions: '0755'
    content: |
      #!/bin/sh
      set -eu
      requested_mode="${1:-${CRABBOX_DESKTOP_THEME:-}}"
      user="${CRABBOX_DESKTOP_USER:-crabbox}"
      home_dir="$(getent passwd "$user" | cut -d: -f6)"
      if [ -z "$home_dir" ]; then
        home_dir="/home/$user"
      fi
      config_dir="$home_dir/.config"
      mode="$requested_mode"
      if [ -z "$mode" ] && [ -f "$config_dir/crabbox/desktop-theme" ]; then
        mode="$(cat "$config_dir/crabbox/desktop-theme" 2>/dev/null || true)"
      fi
      case "$mode" in
        light|dark) ;;
        *) mode=dark ;;
      esac
      if [ "$mode" = "light" ]; then
        gtk_theme=Adwaita
        gtk_prefer_dark=false
        gtk_prefer_dark_ini=0
        gsettings_scheme=prefer-light
        root_color="#f4f6f8"
        terminal_fg="#1f2937"
        terminal_bg="#f8fafc"
        terminal_cursor="#111827"
        panel_rgba="0.94 0.95 0.97 1"
        panel_css_bg="#eef2f7"
        panel_css_fg="#111827"
        gtk_candidates="Arc Greybird Adwaita"
        xfwm_candidates="Arc Greybird Daloa Default"
      else
        gtk_theme=Adwaita-dark
        gtk_prefer_dark=true
        gtk_prefer_dark_ini=1
        gsettings_scheme=prefer-dark
        root_color="#20242b"
        terminal_fg="#e5e7eb"
        terminal_bg="#111827"
        terminal_cursor="#f3f4f6"
        panel_rgba="0.12 0.13 0.15 1"
        panel_css_bg="#20242b"
        panel_css_fg="#e5e7eb"
        gtk_candidates="Arc-Dark Greybird-dark Adwaita-dark Greybird"
        xfwm_candidates="Arc-Dark Greybird-dark Daloa Default"
      fi
      for candidate in $gtk_candidates; do
        if [ -d "/usr/share/themes/$candidate/gtk-3.0" ]; then
          gtk_theme="$candidate"
          break
        fi
      done
      xfwm_theme=Default
      for candidate in $xfwm_candidates; do
        if [ -d "/usr/share/themes/$candidate/xfwm4" ]; then
          xfwm_theme="$candidate"
          break
        fi
      done
      if [ "$(id -u)" -eq 0 ]; then
        install -d -m 0700 -o "$user" "$config_dir/xfce4/xfconf/xfce-perchannel-xml" "$config_dir/xfce4/terminal" "$config_dir/gtk-3.0" "$config_dir/crabbox"
      else
        mkdir -p "$config_dir/xfce4/xfconf/xfce-perchannel-xml" "$config_dir/xfce4/terminal" "$config_dir/gtk-3.0" "$config_dir/crabbox"
        chmod 0700 "$config_dir" "$config_dir/xfce4" "$config_dir/xfce4/xfconf" "$config_dir/xfce4/xfconf/xfce-perchannel-xml" "$config_dir/xfce4/terminal" "$config_dir/gtk-3.0" "$config_dir/crabbox"
      fi
      printf '%s\n' "$mode" > "$config_dir/crabbox/desktop-theme"
      cat > "$config_dir/xfce4/xfconf/xfce-perchannel-xml/xsettings.xml" <<XML
      <?xml version="1.0" encoding="UTF-8"?>
      <channel name="xsettings" version="1.0">
        <property name="Net" type="empty">
          <property name="ThemeName" type="string" value="$gtk_theme"/>
          <property name="IconThemeName" type="string" value="Adwaita"/>
        </property>
        <property name="Gtk" type="empty">
          <property name="ApplicationPreferDarkTheme" type="bool" value="$gtk_prefer_dark"/>
        </property>
      </channel>
      XML
      if [ ! -s "$config_dir/xfce4/xfconf/xfce-perchannel-xml/xfwm4.xml" ]; then
        cat > "$config_dir/xfce4/xfconf/xfce-perchannel-xml/xfwm4.xml" <<XML
      <?xml version="1.0" encoding="UTF-8"?>
      <channel name="xfwm4" version="1.0">
        <property name="general" type="empty">
          <property name="theme" type="string" value="$xfwm_theme"/>
          <property name="box_move" type="bool" value="false"/>
          <property name="box_resize" type="bool" value="false"/>
          <property name="move_opacity" type="int" value="100"/>
          <property name="resize_opacity" type="int" value="100"/>
          <property name="snap_resist" type="bool" value="false"/>
          <property name="snap_to_border" type="bool" value="false"/>
          <property name="snap_to_windows" type="bool" value="false"/>
          <property name="snap_width" type="int" value="0"/>
          <property name="tile_on_move" type="bool" value="false"/>
          <property name="use_compositing" type="bool" value="false"/>
          <property name="wrap_windows" type="bool" value="false"/>
        </property>
      </channel>
      XML
      fi
      cat > "$config_dir/xfce4/terminal/terminalrc" <<EOF
      [Configuration]
      ColorForeground=$terminal_fg
      ColorBackground=$terminal_bg
      ColorCursor=$terminal_cursor
      MiscBell=FALSE
      EOF
      cat > "$config_dir/gtk-3.0/settings.ini" <<EOF
      [Settings]
      gtk-theme-name=$gtk_theme
      gtk-icon-theme-name=Adwaita
      gtk-application-prefer-dark-theme=$gtk_prefer_dark_ini
      EOF
      cat > "$home_dir/.gtkrc-2.0" <<EOF
      gtk-theme-name="$gtk_theme"
      gtk-icon-theme-name="Adwaita"
      gtk-application-prefer-dark-theme=$gtk_prefer_dark_ini
      EOF
      css_file="$config_dir/gtk-3.0/gtk.css"
      css_tmp="$(mktemp)"
      if [ -f "$css_file" ]; then
        sed '/^[/][*] crabbox desktop theme start [*][/]$/,/^[/][*] crabbox desktop theme end [*][/]$/d' "$css_file" > "$css_tmp" || true
      fi
      cat >> "$css_tmp" <<EOF
      /* crabbox desktop theme start */
      .xfce4-panel { background: $panel_css_bg; background-color: $panel_css_bg; color: $panel_css_fg; }
      .xfce4-panel * { color: $panel_css_fg; text-shadow: none; -gtk-icon-shadow: none; }
      .xfce4-panel button,
      .xfce4-panel button.flat,
      .xfce4-panel button:hover,
      .xfce4-panel button:active,
      .xfce4-panel button:checked,
      .xfce4-panel button:focus,
      .xfce4-panel button:backdrop,
      .xfce4-panel .tasklist button,
      .xfce4-panel .tasklist button:hover,
      .xfce4-panel .tasklist button:active,
      .xfce4-panel .tasklist button:checked,
      .xfce4-panel .tasklist button:checked:hover,
      .xfce4-panel .tasklist button:focus,
      .xfce4-panel .tasklist button:backdrop,
      .xfce4-panel .tasklist .toggle,
      .xfce4-panel .tasklist .toggle:hover,
      .xfce4-panel .tasklist .toggle:checked,
      .xfce4-panel .tasklist .toggle:checked:hover,
      .xfce4-panel .tasklist button:checked,
      .xfce4-panel .tasklist button:active {
        background: $panel_css_bg;
        background-image: none;
        background-color: $panel_css_bg;
        border-image: none;
        border-color: $panel_css_fg;
        box-shadow: none;
        color: $panel_css_fg;
        outline-color: transparent;
        text-shadow: none;
        -gtk-icon-shadow: none;
      }
      .xfce4-panel .tasklist button label,
      .xfce4-panel .tasklist .toggle label {
        color: $panel_css_fg;
        text-shadow: none;
      }
      /* crabbox desktop theme end */
      EOF
      mv "$css_tmp" "$css_file"
      if [ "$(id -u)" -eq 0 ]; then
        chown -R "$user" "$config_dir" "$home_dir/.gtkrc-2.0"
      fi
      if [ -n "${DISPLAY:-}" ] && command -v xfconf-query >/dev/null 2>&1; then
        xfconf-query -c xsettings -p /Net/ThemeName -n -t string -s "$gtk_theme" >/dev/null 2>&1 || true
        xfconf-query -c xsettings -p /Net/IconThemeName -n -t string -s Adwaita >/dev/null 2>&1 || true
        xfconf-query -c xsettings -p /Gtk/ApplicationPreferDarkTheme -n -t bool -s "$gtk_prefer_dark" >/dev/null 2>&1 || true
        xfconf-query -c xfwm4 -p /general/theme -n -t string -s "$xfwm_theme" >/dev/null 2>&1 || true
        xfconf-query -c xfwm4 -p /general/box_move -n -t bool -s false >/dev/null 2>&1 || true
        xfconf-query -c xfwm4 -p /general/box_resize -n -t bool -s false >/dev/null 2>&1 || true
        xfconf-query -c xfwm4 -p /general/move_opacity -n -t int -s 100 >/dev/null 2>&1 || true
        xfconf-query -c xfwm4 -p /general/resize_opacity -n -t int -s 100 >/dev/null 2>&1 || true
        xfconf-query -c xfwm4 -p /general/snap_resist -n -t bool -s false >/dev/null 2>&1 || true
        xfconf-query -c xfwm4 -p /general/snap_to_border -n -t bool -s false >/dev/null 2>&1 || true
        xfconf-query -c xfwm4 -p /general/snap_to_windows -n -t bool -s false >/dev/null 2>&1 || true
        xfconf-query -c xfwm4 -p /general/snap_width -n -t int -s 0 >/dev/null 2>&1 || true
        xfconf-query -c xfwm4 -p /general/tile_on_move -n -t bool -s false >/dev/null 2>&1 || true
        xfconf-query -c xfwm4 -p /general/use_compositing -n -t bool -s false >/dev/null 2>&1 || true
        xfconf-query -c xfwm4 -p /general/wrap_windows -n -t bool -s false >/dev/null 2>&1 || true
        xfconf-query -c xfce4-panel -p /panels/dark-mode -n -t bool -s "$gtk_prefer_dark" >/dev/null 2>&1 || true
        set -- $panel_rgba
        for panel_id in panel-1 panel-2; do
          xfconf-query -c xfce4-panel -p "/panels/$panel_id/background-style" -n -t int -s 1 >/dev/null 2>&1 || true
          xfconf-query -c xfce4-panel -p "/panels/$panel_id/background-rgba" -n -a -t double -s "$1" -t double -s "$2" -t double -s "$3" -t double -s "$4" >/dev/null 2>&1 || true
        done
        if [ "$(id -un)" = "$user" ]; then
          pkill -TERM -x xfce4-panel >/dev/null 2>&1 || true
          (sleep 0.4; xfce4-panel >"/tmp/crabbox-xfce4-panel-$user.log" 2>&1 &) >/dev/null 2>&1 &
        else
          pkill -USR1 -x xfce4-panel >/dev/null 2>&1 || true
        fi
        xfwm4 --replace --compositor=off >"/tmp/crabbox-xfwm4-replace-$user.log" 2>&1 &
      fi
      if [ -n "${DISPLAY:-}" ] && command -v xsetroot >/dev/null 2>&1; then
        xsetroot -solid "$root_color" || true
      fi
      if command -v gsettings >/dev/null 2>&1; then
        gsettings set org.gnome.desktop.interface color-scheme "$gsettings_scheme" >/dev/null 2>&1 || true
        gsettings set org.gnome.desktop.interface gtk-theme "$gtk_theme" >/dev/null 2>&1 || true
      fi
  - path: /etc/systemd/system/crabbox-desktop.service
    permissions: '0644'
    content: |
      [Unit]
      Description=Crabbox XFCE desktop session
      After=crabbox-xvfb.service
      Requires=crabbox-xvfb.service

      [Service]
      User=crabbox
      Environment=DISPLAY=:99
      ExecStart=/usr/bin/startxfce4
      Restart=always

      [Install]
      WantedBy=multi-user.target
  - path: /usr/local/bin/crabbox-desktop-session
    permissions: '0755'
    content: |
      #!/bin/sh
      set -eu
      export DISPLAY="${DISPLAY:-:99}"
      CRABBOX_DESKTOP_USER="$(id -un)" /usr/local/bin/crabbox-configure-desktop-theme || true
      if command -v xfce4-terminal >/dev/null 2>&1 && ! pgrep -u "$(id -u)" -f 'xfce4-terminal.*Crabbox Desktop' >/dev/null 2>&1; then
        xfce4-terminal --title='Crabbox Desktop' --geometry=110x32+48+48 &
      elif command -v xterm >/dev/null 2>&1 && ! pgrep -u "$(id -u)" -f 'xterm -title Crabbox Desktop' >/dev/null 2>&1; then
        xterm -title 'Crabbox Desktop' -geometry 110x32+48+48 -bg '#111827' -fg '#e5e7eb' &
      fi
      tail -f /dev/null
  - path: /etc/systemd/system/crabbox-desktop-session.service
    permissions: '0644'
    content: |
      [Unit]
      Description=Crabbox visible desktop helper
      After=crabbox-desktop.service
      Requires=crabbox-xvfb.service crabbox-desktop.service

      [Service]
      User=crabbox
      Environment=DISPLAY=:99
      ExecStart=/usr/local/bin/crabbox-desktop-session
      Restart=always

      [Install]
      WantedBy=multi-user.target
  - path: /etc/systemd/system/crabbox-x11vnc.service
    permissions: '0644'
    content: |
      [Unit]
      Description=Crabbox loopback VNC server
      After=crabbox-xvfb.service
      Requires=crabbox-xvfb.service

      [Service]
      User=crabbox
      ExecStart=/usr/bin/x11vnc -display :99 -localhost -rfbport 5900 -forever -shared -rfbauth /var/lib/crabbox/vnc.pass -wait 16 -defer 8 -nowait_bog
      Restart=always

      [Install]
      WantedBy=multi-user.target
`)
	}
	return strings.Join(parts, "\n")
}

func cloudInitWaylandDesktopWriteFiles(desktopEnv string) string {
	displayEnv := ""
	if desktopEnv == desktopEnvGnome {
		displayEnv = "      DISPLAY=:0\n      GDK_BACKEND=x11\n      MOZ_ENABLE_WAYLAND=0\n"
	}
	return `  - path: /usr/local/bin/crabbox-start-wayland-desktop
    permissions: '0755'
    content: |
      #!/bin/sh
      set -eu
      runtime="${XDG_RUNTIME_DIR:-/tmp/crabbox-runtime-$(id -u)}"
      install -d -m 0700 "$runtime"
      export XDG_RUNTIME_DIR="$runtime"
      export WLR_BACKENDS=headless
      export WLR_LIBINPUT_NO_DEVICES=1
      export WLR_RENDERER="${WLR_RENDERER:-pixman}"
      export MOZ_ENABLE_WAYLAND=1
      rm -f /var/lib/crabbox/display.env
      exec dbus-run-session labwc
  - path: /etc/systemd/system/crabbox-desktop.service
    permissions: '0644'
    content: |
      [Unit]
      Description=Crabbox Wayland desktop session
      After=network.target

      [Service]
      User=crabbox
      Environment=WLR_BACKENDS=headless
      Environment=WLR_LIBINPUT_NO_DEVICES=1
      Environment=WLR_RENDERER=pixman
      ExecStart=/usr/local/bin/crabbox-start-wayland-desktop
      Restart=always

      [Install]
      WantedBy=multi-user.target
  - path: /usr/local/bin/crabbox-start-wayvnc
    permissions: '0755'
    content: |
      #!/bin/sh
      set -eu
      runtime="${XDG_RUNTIME_DIR:-/tmp/crabbox-runtime-$(id -u)}"
      export XDG_RUNTIME_DIR="$runtime"
      for i in $(seq 1 60); do
        for socket in "$XDG_RUNTIME_DIR"/wayland-*; do
          [ -S "$socket" ] || continue
          export WAYLAND_DISPLAY="${socket##*/}"
          cat >/var/lib/crabbox/desktop.env <<EOF
      CRABBOX_DESKTOP_ENV=` + desktopEnv + `
      XDG_RUNTIME_DIR=$XDG_RUNTIME_DIR
      WAYLAND_DISPLAY=$WAYLAND_DISPLAY
` + displayEnv + `      EOF
          exec /usr/bin/wayvnc --config "$HOME/.config/wayvnc/config" --render-cursor --max-fps=60
        done
        sleep 1
      done
      echo "wayland socket not ready" >&2
      exit 1
  - path: /etc/systemd/system/crabbox-wayvnc.service
    permissions: '0644'
    content: |
      [Unit]
      Description=Crabbox loopback WayVNC server
      After=crabbox-desktop.service
      Requires=crabbox-desktop.service

      [Service]
      User=crabbox
      ExecStart=/usr/local/bin/crabbox-start-wayvnc
      Restart=always

      [Install]
      WantedBy=multi-user.target
`
}

func cloudInitOptionalBootstrap(cfg Config) string {
	var parts []string
	if cfg.Tailscale.Enabled {
		parts = append(parts, cloudInitTailscaleBootstrap(cfg))
	}
	if cfg.Desktop && isWaylandDesktopEnv(cfg.DesktopEnv) {
		desktopEnv := normalizedDesktopEnv(cfg.DesktopEnv)
		packages := "labwc wayvnc foot grim slurp wtype wl-clipboard wlr-randr dbus-user-session xwayland xdg-desktop-portal-wlr fonts-dejavu-core fonts-liberation iproute2 openssl procps"
		autostart := `    wlr-randr --output HEADLESS-1 --custom-mode 1920x1080 >/tmp/crabbox-wlr-randr.log 2>&1 || true
    foot --title='Crabbox Desktop' >/tmp/crabbox-foot.log 2>&1 &
`
		configDirs := "/home/crabbox/.config/labwc /home/crabbox/.config/wayvnc"
		desktopEnvExtra := ""
		themeBootstrap := ""
		themeConfigure := ""
		if desktopEnv == desktopEnvGnome {
			packages = "labwc wayvnc swaybg librsvg2-common gnome-panel wlr-randr grim slurp wtype wl-clipboard dbus-user-session xwayland xdg-desktop-portal-wlr xdg-desktop-portal-gtk gnome-terminal nautilus gsettings-desktop-schemas adwaita-icon-theme fonts-dejavu-core fonts-liberation iproute2 openssl procps"
			autostart = `    wlr-randr --output HEADLESS-1 --custom-mode 1920x1080 >/tmp/crabbox-wlr-randr.log 2>&1 || true
    for _ in $(seq 1 20); do
      [ -S /tmp/.X11-unix/X0 ] && break
      sleep 0.2
    done
    export XDG_CURRENT_DESKTOP=GNOME
    export XDG_SESSION_DESKTOP=gnome
    theme="$(cat "$HOME/.config/crabbox/desktop-theme" 2>/dev/null || printf dark)"
    if [ "$theme" = light ]; then
      export GTK_THEME=Adwaita
      gsettings set org.gnome.desktop.interface color-scheme prefer-light >/dev/null 2>&1 || true
      gsettings set org.gnome.desktop.interface gtk-theme Adwaita >/dev/null 2>&1 || true
    else
      export GTK_THEME=Adwaita-dark
      gsettings set org.gnome.desktop.interface color-scheme prefer-dark >/dev/null 2>&1 || true
      gsettings set org.gnome.desktop.interface gtk-theme Adwaita-dark >/dev/null 2>&1 || true
    fi
    export DISPLAY="${DISPLAY:-:0}"
    export GDK_BACKEND=x11
    export MOZ_ENABLE_WAYLAND=0
    wallpaper_file="$HOME/.config/crabbox/desktop-background-$theme.svg"
    if command -v swaybg >/dev/null 2>&1; then
      (swaybg -i "$wallpaper_file" -m fill >/tmp/crabbox-swaybg.log 2>&1 || swaybg -c "#0d1117" >/tmp/crabbox-swaybg.log 2>&1) &
    fi
    gnome-panel >/tmp/crabbox-gnome-panel.log 2>&1 &
    gnome-terminal -- bash -l >/tmp/crabbox-gnome-terminal.log 2>&1 &
    nautilus --new-window "$HOME" >/tmp/crabbox-nautilus.log 2>&1 &
`
			desktopEnvExtra = "    DISPLAY=:0\n    GDK_BACKEND=x11\n    MOZ_ENABLE_WAYLAND=0\n"
			themeBootstrap = indentCloudInitRuncmd(`cat >/usr/local/bin/crabbox-configure-desktop-theme <<'THEME'
#!/bin/sh
set -eu
requested_mode="${1:-${CRABBOX_DESKTOP_THEME:-}}"
user="${CRABBOX_DESKTOP_USER:-crabbox}"
home_dir="$(getent passwd "$user" | cut -d: -f6)"
if [ -z "$home_dir" ]; then
  home_dir="/home/$user"
fi
config_dir="$home_dir/.config"
mode="$requested_mode"
if [ -z "$mode" ] && [ -f "$config_dir/crabbox/desktop-theme" ]; then
  mode="$(cat "$config_dir/crabbox/desktop-theme" 2>/dev/null || true)"
fi
case "$mode" in
  light|dark) ;;
  *) mode=dark ;;
esac
if [ "$mode" = "light" ]; then
  gtk_theme=Adwaita
  gtk_prefer_dark_ini=0
  gsettings_scheme=prefer-light
  terminal_fg="#1f2937"
  terminal_bg="#f8fafc"
  labwc_title_bg="#f3f4f6"
  labwc_title_fg="#111827"
  labwc_inactive_title_bg="#e5e7eb"
  labwc_inactive_title_fg="#374151"
  labwc_border="#cbd5e1"
  terminal_menu_bg="#f3f4f6"
  terminal_menu_fg="#111827"
  terminal_menu_hover_bg="#e5e7eb"
  wallpaper_bg="#e7eef7"
  wallpaper_panel="#d6e7f2"
  wallpaper_accent="#0891b2"
  wallpaper_grid="#b9c7d7"
else
  gtk_theme=Adwaita-dark
  gtk_prefer_dark_ini=1
  gsettings_scheme=prefer-dark
  terminal_fg="#e5e7eb"
  terminal_bg="#000000"
  labwc_title_bg="#1f2329"
  labwc_title_fg="#e5e7eb"
  labwc_inactive_title_bg="#111827"
  labwc_inactive_title_fg="#9ca3af"
  labwc_border="#30363d"
  terminal_menu_bg="#2b2f36"
  terminal_menu_fg="#d1d5db"
  terminal_menu_hover_bg="#374151"
  wallpaper_bg="#0d1117"
  wallpaper_panel="#111827"
  wallpaper_accent="#22d3ee"
  wallpaper_grid="#1f2937"
fi
if [ "$(id -u)" -eq 0 ]; then
  install -d -m 0700 -o "$user" "$config_dir/crabbox" "$config_dir/gtk-3.0" "$config_dir/gtk-4.0"
else
  mkdir -p "$config_dir/crabbox" "$config_dir/gtk-3.0" "$config_dir/gtk-4.0" "$config_dir/labwc"
  chmod 0700 "$config_dir" "$config_dir/crabbox" "$config_dir/gtk-3.0" "$config_dir/gtk-4.0" "$config_dir/labwc"
fi
printf '%s\n' "$mode" > "$config_dir/crabbox/desktop-theme"
for gtk_dir in "$config_dir/gtk-3.0" "$config_dir/gtk-4.0"; do
  cat > "$gtk_dir/settings.ini" <<EOF
[Settings]
gtk-theme-name=$gtk_theme
gtk-icon-theme-name=Adwaita
gtk-application-prefer-dark-theme=$gtk_prefer_dark_ini
EOF
done
cat > "$home_dir/.gtkrc-2.0" <<EOF
gtk-theme-name="$gtk_theme"
gtk-icon-theme-name="Adwaita"
gtk-application-prefer-dark-theme=$gtk_prefer_dark_ini
EOF
if [ "$(id -u)" -eq 0 ]; then
  chown -R "$user" "$config_dir/crabbox" "$config_dir/gtk-3.0" "$config_dir/gtk-4.0" "$home_dir/.gtkrc-2.0"
fi
if [ -f /var/lib/crabbox/desktop.env ]; then
  . /var/lib/crabbox/desktop.env
fi
display="${DISPLAY:-:0}"
runtime="${XDG_RUNTIME_DIR:-/tmp/crabbox-runtime-$(id -u "$user")}"
dbus_address="${DBUS_SESSION_BUS_ADDRESS:-}"
if [ -z "$dbus_address" ]; then
  labwc_pid="$(pgrep -u "$user" -n -x labwc 2>/dev/null || true)"
  if [ -n "$labwc_pid" ] && [ -r "/proc/$labwc_pid/environ" ]; then
    dbus_address="$(tr '\0' '\n' < "/proc/$labwc_pid/environ" | sed -n 's/^DBUS_SESSION_BUS_ADDRESS=//p' | head -n1)"
  fi
fi
set_gnome_terminal_theme() {
  profiles="$(gsettings get org.gnome.Terminal.ProfilesList list 2>/dev/null | tr -d "[],'" || true)"
  default_profile="$(gsettings get org.gnome.Terminal.ProfilesList default 2>/dev/null | tr -d "'" || true)"
  if [ -n "$default_profile" ] && ! printf ' %s ' "$profiles" | grep -q " $default_profile "; then
    profiles="$profiles $default_profile"
  fi
  for profile in $profiles; do
    [ -n "$profile" ] || continue
    profile_path="/org/gnome/terminal/legacy/profiles:/:$profile/"
    gsettings set "org.gnome.Terminal.Legacy.Profile:$profile_path" use-theme-colors false >/dev/null 2>&1 || true
    gsettings set "org.gnome.Terminal.Legacy.Profile:$profile_path" foreground-color "$terminal_fg" >/dev/null 2>&1 || true
    gsettings set "org.gnome.Terminal.Legacy.Profile:$profile_path" background-color "$terminal_bg" >/dev/null 2>&1 || true
    gsettings set "org.gnome.Terminal.Legacy.Profile:$profile_path" use-transparent-background false >/dev/null 2>&1 || true
  done
}
set_gtk_chrome_theme() {
  cat > "$config_dir/gtk-3.0/gtk.css" <<EOF
menubar, .menubar {
  background-color: $terminal_menu_bg;
  color: $terminal_menu_fg;
}
menubar menuitem, menubar menuitem label {
  color: $terminal_menu_fg;
}
menubar menuitem:hover {
  background-color: $terminal_menu_hover_bg;
  color: $terminal_menu_fg;
}
EOF
}
set_labwc_theme() {
  mkdir -p "$config_dir/labwc"
  cat > "$config_dir/labwc/themerc-override" <<EOF
window.active.title.bg.color: $labwc_title_bg
window.active.label.text.color: $labwc_title_fg
window.inactive.title.bg.color: $labwc_inactive_title_bg
window.inactive.label.text.color: $labwc_inactive_title_fg
window.active.border.color: $labwc_border
window.inactive.border.color: $labwc_border
window.active.button.unpressed.image.color: $labwc_title_fg
window.inactive.button.unpressed.image.color: $labwc_inactive_title_fg
window.active.button.hover.image.color: $labwc_title_fg
window.inactive.button.hover.image.color: $labwc_inactive_title_fg
window.active.button.pressed.image.color: $labwc_title_fg
window.inactive.button.pressed.image.color: $labwc_inactive_title_fg
EOF
  if command -v labwc >/dev/null 2>&1; then
    labwc_pid="$(pgrep -u "$user" -n -x labwc 2>/dev/null || true)"
    if [ -n "$labwc_pid" ]; then
      LABWC_PID="$labwc_pid" XDG_RUNTIME_DIR="$runtime" WAYLAND_DISPLAY="${WAYLAND_DISPLAY:-wayland-0}" labwc --reconfigure >/dev/null 2>&1 || kill -HUP "$labwc_pid" >/dev/null 2>&1 || true
    fi
  fi
}
set_desktop_background() {
  wallpaper_file="$config_dir/crabbox/desktop-background-$mode.svg"
  cat > "$wallpaper_file" <<EOF
<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1920 1080">
  <rect width="1920" height="1080" fill="$wallpaper_bg"/>
  <path d="M0 720 C360 620 520 760 860 650 C1210 540 1430 660 1920 520 L1920 1080 L0 1080 Z" fill="$wallpaper_panel"/>
  <g stroke="$wallpaper_grid" stroke-width="1" opacity="0.45">
    <path d="M0 180 H1920M0 360 H1920M0 540 H1920M0 720 H1920M0 900 H1920"/>
    <path d="M240 0 V1080M480 0 V1080M720 0 V1080M960 0 V1080M1200 0 V1080M1440 0 V1080M1680 0 V1080"/>
  </g>
  <path d="M220 740 C520 520 790 910 1090 670 S1510 520 1710 700" fill="none" stroke="$wallpaper_accent" stroke-width="18" stroke-linecap="round" opacity="0.8"/>
  <rect x="1320" y="180" width="360" height="170" rx="18" fill="$wallpaper_accent" opacity="0.12"/>
</svg>
EOF
  if command -v swaybg >/dev/null 2>&1; then
    pkill -u "$user" -x swaybg >/dev/null 2>&1 || true
    (XDG_RUNTIME_DIR="$runtime" WAYLAND_DISPLAY="${WAYLAND_DISPLAY:-wayland-0}" swaybg -i "$wallpaper_file" -m fill >/tmp/crabbox-swaybg.log 2>&1 || XDG_RUNTIME_DIR="$runtime" WAYLAND_DISPLAY="${WAYLAND_DISPLAY:-wayland-0}" swaybg -c "$wallpaper_bg" >/tmp/crabbox-swaybg.log 2>&1) &
  fi
}
target_uid="$(id -u "$user" 2>/dev/null || printf 0)"
if [ "$(id -u)" -eq 0 ] && [ "$target_uid" -ne 0 ]; then
  su "$user" -s /bin/sh -c "CRABBOX_DESKTOP_USER='$user' CRABBOX_DESKTOP_THEME='$mode' DISPLAY='$display' XDG_RUNTIME_DIR='$runtime' DBUS_SESSION_BUS_ADDRESS='$dbus_address' GDK_BACKEND=x11 /usr/local/bin/crabbox-configure-desktop-theme '$mode'" || true
  exit 0
fi
if command -v gsettings >/dev/null 2>&1; then
  if [ "$(id -u)" -eq 0 ]; then
    su "$user" -s /bin/sh -c "DISPLAY='$display' XDG_RUNTIME_DIR='$runtime' DBUS_SESSION_BUS_ADDRESS='$dbus_address' GDK_BACKEND=x11 gsettings set org.gnome.desktop.interface color-scheme '$gsettings_scheme' >/dev/null 2>&1 || true"
    su "$user" -s /bin/sh -c "DISPLAY='$display' XDG_RUNTIME_DIR='$runtime' DBUS_SESSION_BUS_ADDRESS='$dbus_address' GDK_BACKEND=x11 gsettings set org.gnome.desktop.interface gtk-theme '$gtk_theme' >/dev/null 2>&1 || true"
  else
    DISPLAY="$display" XDG_RUNTIME_DIR="$runtime" DBUS_SESSION_BUS_ADDRESS="$dbus_address" GDK_BACKEND=x11 gsettings set org.gnome.desktop.interface color-scheme "$gsettings_scheme" >/dev/null 2>&1 || true
    DISPLAY="$display" XDG_RUNTIME_DIR="$runtime" DBUS_SESSION_BUS_ADDRESS="$dbus_address" GDK_BACKEND=x11 gsettings set org.gnome.desktop.interface gtk-theme "$gtk_theme" >/dev/null 2>&1 || true
    DISPLAY="$display" XDG_RUNTIME_DIR="$runtime" DBUS_SESSION_BUS_ADDRESS="$dbus_address" GDK_BACKEND=x11 set_gnome_terminal_theme
  fi
fi
set_gtk_chrome_theme
set_labwc_theme
set_desktop_background
if [ "$(id -u)" -eq 0 ] && pgrep -u "$user" -x gnome-panel >/dev/null 2>&1; then
  pkill -TERM -u "$user" -x gnome-panel >/dev/null 2>&1 || true
  su "$user" -s /bin/sh -c "DISPLAY='$display' XDG_RUNTIME_DIR='$runtime' DBUS_SESSION_BUS_ADDRESS='$dbus_address' GDK_BACKEND=x11 GTK_THEME='$gtk_theme' nohup gnome-panel >/tmp/crabbox-gnome-panel.log 2>&1 &" >/dev/null 2>&1 || true
elif [ "$(id -u)" -ne 0 ] && pgrep -x gnome-panel >/dev/null 2>&1; then
  pkill -TERM -x gnome-panel >/dev/null 2>&1 || true
  DISPLAY="$display" XDG_RUNTIME_DIR="$runtime" DBUS_SESSION_BUS_ADDRESS="$dbus_address" GDK_BACKEND=x11 GTK_THEME="$gtk_theme" nohup gnome-panel >/tmp/crabbox-gnome-panel.log 2>&1 &
fi
previous_terminal_theme="$(cat "$config_dir/crabbox/gnome-terminal-theme" 2>/dev/null || true)"
printf '%s\n' "$mode" > "$config_dir/crabbox/gnome-terminal-theme"
if [ "$(id -u)" -ne 0 ] && [ "$mode" = dark ] && command -v gnome-terminal >/dev/null 2>&1 && { [ "$previous_terminal_theme" != "$mode" ] || ! pgrep -u "$(id -u)" -f '/gnome-terminal-server' >/dev/null 2>&1; }; then
  (sleep 0.4; DISPLAY="$display" XDG_RUNTIME_DIR="$runtime" DBUS_SESSION_BUS_ADDRESS="$dbus_address" GDK_BACKEND=x11 GTK_THEME="$gtk_theme" NO_AT_BRIDGE=1 gnome-terminal -- bash -l >/tmp/crabbox-gnome-terminal.log 2>&1 &) >/dev/null 2>&1 &
fi
THEME
chmod 0755 /usr/local/bin/crabbox-configure-desktop-theme
`)
			themeConfigure = "    CRABBOX_DESKTOP_USER=crabbox /usr/local/bin/crabbox-configure-desktop-theme\n"
		}
		parts = append(parts, `    retry apt-get install -y --no-install-recommends `+packages+`
    install -d -m 0750 -o crabbox -g crabbox /var/lib/crabbox
    if [ ! -s /var/lib/crabbox/vnc.password ]; then
      (umask 077 && openssl rand -base64 18 > /var/lib/crabbox/vnc.password)
    fi
    chown crabbox:crabbox /var/lib/crabbox/vnc.password
    chmod 0600 /var/lib/crabbox/vnc.password
    crabbox_uid="$(id -u crabbox)"
    crabbox_runtime="/tmp/crabbox-runtime-$crabbox_uid"
    install -d -m 0700 -o crabbox -g crabbox "$crabbox_runtime"
    install -d -m 0700 -o crabbox -g crabbox `+configDirs+`
`+themeBootstrap+`    cat >/home/crabbox/.config/labwc/autostart <<'AUTOSTART'
`+autostart+`    AUTOSTART
    chmod 0755 /home/crabbox/.config/labwc/autostart
    cat >/home/crabbox/.config/wayvnc/config <<'WAYVNC'
    address=127.0.0.1
    port=5900
    enable_auth=false
    xkb_layout=us
    WAYVNC
    cat >/var/lib/crabbox/desktop.env <<EOF
    CRABBOX_DESKTOP_ENV=`+desktopEnv+`
    XDG_RUNTIME_DIR=$crabbox_runtime
    WAYLAND_DISPLAY=wayland-1
`+desktopEnvExtra+`
    EOF
    chown -R crabbox:crabbox /home/crabbox/.config /var/lib/crabbox/desktop.env
    chmod 0644 /var/lib/crabbox/desktop.env
`+themeConfigure+`    systemctl daemon-reload
    systemctl disable --now crabbox-xvfb.service crabbox-desktop-session.service crabbox-x11vnc.service 2>/dev/null || true
    systemctl enable crabbox-desktop.service crabbox-wayvnc.service
    systemctl restart crabbox-desktop.service crabbox-wayvnc.service`)
	} else if cfg.Desktop {
		parts = append(parts, `    retry apt-get install -y --no-install-recommends xvfb xfce4-session xfwm4 xfce4-panel xfdesktop4 xfce4-terminal xfconf xfce4-settings x11vnc xauth dbus-x11 x11-xserver-utils xterm scrot ffmpeg xdotool wmctrl xclip xsel fonts-dejavu-core fonts-liberation iproute2 openssl arc-theme
    install -d -m 0750 -o crabbox -g crabbox /var/lib/crabbox
    if [ ! -s /var/lib/crabbox/vnc.password ]; then
      (umask 077 && openssl rand -base64 18 > /var/lib/crabbox/vnc.password)
    fi
    x11vnc -storepasswd "$(cat /var/lib/crabbox/vnc.password)" /var/lib/crabbox/vnc.pass >/dev/null
    chown crabbox:crabbox /var/lib/crabbox/vnc.password /var/lib/crabbox/vnc.pass
    chmod 0600 /var/lib/crabbox/vnc.password /var/lib/crabbox/vnc.pass
    printf 'CRABBOX_DESKTOP_ENV=xfce\nDISPLAY=:99\n' >/var/lib/crabbox/desktop.env
    chown crabbox:crabbox /var/lib/crabbox/desktop.env
    chmod 0644 /var/lib/crabbox/desktop.env
    CRABBOX_DESKTOP_USER=crabbox /usr/local/bin/crabbox-configure-desktop-theme
    systemctl daemon-reload
    systemctl disable --now crabbox-wayvnc.service 2>/dev/null || true
    systemctl enable crabbox-xvfb.service crabbox-desktop.service crabbox-desktop-session.service crabbox-x11vnc.service
    systemctl restart crabbox-xvfb.service crabbox-desktop.service crabbox-desktop-session.service crabbox-x11vnc.service`)
	}
	if cfg.Provider == "gcp" {
		parts = append(parts, cloudInitGCPExpiryGuardBootstrap())
	}
	if cfg.Browser {
		parts = append(parts, `    retry apt-get install -y --no-install-recommends gnupg build-essential python3
    browser_path=""
    if [ "$(dpkg --print-architecture)" = "amd64" ]; then
      install -d -m 0755 /etc/apt/trusted.gpg.d
      curl -fsSL https://dl.google.com/linux/linux_signing_key.pub > /etc/apt/trusted.gpg.d/google.asc
      chmod 0644 /etc/apt/trusted.gpg.d/google.asc
      echo "deb [arch=amd64] https://dl.google.com/linux/chrome/deb/ stable main" > /etc/apt/sources.list.d/google-chrome.list
      if apt-get update && retry apt-get install -y --no-install-recommends google-chrome-stable; then
        browser_path="$(command -v google-chrome || true)"
      else
        rm -f /etc/apt/sources.list.d/google-chrome.list
        retry apt-get update || true
      fi
    fi
    if [ -z "$browser_path" ]; then
      if apt-cache show chromium >/dev/null 2>&1 && retry apt-get install -y --no-install-recommends chromium; then
        browser_path="$(command -v chromium || true)"
      elif apt-cache show chromium-browser >/dev/null 2>&1 && retry apt-get install -y --no-install-recommends chromium-browser; then
        browser_path="$(command -v chromium-browser || true)"
      fi
    fi
    if [ -n "$browser_path" ]; then
      browser_wrapper=/usr/local/bin/crabbox-browser
      install -d -m 0755 /etc/opt/chrome/policies/managed /etc/chromium/policies/managed
      printf '%s\n' '{"DefaultBrowserSettingEnabled":false,"MetricsReportingEnabled":false,"PromotionalTabsEnabled":false}' > /etc/opt/chrome/policies/managed/crabbox.json
      cp /etc/opt/chrome/policies/managed/crabbox.json /etc/chromium/policies/managed/crabbox.json
      if [ -f /var/lib/crabbox/desktop.env ] && grep -q '^CRABBOX_DESKTOP_ENV=gnome$' /var/lib/crabbox/desktop.env; then
        printf '%s\n' '#!/bin/sh' 'if [ -f /var/lib/crabbox/desktop.env ]; then . /var/lib/crabbox/desktop.env; fi' 'export DISPLAY="${DISPLAY:-:0}"' 'export XDG_RUNTIME_DIR WAYLAND_DISPLAY' 'export GDK_BACKEND=x11 MOZ_ENABLE_WAYLAND=0' 'profile="${CRABBOX_BROWSER_PROFILE:-$HOME/.cache/crabbox/browser-profile}"' 'theme="$(cat "${CRABBOX_DESKTOP_THEME_FILE:-$HOME/.config/crabbox/desktop-theme}" 2>/dev/null || printf dark)"' 'umask 077' 'mkdir -p "$profile"' 'chmod 700 "$profile"' 'if [ "$theme" = light ]; then' "  exec \"$browser_path\" --no-first-run --no-default-browser-check --disable-default-apps --hide-crash-restore-bubble --blink-settings=preferredColorScheme=1 --user-data-dir=\"\$profile\" --ozone-platform=x11 --window-size=1500,900 --window-position=80,80 \"\$@\"" 'fi' "exec \"$browser_path\" --no-first-run --no-default-browser-check --disable-default-apps --hide-crash-restore-bubble --force-dark-mode --enable-features=WebUIDarkMode --blink-settings=preferredColorScheme=2 --user-data-dir=\"\$profile\" --ozone-platform=x11 --window-size=1500,900 --window-position=80,80 \"\$@\"" > "$browser_wrapper"
      elif [ -f /var/lib/crabbox/desktop.env ] && grep -q '^CRABBOX_DESKTOP_ENV=wayland$' /var/lib/crabbox/desktop.env; then
        printf '%s\n' '#!/bin/sh' 'if [ -f /var/lib/crabbox/desktop.env ]; then . /var/lib/crabbox/desktop.env; fi' 'export XDG_RUNTIME_DIR WAYLAND_DISPLAY' 'export MOZ_ENABLE_WAYLAND=1' 'profile="${CRABBOX_BROWSER_PROFILE:-$HOME/.cache/crabbox/browser-profile}"' 'umask 077' 'mkdir -p "$profile"' 'chmod 700 "$profile"' "exec \"$browser_path\" --no-first-run --no-default-browser-check --disable-default-apps --hide-crash-restore-bubble --user-data-dir=\"\$profile\" --ozone-platform=wayland --window-size=1500,900 --window-position=80,80 \"\$@\"" > "$browser_wrapper"
      else
        printf '%s\n' '#!/bin/sh' 'profile="${CRABBOX_BROWSER_PROFILE:-$HOME/.cache/crabbox/browser-profile}"' 'umask 077' 'mkdir -p "$profile"' 'chmod 700 "$profile"' "exec \"$browser_path\" --no-first-run --no-default-browser-check --disable-default-apps --hide-crash-restore-bubble --user-data-dir=\"\$profile\" --window-size=1500,900 --window-position=80,80 \"\$@\"" > "$browser_wrapper"
      fi
      chmod 0755 "$browser_wrapper"
      printf 'CHROME_BIN=%s\nBROWSER=%s\n' "$browser_wrapper" "$browser_wrapper" > /var/lib/crabbox/browser.env
      chown crabbox:crabbox /var/lib/crabbox/browser.env
      chmod 0644 /var/lib/crabbox/browser.env
    fi`)
	}
	if cfg.Code {
		parts = append(parts, `    retry apt-get install -y --no-install-recommends libatomic1
    retry env HOME=/root sh -c 'curl -fsSL https://code-server.dev/install.sh | sh -s -- --method=standalone --prefix=/usr/local'
    /usr/local/bin/code-server --version >/dev/null`)
	}
	return strings.Join(parts, "\n")
}

func indentCloudInitRuncmd(script string) string {
	if script == "" {
		return ""
	}
	lines := strings.SplitAfter(script, "\n")
	for i, line := range lines {
		if line == "" {
			continue
		}
		lines[i] = "    " + line
	}
	return strings.Join(lines, "")
}

func cloudInitGCPExpiryGuardFiles() string {
	return `  - path: /usr/local/sbin/crabbox-gcp-expiry-guard
    permissions: '0755'
    content: |
      #!/usr/bin/env bash
      set -euo pipefail
      metadata() {
        curl -fsS -H 'Metadata-Flavor: Google' "http://metadata.google.internal/computeMetadata/v1/$1"
      }
      project="$(metadata project/project-id 2>/dev/null || true)"
      name="$(metadata instance/name 2>/dev/null || true)"
      zone_path="$(metadata instance/zone 2>/dev/null || true)"
      zone="${zone_path##*/}"
      token_json="$(metadata instance/service-accounts/default/token 2>/dev/null || true)"
      token="$(printf '%s' "$token_json" | jq -r '.access_token // empty' 2>/dev/null || true)"
      if [ -z "$project" ] || [ -z "$name" ] || [ -z "$zone" ] || [ -z "$token" ]; then
        exit 0
      fi
      instance="$(curl -fsS -H "Authorization: Bearer $token" "https://compute.googleapis.com/compute/v1/projects/$project/zones/$zone/instances/$name" 2>/dev/null || true)"
      if [ -z "$instance" ]; then
        exit 0
      fi
      label() {
        printf '%s' "$instance" | jq -r --arg key "$1" '.labels[$key] // empty'
      }
      crabbox="$(label crabbox)"
      provider="$(label provider)"
      if [ "$crabbox" != "true" ] || { [ -n "$provider" ] && [ "$provider" != "gcp" ]; }; then
        exit 0
      fi
      keep="$(label keep | tr '[:upper:]' '[:lower:]')"
      if [ "$keep" = "true" ]; then
        exit 0
      fi
      expires_at="$(label expires_at)"
      state="$(label state | tr '[:upper:]' '[:lower:]')"
      now="$(date -u +%s)"
      delete=false
      case "$state" in
        failed|released|expired)
          delete=true
          ;;
        running|provisioning)
          if [[ "$expires_at" =~ ^[0-9]+$ ]] && [ "$now" -gt $((expires_at + 43200)) ]; then
            delete=true
          fi
          ;;
        leased|ready|active|"")
          if [[ "$expires_at" =~ ^[0-9]+$ ]] && [ "$now" -gt "$expires_at" ]; then
            delete=true
          fi
          ;;
        *)
          if [[ "$expires_at" =~ ^[0-9]+$ ]] && [ "$now" -gt "$expires_at" ]; then
            delete=true
          fi
          ;;
      esac
      if [ "$delete" != "true" ]; then
        exit 0
      fi
      logger -t crabbox-gcp-expiry-guard "deleting expired lease=$(label lease) state=${state:-unknown} expires_at=${expires_at:-missing}"
      curl -fsS -X DELETE -H "Authorization: Bearer $token" "https://compute.googleapis.com/compute/v1/projects/$project/zones/$zone/instances/$name" >/dev/null || true
  - path: /etc/systemd/system/crabbox-gcp-expiry-guard.service
    permissions: '0644'
    content: |
      [Unit]
      Description=Crabbox GCP direct lease expiry guard
      After=network-online.target
      Wants=network-online.target

      [Service]
      Type=oneshot
      ExecStart=/usr/local/sbin/crabbox-gcp-expiry-guard
  - path: /etc/systemd/system/crabbox-gcp-expiry-guard.timer
    permissions: '0644'
    content: |
      [Unit]
      Description=Run Crabbox GCP direct lease expiry guard

      [Timer]
      OnBootSec=2min
      OnUnitActiveSec=2min
      RandomizedDelaySec=30s
      Persistent=true

      [Install]
      WantedBy=timers.target`
}

func cloudInitGCPExpiryGuardBootstrap() string {
	return `    systemctl daemon-reload
    systemctl enable --now crabbox-gcp-expiry-guard.timer`
}

func cloudInitTailscaleBootstrap(cfg Config) string {
	authKey := strings.TrimSpace(cfg.Tailscale.AuthKey)
	hostname := strings.TrimSpace(cfg.Tailscale.Hostname)
	if hostname == "" {
		hostname = renderTailscaleHostname(cfg.Tailscale.HostnameTemplate, "", "lease", cfg.Provider)
	}
	sshUser := strings.TrimSpace(cfg.SSHUser)
	if sshUser == "" {
		sshUser = "crabbox"
	}
	sshUserOwner := shellQuote(sshUser)
	sshUserGroup := shellQuote(sshUser)
	sshUserChown := shellQuote(sshUser + ":" + sshUser)
	tags := strings.Join(cfg.Tailscale.Tags, ",")
	tailscaleUpArgs := []string{
		"--auth-key=\"$TS_AUTHKEY\"",
		"--hostname=" + shellQuote(hostname),
		"--advertise-tags=" + shellQuote(tags),
	}
	exitNode := strings.TrimSpace(cfg.Tailscale.ExitNode)
	if exitNode != "" {
		tailscaleUpArgs = append(tailscaleUpArgs, "--exit-node="+shellQuote(exitNode))
		if cfg.Tailscale.ExitNodeAllowLANAccess {
			tailscaleUpArgs = append(tailscaleUpArgs, "--exit-node-allow-lan-access")
		}
	}
	if authKey == "" {
		return `    echo "tailscale requested but no auth key was injected" >&2
    exit 1`
	}
	// TS_CONTROL_URL on the operator shell forwards to the box so the
	// embedded `tailscale up` registers against a self-hosted control plane
	// (Headscale, etc.) via --login-server. Unset means the default Tailscale
	// control plane, which keeps the existing behavior identical.
	controlURL := strings.TrimSpace(os.Getenv("TS_CONTROL_URL"))
	loginServerExport := ""
	if controlURL != "" {
		loginServerExport = "TS_LOGIN_SERVER=" + shellQuote(controlURL) + "\n    "
	}
	loginServerFlag := `${TS_LOGIN_SERVER:+--login-server="$TS_LOGIN_SERVER"}`
	tailscaleUpScript := `    retry sh -c 'curl -fsSL https://tailscale.com/install.sh | sh'
    systemctl enable --now tailscaled || service tailscaled start || true
    install -d -m 0750 -o ` + sshUserOwner + ` -g ` + sshUserGroup + ` /var/lib/crabbox
    set +x
    ` + loginServerExport + `TS_AUTHKEY=` + shellQuote(authKey) + `
    tailscale up ` + strings.Join(tailscaleUpArgs, " ") + " " + loginServerFlag + `
    unset TS_AUTHKEY
    set -x
    ts_ip=""
    for _ in $(seq 1 24); do
      ts_ip="$(tailscale ip -4 2>/dev/null | head -n1 || true)"
      if [ -n "$ts_ip" ]; then break; fi
      sleep 5
    done
    test -n "$ts_ip"
    printf '%s\n' "$ts_ip" > /var/lib/crabbox/tailscale-ipv4
    printf '%s\n' ` + shellQuote(hostname) + ` > /var/lib/crabbox/tailscale-hostname
    if [ -n ` + shellQuote(exitNode) + ` ]; then
      printf '%s\n' ` + shellQuote(exitNode) + ` > /var/lib/crabbox/tailscale-exit-node
      printf '%s\n' ` + shellQuote(fmt.Sprint(cfg.Tailscale.ExitNodeAllowLANAccess)) + ` > /var/lib/crabbox/tailscale-exit-node-allow-lan-access
    fi
    if tailscale status --json >/var/lib/crabbox/tailscale-status.json 2>/dev/null; then
      jq -r '.Self.DNSName // empty' /var/lib/crabbox/tailscale-status.json > /var/lib/crabbox/tailscale-fqdn || true
    fi
    chown ` + sshUserChown + ` /var/lib/crabbox/tailscale-* || true
    chmod 0640 /var/lib/crabbox/tailscale-* || true`
	if pond := normalizePondName(cfg.Pond); pond != "" {
		tailscaleUpScript += "\n" + cloudInitPondHostsBootstrap(cfg.Pond)
	}
	return tailscaleUpScript
}

// cloudInitPondHostsBootstrap installs /usr/local/bin/crabbox-pond-hosts and a
// systemd timer that rewrites /etc/hosts.cbx plus a managed /etc/hosts block
// every 30s with one entry per pond peer reachable on the local tailnet. Peers
// are discovered purely from the box-local `tailscale status --json` output
// filtered by the pond ACL tag, so the broker never sees a Tailscale
// credential. Each peer renders as `<tailnet-ipv4> <slug>.cbx` where `<slug>`
// is the suffix of the `crabbox-<slug>` hostname template every
// Tailscale-capable provider already uses.
func cloudInitPondHostsBootstrap(pond string) string {
	tag := pondTailscaleTag(localCoordinatorOwner(), pond)
	if tag == "" {
		return ""
	}
	hostsFile := shellQuote(pondHostsFile)
	tagLiteral := shellQuote(tag)
	systemHostsFile := shellQuote("/etc/hosts")
	return `    install -m 0644 /dev/null ` + hostsFile + ` || true
    cat >/usr/local/bin/crabbox-pond-hosts <<'PONDHOSTS'
#!/bin/sh
set -eu
TAG="$1"
OUT="$2"
SYSTEM_HOSTS="${3:-/etc/hosts}"
TMP="$(mktemp)"
trap 'rm -f "$TMP" "$TMP".raw "$TMP".hosts' EXIT
if ! tailscale status --json >"$TMP".raw 2>/dev/null; then
  exit 0
fi
jq -r --arg tag "$TAG" '
  [(.Peer // {}) | to_entries[] | .value]
  | map(select((.Tags // []) | index($tag)))
  | map({ ip: ((.TailscaleIPs // [])[0] // ""), host: ((.HostName // "") | sub("^crabbox-"; "")) })
  | map(select(.ip != "" and .host != ""))
  | unique_by(.ip)
  | .[]
  | "\(.ip) \(.host).cbx"
' "$TMP".raw > "$TMP"
printf '# managed by crabbox-pond-hosts; do not edit\n' >"$OUT".new
cat "$TMP" >>"$OUT".new
mv "$OUT".new "$OUT"
chmod 0644 "$OUT"
BEGIN="# crabbox pond hosts begin"
END="# crabbox pond hosts end"
if [ -f "$SYSTEM_HOSTS" ]; then
  awk -v begin="$BEGIN" -v end="$END" '
    $0 == begin { skip = 1; next }
    $0 == end { skip = 0; next }
    !skip { print }
  ' "$SYSTEM_HOSTS" >"$TMP".hosts
else
  : >"$TMP".hosts
fi
{
  cat "$TMP".hosts
  printf '%s\n' "$BEGIN"
  cat "$TMP"
  printf '%s\n' "$END"
} >"$SYSTEM_HOSTS".new
mv "$SYSTEM_HOSTS".new "$SYSTEM_HOSTS"
chmod 0644 "$SYSTEM_HOSTS"
PONDHOSTS
    chmod 0755 /usr/local/bin/crabbox-pond-hosts
    cat >/etc/systemd/system/crabbox-pond-hosts.service <<'PONDUNIT'
[Unit]
Description=Refresh Crabbox pond peer hostnames
After=tailscaled.service network-online.target
Wants=network-online.target

[Service]
Type=oneshot
ExecStart=/usr/local/bin/crabbox-pond-hosts ` + tag + ` ` + pondHostsFile + ` /etc/hosts
PONDUNIT
    cat >/etc/systemd/system/crabbox-pond-hosts.timer <<'PONDTIMER'
[Unit]
Description=Refresh Crabbox pond hostnames every ` + pondHostsRefreshPeriod + `

[Timer]
OnBootSec=10s
OnUnitActiveSec=` + pondHostsRefreshPeriod + `
AccuracySec=2s
Unit=crabbox-pond-hosts.service

[Install]
WantedBy=timers.target
PONDTIMER
    systemctl daemon-reload
    systemctl enable --now crabbox-pond-hosts.timer
    /usr/local/bin/crabbox-pond-hosts ` + tagLiteral + ` ` + hostsFile + ` ` + systemHostsFile + ` || true
    test -f ` + hostsFile + `
`
}
