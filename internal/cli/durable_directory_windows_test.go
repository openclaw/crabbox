package cli

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestSelectedLeaseSSHWindowsClaimFirstNamespace(t *testing.T) {
	for _, producer := range []string{"durable-namespace", "operation-lock", "durable-lock", "lock-only", "transaction", "durable-transaction"} {
		t.Run(producer, func(t *testing.T) { testWindowsClaimFirstNamespace(t, producer) })
	}
}

func testWindowsClaimFirstNamespace(t *testing.T, producer string) {
	t.Helper()
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
	const leaseID = "cbx_claim_first_windows"
	var createErr error
	switch producer {
	case "durable-namespace":
		createErr = EnsureCrabboxClaimNamespaceDurable()
	case "operation-lock":
		createErr = withLeaseIDOperationLock(leaseID, func() error { return nil })
	case "durable-lock":
		createErr = WithDurableLeaseClaimLock(leaseID, func(*leaseClaim, bool, func() error) error { return nil })
	case "lock-only":
		_, createErr = leaseClaimLockPath(filepath.Join(state, "claims", leaseID+".json"))
	default:
		policy := claimDirectoryCreate
		if producer == "durable-transaction" {
			policy = claimDirectoryDurableNamespace
		}
		_, createErr = transactLeaseClaim(leaseID, leaseClaimTransaction{
			directory: policy,
			mutate: func(claim *leaseClaim) error {
				claim.LeaseID = leaseID
				return nil
			},
		})
	}
	if createErr != nil {
		t.Fatal(createErr)
	}
	paths := []string{state}
	if producer != "lock-only" {
		paths = append(paths, filepath.Join(state, "claims"))
	}
	if producer != "durable-namespace" {
		paths = append(paths, filepath.Join(state, "claim-locks"))
	}
	for _, path := range paths {
		if err := verifyPrivateWindowsPath(path, true); err != nil {
			t.Fatalf("claim-created namespace is not current-user private: %v", err)
		}
	}
	if producer == "lock-only" {
		if _, err := os.Stat(filepath.Join(state, "claims")); !os.IsNotExist(err) {
			t.Fatalf("lock-only producer created claims: %v", err)
		}
	}
	if _, err := os.Stat(filepath.Join(state, "testboxes")); !os.IsNotExist(err) {
		t.Fatalf("claim creation prepared SSH storage: %v", err)
	}
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
