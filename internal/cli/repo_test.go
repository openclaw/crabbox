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

func TestRepositoryGitEnvironmentExcludesSecretsAndPreservesSafeGitRouting(t *testing.T) {
	t.Setenv("SCREEN_SHARING_PASSWORD", "operator-secret")
	t.Setenv("GIT_CEILING_DIRECTORIES", "/safe/root")
	t.Setenv("GIT_CREDENTIAL_HELPER_TOKEN", "must-not-reach-git")
	env := strings.Join(repositoryGitEnvironment(), "\n")
	if strings.Contains(env, "SCREEN_SHARING_PASSWORD=") {
		t.Fatalf("repository git environment exposed ambient secret: %q", env)
	}
	if !strings.Contains(env, "GIT_CEILING_DIRECTORIES=/safe/root") {
		t.Fatalf("repository git environment lost safe discovery routing: %q", env)
	}
	if strings.Contains(env, "GIT_CREDENTIAL_HELPER_TOKEN=") {
		t.Fatalf("repository git environment exposed ambient Git credential state: %q", env)
	}
}

func TestFindRepoUsesOriginNameInsideLinkedWorktree(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "crabbox")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "init")
	runGit(t, root, "-c", "user.email=test@example.com", "-c", "user.name=Test", "commit", "--allow-empty", "-m", "init")
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

