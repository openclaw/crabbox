package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type separateLoginTestProvider struct{ Provider }

func (separateLoginTestProvider) Spec() ProviderSpec {
	return ProviderSpec{Name: "separate-login-test", DesktopLoginPasswordSeparate: true}
}

func TestWriteVNCCredentialsKeepsSeparateWindowsLoginSecretPrivate(t *testing.T) {
	RegisterProvider(separateLoginTestProvider{})
	t.Cleanup(func() { delete(providerRegistry, "separate-login-test") })
	var output bytes.Buffer
	writeVNCCredentials(
		&output,
		Config{Provider: "separate-login-test"},
		SSHTarget{TargetOS: targetWindows, User: "crabbox"},
		vncEndpoint{Managed: true},
		false,
		rfbCredentials{Username: "crabbox", Password: "vnc-only-secret"},
	)
	got := output.String()
	if !strings.Contains(got, "password: vnc-only-secret") {
		t.Fatalf("missing VNC password: %q", got)
	}
	if strings.Contains(got, "windows password: vnc-only-secret") {
		t.Fatalf("VNC secret was advertised as the Windows login password: %q", got)
	}
	if !strings.Contains(got, "windows username: crabbox") {
		t.Fatalf("missing windows username: %q", got)
	}
}

func TestWindowsDesktopVideoCommandDoesNotRequireCredentialEscrow(t *testing.T) {
	script := windowsDesktopVideoRemoteCommand("task", `C:\ProgramData\crabbox\capture.ps1`, `C:\ProgramData\crabbox\out`, `C:\ProgramData\crabbox\out.zip`, 5*time.Second)
	if strings.Contains(script, "windows.password") || strings.Contains(script, "credential file is missing") {
		t.Fatalf("video recording must not require the Windows credential escrow file: %s", script)
	}
}

func TestWindowsDesktopTerminalScopesMsysConversionToSuppliedCommand(t *testing.T) {
	interactive, err := desktopTerminalCommand(SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeNormal}, nil, desktopTerminalOptions{Sixel: true})
	if err != nil {
		t.Fatal(err)
	}
	shell := interactive[len(interactive)-1]
	if strings.Contains(shell, "MSYS") || !strings.Contains(shell, "TERM=xterm-256color") {
		t.Fatalf("interactive shell must keep MSYS conversion and sixel env: %q", shell)
	}
	command, err := desktopTerminalCommand(SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeNormal}, []string{"cmd.exe", "/c", "ver"}, desktopTerminalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(command[len(command)-1], "MSYS_NO_PATHCONV=1") {
		t.Fatalf("supplied command must keep switch preservation: %q", command)
	}
}

func TestWindowsDesktopDisplayPolicyIsScopedToManagedBootstrap(t *testing.T) {
	cfg := baseConfig()
	cfg.TargetOS = targetWindows
	cfg.WindowsMode = windowsModeNormal
	cfg.Desktop = true
	cfg.SSHUser = "crabbox"
	markers := []string{"powercfg.exe", "ScreenSaveActive", "ScreenSaverIsSecure"}
	for name, script := range map[string]string{
		"aws":   WindowsBootstrapPowerShell(cfg, "ssh-ed25519 test"),
		"azure": azureWindowsSnapshotRehydratePowerShell(cfg, "ssh-ed25519 test"),
	} {
		for _, marker := range markers {
			if strings.Contains(script, marker) {
				t.Errorf("%s desktop bootstrap must not touch power/screensaver preferences (%s)", name, marker)
			}
		}
	}
	managed := managedWindowsDesktopBootstrapPowerShell("crabbox")
	for _, marker := range markers {
		if !strings.Contains(managed, marker) {
			t.Errorf("managed desktop bootstrap must keep the display policy (%s)", marker)
		}
	}
}

func TestTightVNCReconcileRetriesAfterFailedRemoval(t *testing.T) {
	ps, err := exec.LookPath("powershell.exe")
	if err != nil {
		if ps, err = exec.LookPath("pwsh"); err != nil {
			t.Skip("PowerShell not available")
		}
	}
	dir := t.TempDir()
	server := filepath.Join(dir, "tvnserver.exe")
	if err := os.WriteFile(server, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	pending := filepath.Join(dir, "pending")
	calls := filepath.Join(dir, "calls")
	script := tightVNCReconcilePowerShell() + `
$ErrorActionPreference = "Stop"
$calls = ` + psQuote(calls) + `
$failing = { Add-Content -Path $calls -Value "fail"; throw "msi failed" }
$working = { Add-Content -Path $calls -Value "ok" }
try { Invoke-CrabboxTightVNCReconcile -PasswordChanged $true -PendingPath ` + psQuote(pending) + ` -ServerPath ` + psQuote(server) + ` -Remove $failing } catch {}
if (-not (Test-Path -LiteralPath ` + psQuote(pending) + `)) { throw "intent lost after failed removal" }
Invoke-CrabboxTightVNCReconcile -PasswordChanged $false -PendingPath ` + psQuote(pending) + ` -ServerPath ` + psQuote(server) + ` -Remove $working
if (Test-Path -LiteralPath ` + psQuote(pending) + `) { throw "intent not cleared after success" }
Invoke-CrabboxTightVNCReconcile -PasswordChanged $false -PendingPath ` + psQuote(pending) + ` -ServerPath ` + psQuote(server) + ` -Remove $working
`
	file := filepath.Join(dir, "reconcile.ps1")
	if err := os.WriteFile(file, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(ps, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", file).CombinedOutput()
	if err != nil {
		t.Fatalf("reconcile script: %v\n%s", err, out)
	}
	got, _ := os.ReadFile(calls)
	if strings.Fields(string(got))[0] != "fail" || strings.Count(string(got), "ok") != 1 {
		t.Fatalf("removal must be retried once after failure and not repeated: %q", got)
	}
}
