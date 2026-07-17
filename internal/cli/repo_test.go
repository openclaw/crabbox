package cli

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestFindRepoUsesOriginNameInsideLinkedWorktree(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "crabbox")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "init")
	runGit(t, root, "config", "user.email", "test@example.com")
	runGit(t, root, "config", "user.name", "Test")
	writeFile(t, filepath.Join(root, "README.md"), "crabbox\n")
	runGit(t, root, "add", "README.md")
	runGit(t, root, "commit", "-m", "init")
	runGit(t, root, "remote", "add", "origin", "https://github.com/openclaw/crabbox.git")

	worktree := filepath.Join(parent, "fix-blacksmith-success-workflow-state")
	runGit(t, root, "worktree", "add", "-b", "fix/blacksmith-success-workflow-state", worktree)
	t.Chdir(worktree)

	repo, err := findRepo()
	if err != nil {
		t.Fatal(err)
	}
	gotRoot, err := filepath.EvalSymlinks(repo.Root)
	if err != nil {
		t.Fatal(err)
	}
	wantRoot, err := filepath.EvalSymlinks(worktree)
	if err != nil {
		t.Fatal(err)
	}
	if gotRoot != wantRoot {
		t.Fatalf("repo root=%q want %q", repo.Root, worktree)
	}
	if repo.Name != "crabbox" {
		t.Fatalf("repo name=%q want crabbox", repo.Name)
	}
}

func TestRepoNameFromRootAndRemoteFallsBackToRemoteBasename(t *testing.T) {
	if got := repoNameFromRootAndRemote("/tmp/worktrees/feature", "git@gitlab.example.com:team/project.git"); got != "project" {
		t.Fatalf("repo name=%q want project", got)
	}
	if got := repoNameFromRootAndRemote("/tmp/worktrees/feature", ""); got != "feature" {
		t.Fatalf("repo name=%q want feature", got)
	}
}

func TestGitCheckoutHasHiddenOmissions(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	writeFile(t, filepath.Join(dir, "included", "keep.txt"), "keep\n")
	writeFile(t, filepath.Join(dir, "omitted", "drop.txt"), "drop\n")
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "init")
	if omitted, err := GitCheckoutHasHiddenOmissions(dir); err != nil || omitted {
		t.Fatal("full checkout reported omitted tracked paths")
	}

	runGit(t, dir, "sparse-checkout", "set", "included")
	rulesUnavailable := func(string, []gitTrackedPath) (map[string]struct{}, error) {
		return nil, fmt.Errorf("check-rules unsupported")
	}
	if omitted, err := GitCheckoutHasHiddenOmissions(dir); err != nil || !omitted {
		t.Fatal("sparse-checkout omission was not detected")
	}
	if omitted, err := gitCheckoutHasHiddenOmissions(dir, rulesUnavailable); err != nil || !omitted {
		t.Fatal("old Git missed definite skip-worktree omission")
	}
	if _, err := os.Stat(filepath.Join(dir, "omitted", "drop.txt")); !os.IsNotExist(err) {
		t.Fatalf("omitted path still materialized: %v", err)
	}
	// The sparse rules remain authoritative even if another Git operation
	// loses the index's skip-worktree bit for an excluded path.
	runGit(t, dir, "update-index", "--no-skip-worktree", "omitted/drop.txt")
	if omitted, err := GitCheckoutHasHiddenOmissions(dir); err != nil {
		if !strings.Contains(err.Error(), "Git 2.41") {
			t.Fatal(err)
		}
	} else if !omitted {
		t.Fatal("sparse rule omission was missed after clearing skip-worktree")
	}

	// Sparse mode itself is harmless when the current spec materializes every
	// tracked path. Avoid blocking that valid Blacksmith workflow.
	runGit(t, dir, "sparse-checkout", "set", "--no-cone", "/*")
	writeFile(t, filepath.Join(dir, "omitted", "drop.txt"), "drop\n")
	runGit(t, dir, "update-index", "--no-skip-worktree", "omitted/drop.txt")
	if omitted, err := GitCheckoutHasHiddenOmissions(dir); err != nil || omitted {
		t.Fatal("fully materialized sparse checkout reported omissions")
	}
	if omitted, err := gitCheckoutHasHiddenOmissions(dir, rulesUnavailable); err != nil || omitted {
		t.Fatal("old Git rejected fully materialized sparse checkout")
	}
	// An ordinary deletion of an included path is intentional sync input, not
	// a sparse omission.
	if err := os.Remove(filepath.Join(dir, "omitted", "drop.txt")); err != nil {
		t.Fatal(err)
	}
	if omitted, err := GitCheckoutHasHiddenOmissions(dir); err != nil {
		if !strings.Contains(err.Error(), "Git 2.41") {
			t.Fatal(err)
		}
	} else if omitted {
		t.Fatal("intentional deletion in fully included sparse checkout was rejected")
	}
	if omitted, err := gitCheckoutHasHiddenOmissions(dir, rulesUnavailable); omitted || err == nil || !strings.Contains(err.Error(), "Git 2.41") {
		t.Fatalf("old Git ambiguity omission=%v err=%v", omitted, err)
	}
	writeFile(t, filepath.Join(dir, "omitted", "drop.txt"), "drop\n")
	// Actual absence remains unsafe even when the sparse rules include the path.
	runGit(t, dir, "update-index", "--skip-worktree", "included/keep.txt")
	if err := os.Remove(filepath.Join(dir, "included", "keep.txt")); err != nil {
		t.Fatal(err)
	}
	if omitted, err := GitCheckoutHasHiddenOmissions(dir); err != nil || !omitted {
		t.Fatal("absent skip-worktree path matching sparse rules was missed")
	}
	writeFile(t, filepath.Join(dir, "included", "keep.txt"), "keep\n")
	runGit(t, dir, "update-index", "--no-skip-worktree", "included/keep.txt")

	// A manually marked but still-present path in an ordinary checkout is not
	// a sparse omission and must not block delegated sync.
	runGit(t, dir, "sparse-checkout", "disable")
	runGit(t, dir, "update-index", "--skip-worktree", "included/keep.txt")
	if omitted, err := GitCheckoutHasHiddenOmissions(dir); err != nil || omitted {
		t.Fatal("dense checkout with present skip-worktree path reported omissions")
	}
	if err := os.Remove(filepath.Join(dir, "included", "keep.txt")); err != nil {
		t.Fatal(err)
	}
	if omitted, err := GitCheckoutHasHiddenOmissions(dir); err != nil || !omitted {
		t.Fatal("dense checkout missed absent skip-worktree path")
	}
}

