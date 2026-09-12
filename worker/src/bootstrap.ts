import {
  sharedLinuxSSHRestart,
  sharedLinuxOptionalPackages,
  sharedLinuxNodeInstall,
  sharedGnomeDesktopTheme,
  sharedXfceDesktopTheme,
  sharedXfceDesktopSession,
  sharedXfceSessionEnvironment,
  sharedWslTruffleHogInstall,
  sharedWindowsHeader,
  sharedWindowsRuntime,
  sharedWindowsRuntimeGate,
  sharedWindowsNodeInstall,
  sharedWindowsDetachInstall,
  sharedWindowsCore,
  sharedWindowsDesktopPrelude,
  sharedWindowsNativePrelude,
  sharedWindowsFinalize,
  sharedWindowsDesktop,
  sharedMacOS,
  sharedMacOSSSHSession,
  sharedMacOSNodeInstall,
  sharedCodeServerInstall,
  sharedTailscalePinnedInstall,
  sharedTailscalePackageInstall,
  ubuntuWSLRootFSURL,
  ubuntuWSLRootFSSHA256,
  defaultTailscaleVersion,
  googleLinuxSigningKeyFingerprint,
} from "./bootstrap.generated";
import { sshPorts, type LeaseConfig } from "./config";
import { linuxMinimalReadinessBootstrap } from "./linux-readiness.generated";

export function awsUserData(config: LeaseConfig): string {
  if (config.target === "windows") {
    return windowsUserData(config);
  }
  if (config.target === "macos") {
    return macOSUserData(config);
  }
  // Custom images retain their sources. Keep security separate; cloud-init otherwise uses primary.
  const aptConfig =
    config.provider === "aws" &&
    config.target === "linux" &&
    !config.awsPrivate &&
    config.os === "ubuntu:26.04" &&
    config.architecture === "amd64" &&
    config.selectedImage?.source === "stock"
      ? `apt:
  primary:
    - arches: [amd64]
      uri: https://archive.ubuntu.com/ubuntu/
  security:
    - arches: [amd64]
      uri: http://security.ubuntu.com/ubuntu/
`
      : "";
  return cloudInit(config, "", aptConfig);
}

export async function awsRunInstancesUserData(config: LeaseConfig): Promise<string> {
  const userData = awsUserData(config);
  const bytes =
    config.target === "linux" ? await gzip(userData) : new TextEncoder().encode(userData);
  return base64Encode(bytes);
}

async function gzip(value: string): Promise<Uint8Array> {
  const stream = new Blob([value]).stream().pipeThrough(new CompressionStream("gzip"));
  return new Uint8Array(await new Response(stream).arrayBuffer());
}

function base64Encode(bytes: Uint8Array): string {
  const chunks: string[] = [];
  const chunkSize = 0x8000;
  for (let offset = 0; offset < bytes.length; offset += chunkSize) {
    chunks.push(String.fromCharCode(...bytes.subarray(offset, offset + chunkSize)));
  }
  return btoa(chunks.join(""));
}

export function cloudInit(
  config: LeaseConfig,
  additionalBootstrap = "",
  additionalCloudConfig = "",
): string {
  if (config.awsPrivate) {
    return privateAWSCloudInit(config);
  }
  const portLines = sshPorts(config)
    .map((port) => `      Port ${port}`)
    .join("\n");
  const readyChecks = optionalReadyChecks(config);
  const sshHostKeys = optionalSSHHostKeys(config);
  const writeFiles = optionalWriteFiles(config);
  const bootstrap = [optionalBootstrap(config), additionalBootstrap].filter(Boolean).join("\n");
  const readinessBootstrap = indentRuncmdScript(linuxMinimalReadinessBootstrap);
  const sshRestart = indentRuncmdScript(sharedLinuxSSHRestart());
  return `#cloud-config
package_update: false
package_upgrade: false
${additionalCloudConfig}users:
  - name: ${config.sshUser}
    groups: sudo
    shell: /bin/bash
    sudo: ['ALL=(ALL) NOPASSWD:ALL']
    ssh_authorized_keys:
      - ${config.sshPublicKey}
${sshHostKeys}
write_files:
  - path: /etc/ssh/sshd_config.d/99-crabbox-port.conf
    permissions: '0644'
    content: |
${portLines}
      PasswordAuthentication no
  - path: /etc/systemd/system/crabbox-workspace-ready.service
    permissions: '0644'
    content: |
      [Unit]
      Description=Crabbox workspace per-boot readiness
      After=cloud-final.service
      ConditionPathExists=/var/lib/crabbox/bootstrapped

      [Service]
      Type=oneshot
      ExecStart=/usr/local/bin/crabbox-ready
      ExecStart=/usr/bin/install -d /run/crabbox
      ExecStart=/usr/bin/touch /run/crabbox/workspace-ready

      [Install]
      WantedBy=cloud-final.service
  - path: /usr/local/bin/crabbox-ready
    permissions: '0755'
    content: |
      #!/usr/bin/env bash
      set -euo pipefail
      git --version
      rsync --version >/dev/null
      curl --version >/dev/null
      jq --version >/dev/null
      tmux -V >/dev/null
      flock --version >/dev/null
      test -f /var/lib/crabbox/bootstrapped
      test -w ${config.workRoot}
${readyChecks}
${writeFiles}
runcmd:
  - |
    bash -euxo pipefail <<'BOOT'
    export DEBIAN_FRONTEND=noninteractive
${sshRestart}
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
${readinessBootstrap}
    mkdir -p ${config.workRoot} /var/cache/crabbox/pnpm /var/cache/crabbox/npm
    chown -R ${config.sshUser}:${config.sshUser} ${config.workRoot} /var/cache/crabbox
    install -d /var/lib/crabbox
    systemctl enable ssh || true
${sshRestart}
${bootstrap}
    systemctl daemon-reload
    systemctl enable crabbox-workspace-ready.service
    systemctl start --no-block crabbox-workspace-ready.service
    touch /var/lib/crabbox/bootstrapped
    crabbox-ready
    BOOT
`;
}

