package cli

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestSelectedLeaseSSHWindowsClaimFirstNamespace(t *testing.T) {
	dirs := isolateTestUserDirs(t)
	state := filepath.Join(dirs.StateHome, "crabbox")
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("claim fixture namespace already exists: %v", err)
	}
	const securityInfo = windows.OWNER_SECURITY_INFORMATION | windows.DACL_SECURITY_INFORMATION
	before, err := windows.GetNamedSecurityInfo(dirs.StateHome, windows.SE_FILE_OBJECT, securityInfo)
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureCrabboxClaimNamespaceDurable(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{state, filepath.Join(state, "claims")} {
		if err := verifyPrivateWindowsPath(path, true); err != nil {
			t.Fatalf("claim-created namespace is not current-user private: %v", err)
		}
	}
	if _, err := os.Stat(filepath.Join(state, "testboxes")); !os.IsNotExist(err) {
		t.Fatalf("claim creation prepared SSH storage: %v", err)
	}
	const leaseID = "cbx_claim_first_windows"
	key, _, err := ensureTestboxKey(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyPrivateWindowsPath(key, false); err != nil {
		t.Fatal(err)
	}
	if reused, _, err := ensureTestboxKey(leaseID); err != nil || reused != key {
		t.Fatalf("reuse claim-first key: path=%q error=%v", reused, err)
	}
	after, err := windows.GetNamedSecurityInfo(dirs.StateHome, windows.SE_FILE_OBJECT, securityInfo)
	if err != nil || after.String() != before.String() {
		t.Fatalf("selected base security changed: %v", err)
	}
}
