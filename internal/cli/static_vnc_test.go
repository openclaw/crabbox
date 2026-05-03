package cli

import (
	"context"
	"strings"
	"testing"
)

func TestStaticMacOSManagedVNCLoginScript(t *testing.T) {
	got := staticMacOSManagedVNCLoginScript("cbx user", "/Users/admin/crabbox root")
	for _, want := range []string{
		"user='cbx user'",
		"root='/Users/admin/crabbox root'",
		"sysadminctl -addUser",
		"dscl . -passwd",
		"kickstart -configure -access -on -privs -all -users \"$user\"",
		"DidSeeAccessibility",
		"PASSWORD_FILE=%s",
		"PASSWORD=%s",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("script missing %q:\n%s", want, got)
		}
	}
}

func TestEnsureStaticManagedVNCLoginRejectsWindows(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = staticProvider
	cfg.Static.ManagedLogin = true
	target := SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeNormal}
	_, err := ensureStaticManagedVNCLogin(context.Background(), cfg, target)
	if err == nil || !strings.Contains(err.Error(), "requires SSH/WinRM") {
		t.Fatalf("err=%v", err)
	}
}
