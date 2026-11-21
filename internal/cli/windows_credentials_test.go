package cli

import (
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The runner task logs on with the escrowed guest password. Passwords may carry
// Unicode and surrounding whitespace, so the start script must use the exact
// stored text; a trimmed or lossy value would fail the task logon.
func TestWindowsRunnerCredentialReadKeepsEscrowExact(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("runs the guest PowerShell with Windows PowerShell")
	}
	powershell, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Skip("Windows PowerShell is unavailable")
	}
	dir := t.TempDir()
	read := func(t *testing.T, path string) (string, error) {
		t.Helper()
		script := windowsRunnerCredentialReadPowerShell(path) + `
if ($null -eq $logonValue) { Write-Output "<absent>"; exit 0 }
Write-Output ([Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($logonValue)))
`
		out, err := exec.Command(powershell, "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	write := func(t *testing.T, name string, data []byte) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	exact := " pässwörd with spaces\t"
	got, err := read(t, write(t, "exact.password", []byte(exact)))
	if err != nil {
		t.Fatalf("read exact escrow: %v: %s", err, got)
	}
	if decoded, decodeErr := base64.StdEncoding.DecodeString(got); decodeErr != nil || string(decoded) != exact {
		t.Fatalf("escrow read as %q (%v), want the stored text %q unchanged", decoded, decodeErr, exact)
	}

	if got, err := read(t, filepath.Join(dir, "missing.password")); err != nil || got != "<absent>" {
		t.Fatalf("missing escrow: %q, %v; want the non-task fallback", got, err)
	}

	for name, data := range map[string][]byte{"empty.password": {}, "invalid.password": {0xff, 0xfe, 0x00}} {
		if got, err := read(t, write(t, name, data)); err == nil {
			t.Fatalf("%s accepted as %q; want an error before any task registration", name, got)
		}
	}
}
