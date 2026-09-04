package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"
)

const powerShellEncodedCommandPrefix = "powershell.exe -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -EncodedCommand "

func TestSSHCommandContextBoundsPipeDrainAfterCancellation(t *testing.T) {
	cmd := sshCommandContext(context.Background(), SSHTarget{}, "-V")
	if cmd.WaitDelay != sshCommandWaitDelay {
		t.Fatalf("SSH command WaitDelay=%s want %s", cmd.WaitDelay, sshCommandWaitDelay)
	}
}

func TestWindowsPowerShellStdinScriptCommandUsesExactLengthFrame(t *testing.T) {
	command := windowsPowerShellStdinScriptCommand(12_345)
	if len(command) >= 8191 {
		t.Fatalf("stdin script command length=%d exceeds cmd.exe limit", len(command))
	}
	decoded := decodePowerShellCommand(t, command)
	for _, want := range []string{
		"$remaining = [Int64]12345",
		"$stdin.ReadAsync($buffer, 0, $readSize).GetAwaiter().GetResult()",
		"SSH stdin ended before the framed payload",
		"$scriptFile.Flush($true)",
		"-File $path",
	} {
		if !strings.Contains(decoded, want) {
			t.Fatalf("stdin script command missing %q:\n%s", want, decoded)
		}
	}
	if strings.Contains(decoded, "ReadToEnd") || strings.Contains(decoded, "CopyTo") {
		t.Fatalf("stdin script command still depends on EOF:\n%s", decoded)
	}
}

func TestDirectSSHExecutableSelection(t *testing.T) {
	windowsDir := t.TempDir()
	nativeSSH := filepath.Join(windowsDir, "System32", "OpenSSH", "ssh.exe")
	if err := os.MkdirAll(filepath.Dir(nativeSSH), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nativeSSH, []byte("fixture"), 0o755); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		goos       string
		windowsDir string
		want       string
	}{
		{name: "Windows native path exists", goos: "windows", windowsDir: windowsDir, want: nativeSSH},
		{name: "Windows native path missing", goos: "windows", windowsDir: filepath.Join(t.TempDir(), "missing"), want: "ssh"},
		{name: "non-Windows", goos: "linux", windowsDir: windowsDir, want: "ssh"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := directSSHExecutableForGOOS(test.goos, func(name string) string {
				if name != "WINDIR" {
					t.Fatalf("environment lookup=%q", name)
				}
				return test.windowsDir
			}, os.Stat)
			if got != test.want {
				t.Fatalf("executable=%q, want %q", got, test.want)
			}
		})
	}
}

func TestResolveWindowsNativeRsyncPairUsesExactSibling(t *testing.T) {
	toolDir := filepath.Join(t.TempDir(), "MSYS2 tools with spaces")
	if err := os.MkdirAll(toolDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rsyncPath := filepath.Join(toolDir, "rsync.exe")
	sshPath := filepath.Join(toolDir, "ssh.exe")
	for _, path := range []string{rsyncPath, sshPath} {
		if err := os.WriteFile(path, []byte("fixture"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	gotRsync, gotSSH, err := resolveWindowsNativeRsyncPair(func(name string) (string, error) {
		if name != "rsync.exe" {
			t.Fatalf("lookup=%q", name)
		}
		return rsyncPath, nil
	}, os.Stat)
	if err != nil {
		t.Fatal(err)
	}
	if gotRsync != rsyncPath || gotSSH != sshPath {
		t.Fatalf("pair=(%q, %q), want (%q, %q)", gotRsync, gotSSH, rsyncPath, sshPath)
	}
}

func TestResolveWindowsNativeRsyncPairFailsClosedWithoutSibling(t *testing.T) {
	for _, test := range []struct {
		name      string
		directory bool
	}{
		{name: "missing"},
		{name: "not a regular file", directory: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			toolDir := t.TempDir()
			rsyncPath := filepath.Join(toolDir, "rsync.exe")
			if err := os.WriteFile(rsyncPath, []byte("fixture"), 0o755); err != nil {
				t.Fatal(err)
			}
			if test.directory {
				if err := os.Mkdir(filepath.Join(toolDir, "ssh.exe"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			_, _, err := resolveWindowsNativeRsyncPair(func(string) (string, error) { return rsyncPath, nil }, os.Stat)
			var exitErr ExitError
			if !errors.As(err, &exitErr) || exitErr.Code != 2 {
				t.Fatalf("error=%v, want exit 2", err)
			}
			for _, want := range []string{"sibling OpenSSH", "ssh.exe", "WSL2"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("diagnostic=%q missing %q", err, want)
				}
			}
		})
	}
}

func TestBindWindowsNativeRsyncSSHPreservesOwnedArguments(t *testing.T) {
	sshPath := `C:\Program Files\MSYS2\usr\bin\ssh.exe`
	remoteShell := `'ssh' '-F' 'C:\Users\alice\private ssh_config' '-o' 'ProxyCommand=provider-helper token'`
	args := []string{"-az", "-e", remoteShell, "--", "source", "target:/work/repo"}
	got, err := bindWindowsNativeRsyncSSH(args, sshPath)
	if err != nil {
		t.Fatal(err)
	}
	wantShell := rsyncShellWords([]string{sshPath})[0] + remoteShell[len("'ssh'"):]
	if got[2] != wantShell {
		t.Fatalf("remote shell=%q, want %q", got[2], wantShell)
	}
	if !reflect.DeepEqual(got[:2], args[:2]) || !reflect.DeepEqual(got[3:], args[3:]) {
		t.Fatalf("paired args changed unrelated values: got=%#v want=%#v", got, args)
	}
	if args[2] != remoteShell {
		t.Fatalf("input args mutated: %#v", args)
	}
}

func TestApplyTargetChildEnvironmentAddsOverridesAndRemovesAmbientValue(t *testing.T) {
	cmd := &exec.Cmd{Env: []string{"PATH=/bin", "GH_HOST=ambient.example"}}
	applyTargetChildEnvironment(cmd, SSHTarget{
		ChildEnvDenylist: []string{"DENIED_VALUE"},
		ChildEnv:         map[string]string{"GH_HOST": "octocorp.ghe.com", "INVALID=NAME": "ignored"},
	})
	joined := strings.Join(cmd.Env, "\n")
	if strings.Count(joined, "GH_HOST=") != 1 || !strings.Contains(joined, "GH_HOST=octocorp.ghe.com") || strings.Contains(joined, "ambient.example") || strings.Contains(joined, "INVALID=NAME") {
		t.Fatalf("env=%q", cmd.Env)
	}
}

func TestExternalMacTargetScrubsDesktopPasswordFromSSHChild(t *testing.T) {
	cfg := Config{Provider: "external", TargetOS: targetMacOS}
	cfg.External.Connection.Desktop.PasswordEnv = "TEST_ARD_PASSWORD"
	target := sshTargetForLease(cfg, "example.test", "tester", "22")
	if len(target.ChildEnvDenylist) != 1 || target.ChildEnvDenylist[0] != "TEST_ARD_PASSWORD" {
		t.Fatalf("child env denylist=%v", target.ChildEnvDenylist)
	}
	if runtime.GOOS == "windows" {
		return
	}
	dir := t.TempDir()
	sshPath := filepath.Join(dir, "ssh")
	if err := os.WriteFile(sshPath, []byte("#!/bin/sh\nif [ \"${TEST_ARD_PASSWORD+x}\" = x ]; then exit 89; fi\nprintf child-env-ok\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TEST_ARD_PASSWORD", "must-not-reach-ssh")
	got, err := runSSHOutput(context.Background(), target, "true")
	if err != nil {
		t.Fatal(err)
	}
	if got != "child-env-ok" {
		t.Fatalf("output=%q", got)
	}
}

func TestExternalDesktopChildEnvDenylistAppliesToEveryTarget(t *testing.T) {
	for _, targetOS := range []string{targetLinux, targetMacOS, targetWindows} {
		cfg := Config{Provider: "external", TargetOS: targetOS}
		cfg.External.Connection.Desktop.PasswordEnv = "TEST_ARD_PASSWORD"
		if got := externalDesktopChildEnvDenylist(cfg, targetOS); len(got) != 1 || got[0] != "TEST_ARD_PASSWORD" {
			t.Fatalf("target=%s denylist=%v", targetOS, got)
		}
	}
}

func TestExternalDesktopChildEnvDenylistIsMonotonicAcrossOverridesAndEmptyClear(t *testing.T) {
	cfg := Config{Provider: "external", TargetOS: targetLinux}
	for _, name := range []string{"CURRENT_ARD_PASSWORD", "ROUTED_ARD_PASSWORD", "OVERRIDE_ARD_PASSWORD"} {
		cfg.External.Connection.Desktop.PasswordEnv = name
		PreserveExternalDesktopChildEnvironmentBoundary(&cfg)
	}
	cfg.External.Connection.Desktop.PasswordEnv = ""

	target := SSHTarget{
		User: "tester", Host: "example.test", Port: "22", TargetOS: targetLinux,
		ChildEnvDenylist: []string{"EXISTING_DENY"},
	}
	ApplyTargetChildEnvironmentBoundary(cfg, &target)
	want := []string{"EXISTING_DENY", "CURRENT_ARD_PASSWORD", "ROUTED_ARD_PASSWORD", "OVERRIDE_ARD_PASSWORD"}
	if !reflect.DeepEqual(target.ChildEnvDenylist, want) {
		t.Fatalf("child env denylist=%v, want %v", target.ChildEnvDenylist, want)
	}

	environment := append([]string{"KEEP=value"},
		"CURRENT_ARD_PASSWORD=current",
		"ROUTED_ARD_PASSWORD=routed",
		"OVERRIDE_ARD_PASSWORD=override",
	)
	filtered := strings.Join(childEnvironmentWithout(environment, target.ChildEnvDenylist...), "\n")
	if filtered != "KEEP=value" {
		t.Fatalf("filtered child environment=%q", filtered)
	}
	if runtime.GOOS == "windows" {
		return
	}
	dir := t.TempDir()
	sshPath := filepath.Join(dir, "ssh")
	script := `#!/bin/sh
if [ "${CURRENT_ARD_PASSWORD+x}" = x ]; then exit 89; fi
if [ "${ROUTED_ARD_PASSWORD+x}" = x ]; then exit 90; fi
if [ "${OVERRIDE_ARD_PASSWORD+x}" = x ]; then exit 91; fi
[ "${CRABBOX_TEST_KEEP:-}" = preserved ] || exit 92
printf child-env-ok
`
	if err := os.WriteFile(sshPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CURRENT_ARD_PASSWORD", "current-secret")
	t.Setenv("ROUTED_ARD_PASSWORD", "routed-secret")
	t.Setenv("OVERRIDE_ARD_PASSWORD", "override-secret")
	t.Setenv("CRABBOX_TEST_KEEP", "preserved")
	got, err := runSSHOutput(context.Background(), target, "true")
	if err != nil {
		t.Fatal(err)
	}
	if got != "child-env-ok" {
		t.Fatalf("output=%q", got)
	}
}

func TestExternalDesktopChildEnvDenylistConfigCopiesAreIndependent(t *testing.T) {
	original := Config{Provider: "external", TargetOS: targetLinux}
	original.External.Connection.Desktop.PasswordEnv = "ORIGINAL_ARD_PASSWORD"
	PreserveExternalDesktopChildEnvironmentBoundary(&original)

	copied := original
	copied.External.Connection.Desktop.PasswordEnv = "COPIED_ARD_PASSWORD"
	PreserveExternalDesktopChildEnvironmentBoundary(&copied)

	if got := ExternalDesktopChildEnvironmentDenylist(original); !reflect.DeepEqual(got, []string{"ORIGINAL_ARD_PASSWORD"}) {
		t.Fatalf("original denylist mutated through copy: %v", got)
	}
	if got := ExternalDesktopChildEnvironmentDenylist(copied); !reflect.DeepEqual(got, []string{"ORIGINAL_ARD_PASSWORD", "COPIED_ARD_PASSWORD"}) {
		t.Fatalf("copied denylist=%v", got)
	}
}

func TestExternalDesktopChildEnvDenylistIgnoresUntrustedAndInvalidHistoricalNames(t *testing.T) {
	cfg := Config{Provider: "external", TargetOS: targetLinux}
	cfg.External.Connection.Desktop.PasswordEnv = "TRUSTED_ARD_PASSWORD"
	cfg.credentialProvenance.externalDesktopEnv = credentialSourceTrustedFile
	PreserveExternalDesktopChildEnvironmentBoundary(&cfg)

	cfg.External.Connection.Desktop.PasswordEnv = "REPOSITORY_CHOSEN_ENV"
	cfg.credentialProvenance.externalDesktopEnv = credentialSourceRepository
	PreserveExternalDesktopChildEnvironmentBoundary(&cfg)
	cfg.External.Connection.Desktop.PasswordEnv = "PATH"
	cfg.credentialProvenance.externalDesktopEnv = credentialSourceTrustedFile
	PreserveExternalDesktopChildEnvironmentBoundary(&cfg)
	cfg.External.Connection.Desktop.PasswordEnv = "ROUTED_ARD_PASSWORD"
	cfg.credentialProvenance.externalDesktopEnv = credentialSourceTrustedFile
	PreserveExternalDesktopChildEnvironmentBoundary(&cfg)

	want := []string{"TRUSTED_ARD_PASSWORD", "ROUTED_ARD_PASSWORD"}
	if got := ExternalDesktopChildEnvironmentDenylist(cfg); !reflect.DeepEqual(got, want) {
		t.Fatalf("desktop environment denylist=%v, want %v", got, want)
	}

	approved := Config{Provider: "external", TargetOS: targetLinux}
	approved.External.Connection.Desktop.PasswordEnv = "APPROVED_ARD_PASSWORD"
	approved.credentialProvenance.externalDesktopEnv = credentialDestinationSource(
		"APPROVED_ARD_PASSWORD", "APPROVED_ARD_PASSWORD", credentialSourceRepository,
	)
	if approved.credentialProvenance.externalDesktopEnv != credentialSourceTrustedFile {
		t.Fatal("repository value matching trusted approval was not upgraded")
	}
	PreserveExternalDesktopChildEnvironmentBoundary(&approved)
	if got := ExternalDesktopChildEnvironmentDenylist(approved); !reflect.DeepEqual(got, []string{"APPROVED_ARD_PASSWORD"}) {
		t.Fatalf("approved repository denylist=%v", got)
	}
}

func TestExternalDesktopChildEnvDenylistIgnoresUntrustedAndInvalidCurrentNames(t *testing.T) {
	cfg := Config{Provider: "external", TargetOS: targetLinux}
	cfg.External.Connection.Desktop.PasswordEnv = "GH_TOKEN"
	cfg.credentialProvenance.externalDesktopEnv = credentialSourceRepository
	if got := externalDesktopChildEnvDenylist(cfg, cfg.TargetOS); len(got) != 0 {
		t.Fatalf("repository-selected current denylist=%v", got)
	}

	cfg.External.Connection.Desktop.PasswordEnv = "PATH"
	cfg.credentialProvenance.externalDesktopEnv = credentialSourceTrustedFile
	if got := externalDesktopChildEnvDenylist(cfg, cfg.TargetOS); len(got) != 0 {
		t.Fatalf("reserved current denylist=%v", got)
	}

	cfg.External.Connection.Desktop.PasswordEnv = "OPERATOR_ARD_PASSWORD"
	if got := externalDesktopChildEnvDenylist(cfg, cfg.TargetOS); !reflect.DeepEqual(got, []string{"OPERATOR_ARD_PASSWORD"}) {
		t.Fatalf("trusted current denylist=%v", got)
	}
}

func TestExternalDesktopChildEnvDenylistPreservesTrustedSecretAcrossProviderSwitch(t *testing.T) {
	cfg := Config{Provider: "aws", TargetOS: targetLinux}
	cfg.External.Connection.Desktop.PasswordEnv = "OPERATOR_ARD_PASSWORD"
	cfg.credentialProvenance.externalDesktopEnv = credentialSourceTrustedFile
	if got := externalDesktopChildEnvDenylist(cfg, cfg.TargetOS); !reflect.DeepEqual(got, []string{"OPERATOR_ARD_PASSWORD"}) {
		t.Fatalf("provider-switched trusted denylist=%v", got)
	}

	cfg.External.Connection.Desktop.PasswordEnv = "REPOSITORY_SELECTED_ENV"
	cfg.credentialProvenance.externalDesktopEnv = credentialSourceRepository
	if got := externalDesktopChildEnvDenylist(cfg, cfg.TargetOS); len(got) != 0 {
		t.Fatalf("provider-switched repository denylist=%v", got)
	}
}

func TestSystemInspectionEnvironmentExcludesAmbientSecrets(t *testing.T) {
	t.Setenv("SCREEN_SHARING_PASSWORD", "operator-secret")
	entries := systemInspectionEnvironment()
	env := strings.Join(entries, "\n")
	if strings.Contains(env, "SCREEN_SHARING_PASSWORD=") {
		t.Fatal("inspection environment exposed ambient secret variable")
	}
	if env != "LC_ALL=C" {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			name, _, _ := strings.Cut(entry, "=")
			names = append(names, name)
		}
		t.Fatalf("inspection environment names=%q", names)
	}
}

func TestVersion(t *testing.T) {
	var out bytes.Buffer
	app := App{Stdout: &out, Stderr: &bytes.Buffer{}}
	if err := app.Run(context.Background(), []string{"--version"}); err != nil {
		t.Fatalf("Run(--version) error: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != currentVersion() {
		t.Fatalf("Run(--version)=%q want %q", got, currentVersion())
	}
}

func TestRemoteCommandQuotesWorkdirEnvAndArgs(t *testing.T) {
	got := remoteCommand("/work/crabbox/cbx_1/my-app", map[string]string{"NODE_OPTIONS": "--max-old-space-size=8192"}, []string{"pnpm", "check:changed"})
	for _, want := range []string{
		"cd '/work/crabbox/cbx_1/my-app'",
		"NODE_OPTIONS='--max-old-space-size=8192'",
		"bash -lc",
		`bash -lc 'cd '\''/work/crabbox/cbx_1/my-app'\'' && exec "$@"' bash 'pnpm' 'check:changed'`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("remoteCommand() missing %q in %q", want, got)
		}
	}
}

func TestRemoteCommandDropsInvalidEnvNames(t *testing.T) {
	invalid := `PROJECT_$(touch /tmp/cbx-env-pwn)#`
	got := remoteCommand("/work/repo", map[string]string{
		"CI":    "1",
		invalid: "ignored",
	}, []string{"true"})
	if !strings.Contains(got, "CI='1'") {
		t.Fatalf("remoteCommand() missing valid environment variable in %q", got)
	}
	if strings.Contains(got, invalid) || strings.Contains(got, "$(touch") {
		t.Fatalf("remoteCommand() rendered invalid environment name in %q", got)
	}
}

func TestRemoteShellCommandRunsScript(t *testing.T) {
	got := remoteShellCommand("/work/crabbox/cbx_1/repo", map[string]string{"CI": "1"}, "pnpm install && pnpm test")
	for _, want := range []string{
		"cd '/work/crabbox/cbx_1/repo'",
		"CI='1'",
		`bash -lc 'cd '\''/work/crabbox/cbx_1/repo'\'' && pnpm install && pnpm test'`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("remoteShellCommand() missing %q in %q", want, got)
		}
	}
}

func TestShellScriptFromArgvPreservesArgumentsAroundOperators(t *testing.T) {
	got := shellScriptFromArgv([]string{"NODE_OPTIONS=--max old", "printf", "%s\n", "a b", "&&", "echo", "done"})
	want := "NODE_OPTIONS='--max old' 'printf' '%s\n' 'a b' && 'echo' 'done'"
	if got != want {
		t.Fatalf("shellScriptFromArgv()=%q want %q", got, want)
	}
}

func TestRemoteCommandSourcesActionsEnvFile(t *testing.T) {
	got := remoteCommandWithEnvFile("/home/runner/work/repo/repo", map[string]string{"CI": "1"}, "/home/runner/.crabbox/actions/cbx-123.env.sh", []string{"pnpm", "test"})
	for _, want := range []string{
		"cd '/home/runner/work/repo/repo'",
		"if [ -f '/home/runner/.crabbox/actions/cbx-123.env.sh' ]; then . '/home/runner/.crabbox/actions/cbx-123.env.sh'; fi",
		"CI='1'",
		`bash -lc 'cd '\''/home/runner/work/repo/repo'\'' && exec "$@"' bash 'pnpm' 'test'`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("remoteCommandWithEnvFile() missing %q in %q", want, got)
		}
	}
}

func TestRemoteCommandSourcesMultipleEnvFilesWithoutInlineSecret(t *testing.T) {
	got := remoteCommandWithEnvFiles("/work/repo", map[string]string{"CI": "1"}, []string{
		"/home/runner/.crabbox/actions/cbx-123.env.sh",
		".crabbox/env/run.env.sh",
	}, []string{"pnpm", "test"})
	for _, want := range []string{
		"if [ -f '/home/runner/.crabbox/actions/cbx-123.env.sh' ]; then . '/home/runner/.crabbox/actions/cbx-123.env.sh'; fi",
		"if [ -f '.crabbox/env/run.env.sh' ]; then . '.crabbox/env/run.env.sh'; fi",
		"CI='1'",
		`bash -lc 'cd '\''/work/repo'\'' && exec "$@"' bash 'pnpm' 'test'`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("remoteCommandWithEnvFiles() missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, "API_TOKEN") || strings.Contains(got, "secret") {
		t.Fatalf("remoteCommandWithEnvFiles() should not inline profile secrets: %q", got)
	}
}

func TestRemoteResetWorkdirRemovesExistingCheckout(t *testing.T) {
	got := remoteResetWorkdir("/work/crabbox/cbx_1/repo")
	for _, want := range []string{
		"rm -rf --",
		"/work/crabbox/cbx_1/repo",
		"mkdir -p",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("remoteResetWorkdir() missing %q in %q", want, got)
		}
	}
}

func TestSSHWaitNextActionMentionsFullResyncBeforeSync(t *testing.T) {
	got := sshWaitNextAction("before sync")
	if !strings.Contains(got, "--full-resync") || !strings.Contains(got, "fresh lease") {
		t.Fatalf("sshWaitNextAction(before sync)=%q", got)
	}
}

func TestWindowsNativeRemoteCommandUsesPowerShell(t *testing.T) {
	got := windowsRemoteCommandWithEnvFile(`C:\crabbox\cbx\repo`, map[string]string{"CI": "1"}, "", []string{"pwsh", "-NoProfile", "-Command", "echo ok"})
	if !strings.HasPrefix(got, powerShellEncodedCommandPrefix) {
		t.Fatalf("windows command should use encoded powershell: %q", got)
	}
	decoded := decodePowerShellCommand(t, got)
	if !strings.HasPrefix(decoded, "$ProgressPreference = \"SilentlyContinue\"\n") {
		t.Fatalf("windows command should suppress PowerShell progress records: %q", decoded)
	}
}

func TestWindowsNativeRemoteCommandDropsInvalidEnvNames(t *testing.T) {
	invalid := `PROJECT_$(touch C:\cbx-env-pwn)#`
	got := windowsRemoteCommandWithEnvFile(`C:\crabbox\cbx\repo`, map[string]string{
		"CI":    "1",
		invalid: "ignored",
	}, "", []string{"cmd.exe", "/c", "exit", "0"})
	decoded := decodePowerShellCommand(t, got)
	if !strings.Contains(decoded, `$env:CI = '1'`) {
		t.Fatalf("windows command missing valid environment variable in %q", decoded)
	}
	if strings.Contains(decoded, invalid) || strings.Contains(decoded, "$(touch") {
		t.Fatalf("windows command rendered invalid environment name in %q", decoded)
	}
}

func TestWindowsNativeRemoteCommandSourcesMultipleEnvFiles(t *testing.T) {
	got := windowsRemoteCommandWithEnvFiles(`C:\crabbox\cbx\repo`, map[string]string{"CI": "1"}, []string{
		`.crabbox\actions.env`,
		`.crabbox\env\run.env`,
	}, []string{"pwsh", "-NoProfile", "-Command", "echo ok"})
	decoded := decodePowerShellCommand(t, got)
	for _, want := range []string{
		`Import-CrabboxEnvFile '.crabbox\actions.env'`,
		`Import-CrabboxEnvFile '.crabbox\env\run.env'`,
		`$Path -match '^/([A-Za-z])/(.*)$'`,
		`$Path = ($matches[1].ToUpperInvariant() + ':\' + $matches[2].Replace('/', '\'))`,
		`(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)=(.*)$`,
		`Add-CrabboxPath $env:PNPM_HOME`,
		`Get-ChildItem -LiteralPath $nodeRoot -Recurse -Filter node.exe`,
		`$env:CI = '1'`,
	} {
		if !strings.Contains(decoded, want) {
			t.Fatalf("windows command missing %q in %q", want, decoded)
		}
	}
}

func TestWindowsNativeRemoteShellRunsScriptDirectly(t *testing.T) {
	got := windowsRemoteShellCommandWithEnvFile(`C:\crabbox\cbx\repo`, map[string]string{"CRABBOX_BROWSER": "1"}, "", `Write-Output ("COMPUTER=" + $env:COMPUTERNAME)`)
	decoded := decodePowerShellCommand(t, got)
	for _, want := range []string{
		`Set-Location -LiteralPath 'C:\crabbox\cbx\repo'`,
		`$env:CRABBOX_BROWSER = '1'`,
		`Write-Output ("COMPUTER=" + $env:COMPUTERNAME)`,
	} {
		if !strings.Contains(decoded, want) {
			t.Fatalf("windows shell command missing %q in %q", want, decoded)
		}
	}
	if strings.Contains(decoded, `& 'powershell.exe'`) {
		t.Fatalf("windows shell command should not spawn nested powershell: %q", decoded)
	}
}

func TestWindowsPruneSeededSyncManifestDeletesSeededExtras(t *testing.T) {
	got := windowsPruneSeededSyncManifest(`C:\crabbox\cbx\repo`)
	decoded := decodePowerShellCommand(t, got)
	for _, want := range []string{
		`git -c core.quotePath=false ls-files`,
		`Read-NulList $manifestBytes`,
		`Read-NulList $deletedBytes`,
		`-not $wanted.ContainsKey($rel)`,
		`$deleted.ContainsKey($rel)`,
		`Remove-SafeRepoPath $rel`,
	} {
		if !strings.Contains(decoded, want) {
			t.Fatalf("windows prune command missing %q in %q", want, decoded)
		}
	}
}

func TestSyncWindowsNativeFullResyncPrunesAfterGitSeed(t *testing.T) {
	dir := t.TempDir()
	sshPath := filepath.Join(dir, "ssh")
	logPath := filepath.Join(dir, "ssh.log")
	script := `#!/bin/sh
printf 'ssh\n' >> "$CRABBOX_FAKE_SSH_LOG"
cat >/dev/null
exit 0
`
	if err := os.WriteFile(sshPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CRABBOX_FAKE_SSH_LOG", logPath)

	repoRoot := filepath.Join(dir, "repo")
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repoRoot
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}
	if err := os.MkdirAll(repoRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit("init", "-q")
	runGit("config", "user.email", "alice@example.com")
	runGit("config", "user.name", "Alice")
	mustWriteTestFile(t, filepath.Join(repoRoot, "keep.txt"), "keep")
	mustWriteTestFile(t, filepath.Join(repoRoot, "stale.txt"), "stale")
	runGit("add", "keep.txt", "stale.txt")
	runGit("commit", "-q", "-m", "seed")
	head := gitOutput(repoRoot, "rev-parse", "HEAD")
	runGit("update-ref", "refs/remotes/origin/main", head)
	if err := os.Remove(filepath.Join(repoRoot, "stale.txt")); err != nil {
		t.Fatal(err)
	}
	manifest, err := syncManifest(repoRoot, configuredExcludes(baseConfig()))
	if err != nil {
		t.Fatal(err)
	}

	cfg := baseConfig()
	cfg.Sync.Delete = false
	target := SSHTarget{User: "crabbox", Host: "203.0.113.10", Port: "22", TargetOS: targetWindows, WindowsMode: windowsModeNormal}
	repo := Repo{Root: repoRoot, RemoteURL: "https://example.test/repo.git", Head: head}
	coherence, _ := syncGitCoherencePlan(cfg, repo)
	if err := syncWindowsNative(context.Background(), target, repo, cfg, coherence, `C:\crabbox\cbx\repo`, manifest, io.Discard, io.Discard, rsyncOptions{FullResync: true}); err != nil {
		t.Fatal(err)
	}
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Count(string(logData), "ssh\n"), 5; got != want {
		t.Fatalf("ssh calls=%d want %d; log:\n%s", got, want, logData)
	}

	if err := os.Remove(logPath); err != nil {
		t.Fatal(err)
	}
	const secret = "do-not-forward"
	repo.RemoteURL = "https://runner:" + secret + "@example.test/repo.git"
	var stderr bytes.Buffer
	coherence, blocked := syncGitCoherencePlan(cfg, repo)
	if blocked {
		warnCredentialBearingGitSeed(&stderr)
	}
	if err := syncWindowsNative(context.Background(), target, repo, cfg, coherence, `C:\crabbox\cbx\repo`, manifest, io.Discard, &stderr, rsyncOptions{FullResync: true}); err != nil {
		t.Fatal(err)
	}
	logData, err = os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Count(string(logData), "ssh\n"), 2; got != want {
		t.Fatalf("credential-blocked ssh calls=%d want %d; log:\n%s", got, want, logData)
	}
	if !strings.Contains(stderr.String(), "origin URL contains embedded credentials") {
		t.Fatalf("missing safe warning: %q", stderr.String())
	}
	if strings.Contains(stderr.String(), secret) || strings.Contains(stderr.String(), "example.test") {
		t.Fatalf("warning leaked credential-bearing remote: %q", stderr.String())
	}
}

func decodePowerShellCommand(t *testing.T, command string) string {
	t.Helper()
	if offset := strings.Index(command, "[Convert]::FromBase64String('"); offset >= 0 {
		encoded, _, ok := strings.Cut(command[offset+len("[Convert]::FromBase64String('"):], "')")
		if !ok {
			t.Fatal("invalid UTF-8 PowerShell command")
		}
		raw, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	return decodePowerShellEncodedCommand(t, command)
}

func decodePowerShellEncodedCommand(t *testing.T, command string) string {
	t.Helper()
	const prefix = powerShellEncodedCommandPrefix
	if !strings.HasPrefix(command, prefix) {
		t.Fatalf("command missing encoded powershell prefix: %q", command)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(command, prefix))
	if err != nil {
		t.Fatal(err)
	}
	if len(raw)%2 != 0 {
		t.Fatalf("odd UTF-16LE byte length: %d", len(raw))
	}
	units := make([]uint16, len(raw)/2)
	for i := range units {
		units[i] = uint16(raw[i*2]) | uint16(raw[i*2+1])<<8
	}
	return string(utf16.Decode(units))
}

func runDecodedWindowsPowerShell(t *testing.T, command string) ([]byte, error) {
	t.Helper()
	return runWindowsPowerShellScript(t, decodePowerShellCommand(t, command))
}

func windowsPowerShellScriptCommand(t *testing.T, script string) *exec.Cmd {
	t.Helper()
	powerShell, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(t.TempDir(), "script.ps1")
	if err := os.WriteFile(scriptPath, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(powerShell, "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", scriptPath)
	return cmd
}

func runWindowsPowerShellScript(t *testing.T, script string) ([]byte, error) {
	t.Helper()
	return windowsPowerShellScriptCommand(t, script).CombinedOutput()
}

func TestWSL2WrapsRemoteCommand(t *testing.T) {
	target := SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeWSL2}
	remote := `printf "ok\n"; echo 'quoted'`
	got := wrapRemoteForTarget(target, remote)
	if !strings.HasPrefix(got, powerShellEncodedCommandPrefix) {
		t.Fatalf("WSL2 command should use encoded PowerShell: %q", got)
	}
	decoded := decodePowerShellCommand(t, got)
	for _, want := range []string{
		`[Convert]::FromBase64String("`,
		`[System.IO.File]::WriteAllBytes($path, $scriptBytes)`,
		`& wsl.exe --exec bash $wslPath`,
		`$code = $LASTEXITCODE`,
		`exit $code`,
	} {
		if !strings.Contains(decoded, want) {
			t.Fatalf("WSL2 command missing %q in %q", want, decoded)
		}
	}
	start := strings.Index(decoded, `[Convert]::FromBase64String("`)
	if start < 0 {
		t.Fatalf("WSL2 command missing base64 payload: %q", decoded)
	}
	start += len(`[Convert]::FromBase64String("`)
	end := strings.Index(decoded[start:], `")`)
	if end < 0 {
		t.Fatalf("WSL2 command has unterminated base64 payload: %q", decoded)
	}
	payload := decoded[start : start+end]
	raw, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		t.Fatalf("WSL2 command payload is not base64: %v", err)
	}
	if string(raw) != remote {
		t.Fatalf("WSL2 command payload=%q want %q", string(raw), remote)
	}
}

func TestWSL2WrapsLargeRemoteBelowWindowsCommandLimit(t *testing.T) {
	target := SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeWSL2}
	command := wrapRemoteForTarget(target, remoteEnsureLocalActionsRunEnv("cbx_example", ""))
	if len(command) >= 8191 {
		t.Fatalf("WSL2 command length=%d exceeds cmd.exe limit", len(command))
	}
}

func decodeWSLStage(t *testing.T, data []byte) (owner, helper, command string, payload []byte) {
	t.Helper()
	if len(data) < wslStageHeaderSize || string(data[:8]) != "CBXFLAT2" {
		t.Fatal("invalid envelope descriptor")
	}
	offset := wslStageHeaderSize
	take := func(size uint64) []byte {
		if size > uint64(len(data)-offset) {
			t.Fatal("envelope exceeds finite input")
		}
		value := data[offset : offset+int(size)]
		offset += int(size)
		return value
	}
	owner = string(take(uint64(binary.LittleEndian.Uint32(data[8:]))))
	helper = string(take(uint64(binary.LittleEndian.Uint32(data[12:]))))
	command = string(take(binary.LittleEndian.Uint64(data[16:])))
	payload = take(binary.LittleEndian.Uint64(data[24:]))
	if offset != len(data) {
		t.Fatal("unaccounted envelope bytes")
	}
	return
}

func captureWSLStage(t *testing.T, nonce string, inspect func(*wslStageSpool, *SSHTarget, wslStageTiming, []byte)) {
	t.Helper()
	previous := stageWSLSpool
	t.Cleanup(func() { stageWSLSpool = previous })
	stageWSLSpool = func(spool *wslStageSpool, _ context.Context, target *SSHTarget, timing wslStageTiming, _, _ string, _ io.Writer) (string, error) {
		reader, err := spool.input.reset()
		if err != nil {
			return "", err
		}
		data, err := io.ReadAll(reader)
		if err != nil {
			return "", err
		}
		spool.shell = wslStageCMD
		inspect(spool, target, timing, data)
		return nonce, nil
	}
}

func TestWSL2StagedTransportBuildsExactPrivateSpool(t *testing.T) {
	remote := strings.Repeat("# large command\n", 2048) + "printf 'exact'"
	payload := bytes.Repeat([]byte{0, 1, 2, 255}, 2<<20)
	spool, err := newWSLStageSpool(remote, payload, nil, int64(len(payload)), sshCommandLimit{execution: 15 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer spool.close()
	reader, err := spool.input.reset()
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	_, helper, command, input := decodeWSLStage(t, data)
	if command != remote || helper != wslLinuxHelper || !bytes.Equal(input, payload) || sha256.Sum256(data) != spool.digest() {
		t.Fatal("envelope changed exact bytes")
	}
	for _, index := range []int{0, 8, 40, wslStageBlindingOffset, wslStageHeaderSize - 1, len(data) - 1} {
		changed := bytes.Clone(data)
		changed[index] ^= 1
		if sha256.Sum256(changed) == spool.digest() {
			t.Fatal("descriptor or content not bound")
		}
	}
	for _, size := range []int64{0, spool.size, wslStageMaxSize} {
		launcher := wslStageLauncherCommand(strings.Repeat("f", 32), size, spool.digest(), wslStageCMD)
		if len(launcher) >= wslStageLauncherCommandLimit || launcher == "" {
			t.Fatalf("launcher bytes=%d", len(launcher))
		}
		if strings.Contains(decodePowerShellCommand(t, launcher), remote) {
			t.Fatal("command leaked into argv")
		}
	}
}

func TestWSL2StagedTransportPowerShellParses(t *testing.T) {
	powerShell, err := exec.LookPath("pwsh")
	if err != nil {
		powerShell, err = exec.LookPath("powershell.exe")
		if err != nil {
			t.Skip("PowerShell is unavailable")
		}
	}
	launcher := wslStageLauncherCommand(strings.Repeat("f", 32), 100, sha256.Sum256([]byte("test")), wslStageCMD)
	if size := len(wslStageRootPreparationCommand(strings.Repeat("a", 32))); size >= wslStageLauncherCommandLimit {
		t.Fatalf("root preparation bytes=%d", size)
	}
	_, raw := newTestWSLStageSpool(t, nil)
	owner, _, _, _ := decodeWSLStage(t, raw)
	for name, script := range map[string]string{"verifier": decodePowerShellCommand(t, launcher), "owner": owner, "root": decodePowerShellCommand(t, wslStageRootPreparationCommand(strings.Repeat("a", 32)))} {
		t.Run(name, func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), powerShell, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", `$errors=$null;[Management.Automation.Language.Parser]::ParseInput([Console]::In.ReadToEnd(),[ref]$null,[ref]$errors)|Out-Null;if($errors){$errors|%{[Console]::Error.WriteLine($_.Message)};exit 1}`)
			cmd.Stdin = strings.NewReader(script)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("PowerShell rejected %s: %v\n%s", name, err, output)
			}
		})
	}
	// Execute the complete command through the outer default shell, mocking
	// only the inner executable so this also runs on non-Windows hosts.
	script := `function powershell.exe {$script:childArgs=@($args)};` + launcher + `;ConvertTo-Json -Compress -InputObject $script:childArgs`
	output, err := exec.CommandContext(t.Context(), powerShell, "-NoProfile", "-Command", script).CombinedOutput()
	if err != nil {
		t.Fatalf("outer shell: %v %s", err, output)
	}
	var args []string
	if err := json.Unmarshal(bytes.TrimSpace(output), &args); err != nil {
		t.Fatal(err)
	}
	if len(args) != 7 || args[5] != "-Command" || args[6] != wslStagePowerShellCommand(decodePowerShellCommand(t, launcher), wslStagePowerShell) {
		t.Fatal("outer shell changed launcher arguments")
	}
}

func TestWSLHelperBootstrapPreservesExactBytes(t *testing.T) {
	helper := "# é\nprintf '%s' \"$CBX_HELPER\"\ncat\n\n\n"
	payload := []byte{0, 255, 10, 13, 65}
	for _, test := range []struct {
		name     string
		preamble []byte
		declared string
		extra    int
		fail     bool
	}{
		{name: "no preamble", declared: "0"},
		{name: "UTF8 BOM", preamble: []byte{239, 187, 191}, declared: "3"},
		{name: "wrong preamble", preamble: []byte{239, 187, 190}, declared: "3", fail: true},
		{name: "absent declared BOM", declared: "3", fail: true},
		{name: "invalid preamble length", declared: "2", fail: true},
		{name: "incomplete helper", declared: "0", extra: 1, fail: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), "sh", "-c", wslHelperBootstrap, "sh", strconv.Itoa(len(helper)+test.extra), test.declared)
			input := append(bytes.Clone(test.preamble), []byte(helper)...)
			if test.extra == 0 {
				input = append(input, payload...)
			}
			cmd.Stdin = bytes.NewReader(input)
			got, err := cmd.Output()
			if test.fail {
				if err == nil || len(got) != 0 {
					t.Fatalf("invalid preamble/helper executed: %x %v", got, err)
				}
				return
			}
			want := append([]byte(helper), payload...)
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("bootstrap bytes=%x err=%v", got, err)
			}
		})
	}
}

