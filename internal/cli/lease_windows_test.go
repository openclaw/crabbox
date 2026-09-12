//go:build windows

package cli

import (
	"crypto/sha256"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestManagedStateTransferWindowsDirectoryAlias(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	state := filepath.Join(source, "state")
	marker := filepath.Join(state, "crabbox", "marker.txt")
	if err := os.MkdirAll(filepath.Dir(marker), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("benign marker\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "source-junction")
	if output, err := exec.Command("cmd.exe", "/c", "mklink", "/J", alias, source).CombinedOutput(); err != nil {
		t.Fatalf("create owned Windows junction: %v\n%s", err, output)
	}
	t.Cleanup(func() {
		if err := os.Remove(alias); err != nil && !os.IsNotExist(err) {
			t.Errorf("remove owned junction: %v", err)
		}
	})
	want, err := NormalizeManagedStateTransferRoot(source)
	if err != nil {
		t.Fatal(err)
	}
	got, err := NormalizeManagedStateTransferRoot(alias)
	if err != nil || got != want {
		t.Fatalf("junction normalized to %q, want %q: %v", got, want, err)
	}
	t.Setenv("XDG_STATE_HOME", state)
	if err := ValidateManagedStateTransferScope("owned junction fixture", alias); err == nil {
		t.Fatal("junction scope admitted overlapping managed namespace")
	}
	unrelated := filepath.Join(root, "unrelated")
	if err := os.Mkdir(unrelated, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := ValidateManagedStateTransferScope("ordinary fixture", unrelated); err != nil {
		t.Fatalf("unrelated scope rejected: %v", err)
	}
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != "benign marker\n" {
		t.Fatal("metadata-only checks changed the marker")
	}
}

func prepareLeaseSSHTestStateRoot(t *testing.T, root string) {
	t.Helper()
	if err := securePrivateWindowsPath(root, true); err != nil {
		t.Fatal(err)
	}
}

func makeLeaseSSHTestConfigReadOnly(t *testing.T, path string) func() {
	t.Helper()
	if err := createPrivateRunOutputDir(path); err != nil {
		t.Fatal(err)
	}
	if err := securePrivateWindowsPath(path, true); err != nil {
		t.Fatal(err)
	}
	user, err := currentWindowsUserSID()
	if err != nil {
		t.Fatal(err)
	}
	// Keep cleanup/security access, but no right to create files or directories.
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;;GRGXWDWOSD;;;" + user.String() + ")")
	if err != nil {
		t.Fatal(err)
	}
	acl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = securePrivateWindowsPath(path, true) })
	probe := filepath.Join(path, "write-control")
	if err := os.WriteFile(probe, []byte("control"), 0o600); !os.IsPermission(err) {
		_ = os.Remove(probe)
		t.Fatalf("default config fixture is not read-only: %v", err)
	}
	if err := os.Mkdir(probe, 0o700); !os.IsPermission(err) {
		_ = os.Remove(probe)
		t.Fatalf("default config fixture permits child directories: %v", err)
	}
	before, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	beforeText := before.String()
	return func() {
		t.Helper()
		after, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
		if err != nil || after.String() != beforeText {
			t.Fatal("default config directory ACL changed")
		}
	}
}

func TestSelectedLeaseSSHWindowsPrivateKeyReuse(t *testing.T) {
	dirs := isolateTestUserDirs(t)
	prepareLeaseSSHTestStateRoot(t, dirs.StateHome)
	const leaseID = "cbx_1516_windows_file"
	key, _, err := ensureTestboxKey(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{key, key + ".pub"} {
		if err := verifyPrivateWindowsPath(path, false); err != nil {
			t.Fatalf("created file is not private: %v", err)
		}
	}
	before, err := os.ReadFile(key)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(before)
	if _, _, err := ensureTestboxKey(leaseID); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(key)
	if err != nil || sha256.Sum256(after) != digest {
		t.Fatal("existing private key changed")
	}
	other, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		t.Fatal(err)
	}
	if err := setWindowsPathPermissive(t, key, false, other); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ensureTestboxKey(leaseID); err == nil {
		t.Fatal("existing key with a non-private ACL was accepted")
	}
	if err := verifyPrivateWindowsPath(key, false); err == nil {
		t.Fatal("reuse silently repaired the existing key ACL")
	}
	after, err = os.ReadFile(key)
	if err != nil || sha256.Sum256(after) != digest {
		t.Fatal("rejected key content changed")
	}
}

func TestEnsureTestboxLeaseDirectoryCreatesPrivateWindowsComponents(t *testing.T) {
	dirs := isolateTestUserDirs(t)
	t.Setenv("XDG_STATE_HOME", "")
	const leaseID = "cbx_abcdef123456"
	leaseDir, err := ensureTestboxLeaseDirectory(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dirs.AppData, "crabbox", "testboxes", leaseID)
	if leaseDir != want {
		t.Fatalf("lease directory=%q want %q", leaseDir, want)
	}
	for _, path := range []string{
		filepath.Join(dirs.AppData, "crabbox"),
		filepath.Join(dirs.AppData, "crabbox", "testboxes"),
		want,
	} {
		if err := verifyPrivateWindowsPath(path, true); err != nil {
			t.Fatalf("verify private lease SSH directory %q: %v", path, err)
		}
	}
}

func TestSelectedLeaseSSHRootWindowsACLContract(t *testing.T) {
	user, err := windows.StringToSid("S-1-5-21-101-202-303-1001")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, grants string
		wantErr      bool
	}{
		{"owner only", "", false},
		{"read and traverse", "(A;;GRGX;;;WD)", false},
		{"system and administrators", "(A;;GA;;;SY)(A;;GA;;;BA)", false},
		{"unrelated write", "(A;;GW;;;WD)", true},
		{"unrelated delete", "(A;;SD;;;WD)", true},
		{"unrelated change ACL", "(A;;WD;;;WD)", true},
		{"unrelated change owner", "(A;;WO;;;WD)", true},
		{"inheritance only", "(A;OIIO;GA;;;WD)", false},
		{"deny is not a grant", "(D;;GW;;;WD)", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			descriptor, err := windows.SecurityDescriptorFromString("O:" + user.String() + "D:P(A;;GA;;;" + user.String() + ")" + tc.grants)
			if err != nil {
				t.Fatal(err)
			}
			err = validateSelectedLeaseSSHRootDescriptor(descriptor, user)
			if (err != nil) != tc.wantErr {
				t.Fatalf("admission=%v want error=%t", err, tc.wantErr)
			}
		})
	}
}

