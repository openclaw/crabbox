package cli

import (
	"strings"
	"testing"
)

func TestWindowsActionsRunnerInstallScriptExceedsStdinThreshold(t *testing.T) {
	script := githubActionsRunnerInstallScriptForTarget(
		"",
		true,
		SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeNormal},
	)
	if !windowsScriptNeedsFileTransport(script) {
		t.Fatalf("windows runner install script (%d bytes) must use file transport", len(script))
	}
}

func TestActionsRunnerCredentialInputCarriesNoScript(t *testing.T) {
	input := githubActionsRunnerCredentialInput("example-org/my-app", "runner-1", "self-hosted,windows", "tok")
	lines := strings.Split(strings.TrimSuffix(input, "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("credential input must be exactly four lines, got %d: %q", len(lines), input)
	}
	if len(input) > windowsInlineScriptLimit {
		t.Fatalf("credential input (%d bytes) must stay under the stdin threshold", len(input))
	}
}
