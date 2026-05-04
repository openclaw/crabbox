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
		"touch /var/lib/crabbox/bootstrapped",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("cloudInit() missing %q", want)
		}
	}
	if strings.Contains(got, "\npackages:\n") {
		t.Fatal("cloudInit() must not use cloud-init's one-shot packages module")
	}
	for _, notWant := range []string{"go version", "golang-go", "go.dev/dl/go", "/usr/local/go", "node --version", "pnpm --version", "docker --version", "build-essential", "docker.io", "corepack"} {
		if strings.Contains(got, notWant) {
			t.Fatalf("cloudInit() should not install project language runtime %q", notWant)
		}
	}
}

func TestCloudInitDesktopProfile(t *testing.T) {
	cfg := baseConfig()
	cfg.Desktop = true
	got := cloudInit(cfg, "ssh-ed25519 test")
	for _, want := range []string{
		"xvfb xfce4 xfce4-terminal x11vnc xauth dbus-x11",
		"x11-xserver-utils xterm scrot",
		"/etc/systemd/system/crabbox-xvfb.service",
		"/etc/systemd/system/crabbox-desktop.service",
		"/usr/local/bin/crabbox-desktop-session",
		"/etc/systemd/system/crabbox-desktop-session.service",
		"/etc/systemd/system/crabbox-x11vnc.service",
		"ExecStart=/usr/bin/startxfce4",
		"systemctl is-active --quiet crabbox-desktop.service",
		"systemctl is-active --quiet crabbox-desktop-session.service",
		"xsetroot -solid '#20242b'",
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
		"https://dl.google.com/linux/linux_signing_key.pub",
		"chmod 0644 /etc/apt/trusted.gpg.d/google.asc",
		"https://dl.google.com/linux/chrome/deb/",
		"google-chrome-stable",
		"apt-cache show chromium",
		"apt-cache show chromium-browser",
		"/var/lib/crabbox/browser.env",
		"test -x \"$BROWSER\"",
		"\"$BROWSER\" --version >/dev/null",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("cloudInit(browser) missing %q", want)
		}
	}
}

func TestCloudInitTailscaleProfile(t *testing.T) {
	cfg := baseConfig()
	cfg.SSHUser = "runner"
	cfg.Tailscale.Enabled = true
	cfg.Tailscale.AuthKey = "tskey-secret"
	cfg.Tailscale.Hostname = "crabbox-blue-lobster"
	cfg.Tailscale.Tags = []string{"tag:crabbox"}
	got := cloudInit(cfg, "ssh-ed25519 test")
	for _, want := range []string{
		"https://tailscale.com/install.sh",
		"install -d -m 0750 -o 'runner' -g 'runner' /var/lib/crabbox",
		"tailscale up --auth-key=\"$TS_AUTHKEY\" --hostname='crabbox-blue-lobster' --advertise-tags='tag:crabbox'",
		"printf '%s\\n' 'crabbox-blue-lobster' > /var/lib/crabbox/tailscale-hostname",
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

func TestAWSUserDataWindowsProfile(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "aws"
	cfg.TargetOS = targetWindows
	cfg.WindowsMode = windowsModeNormal
	cfg.WorkRoot = `C:\crabbox`
	userData := awsUserData(cfg, "ssh-ed25519 test")
	if !strings.Contains(userData, "version: 1.1") || !strings.Contains(userData, "task: enableOpenSsh") {
		t.Fatalf("windows user data should enable EC2Launch OpenSSH:\n%s", userData)
	}
	got := windowsBootstrapPowerShell(cfg, "ssh-ed25519 test")
	for _, want := range []string{
		"OpenSSH-Win64.zip",
		"install-sshd.ps1",
		"administrators_authorized_keys",
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
		"start-user-vnc.ps1",
		"-run",
		"Set-Service -StartupType Disabled",
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
}

func TestAWSUserDataMacOSProfile(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "aws"
	cfg.TargetOS = targetMacOS
	cfg.SSHUser = "ec2-user"
	got := awsUserData(cfg, "ssh-ed25519 test")
	for _, want := range []string{
		"#!/bin/bash",
		"/var/db/crabbox/vnc.password",
		"com.apple.screensharing",
		"/usr/local/bin/crabbox-ready",
		"nc -z 127.0.0.1 5900",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("macOS user data missing %q", want)
		}
	}
}
