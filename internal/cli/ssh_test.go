package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
		"ServerAliveInterval=15",
		"ServerAliveCountMax=2",
		"ControlMaster=auto",
		"ControlPersist=60s",
		"ControlPath=",
		"crabbox-ssh-%C",
		"UserKnownHostsFile=/tmp/crabbox-lease/known_hosts",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("sshArgs() missing %q in %q", want, got)
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
		got := sshPortCandidates(in)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("sshPortCandidates(%q)=%v want %v", in, got, want)
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
