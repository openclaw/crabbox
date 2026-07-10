package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTestboxKeyPathRejectsTraversalIDs(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	for _, leaseID := range []string{"../target", "nested/target", `nested\target`, " cbx_123 "} {
		if path, err := testboxKeyPath(leaseID); err == nil {
			t.Fatalf("testboxKeyPath(%q)=%q, want error", leaseID, path)
		}
	}
}

func TestTestboxKeyPathAllowsSafeCustomIDs(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	path, err := testboxKeyPath("morphvm_123")
	if err != nil {
		t.Fatal(err)
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(configDir, "crabbox", "testboxes", "morphvm_123", "id_ed25519")
	if path != want {
		t.Fatalf("testboxKeyPath()=%q want %q", path, want)
	}
}

func TestSyncAndRemoveStoredTestboxKey(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const leaseID = "cbx_durable_key"
	keyPath, _, err := ensureTestboxKey(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if err := syncStoredTestboxKey(leaseID); err != nil {
		t.Fatal(err)
	}
	if err := removeStoredTestboxKeyWithError(leaseID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatalf("removed key stat error=%v", err)
	}
}

func TestSyncStoredTestboxKeyStopsAtUserConfigBoundary(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	configDir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	const leaseID = "cbx_bounded_key_sync"
	keyPath, _, err := ensureTestboxKey(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	var synced []string
	if err := syncStoredTestboxKeyWithSync(leaseID, func(dir string) error {
		synced = append(synced, filepath.Clean(dir))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		filepath.Dir(keyPath),
		filepath.Dir(filepath.Dir(keyPath)),
		filepath.Dir(filepath.Dir(filepath.Dir(keyPath))),
		filepath.Clean(configDir),
	}
	if len(synced) != len(want) {
		t.Fatalf("synced=%q want=%q", synced, want)
	}
	for index := range want {
		if synced[index] != filepath.Clean(want[index]) {
			t.Fatalf("synced=%q want=%q", synced, want)
		}
	}
}

func TestUseLeaseKnownHostsScopesAndEnforcesHostVerification(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	const leaseID = "cbx_abcdef123456"
	target := SSHTarget{User: "root", Host: "provider-resource", Port: "22"}
	if err := useLeaseKnownHosts(&target, leaseID); err != nil {
		t.Fatal(err)
	}
	keyPath, err := testboxKeyPath(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(filepath.Dir(keyPath), "known_hosts")
	if target.KnownHostsFile != want {
		t.Fatalf("KnownHostsFile=%q want %q", target.KnownHostsFile, want)
	}
	if info, err := os.Stat(filepath.Dir(want)); err != nil {
		t.Fatalf("stat lease SSH directory: %v", err)
	} else if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("lease SSH directory mode=%#o want private", info.Mode().Perm())
	}

	args := strings.Join(sshBaseArgs(target), " ")
	for _, wantArg := range []string{"StrictHostKeyChecking=accept-new", "UserKnownHostsFile=" + sshConfigFileValue(want)} {
		if !strings.Contains(args, wantArg) {
			t.Fatalf("ssh args missing %q: %s", wantArg, args)
		}
	}
	for _, forbidden := range []string{"StrictHostKeyChecking=no", "UserKnownHostsFile=/dev/null"} {
		if strings.Contains(args, forbidden) {
			t.Fatalf("ssh args contain insecure option %q: %s", forbidden, args)
		}
	}
}

func TestUseLeaseKnownHostsFailsClosedWhenDirectoryCannotBePrepared(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	configDir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	testboxesPath := filepath.Join(configDir, "crabbox", "testboxes")
	if err := os.MkdirAll(filepath.Dir(testboxesPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(testboxesPath, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := SSHTarget{KnownHostsFile: "unchanged"}
	if err := useLeaseKnownHosts(&target, "cbx_abcdef123456"); err == nil {
		t.Fatal("useLeaseKnownHosts succeeded with an unusable lease directory")
	}
	if target.KnownHostsFile != "unchanged" {
		t.Fatalf("KnownHostsFile changed after preparation failure: %q", target.KnownHostsFile)
	}
}
