package testutil

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

func createIsolatedStateHome(path string) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	descriptor, err := windows.SecurityDescriptorFromString("O:" + user.User.Sid.String() + "D:P(A;OICI;GA;;;" + user.User.Sid.String() + ")")
	if err != nil {
		return err
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	attributes := &windows.SecurityAttributes{SecurityDescriptor: descriptor}
	attributes.Length = uint32(unsafe.Sizeof(*attributes))
	// Elevated TempDir fixtures can inherit Administrators ownership. Set the
	// synthetic state directory's user owner and private DACL at creation.
	return windows.CreateDirectory(name, attributes)
}