type localPOSIXWorkspaceOwnerTransport struct {
	home string
}

func (transport localPOSIXWorkspaceOwnerTransport) Do(ctx context.Context, req workspaceOwnerRemoteRequest) (string, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", remoteWorkspaceOwnerCommand(SSHTarget{TargetOS: targetLinux}, req))
	cmd.Env = []string{"HOME=" + transport.home, "PATH=" + os.Getenv("PATH")}
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func TestWSLStageWorkloadPreservesCallerUmask(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executes the staged WSL POSIX helper locally")
	}
	// POSIXTransportIsLoginShellIndependent covers witness masks with and without
	// stdin. Test WSL's masks and one composed witness without repeating that matrix.
	const binaryInput = "payload\x00\xff\n"
	tests := []struct {
		mask  string
		owned bool
		input string
	}{
		{mask: "022"},
		{mask: "027", input: binaryInput},
		{mask: "077", input: binaryInput},
		{mask: "027", owned: true, input: binaryInput},
	}
	for _, test := range tests {
		t.Run(fmt.Sprintf("umask-%s/owned-%t/input-%t", test.mask, test.owned, test.input != ""), func(t *testing.T) {
			home := t.TempDir()
			wantCode := 0
			if test.input != "" {
				wantCode = 23
			}
			// Relative paths keep the framed workload independent of TempDir length.
			script := `private_paths=$(find stage \( -type d ! -perm 700 -o -type f ! -perm 600 \) -print) || exit 99; [ -z "$private_paths" ] || { printf 'non-private staging paths: %s\n' "$private_paths" >&2; exit 99; }
mkdir user-directory && cat >user-file; exit ` + strconv.Itoa(wantCode)
			ctx := t.Context()
			if test.owned {
				// Staging may outlast a claim's TTL; use the same renewal lifecycle as run.
				owner, err := acquireWorkspaceOwnerWithTransport(ctx, SSHTarget{TargetOS: targetLinux}, "cbx_umask", io.Discard, localPOSIXWorkspaceOwnerTransport{home: home}, workspaceOwnerWaitTimeout, workspaceOwnerTTL, workspaceOwnerRenewInterval)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), workspaceOwnerCleanupTimeout)
					defer cancel()
					if err := owner.Close(cleanupCtx); err != nil {
						t.Errorf("release workspace owner: %v", err)
					}
				})
				ctx = owner.Context()
				script = owner.wrapPOSIXCommand(script, test.input != "")
			}
			stage := filepath.Join(home, "stage")
			if err := os.Mkdir(stage, 0o700); err != nil {
				t.Fatal(err)
			}
			for name, data := range map[string]string{
				".armed":  "",
				"command": script,
				"input":   test.input,
			} {
				if err := os.WriteFile(filepath.Join(stage, name), []byte(data), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			// Execute the actual staged workload branch, including its private
			// helper mask and restoration of the incoming caller mask.
			cmd := exec.CommandContext(ctx, "bash", "-c", wslLinuxHelper, "sh", "workload", stage, "nonce", "0", "0", "0", "0", test.mask)
			cmd.Dir = home
			cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}
			if out, err := cmd.CombinedOutput(); exitCode(err) != wantCode {
				t.Fatalf("WSL shell exit=%d want=%d err=%v output=%s", exitCode(err), wantCode, err, out)
			}
			if data, err := os.ReadFile(filepath.Join(home, "user-file")); err != nil || string(data) != test.input {
				t.Fatalf("stdin=%q want=%q err=%v", data, test.input, err)
			}
			bits, err := strconv.ParseUint(test.mask, 8, 32)
			if err != nil {
				t.Fatal(err)
			}
			for path, base := range map[string]os.FileMode{"user-file": 0o666, "user-directory": 0o777} {
				info, err := os.Stat(filepath.Join(home, path))
				if err != nil {
					t.Fatal(err)
				}
				if want := base &^ os.FileMode(bits); info.Mode().Perm() != want {
					t.Errorf("caller umask %s: %s mode=%04o want=%04o", test.mask, path, info.Mode().Perm(), want)
				}
			}
			if result, err := os.ReadFile(filepath.Join(stage, ".result")); err != nil || strings.TrimSpace(string(result)) != strconv.Itoa(wantCode) {
				t.Errorf("WSL result=%q err=%v, want %d", result, err, wantCode)
			}
		})
	}
}

func TestStaticLeaseBypassesCoordinatorAndUsesTargetServerType(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "ssh"
	cfg.Coordinator = "https://broker.example.test"
	cfg.TargetOS = targetMacOS
	cfg.Static.Host = "mac.local"
	cfg.ServerType = "c7a.48xlarge"
	cfg.ServerTypeExplicit = false
	coord, ok, err := newTargetCoordinatorClient(cfg)
	if err != nil || ok || coord != nil {
		t.Fatalf("static coordinator=%v ok=%t err=%v", coord, ok, err)
	}
	server, _, _, err := staticLease(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if server.ServerType.Name != "macos" || server.Labels["server_type"] != "macos" {
		t.Fatalf("static type=%q label=%q", server.ServerType.Name, server.Labels["server_type"])
	}
}

func TestCommandIntentArgv(t *testing.T) {
	tests := []struct {
		name    string
		command []string
		shell   bool
		literal map[int]bool
		want    []string
	}{
		{"literal argv", []string{"printf", "%s", "a b", ""}, false, nil, []string{"printf", "%s", "a b", ""}},
		{"single inferred source", []string{"printf 'a b'"}, false, nil, []string{"bash", "-lc", "printf 'a b'"}},
		{"explicit source", []string{"printf", "'%s'", "'a b'"}, true, nil, []string{"bash", "-lc", "printf '%s' 'a b'"}},
		{"empty explicit source", []string{""}, true, nil, []string{"bash", "-lc", ""}},
		{"operators", []string{"echo", "a b", "&&", "echo", "done"}, false, nil, []string{"bash", "-lc", "'echo' 'a b' && 'echo' 'done'"}},
		{"assignment", []string{"FOO=a b", "printenv", "FOO"}, false, nil, []string{"bash", "-lc", "FOO='a b' 'printenv' 'FOO'"}},
		{"invalid assignment is executable", []string{"bad-name=x", "arg"}, false, nil, []string{"bad-name=x", "arg"}},
		{"literal assignment executable", []string{"FOO=x", "argument"}, false, map[int]bool{0: true}, []string{"FOO=x", "argument"}},
		{"literal operator", []string{"echo", "&&"}, false, map[int]bool{1: true}, []string{"echo", "&&"}},
		{"literal single source", []string{"echo ok && false"}, false, map[int]bool{0: true}, []string{"echo ok && false"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			intent, err := ParseCommandIntent(tt.command, tt.shell, tt.literal)
			if err != nil {
				t.Fatal(err)
			}
			if got := intent.Argv("bash", "-lc"); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("argv=%q want=%q", got, tt.want)
			}
			// Neither caller mutations nor a rendered transport may change the intent.
			tt.command[0] = "changed"
			prefix := []string{"bash", "-lc", "spare"}[:2]
			first := intent.Argv(prefix...)
			first[0] = "changed"
			if got := intent.Argv("bash", "-lc"); !reflect.DeepEqual(got, tt.want) || prefix[0] != "bash" {
				t.Fatalf("intent or prefix aliased caller storage: argv=%q prefix=%q", got, prefix)
			}
		})
	}
	if _, err := ParseCommandIntent(nil, false, nil); err == nil || err.Error() != "missing command" {
		t.Fatalf("missing command error=%v", err)
	}
}

func TestCommandIntentNativeTransport(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX command transport")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash unavailable")
	}
	home := t.TempDir()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "FOO=x"), []byte("#!/bin/sh\nprintf literal-executable\nexit 42\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	marker := filepath.Join(home, "must-not-exist")
	tests := []struct {
		name    string
		command []string
		shell   bool
		want    string
		code    int
		literal map[int]bool
	}{
		{"literal arguments", []string{"printf", "<%s>", "a b", "", "$HOME", "$(printf bad)", "`bad`", "*.go", "a'b"}, false, "<a b><><$HOME><$(printf bad)><`bad`><*.go><a'b>", 0, nil},
		{"inferred source", []string{"printf '%s' 'raw source'"}, false, "raw source", 0, nil},
		{"operators", []string{"printf", "%s", "first", "&&", "printf", "%s", "second"}, false, "firstsecond", 0, nil},
		{"environment", []string{"CBX_NATIVE_VALUE=a b", "printenv", "CBX_NATIVE_VALUE"}, false, "a b\n", 0, nil},
		{"explicit source", []string{"printf '%s' 'explicit source'"}, true, "explicit source", 0, nil},
		{"empty source", []string{""}, true, "", 0, nil},
		{"nonzero exit", []string{"exit 42"}, true, "", 42, nil},
		{"literal assignment executable", []string{"FOO=x"}, false, "literal-executable", 42, nil},
		{"literal assignment with argument", []string{"FOO=x", "argument"}, false, "literal-executable", 42, map[int]bool{0: true}},
		{"literal separator", []string{"printf", "%s", ";", "touch", marker}, false, ";touch" + marker, 0, map[int]bool{2: true}},
		{"literal mixed with intentional operator", []string{"printf", "%s", ";", "&&", "printf", "%s", "done"}, false, ";done", 0, map[int]bool{2: true}},
	}
	for _, tt := range tests {
		for _, serialized := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/serialized=%t", tt.name, serialized), func(t *testing.T) {
				intent, err := ParseCommandIntent(tt.command, tt.shell, tt.literal)
				if err != nil {
					t.Fatal(err)
				}
				argv := intent.Argv(bash, "-lc")
				if serialized {
					argv = []string{"/bin/sh", "-c", intent.ShellCommand(bash, "-lc")}
				}
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
				cmd.Env = []string{"HOME=" + home, "PATH=" + bin + ":" + filepath.Dir(bash) + ":/usr/bin:/bin", "BASH_ENV=" + os.DevNull, "ENV=" + os.DevNull}
				out, err := cmd.CombinedOutput()
				code := 0
				if err != nil {
					var exitErr *exec.ExitError
					if !errors.As(err, &exitErr) {
						t.Fatal(err)
					}
					code = exitErr.ExitCode()
				}
				if string(out) != tt.want || code != tt.code {
					t.Fatalf("output=%q exit=%d want=%q exit=%d", out, code, tt.want, tt.code)
				}
			})
		}
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("literal separator created marker: %v", err)
	}
}

func TestShouldUseShellForControlOperators(t *testing.T) {
	if !shouldUseShell([]string{"pnpm", "install", "&&", "pnpm", "test"}) {
		t.Fatal("expected shell mode for && token")
	}
	if !shouldUseShell([]string{"pnpm install && pnpm test"}) {
		t.Fatal("expected shell mode for single shell string")
	}
	if !shouldUseShell([]string{"pnpm test"}) {
		t.Fatal("expected shell mode for single command string with spaces")
	}
	if shouldUseShell([]string{"pnpm", "test"}) {
		t.Fatal("plain argv command should not use shell")
	}
}

func TestEnvAllowlist(t *testing.T) {
	if !envAllowed("CUSTOM_TOKEN", []string{"CI", "CUSTOM_*"}) {
		t.Fatal("wildcard env allow failed")
	}
	if envAllowed("PROJECT_TOKEN", []string{"CI", "NODE_OPTIONS"}) {
		t.Fatal("unexpected env forwarding without config")
	}
}

func TestEnvAllowlistRejectsEmptyWildcardPrefix(t *testing.T) {
	if envAllowed("CRABBOX_PROOF_API_TOKEN", []string{"*"}) {
		t.Fatal("bare wildcard must not forward every local environment variable")
	}
	if envAllowed("CRABBOX_PROOF_API_TOKEN", []string{"  *  "}) {
		t.Fatal("trimmed bare wildcard must not forward every local environment variable")
	}
	if !envAllowed("PROJECT_FLAG", []string{"PROJECT_*"}) {
		t.Fatal("non-empty prefix wildcard should still work")
	}
}

func TestAllowedEnvDropsInvalidNames(t *testing.T) {
	invalid := `PROJECT_$(touch /tmp/cbx-env-pwn)#`
	t.Setenv(invalid, "1")
	got := allowedEnv([]string{"PROJECT_*"})
	if _, ok := got[invalid]; ok {
		t.Fatalf("allowedEnv() forwarded invalid environment name: %#v", got)
	}
}

func TestSSHArgsIncludeReliabilityOptions(t *testing.T) {
	t.Setenv("HOME", "/tmp/crabbox-home")
	got := strings.Join(sshArgs(SSHTarget{
		User: "crabbox",
		Host: "203.0.113.10",
		Key:  "/tmp/crabbox-lease/id_ed25519",
		Port: "2222",
	}, "true"), "\n")
	for _, want := range []string{
		"ConnectTimeout=10",
		"ConnectionAttempts=3",
		"IdentitiesOnly=yes",
		"ForwardAgent=no",
		"ForwardX11=no",
		"ForwardX11Trusted=no",
		"ServerAliveInterval=15",
		"ServerAliveCountMax=2",
		"ControlMaster=auto",
		"ControlPersist=10m",
		"ControlPath=",
		"crabbox-ssh-",
		"-%C",
		`UserKnownHostsFile=/tmp/crabbox-lease/known_hosts`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("sshArgs() missing %q in %q", want, got)
		}
	}
}

func TestSSHArgsOverrideAmbientForwardingConfig(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("OpenSSH client is unavailable")
	}
	configPath := filepath.Join(t.TempDir(), "hostile_config")
	if err := os.WriteFile(configPath, []byte("Host *\n  ForwardAgent yes\n  ForwardX11 yes\n  ForwardX11Trusted yes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := SSHTarget{User: "crabbox", Host: "203.0.113.10", Port: "2222"}
	args := append(sshBaseArgs(target), "-F", configPath, "-G", target.User+"@"+target.Host)
	out, err := exec.Command("ssh", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("ssh -G: %v: %s", err, out)
	}
	resolved := make(map[string]string)
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			resolved[fields[0]] = fields[1]
		}
	}
	for _, option := range []string{"forwardagent", "forwardx11", "forwardx11trusted"} {
		if got := resolved[option]; got != "no" {
			t.Fatalf("ssh -G resolved %s=%q, want no", option, got)
		}
	}
}

func TestSSHArgsUseStableHostKeyAlias(t *testing.T) {
	target := SSHTarget{User: "lume", Host: "192.0.2.20", Port: "22", HostKeyAlias: "crabbox-lume-worker-1"}
	got := strings.Join(sshBaseArgs(target), " ")
	for _, want := range []string{"HostKeyAlias=crabbox-lume-worker-1", "StrictHostKeyChecking=yes", "HostKeyAlgorithms=ssh-ed25519"} {
		if !strings.Contains(got, want) {
			t.Fatalf("ssh args missing %q: %q", want, got)
		}
	}
	if strings.Contains(got, "StrictHostKeyChecking=accept-new") {
		t.Fatalf("ssh args=%q", got)
	}
}

func TestSSHArgsNoInputAddsNativeStdinRedirect(t *testing.T) {
	target := SSHTarget{User: "crabbox", Host: "203.0.113.10", Port: "22"}
	if got := strings.Join(sshArgsNoInput(target, "true"), " "); !strings.Contains(" "+got+" ", " -n ") {
		t.Fatalf("sshArgsNoInput() missing -n: %q", got)
	}
	if got := strings.Join(sshArgs(target, "true"), " "); strings.Contains(" "+got+" ", " -n ") {
		t.Fatalf("sshArgs() unexpectedly contains -n: %q", got)
	}
}

func TestSSHArgsIncludeCertificateFile(t *testing.T) {
	t.Setenv("HOME", "/tmp/crabbox-home")
	got := strings.Join(sshArgs(SSHTarget{
		User:            "tenki",
		Host:            "sandbox",
		Key:             "/tmp/tenki/id_ed25519",
		CertificateFile: "/tmp/tenki/session-cert.pub",
		KnownHostsFile:  "/tmp/tenki/known_hosts_session",
		Port:            "22",
	}, "true"), "\n")
	if !strings.Contains(got, "CertificateFile=/tmp/tenki/session-cert.pub") {
		t.Fatalf("sshArgs() missing CertificateFile: %q", got)
	}
	if !strings.Contains(got, "UserKnownHostsFile=/tmp/tenki/known_hosts_session") {
		t.Fatalf("sshArgs() missing KnownHostsFile: %q", got)
	}
	if !strings.Contains(got, "ControlMaster=auto") {
		t.Fatalf("sshArgs() should keep ControlMaster enabled for cert auth: %q", got)
	}
}

func TestSSHArgsDisableHostKeyChecking(t *testing.T) {
	t.Setenv("HOME", "/tmp/crabbox-home")
	got := strings.Join(sshArgs(SSHTarget{
		User:                   "tenki",
		Host:                   "sandbox",
		Key:                    "/tmp/tenki/id_ed25519",
		Port:                   "22",
		DisableHostKeyChecking: true,
	}, "true"), "\n")
	for _, want := range []string{
		"StrictHostKeyChecking=no",
		"UserKnownHostsFile=/dev/null",
		"LogLevel=ERROR",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("sshArgs() missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, "accept-new") || strings.Contains(got, "/tmp/tenki/known_hosts") {
		t.Fatalf("sshArgs() should not use persistent known_hosts: %q", got)
	}
}

func TestSSHArgsAllowTokenUserWithoutIdentityFile(t *testing.T) {
	t.Setenv("HOME", "/tmp/crabbox-home")
	got := strings.Join(sshArgs(SSHTarget{
		User: "tok_live_secret",
		Host: "ssh.app.daytona.io",
		Port: "22",
	}, "true"), "\n")
	for _, unwanted := range []string{"-i\n", "IdentitiesOnly=yes"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("sshArgs() should omit key-only option %q when target key is empty: %q", unwanted, got)
		}
	}
	if !strings.Contains(got, "tok_live_secret@ssh.app.daytona.io") {
		t.Fatalf("sshArgs() missing token user target: %q", got)
	}
}

func TestSSHArgsAuthSecretDisablesControlMaster(t *testing.T) {
	t.Setenv("HOME", "/tmp/crabbox-home")
	got := strings.Join(sshArgs(SSHTarget{
		User:       "tok_live_secret",
		Host:       "ssh.app.daytona.io",
		Port:       "22",
		AuthSecret: true,
	}, "true"), "\n")
	for _, unwanted := range []string{"ControlMaster=auto", "ControlPersist=10m"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("sshArgs() should omit mux option %q for secret auth target: %q", unwanted, got)
		}
	}
	for _, required := range []string{"ControlMaster=no", "ControlPath=none", "ControlPersist=no"} {
		if !strings.Contains(got, required) {
			t.Fatalf("sshArgs() missing %q for secret auth target: %q", required, got)
		}
	}
}

func TestSSHArgsNoControlMaster(t *testing.T) {
	t.Setenv("HOME", "/tmp/crabbox-home")
	got := strings.Join(sshArgs(SSHTarget{
		User:            "user",
		Host:            "203.0.113.10",
		Port:            "22",
		Key:             "/tmp/key",
		NoControlMaster: true,
	}, "true"), "\n")
	for _, unwanted := range []string{"ControlMaster=auto", "ControlPersist=10m"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("sshArgs() should omit mux option %q: %q", unwanted, got)
		}
	}
	for _, required := range []string{"ControlMaster=no", "ControlPath=none", "ControlPersist=no"} {
		if !strings.Contains(got, required) {
			t.Fatalf("sshArgs() missing %q: %q", required, got)
		}
	}
}

func TestShouldRetrySSHPortOnlyForTransportExit(t *testing.T) {
	if !shouldRetrySSHPort(exec.Command("sh", "-c", "exit 255").Run()) {
		t.Fatal("ssh transport exit 255 should retry fallback ports")
	}
	if shouldRetrySSHPort(exec.Command("sh", "-c", "exit 7").Run()) {
		t.Fatal("remote command failure should not retry fallback ports")
	}
}

