package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

type localSeedTestWriter func([]byte) (int, error)

func (f localSeedTestWriter) Write(p []byte) (int, error) { return f(p) }

func TestBoundedGitSeedWriter(t *testing.T) {
	t.Run("exact bound and sticky overflow", func(t *testing.T) {
		var out bytes.Buffer
		w := &boundedGitSeedWriter{dst: &out, remaining: 3}
		if n, err := w.Write([]byte("abc")); n != 3 || err != nil {
			t.Fatalf("exact bound: %d %v", n, err)
		}
		n, err := w.Write([]byte("d"))
		var failure *localGitSeedWriteError
		if n != 0 || !errors.As(err, &failure) || failure.written != 3 || failure.limit != 3 || failure.attempted != 1 {
			t.Fatalf("overflow: %d %v", n, err)
		}
		if n, again := w.Write(nil); n != 0 || again != err || out.String() != "abc" {
			t.Fatalf("first refusal lost: %d %v %q", n, again, out.String())
		}
	})
	for _, short := range []bool{false, true} {
		name := "sink error"
		cause := errors.New("private sink detail")
		if short {
			name, cause = "short write", io.ErrShortWrite
		}
		t.Run(name, func(t *testing.T) {
			calls := 0
			w := &boundedGitSeedWriter{remaining: 3, dst: localSeedTestWriter(func([]byte) (int, error) {
				calls++
				if short {
					return 1, nil
				}
				return 1, cause
			})}
			n, err := w.Write([]byte("abc"))
			var failure *localGitSeedWriteError
			if n != 1 || !errors.Is(err, cause) || !errors.As(err, &failure) || failure.written != 1 || failure.limit != 3 || failure.attempted != 3 {
				t.Fatalf("write failure: %d %v", n, err)
			}
			if strings.Contains(err.Error(), "private sink detail") {
				t.Fatal("sink detail exposed")
			}
			if _, again := w.Write([]byte("d")); again != err || calls != 1 {
				t.Fatalf("write retried after refusal: %v calls=%d", again, calls)
			}
		})
	}
}

func TestLocalGitSeedOutputHelper(t *testing.T) {
	switch os.Getenv("CRABBOX_TEST_SEED_OUTPUT") {
	case "overflow":
		data := make([]byte, 32768)
		for {
			if _, err := os.Stdout.Write(data); err != nil {
				os.Exit(17)
			}
		}
	case "once":
		_, _ = os.Stdout.Write([]byte("output"))
		os.Exit(0)
	case "wait":
		time.Sleep(time.Minute)
		os.Exit(0)
	case "oid-flood":
		data := []byte(strings.Repeat(strings.Repeat("a", 40)+"\n", 100))
		for {
			if _, err := os.Stdout.Write(data); err != nil {
				os.Exit(17)
			}
		}
	case "oid-once":
		_, _ = os.Stdout.Write([]byte(strings.Repeat("a", 40) + "\n"))
		os.Exit(0)
	case "oid-partial":
		_, _ = os.Stdout.Write([]byte(strings.Repeat("a", 40)))
		os.Exit(0)
	case "exit":
		os.Exit(7)
	}
}

func TestLocalGitSeedOutputPreservesWriterAndProcessFailures(t *testing.T) {
	for _, mode := range []string{"overflow", "once", "wait"} {
		t.Run(mode, func(t *testing.T) {
			timeout := 5 * time.Second
			if mode == "wait" {
				timeout = 100 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLocalGitSeedOutputHelper$")
			cmd.Env = append(os.Environ(), "CRABBOX_TEST_SEED_OUTPUT="+mode)
			cmd.WaitDelay = time.Second
			w := &boundedGitSeedWriter{dst: io.Discard, remaining: 1}
			err := runLocalGitSeedCommand(ctx, cmd, w)
			if err == nil || cmd.ProcessState == nil {
				t.Fatalf("missing settled failure: %v", err)
			}
			if mode == "wait" {
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("deadline lost: %v", err)
				}
				return
			}
			var failure *localGitSeedWriteError
			if !errors.As(err, &failure) || failure.limit != 1 {
				t.Fatalf("writer refusal lost: %v", err)
			}
			if mode == "overflow" {
				var exit *exec.ExitError
				if !errors.As(err, &exit) {
					t.Fatalf("secondary process failure lost: %v", err)
				}
			}
		})
	}
}

