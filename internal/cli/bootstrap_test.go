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
		"test -w /work/crabbox",
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
		"/etc/systemd/system/crabbox-xvfb.service",
		"/etc/systemd/system/crabbox-desktop.service",
		"/usr/local/bin/crabbox-desktop-session",
		"/etc/systemd/system/crabbox-desktop-session.service",
		"/etc/systemd/system/crabbox-x11vnc.service",
		"ExecStart=/usr/bin/startxfce4",
		"systemctl is-active --quiet crabbox-desktop.service",
		"systemctl is-active --quiet crabbox-desktop-session.service",
		"xsetroot -solid '#20242b'",
		"xfce4-terminal --title='Crabbox Desktop'",
		"xterm -title 'Crabbox Desktop'",
		"(umask 077 && openssl rand -base64 18 > /var/lib/crabbox/vnc.password)",
		"x11vnc -storepasswd",
		"-rfbauth /var/lib/crabbox/vnc.pass",
		"ss -ltn | grep -q '127.0.0.1:5900'",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("cloudInit(desktop) missing %q", want)
		}
	}
}

func TestCloudInitBrowserProfile(t *testing.T) {
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
		"--no-first-run --no-default-browser-check --disable-default-apps --window-size=1500,900 --window-position=80,80",
		"/var/lib/crabbox/browser.env",
		"test -x \"$BROWSER\"",
		"\"$BROWSER\" --version >/dev/null",
		"printf '%s\\n' '{\"DefaultBrowserSettingEnabled\":false,\"MetricsReportingEnabled\":false,\"PromotionalTabsEnabled\":false}' > /etc/opt/chrome/policies/managed/crabbox.json",
		"printf '%s\\n' '#!/bin/sh' \"exec \\\"$browser_path\\\" --no-first-run --no-default-browser-check --disable-default-apps --window-size=1500,900 --window-position=80,80 \\\"\\$@\\\"\" > \"$browser_wrapper\"",
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
		"cat >/usr/local/bin/crabbox-ready",
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
