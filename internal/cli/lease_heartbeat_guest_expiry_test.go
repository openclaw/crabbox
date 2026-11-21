package cli

import (
	"context"
	"strings"
	"testing"
)

func TestHeartbeatTouchRefreshesFileBackedGuestExpiry(t *testing.T) {
	old := refreshGuestLeaseExpirySSH
	defer func() { refreshGuestLeaseExpirySSH = old }()
	calls := 0
	refreshGuestLeaseExpirySSH = func(_ context.Context, target SSHTarget, _, content string) error {
		calls++
		if target.Host != "fixture" || !strings.Contains(content, "expires_at=1800000000\n") {
			t.Fatalf("target=%v content=%q", target.Host, content)
		}
		return nil
	}
	inner := guestExpiryTouchBackend{map[string]string{"crabbox": "true", "state": "running", "keep": "true", "expires_at": "1800000000"}}
	wrapped := guestExpiryHeartbeatBackend{LeaseTouchBackend: inner, cfg: Config{Provider: "hetzner", TargetOS: targetLinux}}
	req := TouchRequest{Lease: LeaseTarget{SSH: SSHTarget{Host: "fixture"}}}
	if _, err := wrapped.Touch(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("heartbeat touch refreshed guest expiry %d times, want 1", calls)
	}
}
