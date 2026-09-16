package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func localSeedTestGit(t *testing.T, root, input string, args ...string) string {
	t.Helper()
	cmd := exec.Command(gitOverlayGitExecutable, args...)
	cmd.Dir = root
	cmd.Env = append(gitOverlayGitEnvironment(), "GIT_AUTHOR_NAME=Example Author", "GIT_AUTHOR_EMAIL=author@example.com", "GIT_COMMITTER_NAME=Example Author", "GIT_COMMITTER_EMAIL=author@example.com", "GIT_AUTHOR_DATE=2026-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2026-01-01T00:00:00Z", "GIT_CONFIG_COUNT=0")
	cmd.Stdin = strings.NewReader(input)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func localSeedTestRepo(t *testing.T, format string) string {
	t.Helper()
	root := t.TempDir()
	localSeedTestGit(t, root, "", "init", "--object-format="+format, "--initial-branch=main")
	return root
}

func localSeedTestCommit(t *testing.T, root, content string, parents ...string) (string, string, string) {
	t.Helper()
	blob := localSeedTestGit(t, root, content, "hash-object", "-w", "--stdin")
	tree := localSeedTestGit(t, root, "100644 blob "+blob+"\tfixture.txt\n", "mktree")
	args := []string{"commit-tree", tree, "-m", content}
	for _, parent := range parents {
		args = append(args, "-p", parent)
	}
	commit := localSeedTestGit(t, root, "", args...)
	return commit, tree, blob
}

func localSeedTestImport(t *testing.T, artifact localGitSeedArtifact) string {
	t.Helper()
	root := localSeedTestRepo(t, artifact.ObjectFormat)
	localSeedTestGit(t, root, "", "bundle", "verify", artifact.Path)
	localSeedTestGit(t, root, "", "fetch", artifact.Path, "+refs/*:refs/*")
	localSeedTestGit(t, root, "", "fsck", "--full", "--strict")
	return root
}

func TestLocalGitSeedArtifactPreservesSelectedHistoriesAndTags(t *testing.T) {
	for _, format := range []string{"sha1", "sha256"} {
		t.Run(format, func(t *testing.T) {
			root := localSeedTestRepo(t, format)
			ancestor, _, _ := localSeedTestCommit(t, root, "ancestor")
			left, _, _ := localSeedTestCommit(t, root, "left", ancestor)
			right, _, _ := localSeedTestCommit(t, root, "right", ancestor)
			head, tree, _ := localSeedTestCommit(t, root, "merge", left, right)
			base, _, _ := localSeedTestCommit(t, root, "independent base")
			unrelated, _, unrelatedBlob := localSeedTestCommit(t, root, "unrelated")
			localSeedTestGit(t, root, "", "update-ref", "refs/heads/main", head)
			localSeedTestGit(t, root, "", "update-ref", "refs/heads/unrelated", unrelated)
			localSeedTestGit(t, root, "", "tag", "-a", "v1", ancestor, "-m", "first version")
			localSeedTestGit(t, root, "", "tag", "-a", "nested", "v1", "-m", "nested annotation")
			localSeedTestGit(t, root, "", "tag", "light", left)
			localSeedTestGit(t, root, "", "tag", "independent", base)
			localSeedTestGit(t, root, "", "tag", "other", unrelated)
			localSeedTestGit(t, root, "", "tag", "tree-only", tree)
			localSeedTestGit(t, root, "", "config", "fixture.marker", "source configuration")
			localSeedTestGit(t, root, "", "repack", "-a", "-d")
			artifact, err := prepareLocalGitSeedArtifact(context.Background(), root, localGitSeedSelection{Head: head, Tree: tree, Base: base, BaseRef: "refs/remotes/origin/main"}, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer artifact.cleanup()
			imported := localSeedTestImport(t, artifact)
			if got := localSeedTestGit(t, imported, "", "rev-parse", "refs/crabbox/local-head^{tree}"); got != tree {
				t.Fatalf("tree %s != %s", got, tree)
			}
			if got := localSeedTestGit(t, imported, "", "rev-list", "--parents", head); got != localSeedTestGit(t, root, "", "rev-list", "--parents", head) {
				t.Fatal("selected parent graph changed")
			}
			for _, args := range [][]string{{"describe", head}, {"describe", "--tags", head}, {"diff", ancestor, head}, {"merge-base", left, right}} {
				if got, want := localSeedTestGit(t, imported, "", args...), localSeedTestGit(t, root, "", args...); got != want {
					t.Fatalf("%v differs: %q != %q", args, got, want)
				}
			}
			names := make([]string, len(artifact.Refs))
			for i, ref := range artifact.Refs {
				names[i] = ref.Name
			}
			want := []string{"refs/crabbox/local-head", "refs/remotes/origin/main", "refs/tags/independent", "refs/tags/light", "refs/tags/nested", "refs/tags/v1"}
			if !reflect.DeepEqual(names, want) {
				t.Fatalf("refs %v", names)
			}
			for _, ref := range artifact.Refs {
				if strings.HasPrefix(ref.Name, "refs/tags/") && ref.OID != localSeedTestGit(t, root, "", "rev-parse", ref.Name) {
					t.Fatalf("tag identity changed: %s", ref.Name)
				}
			}
			objectSizes := localSeedTestGit(t, imported, "", "cat-file", "--batch-check=%(objectsize)", "--batch-all-objects")
			var total int64
			for _, size := range strings.Fields(objectSizes) {
				n, err := strconv.ParseInt(size, 10, 64)
				if err != nil {
					t.Fatal(err)
				}
				total += n
			}
			if total != artifact.ObjectBytes || len(strings.Fields(objectSizes)) != artifact.ObjectCount {
				t.Fatal("raw object accounting differs from independent import")
			}
			if _, err := localGitSeedOutput(context.Background(), imported, false, "cat-file", "-e", unrelatedBlob); err == nil {
				t.Fatal("unrelated object was transported")
			}
			if got := localSeedTestGit(t, imported, "", "config", "--local", "--list"); strings.Contains(got, "fixture.marker") || strings.Contains(got, "remote.") {
				t.Fatalf("source config transferred: %s", got)
			}
			data, err := os.ReadFile(artifact.Path)
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(data)
			if artifact.Digest != hex.EncodeToString(sum[:]) || artifact.PackedBytes != int64(len(data)) {
				t.Fatal("artifact accounting mismatch")
			}
			entries, err := os.ReadDir(artifact.Root)
			if err != nil || len(entries) != 1 || entries[0].Name() != "seed.bundle" {
				t.Fatalf("unexpected artifact administration: %v %v", entries, err)
			}
			if _, err := os.Stat(filepath.Join(root, "fixture.txt")); !os.IsNotExist(err) {
				t.Fatal("metadata preparation materialized files")
			}
		})
	}
}

func TestLocalGitSeedArtifactSupportsLinkedWorktreesAndLocalAlternates(t *testing.T) {
	root := localSeedTestRepo(t, "sha1")
	base, _, _ := localSeedTestCommit(t, root, "base")
	head, tree, _ := localSeedTestCommit(t, root, "selected worktree", base)
	localSeedTestGit(t, root, "", "update-ref", "refs/heads/main", base)
	linked := filepath.Join(t.TempDir(), "linked")
	localSeedTestGit(t, root, "", "worktree", "add", "--detach", linked, head)
	alternate := filepath.Join(t.TempDir(), "alternate")
	localSeedTestGit(t, root, "", "clone", "--shared", root, alternate)
	for _, source := range []string{linked, alternate} {
		artifact, err := prepareLocalGitSeedArtifact(context.Background(), source, localGitSeedSelection{Head: head, Tree: tree}, 0)
		if err != nil {
			t.Fatal(err)
		}
		imported := localSeedTestImport(t, artifact)
		if got := localSeedTestGit(t, imported, "", "rev-parse", head+"^"); got != base {
			t.Fatal("lost ancestor")
		}
		if err := artifact.cleanup(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLocalGitSeedArtifactRequiresActualClosureDespiteShallowAndPartialMarkers(t *testing.T) {
	root := localSeedTestRepo(t, "sha1")
	parent, _, _ := localSeedTestCommit(t, root, "parent")
	head, tree, _ := localSeedTestCommit(t, root, "child", parent)
	localSeedTestGit(t, root, "", "update-ref", "refs/heads/main", head)
	if err := os.WriteFile(filepath.Join(root, ".git", "shallow"), []byte(head+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	localSeedTestGit(t, root, "", "config", "core.repositoryformatversion", "1")
	localSeedTestGit(t, root, "", "config", "extensions.partialClone", "origin")
	localSeedTestGit(t, root, "", "config", "remote.origin.promisor", "true")
	artifact, err := prepareLocalGitSeedArtifact(context.Background(), root, localGitSeedSelection{Head: head, Tree: tree}, 0)
	if err != nil {
		t.Fatal(err)
	}
	imported := localSeedTestImport(t, artifact)
	if got := localSeedTestGit(t, imported, "", "rev-list", "--count", head); got != "2" {
		t.Fatalf("shallow marker truncated closure: %s", got)
	}
	if err := artifact.cleanup(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, ".git", "objects", parent[:2], parent[2:])); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareLocalGitSeedArtifact(context.Background(), root, localGitSeedSelection{Head: head, Tree: tree}, 0); err == nil || !strings.Contains(err.Error(), "required local Git object") {
		t.Fatalf("missing actual parent: %v", err)
	}
}

func TestLocalGitSeedArtifactBoundsAndExactSelection(t *testing.T) {
	root := localSeedTestRepo(t, "sha1")
	head, tree, _ := localSeedTestCommit(t, root, "fixture")
	localSeedTestGit(t, root, "", "update-ref", "refs/heads/main", head)
	for _, tc := range []struct {
		name      string
		selection localGitSeedSelection
		limit     int64
	}{
		{"tiny byte allowance", localGitSeedSelection{Head: head, Tree: tree}, 8},
		{"negative byte allowance", localGitSeedSelection{Head: head, Tree: tree}, -1},
		{"nonexact head", localGitSeedSelection{Head: "HEAD", Tree: tree}, 0},
		{"wrong tree", localGitSeedSelection{Head: head, Tree: head}, 0},
		{"base ref without base", localGitSeedSelection{Head: head, Tree: tree, BaseRef: "refs/heads/main"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if artifact, err := prepareLocalGitSeedArtifact(context.Background(), root, tc.selection, tc.limit); err == nil {
				_ = artifact.cleanup()
				t.Fatal("expected rejected selection/limit")
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if artifact, err := prepareLocalGitSeedArtifact(ctx, root, localGitSeedSelection{Head: head, Tree: tree}, 0); err == nil {
		_ = artifact.cleanup()
		t.Fatal("canceled preparation succeeded")
	}
}

func TestLocalGitSeedArtifactGitlinksDoNotRequireSubmoduleObjects(t *testing.T) {
	root := localSeedTestRepo(t, "sha1")
	gitlink := strings.Repeat("1", 40)
	tree := localSeedTestGit(t, root, "160000 commit "+gitlink+"\tdependency\n", "mktree", "--missing")
	head := localSeedTestGit(t, root, "", "commit-tree", tree, "-m", "submodule entry")
	localSeedTestGit(t, root, "", "update-ref", "refs/heads/main", head)
	artifact, err := prepareLocalGitSeedArtifact(context.Background(), root, localGitSeedSelection{Head: head, Tree: tree}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer artifact.cleanup()
	localSeedTestImport(t, artifact)
	if artifact.ObjectCount != 2 {
		t.Fatalf("object count %d", artifact.ObjectCount)
	}
	if artifact.PackedBytes <= artifact.ObjectBytes {
		t.Fatal("fixture must distinguish packed and raw bounds")
	}
	limited, err := prepareLocalGitSeedArtifact(context.Background(), root, localGitSeedSelection{Head: head, Tree: tree}, artifact.ObjectBytes)
	if err == nil {
		_ = limited.cleanup()
		t.Fatal("packed bundle exceeded allowed bytes")
	}
}

func TestLocalGitSeedArtifactPreservesAnnotatedBaseRef(t *testing.T) {
	root := localSeedTestRepo(t, "sha1")
	base, _, _ := localSeedTestCommit(t, root, "version")
	head, tree, _ := localSeedTestCommit(t, root, "next", base)
	localSeedTestGit(t, root, "", "update-ref", "refs/heads/main", head)
	localSeedTestGit(t, root, "", "tag", "-a", "v1", base, "-m", "version annotation")
	tag := localSeedTestGit(t, root, "", "rev-parse", "refs/tags/v1")
	artifact, err := prepareLocalGitSeedArtifact(context.Background(), root, localGitSeedSelection{Head: head, Tree: tree, Base: base, BaseRef: "refs/tags/v1"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer artifact.cleanup()
	imported := localSeedTestImport(t, artifact)
	if got := localSeedTestGit(t, imported, "", "rev-parse", "refs/tags/v1"); got != tag {
		t.Fatalf("annotated base ref identity changed: %s != %s", got, tag)
	}
}

func TestLocalGitSeedArtifactCleanupClearsOwnedState(t *testing.T) {
	root := localSeedTestRepo(t, "sha1")
	head, tree, _ := localSeedTestCommit(t, root, "cleanup fixture")
	localSeedTestGit(t, root, "", "update-ref", "refs/heads/main", head)
	artifact, err := prepareLocalGitSeedArtifact(context.Background(), root, localGitSeedSelection{Head: head, Tree: tree}, 0)
	if err != nil {
		t.Fatal(err)
	}
	path := artifact.Root
	if err := artifact.cleanup(); err != nil {
		t.Fatal(err)
	}
	if artifact.Root != "" || artifact.Path != "" || artifact.owner != nil {
		t.Fatal("cleaned artifact retained its ownership state")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("artifact directory still present: %v", err)
	}
	if err := artifact.cleanup(); err != nil {
		t.Fatalf("repeated cleanup: %v", err)
	}
}
