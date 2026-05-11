package cli

import (
	"fmt"
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
	workRoot := cfg.WorkRoot
	if workRoot == "" {
		workRoot = `C:\crabbox`
	}
	wslMode := cfg.WindowsMode == windowsModeWSL2
	return windowsBootstrapHeaderPowerShell(cfg, publicKey, workRoot) + `
$wslMode = $` + fmt.Sprint(wslMode) + `
$wslDistro = "Crabbox"
$wslRoot = "C:\ProgramData\crabbox\wsl\Crabbox"
$wslRootfs = "C:\ProgramData\crabbox\wsl\ubuntu-noble-wsl-amd64.rootfs.tar.gz"
$wslRootfsDownload = "$wslRootfs.download"
$wslRootfsMinBytes = 100 * 1024 * 1024
$wslSetup = "C:\ProgramData\crabbox\wsl\linux-setup.sh"
$wslFeaturesMarker = "C:\ProgramData\crabbox\wsl-features-rebooted"
$wslKernelMarker = "C:\ProgramData\crabbox\wsl-kernel-rebooted"
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
function Restart-CrabboxBootstrap($MarkerPath) {
  Set-Content -NoNewline -Encoding ASCII -Path $MarkerPath -Value (Get-Date).ToString("o")
  Restart-Computer -Force
  exit 0
}
function Initialize-CrabboxWSL2 {
  if (-not $wslMode) { return }
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
cat >/usr/local/bin/crabbox-ready <<'READY'
#!/usr/bin/env bash
set -euo pipefail
git --version >/dev/null
rsync --version >/dev/null
curl --version >/dev/null
jq --version >/dev/null
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
}
` + windowsBootstrapCorePowerShell() + `
Initialize-CrabboxWSL2
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
Start-Process -FilePath $exe -ArgumentList "-run" -WindowStyle Minimized
'@
Set-Content -Encoding UTF8 -LiteralPath $userVNCStartupPath -Value $userVNCStartup
New-Item -ItemType Directory -Force -Path (Split-Path -Parent $userVNCStartupCommandPath) | Out-Null
Set-Content -Encoding ASCII -LiteralPath $userVNCStartupCommandPath -Value ('@echo off' + [Environment]::NewLine + 'powershell.exe -NoProfile -WindowStyle Hidden -ExecutionPolicy Bypass -File "' + $userVNCStartupPath + '"' + [Environment]::NewLine)
$startupTask = "CrabboxUserVNC"
cmd.exe /c "schtasks.exe /Delete /TN $startupTask /F 2>NUL" | Out-Null
schtasks.exe /Create /TN $startupTask /SC ONCE /ST ((Get-Date).AddMinutes(1).ToString("HH:mm")) /TR "powershell.exe -NoProfile -WindowStyle Hidden -ExecutionPolicy Bypass -File $userVNCStartupPath" /RU $user /IT /F | Out-Null
Get-Service -Name tvnserver -ErrorAction SilentlyContinue | Set-Service -StartupType Manual
Start-Service -Name tvnserver -ErrorAction SilentlyContinue
$winlogon = "HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion\Winlogon"
Set-ItemProperty -Path $winlogon -Name AutoAdminLogon -Value "1" -Type String
Set-ItemProperty -Path $winlogon -Name ForceAutoLogon -Value "1" -Type String
Set-ItemProperty -Path $winlogon -Name DefaultUserName -Value $user -Type String
Set-ItemProperty -Path $winlogon -Name DefaultPassword -Value $userPassword -Type String
Restart-Service sshd
if (-not (Test-Path -LiteralPath $setupCompletePath)) {
  Set-Content -NoNewline -Encoding ASCII -Path $setupCompletePath -Value (Get-Date).ToString("o")
  Restart-Computer -Force
}
`
}

