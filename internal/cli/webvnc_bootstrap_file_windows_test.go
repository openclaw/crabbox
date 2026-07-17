//go:build windows

package cli

import (
	"testing"

	"golang.org/x/sys/windows"
)

func TestWebVNCPortalBootstrapHandoffUsesProtectedWindowsDACL(t *testing.T) {
	handoff, err := startWebVNCPortalBootstrapHandoff(
		"https://broker.example.test/portal/leases/cbx_abcdef123456/vnc/bootstrap",
		"webvnc_view_0123456789abcdef0123456789abcdef",
	)
	if err != nil {
		t.Fatal(err)
	}
	defer handoff.Close()

	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{handoff.dir, handoff.path} {
		if err := validateWebVNCPortalBootstrapWindowsPath(path, user.User.Sid); err != nil {
			t.Fatalf("bootstrap path %q is not private: %v", path, err)
		}
	}
}
