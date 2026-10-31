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

func TestManagedWindowsDesktopDisplayPolicyDisablesTimeoutsAndScreensaver(t *testing.T) {
	ps, err := exec.LookPath("powershell.exe")
	if err != nil {
		if ps, err = exec.LookPath("pwsh"); err != nil {
			t.Skip("PowerShell not available")
		}
	}
	// Stubs shadow powercfg.exe and the registry cmdlets, so the policy runs
	// for real without touching the host's power plan or registry.
	stubs := `$ErrorActionPreference = "Stop"
$script:calls = [System.Collections.Generic.List[string]]::new()
function powercfg.exe { $script:calls.Add("powercfg " + ($args -join " ")); $global:LASTEXITCODE = [int]$env:FAKE_POWERCFG_EXIT }
function Test-Path { param([string]$LiteralPath) $true }
function Set-ItemProperty { param([string]$LiteralPath, [string]$Name, [string]$Value, [string]$Type) $script:calls.Add("set $LiteralPath $Name=$Value") }
`
	run := func(t *testing.T, powercfgExit string) (string, error) {
		t.Helper()
		script := filepath.Join(t.TempDir(), "display.ps1")
		body := stubs + managedWindowsDesktopDisplayPowerShell() + "\n$script:calls -join \"`n\"\n"
		if err := os.WriteFile(script, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(ps, "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", script)
		cmd.Env = append(os.Environ(), "FAKE_POWERCFG_EXIT="+powercfgExit)
		out, err := cmd.CombinedOutput()
		return strings.ReplaceAll(string(out), "\r\n", "\n"), err
	}

	out, err := run(t, "0")
	if err != nil {
		t.Fatalf("display policy: %v: %s", err, out)
	}
	for _, want := range []string{
		"powercfg /change monitor-timeout-ac 0",
		"powercfg /change monitor-timeout-dc 0",
		"powercfg /change standby-timeout-ac 0",
		"powercfg /change standby-timeout-dc 0",
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("display policy did not run %q:\n%s", want, out)
		}
	}
	for _, key := range []string{`HKCU:\Control Panel\Desktop`, `Registry::HKEY_USERS\.DEFAULT\Control Panel\Desktop`} {
		for _, value := range []string{"ScreenSaveActive=0", "ScreenSaveTimeOut=0", "ScreenSaverIsSecure=0"} {
			if !strings.Contains(out, "set "+key+" "+value) {
				t.Errorf("display policy did not set %s %s:\n%s", key, value, out)
			}
		}
	}

	if out, err := run(t, "1"); err == nil || !strings.Contains(out, "failed to disable desktop power timeout monitor-timeout-ac") {
		t.Fatalf("display policy accepted a failed powercfg: err=%v out=%s", err, out)
	}
}

func TestManagedWindowsDesktopMovesAutoLogonPasswordToLSASecret(t *testing.T) {
	ps, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Skip("Windows PowerShell not available")
	}
	// A fake Crabbox.AutoLogonSecret and stubbed registry cmdlets stand in for
	// LSA and HKLM, so the step runs for real without touching the host.
	stubs := `$ErrorActionPreference = "Stop"
Add-Type -TypeDefinition 'namespace Crabbox { public static class AutoLogonSecret { public static string Stored; public static void StoreDefaultPassword(string p) { if (System.Environment.GetEnvironmentVariable("FAKE_LSA_FAIL") == "1") { throw new System.InvalidOperationException("fixture LSA failure"); } Stored = p; } } }'
$script:calls = [System.Collections.Generic.List[string]]::new()
$script:registryValue = "present"
function Remove-ItemProperty { param([string]$LiteralPath, [string]$Name) $script:calls.Add("remove $LiteralPath $Name stored=" + ($null -ne [Crabbox.AutoLogonSecret]::Stored)); if ($env:FAKE_REGISTRY_STUCK -ne "1") { $script:registryValue = $null } }
function Get-ItemProperty { param([string]$LiteralPath, [string]$Name) if ($null -ne $script:registryValue) { [pscustomobject]@{ DefaultPassword = $script:registryValue } } }
$userPassword = " exact p@ss word "
`
	run := func(t *testing.T, env ...string) (string, error) {
		t.Helper()
		script := filepath.Join(t.TempDir(), "autologon.ps1")
		body := stubs + managedWindowsDesktopAutoLogonSecretPowerShell() + "\n\"stored=[\" + [Crabbox.AutoLogonSecret]::Stored + \"]\"\n$script:calls -join \"`n\"\n"
		if err := os.WriteFile(script, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(ps, "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", script)
		cmd.Env = append(os.Environ(), env...)
		out, err := cmd.CombinedOutput()
		return strings.ReplaceAll(string(out), "\r\n", "\n"), err
	}

	out, err := run(t)
	if err != nil {
		t.Fatalf("auto-logon secret step: %v: %s", err, out)
	}
	if !strings.Contains(out, "stored=[ exact p@ss word ]") {
		t.Fatalf("LSA secret did not receive the exact password:\n%s", out)
	}
	if !strings.Contains(out, `remove HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion\Winlogon DefaultPassword stored=True`) {
		t.Fatalf("registry DefaultPassword was not removed after the LSA secret was stored:\n%s", out)
	}

	out, err = run(t, "FAKE_LSA_FAIL=1")
	if err == nil || !strings.Contains(out, "fixture LSA failure") || strings.Contains(out, "remove ") {
		t.Fatalf("a failed LSA store must stop before removing the registry value: err=%v out=%s", err, out)
	}

	out, err = run(t, "FAKE_REGISTRY_STUCK=1")
	if err == nil || !strings.Contains(out, "auto-logon password is still stored in the Winlogon registry") {
		t.Fatalf("a registry value that survives removal must fail: err=%v out=%s", err, out)
	}

	// The real LSA type compiles; it is not called here.
	compile := filepath.Join(t.TempDir(), "compile.ps1")
	if err := os.WriteFile(compile, []byte("Add-Type -TypeDefinition @'\n"+crabboxAutoLogonSecretSource+"'@\n[Crabbox.AutoLogonSecret].GetMethod('StoreDefaultPassword') -ne $null\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(ps, "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", compile).CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != "True" {
		t.Fatalf("LSA secret type does not compile: %v: %s", err, out)
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
