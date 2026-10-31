package cli

import (
	"os/exec"
	"strings"
	"testing"
)

func TestDesktopLaunchScriptWithLauncherUpgradeGateParses(t *testing.T) {
	ps, err := exec.LookPath("powershell.exe")
	if err != nil {
		ps, err = exec.LookPath("pwsh")
	}
	if err != nil {
		t.Skip("no PowerShell available to parse the generated script")
	}
	script := windowsDesktopLaunchPowerShell(`C:\crabbox\repo`, map[string]string{"A": "b"}, []string{"cmd.exe", "/c", "echo"})
	cmd := exec.Command(ps, "-NoProfile", "-NonInteractive", "-Command",
		"$e=$null;$t=$null;[void][System.Management.Automation.Language.Parser]::ParseInput([Console]::In.ReadToEnd(),[ref]$t,[ref]$e);if($e.Count){$e|%{$_.Message};exit 1}")
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated desktop launch script does not parse: %v\n%s", err, out)
	}
}
