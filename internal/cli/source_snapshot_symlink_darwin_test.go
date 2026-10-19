package cli

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"

	"golang.org/x/sys/unix"
)

func TestLocalGitSeedSnapshotPreservesDarwinSymlinkMode(t *testing.T) {
	if !snapshotSymlinkModeChild(t) {
		return
	}
	repo, cfg := newLocalGitSnapshotFixture(t)
	link := filepath.Join(repo.Root, "link")
	if err := os.Symlink("clean.txt", link); err != nil {
		t.Fatal(err)
	}
	if err := unix.Fchmodat(unix.AT_FDCWD, link, 0o700, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		t.Fatal(err)
	}
	source := observeSnapshotSymlink(t, repo.Root, "link")
	targetPath := filepath.Join(repo.Root, "clean.txt")
	target, err := os.Stat(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	targetBytes, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	var roots []string
	snapshot, snapshotErr := prepareLocalGitSeedSnapshotWithHook(context.Background(), repo, cfg, func(phase string, attempt int, root string) {
		if phase != "snapshot_copied" {
			return
		}
		roots = append(roots, root)
		copied := observeSnapshotSymlink(t, root, "link")
		t.Logf("attempt=%d sourceMode=%#o copiedMode=%#o sourceSize=%d copiedSize=%d sourceTarget=%q copiedTarget=%q fingerprintsEqual=%t", attempt, source.mode.Perm(), copied.mode.Perm(), source.size, copied.size, source.target, copied.target, source.fingerprint == copied.fingerprint)
	})
	if snapshotErr == nil {
		t.Cleanup(func() {
			if err := snapshot.cleanup(); err != nil {
				t.Error(err)
			}
		})
	}
	if got := observeSnapshotSymlink(t, repo.Root, "link"); got != source {
		t.Fatalf("source symlink changed: %+v, want %+v", got, source)
	}
	afterTarget, err := os.Stat(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	afterBytes, err := os.ReadFile(targetPath)
	if err != nil || !sameSourceSnapshotIdentity(target, afterTarget) || string(afterBytes) != string(targetBytes) {
		t.Fatalf("source target changed: %v", err)
	}
	if snapshotErr != nil {
		for _, root := range roots {
			if _, err := os.Lstat(root); !os.IsNotExist(err) {
				t.Errorf("rejected snapshot was not cleaned: %v", err)
			}
		}
		t.Fatalf("stable source with imported symlink mode rejected after %d copies: %v", len(roots), snapshotErr)
	}
	if got := observeSnapshotSymlink(t, snapshot.Root, "link"); got != source {
		t.Fatalf("copied symlink changed: %+v, want %+v", got, source)
	}
	if len(roots) != 1 {
		t.Fatalf("stable source took %d attempts, want 1", len(roots))
	}
}

func TestGitOverlaySnapshotPreservesDarwinSymlinkMode(t *testing.T) {
	if !snapshotSymlinkModeChild(t) {
		return
	}
	fixture := newGitOverlayFixture(t)
	const relative = "mode-link"
	if err := os.Symlink("clean.txt", filepath.Join(fixture.root, relative)); err != nil {
		t.Fatal(err)
	}
	if err := unix.Fchmodat(unix.AT_FDCWD, filepath.Join(fixture.root, relative), 0o700, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		t.Fatal(err)
	}
	source := observeSnapshotSymlink(t, fixture.root, relative)
	_, excludes := fixture.manifest(t)
	snapshot, err := prepareGitOverlaySnapshot(context.Background(), fixture.repo, fixture.cfg, excludes, nil, fixture.plan)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := snapshot.cleanup(); err != nil {
			t.Error(err)
		}
	})
	if got := observeSnapshotSymlink(t, snapshot.Root, relative); got != source {
		t.Fatalf("overlay symlink changed: %+v, want %+v", got, source)
	}
	if got := observeSnapshotSymlink(t, fixture.root, relative); got != source {
		t.Fatalf("source symlink changed: %+v, want %+v", got, source)
	}
	assertGitOverlaySnapshotFingerprint(t, fixture.repo, fixture.cfg, excludes, fixture.plan, snapshot)
}

