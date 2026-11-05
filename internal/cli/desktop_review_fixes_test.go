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

// desktopBootstrapHarnessPowerShell runs a Windows desktop bootstrap script
// for real against in-memory stand-ins for the filesystem, registry, services,
// downloads, LSA, and reboot, and prints the ordered side effects. Global
// functions shadow the cmdlets and executables the script calls, so nothing on
// the host is touched.
const desktopBootstrapHarnessPowerShell = `param([string]$Target)
$ErrorActionPreference = "Stop"
Add-Type -TypeDefinition @'
namespace Fixture { public static class Log { public static System.Collections.Generic.List<string> Events = new System.Collections.Generic.List<string>(); } }
namespace Crabbox { public static class AutoLogonSecret { public static void StoreDefaultPassword(string p) { if (System.Environment.GetEnvironmentVariable("FAKE_LSA_FAIL") == "1") { throw new System.InvalidOperationException("fixture LSA failure"); } Fixture.Log.Events.Add("lsa-store [" + p + "]"); } } }
'@
function global:Ev([string]$Event) { [Fixture.Log]::Events.Add($Event) }
$global:fs = @{}
$global:reg = @{}
$global:services = @{}
$global:winlogonKey = "HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion\Winlogon"
if ($env:FAKE_SETUP_COMPLETE -eq "1") { $global:fs["C:\ProgramData\crabbox\desktop-setup-complete"] = "done" }
if ($env:FAKE_STALE_REGISTRY_PASSWORD -eq "1") { $global:reg["$global:winlogonKey|DefaultPassword"] = "stale" }
function global:New-Item { param([string]$ItemType, [string]$Path, [switch]$Force) $global:fs[$Path] = "<dir>" }
function global:Test-Path { param([string]$LiteralPath, [string]$Path) $p = if ($LiteralPath) { $LiteralPath } else { $Path }; $global:fs.ContainsKey($p) }
function global:Set-Content { param([Parameter(ValueFromPipeline = $true)][object]$Value, [string]$Path, [string]$LiteralPath, [switch]$NoNewline, [string]$Encoding) process { $p = if ($LiteralPath) { $LiteralPath } else { $Path }; $global:fs[$p] = [string]$Value; Ev "write $p" } }
function global:Get-Content { param([string]$Path, [string]$LiteralPath, [switch]$Raw) $p = if ($LiteralPath) { $LiteralPath } else { $Path }; $global:fs[$p] }
function global:Remove-Item { param([string]$LiteralPath, [switch]$Force) $global:fs.Remove($LiteralPath) }
function global:Get-LocalUser { param([string]$Name) [pscustomobject]@{ Name = $Name; SID = [pscustomobject]@{ Value = "S-1-5-21-0-0-0-1001" } } }
function global:icacls.exe { }
function global:cmd.exe { }
function global:powercfg.exe { $global:LASTEXITCODE = 0 }
function global:Get-Process { }
function global:Stop-Process { }
function global:Start-Sleep { }
function global:Get-Service { param([string]$Name) if ($global:services.ContainsKey($Name)) { [pscustomobject]@{ Name = $Name; Status = "Running" } } }
function global:New-Service { param([string]$Name, [string]$BinaryPathName, [string]$StartupType) $global:services[$Name] = $true }
function global:Set-Service { param([Parameter(ValueFromPipeline = $true)][object]$InputObject, [string]$Name, [string]$StartupType) process { } }
function global:Start-Service { param([string]$Name) }
function global:Stop-Service { param([string]$Name, [switch]$Force) }
function global:Restart-Service { param([string]$Name) Ev "restart-service $Name" }
function global:Add-Type { param([string]$TypeDefinition, [string]$Language, [string]$OutputAssembly, [string]$OutputType, [string[]]$ReferencedAssemblies) if (-not $OutputAssembly) { throw "unexpected in-process Add-Type" }; $global:fs[$OutputAssembly] = "<exe>" }
function global:Invoke-WebRequest { param([string]$Uri, [string]$OutFile, [switch]$UseBasicParsing) $global:fs[$OutFile] = "<msi>" }
function global:Get-FileHash { param([string]$LiteralPath, [string]$Algorithm) [pscustomobject]@{ Hash = $env:FAKE_TIGHTVNC_SHA256 } }
function global:Start-Process { param([string]$FilePath, [object[]]$ArgumentList, [switch]$Wait, [switch]$PassThru) $global:services["tvnserver"] = $true; if ($PassThru) { [pscustomobject]@{ ExitCode = 0 } } }
function global:New-ItemProperty { param([string]$Path, [string]$Name, [string]$PropertyType, [object]$Value, [switch]$Force) }
function global:Set-ItemProperty { param([string]$LiteralPath, [string]$Path, [string]$Name, [object]$Value, [string]$Type) $p = if ($LiteralPath) { $LiteralPath } else { $Path }; $global:reg["$p|$Name"] = [string]$Value; if ($p -eq $global:winlogonKey) { Ev "registry-set $Name=[$Value]" } }
function global:Get-ItemProperty { param([string[]]$LiteralPath, [string[]]$Path, [string]$Name) $p = if ($LiteralPath) { $LiteralPath[0] } else { $Path[0] }; if ($global:reg.ContainsKey("$p|$Name")) { [pscustomobject]@{ $Name = $global:reg["$p|$Name"] } } }
function global:Remove-ItemProperty { param([string]$LiteralPath, [string]$Name) if ($env:FAKE_REGISTRY_STUCK -ne "1") { $global:reg.Remove("$LiteralPath|$Name") }; Ev "registry-remove $Name" }
function global:Restart-Computer { param([switch]$Force) Ev "reboot" }
$failure = ""
try { & $Target -userPassword $env:FAKE_GUEST_PASSWORD | Out-Null } catch { $failure = $_.Exception.Message }
foreach ($event in [Fixture.Log]::Events) { "event " + $event }
"registry-password=[" + $global:reg["$global:winlogonKey|DefaultPassword"] + "]"
"failure=[" + $failure + "]"
`

