package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

func TestLoadRunScriptUsesContentHashedStandalonePath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "scripts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "check.sh")
	if err := os.WriteFile(source, []byte("#!/bin/sh\necho first\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	first, err := loadRunScript(source, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if matched, err := regexp.MatchString(`^\.crabbox/scripts/[0-9a-f]{12}-check\.sh$`, filepath.ToSlash(first.RemotePath)); err != nil || !matched {
		t.Fatalf("remote path=%q, want content-hashed standalone path", first.RemotePath)
	}

	if err := os.WriteFile(source, []byte("#!/bin/sh\necho second\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	second, err := loadRunScript(source, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.RemotePath == second.RemotePath {
		t.Fatalf("remote path did not change with script content: %q", first.RemotePath)
	}
}

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

func TestRemoteRunScriptCommandUsesWorkdirAndUploadedScriptIdentity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell execution")
	}
	workdir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	remotePath := ".crabbox/scripts/abc-check.sh"
	fullPath := filepath.Join(workdir, filepath.FromSlash(remotePath))
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
set -eu
if [ "$PWD" = "$1" ]; then echo PWD_WORKDIR=yes; fi
case "$0" in
  .crabbox/scripts/abc-check.sh|"$1"/.crabbox/scripts/abc-check.sh) echo ZERO_UPLOAD=yes ;;
esac
script_dir=$(cd "$(dirname "$0")" && pwd -P)
upload_dir=$(cd "$1/.crabbox/scripts" && pwd -P)
if [ "$script_dir" = "$upload_dir" ]; then echo DIR_UPLOAD=yes; fi
`
	if err := os.WriteFile(fullPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	spec := &RunScriptSpec{Source: "./scripts/check.sh", RemotePath: remotePath, Shebang: true}
	command := remoteRunScriptCommandWithEnvFile(workdir, nil, "", spec, []string{workdir})
	output, err := exec.Command("bash", "-lc", command).CombinedOutput()
	if err != nil {
		t.Fatalf("execute remote script command: %v\n%s", err, output)
	}
	for _, want := range []string{"PWD_WORKDIR=yes", "ZERO_UPLOAD=yes", "DIR_UPLOAD=yes"} {
		if !strings.Contains(string(output), want) {
			t.Fatalf("script output missing %q:\n%s", want, output)
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
		`Import-CrabboxEnvFile '.crabbox\env\run.env'`,
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
