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
		"x11-xserver-utils xterm",
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