type desktopBootstrapRun struct {
	events           []string
	registryPassword string
	failure          string
	output           string
}

func (r desktopBootstrapRun) index(event string) int {
	for i, got := range r.events {
		if got == event {
			return i
		}
	}
	return -1
}

func (r desktopBootstrapRun) hasPrefix(prefix string) bool {
	for _, got := range r.events {
		if strings.HasPrefix(got, prefix) {
			return true
		}
	}
	return false
}

const desktopBootstrapTestPassword = " exact p@ss word "

func runDesktopBootstrapHarness(t *testing.T, script string, env ...string) desktopBootstrapRun {
	t.Helper()
	ps, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Skip("Windows PowerShell not available")
	}
	dir := t.TempDir()
	harness := filepath.Join(dir, "harness.ps1")
	target := filepath.Join(dir, "bootstrap.ps1")
	if err := os.WriteFile(harness, []byte(desktopBootstrapHarnessPowerShell), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(ps, "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", harness, "-Target", target)
	cmd.Env = append(os.Environ(), append([]string{"FAKE_GUEST_PASSWORD=" + desktopBootstrapTestPassword, "FAKE_TIGHTVNC_SHA256=" + tightVNCMSISHA256}, env...)...)
	out, err := cmd.CombinedOutput()
	text := strings.ReplaceAll(string(out), "\r\n", "\n")
	if err != nil {
		t.Fatalf("bootstrap harness: %v\n%s", err, text)
	}
	run := desktopBootstrapRun{output: text}
	for _, line := range strings.Split(text, "\n") {
		switch {
		case strings.HasPrefix(line, "event "):
			run.events = append(run.events, strings.TrimPrefix(line, "event "))
		case strings.HasPrefix(line, "registry-password=["):
			run.registryPassword = strings.TrimSuffix(strings.TrimPrefix(line, "registry-password=["), "]")
		case strings.HasPrefix(line, "failure=["):
			run.failure = strings.TrimSuffix(strings.TrimPrefix(line, "failure=["), "]")
		}
	}
	return run
}