func TestGitCheckoutHasHiddenOmissionsInLinkedWorktree(t *testing.T) {
	parent := t.TempDir()
	source := filepath.Join(parent, "source")
	worktree := filepath.Join(parent, "sparse-worktree")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, source, "init")
	runGit(t, source, "config", "user.email", "test@example.com")
	runGit(t, source, "config", "user.name", "Test")
	writeFile(t, filepath.Join(source, "included", "keep.txt"), "keep\n")
	writeFile(t, filepath.Join(source, "omitted", "drop.txt"), "drop\n")
	runGit(t, source, "add", ".")
	runGit(t, source, "commit", "-m", "init")
	runGit(t, source, "worktree", "add", "--detach", worktree)
	runGit(t, worktree, "sparse-checkout", "set", "included")

	if omitted, err := GitCheckoutHasHiddenOmissions(worktree); err != nil || !omitted {
		t.Fatalf("linked sparse worktree omission=%v err=%v", omitted, err)
	}
}

func TestGitCheckoutHasHiddenOmissionsInNestedCone(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	for path, contents := range map[string]string{
		"root.txt":     "root\n",
		"a/parent.txt": "parent\n",
		"a/b/x.txt":    "included\n",
		"a/c/y.txt":    "omitted\n",
	} {
		writeFile(t, filepath.Join(dir, path), contents)
	}
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "init")
	runGit(t, dir, "sparse-checkout", "set", "a/b")
	runGit(t, dir, "update-index", "--no-skip-worktree", "a/c/y.txt")

	if omitted, err := GitCheckoutHasHiddenOmissions(dir); err != nil {
		if !strings.Contains(err.Error(), "Git 2.41") {
			t.Fatal(err)
		}
	} else if !omitted {
		t.Fatal("nested cone omission was missed")
	}
}

func TestGitCheckoutHasHiddenOmissionsThroughSymlinkAncestor(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	writeFile(t, filepath.Join(dir, "included", "keep.txt"), "keep\n")
	writeFile(t, filepath.Join(dir, "omitted", "drop.txt"), "drop\n")
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "init")
	runGit(t, dir, "sparse-checkout", "set", "included")

	target := t.TempDir()
	writeFile(t, filepath.Join(target, "drop.txt"), "shadow\n")
	if err := os.Symlink(target, filepath.Join(dir, "omitted")); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink unavailable: %v", err)
		}
		t.Fatal(err)
	}
	if omitted, err := GitCheckoutHasHiddenOmissions(dir); err != nil || !omitted {
		t.Fatalf("symlink-shadowed omission=%v err=%v", omitted, err)
	}
}

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

