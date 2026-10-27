package hetzner

import (
	"context"
	"io"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestHetznerReleaseFencesAcquiredClaimGeneration(t *testing.T) {
	for _, phase := range []string{"before release", "before delete"} {
		for _, missing := range []bool{false, true} {
			name := phase + "/replaced"
			if missing {
				name = phase + "/removed"
			}
			t.Run(name, func(t *testing.T) {
				installHetznerClaimState(t)
				const leaseID = "cbx_abcdef123456"
				server := acquiredHetznerTestServer(42, leaseID)
				client := &fakeHetznerClient{servers: map[int64]core.Server{42: server}, createServer: server}
				installHetznerTestHooks(t, client)
				backend := NewHetznerLeaseBackend(core.ProviderSpec{}, core.Config{}, core.Runtime{Stderr: io.Discard}).(*hetznerLeaseBackend)
				lease, err := backend.Acquire(t.Context(), core.AcquireRequest{RequestedSlug: "test"})
				if err != nil {
					t.Fatal(err)
				}
				seedHetznerClaim(t, lease.Server)
				authorized, err := core.ReadLeaseClaim(leaseID)
				if err != nil {
					t.Fatal(err)
				}
				core.SetServerLeaseClaimSnapshot(&lease.Server, authorized, true)
				mutate := func() {
					if missing {
						core.RemoveLeaseClaim(leaseID)
						return
					}
					replacement := authorized
					replacement.RepoRoot = "/replacement-owner"
					if _, err := core.ReplaceLeaseClaimIfUnchangedDurableReturning(leaseID, authorized, replacement); err != nil {
						t.Fatal(err)
					}
				}
				if phase == "before release" {
					mutate()
				} else {
					newHetznerClient = func() (hetznerClient, error) { mutate(); return client, nil }
				}
				cleaned := false
				err = backend.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease, GuardedRemoteCleanup: func(context.Context, core.LeaseTarget) { cleaned = true }})
				if err == nil || len(client.deletedServers) != 0 || len(client.deletedKeys) != 0 || cleaned {
					t.Fatalf("err=%v servers=%v keys=%v remoteCleanup=%v; stale release must have no effects", err, client.deletedServers, client.deletedKeys, cleaned)
				}
			})
		}
	}
}
