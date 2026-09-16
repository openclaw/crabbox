package unikraftcloud

import (
	"context"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestNonAdoptableCreateClaimsClearOnlyAfterAbsenceProof(t *testing.T) {
	for _, state := range []string{ukcStateCreatePreflight, ukcStateCreateConflict} {
		for _, operation := range []string{"stop", "cleanup"} {
			t.Run(state+"/"+operation, func(t *testing.T) {
				t.Setenv("XDG_STATE_HOME", t.TempDir())
				api := &fakeUnikraftCloudAPI{baseURL: "https://api.fra.unikraft.cloud"}
				b := testBackend(api, nil, nil)
				leaseID := newLeaseID()
				createReq := createInstanceRequest{
					Name:      core.LeaseProviderName(leaseID, ""),
					Image:     b.cfg.UnikraftCloud.Image,
					MemoryMB:  b.cfg.UnikraftCloud.MemoryMB,
					Autostart: true,
				}
				claim, err := b.createIntentClaim(
					leaseID,
					"non-adoptable",
					testClaimScope(t, api.BaseURL()),
					testUserUUID, core.WarmupRequest{Repo: core.Repo{Root: t.TempDir(), Name: "demo"}, Keep: true}, createReq,
				)
				if err != nil {
					t.Fatalf("create preflight claim: %v", err)
				}
				if state == ukcStateCreateConflict {
					claim, err = transitionUnikraftCloudCreateState(claim, state)
					if err != nil {
						t.Fatalf("transition conflict claim: %v", err)
					}
				}

				switch operation {
				case "stop":
					err = b.Stop(context.Background(), core.StopRequest{ID: claim.LeaseID})
				case "cleanup":
					err = b.Cleanup(context.Background(), core.CleanupRequest{})
				}
				if err != nil {
					t.Fatalf("%s: %v", operation, err)
				}
				if len(api.created) != 0 || len(api.deletedIDs) != 0 {
					t.Fatalf("provider mutations = created %#v deleted %#v, want none", api.created, api.deletedIDs)
				}
				if _, exists, readErr := core.ReadLeaseClaimWithPresence(claim.LeaseID); readErr != nil || exists {
					t.Fatalf("claim exists=%v err=%v, want removed", exists, readErr)
				}
			})
		}
	}
}
