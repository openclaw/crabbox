package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

const powerShellEncodedCommandPrefix = "powershell.exe -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -EncodedCommand "

func TestVersion(t *testing.T) {
	var out bytes.Buffer
	app := App{Stdout: &out, Stderr: &bytes.Buffer{}}
	if err := app.Run(context.Background(), []string{"--version"}); err != nil {
		t.Fatalf("Run(--version) error: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != version {
		t.Fatalf("Run(--version)=%q want %q", got, version)
	}
}

func TestRemoteCommandQuotesWorkdirEnvAndArgs(t *testing.T) {
	got := remoteCommand("/work/crabbox/cbx_1/openclaw", map[string]string{"NODE_OPTIONS": "--max-old-space-size=8192"}, []string{"pnpm", "check:changed"})
	for _, want := range []string{
		"cd '/work/crabbox/cbx_1/openclaw'",
		"NODE_OPTIONS='--max-old-space-size=8192'",
		"bash -lc",
		"'exec \"$@\"' bash 'pnpm' 'check:changed'",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("remoteCommand() missing %q in %q", want, got)
		}
	}
}

func TestRemoteShellCommandRunsScript(t *testing.T) {
	got := remoteShellCommand("/work/crabbox/cbx_1/repo", map[string]string{"CI": "1"}, "pnpm install && pnpm test")
	for _, want := range []string{
		"cd '/work/crabbox/cbx_1/repo'",
		"CI='1'",
		"bash -lc 'pnpm install && pnpm test'",
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
		"'exec \"$@\"' bash 'pnpm' 'test'",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("remoteCommandWithEnvFile() missing %q in %q", want, got)
		}
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

func decodePowerShellCommand(t *testing.T, command string) string {
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
	for _, unwanted := range []string{"ControlMaster=auto", "ControlPersist=", "ControlPath="} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("sshArgs() should omit mux option %q for secret auth target: %q", unwanted, got)
		}
	}
	if !strings.Contains(got, "ControlMaster=no") {
		t.Fatalf("sshArgs() missing ControlMaster=no for secret auth target: %q", got)
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

func TestRunSSHStreamRetriesFallbackPorts(t *testing.T) {
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
printf 'ok\n'
exit 0
`
	if err := os.WriteFile(sshPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CRABBOX_FAKE_SSH_PORTS", portsPath)

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
	ports, err := os.ReadFile(portsPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(ports) != "2222\n22\n" {
		t.Fatalf("ports=%q want fallback sequence", string(ports))
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

func TestRemotePruneSyncManifestDeletesOnlyManagedPaths(t *testing.T) {
	got := remotePruneSyncManifest("/work/repo")
	for _, want := range []string{
		"sync-deleted.new",
		"manifest_removed_paths",
		"python3 -",
		"rm -f --",
		"rmdir --",
		"sync-manifest.new",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("remotePruneSyncManifest missing %q in %q", want, got)
		}
	}
}

func TestRemotePruneSyncManifestUsesDeletedListBeforeOldManifestDiff(t *testing.T) {
	got := remotePruneSyncManifest("/work/repo")
	deletedIndex := strings.Index(got, `delete_paths < "$deleted"`)
	oldIndex := strings.Index(got, "manifest_removed_paths | delete_paths")
	if deletedIndex < 0 || oldIndex < 0 || deletedIndex > oldIndex {
		t.Fatalf("deleted list should be applied before old manifest diff: %q", got)
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
	workdir := t.TempDir()
	mustWriteTestFile(t, filepath.Join(workdir, ".crabbox", "sync-manifest"), "keep.txt\x00kept-dir/keep.txt\x00stale.txt\x00old-empty/remove.txt\x00non-empty/remove.txt\x00")
	mustWriteTestFile(t, filepath.Join(workdir, ".crabbox", "sync-manifest.new"), "keep.txt\x00kept-dir/keep.txt\x00")
	mustWriteTestFile(t, filepath.Join(workdir, ".crabbox", "sync-deleted.new"), "explicit-delete.txt\x00../outside.txt\x00/absolute.txt\x00")
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

	cmd := exec.Command("bash", "-lc", remotePruneSyncManifest(workdir))
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
	workdir := t.TempDir()
	if err := os.Mkdir(filepath.Join(workdir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	metaDir := filepath.Join(workdir, ".git", "crabbox")
	if err := os.MkdirAll(metaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(metaDir, "sync-manifest.new"), []byte("tracked.txt\x00"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(metaDir, "sync-deleted.new"), []byte("deleted.txt\x00"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("bash", "-lc", remoteFinalizeSync(workdir, remoteSyncFinalizeOptions{
		BaseRef:     "main",
		BaseSHA:     "abc123",
		Fingerprint: "fp123",
	}))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("remote finalize failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(metaDir, "sync-deleted.new")); !os.IsNotExist(err) {
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
	fingerprint, err := os.ReadFile(filepath.Join(metaDir, "sync-fingerprint"))
	if err != nil {
		t.Fatal(err)
	}
	if string(fingerprint) != "fp123" {
		t.Fatalf("unexpected fingerprint: %q", fingerprint)
	}
}

func TestRemoteGitSeedRemovesFailedCheckout(t *testing.T) {
	got := remoteGitSeed("/work/repo", "https://github.com/openclaw/crabbox.git", "missing-sha")
	for _, want := range []string{
		"if (cd \"$tmp\"",
		"git checkout --quiet 'missing-sha' || git checkout --quiet FETCH_HEAD",
		"else rm -rf \"$tmp\"; fi",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("remoteGitSeed missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, "git checkout --quiet FETCH_HEAD || true") {
		t.Fatalf("remoteGitSeed should not keep failed checkouts: %q", got)
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
	workdir := t.TempDir()
	manifest := "keep.txt\x00"
	deleted := "old.txt\x00"
	input := fmt.Sprintf("%d\n", len(manifest)) + manifest + deleted
	cmd := exec.Command("bash", "-lc", remoteWriteSyncManifestsNew(workdir))
	cmd.Stdin = strings.NewReader(input)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("write manifests failed: %v\n%s", err, out)
	}
	metaDir := filepath.Join(workdir, ".crabbox")
	gotManifest, err := os.ReadFile(filepath.Join(metaDir, "sync-manifest.new"))
	if err != nil {
		t.Fatal(err)
	}
	if string(gotManifest) != manifest {
		t.Fatalf("unexpected manifest: %q", gotManifest)
	}
	gotDeleted, err := os.ReadFile(filepath.Join(metaDir, "sync-deleted.new"))
	if err != nil {
		t.Fatal(err)
	}
	if string(gotDeleted) != deleted {
		t.Fatalf("unexpected deleted manifest: %q", gotDeleted)
	}
}

func TestRemoteSyncMetadataUsesGitDirForGitWorktree(t *testing.T) {
	workdir := t.TempDir()
	if err := os.Mkdir(filepath.Join(workdir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
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

func TestServerTypeForClass(t *testing.T) {
	tests := map[string]string{
		"standard": "ccx33",
		"fast":     "ccx43",
		"large":    "ccx53",
		"beast":    "ccx63",
		"ccx23":    "ccx23",
	}
	for in, want := range tests {
		if got := serverTypeForClass(in); got != want {
			t.Fatalf("serverTypeForClass(%q)=%q want %q", in, got, want)
		}
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
		{provider: "daytona", class: "beast", want: "snapshot"},
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

func TestAWSInstanceTypeCandidatesForTargetsAndModes(t *testing.T) {
	tests := []struct {
		name        string
		target      string
		windowsMode string
		class       string
		want        []string
	}{
		{name: "macos", target: targetMacOS, class: "beast", want: []string{"mac2.metal"}},
		{name: "windows normal standard", target: targetWindows, class: "standard", want: []string{"m7i.large", "m7a.large", "t3.large"}},
		{name: "windows normal custom", target: targetWindows, class: "m7i.8xlarge", want: []string{"m7i.8xlarge"}},
		{name: "windows wsl2 fast", target: targetWindows, windowsMode: windowsModeWSL2, class: "fast", want: []string{"m8i.xlarge", "m8i-flex.xlarge", "c8i.xlarge", "r8i.xlarge"}},
		{name: "windows wsl2 custom", target: targetWindows, windowsMode: windowsModeWSL2, class: "m8i.8xlarge", want: []string{"m8i.8xlarge"}},
		{name: "linux large", target: targetLinux, class: "large", want: []string{"c7a.24xlarge", "c7i.24xlarge", "m7a.24xlarge", "m7i.24xlarge", "r7a.24xlarge", "c7a.16xlarge", "c7a.12xlarge"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := awsInstanceTypeCandidatesForTargetModeClass(tt.target, tt.windowsMode, tt.class)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("candidates=%v want %v", got, tt.want)
			}
		})
	}

	got := awsInstanceTypeCandidatesForTargetClass(targetWindows, "fast")
	want := []string{"m7i.xlarge", "m7a.xlarge", "t3.xlarge"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("target class candidates=%v want %v", got, want)
	}
}

func TestAWSLaunchCandidatesAddsPolicyFallbackUnlessExact(t *testing.T) {
	got := awsLaunchCandidates(Config{Provider: "aws", Class: "beast", ServerType: "c7a.48xlarge"})
	if got[len(got)-1] != "t3.small" {
		t.Fatalf("last fallback=%q want t3.small in %v", got[len(got)-1], got)
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
}