func TestRunIdempotentSSHCombinedOutputDoesNotRetryRemoteFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell ssh fixture")
	}
	dir := t.TempDir()
	sshPath := filepath.Join(dir, "ssh")
	callsPath := filepath.Join(dir, "calls")
	script := `#!/bin/sh
printf 'call\n' >> "$CRABBOX_FAKE_SSH_CALLS"
printf 'remote failed\n' >&2
exit 7
`
	if err := os.WriteFile(sshPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CRABBOX_FAKE_SSH_CALLS", callsPath)

	out, err := runIdempotentSSHCombinedOutput(context.Background(), SSHTarget{
		User: "crabbox",
		Host: "gateway.example",
		Port: "22",
	}, "true", 0)
	if err == nil || !strings.Contains(out, "remote failed") {
		t.Fatalf("out=%q err=%v", out, err)
	}
	calls, readErr := os.ReadFile(callsPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if got := strings.Count(string(calls), "call\n"); got != 1 {
		t.Fatalf("ssh calls=%d want 1", got)
	}
}

func TestRunSSHStreamResolvesFallbackBeforeExecution(t *testing.T) {
	dir := t.TempDir()
	sshPath := filepath.Join(dir, "ssh")
	callsPath := filepath.Join(dir, "calls")
	script := `#!/bin/sh
port=""
remote=""
while [ "$#" -gt 0 ]; do
  if [ "$1" = "-p" ]; then
    shift
    port="$1"
  fi
  remote="$1"
  shift
done
printf '%s:%s\n' "$port" "$remote" >> "$CRABBOX_FAKE_SSH_CALLS"
if [ "$port" = "2222" ]; then
  printf 'failed primary probe\n' >&2
  exit 255
fi
printf 'ok\n'
exit 0
`
	if err := os.WriteFile(sshPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CRABBOX_FAKE_SSH_CALLS", callsPath)

	var stdout, stderr bytes.Buffer
	code := runSSHStream(context.Background(), SSHTarget{
		User:          "crabbox",
		Host:          "203.0.113.10",
		Port:          "2222",
		FallbackPorts: []string{"22"},
	}, "true", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("runSSHStream exit=%d stderr=%q", code, stderr.String())
	}
	if stdout.String() != "ok\n" {
		t.Fatalf("stdout=%q want ok", stdout.String())
	}
	if stderr.String() != "" {
		t.Fatalf("successful fallback leaked probe stderr=%q", stderr.String())
	}
	calls, err := os.ReadFile(callsPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(calls) != "2222:exit 0\n22:exit 0\n22:true\n" {
		t.Fatalf("calls=%q want probe fallback then one execution", string(calls))
	}
}

func TestSSHAllProbeFailureReportsOnlyFinalDiagnostic(t *testing.T) {
	tests := []struct {
		name string
		run  func(context.Context, SSHTarget, *bytes.Buffer, *bytes.Buffer) error
	}{
		{name: "combined output", run: func(ctx context.Context, target SSHTarget, stdout, _ *bytes.Buffer) error {
			out, err := runSSHCombinedOutput(ctx, target, "true")
			stdout.WriteString(out)
			return err
		}},
		{name: "stream", run: func(ctx context.Context, target SSHTarget, stdout, stderr *bytes.Buffer) error {
			_, err := runSSHStreamResult(ctx, target, "true", stdout, stderr)
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			sshPath := filepath.Join(dir, "ssh")
			script := `#!/bin/sh
port=""
while [ "$#" -gt 0 ]; do
  if [ "$1" = "-p" ]; then shift; port="$1"; fi
  shift
done
printf 'probe-%s\n' "$port" >&2
exit 255
`
			if err := os.WriteFile(sshPath, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			target := SSHTarget{User: "crabbox", Host: "example.test", Port: "2222", FallbackPorts: []string{"22"}}
			var stdout, stderr bytes.Buffer
			err := test.run(t.Context(), target, &stdout, &stderr)
			if err == nil || exitCode(err) != 255 {
				t.Fatalf("err=%v exit=%d, want 255", err, exitCode(err))
			}
			diagnostic := stdout.String() + stderr.String()
			if diagnostic != "probe-22\n" && diagnostic != "probe-22" {
				t.Fatalf("diagnostic=%q, want only final probe", diagnostic)
			}
		})
	}
}

func TestRunSSHStreamRetriesOnlyMultiplexDescriptorFailures(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows OpenSSH does not multiplex connections")
	}

	tests := []struct {
		name            string
		mode            string
		noControlMaster bool
		authSecret      bool
		failStderr      bool
		wantCalls       int
		wantCode        int
		wantDirect      bool
	}{
		{name: "first multiplex retry succeeds", mode: "retry", wantCalls: 2},
		{name: "repeated multiplex failure disables control master", mode: "fallback", wantCalls: 3, wantDirect: true},
		{name: "generic transport failure is not replayed", mode: "generic", wantCalls: 1, wantCode: 255},
		{name: "remote failure is not replayed", mode: "remote", wantCalls: 1, wantCode: 7},
		{name: "diagnostic without transport exit is not replayed", mode: "remote-diagnostic", wantCalls: 1, wantCode: 7},
		{name: "nested remote SSH failure is not replayed", mode: "nested-ssh", wantCalls: 1, wantCode: 255},
		{name: "server disconnect log injection is not replayed", mode: "server-disconnect", wantCalls: 1, wantCode: 255},
		{name: "oversized diagnostics are drained without replay", mode: "overflow", wantCalls: 1, wantCode: 255},
		{name: "failed output still drains oversized diagnostics", mode: "overflow", failStderr: true, wantCalls: 1, wantCode: 255},
		{name: "diagnostic writer failure is not replayed", mode: "retry", failStderr: true, wantCalls: 1, wantCode: 255},
		{name: "embedded diagnostic is not replayed", mode: "embedded-diagnostic", wantCalls: 1, wantCode: 255},
		{name: "disabled control master is not replayed", mode: "fallback", noControlMaster: true, wantCalls: 1, wantCode: 255, wantDirect: true},
		{name: "secret authentication is not replayed", mode: "fallback", authSecret: true, wantCalls: 1, wantCode: 255, wantDirect: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			sshPath := filepath.Join(dir, "ssh")
			callsPath := filepath.Join(dir, "calls")
			script := `#!/bin/sh
printf '%s\n' "$*" >> "$CRABBOX_FAKE_SSH_CALLS"
diagnostics=/dev/stderr
if [ "$1" = -E ]; then
  diagnostics=$2; shift 2
  [ -p "$diagnostics" ] || exit 99
fi
attempt=$(wc -l < "$CRABBOX_FAKE_SSH_CALLS" | tr -d ' ')
case "$CRABBOX_FAKE_SSH_MODE:$attempt" in
  retry:1|fallback:1|fallback:2)
    printf 'mm_send_fd: sendmsg(0): Message too long\n' >> "$diagnostics"
    printf 'mux_client_request_session: send fds failed\n' >> "$diagnostics"
    exit 255
    ;;
  generic:*)
    printf 'connection closed\n' >&2
    exit 255
    ;;
  remote:*)
    printf 'remote command failed\n' >&2
    exit 7
    ;;
  remote-diagnostic:*)
    printf 'mux_client_request_session: send fds failed\n' >&2
    exit 7
    ;;
  nested-ssh:*)
    printf 'executed once\n'
    printf 'mm_send_fd: sendmsg(0): Message too long\n' >&2
    printf 'mux_client_request_session: send fds failed\n' >&2
    exit 255
    ;;
  server-disconnect:*)
    printf 'executed once\n'
    printf 'Received disconnect from 203.0.113.10 port 2222:11: \n' >> "$diagnostics"
    printf 'mm_send_fd: sendmsg(0): Message too long\n' >> "$diagnostics"
    printf 'mux_client_request_session: send fds failed\n' >> "$diagnostics"
    exit 255
    ;;
  embedded-diagnostic:*)
    printf 'remote output: mux_client_request_session: send fds failed\n' >&2
    exit 255
    ;;
  overflow:*)
    printf 'mm_send_fd: sendmsg(0): Message too long\nmux_client_request_session: send fds failed\n' >> "$diagnostics"
    dd if=/dev/zero bs=1024 count=1024 2>/dev/null >> "$diagnostics"
    exit 255
    ;;
esac
printf 'executed once\n'
`
			if err := os.WriteFile(sshPath, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("CRABBOX_FAKE_SSH_CALLS", callsPath)
			t.Setenv("CRABBOX_FAKE_SSH_MODE", test.mode)

			var stdout, stderr bytes.Buffer
			stderrWriter := io.Writer(&stderr)
			if test.failStderr {
				stderrWriter = failingWriter{}
			}
			code, err := runSSHStreamResult(t.Context(), SSHTarget{
				User:            "crabbox",
				Host:            "203.0.113.10",
				Port:            "2222",
				FallbackPorts:   []string{},
				NoControlMaster: test.noControlMaster,
				AuthSecret:      test.authSecret,
			}, "true", &stdout, stderrWriter)
			if code != test.wantCode {
				t.Fatalf("exit=%d err=%v stderr=%q, want exit %d", code, err, stderr.String(), test.wantCode)
			}
			if test.mode == "overflow" && !test.failStderr && (stderr.Len() > 66*1024 || !strings.Contains(stderr.String(), "diagnostics truncated after 65536 bytes")) {
				t.Fatalf("diagnostics not bounded with a truncation notice: %d bytes", stderr.Len())
			}
			calls, readErr := os.ReadFile(callsPath)
			if readErr != nil {
				t.Fatal(readErr)
			}
			lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
			if len(lines) != test.wantCalls {
				t.Fatalf("ssh calls=%d, want %d:\n%s", len(lines), test.wantCalls, calls)
			}
			if got := strings.Contains(lines[len(lines)-1], "ControlMaster=no"); !test.authSecret && got != test.wantDirect {
				t.Fatalf("last SSH invocation disables multiplexing=%t, want %t:\n%s", got, test.wantDirect, calls)
			}
			if (test.wantCode == 0 || test.mode == "nested-ssh" || test.mode == "server-disconnect") && stdout.String() != "executed once\n" {
				t.Fatalf("remote command output=%q, want exactly one execution", stdout.String())
			}
			for _, line := range lines {
				args := strings.Fields(line)
				if args[0] == "-E" {
					if _, err := os.Stat(args[1]); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("local diagnostics file not removed: %v", err)
					}
				}
			}
			if !test.failStderr && (test.mode == "retry" || test.mode == "fallback") {
				if !strings.Contains(stderr.String(), sshMuxDescriptorFailure) {
					t.Fatalf("local SSH diagnostics were not forwarded: %q", stderr.String())
				}
			}
		})
	}
}

func TestRunSSHInputReplaysAfterMultiplexDescriptorFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows OpenSSH does not multiplex connections")
	}
	dir := t.TempDir()
	sshPath := filepath.Join(dir, "ssh")
	callsPath := filepath.Join(dir, "calls")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$CRABBOX_FAKE_SSH_CALLS"
diagnostics=/dev/stderr
if [ "$1" = -E ]; then diagnostics=$2; shift 2; fi
attempt=$(wc -l < "$CRABBOX_FAKE_SSH_CALLS" | tr -d ' ')
if [ "$attempt" -lt 3 ]; then
  dd bs=1 count=3 of=/dev/null 2>/dev/null
  printf 'mm_send_fd: sendmsg(0): Message too long\n' >> "$diagnostics"
  printf 'mux_client_request_session: send fds failed\n' >> "$diagnostics"
  exit 255
fi
exec /bin/cat
`
	if err := os.WriteFile(sshPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CRABBOX_FAKE_SSH_CALLS", callsPath)

	var stdout, stderr bytes.Buffer
	err := runSSHInput(t.Context(), SSHTarget{
		User:          "crabbox",
		Host:          "203.0.113.10",
		Port:          "2222",
		FallbackPorts: []string{},
	}, "cat", strings.NewReader("complete replayed input"), &stdout, &stderr)
	if err != nil {
		t.Fatalf("SSH input failed: %v; stderr=%q", err, stderr.String())
	}
	if stdout.String() != "complete replayed input" {
		t.Fatalf("replayed stdin=%q", stdout.String())
	}
	calls, err := os.ReadFile(callsPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
	if len(lines) != 3 || !strings.Contains(lines[2], "ControlMaster=no") {
		t.Fatalf("SSH retry/fallback calls:\n%s", calls)
	}
}

func TestRunSSHStreamSerializesSharedOutputWriter(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows OpenSSH uses native shared stream spooling")
	}
	dir := t.TempDir()
	sshPath := filepath.Join(dir, "ssh")
	script := `#!/bin/sh
diagnostics=$2
i=0
while [ "$i" -lt 200 ]; do
  printf 'stdout-%s\n' "$i"
  printf 'stderr-%s\n' "$i" >&2
  printf 'local-%s\n' "$i" >> "$diagnostics"
  i=$((i + 1))
done
`
	if err := os.WriteFile(sshPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	var output bytes.Buffer
	code, err := runSSHStreamResult(t.Context(), SSHTarget{
		User: "crabbox",
		Host: "203.0.113.10",
		Port: "22",
	}, "true", &output, &output)
	if code != 0 || err != nil {
		t.Fatalf("exit=%d err=%v", code, err)
	}
	if stdout, stderr := strings.Count(output.String(), "stdout-"), strings.Count(output.String(), "stderr-"); stdout != 200 || stderr != 200 {
		t.Fatalf("combined stream lines: stdout=%d stderr=%d, want 200 each", stdout, stderr)
	}
	if got := strings.Count(output.String(), "local-"); got != 200 {
		t.Fatalf("local diagnostic lines=%d, want 200", got)
	}
}

func TestSSHMuxFailureDetectorRequiresExactDiagnosticAcrossWrites(t *testing.T) {
	prefix := "mm_send_fd: sendmsg(0): Message too long\n"
	tests := []struct {
		name   string
		chunks []string
		want   bool
	}{
		{name: "complete record", chunks: []string{prefix + sshMuxDescriptorFailure + "\n"}, want: true},
		{name: "split record", chunks: []string{prefix + "mux_client_request_", "session: send fds failed\n"}, want: true},
		{name: "unterminated record", chunks: []string{prefix + sshMuxDescriptorFailure}, want: true},
		{name: "windows line endings", chunks: []string{strings.ReplaceAll(prefix, "\n", "\r\n") + sshMuxDescriptorFailure + "\r\n"}, want: true},
		{name: "stdout descriptor failure", chunks: []string{"mm_send_fd: sendmsg(1): Broken pipe\n" + sshMuxDescriptorFailure + "\n"}, want: true},
		{name: "bare diagnostic", chunks: []string{sshMuxDescriptorFailure + "\n"}},
		{name: "embedded line", chunks: []string{"remote output: " + sshMuxDescriptorFailure + "\n"}},
		{name: "diagnostic suffix", chunks: []string{prefix + sshMuxDescriptorFailure + " unexpectedly\n"}},
		{name: "server disconnect injection", chunks: []string{"Received disconnect from server:11: \n" + prefix + sshMuxDescriptorFailure + "\n"}},
		{name: "trailing unrelated record", chunks: []string{prefix + sshMuxDescriptorFailure + "\nother failure\n"}},
		{name: "oversized log", chunks: []string{strings.Repeat("x", 513), prefix + sshMuxDescriptorFailure + "\n"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var detector sshMuxFailureDetector
			for _, chunk := range test.chunks {
				if _, err := detector.Write([]byte(chunk)); err != nil {
					t.Fatal(err)
				}
			}
			if got := detector.failed(); got != test.want {
				t.Fatalf("matched=%t, want %t", got, test.want)
			}
		})
	}
}

func TestWaitForSSHReadyRecordsProxyFallbackPort(t *testing.T) {
	dir := t.TempDir()
	sshPath := filepath.Join(dir, "ssh")
	portsPath := filepath.Join(dir, "ports")
	script := `#!/bin/sh
port=""
while [ "$#" -gt 0 ]; do
  if [ "$1" = "-p" ]; then
    shift
    port="$1"
  fi
  shift
done
printf '%s\n' "$port" >> "$CRABBOX_FAKE_SSH_PORTS"
if [ "$port" = "2222" ]; then
  exit 255
fi
exit 0
`
	if err := os.WriteFile(sshPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CRABBOX_FAKE_SSH_PORTS", portsPath)

	target := SSHTarget{
		User:           "crabbox",
		Host:           "private.example",
		Port:           "2222",
		FallbackPorts:  []string{"22"},
		SSHConfigProxy: true,
		ProxyCommand:   "provider proxy %h %p",
		ReadyCheck:     "true",
	}
	if err := waitForSSHReady(context.Background(), &target, io.Discard, "test", time.Second); err != nil {
		t.Fatalf("waitForSSHReady: %v", err)
	}
	if target.Port != "22" {
		t.Fatalf("target.Port=%q want resolved fallback port 22", target.Port)
	}
	ports, err := os.ReadFile(portsPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(ports) != "2222\n22\n" {
		t.Fatalf("ports=%q want one readiness execution per candidate", string(ports))
	}
}

func TestProxySSHReadinessPreservesCandidatesUntilSuccessfulProbe(t *testing.T) {
	dir := t.TempDir()
	sshPath := filepath.Join(dir, "ssh")
	callsPath := filepath.Join(dir, "calls")
	script := `#!/bin/sh
port=""
remote=""
while [ "$#" -gt 0 ]; do
  if [ "$1" = "-p" ]; then shift; port="$1"; fi
  remote="$1"
  shift
done
count=0
if [ -f "$CRABBOX_FAKE_SSH_CALLS" ]; then count=$(/usr/bin/wc -l < "$CRABBOX_FAKE_SSH_CALLS" | /usr/bin/tr -d ' '); fi
printf '%s:%s\n' "$port" "$remote" >> "$CRABBOX_FAKE_SSH_CALLS"
if [ "$remote" = "exit 0" ] && [ "$count" -lt 2 ]; then exit 255; fi
exit 0
`
	if err := os.WriteFile(sshPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CRABBOX_FAKE_SSH_CALLS", callsPath)
	target := SSHTarget{User: "crabbox", Host: "proxy.example", Port: "2222", FallbackPorts: []string{"22"}, SSHConfigProxy: true}
	original := target
	if err := runSSHQuietWithOptionsResolvePort(t.Context(), &target, "true", "1", "1"); exitCode(err) != 255 {
		t.Fatalf("first readiness err=%v exit=%d, want 255", err, exitCode(err))
	}
	if !reflect.DeepEqual(target, original) {
		t.Fatalf("failed readiness mutated target: got=%#v want=%#v", target, original)
	}
	if err := runSSHQuietWithOptionsResolvePort(t.Context(), &target, "true", "1", "1"); err != nil {
		t.Fatal(err)
	}
	if target.Port != "2222" || len(target.FallbackPorts) != 0 {
		t.Fatalf("successful readiness target=%#v, want pinned port 2222", target)
	}
	calls, err := os.ReadFile(callsPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(calls), "2222:exit 0\n22:exit 0\n2222:exit 0\n2222:true\n"; got != want {
		t.Fatalf("calls=%q want %q", got, want)
	}
}

func TestProxySSHReadinessPinsOnlyFullyReadyFallback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fake SSH fixture")
	}
	for _, test := range []struct {
		name string
		run  func(context.Context, *SSHTarget) bool
	}{
		{name: "wait", run: func(ctx context.Context, target *SSHTarget) bool {
			return waitForSSHReady(ctx, target, io.Discard, "test", 5*time.Second) == nil
		}},
		{name: "probe", run: func(ctx context.Context, target *SSHTarget) bool {
			return probeSSHReady(ctx, target, 5*time.Second)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			callsPath := filepath.Join(dir, "calls")
			script := `#!/bin/sh
port=
remote=
while [ "$#" -gt 0 ]; do
  if [ "$1" = "-p" ]; then shift; port="$1"; fi
  remote="$1"
  shift
done
printf '%s:%s\n' "$port" "$remote" >> "$CRABBOX_FAKE_SSH_CALLS"
if [ "$port" = "2222" ] && [ "$remote" != "exit 0" ]; then exit 1; fi
exit 0
`
			if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("CRABBOX_FAKE_SSH_CALLS", callsPath)
			target := SSHTarget{
				User: "crabbox", Host: "proxy.example", Port: "2222", FallbackPorts: []string{"22"},
				SSHConfigProxy: true, ReadyCheck: "true",
			}
			if !test.run(t.Context(), &target) || target.Port != "22" || len(target.FallbackPorts) != 0 {
				t.Fatalf("readiness did not pin the fully ready fallback: %+v", target)
			}
			calls, err := os.ReadFile(callsPath)
			if got, want := string(calls), "2222:true\n22:true\n"; err != nil || got != want {
				t.Fatalf("readiness calls=%q error=%v want=%q", got, err, want)
			}
		})
	}
}

func TestSSHReadinessProfileForTarget(t *testing.T) {
	tests := []struct {
		name               string
		target             SSHTarget
		connectTimeout     string
		connectionAttempts string
	}{
		{
			name:               "linux",
			target:             SSHTarget{TargetOS: targetLinux},
			connectTimeout:     "5",
			connectionAttempts: "1",
		},
		{
			name:               "macos",
			target:             SSHTarget{TargetOS: targetMacOS},
			connectTimeout:     "5",
			connectionAttempts: "1",
		},
		{
			name:               "windows normal",
			target:             SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeNormal},
			connectTimeout:     "10",
			connectionAttempts: "3",
		},
		{
			name:               "windows wsl2",
			target:             SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeWSL2},
			connectTimeout:     "10",
			connectionAttempts: "3",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			profile := sshReadinessProfileForTarget(test.target)
			if profile.connectTimeout != test.connectTimeout || profile.connectionAttempts != test.connectionAttempts {
				t.Fatalf("profile=%+v want ConnectTimeout=%s ConnectionAttempts=%s", profile, test.connectTimeout, test.connectionAttempts)
			}
		})
	}
}

func TestSSHReadinessCallsUseTargetProfile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fake ssh helper is only reliable on Unix hosts")
	}
	tests := []struct {
		name               string
		target             SSHTarget
		connectTimeout     string
		connectionAttempts string
	}{
		{name: "linux", target: SSHTarget{TargetOS: targetLinux}, connectTimeout: "5", connectionAttempts: "1"},
		{name: "macos", target: SSHTarget{TargetOS: targetMacOS}, connectTimeout: "5", connectionAttempts: "1"},
		{name: "windows normal", target: SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeNormal}, connectTimeout: "10", connectionAttempts: "3"},
		{name: "windows wsl2", target: SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeWSL2}, connectTimeout: "10", connectionAttempts: "3"},
	}
	for _, test := range tests {
		for _, call := range []struct {
			name string
			run  func(context.Context, *SSHTarget) bool
		}{
			{
				name: "wait",
				run: func(ctx context.Context, target *SSHTarget) bool {
					return waitForSSHReady(ctx, target, io.Discard, "test", 4*time.Second) == nil
				},
			},
			{
				name: "probe",
				run: func(ctx context.Context, target *SSHTarget) bool {
					return probeSSHReady(ctx, target, 4*time.Second)
				},
			},
		} {
			t.Run(test.name+"/"+call.name, func(t *testing.T) {
				logPath := installSSHArgsRecorder(t)
				if isWindowsWSL2Target(test.target) {
					oldProbe := probeWSLSFTPSubsystem
					probeWSLSFTPSubsystem = func(_ context.Context, _ SSHTarget, connectTimeout, attempts string, _ io.Writer) error {
						if connectTimeout != test.connectTimeout || attempts != test.connectionAttempts {
							t.Fatalf("SFTP profile timeout=%q attempts=%q", connectTimeout, attempts)
						}
						return nil
					}
					t.Cleanup(func() { probeWSLSFTPSubsystem = oldProbe })
				}
				target := test.target
				target.User = "crabbox"
				target.Host = "private.example"
				target.Port = "22"
				target.SSHConfigProxy = true
				target.ProxyCommand = "provider proxy %h %p"
				target.ReadyCheck = "true"
				if !call.run(context.Background(), &target) {
					t.Fatal("readiness call failed with fake ssh")
				}
				args := readSSHArgsRecorder(t, logPath)
				assertSSHOption(t, args, "ConnectTimeout", test.connectTimeout)
				assertSSHOption(t, args, "ConnectionAttempts", test.connectionAttempts)
			})
		}
	}
}

func installWSL2ReadinessRecorder(t *testing.T, shell, ready string) string {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls")
	script := `#!/bin/sh
port=""
remote=""
while [ "$#" -gt 0 ]; do
  if [ "$1" = "-p" ]; then shift; port="$1"; fi
  remote="$1"
  shift
done
kind=other
if [ "$remote" = "$CRABBOX_WSL_SHELL" ]; then kind=shell; fi
if [ "$remote" = "$CRABBOX_WSL_READY" ]; then kind=ready; fi
printf 'ssh:%s:%s\n' "$port" "$kind" >> "$CRABBOX_WSL_CALLS"
if [ "$kind" = ready ] && [ "${CRABBOX_WSL_READY_FAIL:-}" = 1 ]; then exit 1; fi
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CRABBOX_WSL_CALLS", logPath)
	t.Setenv("CRABBOX_WSL_SHELL", wsl2ReadinessCommand(shell))
	t.Setenv("CRABBOX_WSL_READY", wsl2ReadinessCommand(ready))
	return logPath
}

func TestWSL2ReadinessUsesDirectNoInputWrapperAndPinsFullFallback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fake ssh helper is only reliable on Unix hosts")
	}
	target := SSHTarget{User: "crabbox", Host: "example.test", Port: "2222", FallbackPorts: []string{"22"}, TargetOS: targetWindows, WindowsMode: windowsModeWSL2, ReadyCheck: "git --version"}
	logPath := installWSL2ReadinessRecorder(t, "exit 0", target.ReadyCheck)
	oldProbe := probeWSLSFTPSubsystem
	probeWSLSFTPSubsystem = func(_ context.Context, candidate SSHTarget, _, _ string, _ io.Writer) error {
		file, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		defer file.Close()
		fmt.Fprintf(file, "sftp:%s\n", candidate.Port)
		if candidate.Port == "2222" {
			return errWSLSFTPUnavailable
		}
		return nil
	}
	t.Cleanup(func() { probeWSLSFTPSubsystem = oldProbe })

	if err := probeWSL2SSHReady(t.Context(), &target, sshReadinessProfileForTarget(target), io.Discard); err != nil {
		t.Fatal(err)
	}
	if target.Port != "22" || len(target.FallbackPorts) != 0 {
		t.Fatalf("target=%+v, want fully-ready fallback pinned", target)
	}
	calls, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(calls), "ssh:2222:shell\nsftp:2222\nssh:22:shell\nsftp:22\nssh:22:ready\n"; got != want {
		t.Fatalf("calls=%q want=%q", got, want)
	}
}

func TestWSL2ReadinessAllMissingSFTPStopsWithoutRepollOrMutation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fake ssh helper is only reliable on Unix hosts")
	}
	target := SSHTarget{User: "crabbox", Host: "example.test", Port: "2222", FallbackPorts: []string{"22"}, TargetOS: targetWindows, WindowsMode: windowsModeWSL2, ReadyCheck: "true"}
	original := target
	logPath := installWSL2ReadinessRecorder(t, "exit 0", target.ReadyCheck)
	oldProbe := probeWSLSFTPSubsystem
	probes := 0
	probeWSLSFTPSubsystem = func(context.Context, SSHTarget, string, string, io.Writer) error {
		probes++
		return errWSLSFTPUnavailable
	}
	t.Cleanup(func() { probeWSLSFTPSubsystem = oldProbe })

	err := waitForSSHReady(t.Context(), &target, io.Discard, "doctor", 4*time.Second)
	if !IsWSLSFTPUnavailable(err) || probes != 2 || !reflect.DeepEqual(target, original) {
		t.Fatalf("err=%v probes=%d target=%+v", err, probes, target)
	}
	calls, readErr := os.ReadFile(logPath)
	if readErr != nil || strings.Count(string(calls), "\n") != 2 {
		t.Fatalf("calls=%q err=%v, want one shell/auth attempt per port", calls, readErr)
	}
}

func TestWSL2ReadinessMixedCandidateFailuresPreserveNonMissingError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fake ssh helper is only reliable on Unix hosts")
	}
	exit255 := exec.Command("sh", "-c", "exit 255").Run()
	for _, test := range []struct {
		name    string
		results map[string]error
	}{
		{name: "missing then transport", results: map[string]error{"2222": errWSLSFTPUnavailable, "22": exit255}},
		{name: "transport then missing", results: map[string]error{"2222": exit255, "22": errWSLSFTPUnavailable}},
	} {
		t.Run(test.name, func(t *testing.T) {
			target := SSHTarget{User: "crabbox", Host: "example.test", Port: "2222", FallbackPorts: []string{"22"}, TargetOS: targetWindows, WindowsMode: windowsModeWSL2, ReadyCheck: "true"}
			original := target
			installWSL2ReadinessRecorder(t, "exit 0", target.ReadyCheck)
			oldProbe := probeWSLSFTPSubsystem
			probeWSLSFTPSubsystem = func(_ context.Context, candidate SSHTarget, _, _ string, _ io.Writer) error {
				return test.results[candidate.Port]
			}
			t.Cleanup(func() { probeWSLSFTPSubsystem = oldProbe })

			err := probeWSL2SSHReady(t.Context(), &target, sshReadinessProfileForTarget(target), io.Discard)
			if exitCode(err) != 255 || IsWSLSFTPUnavailable(err) || !reflect.DeepEqual(target, original) {
				t.Fatalf("err=%v target=%+v", err, target)
			}
		})
	}
}

func TestWSL2ReadinessToolFailureIsNotSFTPFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fake ssh helper is only reliable on Unix hosts")
	}
	target := SSHTarget{User: "crabbox", Host: "example.test", Port: "22", FallbackPorts: []string{}, TargetOS: targetWindows, WindowsMode: windowsModeWSL2, ReadyCheck: "git --version"}
	installWSL2ReadinessRecorder(t, "exit 0", target.ReadyCheck)
	t.Setenv("CRABBOX_WSL_READY_FAIL", "1")
	oldProbe := probeWSLSFTPSubsystem
	probeWSLSFTPSubsystem = func(context.Context, SSHTarget, string, string, io.Writer) error { return nil }
	t.Cleanup(func() { probeWSLSFTPSubsystem = oldProbe })

	err := probeWSL2SSHReady(t.Context(), &target, sshReadinessProfileForTarget(target), io.Discard)
	if err == nil || IsWSLSFTPUnavailable(err) {
		t.Fatalf("tool failure misclassified: %v", err)
	}
}

func TestWSL2ReadinessCommandIsFixedDirectAndZeroInput(t *testing.T) {
	command := wsl2ReadinessCommand("printf ready")
	if len(command) >= 2048 {
		t.Fatalf("command length=%d", len(command))
	}
	script := decodePowerShellEncodedCommand(t, command)
	for _, want := range []string{"FromBase64String", "wsl.exe --exec sh -lc", "exit $LASTEXITCODE"} {
		if !strings.Contains(script, want) {
			t.Fatalf("readiness wrapper missing %q: %s", want, script)
		}
	}
	for _, absent := range []string{"OpenStandardInput", "wsl-stage", "Set-Content", "WriteAllBytes"} {
		if strings.Contains(script, absent) {
			t.Fatalf("readiness wrapper contains staged/input path %q: %s", absent, script)
		}
	}
	args := sshArgsNoInputWithOptions(SSHTarget{User: "crabbox", Host: "example.test", Port: "22"}, command, "10", "3")
	if !slices.Contains(args, "-n") {
		t.Fatalf("readiness args missing -n: %v", args)
	}
}

func TestWindowsSSHReadyProbeHonorsCallerBudget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fake ssh helper is only reliable on Unix hosts")
	}
	installSSHArgsRecorder(t)
	t.Setenv("CRABBOX_FAKE_SSH_DELAY", "0.1")
	target := SSHTarget{
		User:           "crabbox",
		Host:           "private.example",
		Port:           "22",
		TargetOS:       targetWindows,
		WindowsMode:    windowsModeNormal,
		SSHConfigProxy: true,
		ProxyCommand:   "provider proxy %h %p",
		ReadyCheck:     "true",
	}
	start := time.Now()
	if probeSSHReady(context.Background(), &target, 20*time.Millisecond) {
		t.Fatal("Windows readiness probe ignored the caller's short budget")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("probe exceeded the caller's short budget by too much: %s", elapsed)
	}
}

func TestWaitForSSHReadyHonorsDeadlineDuringWindowsProbe(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fake ssh helper is only reliable on Unix hosts")
	}
	installSSHArgsRecorder(t)
	t.Setenv("CRABBOX_FAKE_SSH_DELAY", "0.2")
	target := SSHTarget{
		User:           "crabbox",
		Host:           "private.example",
		Port:           "22",
		TargetOS:       targetWindows,
		WindowsMode:    windowsModeNormal,
		SSHConfigProxy: true,
		ProxyCommand:   "provider proxy %h %p",
		ReadyCheck:     "true",
	}
	start := time.Now()
	err := waitForSSHReady(context.Background(), &target, io.Discard, "test", 30*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out waiting for SSH") {
		t.Fatalf("waitForSSHReady error=%v, want readiness timeout", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Windows readiness attempt exceeded overall deadline by too much: %s", elapsed)
	}
}

type sshWaitProgressSignal struct {
	once  sync.Once
	ready chan struct{}
}

func (w *sshWaitProgressSignal) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.ready) })
	return len(p), nil
}

func installSSHArgsRecorder(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "ssh-args")
	script := `#!/bin/sh
printf '%s\n' "$@" > "$CRABBOX_FAKE_SSH_ARGS"
if [ -n "${CRABBOX_FAKE_SSH_DELAY:-}" ]; then
  sleep "$CRABBOX_FAKE_SSH_DELAY"
fi
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CRABBOX_FAKE_SSH_ARGS", logPath)
	return logPath
}

func readSSHArgsRecorder(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func assertSSHOption(t *testing.T, args []string, name, value string) {
	t.Helper()
	want := name + "=" + value
	for _, arg := range args {
		if arg == want {
			return
		}
	}
	t.Fatalf("SSH args missing %q: %q", want, args)
}

func TestWaitForSSHReadyPreservesCancellationCauseDuringBackoff(t *testing.T) {
	// Prove cancel is observed during the inter-attempt backoff, not only at
	// the top of the loop. Fake ssh always fails so wait enters the sleep.
	dir := t.TempDir()
	sshPath := filepath.Join(dir, "ssh")
	script := `#!/bin/sh
exit 255
`
	if err := os.WriteFile(sshPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	target := SSHTarget{
		User:           "crabbox",
		Host:           "private.example",
		Port:           "22",
		SSHConfigProxy: true,
		ProxyCommand:   "provider proxy %h %p",
		ReadyCheck:     "true",
	}

	cause := errors.New("lease disappeared during SSH readiness")
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	progress := &sshWaitProgressSignal{ready: make(chan struct{})}

	errCh := make(chan error, 1)
	go func() {
		errCh <- waitForSSHReady(ctx, &target, progress, "test", time.Minute)
	}()

	select {
	case <-progress.ready:
		// Progress is written after the failed probe and immediately before backoff.
	case err := <-errCh:
		t.Fatalf("waitForSSHReady returned before backoff: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("waitForSSHReady did not reach the backoff")
	}
	cancel(cause)

	select {
	case err := <-errCh:
		if !errors.Is(err, cause) {
			t.Fatalf("waitForSSHReady returned %v, want cancellation cause %v", err, cause)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("waitForSSHReady did not return within 3s after cancel; still blocked on bare sleep")
	}
}

func TestWaitForLoopbackVNCRecordsResolvedFallbackPort(t *testing.T) {
	dir := t.TempDir()
	sshPath := filepath.Join(dir, "ssh")
	portsPath := filepath.Join(dir, "ports")
	script := `#!/bin/sh
port=""
while [ "$#" -gt 0 ]; do
  if [ "$1" = "-p" ]; then
    shift
    port="$1"
  fi
  shift
done
printf '%s\n' "$port" >> "$CRABBOX_FAKE_SSH_PORTS"
if [ "$port" = "2222" ]; then
  exit 255
fi
exit 0
`
	if err := os.WriteFile(sshPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CRABBOX_FAKE_SSH_PORTS", portsPath)

	target := SSHTarget{
		User:          "ec2-user",
		Host:          "203.0.113.10",
		Port:          "2222",
		FallbackPorts: []string{"22"},
		TargetOS:      targetMacOS,
	}
	if err := waitForLoopbackVNC(context.Background(), &target); err != nil {
		t.Fatalf("waitForLoopbackVNC failed: %v", err)
	}
	if target.Port != "22" {
		t.Fatalf("target.Port=%q want resolved fallback port 22", target.Port)
	}
	ports, err := os.ReadFile(portsPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(ports) != "2222\n22\n" {
		t.Fatalf("ports=%q want outer fallback sequence", string(ports))
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, fmt.Errorf("capture disk full")
}

func TestRunSSHStreamResultReturnsWriterErrors(t *testing.T) {
	dir := t.TempDir()
	sshPath := filepath.Join(dir, "ssh")
	script := `#!/bin/sh
