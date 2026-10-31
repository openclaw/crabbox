package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The launcher service runs as LocalSystem and starts queued scripts in an
// interactive session. A request names its user, so a request whose user has
// no active session must be refused instead of running in another user's
// session.
func TestWindowsDesktopLauncherRefusesUsersWithoutAnActiveSession(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows session selection")
	}
	installer := windowsDesktopLauncherServicePowerShell()
	start := strings.Index(installer, "$desktopLauncherSource = @'\n")
	end := strings.Index(installer, "\n'@\n")
	if start < 0 || end < start {
		t.Fatal("launcher service source not found in installer")
	}
	source := installer[start+len("$desktopLauncherSource = @'\n") : end]
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "launcher.cs")
	if err := os.WriteFile(sourcePath, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "select.ps1")
	body := `$ErrorActionPreference = "Stop"
Add-Type -TypeDefinition (Get-Content -Raw -LiteralPath ` + psQuote(sourcePath) + `) -Language CSharp -ReferencedAssemblies "System.ServiceProcess.dll"
$method = [CrabboxDesktopLauncherService].GetMethod("ActiveSessionId", [Reflection.BindingFlags]"NonPublic, Static")
try {
  $session = $method.Invoke($null, @("crabbox-no-such-user-7f3a"))
  Write-Output ("selected session " + $session)
} catch {
  $inner = $_.Exception
  while ($inner.InnerException) { $inner = $inner.InnerException }
  Write-Output ("refused: " + $inner.Message)
}
`
	if err := os.WriteFile(script, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", script).CombinedOutput()
	if err != nil {
		t.Fatalf("compile and select session: %v: %s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "refused: no active interactive Windows session for requested user" {
		t.Fatalf("request for a user without an active session: %q", got)
	}
}
