//go:build windows

package cli

import "testing"

func TestUserHomeDirectoryPrefersWindowsUserProfileOverMsysHome(t *testing.T) {
	t.Setenv("USERPROFILE", `C:\Users\alice`)
	t.Setenv("HOME", `C:\msys64\home\alice`)
	if got := userHomeDirectory(); got != `C:\Users\alice` {
		t.Fatalf("userHomeDirectory()=%q, want the Windows profile directory", got)
	}
}
