package cli

import (
	"context"
	"crypto/sha256"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
)

func TestLocalGitSeedSelection(t *testing.T) {
	root := localSeedTestRepo(t, "sha1")
	head, tree, _ := localSeedTestCommit(t, root, "head")
	base, _, _ := localSeedTestCommit(t, root, "independent base")
	localSeedTestGit(t, root, "", "update-ref", "refs/heads/main", head)
	localSeedTestGit(t, root, "", "update-ref", "refs/remotes/origin/main", base)
	localSeedTestGit(t, root, "", "tag", "-a", "release", head, "-m", "release fixture")
	for _, tc := range []struct {
		name, explicit, inferred, wantBase, wantRef string
		wantError                                   bool
	}{
		{name: "head only"},
		{name: "origin first", explicit: "main", wantBase: base, wantRef: "refs/remotes/origin/main"},
		{name: "explicit local branch", explicit: "refs/heads/main", wantBase: head, wantRef: "refs/heads/main"},
		{name: "annotated tag", explicit: "refs/tags/release", wantBase: head, wantRef: "refs/tags/release"},
		{name: "exact commit", explicit: base, wantBase: base, wantRef: "refs/crabbox/local-base"},
		{name: "unavailable inferred is optional", inferred: "absent"},
		{name: "unavailable explicit is binding", explicit: "absent", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := selectLocalGitSeed(context.Background(), Repo{Root: root, Head: head, BaseRef: tc.inferred}, tc.explicit)
			if (err != nil) != tc.wantError {
				t.Fatalf("error=%v", err)
			}
			if !tc.wantError && (got.Head != head || got.Tree != tree || got.Base != tc.wantBase || got.BaseRef != tc.wantRef) {
				t.Fatalf("selection=%+v", got)
			}
		})
	}
}

func TestLocalGitSeedPreflightCountsFullManifestAndObjects(t *testing.T) {
	clearConfigEnv(t)
	cfg := defaultConfig()
	cfg.Sync.FailBytes = 120
	manifest := SyncManifest{Files: []string{"full"}, Bytes: 100, Changed: []string{"small edit"}, ChangedBytes: 1}
	if err := checkLocalGitSeedPreflight(manifest, 30, cfg, false, io.Discard); err == nil {
		t.Fatal("dirty delta concealed complete metadata transfer")
	}
	if err := checkLocalGitSeedPreflight(manifest, 30, cfg, true, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := checkLocalGitSeedPreflight(manifest, math.MaxInt64, cfg, true, io.Discard); err == nil {
		t.Fatal("overflow accepted")
	}
}

func TestPrepareLocalGitSeedPinsWorkingFilesAndMetadata(t *testing.T) {
	clearConfigEnv(t)
	root := localSeedTestRepo(t, "sha1")
	path := filepath.Join(root, "fixture.txt")
	if err := os.WriteFile(path, []byte("committed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	localSeedTestGit(t, root, "", "add", "fixture.txt")
	localSeedTestGit(t, root, "", "commit", "-m", "initial fixture")
	head := localSeedTestGit(t, root, "", "rev-parse", "HEAD")
	if err := os.WriteFile(path, []byte("accepted dirty bytes\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig()
	cfg.Sync.GitSeedSource = "local"
	seed, err := prepareLocalGitSeed(context.Background(), Repo{Root: root, Head: head}, cfg, false, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := seed.cleanup(); err != nil {
			t.Error(err)
		}
	}()
	if seed.Plan.Head != head || seed.Plan.Fingerprint == "" || seed.Artifact.PackedBytes == 0 {
		t.Fatalf("incomplete plan: %+v", seed.Plan)
	}
	if err := os.WriteFile(path, []byte("later edit\n"), 0600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(seed.Snapshot.Root, "fixture.txt"))
	if err != nil || string(data) != "accepted dirty bytes\n" {
		t.Fatalf("snapshot=%q error=%v", data, err)
	}
	imported := localSeedTestImport(t, seed.Artifact)
	if got := localSeedTestGit(t, imported, "", "show", head+":fixture.txt"); got != "committed" {
		t.Fatalf("committed metadata=%q", got)
	}
	coherence, blocked := syncGitCoherencePlan(cfg, Repo{Root: root, Head: head, RemoteURL: "https://example.invalid/repository.git"})
	if coherence.seedEnabled() || blocked {
		t.Fatal("local mode entered origin routing")
	}
}

func TestLocalGitSeedPreservesGlobalIgnorePolicy(t *testing.T) {
	clearConfigEnv(t)
	root := localSeedTestRepo(t, "sha1")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	ignore := filepath.Join(home, "global-ignore")
	writeFile(t, ignore, "global-only.txt\nreinclude.txt\n")
	localSeedTestGit(t, root, "", "config", "--file", filepath.Join(home, ".gitconfig"), "core.excludesFile", ignore)
	writeFile(t, filepath.Join(root, ".gitignore"), "!reinclude.txt\n")
	writeFile(t, filepath.Join(root, "global-only.txt"), "globally ignored fixture\n")
	writeFile(t, filepath.Join(root, "reinclude.txt"), "included by repository policy\n")
	localSeedTestGit(t, root, "", "add", ".gitignore")
	localSeedTestGit(t, root, "", "commit", "-m", "ignore fixture")
	cfg := defaultConfig()
	excludes, err := syncExcludes(root, cfg)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := localGitSnapshotManifest(context.Background(), root, excludes, nil)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(manifest.Files, "global-only.txt") || !slices.Contains(manifest.Files, "reinclude.txt") {
		t.Fatalf("global ignore/reinclude changed: %v", manifest.Files)
	}
}

func TestLocalGitSeedBaseTagMustNameSelectedCommit(t *testing.T) {
	root := localSeedTestRepo(t, "sha1")
	head, tree, _ := localSeedTestCommit(t, root, "selected")
	other, _, _ := localSeedTestCommit(t, root, "other history")
	localSeedTestGit(t, root, "", "update-ref", "refs/heads/main", head)
	localSeedTestGit(t, root, "", "tag", "release", head)
	selection, err := selectLocalGitSeed(context.Background(), Repo{Root: root, Head: head}, "refs/tags/release")
	if err != nil {
		t.Fatal(err)
	}
	localSeedTestGit(t, root, "", "update-ref", "refs/tags/release", other)
	artifact, err := prepareLocalGitSeedArtifact(context.Background(), root, selection, 0)
	defer artifact.cleanup()
	if err == nil {
		t.Fatalf("moved base tag was synthesized at old commit %s / tree %s", head, tree)
	}
}

func TestLocalGitSeedPreservesSourceAdministration(t *testing.T) {
	clearConfigEnv(t)
	repo, cfg := newLocalGitSnapshotFixture(t)
	writeFile(t, filepath.Join(repo.Root, "staged.txt"), "new staged bytes\n")
	runGit(t, repo.Root, "add", "staged.txt")
	metadata := filepath.Join(repo.Root, ".git")
	readMetadata := func() map[string][32]byte {
		files := make(map[string][32]byte)
		err := filepath.WalkDir(metadata, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(metadata, path)
			if err != nil {
				return err
			}
			files[rel] = sha256.Sum256(data)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return files
	}
	before := readMetadata()
	seed, err := prepareLocalGitSeed(context.Background(), repo, cfg, false, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := seed.cleanup(); err != nil {
			t.Error(err)
		}
	}()
	if after := readMetadata(); !reflect.DeepEqual(before, after) {
		t.Fatal("local preparation wrote source Git administration")
	}
}
