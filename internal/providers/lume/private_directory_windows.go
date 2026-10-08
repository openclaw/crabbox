//go:build windows

package lume

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"

	core "github.com/openclaw/crabbox/internal/cli"
)

// The shared private-path owner secures the directory with its retained-handle and reparse-point checks.
func secureLumePrivateDirectory(path string) error {
	return core.SecurePrivatePath(path, true)
}

func verifyLumePrivateDirectory(path string) error {
	descriptor, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		return fmt.Errorf("inspect private Windows access-control list: %w", err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return fmt.Errorf("read current Windows user: %w", err)
	}
	if user == nil || user.User.Sid == nil {
		return fmt.Errorf("read current Windows user: missing SID")
	}
	owner, _, err := descriptor.Owner()
	if err != nil || owner == nil || !owner.Equals(user.User.Sid) {
		return fmt.Errorf("private Windows directory must be owned by the current user")
	}
	dacl, _, err := descriptor.DACL()
	if err != nil || dacl == nil || dacl.AceCount == 0 {
		return fmt.Errorf("private Windows directory must grant access only to the current user")
	}
	effectiveGrant := false
	for index := uint16(0); index < dacl.AceCount; index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, uint32(index), &ace); err != nil {
			return fmt.Errorf("inspect private Windows access-control entry: %w", err)
		}
		if ace == nil || ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Mask == 0 {
			return fmt.Errorf("private Windows directory has an invalid access-control grant")
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !sid.Equals(user.User.Sid) {
			return fmt.Errorf("private Windows directory grants access to another user")
		}
		if ace.Header.AceFlags&windows.INHERIT_ONLY_ACE == 0 {
			effectiveGrant = true
		}
	}
	if !effectiveGrant {
		return fmt.Errorf("private Windows directory does not grant access to the current user")
	}
	return nil
}
