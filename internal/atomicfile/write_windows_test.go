//go:build windows

package atomicfile

import (
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestWritePrivateWindowsProtectedDACL(t *testing.T) {
	for _, existing := range []bool{false, true} {
		name := "create"
		if existing {
			name = "replace"
		}
		t.Run(name, func(t *testing.T) {
			dir := permissiveWindowsParent(t)
			before := windowsDescriptor(t, dir).String()
			path := filepath.Join(dir, "config.yaml")
			if existing {
				if err := os.WriteFile(path, []byte("previous fixture"), 0o600); err != nil {
					t.Fatal(err)
				}
				acl, _, err := windowsDescriptor(t, path).DACL()
				if err != nil || acl == nil || acl.AceCount < 2 {
					t.Fatalf("fixture did not inherit permissive parent: acl=%v err=%v", acl, err)
				}
			}
			var staged string
			err := WritePrivate(path, ".config-*.tmp", []byte("harmless fixture"), func(from, to string) error {
				staged = from
				assertWindowsOwnerOnly(t, from)
				return os.Rename(from, to)
			})
			if err != nil {
				t.Fatal(err)
			}
			assertWindowsOwnerOnly(t, path)
			if got := windowsDescriptor(t, dir).String(); got != before {
				t.Fatalf("parent descriptor changed: before=%s after=%s", before, got)
			}
			if _, err := os.Stat(staged); !os.IsNotExist(err) {
				t.Fatalf("staged file remains: %v", err)
			}
			data, err := os.ReadFile(path)
			if err != nil || string(data) != "harmless fixture" {
				t.Fatalf("published contents=%q error=%v", data, err)
			}
		})
	}
}

func permissiveWindowsParent(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	// An unrelated synthetic SID avoids granting fixture access to a real account.
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;OICI;GA;;;" + user.User.Sid.String() + ")(A;OICI;GR;;;S-1-5-21-111111111-222222222-333333333-1001)")
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
	return dir
}

func windowsDescriptor(t *testing.T, path string) *windows.SECURITY_DESCRIPTOR {
	t.Helper()
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	return descriptor
}

func assertWindowsOwnerOnly(t *testing.T, path string) {
	t.Helper()
	descriptor := windowsDescriptor(t, path)
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	owner, _, err := descriptor.Owner()
	if err != nil || owner == nil || !owner.Equals(user.User.Sid) {
		t.Fatalf("file is not owned by current user: %v", err)
	}
	control, _, err := descriptor.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatalf("file DACL is not protected: control=%#x err=%v", control, err)
	}
	acl, _, err := descriptor.DACL()
	if err != nil || acl == nil || acl.AceCount != 1 {
		t.Fatalf("expected exactly one current-user grant: acl=%v err=%v", acl, err)
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(acl, 0, &ace); err != nil {
		t.Fatal(err)
	}
	if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Header.AceFlags != 0 || ace.Mask == 0 {
		t.Fatalf("unexpected file access-control entry: %+v", ace)
	}
	sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
	if !sid.Equals(user.User.Sid) {
		t.Fatal("file grants access to an unrelated principal")
	}
}
