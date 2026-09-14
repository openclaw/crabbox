package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestNewRunIDUsesCanonicalFormatAndIsUnique(t *testing.T) {
	first, err := newRunID()
	if err != nil {
		t.Fatal(err)
	}
	second, err := newRunID()
	if err != nil {
		t.Fatal(err)
	}
	pattern := regexp.MustCompile(`^run_[a-f0-9]{32}$`)
	if !pattern.MatchString(first) || !pattern.MatchString(second) {
		t.Fatalf("run IDs must use canonical format: first=%q second=%q", first, second)
	}
	if first == second {
		t.Fatalf("run IDs must be unique per invocation: %q", first)
	}
}

func TestNewCreateAttemptIDIsOpaqueAndUnique(t *testing.T) {
	first := newCreateAttemptID()
	second := newCreateAttemptID()
	pattern := regexp.MustCompile(`^cat_[a-f0-9]{32}$`)
	if !pattern.MatchString(first) || !pattern.MatchString(second) {
		t.Fatalf("create attempt IDs must use the opaque canonical format: first=%q second=%q", first, second)
	}
	if first == second {
		t.Fatalf("create attempt IDs must be fresh per acquisition: %q", first)
	}
}

func TestLeaseOperationLockSerializesFixedIDKeyCreation(t *testing.T) {
	dirs := isolateTestUserDirs(t)
	prepareLeaseSSHTestStateRoot(t, dirs.StateHome)
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
	t.Setenv("XDG_STATE_HOME", "")

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

func TestUseStoredTestboxKeyPreservesOptionalFallback(t *testing.T) {
	for _, tc := range []struct {
		name     string
		leaseID  string
		stored   bool
		fallback string
	}{
		{name: "stored key overrides configured key", leaseID: "cbx_optional", stored: true, fallback: "configured-key"},
		{name: "missing key preserves configured key", leaseID: "cbx_optional", fallback: "configured-key"},
		{name: "invalid ID preserves configured key", leaseID: "invalid/id", fallback: "configured-key"},
		{name: "missing key preserves empty key", leaseID: "cbx_optional"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateTestUserDirs(t)
			t.Setenv("XDG_STATE_HOME", "")
			want := tc.fallback
			if tc.stored {
				path, err := testboxKeyPath(tc.leaseID)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				want = path
			}
			target := SSHTarget{Key: tc.fallback}
			UseStoredTestboxKey(&target, tc.leaseID)
			if target.Key != want {
				t.Fatalf("key=%q want %q", target.Key, want)
			}
		})
	}
}

func TestLeaseSSHRootSelection(t *testing.T) {
	dirs := isolateTestUserDirs(t)
	configDir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, root, want string }{
		{"explicit", dirs.StateHome, dirs.StateHome},
		{"legacy empty", "", configDir},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", tc.root)
			got, err := testboxKeyPath("cbx_1516")
			want := filepath.Join(tc.want, "crabbox", "testboxes", "cbx_1516", "id_ed25519")
			if err != nil || got != want {
				t.Fatalf("key path=%q err=%v want %q", got, err, want)
			}
		})
	}
	t.Setenv("XDG_STATE_HOME", "relative-state")
	if _, err := testboxKeyPath("cbx_1516"); err == nil {
		t.Fatal("relative explicit root accepted")
	}
	target := SSHTarget{Key: "external-key"}
	if err := useStoredTestboxKey(&target, "cbx_1516"); err == nil || target.Key != "external-key" {
		t.Fatalf("invalid root must return error without changing target: %+v, %v", target, err)
	}
}

