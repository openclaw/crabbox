package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	jjsource "github.com/openclaw/crabbox/tools/jj-source"
)

func TestSyncFingerprintPathsPreservesContentEncoding(t *testing.T) {
	root := t.TempDir()
	const content = "ordinary fingerprint content\n"
	path := filepath.Join(root, "source.txt")
	writeFile(t, path, content)
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	want := sha256.New()
	fmt.Fprintf(want, "path=source.txt\nmode=%s size=%d\n", info.Mode().String(), info.Size())
	want.Write([]byte(content))
	want.Write([]byte{0})
	got := sha256.New()
	if err := syncFingerprintPaths(context.Background(), got, root, []string{"source.txt"}, true); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Sum(nil), want.Sum(nil)) {
		t.Fatal("stable fingerprint encoding changed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := syncFingerprintPaths(ctx, sha256.New(), root, []string{"source.txt"}, true); !errors.Is(err, context.Canceled) {
		t.Fatalf("fingerprint ignored cancellation: %v", err)
	}
}

func TestSyncFingerprintPathsCancellationDuringContent(t *testing.T) {
	root := t.TempDir()
	content := strings.Repeat("x", 256*1024)
	writeFile(t, filepath.Join(root, "source.txt"), content)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := &cancelSourceFingerprintHash{Hash: sha256.New(), cancel: cancel}
	err := syncFingerprintPaths(ctx, h, root, []string{"source.txt"}, true)
	if !errors.Is(err, context.Canceled) || h.chunks != 1 || h.bytes <= 0 || h.bytes >= len(content) {
		t.Fatalf("canceled=%t payload_chunks=%d payload_bytes=%d error=%v", errors.Is(err, context.Canceled), h.chunks, h.bytes, err)
	}
	t.Logf("canceled=true; payload_chunks=%d; payload_bytes=%d; total_bytes=%d", h.chunks, h.bytes, len(content))
}

func TestJJSourceContentDigestCanonicalizesSymlinkMetadata(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "source.txt"), "content\n")
	link := filepath.Join(root, "link")
	if err := os.Symlink("source.txt", link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink creation unavailable: %v", err)
		}
		t.Fatal(err)
	}
	got, err := jjSourceContentDigest(context.Background(), root, []string{"link"})
	if err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256([]byte("path=link\nmode=Lrwxrwxrwx size=10\nsymlink=source.txt\n\x00"))
	if got != fmt.Sprintf("%x", want) {
		t.Fatalf("native symlink checksum=%s, want canonical %x", got, want)
	}
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	legacyWant := sha256.Sum256([]byte(fmt.Sprintf("path=link\nmode=%s size=%d\nsymlink=source.txt\n\x00", info.Mode().String(), info.Size())))
	legacy := sha256.New()
	if err := syncFingerprintPaths(context.Background(), legacy, root, []string{"link"}, true); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(legacy.Sum(nil), legacyWant[:]) {
		t.Fatal("filesystem sync fingerprint changed its existing symlink encoding")
	}
}

type cancelSourceFingerprintHash struct {
	hash.Hash
	cancel        func()
	chunks, bytes int
}

func (h *cancelSourceFingerprintHash) Write(data []byte) (int, error) {
	n, err := h.Hash.Write(data)
	if len(data) > 1024 && data[0] == 'x' {
		h.chunks++
		h.bytes += n
		h.cancel()
	}
	return n, err
}

func TestManagedStateSyncExclusionIsLiteralAndProtected(t *testing.T) {
	root := t.TempDir()
	runGit(t, root, "init")
	runGit(t, root, "config", "user.email", "test@example.com")
	runGit(t, root, "config", "user.name", "Test")
	base := "state [cache]"
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, base))
	managed := base + "/crabbox/marker.txt"
	deleted := base + "/crabbox/old-marker.txt"
	for _, path := range []string{managed, deleted, base + "/source.txt", base + "/old-source.txt", "source.txt"} {
		writeFile(t, filepath.Join(root, filepath.FromSlash(path)), "benign marker\n")
	}
	runGit(t, root, "add", ".")
	runGit(t, root, "commit", "-m", "markers")
	writeFile(t, filepath.Join(root, filepath.FromSlash(managed)), "updated marker\n")
	writeFile(t, filepath.Join(root, base, "crabbox", "new-marker.txt"), "new marker\n")
	writeFile(t, filepath.Join(root, base, "source.txt"), "updated source marker\n")
	if err := os.Remove(filepath.Join(root, filepath.FromSlash(deleted))); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, base, "old-source.txt")); err != nil {
		t.Fatal(err)
	}
	cfg := baseConfig()
	cfg.Sync.Excludes = []string{"!**"}
	writeFile(t, filepath.Join(root, ".crabboxignore"), "!**\n")
	rules, err := syncExcludes(root, cfg)
	if err != nil {
		t.Fatal(err)
	}
	includes := []string{base, "source.txt"}
	if !pathIncluded(managed, includes) {
		t.Fatal("include control must admit the entire managed subtree before protection")
	}
	manifest, err := syncManifestFilteredRules(root, rules, includes)
	if err != nil {
		t.Fatal(err)
	}
	for _, paths := range [][]string{manifest.Files, manifest.Changed, manifest.Deleted, manifest.OverlayFiles} {
		for _, path := range paths {
			if strings.HasPrefix(path, base+"/crabbox/") {
				t.Fatalf("managed marker remained in manifest: %q", path)
			}
		}
	}
	for _, wanted := range []string{base + "/source.txt", "source.txt"} {
		if !slices.Contains(manifest.Files, wanted) {
			t.Fatalf("ordinary source omitted: %q", wanted)
		}
	}
	if !slices.Contains(manifest.Changed, base+"/source.txt") || !slices.Contains(manifest.OverlayFiles, base+"/source.txt") || !slices.Contains(manifest.Deleted, base+"/old-source.txt") {
		t.Fatal("ordinary changed/overlay/deleted controls were lost")
	}
	if newWatchPathScope(rules, nil).traverseExcludedDir(base + "/crabbox") {
		t.Fatal("negation reopened protected watch subtree")
	}
	cfg.Sync.GitSeed = true
	cfg.Sync.BaseRef = "main"
	head := gitOutput(root, "rev-parse", "HEAD")
	if head == "" {
		t.Fatal("fixture HEAD unavailable")
	}
	runGit(t, root, "remote", "add", "origin", "https://example.invalid/repository")
	runGit(t, root, "update-ref", "refs/remotes/origin/main", head)
	repo := Repo{Root: root, Head: head, BaseRef: "main", RemoteURL: "https://example.invalid/repository"}
	for _, state := range []string{"", t.TempDir()} {
		t.Setenv("XDG_STATE_HOME", state)
		plan, _ := syncGitCoherencePlan(cfg, repo)
		if !plan.seedEnabled() || !plan.enabled() {
			t.Fatal("ordinary metadata-only seed control is not eligible")
		}
	}
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, base))
	plan, _ := syncGitCoherencePlan(cfg, repo)
	if plan.seedEnabled() || plan.enabled() {
		t.Fatal("nested state retained whole-tree seeding")
	}
}

func TestManagedStateTransferScopeBoundaries(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	for _, scope := range []string{root, filepath.Join(root, "state", "crabbox"), filepath.Join(root, "state", "crabbox", "marker.txt")} {
		if err := ValidateManagedStateTransferScope("fixture native scope", scope); err == nil {
			t.Fatalf("overlap accepted: %q", scope)
		}
	}
	if err := ValidateManagedStateTransferScope("fixture native scope", filepath.Join(root, "state", "source")); err != nil {
		t.Fatal(err)
	}
	volumeRoot := filepath.VolumeName(root) + string(filepath.Separator)
	if !managedPathContains(volumeRoot, root) {
		t.Fatal("filesystem root containment failed")
	}
	if _, err := managedStateSyncSubtree(filepath.Join(root, "state", "crabbox", "repo")); err == nil {
		t.Fatal("source inside managed namespace accepted")
	}
	t.Setenv("XDG_STATE_HOME", "")
	if err := ValidateManagedStateTransferScope("fixture", "relative-default-scope"); err != nil {
		t.Fatal("unset default changed")
	}
}