func snapshotSymlinkModeChild(t *testing.T) bool {
	t.Helper()
	const child = "CRABBOX_TEST_SYMLINK_MODE_CHILD"
	if os.Getenv(child) == t.Name() {
		// Umask is process-wide, so only this dedicated test child changes it.
		unix.Umask(0o022)
		return true
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=^"+regexp.QuoteMeta(t.Name())+"$", "-test.v", "-test.timeout=30s")
	command.Env = append(os.Environ(), child+"="+t.Name())
	output, err := command.CombinedOutput()
	t.Logf("isolated child:\n%s", output)
	if err != nil {
		t.Fatal(err)
	}
	return false
}

func TestSourceSnapshotDarwinSymlinkModeDoesNotChangeTarget(t *testing.T) {
	for _, dangling := range []bool{false, true} {
		t.Run(fmt.Sprintf("dangling=%t", dangling), func(t *testing.T) {
			sourceRoot, snapshotRoot, outside := t.TempDir(), t.TempDir(), t.TempDir()
			target := filepath.Join(outside, "target")
			if !dangling {
				if err := os.WriteFile(target, []byte("outside target\n"), 0o640); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(target, filepath.Join(sourceRoot, "link")); err != nil {
				t.Fatal(err)
			}
			if err := unix.Fchmodat(unix.AT_FDCWD, filepath.Join(sourceRoot, "link"), 0o700, unix.AT_SYMLINK_NOFOLLOW); err != nil {
				t.Fatal(err)
			}
			source := observeSnapshotSymlink(t, sourceRoot, "link")
			before, _ := os.Stat(target)
			if err := copySourceSnapshot(context.Background(), sourceRoot, snapshotRoot, []string{"link"}); err != nil {
				t.Fatal(err)
			}
			if got := observeSnapshotSymlink(t, snapshotRoot, "link"); got != source {
				t.Fatalf("copied link=%+v want %+v", got, source)
			}
			after, err := os.Stat(target)
			if dangling {
				if !os.IsNotExist(err) {
					t.Fatalf("dangling target created: %v", err)
				}
			} else {
				contents, readErr := os.ReadFile(target)
				if err != nil || readErr != nil || !sameSourceSnapshotIdentity(before, after) || string(contents) != "outside target\n" {
					t.Fatalf("outside target changed: %v, %v", err, readErr)
				}
			}
		})
	}
}

func TestSourceSnapshotDarwinSymlinkModeRejectsReplacement(t *testing.T) {
	for _, replacement := range []string{"file", "fifo", "symlink", "parent"} {
		t.Run(replacement, func(t *testing.T) {
			sourceRoot, snapshotRoot, outside := t.TempDir(), t.TempDir(), t.TempDir()
			if err := os.Mkdir(filepath.Join(sourceRoot, "nested"), 0o755); err != nil {
				t.Fatal(err)
			}
			relative := "nested/link"
			if err := os.Symlink("missing", filepath.Join(sourceRoot, relative)); err != nil {
				t.Fatal(err)
			}
			if err := unix.Fchmodat(unix.AT_FDCWD, filepath.Join(sourceRoot, relative), 0o700, unix.AT_SYMLINK_NOFOLLOW); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(outside, "target")
			if err := os.WriteFile(target, []byte("outside\n"), 0o640); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(target)
			if err != nil {
				t.Fatal(err)
			}
			mutated := false
			var replacementInfo os.FileInfo
			err = copySourceSnapshotWithHook(context.Background(), sourceRoot, snapshotRoot, []string{relative}, 1, func(phase string, _ int, _ string) {
				if phase != "before_symlink_mode_restore" {
					return
				}
				mutated = true
				destination := filepath.Join(snapshotRoot, relative)
				if replacement == "parent" {
					parent := filepath.Dir(destination)
					if err := os.Rename(parent, parent+".retained"); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(outside, parent); err != nil {
						t.Fatal(err)
					}
					return
				}
				if err := os.Remove(destination); err != nil {
					t.Fatal(err)
				}
				var err error
				switch replacement {
				case "file":
					err = os.WriteFile(destination, []byte("replacement\n"), 0o640)
				case "fifo":
					err = unix.Mkfifo(destination, 0o600)
				case "symlink":
					err = os.Symlink(target, destination)
				}
				if err != nil {
					t.Fatal(err)
				}
				replacementInfo, err = os.Lstat(destination)
				if err != nil {
					t.Fatal(err)
				}
			})
			if !mutated || err == nil {
				t.Fatalf("replacement was not rejected: mutated=%t, error=%v", mutated, err)
			}
			if replacementInfo != nil {
				after, err := os.Lstat(filepath.Join(snapshotRoot, relative))
				if err != nil || !sameSourceSnapshotIdentity(replacementInfo, after) {
					t.Fatalf("replacement metadata changed: %v", err)
				}
			}
			after, statErr := os.Stat(target)
			contents, readErr := os.ReadFile(target)
			if statErr != nil || readErr != nil || !sameSourceSnapshotIdentity(before, after) || string(contents) != "outside\n" {
				t.Fatalf("outside target changed: %v, %v", statErr, readErr)
			}
		})
	}
}

func TestSourceSnapshotDarwinSymlinkModePreservesSpecialBits(t *testing.T) {
	sourceRoot, snapshotRoot := t.TempDir(), t.TempDir()
	link := filepath.Join(sourceRoot, "link")
	if err := os.Symlink("missing", link); err != nil {
		t.Fatal(err)
	}
	if err := unix.Fchmodat(unix.AT_FDCWD, link, 0o7700, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		t.Fatal(err)
	}
	source := observeSnapshotSymlink(t, sourceRoot, "link")
	if sourceSnapshotSupportedMode(source.mode) != 0o700|os.ModeSetuid|os.ModeSetgid|os.ModeSticky {
		t.Fatalf("fixture lost special bits: %v", source.mode)
	}
	if err := copySourceSnapshot(context.Background(), sourceRoot, snapshotRoot, []string{"link"}); err != nil {
		t.Fatal(err)
	}
	if copied := observeSnapshotSymlink(t, snapshotRoot, "link"); copied != source {
		t.Fatalf("copied symlink=%+v want %+v", copied, source)
	}
}

func TestSourceSnapshotDarwinSymlinkModePinnedLeafRejectsReplacement(t *testing.T) {
	path, outside := t.TempDir(), t.TempDir()
	target := filepath.Join(outside, "target")
	if err := os.WriteFile(target, []byte("outside\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	targetBefore, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(path, "link")); err != nil {
		t.Fatal(err)
	}
	observed, err := os.Lstat(filepath.Join(path, "link"))
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	parent, err := root.Open(".")
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	fd, err := unix.Openat(int(parent.Fd()), "link", unix.O_SYMLINK|unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	link := os.NewFile(uintptr(fd), "link")
	defer link.Close()
	if err := verifySourceSnapshotSymlink(root, "link", link, observed, 0, false); err != nil {
		t.Fatal(err)
	}
	if err := root.Rename("link", "retained"); err != nil {
		t.Fatal(err)
	}
	if err := root.Symlink(target, "link"); err != nil {
		t.Fatal(err)
	}
	replacementBefore, err := root.Lstat("link")
	if err != nil {
		t.Fatal(err)
	}
	if err := link.Chmod(0o700); err != nil {
		t.Fatal(err)
	}
	if err := verifySourceSnapshotSymlink(root, "link", link, observed, 0o700, true); err == nil {
		t.Fatal("post-open replacement accepted")
	}
	targetAfter, err := os.Stat(target)
	if err != nil || !sameSourceSnapshotIdentity(targetBefore, targetAfter) {
		t.Fatalf("outside target changed: %v", err)
	}
	replacementAfter, err := root.Lstat("link")
	if err != nil || !sameSourceSnapshotIdentity(replacementBefore, replacementAfter) {
		t.Fatalf("replacement link changed: %v", err)
	}
}

type snapshotSymlinkObservation struct {
	mode        os.FileMode
	size        int64
	target      string
	fingerprint string
}

func observeSnapshotSymlink(t *testing.T, root, relative string) snapshotSymlinkObservation {
	t.Helper()
	path := filepath.Join(root, relative)
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("expected a symbolic link")
	}
	target, err := os.Readlink(path)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.New()
	if _, err := syncFingerprintPathsAndSize(context.Background(), hash, root, []string{relative}, true, false); err != nil {
		t.Fatal(err)
	}
	return snapshotSymlinkObservation{info.Mode(), info.Size(), target, fmt.Sprintf("%x", hash.Sum(nil))}
}
