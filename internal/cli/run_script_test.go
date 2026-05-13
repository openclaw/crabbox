package cli

import (
	"strings"
	"testing"
)

func TestRemoteRunScriptCommandUsesUploadedFile(t *testing.T) {
	spec := &RunScriptSpec{
		Source:     "live.sh",
		RemotePath: ".crabbox/scripts/abc-live.sh",
		Shebang:    true,
	}
	got := remoteRunScriptCommandWithEnvFile("/work/repo", map[string]string{"OPENAI_API_KEY": "sk-test"}, "", spec, []string{"arg one"})
	for _, want := range []string{
		"cd '/work/repo'",
		"OPENAI_API_KEY='sk-test'",
		"exec \"$@\"",
		"'.crabbox/scripts/abc-live.sh'",
		"'arg one'",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("remote command missing %q in %q", want, got)
		}
	}
}

func TestRemoteRunScriptCommandWithoutShebangUsesBash(t *testing.T) {
	spec := &RunScriptSpec{RemotePath: ".crabbox/scripts/abc-script.sh"}
	got := remoteRunScriptCommandWithEnvFile("/work/repo", nil, "", spec, nil)
	if !strings.Contains(got, `exec bash "$@"`) {
		t.Fatalf("remote command should run script through bash: %q", got)
	}
}

func TestWindowsRunScriptForTargetUsesPowerShellExtension(t *testing.T) {
	spec := runScriptForTarget(&RunScriptSpec{RemotePath: ".crabbox/scripts/abc-script"}, SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeNormal})
	if spec.RemotePath != ".crabbox/scripts/abc-script.ps1" {
		t.Fatalf("remote path=%q", spec.RemotePath)
	}
}

func TestWindowsRemoteRunScriptCommandUsesPowerShellFile(t *testing.T) {
	spec := &RunScriptSpec{RemotePath: `.crabbox\scripts\abc-script.ps1`}
	got := windowsRemoteRunScriptCommandWithEnvFiles(`C:\crabbox\repo`, map[string]string{"API_TOKEN": "secret"}, []string{`.crabbox\env\run.env`}, spec, []string{"arg one"})
	decoded := decodePowerShellCommand(t, got)
	for _, want := range []string{
		`Set-Location -LiteralPath 'C:\crabbox\repo'`,
		`Get-Content -Encoding UTF8 -LiteralPath '.crabbox\env\run.env'`,
		`$env:API_TOKEN = 'secret'`,
		`$__crabboxScript = '.crabbox\scripts\abc-script.ps1'`,
		`$__crabboxArgs = @('arg one')`,
		`powershell.exe -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File $__crabboxScript @__crabboxArgs`,
	} {
		if !strings.Contains(decoded, want) {
			t.Fatalf("windows script command missing %q in %q", want, decoded)
		}
	}
}

func TestWindowsRemoteUploadRunScriptWritesUTF8BOMBytes(t *testing.T) {
	got := windowsRemoteUploadRunScriptCommand(`C:\crabbox\repo`, `.crabbox\scripts\abc-script.ps1`)
	decoded := decodePowerShellCommand(t, got)
	for _, want := range []string{
		`Set-Location -LiteralPath 'C:\crabbox\repo'`,
		`$stdin = [Console]::OpenStandardInput()`,
		`$stdin.CopyTo($memory)`,
		`[byte[]](0xEF, 0xBB, 0xBF)`,
		`[System.IO.File]::WriteAllBytes($fullPath, $out)`,
	} {
		if !strings.Contains(decoded, want) {
			t.Fatalf("windows script upload command missing %q in %q", want, decoded)
		}
	}
	if strings.Contains(decoded, "ReadToEnd()") || strings.Contains(decoded, "WriteAllText") {
		t.Fatalf("windows script upload command should preserve bytes, got %q", decoded)
	}
}

func TestRunScriptRecordCommand(t *testing.T) {
	got := runScriptRecordCommand(&RunScriptSpec{Source: "./smoke.sh"}, []string{"--flag"})
	if strings.Join(got, " ") != "--script ./smoke.sh --flag" {
		t.Fatalf("record command=%q", got)
	}
	got = runScriptRecordCommand(&RunScriptSpec{Source: "stdin"}, nil)
	if strings.Join(got, " ") != "--script-stdin" {
		t.Fatalf("stdin record command=%q", got)
	}
}

func TestSafeScriptNameKeepsBasenameAndHash(t *testing.T) {
	got := safeScriptName("../bad live.sh", "abc123")
	if got != "abc123-badlive.sh" {
		t.Fatalf("safe name=%q", got)
	}
}
