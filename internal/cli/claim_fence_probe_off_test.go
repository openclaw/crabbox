//go:build !crabbox_claim_fence_probe

package cli

import (
	"reflect"
	"testing"
	"time"
)

// Release builds must not let the probe environment steer the claim fence.
func TestClaimFenceProbeEnvironmentIsInertInDefaultBuild(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_fenceprobeinert1"
	if err := ClaimLeaseTargetForRepoConfig(leaseID, "probe-inert", Config{Provider: "aws"}, Server{Provider: "aws", CloudID: "i-123"}, SSHTarget{}, "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	expected, err := ReadLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"revoke", "reassign"} {
		t.Setenv("CRABBOX_CLAIM_FENCE_PROBE", "TestClaimFenceProbeEnvironmentIsInertInDefaultBuild:"+mode)
		called := false
		if err := WithLeaseClaimUnchanged(leaseID, expected, func() error {
			called = true
			return nil
		}); err != nil || !called {
			t.Fatalf("%s: default build fence err=%v called=%v", mode, err, called)
		}
		current, err := ReadLeaseClaim(leaseID)
		if err != nil || !reflect.DeepEqual(current, expected) {
			t.Fatalf("%s: default build changed the claim: err=%v current=%#v", mode, err, current)
		}
	}
}