function privateAWSCloudInit(config: LeaseConfig): string {
  return `#cloud-config
package_update: false
package_upgrade: false
ssh_pwauth: false
disable_root: true
users:
  - name: ${config.sshUser}
    shell: /bin/bash
    lock_passwd: true
write_files:
  - path: /usr/local/bin/crabbox-ready
    permissions: '0755'
    content: |
      #!/usr/bin/env bash
      set -euo pipefail
      git --version
      curl --version >/dev/null
      jq --version >/dev/null
      test -d ${config.workRoot}/workspaces
      test ! -L ${config.workRoot}
      test ! -L ${config.workRoot}/workspaces
      if systemctl list-unit-files amazon-ssm-agent.service >/dev/null 2>&1; then
        systemctl is-active --quiet amazon-ssm-agent.service
      else
        systemctl is-active --quiet snap.amazon-ssm-agent.amazon-ssm-agent.service
      fi
      test -f /var/lib/crabbox/bootstrapped
runcmd:
  - |
    bash -euxo pipefail <<'BOOT'
    export DEBIAN_FRONTEND=noninteractive
    if grep -RIl 'http://' /etc/apt 2>/dev/null | grep -q .; then
      grep -RIl 'http://' /etc/apt | xargs -r sed -i 's|http://|https://|g'
    fi
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
    systemctl disable --now ssh.service ssh.socket 2>/dev/null || true
    systemctl mask ssh.service ssh.socket 2>/dev/null || true
    if systemctl list-unit-files amazon-ssm-agent.service >/dev/null 2>&1; then
      systemctl enable --now amazon-ssm-agent.service
    elif systemctl list-unit-files snap.amazon-ssm-agent.amazon-ssm-agent.service >/dev/null 2>&1; then
      systemctl enable --now snap.amazon-ssm-agent.amazon-ssm-agent.service
    else
      echo 'stock AWS Ubuntu image is missing the preinstalled SSM Agent' >&2
      exit 1
    fi
    retry apt-get update
    retry apt-get install -y --no-install-recommends ca-certificates curl git jq util-linux
    install -d -m 0755 -o root -g root ${config.workRoot} ${config.workRoot}/workspaces
    install -d -m 0755 /var/lib/crabbox
    touch /var/lib/crabbox/bootstrapped
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

function windowsBootstrapHeaderPowerShell(config: LeaseConfig): string {
  let script = sharedWindowsHeader(
    config.sshUser,
    config.sshPublicKey,
    config.workRoot,
    sshPorts(config),
  );
  // An omitted mode retains the native default; WSL2 owns a separate Linux runtime.
  if (config.windowsMode !== "wsl2") {
    script +=
      sharedWindowsRuntime() +
      sharedWindowsRuntimeGate() +
      sharedWindowsNodeInstall() +
      sharedWindowsDetachInstall();
  }
  return script;
}

export function windowsBootstrapPowerShell(config: LeaseConfig): string {
  const script =
    windowsBootstrapHeaderPowerShell({ ...config, workRoot: windowsBootstrapWorkRoot(config) }) +
    windowsManagedCorePreludePowerShell(config) +
    sharedWindowsCore();
  if (config.windowsMode === "wsl2") {
    return script + windowsWSL2BootstrapPowerShell(config);
  }
  if (config.desktop) {
    return script + sharedWindowsDesktop();
  }
  return script + sharedWindowsFinalize();
}

function windowsBootstrapWorkRoot(config: LeaseConfig): string {
  if (config.windowsMode === "wsl2") {
    return "C:\\crabbox";
  }
  return config.workRoot || "C:\\crabbox";
}

function windowsWSLWorkRoot(config: LeaseConfig): string {
  return config.workRoot || "/work/crabbox";
}

function windowsManagedCorePreludePowerShell(config: LeaseConfig): string {
  if (config.windowsMode === "normal" && config.desktop) {
    return sharedWindowsDesktopPrelude();
  }
  return sharedWindowsNativePrelude();
}

function windowsWSL2BootstrapPowerShell(config: LeaseConfig): string {
  const workRoot = windowsWSLWorkRoot(config);
  return `
	$wslDistro = "Crabbox"
	$wslRoot = "C:\\ProgramData\\crabbox\\wsl\\Crabbox"
	$wslRootfs = "C:\\ProgramData\\crabbox\\wsl\\ubuntu-noble-wsl-amd64.rootfs.tar.gz"
	$wslRootfsDownload = "$wslRootfs.download"
	$wslRootfsMinBytes = 100 * 1024 * 1024
	$wslSetup = "C:\\ProgramData\\crabbox\\wsl\\linux-setup.sh"
	$wslFeaturesMarker = "C:\\ProgramData\\crabbox\\wsl-features-rebooted"
	$wslKernelMarker = "C:\\ProgramData\\crabbox\\wsl-kernel-rebooted"
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
	        $head = Invoke-WebRequest -Uri ${psQuote(ubuntuWSLRootFSURL)} -Method Head -UseBasicParsing
	        if ($head.Headers.ContainsKey("Content-Length")) {
	          [void][Int64]::TryParse(($head.Headers["Content-Length"] | Select-Object -First 1), [ref]$expectedLength)
	        }
	      } catch {
	        $expectedLength = 0
	      }
	      if (Get-Command curl.exe -ErrorAction SilentlyContinue) {
	        & curl.exe -fL --retry 8 --retry-delay 5 --connect-timeout 30 --speed-time 30 --speed-limit 1024 -o $wslRootfsDownload ${psQuote(ubuntuWSLRootFSURL)}
	        if ($LASTEXITCODE -ne 0) { throw "download WSL rootfs failed with exit $LASTEXITCODE" }
	      } else {
	        Invoke-WebRequest -Uri ${psQuote(ubuntuWSLRootFSURL)} -OutFile $wslRootfsDownload -UseBasicParsing
	      }
	      $actualLength = (Get-Item -LiteralPath $wslRootfsDownload).Length
	      if ($actualLength -lt $wslRootfsMinBytes) { throw "downloaded WSL rootfs is incomplete" }
	      if ($expectedLength -gt 0 -and $actualLength -ne $expectedLength) {
	        throw "downloaded WSL rootfs is incomplete: $actualLength of $expectedLength bytes"
	      }
	      Assert-CrabboxFileSHA256 $wslRootfsDownload ${psQuote(ubuntuWSLRootFSSHA256)}
	    }
	    Move-Item -Force -LiteralPath $wslRootfsDownload -Destination $wslRootfs
	  }
	  Assert-CrabboxFileSHA256 $wslRootfs ${psQuote(ubuntuWSLRootFSSHA256)}
	  wsl.exe --import $wslDistro $wslRoot $wslRootfs --version 2 | Out-Host
	  if ($LASTEXITCODE -ne 0) { throw "wsl --import failed with exit $LASTEXITCODE" }
	  wsl.exe --set-default $wslDistro | Out-Host
	  if ($LASTEXITCODE -ne 0) { throw "wsl --set-default failed with exit $LASTEXITCODE" }
	}
	$linuxSetup = @'
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
mkdir -p ${shellQuote(workRoot)} /var/cache/crabbox/pnpm /var/cache/crabbox/npm /var/lib/crabbox
cat >/etc/apt/apt.conf.d/80-crabbox-retries <<'APT'
Acquire::Retries "8";
Acquire::http::Timeout "30";
Acquire::https::Timeout "30";
APT
rm -rf /var/lib/apt/lists/*
apt-get update
apt-get install -y --no-install-recommends ca-certificates curl git rsync jq tmux
${sharedLinuxNodeInstall()}${sharedWslTruffleHogInstall()}if [ -d /proc/sys/fs/binfmt_misc ]; then
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
trufflehog --no-update --version >/dev/null
node --version >/dev/null
npm --version >/dev/null
test -e /proc/sys/fs/binfmt_misc/WSLInterop
test -w ${shellQuote(workRoot)}
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
	`;
}

export function azureWindowsBootstrapPowerShell(config: LeaseConfig): string {
  const coreConfig = { ...config, workRoot: windowsBootstrapWorkRoot(config) };
  const setupComplete = config.desktop
    ? ""
    : `Set-Content -NoNewline -Encoding ASCII -Path $setupCompletePath -Value (Get-Date).ToString("o")`;
  return (
    windowsBootstrapHeaderPowerShell(coreConfig) +
    `
$passwordPath = Join-Path $base "windows.password"
$usernamePath = Join-Path $base "windows.username"
$passwordMirrorPath = $null
` +
    sharedWindowsCore() +
    `
Restart-Service sshd -Force
git --version | Out-Null
tar --version | Out-Null
${setupComplete}
`
  );
}

export function macOSUserData(config: LeaseConfig): string {
  return (
    "#!/bin/bash\nset -euo pipefail\n(\n" +
    sharedMacOSSSHSession() +
    ")\n(\n" +
    sharedMacOSNodeInstall() +
    ")\n" +
    sharedMacOS(config.sshUser, config.sshPublicKey, config.workRoot, sshPorts(config))
  );
}

function optionalReadyChecks(config: LeaseConfig): string {
  const lines: string[] = [];
  if (config.tailscale) {
    lines.push(
      "      test -s /var/lib/crabbox/tailscale-ipv4",
      "      grep -Eq '^100\\.' /var/lib/crabbox/tailscale-ipv4",
    );
  }
  if (config.desktop) {
    if (config.desktopEnv !== "xfce") {
      lines.push(
        "      systemctl is-active --quiet crabbox-desktop.service",
        "      systemctl is-active --quiet crabbox-wayvnc.service",
        "      ss -ltn | grep -q '127.0.0.1:5900'",
      );
    } else {
      lines.push(
        "      systemctl is-active --quiet crabbox-xvfb.service",
        "      systemctl is-active --quiet crabbox-desktop.service",
        "      ss -ltn | grep -q '127.0.0.1:5900'",
      );
    }
  }
  if (config.browser) {
    lines.push(
      "      test -s /var/lib/crabbox/browser.env",
      "      . /var/lib/crabbox/browser.env",
      '      test -x "$BROWSER"',
      '      "$BROWSER" --version >/dev/null',
    );
  }
  if (config.code) {
    lines.push(
      "      test -x /usr/local/bin/code-server",
      "      /usr/local/bin/code-server --version >/dev/null",
    );
  }
  return lines.join("\n");
}

function optionalSSHHostKeys(config: LeaseConfig): string {
  if (!config.sshHostPrivateKey || !config.sshHostPublicKey) {
    return "";
  }
  const privateKey = config.sshHostPrivateKey
    .trimEnd()
    .split("\n")
    .map((line) => `    ${line}`)
    .join("\n");
  return `ssh_keys:
  ed25519_private: |
${privateKey}
  ed25519_public: ${config.sshHostPublicKey.trim()}
`;
}

function optionalWriteFiles(config: LeaseConfig): string {
  if (!config.desktop) {
    return "";
  }
  if (config.desktopEnv !== "xfce") {
    const desktopEnv = config.desktopEnv;
    const displayEnv =
      desktopEnv === "gnome"
        ? "      DISPLAY=:0\n      GDK_BACKEND=x11\n      MOZ_ENABLE_WAYLAND=0\n"
        : "";
    return `  - path: /usr/local/bin/crabbox-start-wayland-desktop
    permissions: '0755'
    content: |
      #!/bin/sh
      set -eu
      runtime="\${XDG_RUNTIME_DIR:-/tmp/crabbox-runtime-$(id -u)}"
      install -d -m 0700 "$runtime"
      export XDG_RUNTIME_DIR="$runtime"
      export WLR_BACKENDS=headless
      export WLR_LIBINPUT_NO_DEVICES=1
      export WLR_RENDERER="\${WLR_RENDERER:-pixman}"
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
      ExecStart=/usr/local/bin/crabbox-start-wayland-desktop
      Restart=always

      [Install]
      WantedBy=multi-user.target
  - path: /usr/local/bin/crabbox-start-wayvnc
    permissions: '0755'
    content: |
      #!/bin/sh
      set -eu
      runtime="\${XDG_RUNTIME_DIR:-/tmp/crabbox-runtime-$(id -u)}"
      export XDG_RUNTIME_DIR="$runtime"
      for i in $(seq 1 60); do
        for socket in "$XDG_RUNTIME_DIR"/wayland-*; do
          [ -S "$socket" ] || continue
          export WAYLAND_DISPLAY="\${socket##*/}"
          cat >/var/lib/crabbox/desktop.env <<EOF
      CRABBOX_DESKTOP_ENV=${desktopEnv}
      XDG_RUNTIME_DIR=$XDG_RUNTIME_DIR
      WAYLAND_DISPLAY=$WAYLAND_DISPLAY
