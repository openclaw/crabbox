//go:build windows

package cli

import (
	"testing"

	"golang.org/x/sys/windows"
)

func prepareConfigPrivateParent(t *testing.T, path string) {
	t.Helper()
	sid, err := windows.StringToSid("S-1-5-21-111111111-222222222-333333333-1001")
	if err != nil {
		t.Fatal(err)
	}
	if err := setWindowsPathPermissive(t, path, true, sid); err != nil {
		t.Fatal(err)
	}
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	before := descriptor.String()
	t.Cleanup(func() {
		after, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal(err)
		}
		if after.String() != before {
			t.Fatal("config writer changed parent DACL")
		}
	})
}

func assertConfigPrivate(t *testing.T, path string) {
	t.Helper()
	if err := verifyPrivateWindowsPath(path, false); err != nil {
		t.Fatalf("config is not current-user private: %v", err)
	}
}
