import { describe, expect, it } from "vitest";

import {
  awsUserData,
  azureWindowsBootstrapPowerShell,
  cloudInit,
  windowsBootstrapPowerShell,
} from "../src/bootstrap";
import type { LeaseConfig } from "../src/config";

const config: LeaseConfig = {
  provider: "aws",
  target: "linux",
  windowsMode: "normal",
  desktop: false,
  browser: false,
  code: false,
  tailscale: false,
  tailscaleTags: ["tag:crabbox"],
  tailscaleHostname: "",
  tailscaleAuthKey: "",
  tailscaleExitNode: "",
  tailscaleExitNodeAllowLanAccess: false,
  profile: "project-check",
  class: "standard",
  serverType: "c7a.8xlarge",
  location: "fsn1",
  image: "ubuntu-24.04",
  awsRegion: "eu-west-1",
  awsAMI: "",
  awsSGID: "",
  awsSubnetID: "",
  awsProfile: "",
  awsRootGB: 400,
  capacityMarket: "spot",
  capacityStrategy: "most-available",
  capacityFallback: "on-demand-after-120s",
  capacityRegions: [],
  capacityAvailabilityZones: [],
  sshUser: "crabbox",
  sshPort: "2222",
  sshFallbackPorts: ["22"],
  providerKey: "crabbox-steipete",
  workRoot: "/work/crabbox",
  ttlSeconds: 1200,
  idleTimeoutSeconds: 360,
  keep: false,
  sshPublicKey: "ssh-ed25519 test",
};