${displayEnv}      EOF
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
`;
  }
  return `  - path: /etc/systemd/system/crabbox-xvfb.service
    permissions: '0644'
    content: |
      [Unit]
      Description=Crabbox resize-capable TigerVNC display
      After=network.target

      [Service]
      User=crabbox
      ExecStart=/usr/bin/Xtigervnc :99 -geometry 1920x1080 -depth 24 -localhost yes -rfbport 5900 -SecurityTypes VncAuth -PasswordFile=/var/lib/crabbox/vnc.pass -AlwaysShared -AcceptSetDesktopSize -nolisten tcp -ac
      Restart=always

      [Install]
      WantedBy=multi-user.target
  - path: /usr/local/lib/crabbox/xfce-session.sh
    permissions: '0644'
    content: |
      ${sharedXfceSessionEnvironment().trimEnd().replaceAll("\n", "\n      ")}
  - path: /usr/local/bin/crabbox-configure-desktop-theme
    permissions: '0755'
    content: |
      ${sharedXfceDesktopTheme("coordinator").trimEnd().replaceAll("\n", "\n      ")}
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
      # Published CLI reset commands still request this legacy name.
      Alias=crabbox-desktop-session.service
  - path: /usr/local/bin/crabbox-desktop-session
    permissions: '0755'
    content: |
      ${sharedXfceDesktopSession().trimEnd().replaceAll("\n", "\n      ")}
  - path: /etc/xdg/autostart/crabbox-desktop.desktop
    permissions: '0644'
    content: |
      [Desktop Entry]
      Type=Application
      Name=Crabbox Desktop
      Exec=/usr/local/bin/crabbox-desktop-session
      OnlyShowIn=XFCE;
