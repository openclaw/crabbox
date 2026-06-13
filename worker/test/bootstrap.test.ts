import { describe, expect, it } from "vitest";

import {
  awsRunInstancesUserData,
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
  desktopEnv: "xfce",
  browser: false,
  code: false,
  tailscale: false,
  tailscaleTags: ["tag:crabbox"],
  tailscaleHostname: "",
  tailscaleAuthKey: "",
  tailscaleInstallMode: "package",
  tailscaleVersion: "1.98.4",
  tailscaleSHA256: {
    amd64: "e6c08a8ee7e63e69aaf1b62ecd12672b3883fbcd2a176bf6cfa42a15fdce0b6b",
    arm64: "3cb068eb1368b6bb218d0ef0aa0a7a679a7156b7c979e2279cc2c2321b5f05c7",
  },
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

async function gunzipBase64(value: string): Promise<string> {
  const bytes = Uint8Array.from(atob(value), (char) => char.charCodeAt(0));
  const stream = new Blob([bytes]).stream().pipeThrough(new DecompressionStream("gzip"));
  return await new Response(stream).text();
}

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
    expect(got).toContain("arc-theme");
    expect(got).toContain("/etc/systemd/system/crabbox-xvfb.service");
    expect(got).toContain("/usr/local/bin/crabbox-configure-desktop-theme");
    expect(got).toContain("/etc/systemd/system/crabbox-desktop.service");
    expect(got).toContain("/usr/local/bin/crabbox-desktop-session");
    expect(got).toContain("/etc/systemd/system/crabbox-desktop-session.service");
    expect(got).toContain("/etc/systemd/system/crabbox-x11vnc.service");
    expect(got).toContain("ExecStart=/usr/bin/startxfce4");
    expect(got).toContain("systemctl is-active --quiet crabbox-desktop.service");
    expect(got).toContain("systemctl is-active --quiet crabbox-desktop-session.service");
    expect(got).toContain('requested_mode="${1:-${CRABBOX_DESKTOP_THEME:-}}"');
    expect(got).toContain('"$config_dir/crabbox/desktop-theme"');
    expect(got).toContain(`printf '%s\\n' "$mode" > "$config_dir/crabbox/desktop-theme"`);
    expect(got).not.toContain(`printf '%s\n' "$mode" > "$config_dir/crabbox/desktop-theme"`);
    expect(got).toContain("gtk_theme=Adwaita-dark");
    expect(got).toContain('gtk_candidates="Arc-Dark Greybird-dark Adwaita-dark Greybird"');
    expect(got).toContain('gtk_candidates="Arc Greybird Adwaita"');
    expect(got).toContain("xfwm_theme=Default");
    expect(got).toContain('xfwm_candidates="Arc-Dark Greybird-dark Daloa Default"');
    expect(got).toContain('xfwm_candidates="Arc Greybird Daloa Default"');
    expect(got).toContain('ThemeName" type="string" value="$gtk_theme');
    expect(got).toContain("$config_dir/xfce4/xfconf/xfce-perchannel-xml/xfwm4.xml");
    expect(got).toContain('theme" type="string" value="$xfwm_theme');
    expect(got).toContain('box_move" type="bool" value="false');
    expect(got).toContain('box_resize" type="bool" value="false');
    expect(got).toContain('move_opacity" type="int" value="100');
    expect(got).toContain('resize_opacity" type="int" value="100');
    expect(got).toContain('snap_to_border" type="bool" value="false');
    expect(got).toContain('snap_width" type="int" value="0');
    expect(got).toContain('tile_on_move" type="bool" value="false');
    expect(got).toContain('use_compositing" type="bool" value="false');
    expect(got).toContain('wrap_windows" type="bool" value="false');
    expect(got).toContain("gtk-application-prefer-dark-theme=$gtk_prefer_dark_ini");
    expect(got).toContain('mkdir -p "$config_dir/xfce4/xfconf/xfce-perchannel-xml"');
    expect(got).toContain("xfconf-query -c xsettings -p /Gtk/ApplicationPreferDarkTheme");
    expect(got).toContain("xfconf-query -c xfwm4 -p /general/theme");
    expect(got).toContain("xfconf-query -c xfwm4 -p /general/box_move");
    expect(got).toContain("xfconf-query -c xfwm4 -p /general/box_resize");
    expect(got).toContain("xfconf-query -c xfwm4 -p /general/move_opacity");
    expect(got).toContain("xfconf-query -c xfwm4 -p /general/resize_opacity");
    expect(got).toContain("xfconf-query -c xfwm4 -p /general/snap_to_border");
    expect(got).toContain("xfconf-query -c xfwm4 -p /general/snap_width");
    expect(got).toContain("xfconf-query -c xfwm4 -p /general/tile_on_move");
    expect(got).toContain("xfconf-query -c xfwm4 -p /general/use_compositing");
    expect(got).toContain("xfconf-query -c xfwm4 -p /general/wrap_windows");
    expect(got).toContain("xfconf-query -c xfce4-panel -p /panels/dark-mode");
    expect(got).toContain("/panels/$panel_id/background-rgba");
    expect(got).toContain("desktop-background-$mode.svg");
    expect(got).toContain('xfconf-query -c xfce4-desktop -p "$backdrop/image-style"');
    expect(got).toContain('xfconf-query -c xfce4-desktop -p "$backdrop/last-image"');
    expect(got).toContain("crabbox desktop theme start");
    expect(got).toContain("border-color: transparent");
    expect(got).toContain("menubar > menuitem");
    expect(got).toContain("menubar > menuitem label");
    expect(got).toContain("crabbox-xfce4-panel-$user.log");
    expect(got).toContain('pkill -TERM -u "$user_id" -x xfce4-panel');
    expect(got).toContain("pkill -TERM -u \"$user_id\" -f '/xfce4/panel/wrapper-2.0'");
    expect(got).toContain('pgrep -u "$user_id" -x xfce4-panel');
    expect(got).toContain("sleep 1");
    expect(got).toContain("xfce4-panel --disable-wm-check");
    expect(got).toContain("xfwm4 --replace --compositor=off");
    expect(got).toContain('xsetroot -solid "$root_color"');
    expect(got).toContain("crabbox-xfdesktop-$user.log");
    expect(got).toContain(
      'gsettings set org.gnome.desktop.interface color-scheme "$gsettings_scheme"',
    );
    expect(got).toContain(
      "CRABBOX_DESKTOP_USER=crabbox /usr/local/bin/crabbox-configure-desktop-theme",
    );
    expect(got).toContain(
      'CRABBOX_DESKTOP_USER="$(id -un)" /usr/local/bin/crabbox-configure-desktop-theme',
    );
    expect(got).toContain("x11-xserver-utils xterm scrot ffmpeg xdotool wmctrl");
    expect(got).toContain("xfce4-terminal --title='Crabbox Desktop'");
    expect(got).toContain("xterm -title 'Crabbox Desktop'");
    expect(got).toContain("(umask 077 && openssl rand -base64 18 > /var/lib/crabbox/vnc.password)");
    expect(got).toContain("-rfbauth /var/lib/crabbox/vnc.pass");
    expect(got).toContain("-wait 16 -defer 8 -nowait_bog");
    expect(got).toContain("ss -ltn | grep -q '127.0.0.1:5900'");
    expect(got).toContain("systemctl disable --now crabbox-wayvnc.service 2>/dev/null || true");
    expect(got).toContain(
      "systemctl enable crabbox-xvfb.service crabbox-desktop.service crabbox-desktop-session.service crabbox-x11vnc.service",
    );
    expect(got).toContain(
      "systemctl restart crabbox-xvfb.service crabbox-desktop.service crabbox-desktop-session.service crabbox-x11vnc.service",
    );
  });

  it("adds Wayland desktop services when requested", () => {
    const got = cloudInit({ ...config, desktop: true, desktopEnv: "wayland", browser: true });
    expect(got).toContain("labwc wayvnc foot grim slurp wtype wl-clipboard wlr-randr");
    expect(got).toContain("xdg-desktop-portal-wlr");
    expect(got).toContain("/usr/local/bin/crabbox-start-wayland-desktop");
    expect(got).toContain("/etc/systemd/system/crabbox-wayvnc.service");
    expect(got).toContain("CRABBOX_DESKTOP_ENV=wayland");
    expect(got).toContain("WLR_BACKENDS=headless");
    expect(got).toContain("exec dbus-run-session labwc");
    expect(got).toContain("install -d -m 0700 -o crabbox -g crabbox /home/crabbox/.config/labwc");
    expect(got).toContain("cat >/home/crabbox/.config/labwc/autostart");
    expect(got).toContain("wlr-randr --output HEADLESS-1 --custom-mode 1920x1080");
    expect(got).toContain("foot --title='Crabbox Desktop' >/tmp/crabbox-foot.log 2>&1 &");
    expect(got).toContain('for socket in "$XDG_RUNTIME_DIR"/wayland-*');
    expect(got).toContain('WAYLAND_DISPLAY="${socket##*/}"');
    expect(got).toContain(
      'wayvnc --config "$HOME/.config/wayvnc/config" --render-cursor --max-fps=60',
    );
    expect(got).toContain("systemctl is-active --quiet crabbox-wayvnc.service");
    expect(got).toContain(
      "systemctl disable --now crabbox-xvfb.service crabbox-desktop-session.service crabbox-x11vnc.service 2>/dev/null || true",
    );
    expect(got).toContain("systemctl enable crabbox-desktop.service crabbox-wayvnc.service");
    expect(got).toContain("systemctl restart crabbox-desktop.service crabbox-wayvnc.service");
    expect(got).toContain("--ozone-platform=wayland");
    expect(got).not.toContain("startxfce4");
    expect(got).not.toContain("x11vnc -storepasswd");
    expect(got).not.toContain("XDG_RUNTIME_DIR=/tmp/crabbox-runtime-1000");
    expect(got).not.toContain("\nset $mod");
    expect(got).not.toContain("sway --unsupported-gpu");
    expect(got).not.toContain("crabbox-sway-status");
    expect(got).not.toContain("\nSWAY");
    expect(got).not.toContain("\nCRABBOX_DESKTOP_ENV=wayland");
    expect(got).not.toContain("\nEOF");
    expect(got).not.toContain("\n#!/bin/sh\nwhile");
  });

  it("adds GNOME Wayland desktop services when requested", () => {
    const got = cloudInit({ ...config, desktop: true, desktopEnv: "gnome", browser: true });
    expect(got).toContain(
      "labwc wayvnc swaybg librsvg2-common gnome-panel wlr-randr grim slurp wtype wl-clipboard",
    );
    expect(got).toContain("swaybg librsvg2-common");
    expect(got).toContain("dbus-user-session xwayland");
    expect(got).toContain("gnome-terminal nautilus gsettings-desktop-schemas adwaita-icon-theme");
    expect(got).toContain("/usr/local/bin/crabbox-start-wayland-desktop");
    expect(got).toContain("CRABBOX_DESKTOP_ENV=gnome");
    expect(got).toContain("DISPLAY=:0");
    expect(got).toContain("WAYLAND_DISPLAY=wayland-1");
    expect(got).toContain("exec dbus-run-session labwc");
    expect(got).toContain("export GDK_BACKEND=x11");
    expect(got).toContain("export MOZ_ENABLE_WAYLAND=0");
    expect(got).toContain('theme="$(cat "$HOME/.config/crabbox/desktop-theme"');
    expect(got).toContain("gsettings set org.gnome.desktop.interface color-scheme prefer-dark");
    expect(got).toContain("/usr/local/bin/crabbox-configure-desktop-theme");
    expect(got).toContain(
      "CRABBOX_DESKTOP_USER=crabbox /usr/local/bin/crabbox-configure-desktop-theme",
    );
    expect(got).toContain('if [ "$(id -u)" -eq 0 ]; then');
    expect(got).toContain(
      'mkdir -p "$config_dir/crabbox" "$config_dir/gtk-3.0" "$config_dir/gtk-4.0" "$config_dir/labwc"',
    );
    expect(got).toContain('dbus_address="${DBUS_SESSION_BUS_ADDRESS:-}"');
    expect(got).toContain(
      "DBUS_SESSION_BUS_ADDRESS='$dbus_address' GDK_BACKEND=x11 gsettings set org.gnome.desktop.interface color-scheme",
    );
    expect(got).toContain(
      'DISPLAY="$display" XDG_RUNTIME_DIR="$runtime" DBUS_SESSION_BUS_ADDRESS="$dbus_address" GDK_BACKEND=x11 gsettings set org.gnome.desktop.interface color-scheme "$gsettings_scheme"',
    );
    expect(got).toContain('"$config_dir/labwc/themerc-override"');
    expect(got).toContain("window.active.title.bg.color");
    expect(got).toContain("window.active.button.unpressed.image.color");
    expect(got).toContain('LABWC_PID="$labwc_pid"');
    expect(got).toContain("labwc --reconfigure");
    expect(got).toContain('kill -HUP "$labwc_pid"');
    expect(got).toContain('"$config_dir/gtk-3.0/gtk.css"');
    expect(got).toContain("menubar menuitem");
    expect(got).toContain("desktop-background-$mode.svg");
    expect(got).toContain('swaybg -i "$wallpaper_file" -m fill');
    expect(got).toContain("nohup gnome-panel >/tmp/crabbox-gnome-panel.log 2>&1 &");
    expect(got).toContain('elif [ "$(id -u)" -ne 0 ] && pgrep -x gnome-panel');
    expect(got).toContain("gnome-panel >/tmp/crabbox-gnome-panel.log 2>&1 &");
    expect(got).toContain("gnome-terminal -- bash -l");
    expect(got).toContain("nautilus --new-window");
    expect(got).toContain("rm -f /var/lib/crabbox/display.env");
    expect(got).toContain("/etc/systemd/system/crabbox-wayvnc.service");
    expect(got).toContain("--user-data-dir=");
    expect(got).toContain("--ozone-platform=x11");
    expect(got).toContain(
      "--force-dark-mode --enable-features=WebUIDarkMode --blink-settings=preferredColorScheme=2",
    );
    expect(got).toContain("--blink-settings=preferredColorScheme=1");
    expect(got).not.toContain("startxfce4");
    expect(got).not.toContain("x11vnc -storepasswd");
    expect(got).not.toContain("gnome-shell");
    expect(got).not.toContain("lxqt-panel");
    expect(got).not.toContain("QT_QPA_PLATFORM=xcb");
    expect(got).not.toContain("waybar");
    expect(got).not.toContain('"wlr/taskbar"');
    expect(got).not.toContain("\n#!/bin/sh\nset -eu\nrequested_mode=");
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

  it("compresses AWS Linux user data below the EC2 launch limit", async () => {
    const longKey = `ssh-rsa ${"a".repeat(724)}`;
    const raw = awsUserData({ ...config, desktop: true, browser: true, sshPublicKey: longKey });
    const encoded = await awsRunInstancesUserData({
      ...config,
      desktop: true,
      browser: true,
      sshPublicKey: longKey,
    });
    const compressedBytes = atob(encoded).length;
    expect(new TextEncoder().encode(raw).length).toBeGreaterThan(16 * 1024);
    expect(compressedBytes).toBeLessThan(16 * 1024);
    expect(await gunzipBase64(encoded)).toBe(raw);
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
      "--no-first-run --no-default-browser-check --disable-default-apps --hide-crash-restore-bubble --user-data-dir=",
    );
    expect(got).toContain("browser-profile");
    expect(got).toContain("/var/lib/crabbox/browser.env");
    expect(got).toContain('test -x "$BROWSER"');
    expect(got).toContain('"$BROWSER" --version >/dev/null');
    expect(got).toContain(
      `printf '%s\\n' '{"DefaultBrowserSettingEnabled":false,"MetricsReportingEnabled":false,"PromotionalTabsEnabled":false}' > /etc/opt/chrome/policies/managed/crabbox.json`,
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
    expect(got).toContain("/usr/local/bin/crabbox-tailscale-logout");
    expect(got).toContain("install -d -m 0750 -o 'runner' -g 'runner' /var/lib/crabbox");
    expect(got).toContain(
      "printf '%s' \"$TS_AUTHKEY\" | tailscale up --auth-key=file:/dev/stdin --hostname='crabbox-blue-lobster' --advertise-tags='tag:crabbox' --exit-node='mac-studio.tailnet.ts.net' --exit-node-allow-lan-access",
    );
    expect(got).not.toContain('--auth-key="$TS_AUTHKEY"');
    expect(got).toContain(
      "printf '%s\\n' 'crabbox-blue-lobster' > /var/lib/crabbox/tailscale-hostname",
    );
    expect(got).toContain(
      "tailscale version 2>/dev/null | head -n1 > /var/lib/crabbox/tailscale-version",
    );
    expect(got).toContain(
      "jq -r '.Self.ID // .Self.NodeID // .Self.StableID // empty' /var/lib/crabbox/tailscale-status.json > /var/lib/crabbox/tailscale-device-id",
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

  it("can install a pinned static Tailscale build with checksums", () => {
    const got = cloudInit({
      ...config,
      tailscale: true,
      tailscaleTags: ["tag:crabbox"],
      tailscaleHostname: "crabbox-blue-lobster",
      tailscaleAuthKey: "tskey-secret",
      tailscaleInstallMode: "pinned",
      tailscaleVersion: "1.98.4",
      tailscaleSHA256: {
        amd64: "amd64sum",
        arm64: "arm64sum",
      },
    });
    expect(got).not.toContain("https://tailscale.com/install.sh");
    expect(got).toContain("TS_VERSION='1.98.4'");
    expect(got).toContain("x86_64) TS_ARCH=amd64; TS_SHA256='amd64sum'");
    expect(got).toContain("aarch64|arm64) TS_ARCH=arm64; TS_SHA256='arm64sum'");
    expect(got).toContain(
      "https://pkgs.tailscale.com/stable/tailscale_${TS_VERSION}_${TS_ARCH}.tgz",
    );
    expect(got).toContain("sha256sum -c -");
    expect(got).toContain("/etc/systemd/system/tailscaled.service");
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
    expect(got).toContain("mount -t binfmt_misc binfmt_misc /proc/sys/fs/binfmt_misc");
    expect(got).toContain("test -w /proc/sys/fs/binfmt_misc/register");
    expect(got).toContain(":WSLInterop:M::MZ::/init:PF");
    expect(got).toContain("test -e /proc/sys/fs/binfmt_misc/WSLInterop");
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
