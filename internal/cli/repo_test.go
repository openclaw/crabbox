package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSyncManifestUsesGitFilesAndIgnoresIgnoredJunk(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	writeFile(t, filepath.Join(dir, ".gitignore"), ".local/\n.build/\n")
	writeFile(t, filepath.Join(dir, "tracked.txt"), "tracked")
	runGit(t, dir, "add", ".gitignore", "tracked.txt")
	runGit(t, dir, "commit", "-m", "init")
	writeFile(t, filepath.Join(dir, "untracked.txt"), "untracked")
	writeFile(t, filepath.Join(dir, ".local", "cache.bin"), strings.Repeat("x", 1024))
	writeFile(t, filepath.Join(dir, ".build", "artifact"), strings.Repeat("x", 1024))
	writeFile(t, filepath.Join(dir, ".crabbox", "runs", "run_123", "artifacts.tgz"), "artifact")

	manifest, err := syncManifest(dir, configuredExcludes(baseConfig()))
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(manifest.Files, ",")
	for _, want := range []string{".gitignore", "tracked.txt", "untracked.txt"} {
		if !strings.Contains(got, want) {
			t.Fatalf("manifest %q missing %q", got, want)
		}
	}
	for _, notWant := range []string{".local/cache.bin", ".build/artifact", ".crabbox/runs/run_123/artifacts.tgz", ".git/HEAD"} {
		if strings.Contains(got, notWant) {
			t.Fatalf("manifest %q should not contain %q", got, notWant)
		}
	}
	if !bytes.Contains(manifest.NUL(), []byte("tracked.txt\x00")) {
		t.Fatalf("manifest NUL list missing tracked file: %q", string(manifest.NUL()))
	}
}

func TestSyncManifestPrunesNestedDefaultExcludes(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	writeFile(t, filepath.Join(dir, "packages", "app", "node_modules", "lib.js"), "cache")
	writeFile(t, filepath.Join(dir, ".ignored", "churn"), "cache")
	writeFile(t, filepath.Join(dir, "playwright-report", "index.html"), "cache")
	writeFile(t, filepath.Join(dir, "apps", "foo", ".build", "debug.o"), "cache")
	writeFile(t, filepath.Join(dir, "apps", "foo", "src", "main.go"), "package main\n")
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "init")

	manifest, err := syncManifest(dir, configuredExcludes(baseConfig()))
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(manifest.Files, ",")
	if strings.Contains(got, "node_modules") || strings.Contains(got, ".build") || strings.Contains(got, ".ignored") || strings.Contains(got, "playwright-report") {
		t.Fatalf("manifest should prune nested cache dirs: %q", got)
	}
	if !strings.Contains(got, "apps/foo/src/main.go") {
		t.Fatalf("manifest missing source file: %q", got)
	}
}

func TestSyncManifestDoesNotExcludeTrackedBuildOrOutSourcePaths(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	writeFile(t, filepath.Join(dir, "cmd", "build", "main.go"), "package main\n")
	writeFile(t, filepath.Join(dir, "src", "out", "schema.sql"), "select 1;\n")
	writeFile(t, filepath.Join(dir, "testdata", "tmp", "input.json"), "{}\n")
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "init")

	manifest, err := syncManifest(dir, configuredExcludes(baseConfig()))
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(manifest.Files, ",")
	for _, want := range []string{"cmd/build/main.go", "src/out/schema.sql", "testdata/tmp/input.json"} {
		if !strings.Contains(got, want) {
			t.Fatalf("manifest %q missing tracked source path %q", got, want)
		}
	}
}

func TestSyncManifestPrunesAppleDoubleSidecars(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	writeFile(t, filepath.Join(dir, "src", "index.ts"), "export const ok = true\n")
	writeFile(t, filepath.Join(dir, "src", "._index.ts"), "appledouble")
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "init")

	manifest, err := syncManifest(dir, configuredExcludes(baseConfig()))
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(manifest.Files, ",")
	if !strings.Contains(got, "src/index.ts") {
		t.Fatalf("manifest missing real source file: %q", got)
	}
	if strings.Contains(got, "._index.ts") {
		t.Fatalf("manifest should exclude AppleDouble sidecars: %q", got)
	}
}