func TestManagedWindowsDesktopBootstrapStoresAutoLogonPasswordInLSABeforeFirstReboot(t *testing.T) {
	script := managedWindowsDesktopBootstrapPowerShell("crabbox")
	lsaStore := "lsa-store [" + desktopBootstrapTestPassword + "]"

	// First pass on a fresh guest, with a registry value left by an older
	// bootstrap: the shared recipe reboots and exits partway through.
	first := runDesktopBootstrapHarness(t, script, "FAKE_STALE_REGISTRY_PASSWORD=1")
	if first.failure != "" {
		t.Fatalf("first pass failed: %s\n%s", first.failure, first.output)
	}
	reboot := first.index("reboot")
	if reboot < 0 || first.index("restart-service sshd") >= 0 {
		t.Fatalf("first pass did not stop at the setup reboot:\n%s", first.output)
	}
	if store := first.index(lsaStore); store < 0 || store > reboot {
		t.Fatalf("LSA secret was not stored with the exact password before the reboot:\n%s", first.output)
	}
	if remove := first.index("registry-remove DefaultPassword"); remove < 0 || remove > reboot {
		t.Fatalf("stale registry password was not removed before the reboot:\n%s", first.output)
	}
	if first.hasPrefix("registry-set DefaultPassword=") || first.registryPassword != "" {
		t.Fatalf("auto-logon password reached the Winlogon registry:\n%s", first.output)
	}
	if first.index("registry-set AutoAdminLogon=[1]") < 0 || first.index("registry-set DefaultUserName=[crabbox]") < 0 {
		t.Fatalf("auto-logon was not configured:\n%s", first.output)
	}

	second := runDesktopBootstrapHarness(t, script, "FAKE_SETUP_COMPLETE=1")
	if second.failure != "" || second.index("reboot") >= 0 || second.index("restart-service sshd") < 0 || second.index(lsaStore) < 0 {
		t.Fatalf("a later pass must keep the LSA secret current without rebooting:\n%s", second.output)
	}
	if second.hasPrefix("registry-set DefaultPassword=") || second.registryPassword != "" {
		t.Fatalf("auto-logon password reached the Winlogon registry on a later pass:\n%s", second.output)
	}

	failed := runDesktopBootstrapHarness(t, script, "FAKE_LSA_FAIL=1")
	if !strings.Contains(failed.failure, "fixture LSA failure") || failed.index("reboot") >= 0 || failed.hasPrefix(`write C:\ProgramData\crabbox\desktop-setup-complete`) {
		t.Fatalf("a failed LSA store must stop before the setup marker and reboot:\n%s", failed.output)
	}
	if failed.hasPrefix("registry-set DefaultPassword=") || failed.registryPassword != "" {
		t.Fatalf("a failed LSA store fell back to the Winlogon registry:\n%s", failed.output)
	}

	stuck := runDesktopBootstrapHarness(t, script, "FAKE_STALE_REGISTRY_PASSWORD=1", "FAKE_REGISTRY_STUCK=1")
	if stuck.failure != "auto-logon password is still stored in the Winlogon registry" || stuck.index("reboot") >= 0 {
		t.Fatalf("a registry password that survives removal must fail before the reboot:\n%s", stuck.output)
	}

	// The real LSA type compiles; it is not called here.
	ps, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Skip("Windows PowerShell not available")
	}
	compile := filepath.Join(t.TempDir(), "compile.ps1")
	if err := os.WriteFile(compile, []byte("Add-Type -TypeDefinition @'\n"+crabboxAutoLogonSecretSource+"'@\n[Crabbox.AutoLogonSecret].GetMethod('StoreDefaultPassword') -ne $null\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(ps, "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", compile).CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != "True" {
		t.Fatalf("LSA secret type does not compile: %v: %s", err, out)
	}
}

func TestSharedWindowsDesktopRecipeKeepsRegistryAutoLogonWithoutTheHook(t *testing.T) {
	// AWS and Azure run the shared recipe without Set-CrabboxAutoLogonPassword.
	script := `param([string]$userPassword)
` + windowsBootstrapUtilitiesPowerShell() + `
$user = "crabbox"
$base = "C:\ProgramData\crabbox"
$setupCompletePath = Join-Path $base "desktop-setup-complete"
$vncPasswordPath = Join-Path $base "vnc.password"
$tightVNCInstaller = "$env:TEMP\tightvnc.msi"
Set-Content -NoNewline -Encoding ASCII -Path $vncPasswordPath -Value "Cb1!vnc-fixture"
` + windowsDesktopBootstrapPowerShell()
	run := runDesktopBootstrapHarness(t, script)
	if run.failure != "" || run.index("reboot") < 0 {
		t.Fatalf("shared recipe did not complete its first pass:\n%s", run.output)
	}
	if run.registryPassword != desktopBootstrapTestPassword || run.hasPrefix("lsa-store") {
		t.Fatalf("shared recipe without the hook must keep the registry auto-logon password:\n%s", run.output)
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