func TestLocalGitSeedOutputIndependentGitFailure(t *testing.T) {
	out, err := localGitSeedOutput(context.Background(), t.TempDir(), false, "not-a-git-subcommand")
	var exit *exec.ExitError
	var write *localGitSeedWriteError
	if out != "" || !errors.As(err, &exit) || errors.As(err, &write) {
		t.Fatalf("independent Git failure changed: %q %v", out, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if out, err := localGitSeedOutput(ctx, t.TempDir(), false, "version"); out != "" || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %q %v", out, err)
	}
}

func TestLocalGitSeedOutputEnumerationFraming(t *testing.T) {
	for _, format := range []string{"sha1", "sha256"} {
		t.Run(format, func(t *testing.T) {
			width := 40
			if format == "sha256" {
				width = 64
			}
			oid := strings.Repeat("a", width)
			for _, split := range []bool{false, true} {
				var got []string
				records := &localGitSeedEnumeration{format: format, width: width, maxRecords: 2, consume: func(oid string) error {
					got = append(got, oid)
					return nil
				}}
				wire := &boundedGitSeedWriter{dst: records, remaining: int64(2 * (width + 1))}
				data := []byte(oid + "\n" + oid + "\n")
				chunks := [][]byte{data}
				if split {
					chunks = [][]byte{data[:3], data[3:width], data[width : width+2], data[width+2:]}
				}
				for _, chunk := range chunks {
					if n, err := wire.Write(chunk); err != nil || n != len(chunk) {
						t.Fatalf("framing: %d %v", n, err)
					}
				}
				if err := records.finish(); err != nil || !reflect.DeepEqual(got, []string{oid, oid}) || records.records != 2 || len(records.partial) != 0 {
					t.Fatalf("complete records: %v %v", got, err)
				}
				if _, err := wire.Write([]byte("a")); err == nil || !strings.Contains(err.Error(), "byte limit") {
					t.Fatalf("one over wire bound: %v", err)
				}
				if _, err := records.Write([]byte(oid + "\n")); err == nil || !strings.Contains(err.Error(), "record limit") || records.records != 2 {
					t.Fatalf("duplicate over record bound: %v", err)
				}
			}
		})
	}
}

func TestLocalGitSeedOutputEnumerationBeyondMetadataCapture(t *testing.T) {
	for _, format := range []string{"sha1", "sha256"} {
		t.Run(format, func(t *testing.T) {
			width := 40
			if format == "sha256" {
				width = 64
			}
			seen := 0
			records := &localGitSeedEnumeration{format: format, width: width, maxRecords: localGitSeedMaxObjects, consume: func(string) error { seen++; return nil }}
			wire := &boundedGitSeedWriter{dst: records, remaining: int64(localGitSeedMaxObjects * (width + 1))}
			line := []byte(strings.Repeat("b", width) + "\n")
			count := localGitSeedMaxMetadata/len(line) + 1
			for range count {
				if _, err := wire.Write(line); err != nil {
					t.Fatal(err)
				}
			}
			if err := records.finish(); err != nil || seen != count || wire.written <= localGitSeedMaxMetadata || cap(records.partial) > 128 {
				t.Fatalf("streaming bound: records=%d bytes=%d partial=%d error=%v", seen, wire.written, cap(records.partial), err)
			}
		})
	}
}

func TestLocalGitSeedOutputEnumerationRejectsMalformedRecords(t *testing.T) {
	for _, data := range []string{"\n", "short\n", strings.Repeat("a", 41), strings.Repeat("g", 40) + "\n", strings.Repeat("A", 40) + "\n", strings.Repeat("a", 40) + " \n", strings.Repeat("a", 40), "a"} {
		t.Run(strconv.Itoa(len(data))+"-"+strconv.Quote(data[:1]), func(t *testing.T) {
			records := &localGitSeedEnumeration{format: "sha1", width: 40, maxRecords: 2, consume: func(string) error { t.Fatal("malformed record consumed"); return nil }}
			_, writeErr := records.Write([]byte(data))
			err := records.finish()
			if err == nil || (writeErr != nil && err != writeErr) {
				t.Fatalf("malformed/partial output accepted: %v %v", writeErr, err)
			}
			if _, again := records.Write([]byte(strings.Repeat("a", 40) + "\n")); again != err {
				t.Fatal("first framing failure lost")
			}
		})
	}
}

func TestLocalGitSeedOutputEnumerationProcessCustody(t *testing.T) {
	for _, mode := range []string{"oid-once", "oid-partial", "oid-flood", "exit", "wait"} {
		t.Run(mode, func(t *testing.T) {
			timeout := 5 * time.Second
			if mode == "wait" {
				timeout = 100 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLocalGitSeedOutputHelper$")
			cmd.Env = append(os.Environ(), "CRABBOX_TEST_SEED_OUTPUT="+mode)
			cmd.WaitDelay = time.Second
			readFailure := errors.New("local Git seed test reader refused")
			seen := 0
			err := localGitSeedEnumerationOutput(ctx, cmd, "sha1", func(string) error {
				seen++
				if mode == "oid-flood" {
					return readFailure
				}
				return nil
			})
			if cmd.ProcessState == nil {
				t.Fatal("child not settled")
			}
			switch mode {
			case "oid-once":
				if err != nil || seen != 1 {
					t.Fatalf("valid enumeration: %d %v", seen, err)
				}
			case "oid-partial":
				if err == nil || !strings.Contains(err.Error(), "unterminated") || seen != 0 {
					t.Fatalf("partial enumeration accepted: %v", err)
				}
			case "oid-flood":
				var exit *exec.ExitError
				if !errors.Is(err, readFailure) || !errors.As(err, &exit) || seen != 1 {
					t.Fatalf("reader/secondary process failure lost: %d %v", seen, err)
				}
			case "exit":
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != 7 {
					t.Fatalf("Git failure lost: %v", err)
				}
			case "wait":
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("deadline lost: %v", err)
				}
			}
		})
	}
}

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
	if err := os.Remove(filepath.Join(root, ".git", "shallow")); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareLocalGitSeedArtifact(context.Background(), root, localGitSeedSelection{Head: head, Tree: tree}, 0); err == nil || !strings.Contains(err.Error(), "required local Git object") || strings.Contains(err.Error(), "shallow") {
		t.Fatalf("non-shallow missing parent: %v", err)
	}
}

