//go:build windows

package privatefile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestCreateTempWindowsPrivateBeforeWrite(t *testing.T) {
	for _, longPath := range []bool{false, true} {
		name := "short path"
		if longPath {
			name = "long path"
		}
		t.Run(name, func(t *testing.T) {
			testCreateTempWindowsPrivateBeforeWrite(t, longPath)
		})
	}
}

func testCreateTempWindowsPrivateBeforeWrite(t *testing.T, longPath bool) {
	t.Helper()
	dir := t.TempDir()
	user, err := WindowsUserSID()
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;OICI;GA;;;" + user.String() + ")(A;OICI;GR;;;S-1-5-21-111111111-222222222-333333333-1001)")
	if err != nil {
		t.Fatal(err)
	}
	acl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
	if longPath {
		for len(dir) < 300 {
			dir = filepath.Join(dir, strings.Repeat("nested", 8))
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	file, err := CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(file.Name())
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() != 0 {
		t.Fatalf("new file is not empty: info=%v error=%v", info, err)
	}
	if err := VerifyWindowsHandle(windows.Handle(file.Fd()), false, user); err != nil {
		t.Fatalf("empty staged file is not private: %v", err)
	}
}

func TestValidateWindowsDescriptorRejectsNonPrivate(t *testing.T) {
	user, err := WindowsUserSID()
	if err != nil {
		t.Fatal(err)
	}
	for name, sddl := range map[string]string{
		"inherited":       "O:" + user.String() + "D:(A;;GA;;;" + user.String() + ")",
		"unrelated grant": "O:" + user.String() + "D:P(A;;GA;;;" + user.String() + ")(A;;GR;;;WD)",
		"wrong owner":     "O:S-1-5-21-111111111-222222222-333333333-1001D:P(A;;GA;;;" + user.String() + ")",
		"empty ACL":       "O:" + user.String() + "D:P",
	} {
		t.Run(name, func(t *testing.T) {
			descriptor, err := windows.SecurityDescriptorFromString(sddl)
			if err != nil {
				t.Fatal(err)
			}
			if err := ValidateWindowsDescriptor(descriptor, user); err == nil {
				t.Fatal("accepted non-private security descriptor")
			}
		})
	}
}
