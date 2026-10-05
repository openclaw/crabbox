//go:build darwin || linux

package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

func TestSeededSyncDeltaRoundTripReusedWorkspace(t *testing.T) {
	for _, exact := range []bool{false, true} {
		t.Run(map[bool]string{false: "branch", true: "exact-commit"}[exact], func(t *testing.T) {
			fixture := newGitOverlayFixture(t)
			plan := fixture.plan
			if exact {
				plan.Branch = ""
			}
			remote := filepath.Join(t.TempDir(), "remote workspace")
			run := func(command string, input []byte) []byte {
				t.Helper()
				out, err := runPortableGitControlCommand(t, command, input)
				if err != nil {
					t.Fatalf("remote: %v\n%s", err, out)
				}
				return out
			}
			run(remoteGitSeed(remote, plan), nil)
			run(remoteSeedSyncManifestFromGit(remote), nil)
			mustWriteTestFile(t, filepath.Join(fixture.root, "staged.txt"), "staged edit\n")
			mustWriteTestFile(t, filepath.Join(fixture.root, "added.txt"), "added\n")
			runGit(t, fixture.root, "add", "staged.txt", "added.txt")
			mustWriteTestFile(t, filepath.Join(fixture.root, "unstaged.txt"), "unstaged edit\n")
			runGit(t, fixture.root, "rm", "deleted.txt")
			runGit(t, fixture.root, "mv", "renamed.txt", "new name\n.txt")
			mustWriteTestFile(t, filepath.Join(fixture.root, "untracked.txt"), "untracked\n")
			mustWriteTestFile(t, filepath.Join(fixture.root, "node_modules", "ignored"), "ignored\n")
			if err := os.Chmod(filepath.Join(fixture.root, "mode.sh"), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(fixture.root, "link")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("missing", filepath.Join(fixture.root, "link")); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(filepath.Join(fixture.root, "staged.txt"), 0o640); err != nil {
				t.Fatal(err)
			}
			for iteration := 0; iteration < 2; iteration++ {
				if iteration == 1 {
					// A local revert must restore the old remote delta, and remote test edits
					// must be repaired even when that path is locally clean.
					mustWriteTestFile(t, filepath.Join(fixture.root, "unstaged.txt"), "base unstaged.txt\n")
					mustWriteTestFile(t, filepath.Join(remote, "clean.txt"), "remote test edit\n")
					if err := os.Chmod(filepath.Join(remote, "clean.txt"), 0o600); err != nil {
						t.Fatal(err)
					}
					if err := os.Remove(filepath.Join(fixture.root, "untracked.txt")); err != nil {
						t.Fatal(err)
					}
				}
				manifest, _ := fixture.manifest(t)
				if err := validateGitOverlayManifest(fixture.repo, manifest); err != nil {
					t.Fatal(err)
				}
				changes := run(remoteSeededSyncChanges(remote, plan), nil)
				delta, count, _, ok := seededSyncTransferFromChanges(fixture.root, manifest, changes)
				if !ok || count >= len(manifest.Files) {
					t.Fatalf("full transfer: accepted=%t delta=%q", ok, delta)
				}
				if iteration == 0 && bytes.Contains(delta, []byte("clean.txt\x00")) {
					t.Fatalf("clean seeded file selected: %q", delta)
				}
				if iteration == 1 && (!bytes.Contains(delta, []byte("clean.txt\x00")) || !bytes.Contains(delta, []byte("unstaged.txt\x00"))) {
					t.Fatalf("reused delta misses repairs: %q", delta)
				}
				run(remoteWriteSyncManifestsNew(remote, coldTestToken), []byte(syncManifestInputForTarget(SSHTarget{}, manifest.NUL(), manifest.DeletedNUL())))
				run(remotePruneSyncManifest(remote, coldTestToken), nil)
				cmd := exec.Command("rsync", "-a", "--checksum", "--from0", "--files-from=-", fixture.root+"/", remote+"/")
				cmd.Stdin = bytes.NewReader(delta)
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("rsync: %v\n%s", err, out)
				}
				for _, name := range manifest.Files {
					localPath, remotePath := filepath.Join(fixture.root, name), filepath.Join(remote, name)
					localInfo, err := os.Lstat(localPath)
					if err != nil {
						t.Fatal(err)
					}
					remoteInfo, err := os.Lstat(remotePath)
					if err != nil {
						t.Fatal(err)
					}
					if localInfo.Mode() != remoteInfo.Mode() {
						t.Fatalf("%s mode: %v != %v", name, localInfo.Mode(), remoteInfo.Mode())
					}
					if localInfo.Mode()&os.ModeSymlink != 0 {
						a, _ := os.Readlink(localPath)
						b, _ := os.Readlink(remotePath)
						if a != b {
							t.Fatalf("link %s: %q != %q", name, a, b)
						}
					} else {
						a, _ := os.ReadFile(localPath)
						b, _ := os.ReadFile(remotePath)
						if !bytes.Equal(a, b) {
							t.Fatalf("contents differ: %s", name)
						}
					}
				}
				for _, name := range []string{"deleted.txt", "renamed.txt", "excluded.txt", "node_modules/ignored"} {
					if _, err := os.Lstat(filepath.Join(remote, name)); !os.IsNotExist(err) {
						t.Fatalf("extra %s: %v", name, err)
					}
				}
				if iteration == 1 {
					if _, err := os.Lstat(filepath.Join(remote, "untracked.txt")); !os.IsNotExist(err) {
						t.Fatalf("stale untracked: %v", err)
					}
				}
				// Publish only the managed manifest, as the production finalizer does.
				if err := os.Rename(filepath.Join(remote, ".git/crabbox", remoteSyncPendingManifestName(coldTestToken)), filepath.Join(remote, ".git/crabbox/sync-manifest")); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestSeededSyncDeltaRejectsUnverifiableRemote(t *testing.T) {
	for _, kind := range []string{"head", "index", "assume-unchanged", "skip-worktree", "attributes", "untracked-attributes"} {
		t.Run(kind, func(t *testing.T) {
			fixture := newGitOverlayFixture(t)
			remote := filepath.Join(t.TempDir(), "remote")
			if out, err := runPortableGitControlCommand(t, remoteGitSeed(remote, fixture.plan), nil); err != nil {
				t.Fatalf("seed: %v %s", err, out)
			}
			switch kind {
			case "head":
				runGit(t, remote, "checkout", "--detach", "HEAD~1")
			case "index":
				mustWriteTestFile(t, filepath.Join(remote, "clean.txt"), "staged")
				runGit(t, remote, "add", "clean.txt")
			case "assume-unchanged":
				runGit(t, remote, "update-index", "--assume-unchanged", "clean.txt")
			case "skip-worktree":
				runGit(t, remote, "update-index", "--skip-worktree", "clean.txt")
			case "untracked-attributes":
				mustWriteTestFile(t, filepath.Join(remote, ".gitattributes"), "*.txt ident\n")
			case "attributes":
				mustWriteTestFile(t, filepath.Join(remote, ".git/info/attributes"), "*.txt text\n")
			}
			if out, err := runPortableGitControlCommand(t, remoteSeededSyncChanges(remote, fixture.plan), nil); err == nil {
				t.Fatalf("unsafe remote accepted: %q", out)
			}
		})
	}
}

func TestSeededSyncDeltaProtocol(t *testing.T) {
	manifest := SyncManifest{Files: []string{"clean", "local", "remote"}, OverlayFiles: []string{"local"}}
	for _, data := range []string{"", seededSyncDeltaMarker + "truncated", seededSyncDeltaMarker + "../escape\x00", seededSyncDeltaMarker + ".gitattributes\x00", seededSyncDeltaMarker + "nested/.gitattributes\x00"} {
		if _, _, _, ok := seededSyncTransferFromChanges(t.TempDir(), manifest, []byte(data)); ok {
			t.Fatalf("accepted %q", data)
		}
	}
	delta, count, _, ok := seededSyncTransferFromChanges(t.TempDir(), manifest, []byte(seededSyncDeltaMarker+"remote\x00excluded\x00remote\x00"))
	if !ok || count != 2 || !slices.Equal(splitNul(delta), []string{"local", "remote"}) {
		t.Fatalf("delta=%q count=%d ok=%t", delta, count, ok)
	}
}

func TestSeededSyncIncludesSpecialPermissionBits(t *testing.T) {
	fixture := newGitOverlayFixture(t)
	for _, special := range []os.FileMode{os.ModeSetuid, os.ModeSetgid, os.ModeSticky} {
		if err := os.Chmod(filepath.Join(fixture.root, "clean.txt"), 0o644|special); err != nil {
			t.Fatal(err)
		}
		manifest, _ := fixture.manifest(t)
		if !slices.Contains(manifest.seededExtraFiles, "clean.txt") {
			t.Fatalf("special mode %v omitted from delta", special)
		}
		delta, _, _, ok := seededSyncTransferFromChanges(fixture.root, manifest, []byte(seededSyncDeltaMarker))
		if !ok || !slices.Contains(splitNul(delta), "clean.txt") {
			t.Fatalf("special mode %v omitted from transfer", special)
		}
	}
}

func TestSeededSyncTextAttributesPreserveWorkingBytes(t *testing.T) {
	fixture := newGitOverlayFixture(t)
	mustWriteTestFile(t, filepath.Join(fixture.root, ".gitattributes"), "* text=auto eol=lf\nforced.bin text\n")
	for name, content := range map[string]string{"windows.txt": "first\r\nsecond\r\n", "forced.bin": "\x00forced\r\n", "auto.bin": "\x00binary\r\n"} {
		mustWriteTestFile(t, filepath.Join(fixture.root, name), content)
	}
	runGit(t, fixture.root, "add", ".")
	runGit(t, fixture.root, "commit", "-qm", "text attributes")
	runGit(t, fixture.root, "push", "-q", "origin", "HEAD:main")
	fixture.repo.Head = gitOutput(fixture.root, "rev-parse", "HEAD")
	fixture.plan, _ = syncGitCoherencePlan(fixture.cfg, fixture.repo)
	remote := filepath.Join(t.TempDir(), "remote")
	if out, err := runPortableGitControlCommand(t, remoteGitSeed(remote, fixture.plan), nil); err != nil {
		t.Fatalf("seed: %v %s", err, out)
	}
	// Git normalizes this remote-only edit to the original blob, so diff-files
	// alone cannot identify it. The line-ending inventory must repair it.
	mustWriteTestFile(t, filepath.Join(remote, "clean.txt"), "base clean.txt\r\n")
	manifest, _ := fixture.manifest(t)
	manifest, err := seededSyncManifest(fixture.repo, manifest)
	if err != nil {
		t.Fatal(err)
	}
	changes, err := runPortableGitControlCommand(t, remoteSeededSyncChanges(remote, fixture.plan), nil)
	if err != nil {
		t.Fatalf("changes: %v %s", err, changes)
	}
	delta, _, _, ok := seededSyncTransferFromChanges(fixture.root, manifest, changes)
	if !ok {
		t.Fatalf("text attributes fell back: %q", changes)
	}
	for _, name := range []string{"windows.txt", "forced.bin", "clean.txt"} {
		if !slices.Contains(splitNul(delta), name) {
			t.Fatalf("missing %s in %q", name, delta)
		}
	}
	if slices.Contains(splitNul(delta), "auto.bin") || slices.Contains(splitNul(delta), "staged.txt") {
		t.Fatalf("unchanged bytes selected: %q", delta)
	}
	cmd := exec.Command("rsync", "-a", "--checksum", "--from0", "--files-from=-", fixture.root+"/", remote+"/")
	cmd.Stdin = bytes.NewReader(delta)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("rsync: %v %s", err, out)
	}
	for _, name := range []string{"windows.txt", "forced.bin", "auto.bin", "clean.txt"} {
		want, _ := os.ReadFile(filepath.Join(fixture.root, name))
		got, _ := os.ReadFile(filepath.Join(remote, name))
		if !bytes.Equal(got, want) {
			t.Fatalf("working bytes differ for %s: %q != %q", name, got, want)
		}
	}
}

func TestSeededSyncDoesNotTrustRelaxedRemoteStatSettings(t *testing.T) {
	fixture := newGitOverlayFixture(t)
	remote := filepath.Join(t.TempDir(), "remote")
	if out, err := runPortableGitControlCommand(t, remoteGitSeed(remote, fixture.plan), nil); err != nil {
		t.Fatalf("seed: %v %s", err, out)
	}
	runGit(t, remote, "config", "core.trustctime", "false")
	runGit(t, remote, "config", "core.checkStat", "minimal")
	path := filepath.Join(remote, "clean.txt")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	mustWriteTestFile(t, path, "edit clean.txt\n") // same size as "base clean.txt\n"
	if err := os.Chtimes(path, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	out, err := runPortableGitControlCommand(t, remoteSeededSyncChanges(remote, fixture.plan), nil)
	if err != nil {
		t.Fatalf("probe: %v %s", err, out)
	}
	manifest, _ := fixture.manifest(t)
	delta, _, _, ok := seededSyncTransferFromChanges(fixture.root, manifest, out)
	if !ok || !slices.Contains(splitNul(delta), "clean.txt") {
		t.Fatalf("remote edit missed: %q", out)
	}
	for key, value := range map[string]string{"core.trustctime": "false", "core.checkStat": "minimal", "core.ignorestat": "true"} {
		runGit(t, fixture.root, "config", key, value)
		if _, err := seededSyncManifest(fixture.repo, manifest); err == nil {
			t.Fatalf("local relaxed setting %s accepted", key)
		}
		runGit(t, fixture.root, "config", "--unset", key)
	}
}