`;
}

function optionalBootstrap(config: LeaseConfig): string {
  const parts: string[] = [];
  if (config.desktop || config.browser) {
    parts.push(indentRuncmdScript(sharedLinuxOptionalPackages()));
  }
  if (config.tailscale) {
    parts.push(tailscaleBootstrap(config));
  }
  if (config.desktop && config.desktopEnv !== "xfce") {
    const gnome = config.desktopEnv === "gnome";
    const packages = gnome
      ? "labwc wayvnc swaybg librsvg2-common gnome-panel wlr-randr grim slurp wtype wl-clipboard dbus-user-session xwayland xdg-desktop-portal-wlr xdg-desktop-portal-gtk gnome-terminal nautilus gsettings-desktop-schemas adwaita-icon-theme fonts-dejavu-core fonts-liberation iproute2 openssl procps util-linux novnc websockify"
      : "labwc wayvnc foot grim slurp wtype wl-clipboard wlr-randr dbus-user-session xwayland xdg-desktop-portal-wlr fonts-dejavu-core fonts-liberation iproute2 openssl procps util-linux novnc websockify";
    const desktopEnvExtra = gnome
      ? "    DISPLAY=:0\n    GDK_BACKEND=x11\n    MOZ_ENABLE_WAYLAND=0\n"
      : "";
    const autostart = gnome
      ? `    wlr-randr --output HEADLESS-1 --custom-mode 1920x1080 >/tmp/crabbox-wlr-randr.log 2>&1 || true
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
    export DISPLAY="\${DISPLAY:-:0}"
    export GDK_BACKEND=x11
    export MOZ_ENABLE_WAYLAND=0
    wallpaper_file="$HOME/.config/crabbox/desktop-background-$theme.svg"
    if command -v swaybg >/dev/null 2>&1; then
      (
        if swaybg -i "$wallpaper_file" -m fill; then
          exit 0
        else
          status=$?
        fi
        [ "$status" -lt 128 ] || exit "$status"
        exec swaybg -c "#0d1117"
      ) </dev/null >/tmp/crabbox-swaybg.log 2>&1 &
    fi
    gnome-panel >/tmp/crabbox-gnome-panel.log 2>&1 &
    gnome-terminal -- bash -l >/tmp/crabbox-gnome-terminal.log 2>&1 &
    nautilus --new-window "$HOME" >/tmp/crabbox-nautilus.log 2>&1 &