func azureWindowsBootstrapPowerShell(cfg Config, publicKey string) string {
	workRoot := cfg.WorkRoot
	if workRoot == "" {
		workRoot = defaultWindowsWorkRoot
	}
	return windowsBootstrapHeaderPowerShell(cfg, publicKey, workRoot) + `
$passwordPath = Join-Path $base "windows.password"
$usernamePath = Join-Path $base "windows.username"
$passwordMirrorPath = $null
$enforceKeyAuth = $true
` + windowsBootstrapCorePowerShell() + `
Restart-Service sshd -Force
git --version | Out-Null
tar --version | Out-Null
Set-Content -NoNewline -Encoding ASCII -Path $setupCompletePath -Value (Get-Date).ToString("o")
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

func macOSUserData(cfg Config, _ string) string {
	workRoot := cfg.WorkRoot
	if workRoot == "" {
		workRoot = defaultMacOSWorkRoot
	}
	return `#!/bin/bash
set -euxo pipefail
install -d -m 0755 ` + shellQuote(workRoot) + ` /var/db/crabbox
chown -R ` + shellQuote(cfg.SSHUser) + `:staff ` + shellQuote(workRoot) + `
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
cat >/usr/local/bin/crabbox-ready <<'READY'
#!/bin/bash
set -euo pipefail
rsync --version >/dev/null
curl --version >/dev/null
test -w ` + shellQuote(workRoot) + `
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
		b.WriteString("      systemctl is-active --quiet crabbox-xvfb.service\n")
		b.WriteString("      systemctl is-active --quiet crabbox-desktop.service\n")
		b.WriteString("      systemctl is-active --quiet crabbox-desktop-session.service\n")
		b.WriteString("      systemctl is-active --quiet crabbox-x11vnc.service\n")
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
	if !cfg.Desktop {
		return ""
	}
	return `  - path: /etc/systemd/system/crabbox-xvfb.service
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
      if command -v xsetroot >/dev/null 2>&1; then
        xsetroot -solid '#20242b' || true
      fi
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
      ExecStart=/usr/bin/x11vnc -display :99 -localhost -rfbport 5900 -forever -shared -rfbauth /var/lib/crabbox/vnc.pass
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
	if cfg.Desktop {
		parts = append(parts, `    retry apt-get install -y --no-install-recommends xvfb xfce4-session xfwm4 xfce4-panel xfdesktop4 xfce4-terminal xfconf xfce4-settings x11vnc xauth dbus-x11 x11-xserver-utils xterm scrot ffmpeg xdotool wmctrl xclip xsel fonts-dejavu-core fonts-liberation iproute2 openssl
    install -d -m 0750 -o crabbox -g crabbox /var/lib/crabbox
    if [ ! -s /var/lib/crabbox/vnc.password ]; then
      (umask 077 && openssl rand -base64 18 > /var/lib/crabbox/vnc.password)
    fi
    x11vnc -storepasswd "$(cat /var/lib/crabbox/vnc.password)" /var/lib/crabbox/vnc.pass >/dev/null
    chown crabbox:crabbox /var/lib/crabbox/vnc.password /var/lib/crabbox/vnc.pass
    chmod 0600 /var/lib/crabbox/vnc.password /var/lib/crabbox/vnc.pass
    systemctl daemon-reload
    systemctl enable --now crabbox-xvfb.service crabbox-desktop.service crabbox-desktop-session.service crabbox-x11vnc.service`)
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
      printf '%s\n' '#!/bin/sh' "exec \"$browser_path\" --no-first-run --no-default-browser-check --disable-default-apps --window-size=1500,900 --window-position=80,80 \"\$@\"" > "$browser_wrapper"
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
	return `    retry sh -c 'curl -fsSL https://tailscale.com/install.sh | sh'
    systemctl enable --now tailscaled || service tailscaled start || true
    install -d -m 0750 -o ` + sshUserOwner + ` -g ` + sshUserGroup + ` /var/lib/crabbox
    set +x
    TS_AUTHKEY=` + shellQuote(authKey) + `
    tailscale up ` + strings.Join(tailscaleUpArgs, " ") + `
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
}