func TestSelectedLeaseSSHRootWindowsPrivateComponents(t *testing.T) {
	dirs := isolateTestUserDirs(t)
	if err := securePrivateWindowsPath(dirs.StateHome, true); err != nil {
		t.Fatal(err)
	}
	const leaseID = "cbx_1516_windows"
	dir, err := ensureTestboxLeaseDirectory(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if dir != filepath.Join(dirs.StateHome, "crabbox", "testboxes", leaseID) {
		t.Fatalf("selected directory=%q", dir)
	}
	for _, path := range []string{dirs.StateHome, filepath.Join(dirs.StateHome, "crabbox"), filepath.Dir(dir), dir} {
		if err := verifyPrivateWindowsPath(path, true); err != nil {
			t.Fatalf("selected private directory %q: %v", path, err)
		}
	}
}

func TestSelectedLeaseSSHWindowsOwnedNamespaceSecuring(t *testing.T) {
	dirs := isolateTestUserDirs(t)
	if err := securePrivateWindowsPath(dirs.StateHome, true); err != nil {
		t.Fatal(err)
	}
	const leaseID = "cbx_1516_windows_securing"
	dir, err := ensureTestboxLeaseDirectory(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	other, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		t.Fatal(err)
	}
	if err := setWindowsPathPermissive(t, dir, true, other); err != nil {
		t.Fatal(err)
	}
	if err := verifySelectedLeaseSSHOwnership(dir, true); err != nil {
		t.Fatalf("fixture lost current-user ownership: %v", err)
	}
	if err := verifySelectedLeaseSSHPath(dir, true); err == nil {
		t.Fatal("nonmutating inspection accepted a non-private generated directory")
	}
	if _, err := ensureTestboxLeaseDirectory(leaseID); err != nil {
		t.Fatalf("preparing an owned generated directory failed: %v", err)
	}
	if err := verifyPrivateWindowsPath(dir, true); err != nil {
		t.Fatalf("generated directory ACL was not secured: %v", err)
	}
}