func TestFindRepoNearestRepositoryMarkerWins(t *testing.T) {
	t.Run("native Jujutsu nested in outer Git", func(t *testing.T) {
		outer := t.TempDir()
		runGit(t, outer, "init")
		nativeRoot := filepath.Join(outer, "native")
		makeNativeJujutsuWorkspace(t, nativeRoot)
		workingDir := filepath.Join(nativeRoot, "src")
		if err := os.MkdirAll(workingDir, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Chdir(workingDir)

		repo, err := findRepo()
		if err != nil {
			t.Fatal(err)
		}
		if !sameRepositoryPath(repo.Root, nativeRoot) {
			t.Fatalf("repo root=%q want native Jujutsu root %q", repo.Root, nativeRoot)
		}
	})

	t.Run("colocated Jujutsu and Git", func(t *testing.T) {
		root := t.TempDir()
		runGit(t, root, "init")
		makeNativeJujutsuWorkspace(t, root)
		workingDir := filepath.Join(root, "src")
		if err := os.Mkdir(workingDir, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Chdir(workingDir)

		repo, err := findRepo()
		if err != nil {
			t.Fatal(err)
		}
		if !sameRepositoryPath(repo.Root, root) {
			t.Fatalf("repo root=%q want colocated Git root %q", repo.Root, root)
		}
	})

	t.Run("closer Git nested in outer Jujutsu", func(t *testing.T) {
		outer := t.TempDir()
		makeNativeJujutsuWorkspace(t, outer)
		gitRoot := filepath.Join(outer, "git-workspace")
		if err := os.Mkdir(gitRoot, 0o755); err != nil {
			t.Fatal(err)
		}
		runGit(t, gitRoot, "init")
		workingDir := filepath.Join(gitRoot, "src")
		if err := os.Mkdir(workingDir, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Chdir(workingDir)

		repo, err := findRepo()
		if err != nil {
			t.Fatal(err)
		}
		if !sameRepositoryPath(repo.Root, gitRoot) {
			t.Fatalf("repo root=%q want closer Git root %q", repo.Root, gitRoot)
		}
	})
}

func TestNearestRepositoryBoundaryAcceptsGitFileMarker(t *testing.T) {
	parent := t.TempDir()
	source := filepath.Join(parent, "source")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, source, "init")
	runGit(t, source, "-c", "user.email=test@example.com", "-c", "user.name=Test", "commit", "--allow-empty", "-m", "init")
	worktree := filepath.Join(parent, "worktree")
	runGit(t, source, "worktree", "add", "--detach", worktree)
	makeNativeJujutsuWorkspace(t, worktree)
	boundary, err := nearestRepositoryBoundary(worktree, "")
	if err != nil {
		t.Fatal(err)
	}
	if boundary.kind != repositoryBoundaryGit || !sameRepositoryPath(boundary.root, worktree) {
		t.Fatalf("boundary=%#v, want colocated Git root", boundary)
	}
}

func TestSyncManifestRejectsInvalidColocatedGitMarkerInsideOuterGit(t *testing.T) {
	outer := t.TempDir()
	runGit(t, outer, "init")
	nativeRoot := filepath.Join(outer, "native-workspace")
	makeNativeJujutsuWorkspace(t, nativeRoot)
	if err := os.Mkdir(filepath.Join(nativeRoot, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	workingDir := filepath.Join(nativeRoot, "src")
	if err := os.Mkdir(workingDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(workingDir)

	repo, err := findRepo()
	if err != nil {
		t.Fatal(err)
	}
	if !sameRepositoryPath(repo.Root, nativeRoot) {
		t.Fatalf("repo root=%q want native Jujutsu root %q", repo.Root, nativeRoot)
	}
	_, err = syncManifest(repo.Root, configuredExcludes(baseConfig()))
	if err == nil || !strings.Contains(err.Error(), "native Jujutsu workspace") {
		t.Fatalf("manifest error=%v, want native Jujutsu rejection", err)
	}
}

func TestFindRepoPreservesExplicitGitRoutingInsideNativeJujutsu(t *testing.T) {
	parent := t.TempDir()
	explicitWorktree := filepath.Join(parent, "explicit-worktree")
	if err := os.Mkdir(explicitWorktree, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, explicitWorktree, "init")
	writeFile(t, filepath.Join(explicitWorktree, "tracked.txt"), "tracked\n")
	runGit(t, explicitWorktree, "add", "tracked.txt")

	nativeRoot := filepath.Join(parent, "native-workspace")
	makeNativeJujutsuWorkspace(t, nativeRoot)
	workingDir := filepath.Join(nativeRoot, "src")
	if err := os.Mkdir(workingDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(workingDir)
	relativeGitDir, err := filepath.Rel(workingDir, filepath.Join(explicitWorktree, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	relativeWorktree, err := filepath.Rel(workingDir, explicitWorktree)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_DIR", relativeGitDir)
	t.Setenv("GIT_WORK_TREE", relativeWorktree)

	repo, err := findRepo()
	if err != nil {
		t.Fatal(err)
	}
	if !sameRepositoryPath(repo.Root, explicitWorktree) {
		t.Fatalf("repo root=%q want explicit Git worktree %q", repo.Root, explicitWorktree)
	}
	manifest, err := syncManifest(repo.Root, configuredExcludes(baseConfig()))
	if err != nil {
		t.Fatalf("explicit Git manifest failed: %v", err)
	}
	if strings.Join(manifest.Files, ",") != "tracked.txt" {
		t.Fatalf("manifest files=%v, want explicitly routed Git manifest", manifest.Files)
	}
}

func TestFindRepoHonorsGitDiscoveryCeilingForJujutsuFallback(t *testing.T) {
	outerGit := t.TempDir()
	runGit(t, outerGit, "init")
	nativeRoot := filepath.Join(outerGit, "native-workspace")
	makeNativeJujutsuWorkspace(t, nativeRoot)
	workingDir := filepath.Join(nativeRoot, "work")
	if err := os.Mkdir(workingDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(workingDir)
	t.Setenv("GIT_DIR", "")
	t.Setenv("GIT_WORK_TREE", "")
	t.Setenv("GIT_CEILING_DIRECTORIES", nativeRoot)

	repo, err := findRepo()
	if err != nil {
		t.Fatal(err)
	}
	if !sameRepositoryPath(repo.Root, workingDir) {
		t.Fatalf("repo root=%q want non-repository fallback %q", repo.Root, workingDir)
	}
	_, err = syncManifest(repo.Root, configuredExcludes(baseConfig()))
	if err == nil {
		t.Fatal("expected ordinary non-Git manifest error below discovery ceiling")
	}
	if strings.Contains(err.Error(), "native Jujutsu workspace") {
		t.Fatalf("manifest crossed Git discovery ceiling: %v", err)
	}
	for _, want := range []string{"not a Git repository", "git init", "--no-sync"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("ordinary non-Git error missing %q: %v", want, err)
		}
	}
}

func makeNativeJujutsuWorkspace(t *testing.T, root string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, ".jj"), 0o755); err != nil {
		t.Fatal(err)
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

func TestParseGitTrackedPaths(t *testing.T) {
	raw := []byte(
		"h 100644 aaaa 0\tspace name.txt\x00" +
			"S 120000 bbbb 0\ttab\tname\n.txt\x00" +
			"M 100644 cccc 1\tconflict.txt\x00" +
			"M 100755 dddd 2\tconflict.txt\x00" +
			"H 160000 eeee 0\tvendor/submodule\x00" +
			"s 100644 ffff 0\thidden.txt\x00",
	)
	got, err := parseGitTrackedPaths(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 6 {
		t.Fatalf("tracked=%#v", got)
	}
	if got[0].name != "space name.txt" || got[0].mode != "100644" || got[0].stage != 0 || got[0].skipWorktree || !got[0].assumeUnchanged {
		t.Fatalf("regular=%#v", got[0])
	}
	if got[1].name != "tab\tname\n.txt" || got[1].mode != "120000" || got[1].stage != 0 || !got[1].skipWorktree {
		t.Fatalf("symlink=%#v", got[1])
	}
	if got[2].name != "conflict.txt" || got[2].stage != 1 ||
		got[3].name != "conflict.txt" || got[3].mode != "100755" || got[3].stage != 2 {
		t.Fatalf("unmerged=%#v", got[2:4])
	}
	if got[4].mode != "160000" || got[4].stage != 0 {
		t.Fatalf("gitlink=%#v", got[4])
	}
	if got[5].name != "hidden.txt" || !got[5].skipWorktree || !got[5].assumeUnchanged {
		t.Fatalf("combined index flags=%#v", got[5])
	}
}

func TestParseGitTrackedPathsRejectsMalformedMetadata(t *testing.T) {
	for _, raw := range []string{
		"H 100644 aaaa 0 missing-tab\x00",
		"H invalid aaaa 0\tfile.txt\x00",
		"H 100644 aaaa 4\tfile.txt\x00",
		"H 100644 aaaa 0\t\x00",
	} {
		t.Run(fmt.Sprintf("%q", raw), func(t *testing.T) {
			if _, err := parseGitTrackedPaths([]byte(raw)); err == nil {
				t.Fatal("malformed metadata was accepted")
			}
		})
	}
}

func TestParseGitCachedDeletions(t *testing.T) {
	raw := []byte(
		":100644 000000 aaaa 0000 D\x00space name.txt\x00" +
			":160000 000000 bbbb 0000 D\x00vendor/tab\tname\nmodule\x00",
	)
	got, err := parseGitCachedDeletions(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].name != "space name.txt" || got[0].preimageMode != "100644" ||
		got[1].name != "vendor/tab\tname\nmodule" || got[1].preimageMode != "160000" {
		t.Fatalf("deletions=%#v", got)
	}
	for _, raw := range [][]byte{
		[]byte(":160000 000000 aaaa 0000 D\x00missing-name"),
		[]byte(":invalid 000000 aaaa 0000 D\x00module\x00"),
		[]byte(":160000 000000 aaaa 0000 M\x00module\x00"),
	} {
		if _, err := parseGitCachedDeletions(raw); err == nil {
			t.Fatalf("malformed staged deletion metadata accepted: %q", raw)
		}
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
	runGit(t, dir, "update-index", "--assume-unchanged", "included/keep.txt")
	if omitted, err := GitCheckoutHasHiddenOmissions(dir); err != nil || !omitted {
		t.Error("dense checkout missed absent skip-worktree path marked assume-unchanged")
	}
	if _, err := syncManifestFiltered(dir, nil, nil); err == nil || !strings.Contains(err.Error(), "skip-worktree") {
		t.Errorf("combined index flags bypassed manifest omission guard: %v", err)
	}
}

func TestGitCheckoutHiddenOmissionAppliesScopeBeforeClassification(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	writeFile(t, filepath.Join(dir, "scoped.txt"), "scoped\n")
	writeFile(t, filepath.Join(dir, "outside.txt"), "outside\n")
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "init")
	runGit(t, dir, "sparse-checkout", "set", "--no-cone", "/*")
	if err := os.Remove(filepath.Join(dir, "scoped.txt")); err != nil {
		t.Fatal(err)
	}

	resolverCalls := 0
	rulesUnavailable := func(string, []gitTrackedPath) (map[string]struct{}, error) {
		resolverCalls++
		return nil, fmt.Errorf("check-rules unsupported")
	}
	if path, err := gitCheckoutHiddenOmission(dir, func(gitTrackedPath) bool { return false }, rulesUnavailable); err != nil || path != "" {
		t.Fatalf("empty scope path=%q err=%v", path, err)
	}
	if resolverCalls != 0 {
		t.Fatalf("resolver calls=%d, want 0", resolverCalls)
	}
	if path, err := gitCheckoutHiddenOmission(dir, func(entry gitTrackedPath) bool {
		return entry.name == "scoped.txt"
	}, rulesUnavailable); path != "" || err == nil || !strings.Contains(err.Error(), "Git 2.41") {
		t.Fatalf("old Git ambiguity path=%q err=%v", path, err)
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

func TestSyncManifestRejectsOnlyHiddenPathsInEffectiveScope(t *testing.T) {
	tests := []struct {
		name      string
		excludes  []string
		includes  []string
		wantError bool
	}{
		{name: "in scope", wantError: true},
		{name: "outside include", includes: []string{"visible"}},
		{name: "excluded", excludes: []string{"hidden"}},
		{name: "ordered reinclude", excludes: []string{"hidden", "!hidden/drop.txt"}, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			runGit(t, dir, "init")
			runGit(t, dir, "config", "user.email", "test@example.com")
			runGit(t, dir, "config", "user.name", "Test")
			writeFile(t, filepath.Join(dir, "visible", "keep.txt"), "keep\n")
			writeFile(t, filepath.Join(dir, "hidden", "drop.txt"), "drop\n")
			runGit(t, dir, "add", ".")
			runGit(t, dir, "commit", "-m", "init")
			runGit(t, dir, "sparse-checkout", "set", "visible")

			manifest, err := syncManifestFiltered(dir, test.excludes, test.includes)
			if test.wantError {
				if err == nil || !strings.Contains(err.Error(), `tracked path "hidden/drop.txt"`) {
					t.Fatalf("manifest=%#v err=%v", manifest, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(manifest.Files, ",") != "visible/keep.txt" {
				t.Fatalf("manifest files=%v", manifest.Files)
			}
		})
	}
}

func TestSyncManifestAllowsIntentionalDeletionInMaterializedSparseCheckout(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	writeFile(t, filepath.Join(dir, "keep.txt"), "keep\n")
	writeFile(t, filepath.Join(dir, "deleted.txt"), "delete\n")
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "init")
	runGit(t, dir, "sparse-checkout", "set", "--no-cone", "/*")

	if _, err := syncManifestFiltered(dir, nil, nil); err != nil {
		t.Fatalf("fully materialized sparse checkout: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "deleted.txt")); err != nil {
		t.Fatal(err)
	}
	manifest, err := syncManifestFiltered(dir, nil, nil)
	if err != nil {
		if strings.Contains(err.Error(), "Git 2.41") {
			t.Skipf("intentional deletion classification requires Git 2.41+: %v", err)
		}
		t.Fatal(err)
	}
	if strings.Join(manifest.Deleted, ",") != "deleted.txt" {
		t.Fatalf("deleted=%v", manifest.Deleted)
	}
}

func TestSyncManifestTreatsSymlinksAsFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink fixture requires Unix semantics")
	}
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	writeFile(t, filepath.Join(dir, "visible", "keep.txt"), "keep\n")
	if err := os.MkdirAll(filepath.Join(dir, "hidden"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing-target", filepath.Join(dir, "hidden", "link.txt")); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "init")
	runGit(t, dir, "sparse-checkout", "set", "--no-cone", "/*")
	runGit(t, dir, "update-index", "--skip-worktree", "hidden/link.txt")

	manifest, err := syncManifestFiltered(dir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(manifest.Files, ","), "hidden/link.txt") {
		t.Fatalf("manifest files=%v", manifest.Files)
	}

	runGit(t, dir, "update-index", "--no-skip-worktree", "hidden/link.txt")
	runGit(t, dir, "sparse-checkout", "set", "visible")
	if _, err := syncManifestFiltered(dir, nil, nil); err == nil || !strings.Contains(err.Error(), "hidden/link.txt") {
		t.Fatalf("absent sparse symlink err=%v", err)
	}
}

func TestSyncManifestIgnoresGitlinksButWholeCheckoutGuardDoesNot(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	writeFile(t, filepath.Join(dir, "visible", "keep.txt"), "keep\n")
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "base")
	head := gitOutput(dir, "rev-parse", "HEAD")
	runGit(t, dir, "update-index", "--add", "--cacheinfo", "160000,"+head+",vendor/submodule")
	runGit(t, dir, "commit", "-m", "gitlink")
	runGit(t, dir, "sparse-checkout", "set", "visible")

	manifest, err := syncManifestFiltered(dir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(manifest.Files, ",") != "visible/keep.txt" || len(manifest.Deleted) != 0 {
		t.Fatalf("manifest=%#v", manifest)
	}
	if omitted, err := GitCheckoutHasHiddenOmissions(dir); err != nil || !omitted {
		t.Fatalf("whole checkout omission=%v err=%v", omitted, err)
	}
}

func TestSyncManifestRejectsMixedFileGitlinkConflictInEffectiveScope(t *testing.T) {
	tests := []struct {
		name      string
		excludes  []string
		includes  []string
		wantError bool
	}{
		{name: "in scope", wantError: true},
		{name: "outside include", includes: []string{"visible.txt"}},
		{name: "excluded", excludes: []string{"conflict.txt"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			runGit(t, dir, "init")
			runGit(t, dir, "config", "user.email", "test@example.com")
			runGit(t, dir, "config", "user.name", "Test")
			writeFile(t, filepath.Join(dir, "visible.txt"), "visible\n")
			writeFile(t, filepath.Join(dir, "conflict.txt"), "conflict\n")
			runGit(t, dir, "add", ".")
			runGit(t, dir, "commit", "-m", "base")
			setUnmergedIndexModes(t, dir, "conflict.txt", "100644", "160000", "100644")

			manifest, err := syncManifestFiltered(dir, test.excludes, test.includes)
			if test.wantError {
				if err == nil ||
					!strings.Contains(err.Error(), `tracked path "conflict.txt"`) ||
					!strings.Contains(err.Error(), "file mode 100644 at stage 1") ||
					!strings.Contains(err.Error(), "gitlink mode 160000 at stage 2") {
					t.Fatalf("manifest=%#v err=%v", manifest, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(manifest.Files, ",") != "visible.txt" {
				t.Fatalf("manifest files=%v", manifest.Files)
			}
		})
	}
}

func TestSyncManifestKeepsFileLikeConflict(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	writeFile(t, filepath.Join(dir, "conflict.txt"), "conflict\n")
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "base")
	setUnmergedIndexModes(t, dir, "conflict.txt", "100644", "100755", "100644")

	manifest, err := syncManifestFiltered(dir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(manifest.Files, ",") != "conflict.txt" {
		t.Fatalf("manifest files=%v", manifest.Files)
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

func TestSyncManifestNonGitWorkdirReturnsActionableError(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "main.txt"), "hello\n")

	_, err := syncManifest(dir, configuredExcludes(baseConfig()))
	if err == nil {
		t.Fatal("expected error for non-Git workdir, got nil")
	}
	msg := err.Error()
	if strings.Contains(msg, "exit status") {
		t.Fatalf("error should not surface an opaque process status: %q", msg)
	}
	if !strings.Contains(msg, dir) {
		t.Fatalf("error should name the workdir %q: %q", dir, msg)
	}
	if !strings.Contains(msg, "not a Git repository") {
		t.Fatalf("error should identify the non-Git cause: %q", msg)
	}
	for _, want := range []string{"git init", "--no-sync"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error should suggest %q: %q", want, msg)
		}
	}
}

func TestSyncManifestNativeJujutsuReturnsActionableError(t *testing.T) {
	outer := t.TempDir()
	runGit(t, outer, "init")
	root := filepath.Join(outer, "native-workspace")
	makeNativeJujutsuWorkspace(t, root)
	writeFile(t, filepath.Join(root, "main.txt"), "hello\n")

	_, err := syncManifest(root, configuredExcludes(baseConfig()))
	if err == nil {
		t.Fatal("expected error for native Jujutsu workspace, got nil")
	}
	msg := err.Error()
	for _, want := range []string{
		root,
		"native Jujutsu workspace",
		"Git-manifest-based",
		"native Jujutsu sync is not supported yet",
		"wrong revision",
		"colocated Git workspace",
		"from an existing Git checkout",
		"jj git init --git-repo=.",
		"does not convert the current native workspace in place",
		"--no-sync",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error should include %q: %q", want, msg)
		}
	}
}

func TestSyncManifestColocatedJujutsuUsesGitManifest(t *testing.T) {
	root := t.TempDir()
	runGit(t, root, "init")
	makeNativeJujutsuWorkspace(t, root)
	writeFile(t, filepath.Join(root, "tracked.txt"), "tracked\n")
	runGit(t, root, "add", "tracked.txt")

	manifest, err := syncManifest(root, configuredExcludes(baseConfig()))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(manifest.Files, ",") != "tracked.txt" {
		t.Fatalf("manifest files=%v, want ordinary Git manifest", manifest.Files)
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

	manifest, err := syncManifestFilteredRules(dir, configuredExcludes(baseConfig()), []string{"src", "scripts", "package.json"})
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
	if plan, _ := syncGitCoherencePlan(cfg, repo); !plan.seedEnabled() {
		t.Fatal("seedable repo without includes should use git seed")
	}
	cfg.Sync.Includes = []string{"src"}
	if plan, _ := syncGitCoherencePlan(cfg, repo); plan.seedEnabled() {
		t.Fatal("sync.include should disable full-repo git seed")
	}
	cfg.Sync.Includes = []string{" "}
	if plan, _ := syncGitCoherencePlan(cfg, repo); !plan.seedEnabled() {
		t.Fatal("blank include entries should not disable git seed")
	}
	cfg.Sync.GitSeed = false
	if plan, _ := syncGitCoherencePlan(cfg, repo); plan.seedEnabled() {
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
			if plan, blocked := syncGitCoherencePlan(baseConfig(), repo); plan.seedEnabled() || !blocked {
				t.Fatalf("plan=%#v blocked=%v", plan, blocked)
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
	writeFile(t, filepath.Join(dir, "apps", "foo", "src", "main.go"), "package main\n")
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "init")
	writeFile(t, filepath.Join(dir, "playwright-report", "index.html"), "cache")
	writeFile(t, filepath.Join(dir, "apps", "foo", ".build", "debug.o"), "cache")

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

func TestSyncManifestProtectsTrackedFilesFromAmbiguousBuiltInExcludes(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	ambiguous := []string{"dist", "dist-runtime", "coverage", "playwright-report", "test-results", ".build", "target"}
	var tracked []string
	for _, component := range ambiguous {
		for _, rel := range []string{
			component + "/root.txt",
			"packages/app/" + component + "/nested.txt",
		} {
			writeFile(t, filepath.Join(dir, filepath.FromSlash(rel)), "tracked\n")
			tracked = append(tracked, rel)
		}
	}
	for _, rel := range []string{
		"vendor/node_modules/pkg/index.js",
		"nested/.cache/state.bin",
		"python/.venv/lib.py",
		"python/__pycache__/module.pyc",
	} {
		writeFile(t, filepath.Join(dir, filepath.FromSlash(rel)), "tracked cache\n")
	}
	writeFile(t, filepath.Join(dir, "src", "main.go"), "package main\n")
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "init")
	for _, component := range ambiguous {
		writeFile(t, filepath.Join(dir, "generated", component, "output.bin"), "untracked output\n")
	}

	manifest, err := syncManifest(dir, configuredExcludes(baseConfig()))
	if err != nil {
		t.Fatal(err)
	}
	files := strings.Join(manifest.Files, ",")
	for _, rel := range tracked {
		if !strings.Contains(files, rel) {
			t.Errorf("manifest missing protected tracked path %q: %s", rel, files)
		}
	}
	for _, component := range ambiguous {
		rel := "generated/" + component + "/output.bin"
		if strings.Contains(files, rel) {
			t.Errorf("manifest included untracked generated path %q", rel)
		}
	}
	for _, rel := range []string{"vendor/node_modules/pkg/index.js", "nested/.cache/state.bin", "python/.venv/lib.py", "python/__pycache__/module.pyc"} {
		if strings.Contains(files, rel) {
			t.Errorf("manifest included tracked dependency/cache path %q", rel)
		}
	}
	if got, want := len(manifest.ProtectedTrackedExcludes), len(tracked); got != want {
		t.Fatalf("protected annotations=%d want %d: %+v", got, want, manifest.ProtectedTrackedExcludes)
	}
}

func TestSyncManifestConfiguredExcludesRemainAuthoritativeForTrackedArtifacts(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	for _, rel := range []string{"target/drop.txt", "target/keep.txt", "coverage/drop.txt", "coverage/keep.txt", "dist/repo-excluded.txt"} {
		writeFile(t, filepath.Join(dir, filepath.FromSlash(rel)), "tracked\n")
	}
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "init")
	writeFile(t, filepath.Join(dir, ".crabboxignore"), "dist\n")

	cfg := baseConfig()
	cfg.Sync.Excludes = []string{"target", "!target/keep.txt", "coverage/drop.txt"}
	rules, err := syncExcludes(dir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := syncManifest(dir, rules)
	if err != nil {
		t.Fatal(err)
	}
	files := strings.Join(manifest.Files, ",")
	for _, want := range []string{"target/keep.txt", "coverage/keep.txt"} {
		if !strings.Contains(files, want) {
			t.Errorf("manifest missing %q after ordered re-include: %s", want, files)
		}
	}
	for _, excluded := range []string{"target/drop.txt", "coverage/drop.txt", "dist/repo-excluded.txt"} {
		if strings.Contains(files, excluded) {
			t.Errorf("manifest ignored configured exclude for %q: %s", excluded, files)
		}
	}
	if got := len(manifest.ProtectedTrackedExcludes); got != 1 || manifest.ProtectedTrackedExcludes[0].Path != "coverage/keep.txt" {
		t.Fatalf("protected annotations=%+v, want only coverage/keep.txt", manifest.ProtectedTrackedExcludes)
	}
}

func TestSyncManifestIncludeWhitelistScopesTrackedProtection(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	writeFile(t, filepath.Join(dir, "target", "keep.txt"), "tracked\n")
	writeFile(t, filepath.Join(dir, "dist", "outside.txt"), "tracked\n")
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "init")

	manifest, err := syncManifestFilteredRules(dir, configuredExcludes(baseConfig()), []string{"target"})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(manifest.Files, ","); got != "target/keep.txt" {
		t.Fatalf("manifest files=%q", got)
	}
	if got := manifest.ProtectedTrackedExcludes; len(got) != 1 || got[0].Path != "target/keep.txt" || got[0].Pattern != "target" {
		t.Fatalf("protected annotations=%+v", got)
	}
}

func TestSyncManifestTrackedAmbiguousDeletionFollowsEffectiveRules(t *testing.T) {
	for _, staged := range []bool{false, true} {
		t.Run(fmt.Sprintf("staged=%t", staged), func(t *testing.T) {
			dir := t.TempDir()
			runGit(t, dir, "init")
			runGit(t, dir, "config", "user.email", "test@example.com")
			runGit(t, dir, "config", "user.name", "Test")
			writeFile(t, filepath.Join(dir, "target", "obsolete.txt"), "tracked\n")
			runGit(t, dir, "add", ".")
			runGit(t, dir, "commit", "-m", "init")
			if staged {
				runGit(t, dir, "rm", "target/obsolete.txt")
			} else if err := os.Remove(filepath.Join(dir, "target", "obsolete.txt")); err != nil {
				t.Fatal(err)
			}

			manifest, err := syncManifest(dir, configuredExcludes(baseConfig()))
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(manifest.Deleted, ","); got != "target/obsolete.txt" {
				t.Fatalf("default deleted=%q", got)
			}
			if got := strings.Join(manifest.Changed, ","); got != "target/obsolete.txt" {
				t.Fatalf("default dirty delta=%q", got)
			}

			cfg := baseConfig()
			cfg.Sync.Excludes = []string{"target"}
			manifest, err = syncManifest(dir, configuredExcludes(cfg))
			if err != nil {
				t.Fatal(err)
			}
			if len(manifest.Deleted) != 0 || len(manifest.Changed) != 0 {
				t.Fatalf("configured exclude should omit deletion and dirty delta: %+v", manifest)
			}
		})
	}
}

func TestSyncFingerprintIncludesExcludeRuleProvenance(t *testing.T) {
	plan := gitCoherencePlan{RemoteURL: "https://example.test/repo.git", Target: "target", Tree: "tree", Branch: "main"}
	builtIn := newSyncExcludeRules([]string{"target"}, syncExcludeBuiltIn)
	configured := newSyncExcludeRules([]string{"target"}, syncExcludeConfigured)
	a, err := syncFingerprintForManifest(Repo{Root: t.TempDir()}, baseConfig(), SyncManifest{}, builtIn, plan)
	if err != nil {
		t.Fatal(err)
	}
	b, err := syncFingerprintForManifest(Repo{Root: t.TempDir()}, baseConfig(), SyncManifest{}, configured, plan)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatalf("fingerprint did not distinguish built-in and configured provenance: %q", a)
	}
}

func TestSyncFingerprintHashesChangedSymlinkIdentity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink fixture requires Unix semantics")
	}
	for _, overlay := range []bool{false, true} {
		for _, targetKind := range []string{"file", "directory", "missing"} {
			t.Run(fmt.Sprintf("overlay=%t/%s", overlay, targetKind), func(t *testing.T) {
				root := t.TempDir()
				for _, name := range []string{"target-a", "target-b"} {
					switch targetKind {
					case "file":
						writeFile(t, filepath.Join(root, name), "identical target contents\n")
					case "directory":
						if err := os.Mkdir(filepath.Join(root, name), 0o755); err != nil {
							t.Fatal(err)
						}
					}
				}
				link := filepath.Join(root, "link")
				if err := os.Symlink("target-a", link); err != nil {
					t.Fatal(err)
				}
				cfg := baseConfig()
				cfg.Sync.GitOverlay = overlay
				manifest := SyncManifest{Files: []string{"link"}, Changed: []string{"link"}}
				plan := gitCoherencePlan{RemoteURL: "https://example.test/repo.git", Target: "target", Tree: "tree", Branch: "main"}
				fingerprint := func() string {
					t.Helper()
					value, err := syncFingerprintForManifest(Repo{Root: root}, cfg, manifest, SyncExcludeRules{}, plan)
					if err != nil {
						t.Fatalf("fingerprint changed %s symlink: %v", targetKind, err)
					}
					return value
				}
				before := fingerprint()
				if err := os.Remove(link); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("target-b", link); err != nil {
					t.Fatal(err)
				}
				after := fingerprint()
				if before == after {
					t.Error("retargeted symlink kept the same fingerprint")
				}
				if targetKind == "file" {
					writeFile(t, filepath.Join(root, "target-b"), "changed target contents\n")
					if got := fingerprint(); got != after {
						t.Error("fingerprint followed symlink target contents outside the manifest")
					}
				}
			})
		}
	}
}

func TestSyncExcludeRuleUpgradeCompatibilityPreservesRenderedOrder(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".crabboxignore"), "coverage\n!coverage/keep.txt\n")
	cfg := baseConfig()
	cfg.Sync.Excludes = []string{"target", "!target/keep.txt"}

	rules, err := syncExcludes(dir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := appendOrderedStrings(defaultExcludes(), cfg.Sync.Excludes...)
	want = appendOrderedStrings(want, "coverage", "!coverage/keep.txt")
	want = appendOrderedStrings(want, protectedSyncExcludes()...)
	if got := strings.Join(rules.patterns(), "\n"); got != strings.Join(want, "\n") {
		t.Fatalf("rendered rule order changed across provenance upgrade:\n%s", got)
	}
	if !pathExcludedByRules("target/drop.txt", rules, true) || pathExcludedByRules("target/keep.txt", rules, true) {
		t.Fatal("ordered configured target rules lost authority")
	}
	if !pathExcludedByRules("coverage/drop.txt", rules, true) || pathExcludedByRules("coverage/keep.txt", rules, true) {
		t.Fatal("ordered .crabboxignore coverage rules lost authority")
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

func TestCrabboxIgnoreCanReincludeDefaultExcludedUntrackedPath(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	writeFile(t, filepath.Join(dir, ".crabboxignore"), "!apps/backend/app/connectors/target\n")
	runGit(t, dir, "add", ".crabboxignore")
	runGit(t, dir, "commit", "-m", "init")
	writeFile(t, filepath.Join(dir, "apps", "backend", "app", "connectors", "target", "schemas.py"), "class Schema: ...\n")
	writeFile(t, filepath.Join(dir, "build", "target", "debug.o"), "cache")

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
		if !pathExcludedByRules(notWant, excludes, true) {
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
		if !pathExcludedByRules(alias, excludes, true) {
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
	if pathExcludedByRules("apps/backend/target/schema.py", excludes, true) {
		t.Fatal("final precise negation should reinclude source target path")
	}
	if !pathExcludedByRules("build/target/debug.o", excludes, false) {
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

func TestSyncManifestDoesNotDeleteStagedGitlink(t *testing.T) {
	tests := []struct {
		name     string
		excludes []string
		includes []string
		want     string
	}{
		{name: "effective scope", want: "deleted.txt"},
		{name: "gitlink only", includes: []string{"vendor/submodule"}},
		{name: "outside include", includes: []string{"visible.txt"}},
		{name: "ordinary deletion only", includes: []string{"deleted.txt"}, want: "deleted.txt"},
		{name: "excluded", excludes: []string{"vendor/submodule"}, want: "deleted.txt"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			runGit(t, dir, "init")
			runGit(t, dir, "config", "user.email", "test@example.com")
			runGit(t, dir, "config", "user.name", "Test")
			writeFile(t, filepath.Join(dir, "visible.txt"), "visible\n")
			writeFile(t, filepath.Join(dir, "deleted.txt"), "delete\n")
			runGit(t, dir, "add", ".")
			runGit(t, dir, "commit", "-m", "base")
			head := gitOutput(dir, "rev-parse", "HEAD")
			runGit(t, dir, "update-index", "--add", "--cacheinfo", "160000,"+head+",vendor/submodule")
			runGit(t, dir, "commit", "-m", "gitlink")
			runGit(t, dir, "rm", "--cached", "vendor/submodule")
			if err := os.Remove(filepath.Join(dir, "deleted.txt")); err != nil {
				t.Fatal(err)
			}

			tracked, err := loadGitTrackedPaths(dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range tracked {
				if entry.name == "vendor/submodule" {
					t.Fatalf("staged gitlink deletion retained current index entry: %#v", entry)
				}
			}
			manifest, err := syncManifestFiltered(dir, test.excludes, test.includes)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(manifest.Deleted, ","); got != test.want {
				t.Fatalf("deleted=%q want %q", got, test.want)
			}
		})
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
	if plan, _ := syncGitCoherencePlan(baseConfig(), repo); plan.enabled() {
		t.Fatal("unpublished head should not be a seed candidate")
	}
	runGit(t, dir, "update-ref", "refs/remotes/origin/main", head)
	if plan, _ := syncGitCoherencePlan(baseConfig(), repo); !plan.enabled() {
		t.Fatal("head in a remote-tracking ref should be a seed candidate")
	}
}

func TestSyncGitCoherencePlanSelectsEligibleOriginBranch(t *testing.T) {
	newRepo := func(t *testing.T) string {
		t.Helper()
		dir := t.TempDir()
		runGit(t, dir, "init")
		runGit(t, dir, "config", "user.email", "test@example.com")
		runGit(t, dir, "config", "user.name", "Test")
		writeFile(t, filepath.Join(dir, "plain.txt"), "plain\n")
		runGit(t, dir, "add", ".")
		runGit(t, dir, "commit", "-m", "init")
		runGit(t, dir, "update-ref", "refs/remotes/origin/main", "HEAD")
		return dir
	}
	planFor := func(dir, target, baseRef string) gitCoherencePlan {
		plan, _ := syncGitCoherencePlan(baseConfig(), Repo{
			Root:      dir,
			RemoteURL: "https://example.test/repo.git",
			Head:      target,
			BaseRef:   baseRef,
		})
		return plan
	}
	requireSeedOnly := func(t *testing.T, plan gitCoherencePlan) {
		t.Helper()
		if !plan.seedEnabled() || plan.enabled() {
			t.Fatalf("plan should seed without coherence: %#v", plan)
		}
		fingerprint, _ := syncFingerprintForManifest(Repo{}, baseConfig(), SyncManifest{}, SyncExcludeRules{}, plan)
		if fingerprint != "" {
			t.Fatalf("ineligible coherence published fingerprint %q", fingerprint)
		}
		if remoteGitSeed("/work/repo", plan) == "true" {
			t.Fatal("ineligible coherence disabled Git seed")
		}
	}

	t.Run("exact base ref wins with multiple containing refs", func(t *testing.T) {
		dir := newRepo(t)
		runGit(t, dir, "update-ref", "refs/remotes/origin/release", "HEAD")
		plan := planFor(dir, gitOutput(dir, "rev-parse", "HEAD"), "release")
		if !plan.enabled() || plan.Branch != "release" {
			t.Fatalf("exact BaseRef plan=%#v", plan)
		}
	})

	t.Run("origin HEAD exact tip fallback", func(t *testing.T) {
		dir := newRepo(t)
		runGit(t, dir, "update-ref", "refs/remotes/origin/trunk", "HEAD")
		runGit(t, dir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/trunk")
		plan := planFor(dir, gitOutput(dir, "rev-parse", "HEAD"), "missing")
		if !plan.enabled() || plan.Branch != "trunk" {
			t.Fatalf("origin/HEAD plan=%#v", plan)
		}
	})

	t.Run("deterministic containing ref fallback", func(t *testing.T) {
		dir := newRepo(t)
		target := gitOutput(dir, "rev-parse", "HEAD")
		writeFile(t, filepath.Join(dir, "later.txt"), "later\n")
		runGit(t, dir, "add", ".")
		runGit(t, dir, "commit", "-m", "later")
		runGit(t, dir, "update-ref", "refs/remotes/origin/zeta", "HEAD")
		runGit(t, dir, "update-ref", "refs/remotes/origin/alpha", "HEAD")
		runGit(t, dir, "update-ref", "-d", "refs/remotes/origin/main")
		plan := planFor(dir, target, "")
		if !plan.enabled() || plan.Branch != "alpha" {
			t.Fatalf("deterministic containing-ref plan=%#v", plan)
		}
	})

	t.Run("filter managed path", func(t *testing.T) {
		dir := newRepo(t)
		writeFile(t, filepath.Join(dir, ".gitattributes"), "*.bin filter=fixture -text\n")
		writeFile(t, filepath.Join(dir, "asset.bin"), "payload\n")
		runGit(t, dir, "-c", "filter.fixture.clean=cat", "-c", "filter.fixture.required=false", "add", ".")
		runGit(t, dir, "commit", "-m", "filter")
		runGit(t, dir, "update-ref", "refs/remotes/origin/main", "HEAD")
		requireSeedOnly(t, planFor(dir, gitOutput(dir, "rev-parse", "HEAD"), ""))
	})

	t.Run("gitlink", func(t *testing.T) {
		dir := newRepo(t)
		head := gitOutput(dir, "rev-parse", "HEAD")
		runGit(t, dir, "update-index", "--add", "--cacheinfo", "160000,"+head+",nested")
		runGit(t, dir, "commit", "-m", "gitlink")
		runGit(t, dir, "update-ref", "refs/remotes/origin/main", "HEAD")
		requireSeedOnly(t, planFor(dir, gitOutput(dir, "rev-parse", "HEAD"), ""))
	})
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

func setUnmergedIndexModes(t *testing.T, dir, rel string, modes ...string) {
	t.Helper()
	runGit(t, dir, "update-index", "--force-remove", "--", rel)
	blob := gitOutput(dir, "hash-object", "--", rel)
	commit := gitOutput(dir, "rev-parse", "HEAD")
	var input strings.Builder
	for i, mode := range modes {
		object := blob
		if mode == "160000" {
			object = commit
		}
		fmt.Fprintf(&input, "%s %s %d\t%s\n", mode, object, i+1, rel)
	}
	cmd := exec.Command("git", "update-index", "--index-info")
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(input.String())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create unmerged index for %q: %v\n%s", rel, err, out)
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