func TestCrabboxIgnoreExtendsSyncExcludes(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	writeFile(t, filepath.Join(dir, ".crabboxignore"), "# local-only artifacts\nlocal-artifacts\n*.tmp\n\n")
	writeFile(t, filepath.Join(dir, "src", "main.go"), "package main\n")
	writeFile(t, filepath.Join(dir, "local-artifacts", "cache.bin"), "cache")
	writeFile(t, filepath.Join(dir, "notes.tmp"), "tmp")
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "init")

	excludes, err := syncExcludes(dir, baseConfig())
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := syncManifest(dir, excludes)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(manifest.Files, ",")
	if !strings.Contains(got, "src/main.go") {
		t.Fatalf("manifest missing source file: %q", got)
	}
	for _, notWant := range []string{"local-artifacts/cache.bin", "notes.tmp"} {
		if strings.Contains(got, notWant) {
			t.Fatalf("manifest %q should exclude .crabboxignore pattern %q", got, notWant)
		}
	}
}

func TestCrabboxIgnorePrunesDeletedPaths(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	writeFile(t, filepath.Join(dir, ".crabboxignore"), "generated.bin\n")
	writeFile(t, filepath.Join(dir, "generated.bin"), "old")
	writeFile(t, filepath.Join(dir, "deleted.txt"), "old")
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "init")
	if err := os.Remove(filepath.Join(dir, "generated.bin")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "deleted.txt")); err != nil {
		t.Fatal(err)
	}

	excludes, err := syncExcludes(dir, baseConfig())
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := syncManifest(dir, excludes)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(manifest.Deleted, ",") != "deleted.txt" {
		t.Fatalf("deleted manifest should omit .crabboxignore patterns: %v", manifest.Deleted)
	}
}

func TestReadCrabboxIgnoreSkipsBlankAndCommentLines(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".crabboxignore"), "\n# comment\n  build-output  \n*.tmp\r\n")
	got, err := readCrabboxIgnore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "build-output,*.tmp" {
		t.Fatalf("patterns=%q", got)
	}
}

func TestSyncManifestRecordsTrackedDeletes(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	writeFile(t, filepath.Join(dir, "deleted.txt"), "tracked")
	writeFile(t, filepath.Join(dir, "kept.txt"), "tracked")
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "init")
	if err := os.Remove(filepath.Join(dir, "deleted.txt")); err != nil {
		t.Fatal(err)
	}

	manifest, err := syncManifest(dir, configuredExcludes(baseConfig()))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(manifest.Files, ","), "deleted.txt") {
		t.Fatalf("deleted file should not be synced: %v", manifest.Files)
	}
	if strings.Join(manifest.Deleted, ",") != "deleted.txt" {
		t.Fatalf("deleted manifest=%v", manifest.Deleted)
	}
	if !bytes.Equal(manifest.DeletedNUL(), []byte("deleted.txt\x00")) {
		t.Fatalf("deleted NUL=%q", string(manifest.DeletedNUL()))
	}
	if strings.Join(manifest.Changed, ",") != "deleted.txt" {
		t.Fatalf("deleted path should count in dirty delta: %v", manifest.Changed)
	}
}

func TestSyncManifestRecordsDirtyDelta(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	writeFile(t, filepath.Join(dir, "src", "main.go"), "package main\n")
	writeFile(t, filepath.Join(dir, "README.md"), "hello\n")
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "init")
	writeFile(t, filepath.Join(dir, "src", "main.go"), "package main\n// changed\n")
	writeFile(t, filepath.Join(dir, "scratch.txt"), "local\n")

	manifest, err := syncManifest(dir, configuredExcludes(baseConfig()))
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(manifest.Changed, ",")
	if got != "scratch.txt,src/main.go" {
		t.Fatalf("dirty delta=%q", got)
	}
	if manifest.ChangedBytes <= 0 {
		t.Fatalf("dirty delta bytes=%d", manifest.ChangedBytes)
	}
}

