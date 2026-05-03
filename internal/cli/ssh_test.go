package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

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
	if !strings.HasPrefix(got, "powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -EncodedCommand ") {
		t.Fatalf("windows command should use encoded powershell: %q", got)
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
	const prefix = "powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -EncodedCommand "
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
	got := wrapRemoteForTarget(target, "echo ok")
	if got != "wsl.exe --exec bash -lc 'echo ok'" {
		t.Fatalf("wrapRemoteForTarget()=%q", got)
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
		"ControlPersist=60s",
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
		95*time.Second,
		10*time.Minute,
	)
	for _, want := range []string{
		"waiting for 203.0.113.10:2222 bootstrap toolchain...",
		"elapsed=1m35s",
		"remaining=10m0s",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("progress message missing %q in %q", want, got)
		}
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

func TestAWSLaunchCandidatesAddsPolicyFallbackUnlessExact(t *testing.T) {
	got := awsLaunchCandidates(Config{Provider: "aws", Class: "beast", ServerType: "c7a.48xlarge"})
	if got[len(got)-1] != "t3.small" {
		t.Fatalf("last fallback=%q want t3.small in %v", got[len(got)-1], got)
	}
	exact := awsLaunchCandidates(Config{Provider: "aws", Class: "beast", ServerType: "t3.small", ServerTypeExplicit: true})
	if len(exact) != 1 || exact[0] != "t3.small" {
		t.Fatalf("exact candidates=%v", exact)
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
