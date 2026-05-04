import { sshPorts, type LeaseConfig } from "./config";

const tightVNCMSIURL =
  "https://www.tightvnc.com/download/2.8.85/tightvnc-2.8.85-gpl-setup-64bit.msi";
const gitForWindowsSetupURL =
  "https://github.com/git-for-windows/git/releases/download/v2.52.0.windows.1/Git-2.52.0-64-bit.exe";
const openSSHWin64ZipURL =
  "https://github.com/PowerShell/Win32-OpenSSH/releases/download/v9.8.3.0p2-Preview/OpenSSH-Win64.zip";

export function awsUserData(config: LeaseConfig): string {
  if (config.target === "windows") {
    return windowsUserData(config);
  }
  if (config.target === "macos") {
    return macOSUserData(config);
  }
  return cloudInit(config);
}

export function cloudInit(config: LeaseConfig): string {
  const portLines = sshPorts(config)
    .map((port) => `      Port ${port}`)
    .join("\n");
  const readyChecks = optionalReadyChecks(config);
  const writeFiles = optionalWriteFiles(config);
  const bootstrap = optionalBootstrap(config);
  return `#cloud-config
package_update: false
package_upgrade: false
users:
  - name: ${config.sshUser}
    groups: sudo
    shell: /bin/bash
    sudo: ['ALL=(ALL) NOPASSWD:ALL']
    ssh_authorized_keys:
      - ${config.sshPublicKey}
write_files:
  - path: /etc/ssh/sshd_config.d/99-crabbox-port.conf
    permissions: '0644'
    content: |
${portLines}
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
      test -w ${config.workRoot}
${readyChecks}
${writeFiles}
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
${bootstrap}
    mkdir -p ${config.workRoot} /var/cache/crabbox/pnpm /var/cache/crabbox/npm
    chown -R ${config.sshUser}:${config.sshUser} ${config.workRoot} /var/cache/crabbox
    install -d /var/lib/crabbox
    touch /var/lib/crabbox/bootstrapped
    systemctl enable --now ssh
    systemctl restart ssh
    crabbox-ready
    BOOT
`;
}

export function windowsUserData(config: LeaseConfig): string {
  void config;
  return `version: 1.1
tasks:
- task: enableOpenSsh
`;
}

