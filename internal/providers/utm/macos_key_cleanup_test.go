package utm

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func keyCleanupStateAliases(t *testing.T) (string, string) {
	t.Helper()
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	realRoot := filepath.Join(parent, "state")
	if err := os.Mkdir(realRoot, 0700); err != nil {
		t.Fatal(err)
	}
	aliasRoot := filepath.Join(parent, "state-alias")
	if err := os.Symlink(realRoot, aliasRoot); err != nil {
		t.Fatal(err)
	}
	return realRoot, aliasRoot
}

func keyCleanupRecord(t *testing.T, root string) (record, string) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", root)
	r := record{Version: 2, Acquisition: "ipsw", LeaseID: "fixture_lease", GeneratedKey: true}
	dir, err := macOSKeyDir(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"id_ed25519", "id_ed25519.pub", "known_hosts"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("inert fixture "+name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	r.SSHKey = filepath.Join(dir, "id_ed25519")
	return r, dir
}

func TestMacOSGeneratedKeyCleanupStateAliases(t *testing.T) {
	for _, direction := range []string{"alias-to-real", "real-to-alias"} {
		t.Run(direction, func(t *testing.T) {
			realRoot, aliasRoot := keyCleanupStateAliases(t)
			original, current := aliasRoot, realRoot
			if direction == "real-to-alias" {
				original, current = realRoot, aliasRoot
			}
			r, dir := keyCleanupRecord(t, original)
			unrelated := filepath.Join(original, "crabbox", "utm", "keys-another_lease")
			if err := os.Mkdir(unrelated, 0700); err != nil {
				t.Fatal(err)
			}
			sentinel := filepath.Join(unrelated, "id_ed25519")
			if err := os.WriteFile(sentinel, []byte("unrelated fixture"), 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("XDG_STATE_HOME", current)
			if err := (&backend{}).removeGeneratedKey(r); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(dir); !os.IsNotExist(err) {
				t.Fatalf("owned key directory remains: %v", err)
			}
			data, err := os.ReadFile(sentinel)
			if err != nil || string(data) != "unrelated fixture" {
				t.Fatal("unrelated key changed")
			}
			if err := (&backend{}).removeGeneratedKey(r); err != nil {
				t.Fatalf("repeat cleanup: %v", err)
			}
		})
	}
}

func TestMacOSGeneratedKeyCleanupRejectsUnownedPaths(t *testing.T) {
	for _, kind := range []string{"foreign-state", "other-lease", "wrong-file", "relative", "unclean", "recorded-directory-symlink", "current-directory-symlink", "current-directory-file"} {
		t.Run(kind, func(t *testing.T) {
			realRoot, aliasRoot := keyCleanupStateAliases(t)
			r, dir := keyCleanupRecord(t, realRoot)
			foreign := filepath.Join(t.TempDir(), "keys-"+r.LeaseID)
			if err := os.Mkdir(foreign, 0700); err != nil {
				t.Fatal(err)
			}
			sentinel := filepath.Join(foreign, "id_ed25519")
			if err := os.WriteFile(sentinel, []byte("foreign fixture"), 0600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "foreign-state":
				r.SSHKey = sentinel
			case "other-lease":
				r.SSHKey = filepath.Join(filepath.Dir(dir), "keys-other_lease", "id_ed25519")
			case "wrong-file":
				r.SSHKey = filepath.Join(dir, "another-file")
			case "relative":
				r.SSHKey = filepath.Join("keys-"+r.LeaseID, "id_ed25519")
			case "unclean":
				r.SSHKey = dir + string(os.PathSeparator) + ".." + string(os.PathSeparator) + filepath.Base(dir) + string(os.PathSeparator) + "id_ed25519"
			case "recorded-directory-symlink":
				// The recorded leaf directory aliases the owned one, but is not itself an owned directory.
				if err := os.Symlink(dir, filepath.Join(filepath.Dir(dir), "keys-other_lease")); err != nil {
					t.Fatal(err)
				}
				r.SSHKey = filepath.Join(filepath.Dir(dir), "keys-other_lease", "id_ed25519")
			case "current-directory-symlink", "current-directory-file":
				if err := os.Rename(dir, dir+"-preserved"); err != nil {
					t.Fatal(err)
				}
				if kind == "current-directory-symlink" {
					if err := os.Symlink(foreign, dir); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(dir, []byte("replacement fixture"), 0600); err != nil {
					t.Fatal(err)
				}
				r.SSHKey = filepath.Join(aliasRoot, "crabbox", "utm", filepath.Base(dir), "id_ed25519")
			}
			if err := (&backend{}).removeGeneratedKey(r); err == nil {
				t.Fatal("accepted unowned or replaced key path")
			}
			data, err := os.ReadFile(sentinel)
			if err != nil || string(data) != "foreign fixture" {
				t.Fatal("foreign key changed")
			}
			ownedDir := dir
			if kind == "current-directory-symlink" || kind == "current-directory-file" {
				ownedDir += "-preserved"
			}
			for _, name := range []string{"id_ed25519", "id_ed25519.pub", "known_hosts"} {
				data, err := os.ReadFile(filepath.Join(ownedDir, name))
				if err != nil || string(data) != "inert fixture "+name {
					t.Fatalf("owned %s changed before rejection", name)
				}
			}
		})
	}
}

func TestMacOSGeneratedKeyCleanupPartialAliases(t *testing.T) {
	for _, removed := range []string{"id_ed25519", "id_ed25519.pub", "all", "empty-recorded-key"} {
		t.Run(removed, func(t *testing.T) {
			realRoot, aliasRoot := keyCleanupStateAliases(t)
			r, dir := keyCleanupRecord(t, aliasRoot)
			if removed == "empty-recorded-key" {
				r.SSHKey = ""
			} else {
				for _, name := range []string{"id_ed25519", "id_ed25519.pub", "known_hosts"} {
					if removed == "all" || removed == name {
						if err := os.Remove(filepath.Join(dir, name)); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			t.Setenv("XDG_STATE_HOME", realRoot)
			if err := (&backend{}).removeGeneratedKey(r); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(dir); !os.IsNotExist(err) {
				t.Fatalf("partial directory remains: %v", err)
			}
		})
	}
}

func TestMacOSGeneratedKeyCleanupPreservesUnexpectedFiles(t *testing.T) {
	r, dir := keyCleanupRecord(t, t.TempDir())
	sentinel := filepath.Join(dir, "unrelated")
	if err := os.WriteFile(sentinel, []byte("unrelated fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := (&backend{}).removeGeneratedKey(r); err == nil {
		t.Fatal("unexpected file silently removed")
	}
	data, err := os.ReadFile(sentinel)
	if err != nil || string(data) != "unrelated fixture" {
		t.Fatal("unexpected file changed")
	}
	if err := os.Remove(sentinel); err != nil {
		t.Fatal(err)
	}
	if err := (&backend{}).removeGeneratedKey(r); err != nil {
		t.Fatal(err)
	}
}

func TestMacOSReleaseDeletingJournalStateAlias(t *testing.T) {
	b, f := setupMacOS(t)
	realRoot, aliasRoot := keyCleanupStateAliases(t)
	t.Setenv("XDG_STATE_HOME", aliasRoot)
	lease, err := b.Acquire(context.Background(), core.AcquireRequest{Keep: true})
	if err != nil {
		t.Fatal(err)
	}
	r, err := b.lookup(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	keyDir := filepath.Dir(r.SSHKey)
	if err := b.stampActivity(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	activity, err := activityPath(r.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	r.State = "deleting"
	r.Labels["state"] = "deleting"
	if err := saveRecord(r); err != nil {
		t.Fatal(err)
	}
	// Reproduce the retained failure state: owned VM is already absent, while its journal and keys remain.
	for i, v := range f.base.vms {
		if v.ID == r.VMID {
			f.base.vms = append(f.base.vms[:i], f.base.vms[i+1:]...)
			break
		}
	}
	before := len(f.base.actions)
	sourceBefore, err := os.ReadFile(b.cfg.UTM.IPSW)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", realRoot)
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if len(f.base.actions) != before {
		t.Fatal("absent VM recovery repeated a control mutation")
	}
	if _, err := os.Lstat(keyDir); !os.IsNotExist(err) {
		t.Fatalf("owned key directory remains: %v", err)
	}
	if records, err := b.records(); err != nil || len(records) != 0 {
		t.Fatalf("recovered journal remains: %v", err)
	}
	if _, err := os.Lstat(activity); !os.IsNotExist(err) {
		t.Fatalf("released activity remains: %v", err)
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(lease.LeaseID); err != nil || exists {
		t.Fatalf("released claim remains: %v", err)
	}
	sourceAfter, err := os.ReadFile(b.cfg.UTM.IPSW)
	if err != nil || string(sourceAfter) != string(sourceBefore) {
		t.Fatal("source IPSW changed")
	}
	if _, ok := findVM(f.base.vms, sourceUUID); !ok {
		t.Fatal("source VM changed")
	}
	if _, ok := findVM(f.base.vms, otherUUID); !ok {
		t.Fatal("unrelated VM changed")
	}
}
