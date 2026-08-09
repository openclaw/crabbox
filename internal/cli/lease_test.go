package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
)

func TestNewRunIDUsesCanonicalFormatAndIsUnique(t *testing.T) {
	first := newRunID()
	second := newRunID()
	pattern := regexp.MustCompile(`^run_[a-f0-9]{12}$`)
	if !pattern.MatchString(first) || !pattern.MatchString(second) {
		t.Fatalf("run IDs must use canonical format: first=%q second=%q", first, second)
	}
	if first == second {
		t.Fatalf("run IDs must be unique per invocation: %q", first)
	}
}

func TestLeaseOperationLockSerializesFixedIDKeyCreation(t *testing.T) {
	isolateTestUserDirs(t)
	const leaseID = "cbx_abcdef123456"
	start := make(chan struct{})
	paths := make(chan string, 2)
	errs := make(chan error, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	for range 2 {
		go func() {
			ready.Done()
			<-start
			err := withLeaseIDOperationLock(leaseID, func() error {
				path, _, err := ensureTestboxKey(leaseID)
				if err == nil {
					paths <- path
				}
				return err
			})
			errs <- err
		}()
	}
	ready.Wait()
	close(start)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	first, second := <-paths, <-paths
	if first != second {
		t.Fatalf("key paths differ: %q != %q", first, second)
	}
}

func TestTestboxKeyPathRejectsTraversalIDs(t *testing.T) {
	isolateTestUserDirs(t)

	for _, leaseID := range []string{"../target", "nested/target", `nested\target`, " cbx_123 "} {
		if path, err := testboxKeyPath(leaseID); err == nil {
			t.Fatalf("testboxKeyPath(%q)=%q, want error", leaseID, path)
		}
	}
}

func TestTestboxKeyPathAllowsSafeCustomIDs(t *testing.T) {
	isolateTestUserDirs(t)

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

func TestUseLeaseKnownHostsScopesAndEnforcesHostVerification(t *testing.T) {
	isolateTestUserDirs(t)

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
	isolateTestUserDirs(t)
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
