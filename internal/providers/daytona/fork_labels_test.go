package daytona

import (
	"fmt"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestDaytonaForkOmitsUnverifiedClass(t *testing.T) {
	for _, fixed := range []bool{false, true} {
		for _, custom := range []bool{false, true} {
			t.Run(fmt.Sprintf("fixed=%t/custom=%t", fixed, custom), func(t *testing.T) {
				f, b, req := newFixedDaytonaFixture(t)
				if !fixed {
					req.RequestedLeaseID = ""
				}
				if custom {
					f.classSnapshot.SetCpu(3)
					f.classSnapshot.SetMem(6)
					// A configured class does not constrain a captured snapshot.
					core.MarkClassExplicit(&b.cfg)
				}
				req.RequestedCheckpointID = "chk_0123456789abcdef"
				req.CheckpointSource = &core.NativeCheckpointForkRecord{
					Kind: core.CheckpointKindDaytona, Direct: true, ImageID: f.classSnapshot.GetId(), Name: f.classSnapshot.GetName(),
					Metadata: map[string]string{"api_url": b.cfg.Daytona.APIURL, "organization": "org-test", "checkpoint": req.RequestedCheckpointID,
						"snapshot_id": f.classSnapshot.GetId(), "source": "original-source", "work_root": b.cfg.Daytona.WorkRoot, "user": "daytona", "target": "us"},
				}
				if err := (Provider{}).ApplyNativeCheckpointForkConfig(core.NativeCheckpointForkRequest{Config: &b.cfg, Record: *req.CheckpointSource}); err != nil {
					t.Fatal(err)
				}
				lease, err := b.Acquire(t.Context(), req)
				if err != nil {
					t.Fatal(err)
				}
				claim, exists, err := core.ReadLeaseClaimWithPresence(lease.LeaseID)
				if err != nil || !exists {
					t.Fatalf("missing claim: %v", err)
				}
				for source, labels := range map[string]map[string]string{"create": f.create.GetLabels(), "sandbox": f.sandbox.GetLabels(), "claim": claim.Labels} {
					if value, exists := labels["class"]; exists {
						t.Errorf("%s class=%q misrepresents snapshot CPU=%g memory=%g GiB", source, value, f.sandbox.GetCpu(), f.sandbox.GetMemory())
					}
				}
				t.Logf("snapshot CPU=%g memory=%g GiB; create/sandbox/claim class=%q", f.sandbox.GetCpu(), f.sandbox.GetMemory(), claim.Labels["class"])
				if fixed {
					fingerprint, nonce := claim.FixedCreateIntent.Fingerprint, claim.FixedCreateIntent.Attempt["nonce"]
					replayed, err := b.Acquire(t.Context(), req)
					if err != nil || replayed.Server.CloudID != lease.Server.CloudID || f.sandboxCreates != 1 {
						t.Fatalf("fork replay changed allocation: %v", err)
					}
					after, _, err := core.ReadLeaseClaimWithPresence(lease.LeaseID)
					if err != nil || after.FixedCreateIntent.Fingerprint != fingerprint || after.FixedCreateIntent.Attempt["nonce"] != nonce {
						t.Fatalf("fork replay changed ownership: %v", err)
					}
				}
			})
		}
	}
}

func TestDaytonaClassSelectionKeepsVerifiedLabel(t *testing.T) {
	for _, fixed := range []bool{false, true} {
		t.Run(fmt.Sprintf("fixed=%t", fixed), func(t *testing.T) {
			f, b, req := newFixedDaytonaFixture(t)
			if !fixed {
				req.RequestedLeaseID = ""
			}
			b.cfg.Class, b.cfg.Daytona.Snapshot = "small", ""
			core.MarkClassExplicit(&b.cfg)
			f.classSnapshot.SetName("daytona-small")
			lease, err := b.Acquire(t.Context(), req)
			if err != nil {
				t.Fatal(err)
			}
			claim, exists, err := core.ReadLeaseClaimWithPresence(lease.LeaseID)
			if err != nil || !exists || claim.Labels["class"] != "small" || f.create.GetLabels()["class"] != "small" || f.sandbox.GetLabels()["class"] != "small" {
				t.Fatalf("verified class label lost: labels=%v err=%v", claim.Labels, err)
			}
		})
	}
}