func TestSyncManifestIncludeWhitelist(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	writeFile(t, filepath.Join(dir, "src", "main.go"), "package main\n")
	writeFile(t, filepath.Join(dir, "scripts", "build.sh"), "echo hi\n")
	writeFile(t, filepath.Join(dir, "package.json"), "{}\n")
	writeFile(t, filepath.Join(dir, "data", "huge.bin"), strings.Repeat("x", 4096))
	writeFile(t, filepath.Join(dir, "notes.txt"), "ignore me\n")
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "init")

	manifest, err := syncManifestFiltered(dir, configuredExcludes(baseConfig()), []string{"src", "scripts", "package.json"})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(manifest.Files, ",")
	for _, want := range []string{"src/main.go", "scripts/build.sh", "package.json"} {
		if !strings.Contains(got, want) {
			t.Fatalf("include whitelist dropped wanted path %q: %q", want, got)
		}
	}
	for _, notWant := range []string{"data/huge.bin", "notes.txt"} {
		if strings.Contains(got, notWant) {
			t.Fatalf("include whitelist kept non-included path %q: %q", notWant, got)
		}
	}
}

func TestPathIncluded(t *testing.T) {
	if !pathIncluded("anything/at/all.txt", nil) {
		t.Fatal("empty includes should keep all paths")
	}
	includes := []string{"src", "scripts/proof", "package.json"}
	for _, in := range []string{"src/a.go", "src/deep/b.go", "scripts/proof/run.sh", "package.json"} {
		if !pathIncluded(in, includes) {
			t.Fatalf("expected %q to be included", in)
		}
	}
	for _, out := range []string{"data/x.bin", "scripts/other.sh", "package.lock", "packages/app/src/main.go", "examples/package.json"} {
		if pathIncluded(out, includes) {
			t.Fatalf("expected %q to be excluded by whitelist", out)
		}
	}
	globIncludes := []string{"*.go", "docs/*.md"}
	for _, in := range []string{"main.go", "docs/readme.md"} {
		if !pathIncluded(in, globIncludes) {
			t.Fatalf("expected glob to include %q", in)
		}
	}
	for _, out := range []string{"src/main.go", "docs/nested/readme.md"} {
		if pathIncluded(out, globIncludes) {
			t.Fatalf("expected root-relative glob to exclude %q", out)
		}
	}
}

func TestSyncGitSeedDisabledByIncludeWhitelist(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	writeFile(t, filepath.Join(dir, "src", "main.go"), "package main\n")
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "init")
	runGit(t, dir, "update-ref", "refs/remotes/origin/main", "HEAD")
	head := gitOutput(dir, "rev-parse", "HEAD")
	repo := Repo{Root: dir, RemoteURL: "https://github.com/example-org/my-app.git", Head: head}

	cfg := baseConfig()
	if !syncGitSeedEnabled(cfg, repo) {
		t.Fatal("seedable repo without includes should use git seed")
	}
	cfg.Sync.Includes = []string{"src"}
	if syncGitSeedEnabled(cfg, repo) {
		t.Fatal("sync.include should disable full-repo git seed")
	}
	cfg.Sync.Includes = []string{" "}
	if !syncGitSeedEnabled(cfg, repo) {
		t.Fatal("blank include entries should not disable git seed")
	}
	cfg.Sync.GitSeed = false
	if syncGitSeedEnabled(cfg, repo) {
		t.Fatal("gitSeed=false should disable git seed")
	}
}

func TestSyncGitSeedRejectsCredentialBearingRemote(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	writeFile(t, filepath.Join(dir, "main.go"), "package main\n")
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "init")
	runGit(t, dir, "update-ref", "refs/remotes/origin/main", "HEAD")
	for _, remoteURL := range []string{
		"https://runner:do-not-forward@example.test/repo.git",
		"ssh://runner:do-not-forward@example.test/repo.git",
		"git+https://runner:do-not-forward@example.test/repo.git",
	} {
		t.Run(remoteURL, func(t *testing.T) {
			repo := Repo{Root: dir, RemoteURL: remoteURL, Head: gitOutput(dir, "rev-parse", "HEAD")}
			if enabled, blocked := syncGitSeedDecision(baseConfig(), repo); enabled || !blocked {
				t.Fatalf("enabled=%v blocked=%v", enabled, blocked)
			}
			if remoteGitSeedCandidate(repo) {
				t.Fatal("credential-bearing remote must not be a seed candidate")
			}
		})
	}
}

