package parallels

import (
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestNewBackendPreservesMacOSGuestUser(t *testing.T) {
	t.Setenv("USER", "host-user")
	for _, guestUser := range []string{"crabbox", "guest-user"} {
		t.Run(guestUser, func(t *testing.T) {
			cfg := core.BaseConfig()
			cfg.TargetOS = core.TargetMacOS
			cfg.Parallels.User = guestUser

			backend := NewBackend(Provider{}.Spec(), cfg, core.Runtime{}).(*leaseBackend)
			if got := backend.Cfg.SSHUser; got != guestUser {
				t.Fatalf("guest SSH user = %q, want %q", got, guestUser)
			}
		})
	}
}
