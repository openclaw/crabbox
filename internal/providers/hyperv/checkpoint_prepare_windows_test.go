//go:build windows

package hyperv

import (
	"os/exec"
	"strings"
	"testing"
)

// runGuestCheckpointPrepare runs the preparation script in Windows PowerShell with
// bcdedit replaced by a function that records its arguments, so the host's boot
// configuration is never touched.
func runGuestCheckpointPrepare(t *testing.T, bcdeditExit int) (string, error) {
	t.Helper()
	powershell, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Skip("powershell.exe unavailable")
	}
	stub := `$global:bcd=@(); function bcdedit { $global:bcd += ,(@($args) -join ' '); $global:LASTEXITCODE = ` + map[bool]string{true: "0", false: "1"}[bcdeditExit == 0] + ` }; `
	report := `; 'prepared'; $global:bcd`
	output, err := exec.Command(powershell, "-NoProfile", "-NonInteractive", "-Command", stub+guestCheckpointPrepareScript+report).CombinedOutput()
	return string(output), err
}

// The preparation runs inside the guest; a wrong bcdedit argument or a broken
// registry flush declaration would make every checkpoint create fail or leave
// restored guests at the recovery screen.
func TestGuestCheckpointPrepareScriptSetsBootPolicyAndFlushesRegistry(t *testing.T) {
	output, err := runGuestCheckpointPrepare(t, 0)
	if err != nil {
		t.Fatalf("preparation script failed (err=%v): %s", err, output)
	}
	lines := strings.Split(strings.TrimSpace(strings.ReplaceAll(output, "\r\n", "\n")), "\n")
	want := []string{"prepared", "/set {default} bootstatuspolicy IgnoreAllFailures", "/set {default} recoveryenabled Off"}
	if len(lines) != len(want) {
		t.Fatalf("preparation output=%q, want %q", lines, want)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Fatalf("preparation output=%q, want %q", lines, want)
		}
	}
}

func TestGuestCheckpointPrepareScriptStopsWhenBcdeditFails(t *testing.T) {
	output, err := runGuestCheckpointPrepare(t, 1)
	if err == nil || strings.Contains(output, "prepared") || !strings.Contains(output, "bcdedit bootstatuspolicy failed") {
		t.Fatalf("preparation continued after bcdedit failed (err=%v): %s", err, output)
	}
}

// The flush has no visible effect, so this run predefines the flush type with a
// RegFlushKey that records the handles it receives and turns the script's own
// declaration into a no-op. The script must flush exactly HKEY_LOCAL_MACHINE and
// HKEY_USERS, as the sign-extended 64-bit handles the registry API expects.
func TestGuestCheckpointPrepareScriptFlushesMachineAndUserHives(t *testing.T) {
	powershell, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Skip("powershell.exe unavailable")
	}
	recorder := `Add-Type -TypeDefinition 'namespace CrabboxCheckpoint { public static class Registry { public static System.Collections.Generic.List<long> Flushed = new System.Collections.Generic.List<long>(); public static int RegFlushKey(System.IntPtr hKey) { Flushed.Add(hKey.ToInt64()); return 0; } } }'; ` +
		`function Add-Type { }; function bcdedit { $global:LASTEXITCODE = 0 }; `
	report := `; 'flushed=' + ([CrabboxCheckpoint.Registry]::Flushed -join ',')`
	output, err := exec.Command(powershell, "-NoProfile", "-NonInteractive", "-Command", recorder+guestCheckpointPrepareScript+report).CombinedOutput()
	if err != nil || strings.TrimSpace(string(output)) != "flushed=-2147483646,-2147483645" {
		t.Fatalf("registry flush handles (err=%v): %q, want HKLM and HKU", err, output)
	}
}