printf 'hello\n'
exit 0
`
	if err := os.WriteFile(sshPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	code, err := runSSHStreamResult(context.Background(), SSHTarget{
		User: "crabbox",
		Host: "203.0.113.10",
		Port: "22",
	}, "true", failingWriter{}, io.Discard)
	if code != 1 {
		t.Fatalf("code=%d want 1", code)
	}
	if err == nil || !strings.Contains(err.Error(), "capture disk full") {
		t.Fatalf("err=%v want capture disk full", err)
	}
	if isSSHCommandExitError(err) {
		t.Fatalf("writer error should not be treated as SSH exit error: %v", err)
	}
}

func TestSSHCommandLineRedactsSecretAuthUser(t *testing.T) {
	target := SSHTarget{
		User:       "tok_live_secret",
		Host:       "ssh.app.daytona.io",
		Port:       "22",
		AuthSecret: true,
	}
	redacted := sshCommandLine(target, true)
	if strings.Contains(redacted, target.User) {
		t.Fatalf("redacted command leaked token: %q", redacted)
	}
	if !strings.Contains(redacted, "<token>@ssh.app.daytona.io") {
		t.Fatalf("redacted command missing placeholder user: %q", redacted)
	}
	full := sshCommandLine(target, false)
	if !strings.Contains(full, target.User+"@ssh.app.daytona.io") {
		t.Fatalf("full command missing token user: %q", full)
	}
}

func TestSSHRegistersProviderRoutingFlags(t *testing.T) {
	defaults := baseConfig()
	fs := newFlagSet("ssh", io.Discard)
	provider := fs.String("provider", defaults.Provider, "")
	id := fs.String("id", "", "")
	providerFlags := registerProviderFlags(fs, defaults)
	targetFlags := registerTargetFlags(fs, defaults)
	networkFlags := registerNetworkModeFlag(fs, defaults)
	if err := parseFlags(fs, []string{
		"--provider", "proxmox",
		"--proxmox-api-url", "https://pve.example.test:8006",
		"--proxmox-node", "pve1",
		"--proxmox-template-id", "9000",
		"--proxmox-user", "runner",
		"--proxmox-work-root", "/work/test",
		"--id", "cbx_123",
	}); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadLeaseTargetConfig(fs, *provider, targetFlags, networkFlags, leaseTargetConfigOptions{LeaseID: *id})
	if err != nil {
		t.Fatal(err)
	}
	if err := applyProviderFlags(&cfg, fs, providerFlags); err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "proxmox" || cfg.Proxmox.APIURL != "https://pve.example.test:8006" || cfg.Proxmox.Node != "pve1" || cfg.Proxmox.TemplateID != 9000 || cfg.SSHUser != "runner" || cfg.WorkRoot != "/work/test" {
		t.Fatalf("provider routing flags not applied for ssh: provider=%q proxmox=%#v", cfg.Provider, cfg.Proxmox)
	}
}

func TestSSHTransportProbeDoesNotRequireCrabboxReady(t *testing.T) {
	got := sshTransportProbeCommand(SSHTarget{Host: "100.64.0.10", Port: "2222"})
	if strings.Contains(got, "crabbox-ready") || strings.Contains(got, "git --version") || strings.Contains(got, "/work/crabbox") {
		t.Fatalf("transport probe should not run readiness checks: %q", got)
	}
}

func TestSSHReadyCommandUsesAbsoluteCrabboxReadyPath(t *testing.T) {
	got := sshReadyCommand(SSHTarget{})
	if !strings.Contains(got, "/usr/local/bin/crabbox-ready >/tmp/crabbox-ready.log") {
		t.Fatalf("sshReadyCommand() should use absolute crabbox-ready path: %q", got)
	}
}

func TestSSHArgsQuoteKnownHostsPathWithSpaces(t *testing.T) {
	got := strings.Join(sshArgs(SSHTarget{
		User: "crabbox",
		Host: "203.0.113.10",
		Key:  "/tmp/Application Support/crabbox/id_ed25519",
		Port: "2222",
	}, "true"), "\n")
	if !strings.Contains(got, `UserKnownHostsFile="/tmp/Application Support/crabbox/known_hosts"`) {
		t.Fatalf("sshArgs() should quote known_hosts path with spaces: %q", got)
	}
}

func TestSSHControlPathIsScopedByKey(t *testing.T) {
	left := sshControlPath(SSHTarget{User: "crabbox", Key: "/tmp/lease-a/id_ed25519"})
	right := sshControlPath(SSHTarget{User: "crabbox", Key: "/tmp/lease-b/id_ed25519"})
	if left == right {
		t.Fatalf("control paths should differ for different lease keys: %q", left)
	}
	if !strings.HasPrefix(filepath.Base(left), "crabbox-ssh-") || !strings.HasSuffix(left, "-%C") {
		t.Fatalf("unexpected control path %q", left)
	}
}

func TestSSHControlPathIsScopedByProxyAndCertificate(t *testing.T) {
	base := SSHTarget{
		User:            "tenki",
		Host:            "sandbox",
		Key:             "/tmp/tenki/id_ed25519",
		CertificateFile: "/tmp/tenki/ssh-certs/session-a/cert.pub",
		ProxyCommand:    "tenki sandbox ssh-proxy --session session-a",
	}
	otherCert := base
	otherCert.CertificateFile = "/tmp/tenki/ssh-certs/session-b/cert.pub"
	otherProxy := base
	otherProxy.ProxyCommand = "tenki sandbox ssh-proxy --session session-b"
	if sshControlPath(base) == sshControlPath(otherCert) {
		t.Fatal("control paths should differ for different certificate files")
	}
	if sshControlPath(base) == sshControlPath(otherProxy) {
		t.Fatal("control paths should differ for different proxy commands")
	}
}

func TestSSHControlPathIsScopedByAuthoritativeHostKey(t *testing.T) {
	base := SSHTarget{
		User:           "crabbox",
		Key:            "/tmp/lease/id_ed25519",
		KnownHostsFile: "/tmp/lease/known_hosts",
		HostKeyAlias:   "crabbox-lease-stable",
	}
	first := base
	first.SSHHostKey = "ssh-ed25519 AAAA-first"
	second := base
	second.SSHHostKey = "ssh-ed25519 AAAA-second"
	if sshControlPath(base) == sshControlPath(first) {
		t.Fatal("adding an authoritative host key reused the TOFU control path")
	}
	if sshControlPath(first) == sshControlPath(second) {
		t.Fatal("rotating an authoritative host key reused the prior control path")
	}
}

func TestSSHWaitProgressIncludesElapsedAndRemaining(t *testing.T) {
	got := sshWaitProgressMessage(
		&SSHTarget{Host: "203.0.113.10", Port: "2222"},
		"bootstrap",
		"2222",
		"2222",
		"2222:auth",
		95*time.Second,
		10*time.Minute,
	)
	for _, want := range []string{
		"waiting for 203.0.113.10:2222 bootstrap ready-check...",
		"elapsed=1m35s",
		"remaining=10m0s",
		"ports=2222:auth",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("progress message missing %q in %q", want, got)
		}
	}
}

func TestSSHWaitProgressDistinguishesAuthFromReadiness(t *testing.T) {
	target := &SSHTarget{Host: "203.0.113.10", Port: "2222"}
	got := sshWaitProgressMessage(target, "bootstrap", "2222", "", "2222:tcp", 5*time.Second, time.Minute)
	if !strings.Contains(got, "bootstrap ssh-auth") {
		t.Fatalf("TCP-only progress should report ssh-auth stage: %q", got)
	}
	got = sshWaitProgressMessage(target, "bootstrap", "2222", "2222", "2222:auth", 5*time.Second, time.Minute)
	if !strings.Contains(got, "bootstrap ready-check") {
		t.Fatalf("SSH transport progress should report ready-check stage: %q", got)
	}
}

func TestSSHPortCandidatesPreferConfiguredPortWithFallback(t *testing.T) {
	tests := map[string][]string{
		"":     {"22"},
		"22":   {"22"},
		"2222": {"2222", "22"},
	}
	for in, want := range tests {
		got := sshPortCandidates(in, nil)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("sshPortCandidates(%q)=%v want %v", in, got, want)
		}
	}
}

func TestSSHPortCandidatesUseConfiguredFallbacks(t *testing.T) {
	got := sshPortCandidates("2222", []string{"2022", "22", "2222", ""})
	want := []string{"2222", "2022", "22"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("sshPortCandidates()=%v want %v", got, want)
	}
	if got := sshPortCandidates("2222", []string{}); strings.Join(got, ",") != "2222" {
		t.Fatalf("sshPortCandidates(disabled fallback)=%v want [2222]", got)
	}
}

func TestRsyncLocalPathConvertsWindowsDrivePath(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"C:/OpenClaw/crabbox": "/c/OpenClaw/crabbox",
		"D:\\Users\\test":     "/d/Users/test",
		"/already/posix":      "/already/posix",
		"relative/path":       "relative/path",
	}
	for in, want := range tests {
		got := rsyncLocalPathForGOOS("windows", in)
		if got != want {
			t.Errorf("rsyncLocalPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRsyncLocalPathPassesThroughNonWindowsPath(t *testing.T) {
	t.Parallel()
	if got := rsyncLocalPathForGOOS("linux", "C:/OpenClaw/crabbox"); got != "C:/OpenClaw/crabbox" {
		t.Fatalf("non-Windows rsyncLocalPath = %q", got)
	}
}

func TestWindowsToWSLMountPathSupportsHostMountRoot(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		`C:\Users\alice\.ssh\id_ed25519`: "/mnt/host/c/Users/alice/.ssh/id_ed25519",
		"C:/oc-work/my-app":              "/mnt/host/c/oc-work/my-app",
		"/c/msys64/usr/bin/rsync.exe":    "/mnt/host/c/msys64/usr/bin/rsync.exe",
	}
	for in, want := range tests {
		got := windowsToWSLMountPathWithRoot(in, "/mnt/host")
		if got != want {
			t.Fatalf("windowsToWSLMountPathWithRoot(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWindowsHostPathConvertsMSYSDrivePath(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		`C:\Users\alice\.ssh\id_ed25519`: "C:/Users/alice/.ssh/id_ed25519",
		"/c/Users/alice/.ssh/id_ed25519": "c:/Users/alice/.ssh/id_ed25519",
		"/work/repo":                     "/work/repo",
	}
	for in, want := range tests {
		if got := windowsHostPath(in); got != want {
			t.Fatalf("windowsHostPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWindowsWSLNativeToolProbeUsesDirectCommandOutput(t *testing.T) {
	t.Parallel()
	cmd := windowsWSLNativeToolProbeCommand(context.Background(), "wsl.exe")
	want := []string{
		"wsl.exe",
		"sh",
		"-c",
		"command -v rsync || exit 1; command -v ssh || exit 1",
	}
	if !reflect.DeepEqual(cmd.Args, want) {
		t.Fatalf("WSL native-tool probe args = %q, want %q", cmd.Args, want)
	}
}

func TestWindowsWSLNativeToolPathsRejectsWindowsShims(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		paths string
		want  bool
	}{
		{
			name:  "native paths",
			paths: "/usr/bin/rsync\n/usr/bin/ssh\n",
			want:  true,
		},
		{
			name:  "missing ssh",
			paths: "/usr/bin/rsync\n",
			want:  false,
		},
		{
			name:  "exe shim",
			paths: "/usr/bin/rsync.exe\n/usr/bin/ssh\n",
			want:  false,
		},
		{
			name:  "mnt c shim",
			paths: "/mnt/c/msys64/usr/bin/rsync\n/usr/bin/ssh\n",
			want:  false,
		},
		{
			name:  "mnt other drive shim",
			paths: "/mnt/d/tools/rsync\n/usr/bin/ssh\n",
			want:  false,
		},
		{
			name:  "mnt host c shim",
			paths: "/mnt/host/c/msys64/usr/bin/rsync\n/usr/bin/ssh\n",
			want:  false,
		},
		{
			name:  "mnt host other drive shim",
			paths: "/mnt/host/e/tools/rsync\n/usr/bin/ssh\n",
			want:  false,
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := windowsWSLNativeToolPaths(tc.paths); got != tc.want {
				t.Fatalf("windowsWSLNativeToolPaths(%q) = %v, want %v", tc.paths, got, tc.want)
			}
		})
	}
}

func TestNormalizeRsyncOptionsNoTimesForcesChecksum(t *testing.T) {
	t.Parallel()
	got := normalizeRsyncOptions(rsyncOptions{NoTimes: true})
	if !got.Checksum {
		t.Fatal("NoTimes rsync must force checksum comparison")
	}
	got = normalizeRsyncOptions(rsyncOptions{})
	if got.Checksum {
		t.Fatal("normal rsync should keep checksum disabled by default")
	}
}

func TestRsyncFilesFromUsesAuthoritativeManifestWithoutExcludes(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "rsync.log")
	rsyncPath := filepath.Join(dir, "rsync")
	if err := os.WriteFile(rsyncPath, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$CRABBOX_FAKE_RSYNC_LOG\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CRABBOX_FAKE_RSYNC_LOG", logPath)
	target := SSHTarget{Host: "example.test", User: "runner", Port: "22"}
	err := rsync(
		context.Background(),
		target,
		dir,
		"/work",
		[]string{"target", "!target/keep.txt"},
		io.Discard,
		io.Discard,
		rsyncOptions{UseFilesFrom: true, FilesFrom: []byte("target/keep.txt\x00")},
	)
	if err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), "--files-from=-\n") {
		t.Fatalf("rsync args missing authoritative manifest:\n%s", args)
	}
	if strings.Contains(string(args), "--exclude\n") {
		t.Fatalf("rsync args must not reapply excludes to manifest paths:\n%s", args)
	}
}

func TestWindowsToWSLPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in, want string
	}{
		{"C:/Users/test", "/mnt/c/Users/test"},
		{`D:\Users\test`, "/mnt/d/Users/test"},
		{"/c/OpenClaw/crabbox", "/mnt/c/OpenClaw/crabbox"},
		{"'ssh' '-i' 'C:/Users/galini/key' '-o' 'UserKnownHostsFile=C:/Users/galini/known_hosts'",
			"'ssh' '-i' '/mnt/c/Users/galini/key' '-o' 'UserKnownHostsFile=/mnt/c/Users/galini/known_hosts'"},
		{"/work/crabbox", "/work/crabbox"},
		{"crabbox@10.0.0.1:/work/", "crabbox@10.0.0.1:/work/"},
	}
	for _, tc := range tests {
		got := windowsToWSLPath(tc.in)
		if got != tc.want {
			t.Errorf("windowsToWSLPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestWindowsToWSLPathSupportsHostMountRoot(t *testing.T) {
	t.Parallel()
	in := `ssh -i C:\Users\alice\AppData\Local\Temp\cbx\id_ed25519 -o UserKnownHostsFile=/c/tmp/known_hosts`
	got := windowsToWSLPathWithRoot(in, "/mnt/host")
	if !strings.Contains(got, "/mnt/host/c/Users/alice/AppData/Local/Temp/cbx/id_ed25519") {
		t.Fatalf("converted path missing host-root key path: %q", got)
	}
	if !strings.Contains(got, "UserKnownHostsFile=/mnt/host/c/tmp/known_hosts") {
		t.Fatalf("converted path missing host-root known_hosts path: %q", got)
	}
}

func TestRemotePruneSyncManifestDeletesOnlyManagedPaths(t *testing.T) {
	got := remotePruneSyncManifest("/work/repo", "0123456789abcdef0123456789abcdef")
	for _, want := range []string{
		"sync-deleted.0123456789abcdef0123456789abcdef.new",
		"manifest_removed_paths",
		"command -v python3",
		"command -v perl",
		"rm -f --",
		"rmdir --",
		"sync-manifest.0123456789abcdef0123456789abcdef.new",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("remotePruneSyncManifest missing %q in %q", want, got)
		}
	}
}

func TestRemotePruneSyncManifestUsesDeletedListBeforeOldManifestDiff(t *testing.T) {
	got := remotePruneSyncManifest("/work/repo", "0123456789abcdef0123456789abcdef")
	deletedIndex := strings.Index(got, `delete_paths < "$deleted"`)
	oldIndex := strings.Index(got, "manifest_removed_paths | delete_paths")
	if deletedIndex < 0 || oldIndex < 0 || deletedIndex > oldIndex {
		t.Fatalf("deleted list should be applied before old manifest diff: %q", got)
	}
}

func TestRemotePruneSyncManifestForWSL2UsesShortCoreutils(t *testing.T) {
	got := remotePruneSyncManifestForTarget(SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeWSL2}, "/work/repo", "0123456789abcdef0123456789abcdef")
	for _, want := range []string{"sort -z", "comm -z -23", "delete_paths"} {
		if !strings.Contains(got, want) {
			t.Fatalf("WSL2 prune command missing %q in %q", want, got)
		}
	}
	for _, notWant := range []string{"command -v python3", "command -v perl"} {
		if strings.Contains(got, notWant) {
			t.Fatalf("WSL2 prune command should stay short, found %q in %q", notWant, got)
		}
	}
}

func TestRemoteSeedSyncManifestFromGitWritesInitialTrackedManifest(t *testing.T) {
	workdir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = workdir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v failed: %v\n%s", args, err, out)
		}
	}
	run("git", "init", "-q")
	mustWriteTestFile(t, filepath.Join(workdir, "keep.txt"), "keep")
	mustWriteTestFile(t, filepath.Join(workdir, "stale.txt"), "stale")
	run("git", "add", "keep.txt", "stale.txt")

	cmd := exec.Command("bash", "-lc", remoteSeedSyncManifestFromGit(workdir))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("remote seed failed: %v\n%s", err, out)
	}

	got, err := os.ReadFile(filepath.Join(workdir, ".git", "crabbox", "sync-manifest"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "keep.txt\x00stale.txt\x00" {
		t.Fatalf("unexpected seeded manifest: %q", got)
	}
}

func TestRemotePruneSyncManifestPrunesManagedFiles(t *testing.T) {
	testRemotePruneSyncManifestPrunesManagedFiles(t, remotePruneSyncManifest)
}

func TestRemotePruneSyncManifestCoreutilsPrunesManagedFiles(t *testing.T) {
	if out, err := exec.Command("comm", "-z", os.DevNull, os.DevNull).CombinedOutput(); err != nil {
		t.Skipf("comm -z unavailable on this host: %v\n%s", err, out)
	}
	testRemotePruneSyncManifestPrunesManagedFiles(t, remotePruneSyncManifestCoreutils)
}

func testRemotePruneSyncManifestPrunesManagedFiles(t *testing.T, command func(string, string) string) {
	t.Helper()
	workdir := t.TempDir()
	mustWriteTestFile(t, filepath.Join(workdir, ".crabbox", "sync-manifest"), "keep.txt\x00kept-dir/keep.txt\x00stale.txt\x00old-empty/remove.txt\x00non-empty/remove.txt\x00")
	mustWriteTestFile(t, filepath.Join(workdir, ".crabbox", remoteSyncPendingManifestName("0123456789abcdef0123456789abcdef")), "keep.txt\x00kept-dir/keep.txt\x00")
	mustWriteTestFile(t, filepath.Join(workdir, ".crabbox", remoteSyncPendingDeletedName("0123456789abcdef0123456789abcdef")), "explicit-delete.txt\x00../outside.txt\x00/absolute.txt\x00")
	for _, rel := range []string{
		"keep.txt",
		"kept-dir/keep.txt",
		"stale.txt",
		"old-empty/remove.txt",
		"non-empty/remove.txt",
		"non-empty/unmanaged.txt",
		"explicit-delete.txt",
		"unmanaged.txt",
	} {
		mustWriteTestFile(t, filepath.Join(workdir, filepath.FromSlash(rel)), rel)
	}
	outside := filepath.Join(filepath.Dir(workdir), "outside.txt")
	mustWriteTestFile(t, outside, "outside")

	cmd := exec.Command("bash", "-lc", command(workdir, "0123456789abcdef0123456789abcdef"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("remote prune failed: %v\n%s", err, out)
	}

	for _, rel := range []string{"keep.txt", "kept-dir/keep.txt", "non-empty/unmanaged.txt", "unmanaged.txt"} {
		if _, err := os.Stat(filepath.Join(workdir, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("%s should survive prune: %v", rel, err)
		}
	}
	for _, rel := range []string{"stale.txt", "old-empty/remove.txt", "non-empty/remove.txt", "explicit-delete.txt"} {
		if _, err := os.Stat(filepath.Join(workdir, filepath.FromSlash(rel))); !os.IsNotExist(err) {
			t.Fatalf("%s should be pruned, stat err=%v", rel, err)
		}
	}
	if _, err := os.Stat(filepath.Join(workdir, "old-empty")); !os.IsNotExist(err) {
		t.Fatalf("empty parent dir should be pruned, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(workdir, "non-empty")); err != nil {
		t.Fatalf("non-empty parent dir should survive: %v", err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("unsafe deleted path should not escape workdir: %v", err)
	}
	sorted, err := filepath.Glob(filepath.Join(workdir, ".crabbox", "sync-manifest.*.sorted"))
	if err != nil {
		t.Fatal(err)
	}
	if len(sorted) != 0 {
		t.Fatalf("sorted manifest scratch files remain: %v", sorted)
	}
}

func TestRemotePruneSyncManifestFallsBackToPerlWithoutPython(t *testing.T) {
	workdir := t.TempDir()
	mustWriteTestFile(t, filepath.Join(workdir, ".crabbox", "sync-manifest"), "keep.txt\x00stale.txt\x00")
	mustWriteTestFile(t, filepath.Join(workdir, ".crabbox", remoteSyncPendingManifestName("0123456789abcdef0123456789abcdef")), "keep.txt\x00")
	mustWriteTestFile(t, filepath.Join(workdir, ".crabbox", remoteSyncPendingDeletedName("0123456789abcdef0123456789abcdef")), "")
	mustWriteTestFile(t, filepath.Join(workdir, "keep.txt"), "keep")
	mustWriteTestFile(t, filepath.Join(workdir, "stale.txt"), "stale")

	toolDir := t.TempDir()
	for _, name := range []string{"dirname", "rm", "rmdir"} {
		mustWriteTestCommandWrapper(t, toolDir, name)
	}
	mustWriteTestBashNoProfileWrapper(t, toolDir)
	perlMarker := filepath.Join(t.TempDir(), "perl-invoked")
	mustWriteTestCommandWrapperWithMarker(t, toolDir, "perl", perlMarker)
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bashPath, "--noprofile", "--norc", "-c", remotePruneSyncManifest(workdir, "0123456789abcdef0123456789abcdef"))
	cmd.Env = append(os.Environ(), "PATH="+toolDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("remote prune perl fallback failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(perlMarker); err != nil {
		t.Fatalf("perl fallback was not invoked: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workdir, "keep.txt")); err != nil {
		t.Fatalf("keep.txt should survive prune: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workdir, "stale.txt")); !os.IsNotExist(err) {
		t.Fatalf("stale.txt should be pruned, stat err=%v", err)
	}
}

func TestRemotePruneSyncManifestFailsClosedWhenInterpreterFails(t *testing.T) {
	workdir := t.TempDir()
	mustWriteTestFile(t, filepath.Join(workdir, ".crabbox", "sync-manifest"), "stale.txt\x00")
	mustWriteTestFile(t, filepath.Join(workdir, ".crabbox", remoteSyncPendingManifestName("0123456789abcdef0123456789abcdef")), "")
	mustWriteTestFile(t, filepath.Join(workdir, ".crabbox", remoteSyncPendingDeletedName("0123456789abcdef0123456789abcdef")), "")
	mustWriteTestFile(t, filepath.Join(workdir, "stale.txt"), "stale")

	toolDir := t.TempDir()
	for _, name := range []string{"dirname", "rm", "rmdir"} {
		mustWriteTestCommandWrapper(t, toolDir, name)
	}
	mustWriteTestBashNoProfileWrapper(t, toolDir)
	mustWriteTestFailingCommand(t, toolDir, "python3", 23)
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bashPath, "--noprofile", "--norc", "-c", remotePruneSyncManifest(workdir, "0123456789abcdef0123456789abcdef"))
	cmd.Env = append(os.Environ(), "PATH="+toolDir)
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("remote prune unexpectedly succeeded\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(workdir, "stale.txt")); err != nil {
		t.Fatalf("stale.txt should survive interpreter failure: %v", err)
	}
}

func TestRemotePruneSyncManifestDoesNotSwallowReadErrors(t *testing.T) {
	got := remotePruneSyncManifest("/work/repo", "0123456789abcdef0123456789abcdef")
	for _, unsafe := range []string{"except IOError", "return () unless -"} {
		if strings.Contains(got, unsafe) {
			t.Fatalf("remote prune still treats manifest read errors as missing: %q", unsafe)
		}
	}
	if !strings.Contains(got, "set -e -o pipefail") {
		t.Fatalf("remote prune must propagate interpreter failures: %q", got)
	}
}

func TestRemoteApplySyncManifestOnlyCommitsManifest(t *testing.T) {
	got := remoteApplySyncManifest("/work/repo")
	if strings.Contains(got, "manifest_removed_paths") || strings.Contains(got, "delete_paths") {
		t.Fatalf("remoteApplySyncManifest should not delete after rsync: %q", got)
	}
	if !strings.Contains(got, "mv \"$new\" \"$meta_dir/sync-manifest\"") {
		t.Fatalf("remoteApplySyncManifest should commit new manifest: %q", got)
	}
}

func TestRemoteFinalizeSyncCommitsMetadataInOneCommand(t *testing.T) {
	const finalizeToken = "0123456789abcdef0123456789abcdef"
	workdir := t.TempDir()
	metaDir := filepath.Join(workdir, ".crabbox")
	if err := os.MkdirAll(metaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(metaDir, remoteSyncPendingManifestName(finalizeToken)), []byte("tracked.txt\x00"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(metaDir, remoteSyncPendingDeletedName(finalizeToken)), []byte("deleted.txt\x00"), 0o644); err != nil {
		t.Fatal(err)
	}

	remote := remoteFinalizeSync(workdir, remoteSyncFinalizeOptions{
		BaseRef: "main",
		BaseSHA: "abc123",
		Token:   finalizeToken,
	})
	for attempt := 1; attempt <= 2; attempt++ {
		if out, err := exec.Command("bash", "-lc", remote).CombinedOutput(); err != nil {
			t.Fatalf("remote finalize attempt %d failed: %v\n%s", attempt, err, out)
		}
	}
	if _, err := os.Stat(filepath.Join(metaDir, remoteSyncPendingDeletedName(finalizeToken))); !os.IsNotExist(err) {
		t.Fatalf("deleted manifest should be removed, stat err=%v", err)
	}
	manifest, err := os.ReadFile(filepath.Join(metaDir, "sync-manifest"))
	if err != nil {
		t.Fatal(err)
	}
	if string(manifest) != "tracked.txt\x00" {
		t.Fatalf("unexpected manifest: %q", manifest)
	}
	marker, err := os.ReadFile(filepath.Join(metaDir, "git-hydrate-base"))
	if err != nil {
		t.Fatal(err)
	}
	if string(marker) != "main abc123\n" {
		t.Fatalf("unexpected hydrate marker: %q", marker)
	}
	if _, err := os.Stat(filepath.Join(metaDir, "sync-fingerprint")); !os.IsNotExist(err) {
		t.Fatalf("overlay-only finalize should not publish fingerprint: %v", err)
	}
	token, err := os.ReadFile(filepath.Join(metaDir, "sync-finalize-token"))
	if err != nil {
		t.Fatal(err)
	}
	if string(token) != finalizeToken {
		t.Fatalf("unexpected finalize token: %q", token)
	}
	completeToken, err := os.ReadFile(filepath.Join(metaDir, "sync-finalize-complete-token"))
	if err != nil {
		t.Fatal(err)
	}
	if string(completeToken) != finalizeToken {
		t.Fatalf("unexpected complete token: %q", completeToken)
	}
}

func TestRemoteFinalizeSyncPlainManifestSuppressesOriginState(t *testing.T) {
	const token = "0123456789abcdef0123456789abcdef"
	workdir := t.TempDir()
	runGit(t, workdir, "init")
	runGit(t, workdir, "config", "user.email", "alice@example.com")
	runGit(t, workdir, "config", "user.name", "Alice")
	mustWriteTestFile(t, filepath.Join(workdir, "tracked.txt"), "tracked\n")
	runGit(t, workdir, "add", ".")
	runGit(t, workdir, "commit", "-qm", "base")
	metaDir := coherenceMetaDir(t, workdir)
	mustWriteTestFile(t, filepath.Join(metaDir, remoteSyncPendingManifestName(token)), "tracked.txt\x00")
	mustWriteTestFile(t, filepath.Join(metaDir, "sync-fingerprint"), "stale")
	mustWriteTestFile(t, filepath.Join(metaDir, "git-hydrate-base"), "main stale\n")

	command := remoteFinalizeSync(workdir, remoteSyncFinalizeOptions{
		PlainManifest: true,
		HydrateGit:    true,
		BaseRef:       "main",
		BaseSHA:       "abc123",
		Fingerprint:   "must-not-publish",
		Token:         token,
	})
	for _, forbidden := range []string{"git fetch ", "repair_origin", "base_tmp=", "refs/remotes/origin/"} {
		if strings.Contains(command, forbidden) {
			t.Fatalf("plain manifest finalize contains %q:\n%s", forbidden, command)
		}
	}
	for _, want := range []string{"plain_git status --porcelain=v1", "protocol.allow=never", "BASH_ENV=/dev/null", "ENV=/dev/null"} {
		if !strings.Contains(command, want) {
			t.Fatalf("plain manifest finalize missing %q:\n%s", want, command)
		}
	}
	if out, err := exec.Command("/bin/bash", "--noprofile", "--norc", "-c", command).CombinedOutput(); err != nil {
		t.Fatalf("plain manifest finalize: %v\n%s", err, out)
	}
	for _, name := range []string{"sync-fingerprint", "git-hydrate-base"} {
		if _, err := os.Stat(filepath.Join(metaDir, name)); !os.IsNotExist(err) {
			t.Fatalf("%s survived plain manifest finalize: %v", name, err)
		}
	}
}

func TestRemoteFinalizeSyncHydratesForRepositoryDepth(t *testing.T) {
	fixture := newGitCoherenceFixture(t)
	// Exceed the fallback depth so accidentally deepening a complete clone is observable.
	const extraCommits = 1001
	var history strings.Builder
	parent := gitOutput(fixture.origin, "rev-parse", "refs/heads/main")
	for i := 1; i <= extraCommits; i++ {
		message := fmt.Sprintf("hydrate history %d", i)
		fmt.Fprintf(&history, "commit refs/heads/main\nmark :%d\ncommitter Test <test@example.com> %d +0000\ndata %d\n%s\nfrom %s\n\n", i, 1700000000+i, len(message), message, parent)
		parent = fmt.Sprintf(":%d", i)
	}
	fastImport := exec.Command("git", "-C", fixture.origin, "fast-import", "--quiet")
	fastImport.Stdin = strings.NewReader(history.String())
	if out, err := fastImport.CombinedOutput(); err != nil {
		t.Fatalf("extend hydration history: %v\n%s", err, out)
	}

	tests := []struct {
		name    string
		shallow bool
	}{
		{name: "complete", shallow: false},
		{name: "shallow", shallow: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workdir := filepath.Join(t.TempDir(), "work dir's repo")
			cloneArgs := []string{"clone", "--quiet"}
			origin := fixture.origin
			if tt.shallow {
				cloneArgs = append(cloneArgs, "--depth=1")
				origin = "file://" + filepath.ToSlash(origin)
			}
			cloneArgs = append(cloneArgs, origin, workdir)
			if out, err := exec.Command("git", cloneArgs...).CombinedOutput(); err != nil {
				t.Fatalf("clone workspace: %v\n%s", err, out)
			}
			if got := gitOutput(workdir, "rev-parse", "--is-shallow-repository"); got != strconv.FormatBool(tt.shallow) {
				t.Fatalf("is-shallow-repository=%q, want %t", got, tt.shallow)
			}
			for attempt := 1; attempt <= 2; attempt++ {
				token := fmt.Sprintf("%032x", attempt)
				stageCoherenceFinalize(t, workdir, token)
				cmd := exec.Command("bash", "-lc", remoteFinalizeSync(workdir, remoteSyncFinalizeOptions{
					HydrateGit: true,
					BaseRef:    "main",
					Token:      token,
				}))
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("remote finalize attempt %d: %v\n%s", attempt, err, out)
				}
				if got := gitOutput(workdir, "rev-parse", "--is-shallow-repository"); got != "false" {
					t.Fatalf("attempt %d left %s repository shallow: %q", attempt, tt.name, got)
				}
			}
		})
	}
}

func TestRemoteFinalizeSyncRetriesAfterAmbiguousTransportFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell ssh fixture")
	}
	const finalizeToken = "0123456789abcdef0123456789abcdef"
	workdir := t.TempDir()
	metaDir := filepath.Join(workdir, ".crabbox")
	if err := os.MkdirAll(metaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(metaDir, remoteSyncPendingManifestName(finalizeToken)), []byte("tracked.txt\x00"), 0o644); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	sshPath := filepath.Join(dir, "ssh")
	callsPath := filepath.Join(dir, "calls")
	script := `#!/bin/sh
remote=""
for arg do remote="$arg"; done
sh -c "$remote"
status=$?
if [ "$status" -ne 0 ]; then exit "$status"; fi
printf 'call\n' >> "$CRABBOX_FAKE_SSH_CALLS"
if [ "$(wc -l < "$CRABBOX_FAKE_SSH_CALLS")" -eq 1 ]; then exit 255; fi
`
	if err := os.WriteFile(sshPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CRABBOX_FAKE_SSH_CALLS", callsPath)

	out, err := runIdempotentSSHCombinedOutput(context.Background(), SSHTarget{
		User: "crabbox",
		Host: "gateway.example",
		Port: "22",
	}, remoteFinalizeSync(workdir, remoteSyncFinalizeOptions{Fingerprint: "fp123", Token: finalizeToken}), 0)
	if err != nil {
		t.Fatalf("out=%q err=%v", out, err)
	}
	calls, readErr := os.ReadFile(callsPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if got := strings.Count(string(calls), "call\n"); got != 2 {
		t.Fatalf("ssh calls=%d want 2", got)
	}
	manifest, readErr := os.ReadFile(filepath.Join(metaDir, "sync-manifest"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(manifest) != "tracked.txt\x00" {
		t.Fatalf("unexpected manifest: %q", manifest)
	}
}

func TestRemoteFinalizeSyncCompletedRetryPreservesNewerPendingState(t *testing.T) {
	const completedToken = "0123456789abcdef0123456789abcdef"
	const newerToken = "fedcba9876543210fedcba9876543210"
	workdir := t.TempDir()
	metaDir := filepath.Join(workdir, ".crabbox")
	if err := os.MkdirAll(metaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(metaDir, remoteSyncPendingManifestName(completedToken)), []byte("completed.txt\x00"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(metaDir, remoteSyncPendingDeletedName(completedToken)), []byte("completed-old.txt\x00"), 0o644); err != nil {
		t.Fatal(err)
	}
	completedRemote := remoteFinalizeSync(workdir, remoteSyncFinalizeOptions{Token: completedToken})
	if out, err := exec.Command("bash", "-lc", completedRemote).CombinedOutput(); err != nil {
		t.Fatalf("initial finalize: %v\n%s", err, out)
	}

	newerFiles := map[string]string{
		remoteSyncPendingManifestName(newerToken): "newer.txt\x00",
		remoteSyncPendingDeletedName(newerToken):  "newer-old.txt\x00",
	}
	for name, contents := range newerFiles {
		if err := os.WriteFile(filepath.Join(metaDir, name), []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if out, err := exec.Command("bash", "-lc", completedRemote).CombinedOutput(); err != nil {
		t.Fatalf("completed retry: %v\n%s", err, out)
	}
	for name, want := range newerFiles {
		got, err := os.ReadFile(filepath.Join(metaDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Fatalf("%s=%q want %q", name, got, want)
		}
	}
}

func TestRemoteFinalizeSyncRejectsMissingManifests(t *testing.T) {
	workdir := t.TempDir()
	metaDir := filepath.Join(workdir, ".crabbox")
	if err := os.MkdirAll(metaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(metaDir, "sync-manifest"), []byte("stale.txt\x00"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(metaDir, "sync-finalize-token"), []byte("previous-sync"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-lc", remoteFinalizeSync(workdir, remoteSyncFinalizeOptions{Token: "fedcba9876543210fedcba9876543210"}))
	out, err := cmd.CombinedOutput()
	if err == nil || exitCode(err) != 67 {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if !strings.Contains(string(out), "no committed manifest for this sync") {
		t.Fatalf("unexpected output: %q", out)
	}
}

func TestRemoteFinalizeSyncWaitsForLiveOwner(t *testing.T) {
	const finalizeToken = "0123456789abcdef0123456789abcdef"
	workdir := t.TempDir()
	metaDir := filepath.Join(workdir, ".crabbox")
	if err := os.MkdirAll(metaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(metaDir, "sync-finalize-lock")
	if err := os.Symlink(strconv.Itoa(os.Getpid()), lockPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(metaDir, remoteSyncPendingManifestName(finalizeToken)), []byte("tracked.txt\x00"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("bash", "-lc", remoteFinalizeSync(workdir, remoteSyncFinalizeOptions{Token: finalizeToken}))
	done := make(chan error, 1)
	go func() {
		out, err := cmd.CombinedOutput()
		if err != nil {
			err = fmt.Errorf("%w: %s", err, out)
		}
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("finalize did not wait for live owner: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("finalize did not continue after live owner released lock")
	}
}

func TestRemoteFinalizeSyncRecoversDeadOwner(t *testing.T) {
	const finalizeToken = "0123456789abcdef0123456789abcdef"
	workdir := t.TempDir()
	metaDir := filepath.Join(workdir, ".crabbox")
	if err := os.MkdirAll(metaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	crashScript := "set -e\nmeta_dir=" + shellQuote(metaDir) + "\n" + remoteSyncFinalizeLockScript() + "\nkill -9 $$"
	crasher := exec.Command("bash", "-c", crashScript)
	if err := crasher.Run(); err == nil {
		t.Fatal("expected lock owner to terminate")
	}
	if err := os.WriteFile(filepath.Join(metaDir, remoteSyncPendingManifestName(finalizeToken)), []byte("tracked.txt\x00"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := exec.Command("bash", "-lc", remoteFinalizeSync(workdir, remoteSyncFinalizeOptions{Token: finalizeToken})).CombinedOutput()
	if err != nil {
		t.Fatalf("recover dead owner: %v\n%s", err, out)
	}
	if _, err := os.Lstat(filepath.Join(metaDir, "sync-finalize-lock")); !os.IsNotExist(err) {
		t.Fatalf("finalize lock should be removed, stat err=%v", err)
	}
}

func TestRemoteFinalizeSyncWaitIsContextBounded(t *testing.T) {
	const finalizeToken = "0123456789abcdef0123456789abcdef"
	workdir := t.TempDir()
	metaDir := filepath.Join(workdir, ".crabbox")
	if err := os.MkdirAll(metaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(strconv.Itoa(os.Getpid()), filepath.Join(metaDir, "sync-finalize-lock")); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	err := exec.CommandContext(ctx, "bash", "-lc", remoteFinalizeSync(workdir, remoteSyncFinalizeOptions{Token: finalizeToken})).Run()
	if err == nil || ctx.Err() != context.DeadlineExceeded {
		t.Fatalf("err=%v context=%v", err, ctx.Err())
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if err := os.RemoveAll(workdir); err == nil {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("clean canceled finalize fixture: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRemoteSyncFinalizeLockSerializesLiveOwner(t *testing.T) {
	metaDir := t.TempDir()
	acquiredPath := filepath.Join(metaDir, "first-acquired")
	releasePath := filepath.Join(metaDir, "release-first")
	secondPath := filepath.Join(metaDir, "second-entered")
	firstScript := "set -e\nmeta_dir=" + shellQuote(metaDir) + "\n" + remoteSyncFinalizeLockScript() +
		"\nprintf first > " + shellQuote(acquiredPath) +
		"\nwhile [ ! -f " + shellQuote(releasePath) + " ]; do sleep 1; done\n"
	secondScript := "set -e\nmeta_dir=" + shellQuote(metaDir) + "\n" + remoteSyncFinalizeLockScript() +
		"\nprintf second > " + shellQuote(secondPath) + "\n"

	first := exec.Command("bash", "-c", firstScript)
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if first.Process != nil {
			_ = first.Process.Kill()
		}
	})
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(acquiredPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first finalize owner did not acquire lock")
		}
		time.Sleep(10 * time.Millisecond)
	}

	second := exec.Command("bash", "-c", secondScript)
	secondDone := make(chan error, 1)
	if err := second.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if second.Process != nil {
			_ = second.Process.Kill()
		}
	})
	go func() { secondDone <- second.Wait() }()
	select {
	case err := <-secondDone:
		t.Fatalf("second finalize entered while first was live: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	if _, err := os.Stat(secondPath); !os.IsNotExist(err) {
		t.Fatalf("second finalize entered critical section early, stat err=%v", err)
	}
	if err := os.WriteFile(releasePath, []byte("release"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := first.Wait(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("second finalize did not acquire released lock")
	}
}

func TestRemoteFinalizeSyncStatusIsWorkspaceScoped(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell finalization fixture")
	}
	f := newGitCoherenceFixture(t)
	coherentWorkdir := f.workspace(t, f.a, true)
	overlayWorkdir := f.workspace(t, f.a, false)
	plan := f.plan(t, f.b)
	mustWriteTestFile(t, filepath.Join(coherentWorkdir, "tracked.txt"), "B\n")
	const token = "abababababababababababababababab"
	stageCoherenceFinalize(t, coherentWorkdir, token)
	stageCoherenceFinalize(t, overlayWorkdir, token)

	tools := t.TempDir()
	coherentReady := filepath.Join(tools, "coherent-ready")
	overlayWrote := filepath.Join(tools, "overlay-wrote")
	releaseOverlay := filepath.Join(tools, "release-overlay")
	gitScript := `#!/bin/sh
