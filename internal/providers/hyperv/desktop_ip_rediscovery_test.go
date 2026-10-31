package hyperv

import (
	"context"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestSetUpWindowsDesktopRediscoversGuestIPAfterBootstrap(t *testing.T) {
	bootstrapped := false
	runner := &recordingRunner{
		respond: func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
			args := strings.Join(req.Args, "\n")
			if strings.Contains(args, "AutoAdminLogon") {
				bootstrapped = true
			}
			if strings.Contains(args, "Get-VMNetworkAdapter") && strings.Contains(args, "IPAddresses") {
				if bootstrapped {
					return core.LocalCommandResult{Stdout: `["10.0.0.9"]`}, nil, true
				}
				return core.LocalCommandResult{Stdout: `["10.0.0.5"]`}, nil, true
			}
			return core.LocalCommandResult{}, nil, false
		},
	}
	b := testBackend(runner)
	b.guestRetryBackoff = 0
	b.waitWindowsDesktopSession = func(context.Context, string, string, time.Duration) error { return nil }

	ip, err := b.setUpWindowsDesktop(context.Background(), b.configForRun(), "crabbox-desktop-1234")
	if err != nil {
		t.Fatalf("setUpWindowsDesktop: %v", err)
	}
	if !bootstrapped {
		t.Fatal("desktop bootstrap script never ran")
	}
	if ip != "10.0.0.9" {
		t.Fatalf("ip=%q want the address discovered after the bootstrap reboot", ip)
	}
}
