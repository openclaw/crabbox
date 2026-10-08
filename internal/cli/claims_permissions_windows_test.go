//go:build windows

package cli

import (
	"errors"
	"os"
	"runtime"
	"testing"

	"golang.org/x/sys/windows"
)

func makeTestDirectoryUnreadable(t *testing.T, path string) {
	t.Helper()
	user, err := currentWindowsUserSID()
	if err != nil {
		t.Fatal(err)
	}
	var pinner runtime.Pinner
	pinner.Pin(user)
	defer pinner.Unpin()
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.FILE_READ_ATTRIBUTES | windows.FILE_TRAVERSE |
			windows.READ_CONTROL | windows.WRITE_DAC | windows.WRITE_OWNER | windows.SYNCHRONIZE,
		AccessMode: windows.SET_ACCESS,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(user),
		},
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		acl,
		nil,
	); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := secureSSHTransportPath(path, true); err != nil {
			t.Errorf("restore directory access: %v", err)
		}
	})
	if _, err := os.ReadDir(path); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("restrictive directory DACL did not deny reads: %v", err)
	}
}