case "$CRABBOX_STATUS_ROLE:$1" in
  coherent:diff-files)
    touch "$CRABBOX_COHERENT_READY"
    while [ ! -f "$CRABBOX_OVERLAY_WROTE" ]; do sleep 0.01; done
    exit 0
    ;;
  overlay:status)
    i=0
    while [ "$i" -lt 200 ]; do
      printf ' D deleted-%03d\n' "$i"
      i=$((i + 1))
    done
    touch "$CRABBOX_OVERLAY_WROTE"
    while [ ! -f "$CRABBOX_RELEASE_OVERLAY" ]; do sleep 0.01; done
    exit 0
    ;;
esac
exec ` + shellQuote(gitOutput("", "--exec-path")+"/git") + ` "$@"
`
	if err := os.WriteFile(filepath.Join(tools, "git"), []byte(gitScript), 0o755); err != nil {
		t.Fatal(err)
	}
	commonEnv := []string{
		"PATH=" + tools + string(os.PathListSeparator) + os.Getenv("PATH"),
		"CRABBOX_COHERENT_READY=" + coherentReady,
		"CRABBOX_OVERLAY_WROTE=" + overlayWrote,
		"CRABBOX_RELEASE_OVERLAY=" + releaseOverlay,
	}
	coherent := exec.Command("/bin/sh", "-c", remoteFinalizeSync(coherentWorkdir, remoteSyncFinalizeOptions{
		Token:       token,
		Fingerprint: "coherent",
		Coherence:   plan,
	}))
	coherent.Env = append(os.Environ(), append(commonEnv, "CRABBOX_STATUS_ROLE=coherent")...)
	var coherentOutput bytes.Buffer
	coherent.Stdout = &coherentOutput
	coherent.Stderr = &coherentOutput
	if err := coherent.Start(); err != nil {
		t.Fatal(err)
	}
	var overlay *exec.Cmd
	t.Cleanup(func() {
		_ = os.WriteFile(releaseOverlay, []byte("release"), 0o644)
		if coherent.Process != nil {
			_ = coherent.Process.Kill()
		}
		if overlay != nil && overlay.Process != nil {
			_ = overlay.Process.Kill()
		}
	})
	coherentDone := make(chan error, 1)
	go func() { coherentDone <- coherent.Wait() }()

	waitForPath := func(path, message string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			if _, err := os.Stat(path); err == nil {
				return
			}
			if time.Now().After(deadline) {
				t.Fatal(message)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitForPath(coherentReady, "coherent finalizer did not reach candidate status inspection")

	overlay = exec.Command("/bin/sh", "-c", remoteFinalizeSync(overlayWorkdir, remoteSyncFinalizeOptions{Token: token}))
	overlay.Env = append(os.Environ(), append(commonEnv, "CRABBOX_STATUS_ROLE=overlay")...)
	var overlayOutput bytes.Buffer
	overlay.Stdout = &overlayOutput
	overlay.Stderr = &overlayOutput
	if err := overlay.Start(); err != nil {
		t.Fatal(err)
	}
	overlayDone := make(chan error, 1)
	go func() { overlayDone <- overlay.Wait() }()
	waitForPath(overlayWrote, "overlay finalizer did not publish its deletion status")

	select {
	case err := <-coherentDone:
		if err != nil {
			t.Fatalf("coherent finalizer consumed another workspace status: %v\n%s", err, coherentOutput.Bytes())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("coherent finalizer did not finish")
	}
	if err := os.WriteFile(releaseOverlay, []byte("release"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-overlayDone:
		if err == nil || exitCode(err) != 66 {
			t.Fatalf("overlay finalizer err=%v output=%q, want deletion failure", err, overlayOutput.Bytes())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("overlay finalizer did not finish")
	}
	for _, workdir := range []string{coherentWorkdir, overlayWorkdir} {
		statusFiles, err := filepath.Glob(filepath.Join(coherenceMetaDir(t, workdir), "sync-git-status.*"))
		if err != nil {
			t.Fatal(err)
		}
		if len(statusFiles) != 0 {
			t.Fatalf("finalizer left status files in %s: %v", workdir, statusFiles)
		}
	}
	for _, script := range []string{
		remoteFinalizeSync(coherentWorkdir, remoteSyncFinalizeOptions{Token: token, Coherence: plan}),
		remoteFinalizeSync(overlayWorkdir, remoteSyncFinalizeOptions{Token: token}),
	} {
		if strings.Contains(script, "/tmp/crabbox-git-status") {
			t.Fatalf("finalizer still uses a global status file: %q", script)
		}
	}
}

func TestRemoteSyncAbandonedMetadataCleanupRemovesStatusFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell metadata cleanup fixture")
	}
	metaDir := t.TempDir()
	statusPath := filepath.Join(metaDir, "sync-git-status.abandoned")
	if err := os.WriteFile(statusPath, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-9 * 24 * time.Hour)
	if err := os.Chtimes(statusPath, old, old); err != nil {
		t.Fatal(err)
	}
	script := "set -e\nmeta_dir=" + shellQuote(metaDir) + "\n" + remoteSyncAbandonedMetadataCleanup()
	if out, err := exec.Command("bash", "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("cleanup abandoned status: %v\n%s", err, out)
	}
	if _, err := os.Stat(statusPath); !os.IsNotExist(err) {
		t.Fatalf("abandoned status file survived cleanup: %v", err)
	}
}

func TestRemoteFinalizeSyncDoesNotConsumeNewerPendingManifest(t *testing.T) {
	const currentToken = "0123456789abcdef0123456789abcdef"
	const newerToken = "fedcba9876543210fedcba9876543210"
	workdir := t.TempDir()
	metaDir := filepath.Join(workdir, ".crabbox")
	if err := os.MkdirAll(metaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(metaDir, remoteSyncPendingManifestName(newerToken)), []byte("newer.txt\x00"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := exec.Command("bash", "-lc", remoteFinalizeSync(workdir, remoteSyncFinalizeOptions{Token: currentToken})).CombinedOutput()
	if err == nil || exitCode(err) != 67 {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if !strings.Contains(string(out), "no committed manifest for this sync") {
		t.Fatalf("unexpected output: %q", out)
	}
	token, readErr := os.ReadFile(filepath.Join(metaDir, remoteSyncPendingManifestName(newerToken)))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(token) != "newer.txt\x00" {
		t.Fatalf("newer pending manifest changed: %q", token)
	}
}

type gitCoherenceFixture struct {
	source string
	origin string
	a      string
	b      string
	c      string
}

func newGitCoherenceFixture(t *testing.T) gitCoherenceFixture {
	t.Helper()
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, source, "init")
	runGit(t, source, "config", "user.email", "test@example.com")
	runGit(t, source, "config", "user.name", "Test")
	runGit(t, source, "branch", "-M", "main")
	mustWriteTestFile(t, filepath.Join(source, "tracked.txt"), "A\n")
	mustWriteTestFile(t, filepath.Join(source, "modified.txt"), "base\n")
	mustWriteTestFile(t, filepath.Join(source, "deleted.txt"), "base\n")
	mustWriteTestFile(t, filepath.Join(source, "src", "keep.txt"), "keep\n")
	mustWriteTestFile(t, filepath.Join(source, "other", "omit.txt"), "omit\n")
	runGit(t, source, "add", ".")
	runGit(t, source, "commit", "-m", "A")
	a := gitOutput(source, "rev-parse", "HEAD")
	mustWriteTestFile(t, filepath.Join(source, "tracked.txt"), "B\n")
	runGit(t, source, "commit", "-am", "B")
	b := gitOutput(source, "rev-parse", "HEAD")
	mustWriteTestFile(t, filepath.Join(source, "tracked.txt"), "C\n")
	runGit(t, source, "commit", "-am", "C")
	c := gitOutput(source, "rev-parse", "HEAD")
	origin := filepath.Join(root, "origin.git")
	if out, err := exec.Command("git", "clone", "--bare", source, origin).CombinedOutput(); err != nil {
		t.Fatalf("create origin: %v\n%s", err, out)
	}
	runGit(t, source, "remote", "add", "origin", origin)
	runGit(t, source, "fetch", "origin", "+refs/heads/*:refs/remotes/origin/*")
	return gitCoherenceFixture{source: source, origin: origin, a: a, b: b, c: c}
}

func (f gitCoherenceFixture) plan(t *testing.T, target string) gitCoherencePlan {
	t.Helper()
	plan, blocked := syncGitCoherencePlan(baseConfig(), Repo{Root: f.source, RemoteURL: f.origin, Head: target})
	if blocked || !plan.enabled() {
		t.Fatalf("coherence plan unavailable: blocked=%v plan=%#v", blocked, plan)
	}
	return plan
}

func (f gitCoherenceFixture) workspace(t *testing.T, target string, symbolic bool) string {
	t.Helper()
	workdir := filepath.Join(t.TempDir(), "work")
	if out, err := exec.Command("git", "clone", "--quiet", f.origin, workdir).CombinedOutput(); err != nil {
		t.Fatalf("clone workspace: %v\n%s", err, out)
	}
	if symbolic {
		runGit(t, workdir, "checkout", "-q", "-B", "workspace", target)
	} else {
		runGit(t, workdir, "checkout", "-q", "--detach", target)
	}
	return workdir
}

func (f gitCoherenceFixture) linkedWorkspace(t *testing.T, target string) string {
	t.Helper()
	base := filepath.Join(t.TempDir(), "base")
	if out, err := exec.Command("git", "clone", "--quiet", f.origin, base).CombinedOutput(); err != nil {
		t.Fatalf("clone linked base: %v\n%s", err, out)
	}
	workdir := filepath.Join(filepath.Dir(base), "linked")
	runGit(t, base, "worktree", "add", "--detach", workdir, target)
	return workdir
}

func coherenceMetaDir(t *testing.T, workdir string) string {
	t.Helper()
	meta := gitOutput(workdir, "rev-parse", "--git-path", "crabbox")
	if !filepath.IsAbs(meta) {
		meta = filepath.Join(workdir, meta)
	}
	if err := os.MkdirAll(meta, 0o755); err != nil {
		t.Fatal(err)
	}
	return meta
}

func stageCoherenceFinalize(t *testing.T, workdir, token string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(coherenceMetaDir(t, workdir), remoteSyncPendingManifestName(token)), []byte("tracked.txt\x00"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runCoherenceFinalize(workdir string, plan gitCoherencePlan, token, fingerprint string, env ...string) ([]byte, error) {
	cmd := exec.Command("/bin/sh", "-c", remoteFinalizeSync(workdir, remoteSyncFinalizeOptions{
		Token:       token,
		Fingerprint: fingerprint,
		Coherence:   plan,
	}))
	cmd.Env = append(os.Environ(), env...)
	return cmd.CombinedOutput()
}

func readCoherentFingerprint(t *testing.T, workdir string, plan gitCoherencePlan) string {
	t.Helper()
	out, err := exec.Command("bash", "-lc", remoteReadSyncFingerprint(workdir, plan)).CombinedOutput()
	if err != nil {
		t.Fatalf("read fingerprint: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

func coherenceIndexPath(t *testing.T, workdir string) string {
	t.Helper()
	index := gitOutput(workdir, "rev-parse", "--git-path", "index")
	if !filepath.IsAbs(index) {
		index = filepath.Join(workdir, index)
	}
	return index
}

func coherenceIndexBytes(t *testing.T, workdir string) []byte {
	t.Helper()
	index := coherenceIndexPath(t, workdir)
	data, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func requireGitOutput(t *testing.T, workdir, want string, args ...string) {
	t.Helper()
	if got := gitOutput(workdir, args...); got != want {
		t.Fatalf("git %v=%q want %q", args, got, want)
	}
}

func newGitCoherenceFixtureWithDeletedTopic(t *testing.T) gitCoherenceFixture {
	t.Helper()
	f := newGitCoherenceFixture(t)
	runGit(t, f.origin, "update-ref", "refs/heads/alpha-deleted", f.b)
	runGit(t, f.origin, "update-ref", "refs/heads/release", f.c)
	runGit(t, f.source, "fetch", "--no-prune", "origin", "+refs/heads/*:refs/remotes/origin/*")
	runGit(t, f.origin, "update-ref", "-d", "refs/heads/alpha-deleted")
	runGit(t, f.source, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	runGit(t, f.source, "checkout", "--quiet", "--detach", f.b)
	requireGitOutput(t, f.origin, "", "for-each-ref", "refs/heads/alpha-deleted")
	requireGitOutput(t, f.source, f.b, "rev-parse", "refs/remotes/origin/alpha-deleted")
	refs := gitOutput(f.source, "for-each-ref", "--format=%(refname) %(objectname) %(symref)", "refs/remotes/origin")
	t.Cleanup(func() {
		requireGitOutput(t, f.source, refs, "for-each-ref", "--format=%(refname) %(objectname) %(symref)", "refs/remotes/origin")
		requireGitOutput(t, f.source, f.b, "rev-parse", "HEAD")
	})
	return f
}

func TestRemoteGitSeedAndCoherencePreferContainingBranchOverDeletedTopic(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell Git seed/coherence fixture")
	}
	t.Parallel()
	f := newGitCoherenceFixtureWithDeletedTopic(t)
	for _, tc := range []struct {
		name, configuredBase, baseRef, want string
	}{
		{"repository base", "", "main", "main"},
		{"origin default", "", "missing", "main"},
		{"configured base after default deletion", "release", "main", "release"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reusedWorkdir := f.workspace(t, f.c, true)
			if tc.configuredBase != "" {
				runGit(t, f.origin, "update-ref", "-d", "refs/heads/main")
				requireGitOutput(t, f.source, f.c, "rev-parse", "refs/remotes/origin/main")
			}
			cfg := baseConfig()
			cfg.Sync.BaseRef = tc.configuredBase
			plan, blocked := syncGitCoherencePlan(cfg, Repo{
				Root: f.source, RemoteURL: f.origin, Head: f.b, BaseRef: tc.baseRef,
			})
			if blocked || !plan.enabled() {
				t.Fatalf("coherence plan unavailable: blocked=%v plan=%#v", blocked, plan)
			}
			for _, mode := range []string{"seed", "coherence"} {
				t.Run(mode, func(t *testing.T) {
					var workdir string
					if mode == "seed" {
						workdir = filepath.Join(t.TempDir(), "work")
						if out, err := exec.Command("/bin/sh", "-c", remoteGitSeed(workdir, plan)).CombinedOutput(); err != nil {
							t.Fatalf("seed via %q: %v\n%s", plan.Branch, err, out)
						}
					} else {
						workdir = reusedWorkdir
						mustWriteTestFile(t, filepath.Join(workdir, "tracked.txt"), "B\n")
						const token = "abababababababababababababababab"
						stageCoherenceFinalize(t, workdir, token)
						if out, err := runCoherenceFinalize(workdir, plan, token, "fp-b"); err != nil {
							t.Fatalf("coherence via %q: %v\n%s", plan.Branch, err, out)
						}
						if got := readCoherentFingerprint(t, workdir, plan); got != "fp-b" {
							t.Fatalf("coherent fingerprint=%q", got)
						}
					}
					if plan.Branch != tc.want {
						t.Fatalf("branch=%q, want %q", plan.Branch, tc.want)
					}
					requireGitOutput(t, workdir, f.b, "rev-parse", "HEAD")
					requireGitOutput(t, workdir, gitOutput(f.source, "rev-parse", f.b+"^{tree}"), "write-tree")
					requireGitOutput(t, workdir, f.c, "rev-parse", "refs/remotes/origin/"+tc.want)
					requireGitOutput(t, workdir, "", "status", "--porcelain")
				})
			}
		})
	}
}

func TestWindowsGitSeedAndCoherenceUseContainingBranch(t *testing.T) {
	t.Parallel()
	f := newGitCoherenceFixtureWithDeletedTopic(t)
	plan, blocked := syncGitCoherencePlan(baseConfig(), Repo{
		Root: f.source, RemoteURL: f.origin, Head: f.b, BaseRef: "origin/main",
	})
	if blocked || !plan.enabled() {
		t.Fatalf("coherence plan unavailable: blocked=%v plan=%#v", blocked, plan)
	}
	tree := gitOutput(f.source, "rev-parse", f.b+"^{tree}")
	for _, tc := range []struct {
		name, command string
		want          []string
	}{
		{"seed", windowsGitSeed(`C:\work\repo`, plan), []string{
			"--single-branch --branch 'main' $expectedOrigin $tmp",
			"checkout --quiet --detach '" + f.b + "'",
			"$expectedTree = '" + tree + "'",
		}},
		{"coherence", windowsGitCoherence(`C:\work\repo`, plan), []string{
			`("+refs/heads/" + 'main' + ":" + $tmpRef)`,
			"$target = '" + f.b + "'; $tree = '" + tree + "'",
			"git merge-base --is-ancestor $target $tmpRef",
			"git update-ref --no-deref HEAD $target $oldHead",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decoded := decodePowerShellCommand(t, tc.command)
			for _, want := range tc.want {
				if !strings.Contains(decoded, want) {
					t.Errorf("Windows %s missing %q (plan branch=%q)", tc.name, want, plan.Branch)
				}
			}
			if strings.Contains(decoded, "alpha-deleted") || strings.Contains(decoded, f.c) {
				t.Error("Windows command selected deleted topic or newer branch tip")
			}
		})
	}
}

func TestRemoteGitCoherenceExitTrapBehavior(t *testing.T) {
	fixture := newGitCoherenceFixture(t)

	assertCleanup := func(t *testing.T, workdir string, token string) {
		t.Helper()
		meta := coherenceMetaDir(t, workdir)
		if _, err := os.Lstat(filepath.Join(meta, "sync-finalize-lock")); !os.IsNotExist(err) {
			t.Fatalf("finalization lock survived EXIT cleanup: %v", err)
		}
		statusFiles, err := filepath.Glob(filepath.Join(meta, "sync-git-status."+token+".*"))
		if err != nil {
			t.Fatal(err)
		}
		if len(statusFiles) != 0 {
			t.Fatalf("Git status files survived EXIT cleanup: %v", statusFiles)
		}
		ref := "refs/crabbox/sync-" + token
		if err := exec.Command("git", "-C", workdir, "show-ref", "--verify", "--quiet", ref).Run(); exitCode(err) != 1 {
			t.Fatalf("temporary coherence ref %s survived EXIT cleanup: %v", ref, err)
		}
	}

	t.Run("success", func(t *testing.T) {
		workdir := fixture.workspace(t, fixture.a, true)
		plan := fixture.plan(t, fixture.b)
		mustWriteTestFile(t, filepath.Join(workdir, "tracked.txt"), "B\n")
		const token = "51515151515151515151515151515151"
		stageCoherenceFinalize(t, workdir, token)
		out, err := runCoherenceFinalize(workdir, plan, token, "fp-success")
		if err != nil {
			t.Fatalf("successful finalize returned %v\n%s", err, out)
		}
		if strings.Contains(string(out), "pop_var_context") {
			t.Fatalf("successful finalize hit Bash function-context failure:\n%s", out)
		}
		if got := readCoherentFingerprint(t, workdir, plan); got != "fp-success" {
			t.Fatalf("successful finalize fingerprint=%q", got)
		}
		assertCleanup(t, workdir, token)
	})

	t.Run("failure", func(t *testing.T) {
		workdir := fixture.workspace(t, fixture.a, true)
		plan := fixture.plan(t, fixture.b)
		mustWriteTestFile(t, filepath.Join(workdir, "tracked.txt"), "B\n")
		const token = "52525252525252525252525252525252"
		stageCoherenceFinalize(t, workdir, token)
		beforeIndex := coherenceIndexBytes(t, workdir)
		tools := coherenceFailureTools(t, plan.Target)
		out, err := runCoherenceFinalize(workdir, plan, token, "fp-failure",
			"PATH="+tools+string(os.PathListSeparator)+os.Getenv("PATH"), "CRABBOX_FAIL_MV=complete")
		if got := exitCode(err); got != 94 {
			t.Fatalf("failed finalize exit=%d, want original exit 94: %v\n%s", got, err, out)
		}
		if strings.Contains(string(out), "pop_var_context") {
			t.Fatalf("failed finalize hit Bash function-context failure:\n%s", out)
		}
		requireGitOutput(t, workdir, fixture.a, "rev-parse", "HEAD")
		requireGitOutput(t, workdir, "refs/heads/workspace", "symbolic-ref", "-q", "HEAD")
		if afterIndex := coherenceIndexBytes(t, workdir); !bytes.Equal(afterIndex, beforeIndex) {
			t.Fatal("failed finalize did not restore the Git index")
		}
		if got := readCoherentFingerprint(t, workdir, plan); got != "" {
			t.Fatalf("failed finalize certified fingerprint %q", got)
		}
		assertCleanup(t, workdir, token)
	})
}

func TestRemoteGitCoherenceRepairsReuseBeforeFingerprintSkip(t *testing.T) {
	f := newGitCoherenceFixture(t)
	workdir := f.workspace(t, f.a, true)
	planB := f.plan(t, f.b)
	meta := coherenceMetaDir(t, workdir)
	for name, value := range map[string]string{
		"sync-fingerprint":             "fp-b",
		"sync-finalize-token":          "stale",
		"sync-finalize-complete-token": "stale",
	} {
		if err := os.WriteFile(filepath.Join(meta, name), []byte(value), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got := readCoherentFingerprint(t, workdir, planB); got != "" {
		t.Fatalf("stale A metadata certified B fingerprint %q", got)
	}

	mustWriteTestFile(t, filepath.Join(workdir, "tracked.txt"), "B\n")
	mustWriteTestFile(t, filepath.Join(workdir, "modified.txt"), "dirty\n")
	if err := os.Remove(filepath.Join(workdir, "deleted.txt")); err != nil {
		t.Fatal(err)
	}
	mustWriteTestFile(t, filepath.Join(workdir, "untracked.txt"), "keep\n")
	const tokenB = "11111111111111111111111111111111"
	stageCoherenceFinalize(t, workdir, tokenB)
	if out, err := runCoherenceFinalize(workdir, planB, tokenB, "fp-b"); err != nil {
		t.Fatalf("finalize B: %v\n%s", err, out)
	}
	requireGitOutput(t, workdir, f.b, "rev-parse", "HEAD")
	requireGitOutput(t, workdir, planB.Tree, "write-tree")
	requireGitOutput(t, workdir, f.a, "rev-parse", "refs/heads/workspace")
	requireGitOutput(t, workdir, "", "symbolic-ref", "-q", "HEAD")
	for path, want := range map[string]string{"tracked.txt": "B\n", "modified.txt": "dirty\n", "untracked.txt": "keep\n"} {
		got, err := os.ReadFile(filepath.Join(workdir, path))
		if err != nil || string(got) != want {
			t.Fatalf("%s=%q err=%v want %q", path, got, err, want)
		}
	}
	if _, err := os.Stat(filepath.Join(workdir, "deleted.txt")); !os.IsNotExist(err) {
		t.Fatalf("deleted overlay path was restored: %v", err)
	}
	if got := readCoherentFingerprint(t, workdir, planB); got != "fp-b" {
		t.Fatalf("coherent B fingerprint=%q", got)
	}
	if out, err := exec.Command("bash", "-lc", remoteInvalidateSyncFingerprintForTarget(SSHTarget{TargetOS: targetLinux}, workdir, false)).CombinedOutput(); err != nil {
		t.Fatalf("invalidate fingerprint: %v\n%s", err, out)
	}
	if got := readCoherentFingerprint(t, workdir, planB); got != "" {
		t.Fatalf("executed workspace retained fingerprint %q", got)
	}

	planC := f.plan(t, f.c)
	mustWriteTestFile(t, filepath.Join(workdir, "tracked.txt"), "C\n")
	const tokenC = "22222222222222222222222222222222"
	stageCoherenceFinalize(t, workdir, tokenC)
	if out, err := runCoherenceFinalize(workdir, planC, tokenC, "fp-c"); err != nil {
		t.Fatalf("finalize C: %v\n%s", err, out)
	}
	if got := gitOutput(workdir, "rev-parse", "HEAD"); got != f.c {
		t.Fatalf("HEAD=%s want C=%s", got, f.c)
	}
	if got := readCoherentFingerprint(t, workdir, planC); got != "fp-c" {
		t.Fatalf("coherent C fingerprint=%q", got)
	}
}

func TestRemoteGitCoherenceFailsClosedWhenAdvertisedBranchCannotBeVerified(t *testing.T) {
	f := newGitCoherenceFixture(t)
	workdir := f.workspace(t, f.a, true)
	plan := f.plan(t, f.b)
	plan.Branch = "missing"
	const token = "abababababababababababababababab"
	stageCoherenceFinalize(t, workdir, token)
	out, err := runCoherenceFinalize(workdir, plan, token, "must-not-publish")
	if err == nil || !strings.Contains(string(out), "Git coherence fetch failed") {
		t.Fatalf("unverified coherence out=%q err=%v", out, err)
	}
	requireGitOutput(t, workdir, f.a, "rev-parse", "HEAD")
	if got := readCoherentFingerprint(t, workdir, plan); got != "" {
		t.Fatalf("failed coherence published fingerprint %q", got)
	}
	if _, statErr := os.Lstat(filepath.Join(coherenceMetaDir(t, workdir), "sync-finalize-lock")); !os.IsNotExist(statErr) {
		t.Fatalf("failed coherence stranded finalization lock: %v", statErr)
	}
}

func TestRemoteGitCoherenceSupportsDetachedSparseSplitAndLinkedIndexes(t *testing.T) {
	f := newGitCoherenceFixture(t)
	for _, mode := range []string{"detached", "sparse", "split", "linked"} {
		t.Run(mode, func(t *testing.T) {
			workdir := f.workspace(t, f.a, false)
			switch mode {
			case "sparse":
				runGit(t, workdir, "sparse-checkout", "init", "--cone", "--sparse-index")
				runGit(t, workdir, "sparse-checkout", "set", "src")
			case "split":
				runGit(t, workdir, "update-index", "--split-index")
			case "linked":
				workdir = f.linkedWorkspace(t, f.a)
			}
			mustWriteTestFile(t, filepath.Join(workdir, "tracked.txt"), "B\n")
			token := fmt.Sprintf("%032x", len(mode)+10)
			plan := f.plan(t, f.b)
			stageCoherenceFinalize(t, workdir, token)
			if out, err := runCoherenceFinalize(workdir, plan, token, "fp-"+mode); err != nil {
				t.Fatalf("finalize: %v\n%s", err, out)
			}
			if got := gitOutput(workdir, "rev-parse", "HEAD"); got != f.b {
				t.Fatalf("HEAD=%s want %s", got, f.b)
			}
			if got := gitOutput(workdir, "write-tree"); got != plan.Tree {
				t.Fatalf("tree=%s want %s", got, plan.Tree)
			}
		})
	}
}

func coherenceFailureTools(t *testing.T, target string) string {
	t.Helper()
	dir := t.TempDir()
	gitScript := `#!/bin/sh
case "$CRABBOX_FAIL_GIT:$1" in
  read-tree:read-tree|write-tree:write-tree|diff-files:diff-files) exit 91 ;;
esac
if [ "$CRABBOX_FAIL_GIT" = cas ] && [ "$1" = update-ref ] && [ "$2" = --no-deref ] && [ "$3" = HEAD ]; then exit 92; fi
if [ "$CRABBOX_FAIL_GIT" = raw ] && [ "$1" = fetch ]; then
  for arg do case "$arg" in +` + target + `:refs/crabbox/*) exit 93 ;; esac; done
fi
exec ` + shellQuote(gitOutput("", "--exec-path")+"/git") + ` "$@"
`
	mvScript := `#!/bin/sh
last=
for arg do last="$arg"; done
case "$CRABBOX_FAIL_MV:$last" in
  fingerprint:*/sync-fingerprint|complete:*/sync-finalize-complete-token) exit 94 ;;
esac
if [ "$CRABBOX_FAIL_MV" = concurrent-index ]; then
  case "$last" in */sync-fingerprint) index=$(` + shellQuote(gitOutput("", "--exec-path")+"/git") + ` -C "$CRABBOX_CONCURRENT_WORKDIR" rev-parse --git-path index); case "$index" in /*) ;; *) index="$CRABBOX_CONCURRENT_WORKDIR/$index" ;; esac; printf concurrent-index > "$index"; exit 94 ;; esac
fi
if [ "$CRABBOX_FAIL_MV" = concurrent-head ]; then
  case "$last" in */sync-fingerprint) ` + shellQuote(gitOutput("", "--exec-path")+"/git") + ` -C "$CRABBOX_CONCURRENT_WORKDIR" update-ref --no-deref HEAD "$CRABBOX_CONCURRENT_HEAD" "$CRABBOX_TARGET_HEAD"; exit 94 ;; esac
fi
if [ "$CRABBOX_FAIL_MV" = concurrent-branch ]; then
  case "$last" in */sync-fingerprint) ` + shellQuote(gitOutput("", "--exec-path")+"/git") + ` -C "$CRABBOX_CONCURRENT_WORKDIR" update-ref refs/heads/workspace "$CRABBOX_CONCURRENT_HEAD" "$CRABBOX_OLD_HEAD"; exit 94 ;; esac
