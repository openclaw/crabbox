package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

type ownershipMetadata struct {
	uid, gid, mode uint32
	ctime          syscall.Timespec
}

func ownershipStat(t *testing.T, path string) ownershipMetadata {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	s := info.Sys().(*syscall.Stat_t)
	return ownershipMetadata{s.Uid, s.Gid, s.Mode, s.Ctim}
}

func ownershipEntries(t *testing.T, root string) map[string]ownershipMetadata {
	t.Helper()
	result := map[string]ownershipMetadata{}
	if err := filepath.WalkDir(root, func(path string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		result[path] = ownershipStat(t, path)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return result
}

func runOwnershipBlock(t *testing.T, work, cache string) {
	t.Helper()
	cfg := baseConfig()
	cfg.SSHUser, cfg.WorkRoot, cfg.AWSSnapshot = "root", work, "snap-fixture"
	script := cloudInitOwnershipBlock(t, cfg, cache)
	cmd := exec.Command("bash", "--noprofile", "--norc", "-euo", "pipefail", "-c", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ownership block: %v: %s", err, out)
	}
}

func TestCloudInitOwnershipFilesystem(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("arbitrary numeric ownership needs root in a disposable Linux guest")
	}
	for _, scenario := range []string{"correct", "incorrect", "mixed", "symlink roots"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			work, cache, outside := filepath.Join(root, "work's tree"), filepath.Join(root, "cache's tree"), filepath.Join(root, "outside")
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			for _, dir := range []string{work, filepath.Join(work, "nested"), cache, filepath.Join(cache, "npm"), filepath.Join(cache, "pnpm"), outside, filepath.Join(outside, "npm"), filepath.Join(outside, "pnpm")} {
				must(os.Mkdir(dir, 0755))
			}
			for _, file := range []string{"correct", "uid only", "gid\nonly", "nested/-both"} {
				must(os.WriteFile(filepath.Join(work, file), []byte("fixture\n"), 0644))
			}
			must(os.WriteFile(filepath.Join(cache, "npm", "cached"), []byte("cache\n"), 0644))
			must(os.WriteFile(filepath.Join(outside, "file"), []byte("outside\n"), 0644))
			for name, target := range map[string]string{"directory-link": outside, "file-link": filepath.Join(outside, "file"), "dangling-link": filepath.Join(outside, "missing"), "internal-link": "correct", "cycle-link": "."} {
				must(os.Symlink(target, filepath.Join(work, name)))
			}
			if scenario == "symlink roots" {
				work, cache = filepath.Join(root, "work-link"), filepath.Join(root, "cache-link")
				must(os.Symlink(outside, work))
				must(os.Symlink(outside, cache))
			}
			paths := ownershipEntries(t, work)
			for path, stat := range ownershipEntries(t, cache) {
				paths[path] = stat
			}
			for path := range paths {
				must(os.Lchown(path, 0, 0))
				if scenario == "incorrect" || scenario == "symlink roots" {
					must(os.Lchown(path, 12345, 23456))
				}
			}
			if scenario == "mixed" {
				for path, ids := range map[string][2]int{"uid only": {12345, 0}, "gid\nonly": {0, 23456}, "nested": {12345, 23456}, "nested/-both": {12345, 23456}, "dangling-link": {12345, 23456}} {
					must(os.Lchown(filepath.Join(work, path), ids[0], ids[1]))
				}
				must(os.Lchown(filepath.Join(cache, "npm", "cached"), 0, 23456))
			}
			for path := range paths {
				paths[path] = ownershipStat(t, path)
			}
			external := ownershipEntries(t, outside)
			runOwnershipBlock(t, work, cache)
			retained, repaired := 0, 0
			for path, before := range paths {
				after := ownershipStat(t, path)
				if after.uid != 0 || after.gid != 0 {
					t.Errorf("%s: ownership is %d:%d", path, after.uid, after.gid)
				}
				if before.uid == 0 && before.gid == 0 {
					retained++
					if before != after {
						t.Errorf("%s: correct metadata changed: %+v -> %+v", path, before, after)
					}
				} else {
					repaired++
					if before.ctime == after.ctime {
						t.Errorf("%s: repair did not change ctime", path)
					}
				}
				paths[path] = after
			}
			runOwnershipBlock(t, work, cache)
			for path, before := range paths {
				if after := ownershipStat(t, path); before != after {
					t.Errorf("%s: repeat execution changed metadata", path)
				}
			}
			for path, before := range external {
				if after := ownershipStat(t, path); before != after {
					t.Errorf("%s: external target changed", path)
				}
			}
			t.Logf("checked entries=%d; correct before=%d; incorrect before=%d", len(paths), retained, repaired)
		})
	}
}