func TestLocalGitSeedArtifactNamesShallowSourceCause(t *testing.T) {
	source := localSeedTestRepo(t, "sha1")
	parent, _, _ := localSeedTestCommit(t, source, "parent")
	head, tree, _ := localSeedTestCommit(t, source, "child", parent)
	localSeedTestGit(t, source, "", "update-ref", "refs/heads/main", head)
	clone := filepath.Join(t.TempDir(), "clone")
	localSeedTestGit(t, source, "", "-c", "protocol.file.allow=always", "clone", "--quiet", "--depth=1", "--no-local", "file://"+filepath.ToSlash(source), clone)
	if got := localSeedTestGit(t, clone, "", "rev-parse", "--is-shallow-repository"); got != "true" {
		t.Fatalf("fixture is not shallow: %s", got)
	}
	_, err := prepareLocalGitSeedArtifact(context.Background(), clone, localGitSeedSelection{Head: head, Tree: tree}, 0)
	if err == nil || !strings.Contains(err.Error(), "required local Git object") || !strings.Contains(err.Error(), "source repository is shallow") || !strings.Contains(err.Error(), "git fetch --unshallow") {
		t.Fatalf("shallow source error=%v", err)
	}
	if !strings.Contains(err.Error(), "set sync.gitSeed=false and sync.gitSeedSource=origin for file-only sync") {
		t.Fatalf("shallow source error lacks a complete file-only remedy: %v", err)
	}
	if strings.Contains(err.Error(), source) || strings.Contains(err.Error(), clone) {
		t.Fatalf("shallow diagnostic leaked a repository path: %v", err)
	}
}

func TestLocalGitSeedArtifactShallowMarkerDoesNotMislabelByteLimit(t *testing.T) {
	root := localSeedTestRepo(t, "sha1")
	parent, _, _ := localSeedTestCommit(t, root, "parent")
	head, tree, _ := localSeedTestCommit(t, root, "child", parent)
	localSeedTestGit(t, root, "", "update-ref", "refs/heads/main", head)
	if err := os.WriteFile(filepath.Join(root, ".git", "shallow"), []byte(head+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := prepareLocalGitSeedArtifact(context.Background(), root, localGitSeedSelection{Head: head, Tree: tree}, 8)
	if err == nil || !strings.Contains(err.Error(), "uncompressed object byte limit exceeded") {
		t.Fatalf("missing byte-limit cause: %v", err)
	}
	if strings.Contains(err.Error(), "source repository is shallow") || strings.Contains(err.Error(), "unshallow") {
		t.Fatalf("byte-limit refusal incorrectly advises fetching history: %v", err)
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
	temp := t.TempDir()
	t.Setenv("TMPDIR", temp)
	t.Setenv("TMP", temp)
	t.Setenv("TEMP", temp)
	limited, err := prepareLocalGitSeedArtifact(context.Background(), root, localGitSeedSelection{Head: head, Tree: tree}, artifact.ObjectBytes)
	if err == nil {
		_ = limited.cleanup()
		t.Fatal("packed bundle exceeded allowed bytes")
	}
	var failure *localGitSeedWriteError
	if !errors.As(err, &failure) || failure.limit != artifact.ObjectBytes || !strings.Contains(err.Error(), "pack complete local Git objects") {
		t.Fatalf("pack overflow refusal lost: %v", err)
	}
	if entries, readErr := os.ReadDir(temp); readErr != nil || len(entries) != 0 {
		t.Fatalf("failed pack retained temporary state: %v %v", entries, readErr)
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