fi
if [ "$CRABBOX_FAIL_MV" = post-install-index ]; then case "$1" in *index.crabbox*) /bin/mv "$@"; printf post-install-index > "$last"; exit 0 ;; esac; fi
exec /bin/mv "$@"
`
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(gitScript), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mv"), []byte(mvScript), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestRemoteGitCoherenceRollsBackFailuresAndRetries(t *testing.T) {
	f := newGitCoherenceFixture(t)
	for _, failure := range []string{"read-tree", "write-tree", "diff-files", "cas", "fingerprint", "complete"} {
		t.Run(failure, func(t *testing.T) {
			workdir := f.workspace(t, f.a, true)
			plan := f.plan(t, f.b)
			mustWriteTestFile(t, filepath.Join(workdir, "tracked.txt"), "B\n")
			token := fmt.Sprintf("%032x", len(failure)+100)
			stageCoherenceFinalize(t, workdir, token)
			beforeIndex := coherenceIndexBytes(t, workdir)
			tools := coherenceFailureTools(t, plan.Target)
			env := []string{"PATH=" + tools + string(os.PathListSeparator) + os.Getenv("PATH")}
			if failure == "fingerprint" || failure == "complete" {
				env = append(env, "CRABBOX_FAIL_MV="+failure)
			} else {
				env = append(env, "CRABBOX_FAIL_GIT="+failure)
			}
			if out, err := runCoherenceFinalize(workdir, plan, token, "fp-"+failure, env...); err == nil {
				t.Fatalf("fault %s unexpectedly succeeded\n%s", failure, out)
			}
			requireGitOutput(t, workdir, f.a, "rev-parse", "HEAD")
			requireGitOutput(t, workdir, "refs/heads/workspace", "symbolic-ref", "-q", "HEAD")
			if afterIndex := coherenceIndexBytes(t, workdir); !bytes.Equal(afterIndex, beforeIndex) {
				t.Fatal("index bytes changed after failed finalization")
			}
			if got := readCoherentFingerprint(t, workdir, plan); got != "" {
				t.Fatalf("failed finalization certified fingerprint %q", got)
			}
			if out, err := runCoherenceFinalize(workdir, plan, token, "fp-"+failure); err != nil {
				t.Fatalf("retry: %v\n%s", err, out)
			}
			if got := readCoherentFingerprint(t, workdir, plan); got != "fp-"+failure {
				t.Fatalf("retry fingerprint=%q", got)
			}
		})
	}
}
func TestRemoteGitCoherenceDoesNotOverwriteConcurrentRollbackChanges(t *testing.T) {
	f := newGitCoherenceFixture(t)
	for _, failure := range []string{"concurrent-index", "post-install-index", "concurrent-head", "concurrent-branch"} {
		t.Run(failure, func(t *testing.T) {
			workdir := f.workspace(t, f.a, true)
			plan := f.plan(t, f.b)
			mustWriteTestFile(t, filepath.Join(workdir, "tracked.txt"), "B\n")
			token := fmt.Sprintf("%032x", len(failure)+200)
			stageCoherenceFinalize(t, workdir, token)
			tools := coherenceFailureTools(t, plan.Target)
			env := []string{"PATH=" + tools + string(os.PathListSeparator) + os.Getenv("PATH"), "CRABBOX_FAIL_MV=" + failure, "CRABBOX_CONCURRENT_WORKDIR=" + workdir, "CRABBOX_CONCURRENT_HEAD=" + f.c, "CRABBOX_TARGET_HEAD=" + f.b, "CRABBOX_OLD_HEAD=" + f.a}
			out, err := runCoherenceFinalize(workdir, plan, token, "fp-"+failure, env...)
			if err == nil {
				t.Fatalf("concurrent fault unexpectedly succeeded\n%s", out)
			}
			switch failure {
			case "concurrent-index", "post-install-index":
				if got := coherenceIndexBytes(t, workdir); string(got) != failure {
					t.Fatalf("concurrent index overwritten: %q", got)
				}
				requireGitOutput(t, workdir, f.a, "rev-parse", "HEAD")
				if failure == "post-install-index" {
					requireGitOutput(t, workdir, "refs/heads/workspace", "symbolic-ref", "-q", "HEAD")
				}
			case "concurrent-head":
				requireGitOutput(t, workdir, f.c, "rev-parse", "HEAD")
			case "concurrent-branch":
				requireGitOutput(t, workdir, f.a, "rev-parse", "HEAD")
				requireGitOutput(t, workdir, "", "symbolic-ref", "-q", "HEAD")
				requireGitOutput(t, workdir, f.c, "rev-parse", "refs/heads/workspace")
			}
		})
	}
}
func TestRemoteGitCoherenceRepairsAndVerifiesMismatchedOrigin(t *testing.T) {
	f := newGitCoherenceFixture(t)
	workdir := f.workspace(t, f.a, false)
	wrong := filepath.Join(t.TempDir(), "wrong.git")
	if err := os.Mkdir(wrong, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, wrong, "init", "--bare")
	runGit(t, workdir, "remote", "set-url", "origin", wrong)
	plan := f.plan(t, f.b)
	mustWriteTestFile(t, filepath.Join(workdir, "tracked.txt"), "B\n")
	const token = "33333333333333333333333333333333"
	meta := coherenceMetaDir(t, workdir)
	for name, value := range map[string]string{
		"sync-fingerprint":             "stale",
		"sync-finalize-token":          token,
		"sync-finalize-complete-token": token,
	} {
		if err := os.WriteFile(filepath.Join(meta, name), []byte(value), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got := readCoherentFingerprint(t, workdir, plan); got != "" {
		t.Fatalf("mismatched origin certified fingerprint %q", got)
	}
	stageCoherenceFinalize(t, workdir, token)
	if out, err := runCoherenceFinalize(workdir, plan, token, "fp-b"); err != nil {
		t.Fatalf("repair origin: %v\n%s", err, out)
	}
	if got := gitOutput(workdir, "remote", "get-url", "origin"); got != plan.RemoteURL {
		t.Fatalf("repaired origin=%q want planned origin", got)
	}
	if got := gitOutput(workdir, "rev-parse", "HEAD"); got != f.b {
		t.Fatalf("HEAD=%s want %s", got, f.b)
	}
	if got := readCoherentFingerprint(t, workdir, plan); got != "fp-b" {
		t.Fatalf("repaired origin fingerprint=%q", got)
	}
}

func TestRemoteGitCoherenceRejectsAncestorRepository(t *testing.T) {
	f := newGitCoherenceFixture(t)
	outer := f.workspace(t, f.b, false)
	workdir := filepath.Join(outer, "nested")
	if err := os.Mkdir(workdir, 0o755); err != nil {
		t.Fatal(err)
	}
	plan := f.plan(t, f.b)
	const token = "44444444444444444444444444444444"
	meta := filepath.Join(workdir, ".crabbox")
	if err := os.Mkdir(meta, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"sync-fingerprint":                   "ancestor",
		"sync-finalize-token":                token,
		"sync-finalize-complete-token":       token,
		remoteSyncPendingManifestName(token): "nested.txt\x00",
		remoteSyncPendingDeletedName(token):  "",
	} {
		if err := os.WriteFile(filepath.Join(meta, name), []byte(value), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got := readCoherentFingerprint(t, workdir, plan); got != "" {
		t.Fatalf("ancestor repository certified fingerprint %q", got)
	}
	if out, err := runCoherenceFinalize(workdir, plan, token, "nested"); err != nil {
		t.Fatalf("ancestor finalize: %v\n%s", err, out)
	}
	if got := gitOutput(outer, "rev-parse", "HEAD"); got != f.b {
		t.Fatalf("ancestor HEAD changed to %s", got)
	}
	if _, err := os.Stat(filepath.Join(meta, "sync-fingerprint")); !os.IsNotExist(err) {
		t.Fatalf("ancestor finalize published fingerprint: %v", err)
	}
}

func TestWindowsGitCoherenceGeneration(t *testing.T) {
	decoded := decodePowerShellCommand(t, windowsGitCoherence(`C:\work\repo`, gitCoherencePlan{
		RemoteURL: "https://example.test/repo.git",
		Target:    strings.Repeat("a", 40),
		Tree:      strings.Repeat("b", 40),
		Branch:    "main",
	}))
	for _, want := range []string{
		"refs/crabbox/sync-",
		"https://example.test/repo.git",
		"+refs/heads/",
		"rev-parse --show-toplevel",
		"GetFinalPathNameByHandle",
		"Test-CrabboxSameDirectory $workdir $reportedRoot",
		"remote set-url origin",
		"git read-tree --reset",
		"git write-tree",
		"& git update-ref --no-deref HEAD $target $oldHead",
		"& git update-ref --no-deref HEAD $oldHead $target",
		"Git HEAD remained symbolic during coherence",
		"Git symbolic branch changed during coherence",
		"Git coherence fetch failed",
		"requested commit is not on advertised branch",
		"requested Git tree verification failed",
		"& git symbolic-ref HEAD $oldSym",
		"[IO.File]::Replace($candidate, $index, $replaceBackup)",
		"[IO.File]::Replace($replaceBackup, $index, $rollbackBackup)",
		"ReadAllBytes($replaceBackup), [IO.File]::ReadAllBytes($compare)",
		"ReadAllBytes($index), [IO.File]::ReadAllBytes($verify)",
		"$failure = $_",
		"$rollbackError = $null",
	} {
		if !strings.Contains(decoded, want) {
			t.Fatalf("Windows coherence script missing %q:\n%s", want, decoded)
		}
	}
	mergeBase := strings.Index(decoded, "& git merge-base --is-ancestor")
	ancestryStatus := strings.Index(decoded, "$isAncestor = $LASTEXITCODE -eq 0")
	targetTree := strings.Index(decoded, "$targetTree = & git rev-parse")
	if mergeBase < 0 || ancestryStatus <= mergeBase || targetTree <= ancestryStatus {
		t.Fatalf("Windows ancestry status is not preserved before target-tree inspection:\n%s", decoded)
	}
	if strings.Contains(decoded, "reset --hard") {
		t.Fatalf("Windows coherence script uses destructive Git:\n%s", decoded)
	}
	if strings.Contains(decoded, "Resolve-Path -LiteralPath $workdir") {
		t.Fatalf("Windows coherence script compares textual root paths:\n%s", decoded)
	}
	if strings.Contains(decoded, `[IO.File]::Replace($candidate, $index, $null)`) ||
		strings.Contains(decoded, "NullString") {
		t.Fatalf("Windows coherence script uses an invalid replacement backup path:\n%s", decoded)
	}
	headRollback := strings.Index(decoded, `[InvalidOperationException]::new("Git HEAD rollback failed")`)
	indexRollback := strings.Index(decoded, `[IO.File]::Replace($replaceBackup, $index, $rollbackBackup)`)
	throwRollback := strings.Index(decoded, `if ($rollbackError) { throw $rollbackError }`)
	if headRollback < 0 || indexRollback <= headRollback || throwRollback <= indexRollback {
		t.Fatalf("Windows rollback does not preserve the first error through index restoration:\n%s", decoded)
	}
	if got := strings.Count(decoded, "& git update-ref --no-deref HEAD"); got != 2 {
		t.Fatalf("Windows coherence has %d detached HEAD updates, want mutation and rollback only:\n%s", got, decoded)
	}
	if strings.Contains(decoded, "git update-ref $oldSym") {
		t.Fatalf("Windows coherence advances a symbolic branch ref:\n%s", decoded)
	}
}

func TestWindowsGitSeedChecksGitBeforeInstallingClone(t *testing.T) {
	decoded := decodePowerShellCommand(t, windowsGitSeed(`C:\work\repo`, gitCoherencePlan{
		RemoteURL: "https://example.test/repo.git",
		Target:    strings.Repeat("a", 40),
		Tree:      strings.Repeat("b", 40),
		Branch:    "main",
	}))
	clone := strings.Index(decoded, "& git clone")
	cloneCheck := strings.Index(decoded, `if ($LASTEXITCODE -ne 0) { throw "Git seed clone failed" }`)
	checkout := strings.Index(decoded, "& git -C $tmp checkout")
	checkoutCheck := strings.Index(decoded, `if ($LASTEXITCODE -ne 0) { throw "Git seed checkout failed" }`)
	removeWorkdir := strings.Index(decoded, "Remove-Item -LiteralPath $workdir -Recurse -Force")
	moveClone := strings.Index(decoded, "Move-Item -LiteralPath $tmp -Destination $workdir")
	if clone < 0 || cloneCheck <= clone || checkout <= cloneCheck || checkoutCheck <= checkout ||
		removeWorkdir <= checkoutCheck || moveClone <= removeWorkdir {
		t.Fatalf("Windows seed can install before clone and checkout verification:\n%s", decoded)
	}
	for _, want := range []string{
		"rev-parse --show-toplevel",
		"GetFinalPathNameByHandle",
		"Test-CrabboxSameDirectory $Path $reportedRoot",
		"Test-UsableGitWorkspace",
		`$ErrorActionPreference = "Continue"`,
		"rev-parse --verify 'HEAD^{commit}'",
		"rev-parse --git-path index",
		"Test-Path -LiteralPath $index -PathType Leaf",
		"git -C $Path write-tree",
		"remote set-url origin",
		`if ($tmp -and (Test-Path -LiteralPath $tmp))`,
	} {
		if !strings.Contains(decoded, want) {
			t.Fatalf("Windows seed missing %q:\n%s", want, decoded)
		}
	}
	if strings.Contains(decoded, "Resolve-Path -LiteralPath $Path") {
		t.Fatalf("Windows seed compares textual root paths:\n%s", decoded)
	}
	usable := strings.Index(decoded, "function Test-UsableGitWorkspace")
	if usable < 0 {
		t.Fatalf("Windows seed is missing the workspace usability probe:\n%s", decoded)
	}
	probePreference := usable + strings.Index(decoded[usable:], `$ErrorActionPreference = "Continue"`)
	headProbe := usable + strings.Index(decoded[usable:], "$head = & git -C $Path rev-parse --verify")
	if probePreference <= usable || headProbe <= probePreference {
		t.Fatalf("Windows seed does not scope expected native Git failures before probing HEAD:\n%s", decoded)
	}
	reuse := strings.Index(decoded, "if (Test-UsableGitWorkspace $workdir)")
	startClone := strings.Index(decoded, "$tmp = Join-Path")
	if reuse < 0 || startClone <= reuse || !strings.Contains(decoded[reuse:startClone], "Repair-Origin $workdir\n  exit 0") {
		t.Fatalf("Windows seed does not reject unborn or missing-index roots before reuse:\n%s", decoded)
	}
	verifyWorkspace := strings.Index(decoded, "if (-not (Test-UsableGitWorkspace $tmp))")
	verifyTree := strings.Index(decoded, "if ($expectedTree) {")
	if verifyWorkspace <= checkoutCheck || verifyTree <= verifyWorkspace || removeWorkdir <= verifyTree {
		t.Fatalf("Windows seed can replace a workspace before the candidate index and tree are verified:\n%s", decoded)
	}
}

func TestWindowsGitSeedSeedOnlyPlanSkipsTreeEquality(t *testing.T) {
	decoded := decodePowerShellCommand(t, windowsGitSeed(`C:\work\repo`, gitCoherencePlan{
		RemoteURL: "https://example.test/repo.git",
		Target:    strings.Repeat("a", 40),
		Branch:    "main",
	}))
	for _, want := range []string{
		"$expectedTree = ''",
		"Test-UsableGitWorkspace $tmp",
		"return $treeStatus -eq 0 -and [bool]$tree",
		"if ($expectedTree) {",
		"([string]$seedTree).Trim() -ne $expectedTree",
	} {
		if !strings.Contains(decoded, want) {
			t.Fatalf("Windows seed-only script missing %q:\n%s", want, decoded)
		}
	}
	if strings.Contains(decoded, "([string]$seedTree).Trim() -ne ''") {
		t.Fatalf("Windows seed-only script unconditionally compares against an empty tree:\n%s", decoded)
	}
}

func TestWindowsGitRootIdentityGeneration(t *testing.T) {
	script := windowsGitRootIdentityScript()
	for _, want := range []string{
		"GetFinalPathNameByHandle",
		"Microsoft.Win32.SafeHandles.SafeFileHandle",
		"[IO.FileShare]::ReadWrite -bor [IO.FileShare]::Delete",
		"0x02000000",
		"Test-CrabboxSameDirectory",
		"[StringComparison]::OrdinalIgnoreCase",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("Windows Git root identity script missing %q:\n%s", want, script)
		}
	}
	if strings.Contains(script, "Resolve-Path") {
		t.Fatalf("Windows Git root identity relies on textual path normalization:\n%s", script)
	}
}

func TestWindowsGitRootIdentityAcceptsShortAliasAndRejectsDistinctRoot(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("native Windows path identity is covered by Windows CI")
	}
	base := t.TempDir()
	root := filepath.Join(base, "workspace with a long name")
	other := filepath.Join(base, "different workspace")
	for _, path := range []string{root, other} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	script := `$ErrorActionPreference = "Stop"
` + windowsGitRootIdentityScript() + `
Add-Type -Name ShortPath -Namespace Cbx -MemberDefinition '[DllImport("kernel32.dll",CharSet=CharSet.Unicode,SetLastError=true)]public static extern uint GetShortPathName(string l,System.Text.StringBuilder s,uint n);'
$longPath = Get-CrabboxFinalDirectoryPath ` + psQuote(root) + `
$buffer = New-Object Text.StringBuilder 32768
$length = [Cbx.ShortPath]::GetShortPathName($longPath, $buffer, $buffer.Capacity)
if ($length -eq 0 -or $length -ge $buffer.Capacity) { throw "short path lookup failed" }
$shortPath = $buffer.ToString()
if ([string]::Equals($shortPath, $longPath, [StringComparison]::OrdinalIgnoreCase)) { throw "short path fixture did not produce an alias" }
if (-not (Test-CrabboxSameDirectory $shortPath $longPath)) { throw "short and long paths did not match" }
if (Test-CrabboxSameDirectory $shortPath ` + psQuote(other) + `) { throw "distinct roots matched" }
`
	if out, err := runWindowsPowerShellScript(t, script); err != nil {
		t.Fatalf("Windows Git root identity failed: %v\n%s", err, out)
	}
}

func requireWindowsExactGitRoot(t *testing.T, workdir string) {
	t.Helper()
	actual := gitOutput(workdir, "rev-parse", "--show-toplevel")
	if actual == "" {
		t.Fatal("Git reported no top-level worktree")
	}
	actualInfo, err := os.Stat(filepath.FromSlash(actual))
	if err != nil {
		t.Fatalf("stat reported Git root %q: %v", actual, err)
	}
	expectedInfo, err := os.Stat(workdir)
	if err != nil {
		t.Fatalf("stat expected workdir %q: %v", workdir, err)
	}
	if !os.SameFile(actualInfo, expectedInfo) {
		t.Fatalf("Git root=%q does not identify exact workdir %q", actual, workdir)
	}
}

func requireWindowsGitWorkspaceState(t *testing.T, workdir string, plan gitCoherencePlan) {
	t.Helper()
	requireWindowsExactGitRoot(t, workdir)
	requireGitOutput(t, workdir, plan.Target, "rev-parse", "HEAD")
	requireGitOutput(t, workdir, plan.Tree, "write-tree")
	requireGitOutput(t, workdir, plan.RemoteURL, "remote", "get-url", "origin")
	index := gitOutput(workdir, "rev-parse", "--git-path", "index")
	if !filepath.IsAbs(index) {
		index = filepath.Join(workdir, index)
	}
	if _, err := os.Stat(index); err != nil {
		t.Fatalf("Git index missing: %v", err)
	}
}

func TestWindowsGitSeedReplacesUnusableExactRootsAfterVerifiedClone(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("native Windows PowerShell execution is covered by Windows CI")
	}
	f := newGitCoherenceFixture(t)
	plan := f.plan(t, f.b)
	for _, mode := range []string{"unborn", "missing-index"} {
		t.Run(mode, func(t *testing.T) {
			workdir := filepath.Join(t.TempDir(), "work")
			var missingIndex string
			switch mode {
			case "unborn":
				if err := os.MkdirAll(workdir, 0o755); err != nil {
					t.Fatal(err)
				}
				runGit(t, workdir, "init")
			case "missing-index":
				if out, err := exec.Command("git", "clone", "--quiet", f.origin, workdir).CombinedOutput(); err != nil {
					t.Fatalf("clone missing-index fixture: %v\n%s", err, out)
				}
				missingIndex = gitOutput(workdir, "rev-parse", "--git-path", "index")
				if !filepath.IsAbs(missingIndex) {
					missingIndex = filepath.Join(workdir, missingIndex)
				}
				if err := os.Remove(missingIndex); err != nil {
					t.Fatal(err)
				}
			}
			requireWindowsExactGitRoot(t, workdir)
			marker := filepath.Join(workdir, "preserve-until-verified.txt")
			mustWriteTestFile(t, marker, "preserve\n")

			unverified := plan
			unverified.Tree = strings.Repeat("f", 40)
			out, err := runDecodedWindowsPowerShell(t, windowsGitSeed(workdir, unverified))
			if err == nil || !strings.Contains(string(out), "Git seed tree verification failed") {
				t.Fatalf("unverified seed err=%v\n%s", err, out)
			}
			requireWindowsExactGitRoot(t, workdir)
			if got, readErr := os.ReadFile(marker); readErr != nil || string(got) != "preserve\n" {
				t.Fatalf("unverified seed replaced existing root: data=%q err=%v", got, readErr)
			}
			if missingIndex != "" {
				if _, statErr := os.Stat(missingIndex); !os.IsNotExist(statErr) {
					t.Fatalf("unverified seed changed missing-index root: %v", statErr)
				}
			}

			if out, err := runDecodedWindowsPowerShell(t, windowsGitSeed(workdir, plan)); err != nil {
				t.Fatalf("verified seed failed: %v\n%s", err, out)
			}
			requireWindowsGitWorkspaceState(t, workdir, plan)
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("verified seed did not replace unusable root: %v", err)
			}
		})
	}
}

func TestWindowsGitCoherenceSupportsDetachedSparseSplitAndLinkedIndexes(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("native Windows PowerShell execution is covered by Windows CI")
	}
	f := newGitCoherenceFixture(t)
	plan := f.plan(t, f.b)
	for _, mode := range []string{"detached", "sparse-index", "split-index", "linked-worktree"} {
		t.Run(mode, func(t *testing.T) {
			var workdir string
			switch mode {
			case "linked-worktree":
				workdir = f.linkedWorkspace(t, f.a)
				gitFile, err := os.ReadFile(filepath.Join(workdir, ".git"))
				if err != nil || !strings.HasPrefix(string(gitFile), "gitdir: ") {
					t.Fatalf("linked worktree .git=%q err=%v", gitFile, err)
				}
			default:
				workdir = f.workspace(t, f.a, false)
			}
			switch mode {
			case "sparse-index":
				runGit(t, workdir, "sparse-checkout", "init", "--cone", "--sparse-index")
				runGit(t, workdir, "sparse-checkout", "set", "src")
			case "split-index":
				runGit(t, workdir, "update-index", "--split-index")
			}
			requireWindowsExactGitRoot(t, workdir)
			requireGitOutput(t, workdir, f.a, "rev-parse", "HEAD")
			mustWriteTestFile(t, filepath.Join(workdir, "tracked.txt"), "B\n")

			if out, err := runDecodedWindowsPowerShell(t, windowsGitCoherence(workdir, plan)); err != nil {
				t.Fatalf("Windows coherence failed: %v\n%s", err, out)
			}
			requireWindowsGitWorkspaceState(t, workdir, plan)
		})
	}
}

func TestWindowsGitCoherenceRollbackRestoresIndexAfterSymbolicHeadFailure(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("native Windows PowerShell execution is covered by Windows CI")
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	f := newGitCoherenceFixture(t)
	plan := f.plan(t, f.b)
	for _, tc := range []struct {
		name                  string
		symbolic              bool
		injectRollbackFailure bool
		wantError             string
	}{
		{name: "symbolic", symbolic: true, injectRollbackFailure: true, wantError: "Git symbolic HEAD rollback failed"},
		{name: "detached", wantError: "Git coherence verification failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workdir := f.workspace(t, f.a, tc.symbolic)
			mustWriteTestFile(t, filepath.Join(workdir, "tracked.txt"), "B\n")
			beforeIndex := coherenceIndexBytes(t, workdir)
			beforeTree := gitOutput(workdir, "write-tree")
			beforeSym := gitOutput(workdir, "symbolic-ref", "-q", "HEAD")
			if beforeTree == plan.Tree || tc.symbolic != (beforeSym != "") {
				t.Fatalf("invalid rollback fixture: tree=%q target=%q sym=%q", beforeTree, plan.Tree, beforeSym)
			}

			shimDir := t.TempDir()
			tracePath := filepath.Join(shimDir, "git-shim.trace")
			beforeIndexPath := filepath.Join(shimDir, "before.index")
			if err := os.WriteFile(beforeIndexPath, beforeIndex, 0o600); err != nil {
				t.Fatal(err)
			}
			injectRollbackFailure := "$false"
			if tc.injectRollbackFailure {
				injectRollbackFailure = "$true"
			}
			script := `$ErrorActionPreference = "Stop"
