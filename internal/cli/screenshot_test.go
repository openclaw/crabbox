package cli

import (
	"strings"
	"testing"
)

func TestDefaultScreenshotPath(t *testing.T) {
	if got := defaultScreenshotPath("cbx_123", "Blue Lobster"); got != "crabbox-blue-lobster-screenshot.png" {
		t.Fatalf("path=%q", got)
	}
	if got := defaultScreenshotPath("cbx_123", ""); got != "crabbox-cbx-123-screenshot.png" {
		t.Fatalf("fallback path=%q", got)
	}
}

func TestScreenshotRemoteCommandUsesDesktopDisplayAndPNG(t *testing.T) {
	got := screenshotRemoteCommand(SSHTarget{TargetOS: targetLinux})
	for _, want := range []string{
		`DISPLAY="${DISPLAY:-:99}"`,
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
		"schtasks.exe /Create",
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
