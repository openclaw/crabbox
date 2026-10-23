package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
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

func TestWindowsScreenshotRemoteCommandStreamsAndCleansUp(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows PowerShell behavior")
	}
	for _, result := range []string{"success", "register-error", "task-error", "timeout"} {
		t.Run(result, func(t *testing.T) {
			root := t.TempDir()
			base, receipt := filepath.Join(root, "capture"), filepath.Join(root, "removed")
			prefix := `$ErrorActionPreference = "Stop"
$base = ` + psQuote(base) + `
$receipt = ` + psQuote(receipt) + `
$result = ` + psQuote(result) + `
function Protect-CrabboxInteractiveDirectory {
  param($Path)
  [IO.Directory]::CreateDirectory($Path) | Out-Null
}
function Register-CrabboxInteractiveTask {
  param($Name, $Executable, $Arguments)
  if ($Executable -ne "powershell.exe" -or -not $Arguments.EndsWith('-File "' + $script + '"')) { throw "wrong capture command" }
  if (-not [IO.File]::Exists($script)) { throw "capture script missing" }
  if ($result -eq "register-error") { throw "fixture registration failure" }
}
function Start-CrabboxInteractiveTask {
  param($Name)
  if ($result -eq "task-error") { throw "fixture capture failure" }
  if ($result -eq "success") { [IO.File]::WriteAllBytes($out, [byte[]]@(137,80,78,71,0,255,13,10)) }
}
function Remove-CrabboxInteractiveTask {
  param($Name)
  [IO.File]::WriteAllText($receipt, $Name)
}
function Start-Sleep {}
`
			script := filepath.Join(root, "run.ps1")
			if err := os.WriteFile(script, []byte(prefix+windowsScreenshotRemoteBody()), 0o600); err != nil {
				t.Fatal(err)
			}
			output, err := exec.Command("powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", script).CombinedOutput()
			if result == "success" {
				if err != nil || !bytes.Equal(output, []byte{137, 80, 78, 71, 0, 255, 13, 10}) {
					t.Fatalf("screenshot bytes=%q error=%v", output, err)
				}
			} else {
				want := map[string]string{
					"register-error": "fixture registration failure",
					"task-error":     "fixture capture failure",
					"timeout":        "scheduled interactive screenshot did not produce output",
				}[result]
				if err == nil || !strings.Contains(string(output), want) {
					t.Fatalf("capture error=%v output=%s, want %q", err, output, want)
				}
			}
			removed, err := os.ReadFile(receipt)
			if err != nil || !strings.HasPrefix(string(removed), "CrabboxScreenshot-") {
				t.Fatalf("task removal=%q error=%v", removed, err)
			}
			entries, err := os.ReadDir(base)
			if err != nil || len(entries) != 0 {
				t.Fatalf("capture directory not cleaned: %v %v", entries, err)
			}
		})
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