$script:realGit = ` + psQuote(realGit) + `
$script:workdir = ` + psQuote(workdir) + `
$script:indexPath = ` + psQuote(coherenceIndexPath(t, workdir)) + `
$script:beforeIndexPath = ` + psQuote(beforeIndexPath) + `
$script:expectedOldHead = ` + psQuote(f.a) + `
$script:expectedTarget = ` + psQuote(plan.Target) + `
$script:expectedSymbolicHead = ` + psQuote(beforeSym) + `
$script:injectRollbackFailure = ` + injectRollbackFailure + `
$script:tracePath = ` + psQuote(tracePath) + `
$script:gitShimCalls = 0
$script:gitTrace = [Collections.Generic.List[string]]::new()
$script:headUpdateObserved = $false
$script:indexReplacementObserved = $false
$script:targetHeadObserved = $false
$script:symbolicHeadPreservedAtTarget = $false
$script:postHeadFailureInjected = $false
$script:branchRollbackCallObserved = $false
$script:symbolicHeadRestoredBeforeFailure = $false
$script:branchRollbackFailureInjected = $false
function git {
  $gitArgs = @($args | ForEach-Object { [string]$_ })
  $script:gitShimCalls++
  $null = $script:gitTrace.Add(($gitArgs -join '|'))
  $verb = if ($gitArgs.Count -gt 0) { $gitArgs[0] } else { "" }
  $isPostHeadVerify = $gitArgs.Count -ge 3 -and $verb -eq 'rev-parse' -and $gitArgs[1] -eq '--verify' -and $gitArgs[2] -eq 'HEAD^{commit}'
  if ($script:headUpdateObserved -and -not $script:postHeadFailureInjected -and $isPostHeadVerify) {
    $indexBytes = [IO.File]::ReadAllBytes($script:indexPath)
    if ([Linq.Enumerable]::SequenceEqual($indexBytes, [IO.File]::ReadAllBytes($script:beforeIndexPath))) {
      throw "Git shim reached post-HEAD verification before index replacement"
    }
    $verifyPattern = [IO.Path]::GetFileName($script:indexPath) + ".crabbox.*.verify"
    $verifyFiles = @(Get-ChildItem -LiteralPath ([IO.Path]::GetDirectoryName($script:indexPath)) -Filter $verifyPattern -File)
    if ($verifyFiles.Count -ne 1 -or -not [Linq.Enumerable]::SequenceEqual($indexBytes, [IO.File]::ReadAllBytes($verifyFiles[0].FullName))) {
      throw "Git shim did not observe the verified candidate index after replacement"
    }
    $installedHead = & $script:realGit -C $script:workdir rev-parse --verify 'HEAD^{commit}' 2>$null
    $headExit = $LASTEXITCODE
    if ($headExit -ne 0 -or ([string]$installedHead).Trim() -ne $script:expectedTarget) {
      throw "Git shim reached post-HEAD verification before HEAD replacement"
    }
	    if ($script:expectedSymbolicHead) {
	      $installedSym = & $script:realGit -C $script:workdir symbolic-ref -q HEAD 2>$null
	      $symExit = $LASTEXITCODE
	      $installedBranch = & $script:realGit -C $script:workdir rev-parse --verify ($script:expectedSymbolicHead + "^{commit}") 2>$null
	      $branchExit = $LASTEXITCODE
	      if ($symExit -eq 0 -or -not [string]::IsNullOrWhiteSpace([string]$installedSym) -or $branchExit -ne 0 -or ([string]$installedBranch).Trim() -ne $script:expectedOldHead) {
	        throw "Git shim did not observe detached HEAD with the symbolic branch preserved"
	      }
	      $script:symbolicHeadPreservedAtTarget = $true
    }
    $script:indexReplacementObserved = $true
    $script:targetHeadObserved = $true
    $script:postHeadFailureInjected = $true
    $global:LASTEXITCODE = 95
    return
  }
	  $isBranchRollback = $gitArgs.Count -ge 3 -and $verb -eq 'symbolic-ref' -and $gitArgs[1] -eq 'HEAD' -and $gitArgs[2] -eq $script:expectedSymbolicHead
  if ($script:injectRollbackFailure -and $script:postHeadFailureInjected -and $isBranchRollback -and -not $script:branchRollbackFailureInjected) {
    $script:branchRollbackCallObserved = $true
    & $script:realGit @gitArgs
    $rollbackExit = $LASTEXITCODE
    if ($rollbackExit -ne 0) {
      return
    }
    $restoredSym = & $script:realGit -C $script:workdir symbolic-ref -q HEAD 2>$null
    $symExit = $LASTEXITCODE
    $restoredBranch = & $script:realGit -C $script:workdir rev-parse --verify ($script:expectedSymbolicHead + "^{commit}") 2>$null
    $branchExit = $LASTEXITCODE
    if ($symExit -ne 0 -or ([string]$restoredSym).Trim() -ne $script:expectedSymbolicHead -or $branchExit -ne 0 -or ([string]$restoredBranch).Trim() -ne $script:expectedOldHead) {
	      throw "Git shim did not restore symbolic HEAD without changing its branch before fault injection"
    }
    $script:symbolicHeadRestoredBeforeFailure = $true
    $script:branchRollbackFailureInjected = $true
    $global:LASTEXITCODE = 96
    return
  }
  & $script:realGit @gitArgs
  $gitExit = $LASTEXITCODE
	  $isDetachedUpdate = $gitArgs.Count -ge 5 -and $verb -eq 'update-ref' -and $gitArgs[1] -eq '--no-deref' -and $gitArgs[2] -eq 'HEAD' -and $gitArgs[3] -eq $script:expectedTarget -and $gitArgs[4] -eq $script:expectedOldHead
	  if ($gitExit -eq 0 -and $isDetachedUpdate) {
    $script:headUpdateObserved = $true
  }
}
$script:coherenceError = $null
try {
  & {
` + decodePowerShellCommand(t, windowsGitCoherence(workdir, plan)) + `
  }
} catch {
  $script:coherenceError = $_
}
@(
  "calls=$($script:gitShimCalls)"
  "head-update=$($script:headUpdateObserved)"
  "index-installed=$($script:indexReplacementObserved)"
  "target-head=$($script:targetHeadObserved)"
  "symbolic-head-at-target=$($script:symbolicHeadPreservedAtTarget)"
  "post-head-failure=$($script:postHeadFailureInjected)"
  "branch-rollback-call=$($script:branchRollbackCallObserved)"
  "symbolic-head-restored=$($script:symbolicHeadRestoredBeforeFailure)"
  "branch-rollback-failure=$($script:branchRollbackFailureInjected)"
  "coherence-error=$(if ($script:coherenceError) { $script:coherenceError.Exception.Message } else { '<none>' })"
  $script:gitTrace | ForEach-Object { "git=$_" }
) | Set-Content -LiteralPath $script:tracePath -Encoding ASCII
if ($script:gitShimCalls -eq 0) { throw "Git shim was not invoked" }
if (-not $script:headUpdateObserved) { throw "Git shim did not observe the HEAD update" }
if (-not $script:postHeadFailureInjected) { throw "Git shim did not inject the post-HEAD verification failure" }
if ($script:injectRollbackFailure -and -not $script:branchRollbackFailureInjected) { throw "Git shim did not inject the symbolic branch rollback failure" }
if (-not $script:injectRollbackFailure -and $script:branchRollbackFailureInjected) { throw "Git shim unexpectedly injected a detached HEAD rollback failure" }
if ($null -eq $script:coherenceError) { throw "Git coherence unexpectedly succeeded" }
throw $script:coherenceError
`
			out, err := runWindowsPowerShellScript(t, script)
			if err == nil {
				t.Fatalf("injected rollback failure unexpectedly succeeded\n%s", out)
			}
			trace, readErr := os.ReadFile(tracePath)
			if readErr != nil {
				t.Fatalf("read Git shim trace: %v", readErr)
			}
			if !strings.Contains(string(out), tc.wantError) {
				t.Fatalf("unexpected rollback failure: %v\n%s\ntrace:\n%s", err, out, trace)
			}
			for _, want := range []string{
				"head-update=True",
				"index-installed=True",
				"target-head=True",
				"post-head-failure=True",
			} {
				if !strings.Contains(string(trace), want) {
					t.Fatalf("Git shim trace missing %q:\n%s", want, trace)
				}
			}
			for _, state := range []struct {
				key  string
				want bool
			}{
				{key: "branch-rollback-call", want: tc.injectRollbackFailure},
				{key: "symbolic-head-at-target", want: tc.symbolic},
				{key: "symbolic-head-restored", want: tc.injectRollbackFailure},
				{key: "branch-rollback-failure", want: tc.injectRollbackFailure},
			} {
				if got := strings.Contains(string(trace), state.key+"=True"); got != state.want {
					t.Fatalf("Git shim trace %s=%t want %t:\n%s", state.key, got, state.want, trace)
				}
			}
			if !strings.Contains(string(trace), "calls=") || strings.Contains(string(trace), "calls=0") {
				t.Fatalf("Git shim was not invoked:\n%s", trace)
			}
			if afterIndex := coherenceIndexBytes(t, workdir); !bytes.Equal(afterIndex, beforeIndex) {
				t.Fatal("Windows rollback did not restore the original index")
			}
			requireGitOutput(t, workdir, beforeTree, "write-tree")
			requireGitOutput(t, workdir, f.a, "rev-parse", "HEAD")
			requireGitOutput(t, workdir, beforeSym, "symbolic-ref", "-q", "HEAD")
			if tc.symbolic {
				requireGitOutput(t, workdir, f.a, "rev-parse", beforeSym+"^{commit}")
			}
		})
	}
}

func TestRemoteGitSeedRemovesFailedCheckout(t *testing.T) {
	got := remoteGitSeed("/work/repo", gitCoherencePlan{RemoteURL: "https://github.com/openclaw/crabbox.git", Target: "missing-sha", Tree: "tree", Branch: "main"})
	for _, want := range []string{
		"git -C \"$tmp\" checkout --quiet --detach",
		"cleanup_seed() { rm -rf -- \"$tmp\"; rm -f -- \"$transport_error\"; }",
		"trap cleanup_seed EXIT",
		"cat \"$transport_error\" >&2",
		"exit 78",
		"mv -- \"$tmp\" \"$workdir\"",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("remoteGitSeed missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, "git checkout --quiet FETCH_HEAD || true") {
		t.Fatalf("remoteGitSeed should not keep failed checkouts: %q", got)
	}
	for _, forbidden := range []string{"origin_transport_fallback", "CRABBOX_GIT_ORIGIN_FALLBACK", "Authentication failed", "repository not found"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("remoteGitSeed retained origin policy %q in %q", forbidden, got)
		}
	}
}

func TestRemoteGitOriginTransportClassification(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX origin transport classifier")
	}
	tests := []struct {
		name         string
		remoteURL    string
		message      string
		exitCode     int
		truncated    bool
		wantReason   string
		wantFallback bool
	}{
		{name: "authentication", remoteURL: "https://example.test/repo.git", message: "fatal: Authentication failed", exitCode: gitOriginRuntimeFallbackExitCode, wantReason: "origin_auth_required", wantFallback: true},
		{name: "HTTP 403", remoteURL: "https://example.test/repo.git", message: "fatal: unable to access: The requested URL returned error: 403", exitCode: gitOriginRuntimeFallbackExitCode, wantReason: "origin_auth_required", wantFallback: true},
		{name: "DNS", remoteURL: "https://example.test/repo.git", message: "fatal: unable to access: Could not resolve host: example.test", exitCode: gitOriginRuntimeFallbackExitCode, wantReason: "origin_unavailable", wantFallback: true},
		{name: "TLS", remoteURL: "https://example.test/repo.git", message: "fatal: unable to access: SSL certificate problem: unable to get local issuer certificate", exitCode: gitOriginRuntimeFallbackExitCode, wantReason: "origin_unavailable", wantFallback: true},
		{name: "firewall", remoteURL: "https://example.test/repo.git", message: "fatal: unable to access: No route to host", exitCode: gitOriginRuntimeFallbackExitCode, wantReason: "origin_unavailable", wantFallback: true},
		{name: "disconnected socket Linux", remoteURL: "https://example.test/repo.git", message: "fatal: unable to access 'https://example.test/repo.git/': getpeername() failed with errno 107: Transport endpoint is not connected", exitCode: gitOriginRuntimeFallbackExitCode, wantReason: "origin_unavailable", wantFallback: true},
		{name: "disconnected socket BSD", remoteURL: "https://example.test/repo.git", message: "fatal: unable to access: getpeername() failed with errno 57: Socket is not connected", exitCode: gitOriginRuntimeFallbackExitCode, wantReason: "origin_unavailable", wantFallback: true},
		{name: "other socket inspection error", remoteURL: "https://example.test/repo.git", message: "fatal: unable to access: getpeername() failed with errno 9: Bad file descriptor", exitCode: gitOriginRuntimeFallbackExitCode},
		{name: "unclassified HTTP failure", remoteURL: "https://example.test/repo.git", message: "fatal: unable to access: unknown transport failure", exitCode: gitOriginRuntimeFallbackExitCode},
		{name: "HTTP server failure beats disconnected socket", remoteURL: "https://example.test/repo.git", message: "fatal: The requested URL returned error: 503\ngetpeername() failed with errno 107: Transport endpoint is not connected", exitCode: gitOriginRuntimeFallbackExitCode},
		{name: "disconnected socket wrong exit", remoteURL: "https://example.test/repo.git", message: "getpeername() failed with errno 107: Transport endpoint is not connected", exitCode: 67},
		{name: "disconnected socket truncated", remoteURL: "https://example.test/repo.git", message: "getpeername() failed with errno 107: Transport endpoint is not connected", exitCode: gitOriginRuntimeFallbackExitCode, truncated: true},
		{name: "private HTTP repository", remoteURL: "https://example.test/repo.git", message: "remote: Repository not found.", exitCode: gitOriginRuntimeFallbackExitCode, wantReason: "origin_auth_required", wantFallback: true},
		{name: "filesystem unavailable", remoteURL: "/srv/git/repo.git", message: "fatal: '/srv/git/repo.git' does not appear to be a git repository", exitCode: gitOriginRuntimeFallbackExitCode, wantReason: "origin_unavailable", wantFallback: true},
		{name: "HTTP server failure beats transport", remoteURL: "https://example.test/repo.git", message: "fatal: The requested URL returned error: 503; Failed to connect", exitCode: gitOriginRuntimeFallbackExitCode},
		{name: "HTTP response", remoteURL: "https://example.test/repo.git", message: "fatal: unable to access: The requested URL returned error: 500", exitCode: gitOriginRuntimeFallbackExitCode},
		{name: "missing branch", remoteURL: "https://example.test/repo.git", message: "fatal: couldn't find remote ref absent", exitCode: gitOriginRuntimeFallbackExitCode},
		{name: "marker spoof", remoteURL: "https://example.test/repo.git", message: "CRABBOX_GIT_ORIGIN_FALLBACK:origin_unavailable", exitCode: gitOriginRuntimeFallbackExitCode},
		{name: "wrong exit", remoteURL: "https://example.test/repo.git", message: "fatal: Authentication failed", exitCode: 67},
		{name: "truncated authentication", remoteURL: "https://example.test/repo.git", message: "fatal: Authentication failed", exitCode: gitOriginRuntimeFallbackExitCode, truncated: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := exec.Command("sh", "-c", "exit "+strconv.Itoa(tt.exitCode)).Run()
			if tt.truncated {
				err = &gitOriginDiagnosticsTruncatedError{err: err}
			}
			reason, fallback := gitOriginRuntimeFallbackResult(tt.remoteURL, tt.message, err)
			if fallback != tt.wantFallback || reason != tt.wantReason {
				t.Fatalf("fallback=%t reason=%q err=%v output=%q", fallback, reason, err, tt.message)
			}
		})
	}
	if reason, fallback := gitOriginRuntimeFallbackResult("https://example.test/repo.git", "fatal: Authentication failed", nil); fallback || reason != "" {
		t.Fatalf("successful attempt classified fallback=%t reason=%q", fallback, reason)
	}
	functions := remoteGitOriginTransportFunctions()
	for _, forbidden := range []string{"grep", "origin_transport_fallback", "CRABBOX_GIT_ORIGIN_FALLBACK", "Authentication failed", "repository not found"} {
		if strings.Contains(functions, forbidden) {
			t.Fatalf("remote origin helper retained policy %q:\n%s", forbidden, functions)
		}
	}
}

func TestRemoteGitOriginAttemptCommandsDeferPolicyToGo(t *testing.T) {
	plan := gitCoherencePlan{
		RemoteURL: "https://example.test/repo.git",
		Target:    strings.Repeat("a", 40),
		Tree:      strings.Repeat("b", 40),
		Branch:    "main",
	}
	commands := map[string]string{
		"seed": remoteGitSeed("/work/repo", plan),
		"finalize": remoteFinalizeSync("/work/repo", remoteSyncFinalizeOptions{
			HydrateGit: true,
			Token:      strings.Repeat("c", 32),
			Coherence:  plan,
		}),
	}
	for name, command := range commands {
		t.Run(name, func(t *testing.T) {
			for _, want := range []string{`cat "$transport_error" >&2`, "exit 78"} {
				if !strings.Contains(command, want) {
					t.Fatalf("%s missing %q:\n%s", name, want, command)
				}
			}
			catIndex := strings.Index(command, `cat "$transport_error" >&2`)
			exitIndex := strings.Index(command[catIndex:], "exit 78")
			if exitIndex < 0 {
				t.Fatalf("%s does not emit transport diagnostics before exit 78:\n%s", name, command)
			}
			if name == "finalize" {
				diagnostic := "remote sync finalize failed: Git coherence fetch failed"
				diagnosticIndex := strings.Index(command, diagnostic)
				if diagnosticIndex < 0 || diagnosticIndex > catIndex {
					t.Fatalf("finalize does not emit %q before raw transport diagnostics:\n%s", diagnostic, command)
				}
			}
			for _, forbidden := range []string{"origin_transport_fallback", "CRABBOX_GIT_ORIGIN_FALLBACK", "Authentication failed", "repository not found"} {
				if strings.Contains(command, forbidden) {
					t.Fatalf("%s retained origin policy %q:\n%s", name, forbidden, command)
				}
			}
		})
	}
}

func newGitTransportFailureHTTPServer(t *testing.T) string {
	t.Helper()

	// Wait for the HTTP request before closing: resetting immediately after
	// accept races libcurl's connection inspection and varies its diagnostic.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "" {
			t.Error("transport failure fixture received origin credentials")
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack transport failure connection: %v", err)
			return
		}
		if err := conn.Close(); err != nil {
			t.Errorf("close transport failure connection: %v", err)
		}
	}))
	t.Cleanup(server.Close)
	return server.URL
}

func runGitControlWithShellHook(t *testing.T, hook, command string) ([]byte, error) {
	t.Helper()
	home := t.TempDir()
	marker := filepath.Join(home, "hook-ran")
	envFile := os.DevNull
	if hook == "logout" {
		mustWriteTestFile(t, filepath.Join(home, ".bash_profile"), ":\n")
		mustWriteTestFile(t, filepath.Join(home, ".bash_logout"), "printf logout >"+shellQuote(marker)+"\nfalse\n")
	} else {
		envFile = filepath.Join(home, "bash-env")
		mustWriteTestFile(t, envFile, "printf startup >"+shellQuote(marker)+"\nexit 97\n")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", "exec "+command)
	cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "SHLVL=0", "BASH_ENV=" + envFile, "ENV=" + envFile, "LC_ALL=C"}
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	if data, statErr := os.ReadFile(marker); !os.IsNotExist(statErr) {
		t.Errorf("Git control executed %s hook: marker=%q err=%v", hook, data, statErr)
	}
	return out, err
}

func TestRemoteGitControlsIgnoreShellHooks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX Git control shell")
	}
	f := newGitCoherenceFixture(t)
	for _, hook := range []string{"logout", "BASH_ENV"} {
		for _, scenario := range []string{"seed unavailable", "seed fresh", "seed reused", "finalize unavailable", "finalize success", "finalize tree mismatch"} {
			t.Run(hook+"/"+scenario, func(t *testing.T) {
				plan := f.plan(t, f.b)
				workdir := filepath.Join(t.TempDir(), "work")
				wantCode, wantReason := 0, ""
				if strings.HasSuffix(scenario, "unavailable") {
					plan.RemoteURL = filepath.Join(t.TempDir(), "absent-origin.git")
					wantCode, wantReason = gitOriginRuntimeFallbackExitCode, "origin_unavailable"
				}
				var command string
				if strings.HasPrefix(scenario, "seed") {
					if scenario == "seed reused" {
						workdir = f.workspace(t, f.b, false)
					}
					command = remoteGitSeed(workdir, plan)
				} else {
					workdir = f.workspace(t, f.a, true)
					mustWriteTestFile(t, filepath.Join(workdir, "tracked.txt"), "B\n")
					const token = "61616161616161616161616161616161"
					stageCoherenceFinalize(t, workdir, token)
					if scenario == "finalize tree mismatch" {
						plan.Tree = strings.Repeat("f", 40)
						wantCode = 67
					}
					command = remoteFinalizeSync(workdir, remoteSyncFinalizeOptions{Token: token, Fingerprint: "coherent", Coherence: plan})
				}
				out, err := runGitControlWithShellHook(t, hook, command)
				if got := exitCode(err); got != wantCode {
					t.Fatalf("Git control exit=%d want=%d err=%v output=%q", got, wantCode, err, out)
				}
				if reason, fallback := gitOriginRuntimeFallbackResult(plan.RemoteURL, string(out), err); reason != wantReason || fallback != (wantReason != "") {
					t.Fatalf("fallback=%t reason=%q want=%q output=%q", fallback, reason, wantReason, out)
				}
				if wantCode == 0 {
					requireGitOutput(t, workdir, f.b, "rev-parse", "HEAD")
					requireGitOutput(t, workdir, plan.Tree, "write-tree")
				} else if strings.HasPrefix(scenario, "finalize") {
					requireGitOutput(t, workdir, f.a, "rev-parse", "HEAD")
					if got := readCoherentFingerprint(t, workdir, plan); got != "" {
						t.Fatalf("failed finalizer certified fingerprint %q", got)
					}
				}
			})
		}
	}
}

func TestRemoteGitMetadataControlsIgnoreShellHooks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX Git control shell")
	}
	f := newGitCoherenceFixture(t)
	for _, hook := range []string{"logout", "BASH_ENV"} {
		for _, operation := range []string{"hydrate status", "fingerprint", "seed manifest", "discard pending"} {
			t.Run(hook+"/"+operation, func(t *testing.T) {
				workdir := f.workspace(t, f.c, false)
				plan := f.plan(t, f.c)
				meta := coherenceMetaDir(t, workdir)
				var command, want string
				var discarded []string
				var preserved map[string]string
				switch operation {
				case "hydrate status":
					mustWriteTestFile(t, filepath.Join(meta, "git-hydrate-base"), "main "+f.c+"\n")
					command, want = remoteGitHydrateStatus(workdir, "main", f.c), "marker base current"
				case "fingerprint":
					for name, value := range map[string]string{"sync-finalize-token": "complete", "sync-finalize-complete-token": "complete", "sync-fingerprint": "coherent"} {
						mustWriteTestFile(t, filepath.Join(meta, name), value)
					}
					command, want = remoteReadSyncFingerprint(workdir, plan), "coherent"
				case "seed manifest":
					command = remoteSeedSyncManifestFromGit(workdir)
				case "discard pending":
					const token = "81818181818181818181818181818181"
					const neighbor = "82828282828282828282828282828282"
					discarded = []string{remoteSyncPendingManifestName(token), remoteSyncPendingDeletedName(token)}
					preserved = map[string]string{
						remoteSyncPendingManifestName(neighbor): "neighbor.txt\x00",
						remoteSyncPendingDeletedName(neighbor):  "neighbor-deleted.txt\x00",
						"sync-manifest":                         "committed.txt\x00",
					}
					for _, name := range discarded {
						mustWriteTestFile(t, filepath.Join(meta, name), "discard.txt\x00")
					}
					for name, value := range preserved {
						mustWriteTestFile(t, filepath.Join(meta, name), value)
					}
					command = remoteDiscardSyncPendingMetadata(workdir, token, false)
				}
				out, err := runGitControlWithShellHook(t, hook, command)
				if err != nil || string(out) != want {
					t.Fatalf("Git metadata control output=%q want=%q err=%v", out, want, err)
				}
				if operation == "seed manifest" {
					wantManifest := "deleted.txt\x00modified.txt\x00other/omit.txt\x00src/keep.txt\x00tracked.txt\x00"
					if data, readErr := os.ReadFile(filepath.Join(meta, "sync-manifest")); readErr != nil || string(data) != wantManifest {
						t.Fatalf("seeded manifest=%q err=%v", data, readErr)
					}
				}
				for _, name := range discarded {
					if _, statErr := os.Stat(filepath.Join(meta, name)); !os.IsNotExist(statErr) {
						t.Fatalf("discarded pending metadata %s survived: %v", name, statErr)
					}
				}
				for name, value := range preserved {
					if data, readErr := os.ReadFile(filepath.Join(meta, name)); readErr != nil || string(data) != value {
						t.Fatalf("discard changed neighboring metadata %s: data=%q err=%v", name, data, readErr)
					}
				}
			})
		}
	}
}

func TestRemoteUserWorkloadPreservesLoginShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX workload shell")
	}
	for _, shell := range []bool{false, true} {
		t.Run(fmt.Sprintf("shell=%t", shell), func(t *testing.T) {
			home, workdir := t.TempDir(), t.TempDir()
			mustWriteTestFile(t, filepath.Join(home, ".bash_profile"), "export CRABBOX_TEST_LOGIN_VALUE=profile-loaded\n")
			const script = `printf '%s' "$CRABBOX_TEST_LOGIN_VALUE"`
			command := remoteCommand(workdir, nil, []string{"bash", "-c", script})
			if shell {
				command = remoteShellCommand(workdir, nil, script)
			}
			cmd := exec.Command("/bin/sh", "-c", command)
			cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "BASH_ENV=" + os.DevNull, "ENV=" + os.DevNull}
			if out, err := cmd.CombinedOutput(); err != nil || string(out) != "profile-loaded" {
				t.Fatalf("user workload lost login profile: output=%q err=%v", out, err)
			}
		})
	}
}

func TestRemoteGitSeedClassifiesRuntimeOriginFailures(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX Git seed integration")
	}
	for _, kind := range []string{"missing filesystem", "private HTTP", "HTTP transport", "non-auth HTTP", "missing branch"} {
		t.Run(kind, func(t *testing.T) {
			remote := filepath.Join(t.TempDir(), "missing.git")
			var server *httptest.Server
			switch kind {
			case "private HTTP", "non-auth HTTP":
				status := http.StatusUnauthorized
				if kind == "non-auth HTTP" {
					status = http.StatusInternalServerError
				}
				server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
					if request.Header.Get("Authorization") != "" {
						t.Errorf("seed forwarded an Authorization header")
					}
					if status == http.StatusUnauthorized {
						w.Header().Set("WWW-Authenticate", `Basic realm="private"`)
					}
					http.Error(w, http.StatusText(status), status)
				}))
				defer server.Close()
				remote = server.URL + "/repo.git"
			case "HTTP transport":
				remote = newGitTransportFailureHTTPServer(t) + "/repo.git"
			}
			workdir := filepath.Join(t.TempDir(), "work")
			plan := gitCoherencePlan{RemoteURL: remote, Target: strings.Repeat("a", 40), Tree: strings.Repeat("b", 40), Branch: "main"}
			if kind == "missing branch" {
				f := newGitCoherenceFixture(t)
				plan = f.plan(t, f.b)
				plan.Branch = "absent"
			}
			out, err := exec.Command("bash", "-lc", remoteGitSeed(workdir, plan)).CombinedOutput()
			reason, fallback := gitOriginRuntimeFallbackResult(plan.RemoteURL, string(out), err)
			if kind == "HTTP transport" && !strings.Contains(string(out), "Empty reply from server") {
				t.Fatalf("transport fixture did not close after the HTTP request: err=%v output=%q", err, out)
			}
			wantReason := "origin_unavailable"
			wantFallback := true
			if kind == "private HTTP" {
				wantReason = "origin_auth_required"
			} else if kind == "non-auth HTTP" || kind == "missing branch" {
				wantReason, wantFallback = "", false
			}
			if fallback != wantFallback || reason != wantReason || err == nil {
				t.Fatalf("fallback=%t reason=%q err=%v output=%q", fallback, reason, err, out)
			}
			if _, statErr := os.Stat(workdir); !os.IsNotExist(statErr) {
				t.Fatalf("failed seed retained workspace: %v", statErr)
			}
		})
	}
}

func TestRemoteFinalizeRuntimeOriginFallbackRetriesCommittedManifest(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX Git finalize integration")
	}
	for _, kind := range []string{"missing filesystem", "private HTTP", "HTTP transport", "non-auth HTTP", "missing branch", "tree verification"} {
		t.Run(kind, func(t *testing.T) {
			f := newGitCoherenceFixture(t)
			workdir := f.workspace(t, f.a, true)
			plan := f.plan(t, f.b)
			var server *httptest.Server
			switch kind {
			case "missing filesystem":
				if err := os.Rename(f.origin, f.origin+".missing"); err != nil {
					t.Fatal(err)
				}
			case "private HTTP", "non-auth HTTP":
				status := http.StatusUnauthorized
				if kind == "non-auth HTTP" {
					status = http.StatusInternalServerError
				}
				server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
					if request.Header.Get("Authorization") != "" {
						t.Errorf("finalize forwarded an Authorization header")
					}
					if status == http.StatusUnauthorized {
						w.Header().Set("WWW-Authenticate", `Basic realm="private"`)
					}
					http.Error(w, http.StatusText(status), status)
				}))
				defer server.Close()
				plan.RemoteURL = server.URL + "/repo.git"
			case "HTTP transport":
				plan.RemoteURL = newGitTransportFailureHTTPServer(t) + "/repo.git"
			case "missing branch":
				plan.Branch = "absent"
			case "tree verification":
				plan.Tree = strings.Repeat("f", 40)
			}
			const token = "1234567890abcdef1234567890abcdef"
			stageCoherenceFinalize(t, workdir, token)
			meta := coherenceMetaDir(t, workdir)
			mustWriteTestFile(t, filepath.Join(meta, "sync-fingerprint"), "stale")
			mustWriteTestFile(t, filepath.Join(meta, "git-hydrate-base"), "main stale\n")

			tools := t.TempDir()
			logPath := filepath.Join(tools, "ssh.log")
			ssh := `#!/bin/sh