`
      : `    wlr-randr --output HEADLESS-1 --custom-mode 1920x1080 >/tmp/crabbox-wlr-randr.log 2>&1 || true
    foot --title='Crabbox Desktop' >/tmp/crabbox-foot.log 2>&1 &
`;
    const labwcAutostart = `    cat >/home/crabbox/.config/labwc/autostart <<'AUTOSTART'
${autostart}    AUTOSTART
    chmod 0755 /home/crabbox/.config/labwc/autostart
`;
    const themeBootstrap = gnome
      ? indentRuncmdScript(`cat >/usr/local/bin/crabbox-configure-desktop-theme <<'THEME'
${sharedGnomeDesktopTheme()}THEME
chmod 0755 /usr/local/bin/crabbox-configure-desktop-theme
`)
      : "";
    const themeConfigure = gnome
      ? "    CRABBOX_DESKTOP_USER=crabbox /usr/local/bin/crabbox-configure-desktop-theme\n"
      : "";
    parts.push(`    crabbox_install_packages ${packages}
    install -d -m 0750 -o crabbox -g crabbox /var/lib/crabbox
    if [ ! -s /var/lib/crabbox/vnc.password ]; then
      (umask 077 && openssl rand -base64 18 > /var/lib/crabbox/vnc.password)
    fi
    chown crabbox:crabbox /var/lib/crabbox/vnc.password
    chmod 0600 /var/lib/crabbox/vnc.password
    crabbox_uid="$(id -u crabbox)"
    crabbox_runtime="/tmp/crabbox-runtime-$crabbox_uid"
    install -d -m 0700 -o crabbox -g crabbox "$crabbox_runtime"
    install -d -m 0700 -o crabbox -g crabbox /home/crabbox/.config/labwc /home/crabbox/.config/wayvnc
