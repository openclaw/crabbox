//go:build windows

package hyperv

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestPowerShellCredentialPreludeFailsClosedWhenCredentialCannotBeBuilt(t *testing.T) {
	script := powershellCredentialPrelude("") + `Write-Output 'reached-after-prelude'`

	command := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	command.Env = append(os.Environ(), "_CRABBOX_GP=secret")
	output, err := command.CombinedOutput()
	if err == nil || strings.Contains(string(output), "reached-after-prelude") {
		t.Fatalf("script continued after a failed credential construction (err=%v): %s", err, output)
	}
}

func TestPowerShellCredentialPreludeDoesNotRequireSecurityModule(t *testing.T) {
	plainText := strings.Repeat("x", 27)
	script := powershellCredentialPrelude("crabbox") +
		`Write-Output $cred.UserName; Write-Output $cred.Password.Length`

	command := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	command.Env = append(os.Environ(), "PSModulePath=", "_CRABBOX_GP="+plainText)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("PowerShell credential construction failed without PSModulePath: %v\n%s", err, output)
	}
	lines := strings.Fields(string(output))
	if len(lines) != 2 || lines[0] != "crabbox" || lines[1] != "27" {
		t.Fatalf("credential output = %q, want user and password length", output)
	}
}