func TestLeaseSSHOptionalDefaultCompatibility(t *testing.T) {
	dirs := isolateTestUserDirs(t)
	t.Setenv("XDG_STATE_HOME", "")
	const invalidID = "invalid/lease"
	if _, err := StoredTestboxKeyPath(invalidID); err == nil {
		t.Fatal("required key lookup lost its error")
	}
	if path, err := OptionalStoredTestboxKeyPath(invalidID); err != nil || path != "" {
		t.Fatalf("legacy optional lookup=%q %v", path, err)
	}
	target := SSHTarget{Key: "external-key"}
	if err := UseStoredTestboxKey(&target, invalidID); err != nil || target.Key != "external-key" {
		t.Fatal("legacy optional key fallback changed")
	}
	t.Setenv("XDG_STATE_HOME", dirs.StateHome)
	if _, err := OptionalStoredTestboxKeyPath(invalidID); err == nil {
		t.Fatal("selected-root error was suppressed")
	}
	if err := UseStoredTestboxKey(&target, invalidID); err == nil || target.Key != "external-key" {
		t.Fatal("selected-root admission error became fallback")
	}
}

func TestLeaseSSHImportedFilePrivacy(t *testing.T) {
	dirs := isolateTestUserDirs(t)
	prepareLeaseSSHTestStateRoot(t, dirs.StateHome)
	const leaseID = "cbx_1516_import"
	path, err := PrepareStoredTestboxKeyPath(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{"synthetic imported bytes", "replacement synthetic bytes"} {
		if err := WritePreparedLeaseSSHKeyFile(path, []byte(content)); err != nil {
			t.Fatal(err)
		}
		if err := verifySSHTransportPathPrivate(path, false); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil || string(data) != content {
			t.Fatal("imported bytes changed")
		}
		if got, err := StoredTestboxKeyPath(leaseID); err != nil || got != path {
			t.Fatalf("stored import path=%q error=%v", got, err)
		}
	}
}

func TestSelectedLeaseSSHRootReuseAndCleanup(t *testing.T) {
	dirs := isolateTestUserDirs(t)
	prepareLeaseSSHTestStateRoot(t, dirs.StateHome)
	externalPaths := []string{
		filepath.Join(dirs.Root, "operator-key"),
		filepath.Join(dirs.Root, "provider-managed-key"),
	}
	externalInfo := make([]os.FileInfo, len(externalPaths))
	for i, path := range externalPaths {
		if err := os.WriteFile(path, []byte("synthetic external identity\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		externalInfo[i] = info
	}
	checkExternalUnchanged := func() {
		t.Helper()
		for i, path := range externalPaths {
			data, err := os.ReadFile(path)
			if err != nil || string(data) != "synthetic external identity\n" {
				t.Fatalf("external identity contents changed: %s: %v", path, err)
			}
			info, err := os.Stat(path)
			before := externalInfo[i]
			if err != nil || !os.SameFile(before, info) || info.Mode() != before.Mode() || info.Size() != before.Size() || !info.ModTime().Equal(before.ModTime()) {
				t.Fatalf("external identity metadata changed: %s: %v", path, err)
			}
		}
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	checkConfigUnchanged := makeLeaseSSHTestConfigReadOnly(t, configDir)
	const leaseID = "cbx_1516"
	key, _, err := ensureTestboxKey(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(key)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ensureTestboxKey(leaseID); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(key)
	if err != nil || string(before) != string(after) {
		t.Fatal("existing generated key changed")
	}
	target := SSHTarget{}
	if err := useStoredTestboxKey(&target, leaseID); err != nil || target.Key != key {
		t.Fatalf("reuse key=%q err=%v", target.Key, err)
	}
	if err := useLeaseKnownHosts(&target, leaseID); err != nil {
		t.Fatal(err)
	}
	if target.KnownHostsFile != filepath.Join(filepath.Dir(key), "known_hosts") {
		t.Fatal("host trust did not share the selected lease root")
	}
	if _, err := os.Lstat(filepath.Join(configDir, "crabbox")); !os.IsNotExist(err) {
		t.Fatalf("default key tree was touched: %v", err)
	}
	otherState := filepath.Join(dirs.Root, "other-state")
	t.Setenv("XDG_STATE_HOME", otherState)
	external := SSHTarget{Key: "external-key"}
	if err := useStoredTestboxKey(&external, leaseID); err != nil || external.Key != "external-key" {
		t.Fatalf("root switch adopted an alternate key: %+v %v", external, err)
	}
	if _, err := os.Stat(key); err != nil {
		t.Fatal("root switch changed the original key")
	}
	for _, path := range externalPaths {
		target := SSHTarget{Key: path}
		if err := UseStoredTestboxKey(&target, leaseID); err != nil || target.Key != path {
			t.Fatalf("selected-root lookup replaced external identity %s: %+v %v", path, target, err)
		}
	}
	if err := RemoveStoredTestboxConnectionArtifacts(leaseID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(otherState, "crabbox", "testboxes")); !os.IsNotExist(err) {
		t.Fatalf("root switch copied or created a key namespace: %v", err)
	}
	if data, err := os.ReadFile(key); err != nil || string(data) != string(before) {
		t.Fatalf("cleanup under the other root changed the original key: %v", err)
	}
	checkExternalUnchanged()
	t.Setenv("XDG_STATE_HOME", dirs.StateHome)
	if err := RemoveStoredTestboxConnectionArtifacts(leaseID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Dir(key)); !os.IsNotExist(err) {
		t.Fatalf("selected lease directory remains: %v", err)
	}
	checkExternalUnchanged()
	checkConfigUnchanged()
}

func TestSelectedLeaseSSHRootDoesNotRepairCallerDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows privacy is verified through ACL tests")
	}
	for _, tc := range []struct {
		name    string
		mode    os.FileMode
		wantErr bool
	}{{"readable base", 0o755, false}, {"other-writable base", 0o777, true}} {
		t.Run(tc.name, func(t *testing.T) {
			dirs := isolateTestUserDirs(t)
			if err := os.Chmod(dirs.StateHome, tc.mode); err != nil {
				t.Fatal(err)
			}
			err := PreflightLeaseSSHStorage()
			if (err != nil) != tc.wantErr {
				t.Fatalf("preflight=%v want error=%t", err, tc.wantErr)
			}
			info, err := os.Stat(dirs.StateHome)
			if err != nil || info.Mode().Perm() != tc.mode {
				t.Fatal("caller-selected directory was silently repaired")
			}
			info, err = os.Lstat(filepath.Join(dirs.StateHome, "crabbox"))
			if tc.wantErr {
				if !os.IsNotExist(err) {
					t.Fatalf("rejected root gained generated namespace: %v", err)
				}
			} else if err != nil || info.Mode().Perm() != 0o700 {
				t.Fatalf("generated namespace is not private: %v", err)
			}
		})
	}
}

func TestSelectedLeaseSSHExistingKeyRequiresPrivateMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows privacy is verified through ACL tests")
	}
	isolateTestUserDirs(t)
	const leaseID = "cbx_1516_mode"
	key, _, err := ensureTestboxKey(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(key, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ensureTestboxKey(leaseID); err == nil {
		t.Fatal("existing-key fast path accepted a non-private generated key")
	}
	target := SSHTarget{}
	if err := useStoredTestboxKey(&target, leaseID); err == nil || target.Key != "" {
		t.Fatalf("use must report invalid generated key without assigning it: %+v %v", target, err)
	}
	info, err := os.Stat(key)
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Fatal("inspection silently repaired or replaced the existing key")
	}
}

func TestUseLeaseKnownHostsScopesAndEnforcesHostVerification(t *testing.T) {
	dirs := isolateTestUserDirs(t)
	prepareLeaseSSHTestStateRoot(t, dirs.StateHome)

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
	if err := verifySSHTransportPathPrivate(filepath.Dir(want), true); err != nil {
		t.Fatalf("lease SSH directory is not private: %v", err)
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
	t.Setenv("XDG_STATE_HOME", "")
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