${themeBootstrap}
${labwcAutostart}
    cat >/home/crabbox/.config/wayvnc/config <<'WAYVNC'
    address=127.0.0.1
    port=5900
    enable_auth=false
    xkb_layout=us
    WAYVNC
    cat >/var/lib/crabbox/desktop.env <<EOF
    CRABBOX_DESKTOP_ENV=${config.desktopEnv}
    XDG_RUNTIME_DIR=$crabbox_runtime
    WAYLAND_DISPLAY=wayland-1
${desktopEnvExtra}
    EOF
    chown -R crabbox:crabbox /home/crabbox/.config /var/lib/crabbox/desktop.env
    chmod 0644 /var/lib/crabbox/desktop.env
${themeConfigure}    systemctl daemon-reload
    systemctl disable --now crabbox-xvfb.service crabbox-desktop-session.service crabbox-x11vnc.service 2>/dev/null || true
    systemctl enable crabbox-desktop.service crabbox-wayvnc.service
    systemctl restart crabbox-desktop.service crabbox-wayvnc.service`);
  } else if (config.desktop) {
    parts.push(`    crabbox_install_packages tigervnc-standalone-server tigervnc-tools xfce4-session xfwm4 xfce4-panel xfdesktop4 xfce4-terminal xfconf xfce4-settings xauth dbus-x11 x11-xserver-utils xterm scrot ffmpeg xdotool wmctrl xclip xsel fonts-dejavu-core fonts-liberation iproute2 openssl arc-theme util-linux novnc websockify
    install -d -m 0750 -o crabbox -g crabbox /var/lib/crabbox
    if [ ! -s /var/lib/crabbox/vnc.password ]; then
      (umask 077 && openssl rand -base64 18 > /var/lib/crabbox/vnc.password)
    fi
    head -c 8 /var/lib/crabbox/vnc.password | tigervncpasswd -f > /var/lib/crabbox/vnc.pass
    chown crabbox:crabbox /var/lib/crabbox/vnc.password /var/lib/crabbox/vnc.pass
    chmod 0600 /var/lib/crabbox/vnc.password /var/lib/crabbox/vnc.pass
    printf 'CRABBOX_DESKTOP_ENV=xfce\\nDISPLAY=:99\\n' >/var/lib/crabbox/desktop.env
    chown crabbox:crabbox /var/lib/crabbox/desktop.env
    chmod 0644 /var/lib/crabbox/desktop.env
    systemctl disable --now crabbox-desktop-session.service 2>/dev/null || true
    rm -f /etc/systemd/system/crabbox-desktop-session.service
    systemctl stop crabbox-desktop.service 2>/dev/null || true
    env -u DISPLAY CRABBOX_DESKTOP_USER=crabbox /usr/local/bin/crabbox-configure-desktop-theme
    systemctl daemon-reload
    systemctl disable --now crabbox-wayvnc.service crabbox-x11vnc.service 2>/dev/null || true
    systemctl enable crabbox-xvfb.service crabbox-desktop.service
    systemctl restart crabbox-xvfb.service crabbox-desktop.service`);
  }
  if (config.browser) {
    parts.push(`    crabbox_install_packages gnupg build-essential python3
    browser_path="$(crabbox_existing_browser || true)"
    if [ -z "$browser_path" ] && [ "$(dpkg --print-architecture)" = "amd64" ]; then
      install -d -m 0755 /etc/apt/keyrings
      google_key_tmp="$(mktemp -d /etc/apt/keyrings/google-linux.gpg.tmp.XXXXXX)"
      google_key_home="$google_key_tmp/gnupg"
      install -d -m 0700 "$google_key_home"
      google_key_ready=0
      if curl -fsSL https://dl.google.com/linux/linux_signing_key.pub > "$google_key_tmp/google.asc" &&
         GNUPGHOME="$google_key_home" gpg --batch --import "$google_key_tmp/google.asc" >/dev/null 2>&1; then
        google_key_fingerprint="$(GNUPGHOME="$google_key_home" gpg --batch --with-colons --fingerprint ${googleLinuxSigningKeyFingerprint} 2>/dev/null | awk -F: '$1 == "fpr" { print $10; exit }' || true)"
        if [ "$google_key_fingerprint" = "${googleLinuxSigningKeyFingerprint}" ] &&
           GNUPGHOME="$google_key_home" gpg --batch --export ${googleLinuxSigningKeyFingerprint} > "$google_key_tmp/google-linux.gpg" &&
           [ -s "$google_key_tmp/google-linux.gpg" ]; then
          chmod 0644 "$google_key_tmp/google-linux.gpg"
          mv -f "$google_key_tmp/google-linux.gpg" /etc/apt/keyrings/google-linux.gpg
          google_key_ready=1
        fi
      fi
      rm -rf "$google_key_tmp"
      if [ "$google_key_ready" = 1 ]; then
        install -d -m 0755 /etc/default
        google_defaults_tmp="$(mktemp /etc/default/google-chrome.tmp.XXXXXX)"
        if [ -f /etc/default/google-chrome ]; then
          awk '!/^[[:space:]]*repo_add_once=/ && !/^[[:space:]]*repo_reenable_on_distupgrade=/' /etc/default/google-chrome > "$google_defaults_tmp"
        fi
        printf '%s\\n' 'repo_add_once="false"' 'repo_reenable_on_distupgrade="false"' >> "$google_defaults_tmp"
        chmod 0644 "$google_defaults_tmp"
        mv -f "$google_defaults_tmp" /etc/default/google-chrome
        rm -f /etc/apt/sources.list.d/google-chrome.list /etc/apt/sources.list.d/google-chrome.sources
        echo "deb [arch=amd64 signed-by=/etc/apt/keyrings/google-linux.gpg] https://dl.google.com/linux/chrome/deb/ stable main" > /etc/apt/sources.list.d/crabbox-google-chrome.list
        if apt-get update && retry apt-get install -y --no-install-recommends google-chrome-stable; then
          rm -f /etc/apt/sources.list.d/google-chrome.list /etc/apt/sources.list.d/google-chrome.sources
          browser_path="$(command -v google-chrome || true)"
        else
          rm -f /etc/apt/sources.list.d/crabbox-google-chrome.list /etc/apt/sources.list.d/google-chrome.list /etc/apt/sources.list.d/google-chrome.sources
          retry apt-get update || true
        fi
      else
        echo "Google Linux signing key verification failed; trying Chromium fallback" >&2
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
      printf '%s\\n' '{"DefaultBrowserSettingEnabled":false,"MetricsReportingEnabled":false,"PromotionalTabsEnabled":false}' > /etc/opt/chrome/policies/managed/crabbox.json
      cp /etc/opt/chrome/policies/managed/crabbox.json /etc/chromium/policies/managed/crabbox.json
      if [ -f /var/lib/crabbox/desktop.env ] && grep -q '^CRABBOX_DESKTOP_ENV=gnome$' /var/lib/crabbox/desktop.env; then
        printf '%s\\n' '#!/bin/sh' 'if [ -f /var/lib/crabbox/desktop.env ]; then . /var/lib/crabbox/desktop.env; fi' 'export DISPLAY="\${DISPLAY:-:0}"' 'export XDG_RUNTIME_DIR WAYLAND_DISPLAY' 'export GDK_BACKEND=x11 MOZ_ENABLE_WAYLAND=0' 'profile="\${CRABBOX_BROWSER_PROFILE:-$HOME/.cache/crabbox/browser-profile}"' 'theme="$(cat "\${CRABBOX_DESKTOP_THEME_FILE:-$HOME/.config/crabbox/desktop-theme}" 2>/dev/null || printf dark)"' 'umask 077' 'mkdir -p "$profile"' 'chmod 700 "$profile"' 'if [ "$theme" = light ]; then' "  exec \\"$browser_path\\" --no-first-run --no-default-browser-check --disable-default-apps --hide-crash-restore-bubble --blink-settings=preferredColorScheme=1 --user-data-dir=\\"\\$profile\\" --ozone-platform=x11 --window-size=1500,900 --window-position=80,80 \\"\\$@\\"" 'fi' "exec \\"$browser_path\\" --no-first-run --no-default-browser-check --disable-default-apps --hide-crash-restore-bubble --force-dark-mode --enable-features=WebUIDarkMode --blink-settings=preferredColorScheme=2 --user-data-dir=\\"\\$profile\\" --ozone-platform=x11 --window-size=1500,900 --window-position=80,80 \\"\\$@\\"" > "$browser_wrapper"
      elif [ -f /var/lib/crabbox/desktop.env ] && grep -q '^CRABBOX_DESKTOP_ENV=wayland$' /var/lib/crabbox/desktop.env; then
        printf '%s\\n' '#!/bin/sh' 'if [ -f /var/lib/crabbox/desktop.env ]; then . /var/lib/crabbox/desktop.env; fi' 'export XDG_RUNTIME_DIR WAYLAND_DISPLAY' 'export MOZ_ENABLE_WAYLAND=1' 'profile="\${CRABBOX_BROWSER_PROFILE:-$HOME/.cache/crabbox/browser-profile}"' 'umask 077' 'mkdir -p "$profile"' 'chmod 700 "$profile"' "exec \\"$browser_path\\" --no-first-run --no-default-browser-check --disable-default-apps --hide-crash-restore-bubble --user-data-dir=\\"\\$profile\\" --ozone-platform=wayland --window-size=1500,900 --window-position=80,80 \\"\\$@\\"" > "$browser_wrapper"
      else
        printf '%s\\n' '#!/bin/sh' 'profile="\${CRABBOX_BROWSER_PROFILE:-$HOME/.cache/crabbox/browser-profile}"' 'umask 077' 'mkdir -p "$profile"' 'chmod 700 "$profile"' "exec \\"$browser_path\\" --no-first-run --no-default-browser-check --disable-default-apps --hide-crash-restore-bubble --user-data-dir=\\"\\$profile\\" --window-size=1500,900 --window-position=80,80 \\"\\$@\\"" > "$browser_wrapper"
      fi
      chmod 0755 "$browser_wrapper"
      printf 'CHROME_BIN=%s\\nBROWSER=%s\\n' "$browser_wrapper" "$browser_wrapper" > /var/lib/crabbox/browser.env
      chown crabbox:crabbox /var/lib/crabbox/browser.env
      chmod 0644 /var/lib/crabbox/browser.env
    fi`);
  }
  if (config.code) {
    parts.push(`    retry apt-get install -y --no-install-recommends libatomic1
    ${sharedCodeServerInstall()}
    /usr/local/bin/code-server --version >/dev/null`);
  }
  return parts.join("\n");
}

function indentRuncmdScript(script: string): string {
  if (script.length === 0) {
    return "";
  }
  const lines = script.split("\n");
  return lines
    .map((line, index) => {
      if (line === "" && index === lines.length - 1) {
        return "";
      }
      return `    ${line}`;
    })
    .join("\n");
}

function tailscaleInstallBootstrap(config: LeaseConfig): string {
  if (config.tailscaleInstallMode !== "pinned") {
    return sharedTailscalePackageInstall();
  }
  return sharedTailscalePinnedInstall(
    config.tailscaleVersion || defaultTailscaleVersion,
    config.tailscaleSHA256?.amd64 || "",
    config.tailscaleSHA256?.arm64 || "",
  );
}

function tailscaleBootstrap(config: LeaseConfig): string {
  if (!config.tailscaleAuthKey) {
    return `    echo "tailscale requested but no auth key was injected" >&2
    exit 1`;
  }
  const sshUser = config.sshUser.trim() || "crabbox";
  const upArgs = [
    "--auth-key=file:/dev/stdin",
    `--hostname=${shellQuote(config.tailscaleHostname)}`,
    `--advertise-tags=${shellQuote(config.tailscaleTags.join(","))}`,
  ];
  if (config.tailscaleExitNode) {
    upArgs.push(`--exit-node=${shellQuote(config.tailscaleExitNode)}`);
    if (config.tailscaleExitNodeAllowLanAccess) {
      upArgs.push("--exit-node-allow-lan-access");
    }
  }
  return `    ${tailscaleInstallBootstrap(config)}
    systemctl enable --now tailscaled || service tailscaled start || true
    if command -v systemctl >/dev/null 2>&1; then
      systemctl disable crabbox-tailscale-logout.service >/dev/null 2>&1 || true
      rm -f /etc/systemd/system/crabbox-tailscale-logout.service
      systemctl daemon-reload || true
    fi
    rm -f /usr/local/bin/crabbox-tailscale-logout
    install -d -m 0750 -o ${shellQuote(sshUser)} -g ${shellQuote(sshUser)} /var/lib/crabbox
    set +x
    TS_AUTHKEY=${shellQuote(config.tailscaleAuthKey)}
    printf '%s' "$TS_AUTHKEY" | tailscale up ${upArgs.join(" ")}
    unset TS_AUTHKEY
    set -x
    ts_ip=""
    for _ in $(seq 1 24); do
      ts_ip="$(tailscale ip -4 2>/dev/null | head -n1 || true)"
      if [ -n "$ts_ip" ]; then break; fi
      sleep 5
    done
    test -n "$ts_ip"
    printf '%s\\n' "$ts_ip" > /var/lib/crabbox/tailscale-ipv4
    printf '%s\\n' ${shellQuote(config.tailscaleHostname)} > /var/lib/crabbox/tailscale-hostname
    tailscale version 2>/dev/null | head -n1 > /var/lib/crabbox/tailscale-version || true
    if [ -n ${shellQuote(config.tailscaleExitNode)} ]; then
      printf '%s\\n' ${shellQuote(config.tailscaleExitNode)} > /var/lib/crabbox/tailscale-exit-node
      printf '%s\\n' ${shellQuote(String(config.tailscaleExitNodeAllowLanAccess))} > /var/lib/crabbox/tailscale-exit-node-allow-lan-access
    fi
    if tailscale status --json >/var/lib/crabbox/tailscale-status.json 2>/dev/null; then
      jq -r '.Self.DNSName // empty' /var/lib/crabbox/tailscale-status.json > /var/lib/crabbox/tailscale-fqdn || true
      jq -r '.Self.ID // .Self.NodeID // .Self.StableID // empty' /var/lib/crabbox/tailscale-status.json > /var/lib/crabbox/tailscale-device-id || true
    fi
    chown ${shellQuote(`${sshUser}:${sshUser}`)} /var/lib/crabbox/tailscale-* || true
    chmod 0640 /var/lib/crabbox/tailscale-* || true`;
}

function psQuote(value: string): string {
  return `'${value.replaceAll("'", "''")}'`;
}

function shellQuote(value: string): string {
  return `'${value.replaceAll("'", "'\\''")}'`;
}