func TestSyncManifestDoesNotDeleteRecreatedStagedDelete(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	writeFile(t, filepath.Join(dir, "foo.txt"), "old")
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "init")
	runGit(t, dir, "rm", "foo.txt")
	writeFile(t, filepath.Join(dir, "foo.txt"), "new")

	manifest, err := syncManifest(dir, configuredExcludes(baseConfig()))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(manifest.Files, ",") != "foo.txt" {
		t.Fatalf("recreated file should sync: %v", manifest.Files)
	}
	if len(manifest.Deleted) != 0 {
		t.Fatalf("recreated file must not be deleted after rsync: %v", manifest.Deleted)
	}
}

func TestRemoteGitSeedCandidateRequiresRemoteTrackingRef(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	writeFile(t, filepath.Join(dir, "foo.txt"), "old")
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "init")
	head := gitOutput(dir, "rev-parse", "HEAD")

	repo := Repo{Root: dir, RemoteURL: "https://github.com/openclaw/crabbox.git", Head: head}
	if remoteGitSeedCandidate(repo) {
		t.Fatal("unpublished head should not be a seed candidate")
	}
	runGit(t, dir, "update-ref", "refs/remotes/origin/main", head)
	if !remoteGitSeedCandidate(repo) {
		t.Fatal("head in a remote-tracking ref should be a seed candidate")
	}
}

func TestCheckSyncPreflightFailsLargeCandidate(t *testing.T) {
	cfg := baseConfig()
	cfg.Sync.FailFiles = 2
	var stderr bytes.Buffer
	err := checkSyncPreflight(SyncManifest{Files: []string{"a", "b"}, Bytes: 10}, cfg, false, &stderr)
	if err == nil {
		t.Fatal("expected large sync candidate to fail")
	}
	if !strings.Contains(stderr.String(), "sync candidate: 2 files") {
		t.Fatalf("missing preflight output: %q", stderr.String())
	}
}

func TestCheckSyncPreflightUsesDirtyDeltaWhenPresent(t *testing.T) {
	cfg := baseConfig()
	cfg.Sync.FailFiles = 2
	var stderr bytes.Buffer
	err := checkSyncPreflight(SyncManifest{
		Files:        []string{"a", "b", "c", "d"},
		Changed:      []string{"src/changed.go"},
		Bytes:        400,
		ChangedBytes: 10,
	}, cfg, false, &stderr)
	if err != nil {
		t.Fatalf("small dirty delta should not fail on full candidate size: %v", err)
	}
	got := stderr.String()
	if !strings.Contains(got, "sync candidate: 4 files") || !strings.Contains(got, "dirty_delta=1 files") {
		t.Fatalf("missing dirty delta output: %q", got)
	}
}

func TestCheckSyncPreflightUsesDirtyDeltaForDeletions(t *testing.T) {
	cfg := baseConfig()
	cfg.Sync.FailFiles = 2
	var stderr bytes.Buffer
	err := checkSyncPreflight(SyncManifest{
		Files:   []string{"a", "b", "c", "d"},
		Changed: []string{"deleted.go"},
		Bytes:   400,
	}, cfg, false, &stderr)
	if err != nil {
		t.Fatalf("single deleted dirty path should not fail on full candidate size: %v", err)
	}
	got := stderr.String()
	if !strings.Contains(got, "dirty_delta=1 files") {
		t.Fatalf("missing deletion dirty delta output: %q", got)
	}
}

func TestHumanBytes(t *testing.T) {
	if got := humanBytes(1536); got != "1.5 KiB" {
		t.Fatalf("humanBytes=%q", got)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func writeFile(t *testing.T, path, value string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value), 0o644); err != nil {
		t.Fatal(err)
	}
}
