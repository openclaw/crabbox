//go:build windows

package cli

import (
	"testing"

	"github.com/openclaw/crabbox/internal/privatefile"
	"golang.org/x/sys/windows"
)

func TestPendingFinishRejectsPublicWindowsACLWithoutRepair(t *testing.T) {
	f := newPendingFinishFixture(t)
	path := f.retain()
	user, err := privatefile.WindowsUserSID()
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;;GA;;;" + user.String() + ")(A;;GR;;;WD)")
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
	for range 2 {
		if _, err := readPendingFinishFile(path); err == nil {
			t.Fatal("public file accepted or silently repaired")
		}
	}
}
