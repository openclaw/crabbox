package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCaptureLocalMacScreenshotScrubsTargetChildEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell screencapture fixture")
	}
	dir := t.TempDir()
	tool := filepath.Join(dir, "screencapture")
	script := "#!/bin/sh\nif [ \"${TEST_ARD_PASSWORD+x}\" = x ] || [ \"$CRABBOX_TEST_KEEP\" != preserved ]; then exit 89; fi\nprintf png > \"$4\"\n"
	if err := os.WriteFile(tool, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TEST_ARD_PASSWORD", "must-not-reach-screencapture")
	t.Setenv("CRABBOX_TEST_KEEP", "preserved")
	output := filepath.Join(dir, "capture.png")
	target := SSHTarget{ChildEnvDenylist: []string{"TEST_ARD_PASSWORD"}}
	if err := captureLocalMacScreenshot(context.Background(), target, output); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(output); err != nil || string(data) != "png" {
		t.Fatalf("screenshot=%q err=%v", data, err)
	}
}

func TestDesktopScreenshotCapturePathExternalLoopbackUsesRemoteMacVNC(t *testing.T) {
	target := SSHTarget{Host: "127.0.0.1", TargetOS: targetMacOS}
	for _, provider := range []string{"external", "exec-provider"} {
		t.Run(provider, func(t *testing.T) {
			got := desktopScreenshotCapturePathFor(Config{Provider: provider}, target, true)
			if got != desktopScreenshotCaptureRemoteMacVNC {
				t.Fatalf("capture path=%v, want remote macOS VNC", got)
			}
		})
	}
	if got := desktopScreenshotCapturePathFor(Config{Provider: "hetzner"}, target, true); got != desktopScreenshotCaptureLocalMac {
		t.Fatalf("non-external capture path=%v, want local macOS screenshot", got)
	}
}

func TestDefaultScreenshotPath(t *testing.T) {
	if got := defaultScreenshotPath("cbx_123", "Blue Lobster"); got != "crabbox-blue-lobster-screenshot.png" {
		t.Fatalf("path=%q", got)
	}
	if got := defaultScreenshotPath("cbx_123", ""); got != "crabbox-cbx-123-screenshot.png" {
		t.Fatalf("fallback path=%q", got)
	}
}

func TestScreenshotRegistersProviderSpecificFlags(t *testing.T) {
	err := (App{Stdout: io.Discard, Stderr: io.Discard}).screenshot(context.Background(), []string{
		"--provider", "direct-webvnc-test",
		"--direct-webvnc-routing", "route-cbx_abcdef123456",
	})
	if err == nil {
		t.Fatal("screenshot without an id should fail")
	}
	if strings.Contains(err.Error(), "flag provided but not defined") {
		t.Fatalf("provider flag was not registered: %v", err)
	}
}

func TestScreenshotRemoteCommandUsesDesktopDisplayAndPNG(t *testing.T) {
	got := screenshotRemoteCommand(SSHTarget{TargetOS: targetLinux})
	for _, want := range []string{
		`DISPLAY="${DISPLAY:-:99}"`,
		"/var/lib/crabbox/desktop.env",
		"export XDG_RUNTIME_DIR WAYLAND_DISPLAY",
		"command -v grim",
		"grim -",
		"command -v scrot",
		"scrot -z -o",
		"cat \"$tmp\"",
		"import -window root png:-",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("screenshot command missing %q:\n%s", want, got)
		}
	}
}

func TestScreenshotRemoteCommandSupportsWindowsAndMacOS(t *testing.T) {
	windows := screenshotRemoteCommand(SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeNormal})
	for _, want := range []string{
		"System.Windows.Forms",
		"ImageFormat]::Png",
		"& schtasks.exe @createArgs",
		"/IT",
		"windows.password",
	} {
		if !strings.Contains(windows, want) {
			t.Fatalf("windows screenshot command missing %q:\n%s", want, windows)
		}
	}
	mac := screenshotRemoteCommand(SSHTarget{TargetOS: targetMacOS})
	if !strings.Contains(mac, "screencapture -x -t png -") {
		t.Fatalf("mac screenshot command=%s", mac)
	}
}

func TestIsLocalHost(t *testing.T) {
	for _, host := range []string{"", "localhost", "127.0.0.1", "::1"} {
		if !isLocalHost(host) {
			t.Fatalf("expected %q to be local", host)
		}
	}
	if isLocalHost("example.com") {
		t.Fatal("example.com must not be local")
	}
}