export function windowsBootstrapPowerShell(config: LeaseConfig): string {
  return `
$ErrorActionPreference = "Stop"
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
$user = ${psQuote(config.sshUser)}
$publicKey = ${psQuote(config.sshPublicKey)}
$workRoot = ${psQuote(config.workRoot)}
$sshPorts = ${windowsSSHPortsPowerShell(config)}
$vncPasswordPath = "C:\\ProgramData\\crabbox\\vnc.password"
$windowsUsernamePath = "C:\\ProgramData\\crabbox\\windows.username"
$windowsPasswordPath = "C:\\ProgramData\\crabbox\\windows.password"
$setupCompletePath = "C:\\ProgramData\\crabbox\\setup-complete"
$openSSHZip = "$env:TEMP\\OpenSSH-Win64.zip"
$gitInstaller = "$env:TEMP\\Git-2.52.0-64-bit.exe"
$tightVNCInstaller = "$env:TEMP\\tightvnc-2.8.85-gpl-setup-64bit.msi"
New-Item -ItemType Directory -Force -Path "C:\\ProgramData\\crabbox", $workRoot | Out-Null
if (-not (Test-Path -LiteralPath $vncPasswordPath)) {
  New-CrabboxPassword | Set-Content -NoNewline -Encoding ASCII -Path $vncPasswordPath
}
$userPassword = Get-Content -Raw -Path $vncPasswordPath
if ($userPassword.Length -lt 12 -or $userPassword -notmatch '[A-Z]' -or $userPassword -notmatch '[a-z]' -or $userPassword -notmatch '[0-9]' -or $userPassword -notmatch '[^A-Za-z0-9]') {
  $userPassword = New-CrabboxPassword
  Set-Content -NoNewline -Encoding ASCII -Path $vncPasswordPath -Value $userPassword
}
$secure = ConvertTo-SecureString $userPassword -AsPlainText -Force
if (-not (Get-LocalUser -Name $user -ErrorAction SilentlyContinue)) {
  New-LocalUser -Name $user -Password $secure -PasswordNeverExpires -AccountNeverExpires | Out-Null
} else {
  Set-LocalUser -Name $user -Password $secure -PasswordNeverExpires $true
}
Add-LocalGroupMember -Group "Administrators" -Member $user -ErrorAction SilentlyContinue
Set-Content -NoNewline -Encoding ASCII -Path $windowsUsernamePath -Value $user
Set-Content -NoNewline -Encoding ASCII -Path $windowsPasswordPath -Value $userPassword
$userSID = (Get-LocalUser -Name $user).SID.Value
$userSSHDir = Join-Path (Join-Path "C:\\Users" $user) ".ssh"
$userAuthorizedKeys = Join-Path $userSSHDir "authorized_keys"
New-Item -ItemType Directory -Force -Path $userSSHDir | Out-Null
Set-Content -Encoding ASCII -Path $userAuthorizedKeys -Value $publicKey
icacls.exe $userAuthorizedKeys /inheritance:r /grant "*\${userSID}:F" /grant "*S-1-5-32-544:F" /grant "*S-1-5-18:F" | Out-Null
if (-not (Get-Service -Name sshd -ErrorAction SilentlyContinue)) {
  Retry { Invoke-WebRequest -Uri ${psQuote(openSSHWin64ZipURL)} -OutFile $openSSHZip -UseBasicParsing }
  Remove-Item -Recurse -Force "C:\\Program Files\\OpenSSH" -ErrorAction SilentlyContinue
  Expand-Archive -LiteralPath $openSSHZip -DestinationPath "C:\\Program Files" -Force
  if (Test-Path -LiteralPath "C:\\Program Files\\OpenSSH-Win64") {
    Rename-Item -LiteralPath "C:\\Program Files\\OpenSSH-Win64" -NewName "OpenSSH" -Force
  }
  & "C:\\Program Files\\OpenSSH\\install-sshd.ps1"
}
New-Item -ItemType Directory -Force -Path "$env:ProgramData\\ssh" | Out-Null
Set-Content -Encoding ASCII -Path "$env:ProgramData\\ssh\\administrators_authorized_keys" -Value $publicKey
icacls.exe "$env:ProgramData\\ssh\\administrators_authorized_keys" /inheritance:r /grant "*S-1-5-32-544:F" /grant "*S-1-5-18:F" | Out-Null
$sshdConfigPath = "$env:ProgramData\\ssh\\sshd_config"
$sshdConfig = ""
if (Test-Path -LiteralPath $sshdConfigPath) {
  $sshdConfig = Get-Content -Raw -LiteralPath $sshdConfigPath
}
$globalLines = @()
$matchLines = @()
$inMatch = $false
foreach ($line in ($sshdConfig -split "\\r?\\n")) {
  if ($line -match '^\\s*Match\\s+') { $inMatch = $true }
  if (-not $inMatch -and $line -match '^\\s*Port\\s+\\d+\\s*$') { continue }
  if ($inMatch) { $matchLines += $line } else { $globalLines += $line }
}
foreach ($port in $sshPorts) { $globalLines += "Port $port" }
Set-Content -Encoding ASCII -LiteralPath $sshdConfigPath -Value (($globalLines + $matchLines) -join [Environment]::NewLine)
foreach ($port in $sshPorts) {
  $ruleName = "crabbox-sshd-$port"
  if (-not (Get-NetFirewallRule -Name $ruleName -ErrorAction SilentlyContinue)) {
    New-NetFirewallRule -Name $ruleName -DisplayName "Crabbox OpenSSH $port" -Enabled True -Direction Inbound -Protocol TCP -Action Allow -LocalPort $port | Out-Null
  }
}
Set-Service -Name sshd -StartupType Automatic
Start-Service sshd
if (-not (Test-Path -LiteralPath "C:\\Program Files\\Git\\cmd\\git.exe")) {
  Retry { Invoke-WebRequest -Uri ${psQuote(gitForWindowsSetupURL)} -OutFile $gitInstaller -UseBasicParsing }
  Start-Process -FilePath $gitInstaller -ArgumentList "/VERYSILENT","/NORESTART","/NOCANCEL","/SP-" -Wait
}
$machinePath = [Environment]::GetEnvironmentVariable("Path", "Machine")
foreach ($path in @("C:\\Program Files\\OpenSSH", "C:\\Program Files\\Git\\cmd", "C:\\Program Files\\Git\\usr\\bin")) {
  if ($machinePath -notlike "*$path*") { $machinePath = "$machinePath;$path" }
  if ($env:Path -notlike "*$path*") { $env:Path = "$env:Path;$path" }
}
[Environment]::SetEnvironmentVariable("Path", $machinePath, "Machine")
if (-not (Test-Path -LiteralPath "C:\\Program Files\\TightVNC\\tvnserver.exe")) {
  Retry { Invoke-WebRequest -Uri ${psQuote(tightVNCMSIURL)} -OutFile $tightVNCInstaller -UseBasicParsing }
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
Get-Service -Name tvnserver -ErrorAction SilentlyContinue | Set-Service -StartupType Automatic
Start-Service -Name tvnserver -ErrorAction SilentlyContinue
$winlogon = "HKLM:\\SOFTWARE\\Microsoft\\Windows NT\\CurrentVersion\\Winlogon"
Set-ItemProperty -Path $winlogon -Name AutoAdminLogon -Value "1" -Type String
Set-ItemProperty -Path $winlogon -Name ForceAutoLogon -Value "1" -Type String
Set-ItemProperty -Path $winlogon -Name DefaultUserName -Value $user -Type String
Set-ItemProperty -Path $winlogon -Name DefaultPassword -Value $userPassword -Type String
Restart-Service sshd
if (-not (Test-Path -LiteralPath $setupCompletePath)) {
  Set-Content -NoNewline -Encoding ASCII -Path $setupCompletePath -Value (Get-Date).ToString("o")
  Restart-Computer -Force
}
`;
}