func TestGitRemoteURLHasCredentials(t *testing.T) {
	tests := []struct {
		remote string
		want   bool
	}{
		{remote: "https://example.test/repo.git", want: false},
		{remote: "https://runner@example.test/repo.git", want: true},
		{remote: "https://runner:token@example.test/repo.git", want: true},
		{remote: "HTTPS://runner:token@example.test/repo.git", want: true},
		{remote: "https://runner%zz@example.test/repo.git", want: true},
		{remote: "ssh://git@example.test/repo.git", want: false},
		{remote: "ssh://git:token@example.test/repo.git", want: true},
		{remote: "SSH://git:token@example.test/repo.git", want: true},
		{remote: "ssh://git:@example.test/repo.git", want: true},
		{remote: "ssh://git%zz:token@example.test/repo.git", want: true},
		{remote: "git+https://runner:token@example.test/repo.git", want: true},
		{remote: "git@example.test:repo.git", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.remote, func(t *testing.T) {
			if got := gitRemoteURLHasCredentials(tt.remote); got != tt.want {
				t.Fatalf("got=%v want=%v", got, tt.want)
			}
		})
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

func TestCrabboxIgnoreCanReincludeDefaultExcludedSourcePath(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	writeFile(t, filepath.Join(dir, ".crabboxignore"), "!apps/backend/app/connectors/target\n")
	writeFile(t, filepath.Join(dir, "apps", "backend", "app", "connectors", "target", "schemas.py"), "class Schema: ...\n")
	writeFile(t, filepath.Join(dir, "build", "target", "debug.o"), "cache")
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
	if !strings.Contains(got, "apps/backend/app/connectors/target/schemas.py") {
		t.Fatalf("manifest should reinclude source target path: %q", got)
	}
	if strings.Contains(got, "build/target/debug.o") {
		t.Fatalf("manifest should still exclude unrelated target output: %q", got)
	}
}

func TestCrabboxRuntimeExcludesCannotBeReincluded(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	runtimeFiles := []string{
		".crabbox/env/live.env",
		".crabbox/scripts/smoke.sh",
		".crabbox/logs/run.log",
		".crabbox/captures/failure.tgz",
		".crabbox/runs/run_123/artifact.tgz",
	}
	var ignore strings.Builder
	for _, exclude := range protectedSyncExcludes() {
		fmt.Fprintf(&ignore, "!%s\n", exclude)
	}
	writeFile(t, filepath.Join(dir, ".crabboxignore"), ignore.String())
	for _, rel := range runtimeFiles {
		writeFile(t, filepath.Join(dir, filepath.FromSlash(rel)), "runtime state\n")
	}
	writeFile(t, filepath.Join(dir, ".crabbox", "srt-settings.json"), "{}\n")
	writeFile(t, filepath.Join(dir, "src", "main.go"), "package main\n")
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
	for _, want := range []string{".crabbox/srt-settings.json", "src/main.go"} {
		if !strings.Contains(got, want) {
			t.Fatalf("manifest %q missing %q", got, want)
		}
	}
	for _, notWant := range runtimeFiles {
		if strings.Contains(got, notWant) {
			t.Fatalf("manifest %q should not include protected runtime path %q", got, notWant)
		}
		if !pathExcluded(notWant, excludes) {
			t.Fatalf("protected runtime path %q was re-included: %v", notWant, excludes)
		}
	}
	for _, alias := range []string{
		".CRABBOX/env/live.env",
		".crabbox/SCRIPTS/smoke.sh",
		".Crabbox/Logs/run.log",
		".crabbox/CAPTURES/failure.tgz",
		".CRABBOX/RUNS/run_123/artifact.tgz",
	} {
		if !pathExcluded(alias, excludes) {
			t.Fatalf("case alias of protected runtime path %q was re-included: %v", alias, excludes)
		}
	}
}

func TestPathExcludedUsesOrderedNegation(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		patterns []string
		want     bool
	}{
		{name: "exact reinclude", path: "apps/backend/target/schema.py", patterns: []string{"target", "!apps/backend/target"}, want: false},
		{name: "unrelated default remains excluded", path: "build/target/debug.o", patterns: []string{"target", "!apps/backend/target"}, want: true},
		{name: "last matching rule wins", path: "target/debug.o", patterns: []string{"target", "!target", "target"}, want: true},
		{name: "escaped bang is literal", path: "!cache/item.bin", patterns: []string{`\!cache`}, want: true},
		{name: "unescaped bang negates", path: "cache/item.bin", patterns: []string{"cache", "!cache"}, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := pathExcluded(test.path, test.patterns); got != test.want {
				t.Fatalf("pathExcluded(%q, %q) = %v, want %v", test.path, test.patterns, got, test.want)
			}
		})
	}
}

func TestSyncExcludesPreservesRepeatedRuleOrder(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".crabboxignore"), "!target\ntarget\n!apps/backend/target\n")

	excludes, err := syncExcludes(dir, baseConfig())
	if err != nil {
		t.Fatal(err)
	}
	if pathExcluded("apps/backend/target/schema.py", excludes) {
		t.Fatal("final precise negation should reinclude source target path")
	}
	if !pathExcluded("build/target/debug.o", excludes) {
		t.Fatal("repeated target rule should re-exclude unrelated target path")
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