func TestManagedStateSyncDirectoryReplacedByFile(t *testing.T) {
	for _, location := range []string{"unset", "outside", "nested"} {
		t.Run(location, func(t *testing.T) {
			root := t.TempDir()
			state := ""
			if location == "outside" {
				state = t.TempDir()
			} else if location == "nested" {
				state = filepath.Join(root, "state")
			}
			t.Setenv("XDG_STATE_HOME", state)
			runGit(t, root, "init")
			runGit(t, root, "config", "user.email", "test@example.com")
			runGit(t, root, "config", "user.name", "Test")
			old := "src/replaced/deep/old.txt"
			writeFile(t, filepath.Join(root, filepath.FromSlash(old)), "old source\n")
			runGit(t, root, "add", ".")
			runGit(t, root, "commit", "-m", "original directory")
			for _, rel := range []string{old, "src/replaced/deep", "src/replaced"} {
				if err := os.Remove(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
					t.Fatal(err)
				}
			}
			writeFile(t, filepath.Join(root, "src", "replaced"), "replacement file\n")
			rules, err := syncExcludes(root, baseConfig())
			if err != nil {
				t.Fatal(err)
			}
			manifest, err := syncManifestFilteredRules(root, rules, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(manifest.Files, []string{"src/replaced"}) || !slices.Equal(manifest.Deleted, []string{old}) {
				t.Fatalf("replacement manifest files=%q deleted=%q", manifest.Files, manifest.Deleted)
			}
		})
	}
}

func TestManagedStateTransferCaseSpelling(t *testing.T) {
	parent := t.TempDir()
	original := filepath.Join(parent, "MixedCase")
	if err := os.Mkdir(original, 0o700); err != nil {
		t.Fatal(err)
	}
	canonical, err := NormalizeManagedStateTransferRoot(original)
	if err != nil {
		t.Fatal(err)
	}
	variant := filepath.Join(parent, "mixedcase")
	got, err := NormalizeManagedStateTransferRoot(variant)
	if err != nil {
		t.Fatal(err)
	}
	originalInfo, err := os.Stat(original)
	if err != nil {
		t.Fatal(err)
	}
	variantInfo, statErr := os.Stat(variant)
	if statErr == nil {
		if !os.SameFile(originalInfo, variantInfo) || got != canonical {
			t.Fatal("case alias did not resolve to its actual directory")
		}
	} else if os.IsNotExist(statErr) {
		if got != filepath.Join(filepath.Dir(canonical), "mixedcase") {
			t.Fatal("case-sensitive missing sibling was conflated")
		}
	} else {
		t.Fatal(statErr)
	}
}

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

func setupOrdinaryHiddenSyncRepo(t *testing.T, skipWorktree bool) string {
	t.Helper()
	clearConfigEnv(t)
	dir := t.TempDir()
	isolateRunTestUserDirs(t, t.TempDir())
	t.Chdir(dir)
	t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "config.yaml"))
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	writeFile(t, filepath.Join(dir, "visible", "keep.txt"), "keep\n")
	writeFile(t, filepath.Join(dir, "hidden", "drop.txt"), "drop\n")
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "init")
	if skipWorktree {
		runGit(t, dir, "update-index", "--skip-worktree", "hidden/drop.txt")
		if err := os.Remove(filepath.Join(dir, "hidden", "drop.txt")); err != nil {
			t.Fatal(err)
		}
	} else {
		runGit(t, dir, "sparse-checkout", "set", "visible")
	}
	return dir
}

func assertOrdinaryHiddenSyncGuidance(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected hidden in-scope path error")
	}
	for _, text := range []string{`tracked path "hidden/drop.txt"`, "materialize", "sync.include", "sync.exclude", ".crabboxignore"} {
		if !strings.Contains(err.Error(), text) {
			t.Errorf("diagnostic missing %q: %v", text, err)
		}
	}
}

