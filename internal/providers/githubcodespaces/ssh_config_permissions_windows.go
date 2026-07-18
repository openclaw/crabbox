//go:build windows

package githubcodespaces

import (
	"os"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var moveGitHubCodespacesSSHConfigFileExW = syscall.NewLazyDLL("kernel32.dll").NewProc("MoveFileExW")

func securePrivateSSHConfigFile(path string) error {
	user, system, err := githubCodespacesPrivateFileSIDs()
	if err != nil {
		return err
	}
	access := []windows.EXPLICIT_ACCESS{
		githubCodespacesPrivateFileAccess(user, windows.TRUSTEE_IS_USER),
		githubCodespacesPrivateFileAccess(system, windows.TRUSTEE_IS_USER),
	}
	acl, err := windows.ACLFromEntries(access, nil)
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		user,
		nil,
		acl,
		nil,
	)
}

func githubCodespacesPrivateFileAccess(sid *windows.SID, trusteeType windows.TRUSTEE_TYPE) windows.EXPLICIT_ACCESS {
	return windows.EXPLICIT_ACCESS{
		AccessPermissions: windows.ACCESS_MASK(windows.GENERIC_ALL),
		AccessMode:        windows.GRANT_ACCESS,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  trusteeType,
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}
}

func githubCodespacesPrivateFileSIDs() (*windows.SID, *windows.SID, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, nil, err
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return nil, nil, err
	}
	return user.User.Sid, system, nil
}

func validatePrivateSSHConfigPermissions(path string, _ os.FileInfo) error {
	user, system, err := githubCodespacesPrivateFileSIDs()
	if err != nil {
		return err
	}
	descriptor, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		return err
	}
	owner, _, err := descriptor.Owner()
	if err != nil || owner == nil || !owner.Equals(user) {
		return exit(2, "github-codespaces SSH config path %q is not owned by the current user", path)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil || dacl == nil || dacl.AceCount != 2 {
		return exit(2, "github-codespaces SSH config path %q does not have a private DACL", path)
	}
	seenUser, seenSystem := false, false
	for i := uint16(0); i < dacl.AceCount; i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, uint32(i), &ace); err != nil {
			return err
		}
		if ace == nil || ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Mask&windows.ACCESS_MASK(windows.GENERIC_ALL) == 0 {
			return exit(2, "github-codespaces SSH config path %q has a non-private DACL entry", path)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		switch {
		case sid.Equals(user):
			seenUser = true
		case sid.Equals(system):
			seenSystem = true
		default:
			return exit(2, "github-codespaces SSH config path %q grants access to another principal", path)
		}
	}
	if !seenUser || !seenSystem {
		return exit(2, "github-codespaces SSH config path %q does not have the required private DACL", path)
	}
	return nil
}

func replaceSSHConfigFile(tmpPath, path string) error {
	oldPath, err := syscall.UTF16PtrFromString(tmpPath)
	if err != nil {
		return err
	}
	newPath, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	const (
		movefileReplaceExisting = 0x1
		movefileWriteThrough    = 0x8
	)
	r1, _, callErr := moveGitHubCodespacesSSHConfigFileExW.Call(
		uintptr(unsafe.Pointer(oldPath)),
		uintptr(unsafe.Pointer(newPath)),
		uintptr(movefileReplaceExisting|movefileWriteThrough),
	)
	if r1 != 0 {
		return nil
	}
	if callErr != syscall.Errno(0) {
		return callErr
	}
	return syscall.EINVAL
}

func syncSSHConfigDirectory(string) error { return nil }

func quoteSSHProxyExecutable(path string) string {
	return `"` + strings.ReplaceAll(path, `"`, `""`) + `"`
}