describe("cloud-init bootstrap", () => {
  it("uses retrying package installation in runcmd", () => {
    const got = cloudInit(config);
    expect(got).toContain("package_update: false");
    expect(got).toContain("bash -euxo pipefail <<'BOOT'");
    expect(got).toContain('Acquire::Retries "8";');
    expect(got).toContain("retry apt-get update");
    expect(got).toContain(
      "retry apt-get install -y --no-install-recommends openssh-server ca-certificates curl git rsync jq",
    );
    expect(got).toContain("curl --version >/dev/null");
    expect(got).toContain("test -f /var/lib/crabbox/bootstrapped");
    expect(got).toContain("test -w /work/crabbox");
    expect(got).toContain("      Port 2222\n      Port 22");
    expect(got).toContain("systemctl enable ssh || true");
    expect(got).toContain(
      "timeout 30s systemctl restart ssh || timeout 30s systemctl restart ssh.socket || true",
    );
    expect(got).toContain("touch /var/lib/crabbox/bootstrapped");
    expect(got).not.toContain("\npackages:\n");
    expect(got).not.toContain("systemctl enable --now ssh");
    expect(got).not.toContain("go version");
    expect(got).not.toContain("golang-go");
    expect(got).not.toContain("go.dev/dl/go");
    expect(got).not.toContain("/usr/local/go");
    expect(got).not.toContain("node --version");
    expect(got).not.toContain("pnpm --version");
    expect(got).not.toContain("docker --version");
    expect(got).not.toContain("build-essential");
    expect(got).not.toContain("docker.io");
    expect(got).not.toContain("corepack");
  });

  it("adds desktop services only when requested", () => {
    const got = cloudInit({ ...config, desktop: true });
    expect(got).toContain("xvfb xfce4-session xfwm4 xfce4-panel xfdesktop4 xfce4-terminal");
    expect(got).toContain("xfconf xfce4-settings x11vnc xauth dbus-x11");
    expect(got).toContain("/etc/systemd/system/crabbox-xvfb.service");
    expect(got).toContain("/usr/local/bin/crabbox-configure-desktop-theme");
    expect(got).toContain("/etc/systemd/system/crabbox-desktop.service");
    expect(got).toContain("/usr/local/bin/crabbox-desktop-session");
    expect(got).toContain("/etc/systemd/system/crabbox-desktop-session.service");
    expect(got).toContain("/etc/systemd/system/crabbox-x11vnc.service");
    expect(got).toContain("ExecStart=/usr/bin/startxfce4");
    expect(got).toContain("systemctl is-active --quiet crabbox-desktop.service");
    expect(got).toContain("systemctl is-active --quiet crabbox-desktop-session.service");
    expect(got).toContain('ThemeName" type="string" value="Adwaita-dark');
    expect(got).toContain("gtk-application-prefer-dark-theme=1");
    expect(got).toContain("xfconf-query -c xsettings -p /Gtk/ApplicationPreferDarkTheme");
    expect(got).toContain("gsettings set org.gnome.desktop.interface color-scheme prefer-dark");
    expect(got).toContain(
      "CRABBOX_DESKTOP_USER=crabbox /usr/local/bin/crabbox-configure-desktop-theme",
    );
    expect(got).toContain(
      'CRABBOX_DESKTOP_USER="$(id -un)" /usr/local/bin/crabbox-configure-desktop-theme',
    );
    expect(got).toContain("x11-xserver-utils xterm scrot ffmpeg xdotool wmctrl");
    expect(got).toContain("xsetroot -solid '#20242b'");
    expect(got).toContain("xfce4-terminal --title='Crabbox Desktop'");
    expect(got).toContain("xterm -title 'Crabbox Desktop'");
    expect(got).toContain("(umask 077 && openssl rand -base64 18 > /var/lib/crabbox/vnc.password)");
    expect(got).toContain("-rfbauth /var/lib/crabbox/vnc.pass");
    expect(got).toContain("ss -ltn | grep -q '127.0.0.1:5900'");
  });

  it("starts ssh before optional desktop and browser bootstrap", () => {
    const got = cloudInit({ ...config, desktop: true, browser: true });
    const sshIndex = got.indexOf("systemctl restart ssh");
    const desktopIndex = got.indexOf("retry apt-get install -y --no-install-recommends xvfb");
    const browserIndex = got.indexOf("retry apt-get install -y --no-install-recommends gnupg");
    const bootstrappedIndex = got.indexOf("touch /var/lib/crabbox/bootstrapped");
    expect(sshIndex).toBeGreaterThanOrEqual(0);
    expect(desktopIndex).toBeGreaterThanOrEqual(0);
    expect(browserIndex).toBeGreaterThanOrEqual(0);
    expect(bootstrappedIndex).toBeGreaterThanOrEqual(0);
    expect(sshIndex).toBeLessThan(desktopIndex);
    expect(sshIndex).toBeLessThan(browserIndex);
    expect(bootstrappedIndex).toBeGreaterThan(desktopIndex);
    expect(bootstrappedIndex).toBeGreaterThan(browserIndex);
  });

  it("adds browser setup only when requested", () => {
    const got = cloudInit({ ...config, browser: true });
    expect(got).toContain("gnupg build-essential python3");
    expect(got).toContain("https://dl.google.com/linux/linux_signing_key.pub");
    expect(got).toContain("chmod 0644 /etc/apt/trusted.gpg.d/google.asc");
    expect(got).toContain("https://dl.google.com/linux/chrome/deb/");
    expect(got).toContain("google-chrome-stable");
    expect(got).toContain("apt-cache show chromium");
    expect(got).toContain("apt-cache show chromium-browser");
    expect(got).toContain("/etc/opt/chrome/policies/managed/crabbox.json");
    expect(got).toContain("/usr/local/bin/crabbox-browser");
    expect(got).toContain(
      "--no-first-run --no-default-browser-check --disable-default-apps --window-size=1500,900 --window-position=80,80",
    );
    expect(got).toContain("/var/lib/crabbox/browser.env");
    expect(got).toContain('test -x "$BROWSER"');
    expect(got).toContain('"$BROWSER" --version >/dev/null');
    expect(got).toContain(
      `printf '%s\\n' '{"DefaultBrowserSettingEnabled":false,"MetricsReportingEnabled":false,"PromotionalTabsEnabled":false}' > /etc/opt/chrome/policies/managed/crabbox.json`,
    );
    expect(got).toContain(
      `printf '%s\\n' '#!/bin/sh' "exec \\"$browser_path\\" --no-first-run --no-default-browser-check --disable-default-apps --window-size=1500,900 --window-position=80,80 \\"\\$@\\"" > "$browser_wrapper"`,
    );
    expect(got).not.toContain("<<'EOF'");
    expect(got).not.toContain("<<EOF");
    expect(got).not.toContain("\nEOF");
  });

  it("adds code-server setup only when requested", () => {
    const plain = cloudInit(config);
    expect(plain).not.toContain("code-server");
    const got = cloudInit({ ...config, code: true });
    expect(got).toContain("https://code-server.dev/install.sh");
    expect(got).toContain("env HOME=/root");
    expect(got).toContain("--method=standalone --prefix=/usr/local");
    expect(got).toContain("/usr/local/bin/code-server --version >/dev/null");
    expect(got).toContain("test -x /usr/local/bin/code-server");
  });

  it("adds Tailscale setup only when requested", () => {
    const plain = cloudInit(config);
    expect(plain).not.toContain("tailscale up");
    const got = cloudInit({
      ...config,
      sshUser: "runner",
      tailscale: true,
      tailscaleTags: ["tag:crabbox"],
      tailscaleHostname: "crabbox-blue-lobster",
      tailscaleAuthKey: "tskey-secret",
      tailscaleExitNode: "mac-studio.tailnet.ts.net",
      tailscaleExitNodeAllowLanAccess: true,
    });
    expect(got).toContain("https://tailscale.com/install.sh");
    expect(got).toContain("install -d -m 0750 -o 'runner' -g 'runner' /var/lib/crabbox");
    expect(got).toContain(
      "tailscale up --auth-key=\"$TS_AUTHKEY\" --hostname='crabbox-blue-lobster' --advertise-tags='tag:crabbox' --exit-node='mac-studio.tailnet.ts.net' --exit-node-allow-lan-access",
    );
    expect(got).toContain(
      "printf '%s\\n' 'crabbox-blue-lobster' > /var/lib/crabbox/tailscale-hostname",
    );
    expect(got).toContain(
      "printf '%s\\n' 'mac-studio.tailnet.ts.net' > /var/lib/crabbox/tailscale-exit-node",
    );
    expect(got).toContain(
      "printf '%s\\n' 'true' > /var/lib/crabbox/tailscale-exit-node-allow-lan-access",
    );
    expect(got).toContain("chown 'runner:runner' /var/lib/crabbox/tailscale-* || true");
    expect(got).toContain("test -s /var/lib/crabbox/tailscale-ipv4");
    expect(got).toContain("grep -Eq '^100\\.' /var/lib/crabbox/tailscale-ipv4");
  });

  it("builds Windows EC2Launch user data for managed VNC", () => {
    const input = {
      ...config,
      target: "windows",
      desktop: true,
      workRoot: "C:\\crabbox",
    } as const;
    expect(awsUserData(input)).toContain("version: 1.1");
    expect(awsUserData(input)).toContain("task: enableOpenSsh");
    const got = windowsBootstrapPowerShell(input);
    expect(got).toContain("OpenSSH-Win64.zip");
    expect(got).toContain("install-sshd.ps1");
    expect(got).toContain("administrators_authorized_keys");
    expect(got).toContain("Match Group administrators");
    expect(got).toContain("$sshPorts = @('2222', '22')");
    expect(got).toContain("sshd_config");
    expect(got).toContain("Port $port");
    expect(got).toContain("crabbox-sshd-$port");
    expect(got).toContain("tightvnc-2.8.85-gpl-setup-64bit.msi");
    expect(got).toContain("NewNetworkWindowOff");
    expect(got).toContain("DoNotOpenServerManagerAtLogon");
    expect(got).toContain("VALUE_OF_PASSWORD=$vncPassword");
    expect(got).toContain("VALUE_OF_ALLOWLOOPBACK=1");
    expect(got).toContain("CrabboxUserVNC");
    expect(got).toContain("crabbox-user-vnc.cmd");
    expect(got).toContain("AppData\\Roaming\\Microsoft\\Windows\\Start Menu\\Programs\\Startup");
    expect(got).toContain("start-user-vnc.ps1");
    expect(got).toContain("Set-TightVNCBinaryValue");
    expect(got).toContain('reg.exe add "HKCU\\Software\\TightVNC\\Server"');
    expect(got).toContain('$hex = -join ($bytes | ForEach-Object { $_.ToString("X2") })');
    expect(got).toContain("/SC ONLOGON");
    expect(got).toContain("Set-Service -StartupType Disabled");
    expect(got).toContain("Stop-Service -Name tvnserver");
    expect(got).not.toContain("/SC ONCE");
    expect(got).not.toContain("Set-Service -StartupType Manual");
    expect(got).not.toContain("Start-Service -Name tvnserver");
    expect(got).toContain("New-CrabboxPassword");
    expect(got).toContain("${userSID}:F");
    expect(got).toContain("C:\\ProgramData\\crabbox\\windows.username");
    expect(got).toContain("AutoAdminLogon");
    expect(got).toContain("Restart-Computer -Force");
    expect(got).toContain("exit 0");
    const setupIndex = got.indexOf(
      "Set-Content -NoNewline -Encoding ASCII -Path $setupCompletePath",
    );
    const restartIndex = got.indexOf("Restart-Computer -Force");
    expect(setupIndex).toBeGreaterThanOrEqual(0);
    expect(setupIndex).toBeLessThan(restartIndex);
  });

  it("builds Windows core bootstrap without desktop/VNC", () => {
    const input = {
      ...config,
      target: "windows",
      workRoot: "C:\\crabbox",
    } as const;
    const got = windowsBootstrapPowerShell(input);
    expect(got).toContain("OpenSSH-Win64.zip");
    expect(got).toContain("Git-2.52.0-64-bit.exe");
    expect(got).toContain("$passwordPath = $windowsPasswordPath");
    expect(got).toContain("Restart-Service sshd -Force");
    expect(got).toContain("Set-Content -NoNewline -Encoding ASCII -Path $setupCompletePath");
    const setupIndex = got.indexOf(
      "Set-Content -NoNewline -Encoding ASCII -Path $setupCompletePath",
    );
    const restartIndex = got.indexOf("Restart-Service sshd -Force");
    expect(setupIndex).toBeGreaterThanOrEqual(0);
    expect(setupIndex).toBeLessThan(restartIndex);
    expect(got).not.toContain("tightvnc-2.8.85-gpl-setup-64bit.msi");
    expect(got).not.toContain("C:\\ProgramData\\crabbox\\vnc.password");
    expect(got).not.toContain("CrabboxUserVNC");
    expect(got).not.toContain("AutoAdminLogon");
    expect(got).not.toContain("Restart-Computer -Force");
  });

  it("builds Windows WSL2 bootstrap without desktop/VNC", () => {
    const input = {
      ...config,
      target: "windows",
      windowsMode: "wsl2",
      workRoot: "/work/crabbox",
    } as const;
    const got = windowsBootstrapPowerShell(input);
    expect(got).toContain("$workRoot = 'C:\\crabbox'");
    expect(got).toContain("C:\\ProgramData\\crabbox\\windows.password");
    expect(got).toContain("Microsoft-Windows-Subsystem-Linux");
    expect(got).toContain("VirtualMachinePlatform");
    expect(got).toContain("HypervisorPlatform");
    expect(got).toContain("bcdedit.exe /set hypervisorlaunchtype auto");
    expect(got).toContain("wsl.exe --update --web-download");
    expect(got).toContain("wsl.exe --set-default-version 2");
    expect(got).toContain("ubuntu-noble-wsl-amd64-wsl.rootfs.tar.gz");
    expect(got).toContain("$wslRootfsMinBytes = 100 * 1024 * 1024");
    expect(got).toContain("curl.exe -fL --retry 8");
    expect(got).toContain("downloaded WSL rootfs is incomplete");
    expect(got).toContain("wsl.exe --import $wslDistro $wslRoot $wslRootfs --version 2");
    expect(got).toContain("wsl.exe --set-default $wslDistro");
    expect(got).toContain("test -w '/work/crabbox'");
    const setupIndex = got.indexOf(
      "Set-Content -NoNewline -Encoding ASCII -Path $setupCompletePath",
    );
    const restartIndex = got.lastIndexOf("Restart-Service sshd -Force");
    expect(setupIndex).toBeGreaterThanOrEqual(0);
    expect(setupIndex).toBeLessThan(restartIndex);
    expect(got).not.toContain("tightvnc-2.8.85-gpl-setup-64bit.msi");
    expect(got).not.toContain("C:\\ProgramData\\crabbox\\vnc.password");
    expect(got).not.toContain("CrabboxUserVNC");
    expect(got).not.toContain("AutoAdminLogon");
  });

  it("builds Azure Windows extension bootstrap without restart", () => {
    const input = {
      ...config,
      provider: "azure",
      target: "windows",
      workRoot: "C:\\crabbox",
      sshPublicKey: "ssh-rsa test",
    } as const;
    const got = azureWindowsBootstrapPowerShell(input);
    expect(got).toContain("OpenSSH-Win64.zip");
    expect(got).toContain("Git-2.52.0-64-bit.exe");
    expect(got).toContain("administrators_authorized_keys");
    expect(got).toContain("Match Group administrators");
    expect(got).toContain("$sshPorts = @('2222', '22')");
    expect(got).toContain("PasswordAuthentication no");
    expect(got).toContain("Restart-Service sshd -Force");
    expect(got).toContain("Set-Content -NoNewline -Encoding ASCII -Path $setupCompletePath");
    expect(got).not.toContain("Restart-Computer");
    expect(got).not.toContain("tightvnc");
  });

  it("leaves Azure Windows desktop restart to the SSH bootstrap", () => {
    const input = {
      ...config,
      provider: "azure",
      target: "windows",
      desktop: true,
      workRoot: "C:\\crabbox",
      sshPublicKey: "ssh-rsa test",
    } as const;
    const got = azureWindowsBootstrapPowerShell(input);
    expect(got).toContain("PasswordAuthentication no");
    expect(got).not.toContain("Set-Content -NoNewline -Encoding ASCII -Path $setupCompletePath");
    expect(got).not.toContain("Restart-Computer");
    expect(got).not.toContain("tightvnc");
  });

  it("builds macOS user data for managed screen sharing", () => {
    const got = awsUserData({
      ...config,
      target: "macos",
      sshUser: "ec2-user",
      workRoot: "/Users/ec2-user/crabbox",
    });
    expect(got).toContain("#!/bin/bash");
    expect(got).toContain("/Users/ec2-user/crabbox");
    expect(got).toContain("/var/db/crabbox/vnc.password");
    expect(got).toContain("set +o pipefail");
    expect(got).toContain("set -o pipefail");
    expect(got).toContain("failed to generate vnc password");
    expect(got).toContain("crabbox_public_key='ssh-ed25519 test'");
    expect(got).toContain("authorized_keys");
    expect(got).toContain("crabbox_ssh_ports=('2222' '22')");
    expect(got).toContain("printf 'Port %s\\n' \"$port\"");
    expect(got).toContain("systemsetup -setremotelogin on");
    expect(got).toContain("com.openssh.sshd");
    expect(got).toContain("com.apple.screensharing");
    expect(got).toContain("/usr/local/bin/crabbox-ready");
  });
});