func TestSyncManifestOrdinaryHiddenPathRecovery(t *testing.T) {
	for _, skip := range []bool{false, true} {
		t.Run(fmt.Sprintf("skip=%t", skip), func(t *testing.T) {
			dir := setupOrdinaryHiddenSyncRepo(t, skip)
			_, err := syncManifestFiltered(dir, nil, nil)
			assertOrdinaryHiddenSyncGuidance(t, err)
		})
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
	a, err := syncFingerprintForManifest(context.Background(), Repo{Root: t.TempDir()}, baseConfig(), SyncManifest{}, builtIn, plan)
	if err != nil {
		t.Fatal(err)
	}
	b, err := syncFingerprintForManifest(context.Background(), Repo{Root: t.TempDir()}, baseConfig(), SyncManifest{}, configured, plan)
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
					value, err := syncFingerprintForManifest(context.Background(), Repo{Root: root}, cfg, manifest, SyncExcludeRules{}, plan)
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

func TestGitCoherenceRequiresRemoteTrackingRef(t *testing.T) {
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
		t.Fatal("head without a containing branch should not enable coherence")
	}
	runGit(t, dir, "update-ref", "refs/remotes/origin/main", head)
	if plan, _ := syncGitCoherencePlan(baseConfig(), repo); !plan.enabled() {
		t.Fatal("head in a remote-tracking ref should enable coherence")
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
		fingerprint, _ := syncFingerprintForManifest(context.Background(), Repo{}, baseConfig(), SyncManifest{}, SyncExcludeRules{}, plan)
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

func TestSyncGitCoherencePlanRanksContainingOriginBranches(t *testing.T) {
	t.Parallel()
	f := newGitCoherenceFixture(t)
	runGit(t, f.source, "update-ref", "refs/remotes/origin/alpha", f.b)
	runGit(t, f.source, "update-ref", "refs/remotes/origin/release", f.c)
	runGit(t, f.source, "update-ref", "refs/remotes/origin/trunk", f.c)
	runGit(t, f.source, "update-ref", "refs/remotes/origin/old", f.a)
	tree := gitOutput(f.source, "rev-parse", f.b+"^{tree}")
	for _, tc := range []struct {
		name, configuredBase, baseRef, originHead, want string
	}{
		{"repository ancestor", "", "main", "trunk", "main"},
		{"origin-prefixed ancestor", "", "origin/main", "trunk", "main"},
		{"remote-ref ancestor", "", "refs/remotes/origin/main", "trunk", "main"},
		{"heads-ref ancestor", "", "refs/heads/main", "trunk", "main"},
		{"repository base before default", "", "release", "trunk", "release"},
		{"configured before inferred and default", "release", "main", "main", "release"},
		{"absent base", "", "", "trunk", "trunk"},
		{"missing base", "", "missing", "trunk", "trunk"},
		{"base lacks target", "", "old", "trunk", "trunk"},
		{"missing configured base", "missing", "main", "trunk", "trunk"},
		{"configured base lacks target", "old", "main", "trunk", "trunk"},
		{"invalid base", "", "main~1", "trunk", "trunk"},
		{"default lacks target", "", "old", "old", "alpha"},
		{"missing default", "", "missing", "missing", "alpha"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runGit(t, f.source, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/"+tc.originHead)
			refs := gitOutput(f.source, "for-each-ref", "--format=%(refname) %(objectname) %(symref)", "refs/remotes/origin")
			cfg := baseConfig()
			cfg.Sync.BaseRef = tc.configuredBase
			plan, blocked := syncGitCoherencePlan(cfg, Repo{
				Root: f.source, RemoteURL: f.origin, Head: f.b, BaseRef: tc.baseRef,
			})
			if blocked || !plan.enabled() || plan.Branch != tc.want {
				t.Errorf("plan=%#v blocked=%v; want branch %q", plan, blocked, tc.want)
			}
			if plan.Target != f.b || plan.Tree != tree {
				t.Errorf("plan changed selected commit/tree: %#v; want target=%s tree=%s", plan, f.b, tree)
			}
			requireGitOutput(t, f.source, refs, "for-each-ref", "--format=%(refname) %(objectname) %(symref)", "refs/remotes/origin")
			requireGitOutput(t, f.source, f.c, "rev-parse", "HEAD")
		})
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

func TestDirectorySyncManifest(t *testing.T) {
	clearConfigEnv(t)
	root, scratch := t.TempDir(), t.TempDir()
	t.Setenv("TMPDIR", scratch)
	files := map[string]string{
		".gitignore": "*.tmp\n!keep.tmp\n",
		"README.txt": "readme\n", "src/a.txt": "a\n", "src/drop.tmp": "drop\n",
		"src/nested/.gitignore": "local.txt\n", "src/nested/local.txt": "local\n",
		"src/nested/keep.tmp": "keep\n", "src/nested/b.txt": "b\n",
		".crabboxignore": "src/a.txt\n!src/a.txt\nsrc/nested/b.txt\n",
		"outside.txt":    "outside\n",
	}
	before := map[string]os.FileInfo{}
	for name, data := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		writeFile(t, path, data)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		before[name] = info
	}
	// Ambient Git excludes must not participate in a directory-source manifest.
	ambient := filepath.Join(t.TempDir(), "global-ignore")
	writeFile(t, ambient, "README.txt\n")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.excludesFile")
	t.Setenv("GIT_CONFIG_VALUE_0", ambient)
	cfg := baseConfig()
	cfg.Sync.Source = "directory"
	cfg.Sync.Includes = []string{"README.txt", "src"}
	rules, err := syncExcludes(root, cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"README.txt", "src/a.txt", "src/nested/.gitignore", "src/nested/keep.tmp"}
	for i := 0; i < 2; i++ {
		got, err := syncManifestForSource(context.Background(), Repo{Root: root}, cfg, rules)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got.Files, want) {
			t.Fatalf("files=%q want=%q", got.Files, want)
		}
		if len(got.Changed) != 0 || len(got.Deleted) != 0 || len(got.OverlayFiles) != 0 {
			t.Fatalf("manufactured Git delta: %+v", got)
		}
		count, size, _, _ := syncGuardrailScope(got)
		if count != len(want) || size != got.Bytes {
			t.Fatalf("guardrail=%d/%d manifest=%+v", count, size, got)
		}
	}
	for name, data := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != data || info.Mode() != before[name].Mode() || !info.ModTime().Equal(before[name].ModTime()) {
			t.Fatalf("source changed: %s", name)
		}
	}
	if _, err := os.Lstat(filepath.Join(root, ".git")); !os.IsNotExist(err) {
		t.Fatalf("source metadata: %v", err)
	}
	entries, err := os.ReadDir(scratch)
	if err != nil || len(entries) != 0 {
		t.Fatalf("temporary metadata retained: %v %v", entries, err)
	}
	cfg.Sync.Includes = []string{"absent.txt"}
	got, err := syncManifestForSource(context.Background(), Repo{Root: root}, cfg, rules)
	if err != nil || len(got.Files) != 0 {
		t.Fatalf("empty admitted manifest=%+v err=%v", got, err)
	}
	cfg.Sync.Includes = []string{" ", ""}
	if _, err := syncManifestForSource(context.Background(), Repo{Root: root}, cfg, rules); err == nil {
		t.Fatal("empty include accepted")
	}
}

func TestDirectorySyncNestedRepositoryScope(t *testing.T) {
	clearConfigEnv(t)
	root := t.TempDir()
	t.Setenv("TMPDIR", t.TempDir())
	writeFile(t, filepath.Join(root, "src/plain.txt"), "plain\n")
	nested := filepath.Join(root, "src/nested")
	writeFile(t, filepath.Join(nested, "file.txt"), "nested\n")
	runGit(t, nested, "init")
	for _, tc := range []struct {
		name               string
		includes, excludes []string
		reject             bool
	}{
		{"literal ancestor", []string{"src"}, nil, true},
		{"literal repository", []string{"src/nested"}, nil, true},
		{"literal child", []string{"src/nested/file.txt"}, nil, true},
		{"child glob", []string{"src/nested/*.txt"}, nil, true},
		{"component glob", []string{"src/*/*.txt"}, nil, true},
		{"nonrecursive glob", []string{"src/*"}, nil, false},
		{"identical glob excluded", []string{"src/nested/*.txt"}, []string{"src/nested/*.txt"}, false},
		{"normalized identical glob", []string{"/src/nested/*.txt/"}, []string{"src/nested/*.txt"}, false},
		{"identical glob reopened", []string{"src/nested/*.txt"}, []string{"src/nested/*.txt", "!src/nested/file.txt"}, true},
		{"identical glob final exclusion", []string{"src/nested/*.txt"}, []string{"src/nested/*.txt", "!src/nested/file.txt", "src/nested/*.txt"}, false},
		{"overlapping wildcard unresolved", []string{"src/nested/file?.txt"}, []string{"src/nested/*.txt"}, true},

		{"outside include", []string{"src/plain.txt"}, nil, false},
		{"excluded", []string{"src"}, []string{"src/nested"}, false},
		{"excluded literal grant", []string{"src/nested/file.txt"}, []string{"src/nested/file.txt"}, false},
		{"excluded literal prefix grant", []string{"src/nested/subdir"}, []string{"src/nested/subdir"}, false},
		{"literal grant reopened", []string{"src/nested/subdir"}, []string{"src/nested/subdir", "!src/nested/subdir/file.txt"}, true},
		{"later reinclude", []string{"src"}, []string{"src/nested", "!src/nested/file.txt"}, true},
		{"final subtree exclusion", []string{"src"}, []string{"src/nested", "!src/nested/file.txt", "src/nested"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseConfig()
			cfg.Sync.Source = "directory"
			cfg.Sync.Includes = tc.includes
			cfg.Sync.Excludes = tc.excludes
			rules, err := syncExcludes(root, cfg)
			if err != nil {
				t.Fatal(err)
			}
			_, err = syncManifestForSource(context.Background(), Repo{Root: root}, cfg, rules)
			if tc.reject {
				if err == nil || !strings.Contains(err.Error(), "nested repository") {
					t.Fatalf("error=%v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDirectorySyncEffectiveRoot(t *testing.T) {
	clearConfigEnv(t)
	outer := t.TempDir()
	runGit(t, outer, "init")
	root := filepath.Join(outer, "inner")
	writeFile(t, filepath.Join(root, "README.txt"), "inner\n")
	t.Chdir(root)
	cfg := baseConfig()
	cfg.Sync.Source = "directory"
	cfg.Sync.Includes = []string{"README.txt"}
	repo, err := findSyncRepo(context.Background(), cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	if repo.Root != canonicalRepositoryPath(root) || repo.Head != "" || repo.RemoteURL != "" {
		t.Fatalf("repo=%+v", repo)
	}
	if err := os.Mkdir(filepath.Join(root, ".jj"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := findSyncRepo(context.Background(), cfg, true); err == nil || !strings.Contains(err.Error(), "native Jujutsu") {
		t.Fatalf("error=%v", err)
	}
}

func TestDirectorySyncMetadataCleanupOnGitFailure(t *testing.T) {
	clearConfigEnv(t)
	root, scratch := t.TempDir(), t.TempDir()
	t.Setenv("TMPDIR", scratch)
	t.Setenv("PATH", t.TempDir())
	_, err := directorySyncFileList(context.Background(), root)
	if err == nil || !strings.Contains(err.Error(), "installed Git required") {
		t.Fatalf("error=%v", err)
	}
	entries, readErr := os.ReadDir(scratch)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("temporary metadata=%v error=%v", entries, readErr)
	}
	if _, err := os.Lstat(filepath.Join(root, ".git")); !os.IsNotExist(err) {
		t.Fatalf("source Git metadata: %v", err)
	}
}

func TestJJSyncScopeCompleteness(t *testing.T) {
	hidden := jjPendingEntry{Path: "hidden/file.txt", Kind: "file", TrackedRegular: true, CachedMaterialization: "unestablished"}
	missing := jjPendingEntry{Path: "src/old.txt", Kind: "file", TrackedRegular: true, InSparseScope: true, CachedMaterialization: "cached"}
	uncertain := missing
	uncertain.CachedMaterialization = "needs_observation"
	structural := jjPendingEntry{Path: "src/mixed", Kind: "other_conflict", CoversDescendants: true, InSparseScope: true, CachedMaterialization: "cached"}
	submodule := jjPendingEntry{Path: "vendor/module", Kind: "submodule", InSparseScope: true, CachedMaterialization: "cached"}
	for _, tc := range []struct {
		name         string
		entry        jjPendingEntry
		captures     []jjAdmittedFile
		observations []jjPathObservation
		includes     []string
		excludes     []string
		wantReason   string
	}{
		{name: "required sparse file", entry: hidden, wantReason: "outside native sparse scope"},
		{name: "outside includes", entry: hidden, includes: []string{"src"}},
		{name: "explicitly excluded", entry: hidden, excludes: []string{"hidden"}},
		{name: "ordered reinclude", entry: hidden, excludes: []string{"hidden", "!hidden/file.txt"}, wantReason: "outside native sparse scope"},
		{name: "sparse physical capture does not establish native admission", entry: hidden, captures: []jjAdmittedFile{{Path: hidden.Path}}, wantReason: "outside native sparse scope"},
		{name: "ordinary deletion", entry: missing, observations: []jjPathObservation{{Path: missing.Path, Kind: "removed_absent"}}},
		{name: "uncertain deletion", entry: uncertain, observations: []jjPathObservation{{Path: missing.Path, Kind: "removed_absent"}}, wantReason: "prior materialization is unestablished"},
		{name: "captured placeholder", entry: uncertain, captures: []jjAdmittedFile{{Path: missing.Path}}},
		{name: "uncaptured cached file", entry: missing, wantReason: "required path was not admitted"},
		{name: "unsupported replacement", entry: missing, observations: []jjPathObservation{{Path: missing.Path, Kind: "omitted_unsupported_kind"}}, wantReason: "omitted_unsupported_kind"},
		{name: "nested boundary", entry: missing, observations: []jjPathObservation{{Path: "src", Kind: "omitted_nested_repository", Subtree: true}}, wantReason: "omitted_nested_repository"},
		{name: "excluded nested contents", entry: missing, observations: []jjPathObservation{{Path: "src", Kind: "omitted_nested_repository", Subtree: true}}, excludes: []string{"src/old.txt"}},
		{name: "nested omission wins over inferred absence", entry: missing, observations: []jjPathObservation{{Path: missing.Path, Kind: "removed_absent"}, {Path: "src", Kind: "omitted_nested_repository", Subtree: true}}, wantReason: "omitted_nested_repository"},
		{name: "kind replacement may be deletion only", entry: missing, observations: []jjPathObservation{{Path: missing.Path, Kind: "removed_kind_replacement"}}, includes: []string{missing.Path}},
		{name: "structural marker selected", entry: structural, captures: []jjAdmittedFile{{Path: structural.Path}}},
		{name: "structural child selected", entry: structural, captures: []jjAdmittedFile{{Path: structural.Path}}, includes: []string{"src/mixed/child.rs"}, wantReason: "selected descendants are not materialized"},
		{name: "structural child excluded", entry: structural, includes: []string{"src/mixed/child.rs"}, excludes: []string{"src/mixed"}},
		{name: "structural child reincluded", entry: structural, excludes: []string{"src/mixed", "!src/mixed/child.rs"}, wantReason: "selected descendants are not materialized"},
		{name: "ordinary submodule boundary", entry: submodule},
		{name: "submodule contents requested", entry: submodule, includes: []string{"vendor/module/child.rs"}, wantReason: "selected descendants are not materialized"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", "")
			inventory := jjSourceInventory{Pending: []jjPendingEntry{tc.entry}, Admitted: tc.captures, Observations: tc.observations}
			scope, err := validatedJJSyncManifestScope(t.TempDir(), newSyncExcludeRules(tc.excludes, syncExcludeConfigured), tc.includes, inventory)
			if tc.wantReason != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantReason) {
					t.Fatalf("error=%v want %q", err, tc.wantReason)
				}
				if scope.trackedRegular != nil || scope.gitlinkPaths != nil {
					t.Fatal("incomplete source returned an accepted scope")
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestJJSyncScopeUsesTrackedArtifactAndManagedStateRules(t *testing.T) {
	for _, tc := range []struct {
		name, path string
		excludes   []string
		wantError  bool
	}{
		{name: "tracked artifact remains required", path: "target/pkg/main.go", wantError: true},
		{name: "configured artifact exclusion", path: "target/pkg/main.go", excludes: []string{"target"}},
		{name: "configured reinclude", path: "target/pkg/main.go", excludes: []string{"target", "!target/pkg/main.go"}, wantError: true},
		{name: "dependency exclusion remains", path: "node_modules/pkg/main.js"},
		{name: "protected runtime file", path: ".crabbox/env/synthetic", excludes: []string{"!**"}},
		{name: "literal managed namespace", path: "state [cache]/crabbox/synthetic", excludes: []string{"!**"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state [cache]"))
			cfg := baseConfig()
			cfg.Sync.Excludes = tc.excludes
			rules, err := syncExcludes(root, cfg)
			if err != nil {
				t.Fatal(err)
			}
			inventory := jjSourceInventory{Pending: []jjPendingEntry{{Path: tc.path, Kind: "file", TrackedRegular: true, CachedMaterialization: "unestablished"}}}
			_, err = validatedJJSyncManifestScope(root, rules, []string{tc.path}, inventory)
			if (err != nil) != tc.wantError {
				t.Fatalf("error=%v wantError=%v", err, tc.wantError)
			}
		})
	}
}

func TestJJSourceReadRetainsNativeMetadataFacts(t *testing.T) {
	root := t.TempDir()
	header := jjProtocolHeader{Protocol: jjSourceProtocolName, SchemaVersion: jjSourceProtocolVersion, Kind: "context", JJVersion: jjSourceNativeVersion}
	contextData, err := json.Marshal(jjSourceContext{
		jjProtocolHeader: header, WorkspaceRoot: root,
		RepositoryPath: filepath.Join(root, ".jj", "repo"), Workspace: "default",
		WorkingCopyOperation: strings.Repeat("a", 128), OperationHeads: []string{strings.Repeat("a", 128)},
	})
	if err != nil {
		t.Fatal(err)
	}
	header.Kind = "live_inventory"
	facts := jjSourceInventory{
		jjProtocolHeader: header,
		Identity: jjSourceIdentity{
			WorkspaceRoot: root, RepositoryPath: filepath.Join(root, ".jj", "repo"), Workspace: "default",
			Operation: strings.Repeat("a", 128), Commit: strings.Repeat("d", 40), Change: strings.Repeat("e", 32),
			Parents: []string{strings.Repeat("b", 40), strings.Repeat("c", 40)},
			TreeIDs: []string{strings.Repeat("f", 40), strings.Repeat("0", 40), strings.Repeat("1", 40)}, TreeLabels: []string{"left", "base", "right"},
			Policy: jjMaterializationPolicy{ExecPolicy: "auto", EOLConversion: "none", ConflictMarkerStyle: "diff", MergeHunkLevel: "line", SameChange: "accept", MaterializationHost: "macos"},
		},
		WorkingCopyOperation: strings.Repeat("a", 128), WorkingCopyFreshness: "fresh",
		WorkingCopyTreeIDs: []string{strings.Repeat("f", 40), strings.Repeat("0", 40), strings.Repeat("1", 40)}, WorkingCopyTreeLabels: []string{"left", "base", "right"},
		MaxNewFileSize: 1048576, Pending: []jjPendingEntry{}, SparsePrefixes: []string{""},
		Tracked: []jjTrackedEntry{}, Admitted: []jjAdmittedFile{}, Observations: []jjPathObservation{}, Untracked: []jjUntrackedEntry{},
	}
	data, err := json.Marshal(facts)
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := decodeJJSourceRead(contextData, data)
	if err != nil {
		t.Fatal(err)
	}
	for _, terms := range []int{1, 3} {
		t.Run(fmt.Sprintf("unlabeled_%d_terms", terms), func(t *testing.T) {
			unlabeled := facts
			unlabeled.Identity.TreeIDs = facts.Identity.TreeIDs[:terms]
			unlabeled.Identity.TreeLabels = []string{}
			unlabeled.WorkingCopyTreeIDs = facts.WorkingCopyTreeIDs[:terms]
			unlabeled.WorkingCopyTreeLabels = []string{}
			encoded, err := json.Marshal(unlabeled)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := decodeJJSourceRead(contextData, encoded); err != nil {
				t.Fatalf("native unlabeled tree rejected: %v", err)
			}
			recorded, err := json.Marshal(jjRecordedInventory{
				jjProtocolHeader: jjProtocolHeader{Protocol: jjSourceProtocolName, SchemaVersion: jjSourceProtocolVersion, Kind: "recorded_inventory", JJVersion: jjSourceNativeVersion},
				Identity:         unlabeled.Identity, Entries: []jjRecordedEntry{},
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := decodeJJRecordedInventory(recorded); err != nil {
				t.Fatalf("native recorded unlabeled tree rejected: %v", err)
			}
		})
	}
	pretty, err := json.MarshalIndent(facts, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	equivalent, err := decodeJJSourceRead(contextData, pretty)
	if err != nil || equivalent.MetadataDigest != baseline.MetadataDigest {
		t.Fatalf("formatting changed native metadata identity: %v", err)
	}
	for _, field := range []string{"max_new_file_size", "parents", "policy"} {
		t.Run(field, func(t *testing.T) {
			changed := facts
			switch field {
			case "parents":
				changed.Identity.Parents = []string{strings.Repeat("c", 40), strings.Repeat("b", 40)}
			case "policy":
				changed.Identity.Policy.EOLConversion = "input"
			default:
				changed.MaxNewFileSize = 2097152
			}
			data, err := json.Marshal(changed)
			if err != nil {
				t.Fatal(err)
			}
			read, err := decodeJJSourceRead(contextData, data)
			if err != nil || read.MetadataDigest == baseline.MetadataDigest {
				t.Fatalf("native %s change was lost: %v", field, err)
			}
		})
	}
}

func TestJJSyncSelectedObservationsAndStaging(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	const selected = "ordinary selected content\n"
	payloads := map[string]string{
		"selected.txt":             selected,
		"excluded.txt":             "ordinary excluded content\n",
		"state/crabbox/marker.txt": "ordinary state marker\n",
	}
	var inventory jjSourceInventory
	for _, path := range []string{"selected.txt", "excluded.txt", "state/crabbox/marker.txt"} {
		writeFile(t, filepath.Join(root, filepath.FromSlash(path)), payloads[path])
		info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			t.Fatal(err)
		}
		executable := info.Mode().Perm()&0o111 != 0
		inventory.Admitted = append(inventory.Admitted, jjAdmittedFile{Path: path, Kind: "file", Executable: &executable, ObservedSize: uint64(info.Size()), ObservedMtimeMillis: info.ModTime().UnixMilli()})
		inventory.Pending = append(inventory.Pending, jjPendingEntry{Path: path, Kind: "file", TrackedRegular: true, InSparseScope: true, CachedMaterialization: "cached"})
	}
	cfg := baseConfig()
	cfg.Sync.Excludes = append(cfg.Sync.Excludes, "excluded.txt")
	cfg.Sync.FailBytes = int64(len(selected)) + 1
	selection, err := selectJJSourceFiles(context.Background(), root, cfg, inventory, false, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(selection.Files) != 1 || selection.Files[0].Path != "selected.txt" || selection.Files[0].Observed.Size() != int64(len(selected)) || selection.Manifest.Bytes != int64(len(selected)) {
		t.Fatalf("selected observations=%+v manifest=%+v", selection.Files, selection.Manifest)
	}
	cfg.Sync.FailBytes = int64(len(selected))
	if _, err := selectJJSourceFiles(context.Background(), root, cfg, inventory, false, io.Discard); err == nil {
		t.Fatal("selection bypassed the ordinary full-sync byte limit")
	}
	if _, err := selectJJSourceFiles(context.Background(), root, cfg, inventory, true, io.Discard); err != nil {
		t.Fatalf("explicit ordinary size override failed: %v", err)
	}
	cfg.Sync.FailBytes = int64(len(selected)) + 1
	metadata, err := json.Marshal(inventory)
	if err != nil {
		t.Fatal(err)
	}
	read := jjSourceRead{
		Context:   jjSourceContext{WorkspaceRoot: root, RepositoryPath: filepath.Join(root, ".jj", "repo")},
		Inventory: inventory, MetadataDigest: sha256.Sum256(metadata),
	}
	reads, reconfigure := 0, false
	reader := func(context.Context) (jjSourceRead, error) {
		reads++
		if reconfigure && reads == 2 {
			t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state-b"))
		}
		return read, nil
	}
	snapshot, err := prepareJJSourceSnapshot(context.Background(), root, cfg, reader, false, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := snapshot.cleanup(); err != nil {
			t.Error(err)
		}
	})
	if reads != 3 || snapshot.ContentDigest == "" {
		t.Fatalf("native reads=%d content digest=%q", reads, snapshot.ContentDigest)
	}
	entries, err := os.ReadDir(snapshot.Root)
	if err != nil || len(entries) != 1 || entries[0].Name() != "selected.txt" {
		t.Fatalf("staged entries=%v error=%v", entries, err)
	}
	if content, err := os.ReadFile(filepath.Join(snapshot.Root, "selected.txt")); err != nil || string(content) != selected {
		t.Fatalf("staged content=%q error=%v", content, err)
	}
	for _, state := range []string{"state-a", "state-b"} {
		if err := os.MkdirAll(filepath.Join(root, state, "crabbox"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cfg.Sync.Excludes = append(cfg.Sync.Excludes, "state")
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state-a"))
	reads, reconfigure = 0, true
	recaptured, err := prepareJJSourceSnapshot(context.Background(), root, cfg, reader, false, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := recaptured.cleanup(); err != nil {
			t.Error(err)
		}
	})
	if reads != 5 || recaptured.Selection.Excludes.managedSubtree != "state-b/crabbox" || recaptured.ContentDigest != snapshot.ContentDigest {
		t.Fatalf("recaptured reads=%d scope=%q digest=%q", reads, recaptured.Selection.Excludes.managedSubtree, recaptured.ContentDigest)
	}
}

func TestJJSyncManifestNativeInventory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("recorded native fixture uses POSIX symlinks and executable modes")
	}
	t.Setenv("XDG_STATE_HOME", "")
	data, err := os.ReadFile("testdata/jj-live-scope.json")
	if err != nil {
		t.Fatal(err)
	}
	var inventory jjSourceInventory
	if err := json.Unmarshal(data, &inventory); err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Files []struct {
			Path string `json:"path"`
			Raw  struct {
				Kind, Target string
				Bytes        []byte
				Executable   bool
			}
		} `json:"payload_files"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	for _, file := range payload.Files {
		full := filepath.Join(root, filepath.FromSlash(file.Path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if file.Raw.Kind == "symlink" {
			if err := os.Symlink(file.Raw.Target, full); err != nil {
				t.Fatal(err)
			}
			continue
		}
		mode := os.FileMode(0o644)
		if file.Raw.Executable {
			mode = 0o755
		}
		if err := os.WriteFile(full, file.Raw.Bytes, mode); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name                     string
		includes, excludes       []string
		wantOmissions, wantFiles []string
		wantDeleted              []string
	}{
		{name: "default required omissions", wantOmissions: []string{"outside-known.txt", "src/nested/old.rs", "src/special.rs"}},
		{name: "narrow source", includes: []string{"src/base.txt"}, wantFiles: []string{"src/base.txt"}},
		{name: "excluded incomplete content", excludes: []string{"outside-known.txt", "src/nested", "src/special.rs"}, wantDeleted: []string{"src/delete.txt", "src/to-dir.rs", "src/to-file.rs/old.rs"}},
		{name: "nested child reincluded", excludes: []string{"outside-known.txt", "src/nested", "src/special.rs", "!src/nested/old.rs"}, wantOmissions: []string{"src/nested/old.rs"}},
		{name: "replacement child outside glob", includes: []string{"src/*.rs"}, excludes: []string{"src/special.rs"}, wantFiles: []string{"src/a.rs", "src/b.rs", "src/c.rs", "src/candidate.rs", "src/conflict.rs", "src/line-endings.rs", "src/to-file.rs"}, wantDeleted: []string{"src/to-dir.rs"}},
		{name: "old descendant only", includes: []string{"src/to-file.rs/old.rs"}, wantFiles: []string{}, wantDeleted: []string{"src/to-file.rs/old.rs"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manifest, err := syncManifestFilteredRulesWithSource(root, newSyncExcludeRules(tc.excludes, syncExcludeConfigured), tc.includes, jjSyncManifestSource(inventory))
			if tc.wantOmissions != nil {
				incomplete, ok := err.(*jjIncompleteSourceError)
				if !ok {
					t.Fatalf("expected incomplete source: %v", err)
				}
				var paths []string
				for _, omission := range incomplete.Omissions {
					paths = append(paths, omission.Path)
				}
				if !slices.Equal(paths, tc.wantOmissions) || len(manifest.Files) != 0 || len(manifest.Deleted) != 0 {
					t.Fatalf("omissions=%q manifest=%+v", paths, manifest)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(manifest.Deleted, tc.wantDeleted) {
				t.Fatalf("deleted=%q want %q", manifest.Deleted, tc.wantDeleted)
			}
			if tc.wantFiles != nil && !slices.Equal(manifest.Files, tc.wantFiles) {
				t.Fatalf("files=%q want %q", manifest.Files, tc.wantFiles)
			}
			if tc.wantFiles == nil && len(manifest.Files) != len(inventory.Admitted) {
				t.Fatalf("accepted files=%d want %d", len(manifest.Files), len(inventory.Admitted))
			}
		})
	}
}

func TestJJRecordedProtocolRoundTrip(t *testing.T) {
	fixture := os.Getenv("CRABBOX_TEST_JJ_INVENTORY")
	liveFixture := fixture != ""
	if fixture == "" {
		fixture = filepath.Join("testdata", "jj-recorded-protocol-v1.json")
	}
	data, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if !liveFixture {
		var portable jjRecordedInventory
		if err := json.Unmarshal(data, &portable); err != nil {
			t.Fatal(err)
		}
		portable.Identity.WorkspaceRoot = t.TempDir()
		portable.Identity.RepositoryPath = filepath.Join(portable.Identity.WorkspaceRoot, ".jj", "repo")
		data, err = json.Marshal(portable)
		if err != nil {
			t.Fatal(err)
		}
	}
	inventory, err := decodeJJRecordedInventory(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(inventory.Identity.Parents) != 3 {
		t.Fatal("fixture lost its octopus parents")
	}
	paths := []string{"src/shared.txt", "src/link"}
	encoded, err := encodeJJRecordedExportRequest(inventory, paths)
	if err != nil {
		t.Fatal(err)
	}
	var request jjRecordedExportRequest
	if err := json.Unmarshal(encoded, &request); err != nil {
		t.Fatal(err)
	}
	original, err := json.Marshal(inventory.Identity)
	if err != nil {
		t.Fatal(err)
	}
	echoed, err := json.Marshal(request.Identity)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, echoed) || strings.Join(request.Paths, "\x00") != strings.Join(paths, "\x00") {
		t.Fatal("request changed native identity, policy, ordered tree IDs or selected paths")
	}
	if output := os.Getenv("CRABBOX_TEST_JJ_EXPORT_REQUEST"); output != "" {
		if err := os.WriteFile(output, encoded, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	empty, err := encodeJJRecordedExportRequest(inventory, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(empty, []byte(`"paths":[]`)) {
		t.Fatal("empty selection must be an explicit array")
	}
	if _, err := encodeJJRecordedExportRequest(inventory, []string{paths[0], paths[0]}); err == nil {
		t.Fatal("duplicate selected path accepted")
	}
	inventory.SchemaVersion++
	otherVersion, err := json.Marshal(inventory)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeJJRecordedInventory(otherVersion); err == nil {
		t.Fatal("unsupported protocol version accepted")
	}
}

func TestJJRecordedProtocolExportBinding(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "jj-recorded-export-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var response jjRecordedExport
	if err := json.Unmarshal(data, &response); err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	response.Identity.WorkspaceRoot = source
	response.Identity.RepositoryPath = filepath.Join(source, ".jj", "repo")
	owner := t.TempDir()
	response.Output = filepath.Join(owner, "payload")
	response.OwnedState = filepath.Join(owner, "state")
	request := jjRecordedExportRequest{Protocol: jjSourceProtocolName, SchemaVersion: jjSourceProtocolVersion, Identity: response.Identity}
	for _, entry := range response.Entries {
		request.Paths = append(request.Paths, entry.Path)
	}
	if fixture := os.Getenv("CRABBOX_TEST_JJ_EXPORT_RESPONSE"); fixture != "" {
		data, err = os.ReadFile(fixture)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &response); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(os.Getenv("CRABBOX_TEST_JJ_EXPORT_REQUEST"))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &request); err != nil {
			t.Fatal(err)
		}
		if _, err := decodeJJRecordedExport(data, request, os.Getenv("CRABBOX_TEST_JJ_OUTPUT_ROOT"), os.Getenv("CRABBOX_TEST_JJ_STATE_ROOT")); err != nil {
			t.Fatal(err)
		}
		return
	}
	encode := func(value jjRecordedExport) []byte {
		t.Helper()
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	if _, err := decodeJJRecordedExport(encode(response), request, response.Output, response.OwnedState); err != nil {
		t.Fatal(err)
	}
	changed := response
	changed.Identity.Policy.EOLConversion = "input-output"
	if _, err := decodeJJRecordedExport(encode(changed), request, response.Output, response.OwnedState); err == nil {
		t.Fatal("changed policy accepted")
	}
	changed = response
	changed.CheckoutStats.SkippedFiles = 1
	if _, err := decodeJJRecordedExport(encode(changed), request, response.Output, response.OwnedState); err == nil {
		t.Fatal("skipped path accepted")
	}
	if _, err := decodeJJRecordedExport(encode(response), request, filepath.Join(owner, "different"), response.OwnedState); err == nil {
		t.Fatal("different output owner accepted")
	}
	changed = response
	changed.Entries = append([]jjRecordedExportEntry{}, response.Entries...)
	changed.Entries[1] = changed.Entries[0]
	if _, err := decodeJJRecordedExport(encode(changed), request, response.Output, response.OwnedState); err == nil {
		t.Fatal("duplicate output entry accepted")
	}
}

func TestJJLiveProtocolStaging(t *testing.T) {
	fixture, helper := os.Getenv("CRABBOX_TEST_JJ_LIVE_FIXTURE"), jjFixtureHelper(t)
	if fixture == "" || helper == "" {
		t.Skip("requires an owned native JJ fixture and helper")
	}
	root := filepath.Join(fixture, "fixture-source")
	evidence := filepath.Join(fixture, "evidence")
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	runner := &jjProcessProofRunner{evidence: evidence}
	process := jjFixtureProcess(t, root, runner)
	reads := 0
	reader := func(ctx context.Context) (jjSourceRead, error) { reads++; return process.readLive(ctx) }
	cfg := baseConfig()
	cfg.Sync.Includes = []string{"src/shared.txt", "src/line.txt", "src/tool.sh", "src/link", "src/pending.txt", "src/not-selected-by-native.txt", "state/crabbox/marker.txt"}
	snapshot, err := prepareJJSourceSnapshot(context.Background(), root, cfg, reader, false, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := snapshot.cleanup(); err != nil {
			t.Error(err)
		}
	})
	want := []string{"src/line.txt", "src/link", "src/pending.txt", "src/shared.txt", "src/tool.sh"}
	if reads != 3 || !slices.Equal(snapshot.Selection.Manifest.Files, want) || len(snapshot.Read.Inventory.Identity.Parents) != 3 {
		t.Fatalf("reads=%d files=%q parents=%q", reads, snapshot.Selection.Manifest.Files, snapshot.Read.Inventory.Identity.Parents)
	}
	expected := map[string]string{"src/shared.txt": "live resolution, not recorded\n", "src/line.txt": "live line endings\r\n", "src/pending.txt": "pending new file\n", "src/tool.sh": "#!/bin/sh\nprintf \"fixture\\n\"\n"}
	for path, want := range expected {
		got, err := os.ReadFile(filepath.Join(snapshot.Root, path))
		if err != nil || string(got) != want {
			t.Fatalf("staged %s=%q: %v", path, got, err)
		}
	}
	archive, err := CreateSyncArchive(context.Background(), Repo{Root: snapshot.Root}, snapshot.Selection.Manifest, "crabbox-jj-protocol-*.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	archivePath := archive.Name()
	t.Cleanup(func() { archive.Close(); os.Remove(archivePath) })
	gz, err := gzip.NewReader(archive)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var members []string
	for {
		entry, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		members = append(members, entry.Name)
		if entry.Name == "src/link" {
			if entry.Typeflag != tar.TypeSymlink || entry.Linkname != "line.txt" {
				t.Fatalf("archive link=%+v", entry)
			}
		} else {
			contents, err := io.ReadAll(tr)
			if err != nil || string(contents) != expected[entry.Name] {
				t.Fatalf("archive %s=%q: %v", entry.Name, contents, err)
			}
			if entry.Name == "src/tool.sh" && entry.Mode&0111 == 0 {
				t.Fatal("archive lost executable bit")
			}
		}
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(members, want) {
		t.Fatalf("archive members=%q", members)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(archivePath); err != nil {
		t.Fatal(err)
	}
	stage := snapshot.Root
	if err := snapshot.cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stage); !os.IsNotExist(err) {
		t.Fatalf("staging retained: %v", err)
	}
	if os.Getenv("CRABBOX_JJ_FIXTURE_CONDITION") != "" {
		if snapshot.Read.Inventory.MaxNewFileSize != 4<<20 || snapshot.Read.Inventory.Identity.Policy.ExecPolicy != "respect" {
			t.Fatal("native conditional policy changed after environment isolation")
		}
		for _, predicate := range []string{"CRABBOX_JJ_FIXTURE_CONDITION", "CRABBOX_JJ_FIXTURE_MODE=active", "CRABBOX_JJ_FIXTURE_MODE=inactive"} {
			if !slices.Contains(runner.conditions, predicate) {
				t.Fatalf("native condition not negotiated: %s", predicate)
			}
		}
	}
	decisions, err := json.Marshal(runner.conditions)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(evidence, "condition-queries.json"), decisions, 0600); err != nil {
		t.Fatal(err)
	}
	result := map[string]any{"native_read_pairs": reads, "files": want, "bytes": snapshot.Selection.Manifest.Bytes, "source_commit": snapshot.Read.Inventory.Identity.Commit, "parents": snapshot.Read.Inventory.Identity.Parents, "content_digest": snapshot.ContentDigest, "pending_bytes_preserved": true, "archive_from_stage_verified": true, "owned_cleanup": true}
	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(evidence, "go-live-result.json"), encoded, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestJJLiveProtocolFixture(t *testing.T) {
	data, err := os.ReadFile("testdata/jj-live-protocol-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Context   jjSourceContext   `json:"context"`
		Inventory jjSourceInventory `json:"inventory"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	fixture.Context.WorkspaceRoot = root
	fixture.Context.RepositoryPath = filepath.Join(root, ".jj", "repo")
	fixture.Inventory.Identity.WorkspaceRoot = root
	fixture.Inventory.Identity.RepositoryPath = fixture.Context.RepositoryPath
	contextData, err := json.Marshal(fixture.Context)
	if err != nil {
		t.Fatal(err)
	}
	inventoryData, err := json.Marshal(fixture.Inventory)
	if err != nil {
		t.Fatal(err)
	}
	read, err := decodeJJSourceRead(contextData, inventoryData)
	if err != nil {
		t.Fatal(err)
	}
	if len(read.Inventory.Identity.Parents) != 3 || len(read.Inventory.Identity.TreeIDs) != 5 || len(read.Inventory.WorkingCopyTreeIDs) != 5 {
		t.Fatalf("octopus identity terms lost: %+v", read.Inventory.Identity)
	}
	pending := slices.ContainsFunc(read.Inventory.Pending, func(entry jjPendingEntry) bool {
		return entry.Path == "src/shared.txt" && entry.Kind == "file_conflict"
	})
	admitted := slices.ContainsFunc(read.Inventory.Admitted, func(entry jjAdmittedFile) bool { return entry.Path == "src/pending.txt" && !entry.TrackedInWorkingCopy })
	if !pending || !admitted {
		t.Fatal("native recorded conflict or newly admitted file fact lost")
	}
}

// Used only with explicitly controlled, synthetic native fixtures.
type jjProcessProofRunner struct {
	evidence   string
	reads      int
	conditions []string
}

func (r *jjProcessProofRunner) Run(ctx context.Context, request LocalCommandRequest) (LocalCommandResult, error) {
	phase := ""
	for _, arg := range request.Args {
		if strings.HasPrefix(arg, "source-") {
			phase = arg
			break
		}
	}
	for _, entry := range request.Env {
		if strings.HasPrefix(entry, "CRABBOX_JJ_FIXTURE_CONDITION=") || strings.HasPrefix(entry, "CRABBOX_JJ_FIXTURE_MODE=") {
			return LocalCommandResult{}, fmt.Errorf("fixture-only condition value entered child environment")
		}
	}
	input, err := io.ReadAll(request.Stdin)
	if err != nil {
		return LocalCommandResult{}, err
	}
	if bytes.Contains(input, []byte("fixture-parent-only-content")) {
		return LocalCommandResult{}, fmt.Errorf("parent-only condition value entered protocol")
	}
	request.Stdin = bytes.NewReader(input)
	result, err := (execCommandRunner{}).Run(ctx, request)
	if phase == "source-context" && result.ExitCode == 0 {
		r.reads++
	}
	if result.ExitCode == jjConditionRequestExit {
		var condition jjEnvironmentCondition
		if decodeErr := decodeJJProtocolMessage([]byte(result.Stdout), "environment_condition", &condition); decodeErr != nil {
			return result, decodeErr
		}
		r.conditions = append(r.conditions, condition.Predicate)
	}
	name := phase
	switch phase {
	case "source-context":
		name = fmt.Sprintf("live-%d-context", r.reads)
	case "source-live-inventory":
		name = fmt.Sprintf("live-%d-inventory", r.reads)
	}
	if writeErr := os.WriteFile(filepath.Join(r.evidence, name+".json"), []byte(result.Stdout), 0600); writeErr != nil {
		return result, writeErr
	}
	if writeErr := os.WriteFile(filepath.Join(r.evidence, name+".stderr"), []byte(result.Stderr), 0600); writeErr != nil {
		return result, writeErr
	}
	return result, err
}

func TestJJRecordedProcessRoundTrip(t *testing.T) {
	fixture, helper := os.Getenv("CRABBOX_TEST_JJ_LIVE_FIXTURE"), jjFixtureHelper(t)
	if fixture == "" || helper == "" {
		t.Skip("requires an owned native JJ fixture and helper")
	}
	root := filepath.Join(fixture, "fixture-source")
	process := jjFixtureProcess(t, root, nil)
	inventory, err := process.readRecorded(t.Context(), "top")
	if err != nil {
		t.Fatal(err)
	}
	exact, err := process.readRecorded(t.Context(), inventory.Identity.Commit)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(inventory, exact) {
		t.Fatal("bookmark and captured commit selected different native inventory")
	}
	changeRevision, err := os.ReadFile(filepath.Join(fixture, "evidence/native-change-revision.txt"))
	if err != nil {
		t.Fatal(err)
	}
	change, err := process.readRecorded(t.Context(), strings.TrimSpace(string(changeRevision)))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(inventory, change) {
		t.Fatal("native change revision selected a different inventory than bookmark/commit")
	}
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	cfg := baseConfig()
	paths := []string{"src/shared.txt", "src/line.txt", "src/tool.sh", "src/link", "src/historical.txt", "src/tree/child.txt", "coverage/recorded.txt"}
	cfg.Sync.Includes = append(append([]string{}, paths...), "state/crabbox/marker.txt")
	snapshot, err := prepareJJRecordedSnapshot(t.Context(), process, inventory.Identity.Commit, cfg, jjRecordedExportLimits{EntryBytes: 1048576, OutputBytes: 1048576}, false, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := snapshot.cleanup(); err != nil {
			t.Error(err)
		}
	})
	if snapshot.nativeState.Root != "" {
		t.Fatal("native checkout state retained after acceptance")
	}
	sort.Strings(paths)
	if !slices.Equal(snapshot.Manifest.Files, paths) || !reflect.DeepEqual(snapshot.Inventory.Identity, inventory.Identity) {
		t.Fatalf("recorded manifest/identity mismatch: %+v", snapshot.Manifest)
	}
	if len(snapshot.Manifest.ProtectedTrackedExcludes) != 1 || snapshot.Manifest.ProtectedTrackedExcludes[0].Path != "coverage/recorded.txt" {
		t.Fatalf("tracked protection lost: %+v", snapshot.Manifest.ProtectedTrackedExcludes)
	}
	for _, path := range paths {
		expected := filepath.Join(fixture, "fixture-golden", path)
		got := filepath.Join(snapshot.Root, path)
		before, err := os.Lstat(expected)
		if err != nil {
			t.Fatal(err)
		}
		after, err := os.Lstat(got)
		if err != nil {
			t.Fatal(err)
		}
		if before.Mode() != after.Mode() {
			t.Fatalf("%s mode changed", path)
		}
		if before.Mode()&os.ModeSymlink != 0 {
			a, err := os.Readlink(expected)
			if err != nil {
				t.Fatal(err)
			}
			b, err := os.Readlink(got)
			if err != nil {
				t.Fatal(err)
			}
			if a != b {
				t.Fatalf("%s link changed", path)
			}
		} else {
			a, err := os.ReadFile(expected)
			if err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(got)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(a, b) {
				t.Fatalf("%s recorded bytes changed", path)
			}
		}
	}
	archive, err := CreateSyncArchive(t.Context(), Repo{Root: snapshot.Root}, snapshot.Manifest, "crabbox-recorded-proof-*.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	archivePath := archive.Name()
	t.Cleanup(func() { archive.Close(); os.Remove(archivePath) })
	gz, err := gzip.NewReader(archive)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var members []string
	for {
		entry, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		members = append(members, entry.Name)
		if entry.Typeflag == tar.TypeReg {
			expected, err := os.ReadFile(filepath.Join(fixture, "fixture-golden", entry.Name))
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(tr)
			if err != nil || !bytes.Equal(data, expected) {
				t.Fatalf("recorded archive bytes differ at %s: %v", entry.Name, err)
			}
		}
	}
	if !slices.Equal(members, paths) {
		t.Fatalf("recorded archive members=%q", members)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(archivePath); err != nil {
		t.Fatal(err)
	}
	emptyConfig := cfg
	emptyConfig.Sync.Includes = []string{"no-selected-content/**"}
	empty, err := prepareJJRecordedSnapshot(t.Context(), process, inventory.Identity.Commit, emptyConfig, jjRecordedExportLimits{}, false, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := empty.cleanup(); err != nil {
			t.Error(err)
		}
	})
	if len(empty.Manifest.Files) != 0 || empty.Manifest.Bytes != 0 || empty.ContentDigest == "" || empty.nativeState.Root != "" {
		t.Fatalf("empty recorded snapshot=%+v", empty.Manifest)
	}
	if err := empty.cleanup(); err != nil {
		t.Fatal(err)
	}
	result := map[string]any{"identity": snapshot.Inventory.Identity, "manifest": snapshot.Manifest, "content_digest": snapshot.ContentDigest, "native_state_removed_before_handoff": true, "archive_from_recorded_snapshot": true, "empty_zero_budget_selection": true}
	stage := snapshot.Root
	if err := snapshot.cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stage); !os.IsNotExist(err) {
		t.Fatalf("recorded staging retained: %v", err)
	}
	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture, "evidence/recorded-process-result.json"), encoded, 0600); err != nil {
		t.Fatal(err)
	}

}

func TestJJEnvironmentPredicates(t *testing.T) {
	env := jjConditionEnvironment([]string{"EMPTY=", "MODE=active", "TEXT=x=y", "UNICODE=日本語"})
	for _, tc := range []struct {
		predicate string
		want      bool
	}{
		{"EMPTY", true}, {"EMPTY=", true}, {"EMPTY=x", false}, {"MODE", true}, {"MODE=active", true},
		{"MODE=inactive", false}, {"mode", false}, {"TEXT=x=y", true}, {"TEXT=x", false}, {"UNICODE=日本語", true}, {"ABSENT", false},
	} {
		t.Run(tc.predicate, func(t *testing.T) {
			if got := matchesJJEnvironmentCondition(env, tc.predicate); got != tc.want {
				t.Fatalf("match=%v want=%v", got, tc.want)
			}
		})
	}
}

func jjFixtureHelper(t *testing.T) string {
	t.Helper()
	if os.Getenv("CRABBOX_TEST_JJ_INSTALLED") == "1" {
		helper, err := installedJJSourceHelper(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		return helper
	}
	return os.Getenv("CRABBOX_TEST_JJ_HELPER")
}

func TestJJInstalledCompanion(t *testing.T) {
	directory := t.TempDir()
	executable := filepath.Join(directory, "crabbox")
	writeFile(t, executable, "ordinary executable marker")
	name := "crabbox-jj-source"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	helper := filepath.Join(directory, name)
	payload := []byte("ordinary companion fixture")
	if err := os.WriteFile(helper, payload, 0755); err != nil {
		t.Fatal(err)
	}
	identity, err := jjsource.ReadIdentity()
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(payload)
	receipt := jjInstalledReceipt{SchemaVersion: 1, NativeVersion: identity.NativeVersion, SourceTreeSHA256: identity.SourceTreeSHA256, BinarySHA256: fmt.Sprintf("%x", hash), TargetOS: runtime.GOOS, TargetArch: runtime.GOARCH}
	data, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "crabbox-jj-source.json"), data, 0644); err != nil {
		t.Fatal(err)
	}
	helper, err = filepath.EvalSymlinks(helper)
	if err != nil {
		t.Fatal(err)
	}
	got, err := installedJJSourceHelperForExecutable(t.Context(), executable, runtime.GOOS, runtime.GOARCH)
	if err != nil || got != helper {
		t.Fatalf("helper=%q: %v", got, err)
	}
	if runtime.GOOS != "windows" {
		launcher := filepath.Join(t.TempDir(), "crabbox")
		if err := os.Symlink(executable, launcher); err != nil {
			t.Fatal(err)
		}
		got, err := installedJJSourceHelperForExecutable(t.Context(), launcher, runtime.GOOS, runtime.GOARCH)
		if err != nil || got != helper {
			t.Fatalf("linked launcher helper=%q: %v", got, err)
		}
	}
}

func jjFixtureProcess(t *testing.T, root string, runner CommandRunner) *jjSourceProcess {
	t.Helper()
	limits := jjSourceProcessLimits{ObjectBytes: 1048576, InputBytes: 1048576, ScratchBytes: 1048576, MetadataBytes: 1048576, Timeout: 30 * time.Second}
	var process *jjSourceProcess
	var err error
	if os.Getenv("CRABBOX_TEST_JJ_INSTALLED") == "1" {
		process, err = newInstalledJJSourceProcess(t.Context(), root, limits, runner)
	} else {
		parent := os.Environ()
		process, err = newJJSourceProcess(t.Context(), jjSourceProcessOptions{BinaryPath: jjFixtureHelper(t), Directory: root, Environment: jjSourceChildEnvironment(parent), ConditionEnvironment: parent, Limits: limits, Runner: runner})
	}
	if err != nil {
		t.Fatal(err)
	}
	return process
}

func TestJJSourceChildEnvironment(t *testing.T) {
	kept := []string{"HOME=/fixture/home", "JJ_CONFIG=/fixture/jj.toml", "JJ_EMAIL=fixture@example.com", "GIT_CONFIG_GLOBAL=/fixture/gitconfig", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.excludesFile", "GIT_CONFIG_VALUE_0=/fixture/ignore", "GIT_WORK_TREE=/fixture/work", "GIT_NO_REPLACE_OBJECTS=1", "GIT_ALLOC_LIMIT=8192", "GIX_OBJECT_CACHE_MEMORY=8192", "LC_CTYPE=C"}
	omitted := []string{"CRABBOX_JJ_FIXTURE_CONDITION=parent-only", "AWS_SESSION_TOKEN=fixture-only", "GITHUB_TOKEN=fixture-only", "JJ_TRACE=fixture-trace", "JJ_PAGER=fixture-pager", "JJ_EDITOR=fixture-editor", "GIT_SSH_COMMAND=fixture-command", "GIT_ASKPASS=fixture-command", "HTTPS_PROXY=fixture-proxy", "LD_PRELOAD=fixture-loader", "GIT_CONFIG_VALUE_EXTRA=not-an-index"}
	parent := append(append([]string{}, kept...), omitted...)
	got := jjSourceChildEnvironment(parent)
	if !slices.Equal(got, kept) {
		t.Fatalf("unexpected native child keys: %v", got)
	}
	conditions := jjConditionEnvironment(parent)
	if !matchesJJEnvironmentCondition(conditions, "CRABBOX_JJ_FIXTURE_CONDITION") {
		t.Fatal("parent condition context lost")
	}
}

func TestJJRecordedSelectionUsesTreeEntries(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	if err := os.MkdirAll(filepath.Join(root, "src", "historical.txt"), 0755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "src", "parent"), "today this is a file\n")
	inventory := jjRecordedInventory{Identity: jjSourceIdentity{WorkspaceRoot: root}, Entries: []jjRecordedEntry{
		{Path: "src/absent.txt", Kind: "file"}, {Path: "src/historical.txt", Kind: "file"}, {Path: "src/parent/child.txt", Kind: "file"},
		{Path: "state/crabbox/marker.txt", Kind: "file"}, {Path: "coverage/recorded.txt", Kind: "file"},
	}}
	selection, err := selectJJRecordedPaths(t.Context(), inventory, baseConfig())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"coverage/recorded.txt", "src/absent.txt", "src/historical.txt", "src/parent/child.txt"}
	if !slices.Equal(selection.Paths, want) {
		t.Fatalf("recorded paths=%q want=%q", selection.Paths, want)
	}
}

func TestJJRecordedScopeRetainsTerminalBoundaries(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", "")
	inventory := jjRecordedInventory{Identity: jjSourceIdentity{WorkspaceRoot: root}, Entries: []jjRecordedEntry{
		{Path: "module", Kind: "submodule"},
		{Path: "structural", Kind: "other_conflict", Terms: []*jjRecordedTreeTerm{{Kind: "tree"}, nil, {Kind: "file"}}},
	}}
	cfg := baseConfig()
	selected, err := selectJJRecordedPaths(t.Context(), inventory, cfg)
	if err != nil || !slices.Equal(selected.Paths, []string{"structural"}) {
		t.Fatalf("terminal selection=%q: %v", selected.Paths, err)
	}
	for _, path := range []string{"module/child.txt", "structural/child.txt"} {
		cfg.Sync.Includes = []string{path}
		_, err := selectJJRecordedPaths(t.Context(), inventory, cfg)
		var incomplete *jjIncompleteSourceError
		if !errors.As(err, &incomplete) {
			t.Fatalf("selected boundary descendants accepted for %s: %v", path, err)
		}
	}
}