function windowsSSHPortsPowerShell(config: LeaseConfig): string {
  return `@(${sshPorts(config)
    .map((port) => psQuote(port))
    .join(", ")})`;
}

export function macOSUserData(config: LeaseConfig): string {
  return `#!/bin/bash
set -euxo pipefail
install -d -m 0755 ${shellQuote(config.workRoot)} /var/db/crabbox
chown -R ${shellQuote(config.sshUser)}:staff ${shellQuote(config.workRoot)}
if [ ! -s /var/db/crabbox/vnc.password ]; then
  pw="$(LC_ALL=C tr -dc 'A-Za-z0-9' </dev/urandom | head -c 16)"
  printf '%s\\n' "$pw" >/var/db/crabbox/vnc.password
  dscl . -passwd /Users/${shellQuote(config.sshUser)} "$pw"
fi
chmod 0600 /var/db/crabbox/vnc.password
launchctl enable system/com.apple.screensharing || true
launchctl load -w /System/Library/LaunchDaemons/com.apple.screensharing.plist || true
cat >/usr/local/bin/crabbox-ready <<'READY'
#!/bin/bash
set -euo pipefail
rsync --version >/dev/null
curl --version >/dev/null
test -w ${shellQuote(config.workRoot)}
nc -z 127.0.0.1 5900
READY
chmod 0755 /usr/local/bin/crabbox-ready
/usr/local/bin/crabbox-ready
`;
}

function optionalReadyChecks(config: LeaseConfig): string {
  const lines: string[] = [];
  if (config.desktop) {
    lines.push(
      "      systemctl is-active --quiet crabbox-xvfb.service",
      "      systemctl is-active --quiet crabbox-desktop.service",
      "      systemctl is-active --quiet crabbox-desktop-session.service",
      "      systemctl is-active --quiet crabbox-x11vnc.service",
      "      ss -ltn | grep -q '127.0.0.1:5900'",
    );
  }
  if (config.browser) {
    lines.push(
      "      test -s /var/lib/crabbox/browser.env",
      "      . /var/lib/crabbox/browser.env",
      '      test -x "$BROWSER"',
      '      "$BROWSER" --version >/dev/null',
    );
  }
  return lines.join("\n");
}

function optionalWriteFiles(config: LeaseConfig): string {
  if (!config.desktop) {
    return "";
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
      export DISPLAY="\${DISPLAY:-:99}"
      if command -v xsetroot >/dev/null 2>&1; then
        xsetroot -solid '#20242b' || true
      fi
      if command -v xterm >/dev/null 2>&1 && ! pgrep -u "$(id -u)" -f 'xterm -title Crabbox Desktop' >/dev/null 2>&1; then
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
`;
}

function optionalBootstrap(config: LeaseConfig): string {
  const parts: string[] = [];
  if (config.desktop) {
    parts.push(`    retry apt-get install -y --no-install-recommends xvfb xfce4 xfce4-terminal x11vnc xauth dbus-x11 x11-xserver-utils xterm scrot fonts-dejavu-core fonts-liberation iproute2 openssl
    install -d -m 0750 -o crabbox -g crabbox /var/lib/crabbox
    if [ ! -s /var/lib/crabbox/vnc.password ]; then
      (umask 077 && openssl rand -base64 18 > /var/lib/crabbox/vnc.password)
    fi
    x11vnc -storepasswd "$(cat /var/lib/crabbox/vnc.password)" /var/lib/crabbox/vnc.pass >/dev/null
    chown crabbox:crabbox /var/lib/crabbox/vnc.password /var/lib/crabbox/vnc.pass
    chmod 0600 /var/lib/crabbox/vnc.password /var/lib/crabbox/vnc.pass
    systemctl daemon-reload
    systemctl enable --now crabbox-xvfb.service crabbox-desktop.service crabbox-desktop-session.service crabbox-x11vnc.service`);
  }
  if (config.browser) {
    parts.push(`    retry apt-get install -y --no-install-recommends gnupg
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
      printf 'CHROME_BIN=%s\\nBROWSER=%s\\n' "$browser_path" "$browser_path" > /var/lib/crabbox/browser.env
      chown crabbox:crabbox /var/lib/crabbox/browser.env
      chmod 0644 /var/lib/crabbox/browser.env
    fi`);
  }
  return parts.join("\n");
}

function psQuote(value: string): string {
  return `'${value.replaceAll("'", "''")}'`;
}

function shellQuote(value: string): string {
  return `'${value.replaceAll("'", "'\\''")}'`;
}