remote=""
for arg do remote="$arg"; done
printf '%s\n---\n' "$remote" >> "$CRABBOX_FINALIZE_SSH_LOG"
exec /bin/bash --noprofile --norc -c "$remote"
`
			if err := os.WriteFile(filepath.Join(tools, "ssh"), []byte(ssh), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("CRABBOX_FINALIZE_SSH_LOG", logPath)
			out, err, reason, fallback := runRemoteFinalizeSync(context.Background(), SSHTarget{
				User: "crabbox", Host: "example.test", Port: "22", TargetOS: targetLinux, NoControlMaster: true,
			}, workdir, remoteSyncFinalizeOptions{
				HydrateGit: true, BaseRef: "main", BaseSHA: f.b, Fingerprint: "new", Token: token, Coherence: plan,
			})
			wantFallback := kind == "missing filesystem" || kind == "private HTTP" || kind == "HTTP transport"
			wantReason := ""
			if kind == "missing filesystem" || kind == "HTTP transport" {
				wantReason = "origin_unavailable"
			} else if kind == "private HTTP" {
				wantReason = "origin_auth_required"
			}
			if fallback != wantFallback || reason != wantReason {
				t.Fatalf("fallback=%t reason=%q err=%v output=%q", fallback, reason, err, out)
			}
			logData, readErr := os.ReadFile(logPath)
			if readErr != nil {
				t.Fatal(readErr)
			}
			commands := strings.Split(strings.TrimSuffix(string(logData), "---\n"), "---\n")
			wantAttempts := 1
			if wantFallback {
				wantAttempts = 2
			}
			if len(commands) != wantAttempts {
				t.Fatalf("finalize attempts=%d want %d:\n%s", len(commands), wantAttempts, logData)
			}
			for _, command := range commands {
				if !strings.Contains(command, token) {
					t.Fatalf("finalize retry changed token:\n%s", logData)
				}
			}
			if !wantFallback {
				if err == nil {
					t.Fatalf("non-origin failure succeeded: output=%q", out)
				}
				if kind == "missing branch" {
					diagnostic := "remote sync finalize failed: Git coherence fetch failed"
					raw := "fatal: couldn't find remote ref refs/heads/absent"
					diagnosticIndex := strings.Index(out, diagnostic)
					rawIndex := strings.Index(out, raw)
					if diagnosticIndex < 0 || rawIndex < 0 || diagnosticIndex > rawIndex {
						t.Fatalf("missing branch diagnostics out of order: %q", out)
					}
				}
				if kind == "non-auth HTTP" && !strings.Contains(out, "500") {
					t.Fatalf("fatal HTTP diagnostics were not retained: %q", out)
				}
				return
			}
			if err != nil {
				t.Fatalf("fallback finalize: %v\n%s", err, out)
			}
			if strings.Contains(out, "Authentication failed") ||
				strings.Contains(out, "Failed to connect") ||
				strings.Contains(out, "does not appear to be a git repository") {
				t.Fatalf("successful fallback retained first-attempt diagnostics: %q", out)
			}
			for _, name := range []string{"sync-finalize-token", "sync-finalize-complete-token"} {
				value, readErr := os.ReadFile(filepath.Join(meta, name))
				if readErr != nil || string(value) != token {
					t.Fatalf("%s=%q err=%v", name, value, readErr)
				}
			}
			for _, name := range []string{"sync-fingerprint", "git-hydrate-base"} {
				if _, statErr := os.Stat(filepath.Join(meta, name)); !os.IsNotExist(statErr) {
					t.Fatalf("%s survived fallback: %v", name, statErr)
				}
			}
			if _, statErr := os.Stat(filepath.Join(meta, "sync-manifest")); statErr != nil {
				t.Fatalf("committed manifest missing: %v", statErr)
			}
		})
	}
}

func TestGitSeedCommandsRejectCredentialBearingRemote(t *testing.T) {
	const secret = "do-not-forward"
	for remoteName, remote := range map[string]string{
		"https":     "https://runner:" + secret + "@example.test/repo.git",
		"ssh":       "ssh://runner:" + secret + "@example.test/repo.git",
		"git+https": "git+https://runner:" + secret + "@example.test/repo.git",
	} {
		t.Run(remoteName, func(t *testing.T) {
			for target, command := range map[string]string{
				"linux":   remoteGitSeed("/work/repo", gitCoherencePlan{RemoteURL: remote, Target: "abc123", Tree: "tree", Branch: "main"}),
				"windows": windowsGitSeed(`C:\crabbox\repo`, gitCoherencePlan{RemoteURL: remote, Target: "abc123", Tree: "tree", Branch: "main"}),
			} {
				t.Run(target, func(t *testing.T) {
					if strings.Contains(command, secret) || strings.Contains(command, "example.test") {
						t.Fatalf("credential-bearing remote reached %s seed command: %q", target, command)
					}
					if strings.Contains(command, "git clone") {
						t.Fatalf("%s seed command should be disabled: %q", target, command)
					}
				})
			}
		})
	}
}

func TestRemoteGitSeedLocalCanary(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, source, "init")
	runGit(t, source, "config", "user.email", "test@example.com")
	runGit(t, source, "config", "user.name", "Test")
	mustWriteTestFile(t, filepath.Join(source, "proof.txt"), "safe seed\n")
	runGit(t, source, "add", ".")
	runGit(t, source, "commit", "-m", "seed")
	head := gitOutput(source, "rev-parse", "HEAD")
	tree := gitOutput(source, "rev-parse", "HEAD^{tree}")
	branch := gitOutput(source, "branch", "--show-current")
	origin := filepath.Join(root, "origin.git")
	clone := exec.Command("git", "clone", "--bare", source, origin)
	if out, err := clone.CombinedOutput(); err != nil {
		t.Fatalf("create bare origin: %v\n%s", err, out)
	}
	plan := gitCoherencePlan{RemoteURL: origin, Target: head, Tree: tree, Branch: branch}
	runSeed := func(label, workdir string) {
		t.Helper()
		seed := exec.Command("bash", "-lc", remoteGitSeed(workdir, plan))
		if out, err := seed.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", label, err, out)
		}
		staging, err := filepath.Glob(filepath.Join(filepath.Dir(workdir), ".seed.*"))
		if err != nil {
			t.Fatal(err)
		}
		if len(staging) != 0 {
			t.Fatalf("%s left seed staging files: %v", label, staging)
		}
	}
	requireSeeded := func(workdir string) {
		t.Helper()
		if got := gitOutput(workdir, "remote", "get-url", "origin"); got != origin {
			t.Fatalf("seeded origin=%q want %q", got, origin)
		}
		if got := gitOutput(workdir, "rev-parse", "HEAD"); got != head {
			t.Fatalf("seeded HEAD=%q want %q", got, head)
		}
		if got := gitOutput(workdir, "write-tree"); got != tree {
			t.Fatalf("seeded tree=%q want %q", got, tree)
		}
		index := gitOutput(workdir, "rev-parse", "--git-path", "index")
		if !filepath.IsAbs(index) {
			index = filepath.Join(workdir, index)
		}
		if _, err := os.Stat(index); err != nil {
			t.Fatalf("seeded index missing: %v", err)
		}
	}

	workdir := filepath.Join(root, "safe-workdir")
	runSeed("run safe seed", workdir)
	requireSeeded(workdir)
	proof, err := os.ReadFile(filepath.Join(workdir, "proof.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(proof)); got != "safe seed" {
		t.Fatalf("seeded proof=%q", got)
	}
	preserved := filepath.Join(workdir, "local-overlay.txt")
	mustWriteTestFile(t, preserved, "preserve\n")
	runGit(t, workdir, "remote", "set-url", "origin", filepath.Join(root, "wrong.git"))
	runSeed("reuse valid workspace", workdir)
	requireSeeded(workdir)
	if got, err := os.ReadFile(preserved); err != nil || string(got) != "preserve\n" {
		t.Fatalf("valid reusable workspace was replaced: data=%q err=%v", got, err)
	}

	unbornWorkdir := filepath.Join(root, "unborn-workdir")
	if err := os.Mkdir(unbornWorkdir, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, unbornWorkdir, "init")
	mustWriteTestFile(t, filepath.Join(unbornWorkdir, "stale-unborn.txt"), "stale\n")
	runSeed("replace unborn workspace", unbornWorkdir)
	requireSeeded(unbornWorkdir)
	if _, err := os.Stat(filepath.Join(unbornWorkdir, "stale-unborn.txt")); !os.IsNotExist(err) {
		t.Fatalf("unborn workspace was reused instead of reseeded: %v", err)
	}

	missingIndexWorkdir := filepath.Join(root, "missing-index-workdir")
	if out, err := exec.Command("git", "clone", "--quiet", origin, missingIndexWorkdir).CombinedOutput(); err != nil {
		t.Fatalf("clone missing-index fixture: %v\n%s", err, out)
	}
	missingIndex := gitOutput(missingIndexWorkdir, "rev-parse", "--git-path", "index")
	if !filepath.IsAbs(missingIndex) {
		missingIndex = filepath.Join(missingIndexWorkdir, missingIndex)
	}
	if err := os.Remove(missingIndex); err != nil {
		t.Fatal(err)
	}
	mustWriteTestFile(t, filepath.Join(missingIndexWorkdir, "stale-missing-index.txt"), "stale\n")
	runSeed("replace missing-index workspace", missingIndexWorkdir)
	requireSeeded(missingIndexWorkdir)
	if _, err := os.Stat(filepath.Join(missingIndexWorkdir, "stale-missing-index.txt")); !os.IsNotExist(err) {
		t.Fatalf("missing-index workspace was reused instead of reseeded: %v", err)
	}

	nestedWorkdir := filepath.Join(source, "nested-workdir")
	if err := os.Mkdir(nestedWorkdir, 0o755); err != nil {
		t.Fatal(err)
	}
	runSeed("seed inside ancestor repository", nestedWorkdir)
	wantNestedRoot, err := filepath.EvalSymlinks(nestedWorkdir)
	if err != nil {
		t.Fatal(err)
	}
	if got := gitOutput(nestedWorkdir, "rev-parse", "--show-toplevel"); got != wantNestedRoot {
		t.Fatalf("ancestor repository counted as nested seed root: got %q want %q", got, wantNestedRoot)
	}

	renderGitSeed := remoteGitSeed
	remoteGitSeed := func(workdir, remoteURL, target string) string {
		return renderGitSeed(workdir, gitCoherencePlan{RemoteURL: remoteURL, Target: target, Tree: tree, Branch: branch})
	}
	blockedWorkdir := filepath.Join(root, "blocked-workdir")
	blocked := exec.Command("bash", "-lc", remoteGitSeed(blockedWorkdir, "https://runner:do-not-forward@example.test/repo.git", head))
	if out, err := blocked.CombinedOutput(); err != nil {
		t.Fatalf("run blocked seed: %v\n%s", err, out)
	}
	if _, err := os.Stat(blockedWorkdir); !os.IsNotExist(err) {
		t.Fatalf("credential-blocked seed created workdir: %v", err)
	}
}

func TestRemoteGitSeedSupportsSeedOnlyPlanWithoutTree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell Git seed fixture")
	}
	f := newGitCoherenceFixture(t)
	plan := gitCoherencePlan{
		RemoteURL: f.origin,
		Target:    f.b,
		Branch:    "main",
	}
	if !plan.seedEnabled() || plan.enabled() {
		t.Fatalf("expected seed-only plan, got %#v", plan)
	}
	workdir := filepath.Join(t.TempDir(), "seed-only")
	if out, err := exec.Command("bash", "-lc", remoteGitSeed(workdir, plan)).CombinedOutput(); err != nil {
		t.Fatalf("seed-only Git seed failed: %v\n%s", err, out)
	}
	requireGitOutput(t, workdir, f.b, "rev-parse", "HEAD")
	if tree := gitOutput(workdir, "write-tree"); tree == "" {
		t.Fatal("seed-only Git seed produced no usable index tree")
	}
	index := gitOutput(workdir, "rev-parse", "--git-path", "index")
	if !filepath.IsAbs(index) {
		index = filepath.Join(workdir, index)
	}
	if _, err := os.Stat(index); err != nil {
		t.Fatalf("seed-only Git seed index missing: %v", err)
	}
}

func TestRemoteGitHydrateStatusUsesMarkerAndRemoteBase(t *testing.T) {
	got := remoteGitHydrateStatus("/work/repo", "main", "abc123")
	for _, want := range []string{
		"git-hydrate-base",
		"marker base current",
		"remote base current",
		"remote base contains local",
		"merge-base --is-ancestor",
		"refs/remotes/origin/main",
		"abc123",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("remoteGitHydrateStatus missing %q in %q", want, got)
		}
	}
}

func TestRemoteWriteSyncManifestNew(t *testing.T) {
	got := remoteWriteSyncManifestNew("/work/repo")
	if !strings.Contains(got, "cat > \"$meta_dir/sync-manifest.new\"") {
		t.Fatalf("unexpected manifest write command: %q", got)
	}
}

func TestRemoteWriteSyncDeletedNew(t *testing.T) {
	got := remoteWriteSyncDeletedNew("/work/repo")
	if !strings.Contains(got, "cat > \"$meta_dir/sync-deleted.new\"") {
		t.Fatalf("unexpected deleted manifest write command: %q", got)
	}
}

func TestRemoteWriteSyncManifestsNew(t *testing.T) {
	const finalizeToken = "0123456789abcdef0123456789abcdef"
	workdir := t.TempDir()
	manifest := "keep.txt\x00binary\xff\x00"
	deleted := "old.txt\x00gone\xfe\x00"
	input := syncManifestInputForTarget(SSHTarget{TargetOS: targetLinux}, []byte(manifest), []byte(deleted))
	cmd := exec.Command("bash", "-lc", remoteWriteSyncManifestsNew(workdir, finalizeToken))
	cmd.Stdin = strings.NewReader(input)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("write manifests failed: %v\n%s", err, out)
	}
	metaDir := filepath.Join(workdir, ".crabbox")
	gotManifest, err := os.ReadFile(filepath.Join(metaDir, remoteSyncPendingManifestName(finalizeToken)))
	if err != nil {
		t.Fatal(err)
	}
	if string(gotManifest) != manifest {
		t.Fatalf("unexpected manifest: %q", gotManifest)
	}
	gotDeleted, err := os.ReadFile(filepath.Join(metaDir, remoteSyncPendingDeletedName(finalizeToken)))
	if err != nil {
		t.Fatal(err)
	}
	if string(gotDeleted) != deleted {
		t.Fatalf("unexpected deleted manifest: %q", gotDeleted)
	}
}

func TestRemoteWriteSyncManifestsNewCollectsOnlyOldTokenArtifacts(t *testing.T) {
	got := remoteWriteSyncManifestsNew("/work/repo", "0123456789abcdef0123456789abcdef")
	for _, want := range []string{"find \"$meta_dir\"", "-mtime +7", "sync-manifest.*.new", "sync-finalize-complete-token.tmp.*"} {
		if !strings.Contains(got, want) {
			t.Fatalf("manifest writer missing abandoned metadata cleanup %q: %s", want, got)
		}
	}
}

func TestRemoteWriteSyncManifestsNewForTargetUsesInterpretedWriterForWSL2(t *testing.T) {
	got := remoteWriteSyncManifestsNewForTarget(SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeWSL2}, "/work/repo", "0123456789abcdef0123456789abcdef")
	if !strings.Contains(got, "python3 -c") {
		t.Fatalf("WSL2 manifest writer should use Python exact reads: %q", got)
	}
	if strings.Contains(got, "dd bs=1") {
		t.Fatalf("WSL2 manifest writer should avoid byte-at-a-time dd: %q", got)
	}
	if strings.Contains(got, "perl -e") {
		t.Fatalf("WSL2 manifest writer should keep the command short and avoid fallback scripts: %q", got)
	}

	plain := remoteWriteSyncManifestsNewForTarget(SSHTarget{TargetOS: targetLinux}, "/work/repo", "0123456789abcdef0123456789abcdef")
	if !strings.Contains(plain, "dd bs=1") {
		t.Fatalf("non-WSL2 manifest writer should keep portable dd fallback: %q", plain)
	}
	if strings.Contains(plain, "status=none") {
		t.Fatalf("non-WSL2 manifest writer should not require GNU dd extensions: %q", plain)
	}
	if strings.Count(plain, "dd bs=1 count=\"") != 2 {
		t.Fatalf("non-WSL2 manifest writer should exact-read both blobs: %q", plain)
	}
	if strings.Contains(plain, "cat >") {
		t.Fatalf("non-WSL2 manifest writer should not read either blob through EOF: %q", plain)
	}
	for _, want := range []string{"IFS= read -r manifest_len", "IFS= read -r deleted_len", "wc -c", "short sync manifest", "short sync deleted manifest"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("non-WSL2 manifest writer missing %q: %q", want, plain)
		}
	}
}

func TestRemoteWriteSyncManifestsNewForTargetPlainWSL2IsHermetic(t *testing.T) {
	const token = "0123456789abcdef0123456789abcdef"
	target := SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeWSL2}
	if got, want := remoteWriteSyncManifestsNewForTargetMode(target, "/work/repo", token, false), remoteWriteSyncManifestsNewPython("/work/repo", token); got != want {
		t.Fatal("ordinary WSL2 manifest command bytes changed")
	}
	got := remoteWriteSyncManifestsNewForTargetMode(target, "/work/repo", token, true)
	for _, want := range []string{
		"/usr/bin/env -i",
		"BASH_ENV=/dev/null",
		"ENV=/dev/null",
		"/bin/bash --noprofile --norc -c",
		"/usr/bin/python3 -c",
		"/bin/mkdir -p --",
		"/usr/bin/find",
		"-exec /bin/rm",
		"plain_git",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("plain WSL2 writer missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "bash -lc") {
		t.Fatalf("plain WSL2 writer uses a login shell:\n%s", got)
	}
}

func TestPlainManifestMetadataIgnoresHostileEnvironment(t *testing.T) {
	const token = "0123456789abcdef0123456789abcdef"
	workdir := filepath.Join(t.TempDir(), "repo")
	marker := filepath.Join(t.TempDir(), "executed")
	profile := filepath.Join(t.TempDir(), "profile")
	mustWriteTestFile(t, profile, "printf hostile >"+shellQuote(marker)+"\n")
	hostileBin := t.TempDir()
	for _, name := range []string{"bash", "git", "python3", "mkdir", "find", "rm"} {
		writeExecutable(t, filepath.Join(hostileBin, name), "#!/bin/sh\nprintf hostile >"+shellQuote(marker)+"\nexit 97\n")
	}
	manifest := []byte("tracked.txt\x00")
	deleted := []byte("removed.txt\x00")
	command := exec.Command("/bin/sh", "-c", remoteWriteSyncManifestsNewForTargetMode(
		SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeWSL2},
		workdir,
		token,
		true,
	))
	command.Env = []string{"PATH=" + hostileBin, "BASH_ENV=" + profile, "ENV=" + profile}
	command.Stdin = strings.NewReader(syncManifestInputForTarget(
		SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeWSL2},
		manifest,
		deleted,
	))
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("plain WSL2 writer failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("hostile environment executed: %v", err)
	}
	metaDir := filepath.Join(workdir, ".crabbox")
	if got, err := os.ReadFile(filepath.Join(metaDir, remoteSyncPendingManifestName(token))); err != nil || !bytes.Equal(got, manifest) {
		t.Fatalf("manifest=%q err=%v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(metaDir, remoteSyncPendingDeletedName(token))); err != nil || !bytes.Equal(got, deleted) {
		t.Fatalf("deleted=%q err=%v", got, err)
	}
}

func TestSyncManifestInputForTargetFramesBothLengths(t *testing.T) {
	manifest := []byte("keep.txt\x00")
	deleted := []byte("old.txt\x00")
	got := syncManifestInputForTarget(SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeWSL2}, manifest, deleted)
	manifestEncoded := base64.StdEncoding.EncodeToString(manifest)
	deletedEncoded := base64.StdEncoding.EncodeToString(deleted)
	want := fmt.Sprintf("%d\n%d\n", len(manifestEncoded), len(deletedEncoded)) + manifestEncoded + deletedEncoded
	if got != want {
		t.Fatalf("WSL2 manifest input = %q, want %q", got, want)
	}

	plain := syncManifestInputForTarget(SSHTarget{TargetOS: targetLinux}, manifest, deleted)
	plainWant := fmt.Sprintf("%d\n%d\n", len(manifest), len(deleted)) + string(manifest) + string(deleted)
	if plain != plainWant {
		t.Fatalf("plain manifest input = %q, want %q", plain, plainWant)
	}
}

func TestSyncManifestInputForTargetFramesEmptyDeletedData(t *testing.T) {
	manifest := []byte("keep.txt\x00")
	got := syncManifestInputForTarget(SSHTarget{TargetOS: targetLinux}, manifest, nil)
	want := fmt.Sprintf("%d\n0\n", len(manifest)) + string(manifest)
	if got != want {
		t.Fatalf("plain manifest input = %q, want %q", got, want)
	}
}

func TestRemoteWriteSyncManifestsNewRejectsMalformedLengths(t *testing.T) {
	for _, test := range []struct {
		name  string
		input string
		want  string
	}{
		{name: "missing manifest length", input: "", want: "invalid sync manifest length"},
		{name: "non-decimal manifest length", input: "x\n0\n", want: "invalid sync manifest length"},
		{name: "non-canonical manifest length", input: "01\n0\n", want: "invalid sync manifest length"},
		{name: "missing deleted length", input: "0\n", want: "invalid sync deleted length"},
		{name: "non-decimal deleted length", input: "0\nx\n", want: "invalid sync deleted length"},
		{name: "non-canonical deleted length", input: "0\n01\n", want: "invalid sync deleted length"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cmd := exec.Command("bash", "-lc", remoteWriteSyncManifestsNew(t.TempDir(), "0123456789abcdef0123456789abcdef"))
			cmd.Stdin = strings.NewReader(test.input)
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("malformed frame unexpectedly succeeded: %q", out)
			}
			if !strings.Contains(string(out), test.want) {
				t.Fatalf("output=%q, want %q", out, test.want)
			}
		})
	}
}

func TestRemoteWriteSyncManifestsNewRejectsShortInput(t *testing.T) {
	for _, test := range []struct {
		name  string
		input string
		want  string
	}{
		{name: "manifest", input: "2\n0\na", want: "short sync manifest: got 1 want 2"},
		{name: "deleted", input: "1\n2\nab", want: "short sync deleted manifest: got 1 want 2"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cmd := exec.Command("bash", "-lc", remoteWriteSyncManifestsNew(t.TempDir(), "0123456789abcdef0123456789abcdef"))
			cmd.Stdin = strings.NewReader(test.input)
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("short frame unexpectedly succeeded: %q", out)
			}
			if !strings.Contains(string(out), test.want) {
				t.Fatalf("output=%q, want %q", out, test.want)
			}
		})
	}
}

func TestRemoteWriteSyncManifestsNewDoesNotWaitForEOF(t *testing.T) {
	for _, test := range []struct {
		name    string
		deleted []byte
	}{
		{name: "empty deleted data"},
		{name: "non-empty deleted data", deleted: []byte("old.txt\x00")},
	} {
		t.Run(test.name, func(t *testing.T) {
			const finalizeToken = "0123456789abcdef0123456789abcdef"
			workdir := t.TempDir()
			manifest := []byte("keep.txt\x00")
			cmd := exec.Command("bash", "-lc", remoteWriteSyncManifestsNew(workdir, finalizeToken))
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			if _, err := io.WriteString(stdin, syncManifestInputForTarget(SSHTarget{TargetOS: targetLinux}, manifest, test.deleted)); err != nil {
				t.Fatal(err)
			}

			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("write manifests before EOF failed: %v", err)
				}
			case <-time.After(2 * time.Second):
				_ = stdin.Close()
				_ = cmd.Process.Kill()
				<-done
				t.Fatal("manifest writer waited for transport EOF after a complete deleted frame")
			}
			_ = stdin.Close()

			metaDir := filepath.Join(workdir, ".crabbox")
			gotManifest, err := os.ReadFile(filepath.Join(metaDir, remoteSyncPendingManifestName(finalizeToken)))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(gotManifest, manifest) {
				t.Fatalf("manifest=%q, want %q", gotManifest, manifest)
			}
			gotDeleted, err := os.ReadFile(filepath.Join(metaDir, remoteSyncPendingDeletedName(finalizeToken)))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(gotDeleted, test.deleted) {
				t.Fatalf("deleted manifest=%q, want %q", gotDeleted, test.deleted)
			}
		})
	}
}

func TestRemoteWriteSyncManifestsNewPython(t *testing.T) {
	const finalizeToken = "0123456789abcdef0123456789abcdef"
	workdir := t.TempDir()
	manifest := strings.Repeat("manifest-entry\x00", 4096)
	deleted := "old.txt\x00"
	input := syncManifestInputForTarget(SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeWSL2}, []byte(manifest), []byte(deleted))
	cmd := exec.Command("bash", "-lc", remoteWriteSyncManifestsNewPython(workdir, finalizeToken))
	cmd.Stdin = strings.NewReader(input)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("write interpreted manifests failed: %v\n%s", err, out)
	}
	metaDir := filepath.Join(workdir, ".crabbox")
	gotManifest, err := os.ReadFile(filepath.Join(metaDir, remoteSyncPendingManifestName(finalizeToken)))
	if err != nil {
		t.Fatal(err)
	}
	if string(gotManifest) != manifest {
		t.Fatalf("manifest bytes=%d want %d", len(gotManifest), len(manifest))
	}
	gotDeleted, err := os.ReadFile(filepath.Join(metaDir, remoteSyncPendingDeletedName(finalizeToken)))
	if err != nil {
		t.Fatal(err)
	}
	if string(gotDeleted) != deleted {
		t.Fatalf("unexpected deleted manifest: %q", gotDeleted)
	}
}

func TestRemoteWriteSyncManifestsNewReadsChunkedInput(t *testing.T) {
	const finalizeToken = "0123456789abcdef0123456789abcdef"
	workdir := t.TempDir()
	manifest := strings.Repeat("manifest-entry\x00", 4096)
	deleted := "old.txt\x00"

	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bashPath, "--noprofile", "--norc", "-c", remoteWriteSyncManifestsNew(workdir, finalizeToken))
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	frame := syncManifestInputForTarget(SSHTarget{TargetOS: targetLinux}, []byte(manifest), []byte(deleted))
	headerLen := strings.Index(frame, "\n") + 1
	headerLen += strings.Index(frame[headerLen:], "\n") + 1
	if _, err := io.WriteString(stdin, frame[:headerLen+1]); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if _, err := io.WriteString(stdin, frame[headerLen+1:]); err != nil {
		t.Fatal(err)
	}
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("write chunked manifests failed: %v\n%s", err, output.String())
	}
	metaDir := filepath.Join(workdir, ".crabbox")
	gotManifest, err := os.ReadFile(filepath.Join(metaDir, remoteSyncPendingManifestName(finalizeToken)))
	if err != nil {
		t.Fatal(err)
	}
	if string(gotManifest) != manifest {
		t.Fatalf("manifest bytes=%d want %d", len(gotManifest), len(manifest))
	}
	gotDeleted, err := os.ReadFile(filepath.Join(metaDir, remoteSyncPendingDeletedName(finalizeToken)))
	if err != nil {
		t.Fatal(err)
	}
	if string(gotDeleted) != deleted {
		t.Fatalf("unexpected deleted manifest: %q", gotDeleted)
	}
}

func mustWriteTestBashNoProfileWrapper(t *testing.T, dir string) {
	t.Helper()
	target, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "bash")
	script := "#!/bin/sh\n" +
		`if [ "$1" = "-lc" ]; then` + "\n" +
		"  shift\n" +
		"  exec " + shellQuote(target) + ` --noprofile --norc -c "$@"` + "\n" +
		"fi\n" +
		"exec " + shellQuote(target) + ` "$@"` + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWriteTestCommandWrapperWithMarker(t *testing.T, dir, name, marker string) {
	t.Helper()
	target, err := exec.LookPath(name)
	if err != nil {
		t.Fatalf("lookpath %s: %v", name, err)
	}
	path := filepath.Join(dir, name)
	script := "#!/bin/sh\n: > " + shellQuote(marker) + "\nexec " + shellQuote(target) + ` "$@"` + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWriteTestFailingCommand(t *testing.T, dir, name string, code int) {
	t.Helper()
	path := filepath.Join(dir, name)
	script := fmt.Sprintf("#!/bin/sh\nexit %d\n", code)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestRemoteSyncMetadataUsesGitDirForGitWorktree(t *testing.T) {
	workdir := t.TempDir()
	runGit(t, workdir, "init")
	cmd := exec.Command("bash", "-lc", remoteWriteSyncManifestNew(workdir))
	cmd.Stdin = strings.NewReader("tracked.txt\x00")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("write manifest failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(workdir, ".git", "crabbox", "sync-manifest.new")); err != nil {
		t.Fatalf("manifest should be written under .git/crabbox: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workdir, ".crabbox")); !os.IsNotExist(err) {
		t.Fatalf("worktree .crabbox should not be created, stat err=%v", err)
	}
}

func TestIsBootstrapWaitError(t *testing.T) {
	if !isBootstrapWaitError(exit(5, "timed out waiting for SSH on 203.0.113.10 during bootstrap")) {
		t.Fatal("expected SSH timeout to be retryable")
	}
	if !isBootstrapWaitError(exit(5, "timed out waiting for XCP-ng guest IPv4")) {
		t.Fatal("expected XCP-ng guest IPv4 timeout to be retryable")
	}
	if isBootstrapWaitError(exit(6, "rsync failed")) {
		t.Fatal("sync failure must not be treated as retryable bootstrap")
	}
}

func TestAcquireAttemptsRetriesWarmupBootstrapFailures(t *testing.T) {
	if got := acquireAttempts(true); got != 2 {
		t.Fatalf("warmup keep=true attempts=%d want 2", got)
	}
	if got := acquireAttempts(false); got != 2 {
		t.Fatalf("one-shot attempts=%d want 2", got)
	}
}

func TestAcquireAttemptsDoesNotRetryUnconfirmedCoordinatorStaleInstanceFailures(t *testing.T) {
	var stderr strings.Builder
	attempts := 0
	_, err := acquireAttemptsRetry(Runtime{Stderr: &stderr}, false, func() (LeaseTarget, error) {
		attempts++
		return LeaseTarget{}, CoordinatorHTTPError{
			Method:     "POST",
			Path:       "/v1/leases",
			StatusCode: 500,
			Message:    `{"error":"InvalidInstanceID.NotFound"}`,
		}
	})
	if err == nil {
		t.Fatal("expected stale instance error")
	}
	if attempts != 1 {
		t.Fatalf("attempts=%d want 1", attempts)
	}
	if strings.Contains(stderr.String(), "retrying with fresh lease") {
		t.Fatalf("unexpected retry warning: %q", stderr.String())
	}
}

func TestAcquireAttemptsRetriesCleanedCoordinatorStaleInstanceFailures(t *testing.T) {
	var stderr strings.Builder
	attempts := 0
	lease, err := acquireAttemptsRetry(Runtime{Stderr: &stderr}, false, func() (LeaseTarget, error) {
		attempts++
		if attempts == 1 {
			err := CoordinatorHTTPError{
				Method:     "POST",
				Path:       "/v1/leases",
				StatusCode: 500,
				Message:    `{"error":"InvalidInstanceID.NotFound"}`,
			}
			return LeaseTarget{}, coordinatorStaleInstanceCleanedError{err: err}
		}
		return LeaseTarget{LeaseID: "cbx_ok"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 2 || lease.LeaseID != "cbx_ok" {
		t.Fatalf("attempts=%d lease=%#v", attempts, lease)
	}
	if !strings.Contains(stderr.String(), "coordinator returned stale instance") {
		t.Fatalf("missing stale retry warning: %q", stderr.String())
	}
}

func TestAcquireAttemptsRetriesRepeatedCleanedCoordinatorStaleInstanceFailures(t *testing.T) {
	var stderr strings.Builder
	attempts := 0
	lease, err := acquireAttemptsRetry(Runtime{Stderr: &stderr}, false, func() (LeaseTarget, error) {
		attempts++
		if attempts < 5 {
			err := CoordinatorHTTPError{
				Method:     "POST",
				Path:       "/v1/leases",
				StatusCode: 500,
				Message:    `{"error":"InvalidInstanceID.NotFound"}`,
			}
			return LeaseTarget{}, coordinatorStaleInstanceCleanedError{err: err}
		}
		return LeaseTarget{LeaseID: "cbx_ok"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 5 || lease.LeaseID != "cbx_ok" {
		t.Fatalf("attempts=%d lease=%#v", attempts, lease)
	}
	if got := strings.Count(stderr.String(), "coordinator returned stale instance"); got != 4 {
		t.Fatalf("stale retry warnings=%d want 4: %q", got, stderr.String())
	}
}

func TestBootstrapWaitTimeoutExtendsForDesktopBrowser(t *testing.T) {
	if got := bootstrapWaitTimeout(Config{}); got != 20*time.Minute {
		t.Fatalf("plain bootstrap timeout=%s want 20m", got)
	}
	if got := bootstrapWaitTimeout(Config{Desktop: true}); got != 45*time.Minute {
		t.Fatalf("desktop bootstrap timeout=%s want 45m", got)
	}
	if got := bootstrapWaitTimeout(Config{Browser: true}); got != 45*time.Minute {
		t.Fatalf("browser bootstrap timeout=%s want 45m", got)
	}
}

func TestServerProviderKeyUsesOnlyCrabboxLeaseKeys(t *testing.T) {
	server := Server{Labels: map[string]string{"lease": "cbx_123456abcdef"}}
	if got := serverProviderKey(server); got != "crabbox-cbx-123456abcdef" {
		t.Fatalf("serverProviderKey()=%q", got)
	}
	if !validCrabboxProviderKey("crabbox-cbx-123456abcdef") {
		t.Fatal("expected per-lease provider key to be valid")
	}
	if validCrabboxProviderKey("crabbox-steipete") {
		t.Fatal("shared key must not be treated as per-lease cleanup key")
	}
}

func TestMoveStoredTestboxKeyHandlesCoordinatorRenamedLease(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	oldPath, err := testboxKeyPath("cbx_111111111111")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(oldPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oldPath, []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oldPath+".pub", []byte("pub"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := moveStoredTestboxKey("cbx_111111111111", "cbx_222222222222"); err != nil {
		t.Fatal(err)
	}
	newPath, err := testboxKeyPath("cbx_222222222222")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(newPath); err != nil {
		t.Fatalf("moved key missing: %v", err)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("old key still exists or unexpected stat error: %v", err)
	}
}

func mustWriteTestFile(t *testing.T, path, value string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustWriteTestCommandWrapper(t *testing.T, dir, name string) {
	t.Helper()
	target, err := exec.LookPath(name)
	if err != nil {
		t.Fatalf("lookpath %s: %v", name, err)
	}
	path := filepath.Join(dir, name)
	script := "#!/bin/sh\nexec " + shellQuote(target) + ` "$@"` + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestAWSServerTypeForClass(t *testing.T) {
	tests := map[string]string{
		"standard":     "c7a.8xlarge",
		"fast":         "c7a.16xlarge",
		"large":        "c7a.24xlarge",
		"beast":        "c7a.48xlarge",
		"c8a.24xlarge": "c8a.24xlarge",
	}
	for in, want := range tests {
		if got := serverTypeForProviderClass("aws", in); got != want {
			t.Fatalf("serverTypeForProviderClass(%q)=%q want %q", in, got, want)
		}
	}
}

func TestAWSARM64ServerTypeForConfig(t *testing.T) {
	cfg := Config{
		Provider:             "aws",
		TargetOS:             targetLinux,
		Architecture:         ArchitectureARM64,
		architectureExplicit: true,
		Class:                "beast",
	}
	if got := serverTypeForConfig(cfg); got != "c7g.16xlarge" {
		t.Fatalf("serverTypeForConfig arm64=%q", got)
	}
}

func TestAWSExplicitARM64TypeInference(t *testing.T) {
	tests := map[string]string{
		"a1.large":        ArchitectureARM64,
		"c7g.16xlarge":    ArchitectureARM64,
		"c7gd.16xlarge":   ArchitectureARM64,
		"c7gn.16xlarge":   ArchitectureARM64,
		"g5g.xlarge":      ArchitectureARM64,
		"hpc7g.16xlarge":  ArchitectureARM64,
		"im4gn.16xlarge":  ArchitectureARM64,
		"is4gen.16xlarge": ArchitectureARM64,
		"c7a.16xlarge":    ArchitectureAMD64,
		"g5.xlarge":       ArchitectureAMD64,
	}
	for serverType, want := range tests {
		t.Run(serverType, func(t *testing.T) {
			cfg := Config{
				Provider:     "aws",
				TargetOS:     targetLinux,
				Architecture: ArchitectureAMD64,
				ServerType:   serverType,
			}
			if got := effectiveArchitectureForConfig(cfg); got != want {
				t.Fatalf("effectiveArchitectureForConfig(%q)=%q want %q", serverType, got, want)
			}
		})
	}
}

func TestCloudflareContainerInstanceTypeMapping(t *testing.T) {
	tests := []struct {
		class string
		want  string
	}{
		{class: "", want: "standard-4"},
		{class: "tiny", want: "standard-4"},
		{class: "small", want: "standard-4"},
		{class: "standard", want: "standard-4"},
		{class: "fast", want: "standard-4"},
		{class: "large", want: "standard-4"},
		{class: "beast", want: "standard-4"},
		{class: "lite", want: "lite"},
		{class: "basic", want: "basic"},
		{class: "standard-3", want: "standard-3"},
	}
	for _, tt := range tests {
		if got := cloudflareContainerInstanceTypeForClass(tt.class); got != tt.want {
			t.Fatalf("cloudflareContainerInstanceTypeForClass(%q)=%q want %q", tt.class, got, tt.want)
		}
		if got := CloudflareContainerInstanceTypeForClass(tt.class); got != tt.want {
			t.Fatalf("CloudflareContainerInstanceTypeForClass(%q)=%q want %q", tt.class, got, tt.want)
		}
	}
}

func TestNormalizeCloudflareContainerInstanceType(t *testing.T) {
	for _, valid := range CloudflareContainerInstanceTypes() {
		got, ok := NormalizeCloudflareContainerInstanceType(" " + strings.ToUpper(valid) + " ")
		if !ok || got != valid {
			t.Fatalf("NormalizeCloudflareContainerInstanceType(%q)=(%q,%t), want (%q,true)", valid, got, ok, valid)
		}
	}
	if got, ok := NormalizeCloudflareContainerInstanceType("ccx63"); ok || got != "" {
		t.Fatalf("NormalizeCloudflareContainerInstanceType(ccx63)=(%q,%t), want empty,false", got, ok)
	}
}

func TestCloudflareServerTypeForConfig(t *testing.T) {
	tests := []struct {
		cfg  Config
		want string
	}{
		{cfg: Config{Provider: "cloudflare", Class: "standard"}, want: "standard-4"},
		{cfg: Config{Provider: "cf", Class: "large"}, want: "standard-4"},
	}
	for _, tt := range tests {
		if got := serverTypeForConfig(tt.cfg); got != tt.want {
			t.Fatalf("serverTypeForConfig(%+v)=%q want %q", tt.cfg, got, tt.want)
		}
	}
}

func TestServerTypeForProviderClassDirectProviders(t *testing.T) {
	tests := []struct {
		provider string
		class    string
		want     string
	}{
		{provider: "blacksmith-testbox", class: "beast", want: ""},
		{provider: "ssh", class: "beast", want: ""},
		{provider: "islo", class: "beast", want: ""},
		{provider: "e2b", class: "beast", want: "base"},
		{provider: "modal", class: "beast", want: "python:3.13-slim"},
		{provider: "daytona", class: "beast", want: "snapshot"},
		{provider: "namespace", class: "standard", want: "S"},
		{provider: "namespace-devbox", class: " custom-xl ", want: "CUSTOM-XL"},
		{provider: "proxmox", class: "beast", want: "template"},
		{provider: "sprites", class: "beast", want: ""},
		{provider: "cloudflare", class: "standard", want: "standard-4"},
		{provider: "cf", class: "beast", want: "standard-4"},
		{provider: "azure", class: "standard", want: "Standard_D32ads_v6"},
		{provider: "google", class: "standard", want: "c4-standard-32"},
		{provider: "google-cloud", class: "standard", want: "c4-standard-32"},
		{provider: "hetzner", class: "fast", want: "ccx43"},
	}
	for _, tt := range tests {
		if got := serverTypeForProviderClass(tt.provider, tt.class); got != tt.want {
			t.Fatalf("serverTypeForProviderClass(%q, %q)=%q want %q", tt.provider, tt.class, got, tt.want)
		}
	}
}

func TestUnmappedProvidersDoNotInheritHetznerClassTypes(t *testing.T) {
	for _, provider := range []string{"agent-sandbox", "nebius"} {
		for _, class := range []string{"standard", "beast", "FAST", " custom-type "} {
			cfg := Config{Provider: provider, TargetOS: targetLinux, Architecture: ArchitectureAMD64, Class: class}
			if got := serverTypeForConfig(cfg); got != "" {
				t.Errorf("provider=%s class=%q serverTypeForConfig=%q want empty", provider, class, got)
			}
			if got := serverTypeForProviderClass(provider, class); got != "" {
				t.Errorf("provider=%s class=%q serverTypeForProviderClass=%q want empty", provider, class, got)
			}
		}
	}
}

func TestMappedProviderCandidatesPreserveNonCanonicalClassLiterals(t *testing.T) {
	for _, class := range []string{"FAST", " fast ", "custom-shape"} {
		for provider, got := range map[string][]string{
			"aws":     awsInstanceTypeCandidatesForClass(class),
			"azure":   azureVMSizeCandidatesForClass(class),
			"gcp":     gcpMachineTypeCandidatesForClass(class),
			"hetzner": serverTypeCandidatesForClass(class),
		} {
			if !reflect.DeepEqual(got, []string{class}) {
				t.Errorf("provider=%s class=%q candidates=%v want literal", provider, class, got)
			}
		}
		if got := awsLaunchCandidates(Config{Provider: "aws", TargetOS: targetLinux, Architecture: ArchitectureAMD64, Class: class}); !reflect.DeepEqual(got, []string{class, "t3.small"}) {
			t.Errorf("AWS class=%q launch candidates=%v", class, got)
		}
	}
}

func TestAWSLaunchCandidatesAddsPolicyFallbackUnlessExact(t *testing.T) {
	got := awsLaunchCandidates(Config{Provider: "aws", Class: "beast", ServerType: "c7a.48xlarge"})
	if got[len(got)-1] != "t3.small" {
		t.Fatalf("last fallback=%q want t3.small in %v", got[len(got)-1], got)
	}
	arm := awsLaunchCandidates(Config{Provider: "aws", TargetOS: targetLinux, Architecture: ArchitectureARM64, architectureExplicit: true, Class: "beast", ServerType: "c7g.16xlarge"})
	if arm[len(arm)-1] != "t4g.small" {
		t.Fatalf("last arm fallback=%q want t4g.small in %v", arm[len(arm)-1], arm)
	}
	wsl2 := awsLaunchCandidates(Config{Provider: "aws", TargetOS: targetWindows, WindowsMode: windowsModeWSL2, Class: "standard", ServerType: "m8i.large"})
	for _, candidate := range wsl2 {
		if strings.HasPrefix(candidate, "t3.") || strings.HasPrefix(candidate, "m7") {
			t.Fatalf("WSL2 candidate %q does not support nested virtualization: %v", candidate, wsl2)
		}
	}
	exact := awsLaunchCandidates(Config{Provider: "aws", Class: "beast", ServerType: "t3.small", ServerTypeExplicit: true})
	if len(exact) != 1 || exact[0] != "t3.small" {
		t.Fatalf("exact candidates=%v", exact)
	}
}

func TestAWSRegionAndAvailabilityZoneCandidates(t *testing.T) {
	cfg := Config{
		AWSRegion: "eu-west-1",
		Capacity: CapacityConfig{
			Regions:           []string{"us-east-1", "eu-west-1"},
			AvailabilityZones: []string{"us-east-1a", "eu-west-1b"},
		},
	}
	got := awsRegionCandidates(cfg, "eu-west-2")
	want := []string{"eu-west-2", "eu-west-1", "us-east-1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("awsRegionCandidates=%v want %v", got, want)
	}
	if zone := awsAvailabilityZoneForRegion(cfg, "eu-west-1"); zone != "eu-west-1b" {
		t.Fatalf("awsAvailabilityZoneForRegion=%q want eu-west-1b", zone)
	}
}

func TestRemoteSyncSanityReportsDeletionSample(t *testing.T) {
	got := remoteSyncSanity("/work/repo", false)
	for _, want := range []string{
		"remote sync sanity failed: $deletions tracked deletions",
		`awk '/^ D|^D / { print "  " substr($0,4) }'`,
		"head -20",
		"exit 66",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("remoteSyncSanity() missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, "/tmp/crabbox-git-status") {
		t.Fatalf("remoteSyncSanity() uses a global status file: %q", got)
	}
}

func TestSSHControlBudgetsSeparateFiniteAuthorityFromUnlimitedWork(t *testing.T) {
	limit := sshCommandLimit{execution: sshControlExecutionLimit, control: true}
	for _, target := range []SSHTarget{{Port: "22", FallbackPorts: []string{}}, {Port: "2222", FallbackPorts: []string{"22", "22"}}} {
		routes := len(sshPortCandidates(target.Port, target.FallbackPorts))
		attempts := 1
		if routes > 1 {
			attempts += routes
		}
		want := time.Duration(attempts)*sshTransportPreparationTimeout + sshControlExecutionLimit
		if got := sshTransportCallBudget(target, sshControlMetadataLimit, limit); got != want {
			t.Fatalf("control budget=%s want=%s", got, want)
		}
		if got := sshTransportCallBudget(target, sshControlMetadataLimit, sshCommandLimit{}); got != 0 {
			t.Fatal("ordinary unlimited execution acquired a control duration")
		}
	}
	if _, err := prepareSSHTransport(SSHTarget{}, "true", nil, 0, sshCommandLimit{control: true}); err == nil {
		t.Fatal("unlimited control command accepted")
	}
}

func TestWSLStreamWriterPreambleContract(t *testing.T) {
	powerShell, err := exec.LookPath("pwsh")
	if err != nil {
		powerShell, err = exec.LookPath("powershell.exe")
	}
	if err != nil {
		t.Skip("PowerShell is unavailable")
	}
	helper := "# é\nprintf '%s' \"$CBX_HELPER\"\ncat\n\n\n"
	payload := []byte{0, 255, 13, 10}
	frame := append([]byte(helper), payload...)
	for _, bom := range []bool{false, true} {
		t.Run(fmt.Sprint(bom), func(t *testing.T) {
			flag := "$false"
			if bom {
				flag = "$true"
			}
			path := filepath.Join(t.TempDir(), "wire")
			script := `$path=` + psQuote(path) + `;$m=[IO.File]::Open($path,'CreateNew','ReadWrite','ReadWrite');$w=[IO.StreamWriter]::new($m,[Text.UTF8Encoding]::new(` + flag + `),4096,$true);$w.AutoFlush=$true
$t=$w.FlushAsync();if(!$t.Wait(5000)){exit 74};$null=$t.GetAwaiter().GetResult()
$b=[Convert]::FromBase64String('` + base64.StdEncoding.EncodeToString(frame) + `')
$raw=[IO.FileStream]::new($w.BaseStream.SafeFileHandle,[IO.FileAccess]::Write,1,$false)
$t=$raw.WriteAsync($b,0,$b.Length);if(!$t.Wait(5000)){exit 74};$null=$t.GetAwaiter().GetResult()
# Observe bytes before any writer closes; no EOF may be needed for delivery.
[Console]::Out.Write([Convert]::ToBase64String([IO.File]::ReadAllBytes($path)));$raw.Dispose()`
			encoded, err := exec.CommandContext(t.Context(), powerShell, "-NoProfile", "-NonInteractive", "-Command", script).Output()
			if err != nil {
				t.Fatal(err)
			}
			wire, err := base64.StdEncoding.DecodeString(string(encoded))
			if err != nil {
				t.Fatal(err)
			}
			want := frame
			count := "0"
			if bom {
				want = append([]byte{239, 187, 191}, frame...)
				count = "3"
			}
			if !bytes.Equal(wire, want) {
				t.Fatalf("StreamWriter wire=%x want=%x", wire, want)
			}
			cmd := exec.CommandContext(t.Context(), "sh", "-c", wslHelperBootstrap, "sh", strconv.Itoa(len(helper)), count)
			cmd.Stdin = bytes.NewReader(wire)
			got, err := cmd.Output()
			if err != nil || !bytes.Equal(got, frame) {
				t.Fatalf("post-preamble bytes=%x err=%v", got, err)
			}
		})
	}
}
