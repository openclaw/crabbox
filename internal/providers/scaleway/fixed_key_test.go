package scaleway

import (
	"errors"
	"strings"
	"testing"

	iam "github.com/scaleway/scaleway-sdk-go/api/iam/v1alpha1"
	instance "github.com/scaleway/scaleway-sdk-go/api/instance/v1"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestFixedScalewayNormalizedKeyAcquireReplay(t *testing.T) {
	backend, fake := newTestBackend(t)
	fake.normalizeKey = true
	req := fixedRequest(t)
	for range 2 {
		if _, err := backend.Acquire(t.Context(), req); err != nil {
			t.Fatal(err)
		}
	}
	if fake.createKeyCalls != 1 || fake.createVolumeCalls != 0 || fake.createCalls != 1 {
		t.Fatalf("duplicate allocation: keys=%d volumes=%d servers=%d", fake.createKeyCalls, fake.createVolumeCalls, fake.createCalls)
	}
}

func TestFixedScalewayNormalizedKeyStrandedStop(t *testing.T) {
	for _, stage := range []string{"key", "image"} {
		t.Run(stage, func(t *testing.T) {
			backend, fake := newTestBackend(t)
			fake.normalizeKey = true
			req := fixedRequest(t)
			if stage == "key" {
				fake.createKeyErr = errors.New("lost key response")
			} else {
				fake.getImageErr = errors.New("image lookup failed")
			}
			if _, err := backend.Acquire(t.Context(), req); err == nil {
				t.Fatal("expected interrupted acquisition")
			}
			claim, err := core.ReadLeaseClaim(req.RequestedLeaseID)
			if err != nil || claim.CloudID != "" || claim.FixedCreateIntent.Attempt["server"] != "pending" {
				t.Fatalf("unexpected stranded claim: %v", err)
			}
			// An older binary may already have entered deletion before failing
			// to reconcile the API's comment-free key.
			deleting := core.CloneLeaseClaim(claim)
			deleting.FixedCreateIntent.State = "deleting"
			deleting.FixedCreateIntent.Journal.Phase = "deleting"
			if _, err := core.ReplaceLeaseClaimIfUnchangedDurableReturning(req.RequestedLeaseID, claim, deleting); err != nil {
				t.Fatal(err)
			}
			foreign := &iam.SSHKey{ID: "foreign-key", Name: "unrelated", ProjectID: fake.ProjectID(), PublicKey: fake.keys[0].PublicKey}
			fake.keys = append(fake.keys, foreign)
			foreignVolume := &instance.Volume{ID: "55555555-5555-5555-5555-555555555555", Name: "unrelated"}
			if fake.volumes == nil {
				fake.volumes = make(map[string]*instance.Volume)
			}
			fake.volumes[foreignVolume.ID] = foreignVolume
			for range 2 {
				lease, err := backend.Resolve(t.Context(), core.ResolveRequest{ID: req.RequestedLeaseID, ReleaseOnly: true})
				if err != nil {
					t.Fatal(err)
				}
				if err := backend.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
					t.Fatal(err)
				}
			}
			terminal, err := core.ReadLeaseClaim(req.RequestedLeaseID)
			if err != nil || terminal.FixedCreateIntent.State != "released" || len(fake.volumes) != 1 || fake.volumes[foreignVolume.ID] != foreignVolume || len(fake.keys) != 1 || fake.keys[0] != foreign || fake.createCalls != 0 {
				t.Fatalf("cleanup incomplete: err=%v keys=%d volumes=%d servers=%d", err, len(fake.keys), len(fake.volumes), fake.createCalls)
			}
		})
	}
}

func TestFixedScalewayStrandedStopRefusesForeignKey(t *testing.T) {
	for _, change := range []string{"material", "name", "project"} {
		t.Run(change, func(t *testing.T) {
			backend, fake := newTestBackend(t)
			fake.normalizeKey = true
			fake.createKeyErr = errors.New("lost key response")
			req := fixedRequest(t)
			if _, err := backend.Acquire(t.Context(), req); err == nil {
				t.Fatal("expected interrupted acquisition")
			}
			switch change {
			case "material":
				_, publicKey, err := core.EnsureTestboxKey("cbx_ffffffffffff")
				if err != nil {
					t.Fatal(err)
				}
				fake.keys[0].PublicKey = publicKey
			case "name":
				fake.keys[0].Name = "foreign"
			case "project":
				fake.keys[0].ProjectID = "foreign"
			}
			lease, err := backend.Resolve(t.Context(), core.ResolveRequest{ID: req.RequestedLeaseID, ReleaseOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			err = backend.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease})
			if core.ExitCodeForError(err, 1) != 4 || !strings.Contains(err.Error(), "lease_id_conflict") || fake.deletedKey || fake.createKeyCalls != 1 {
				t.Fatalf("foreign key was not retained: %v", err)
			}
		})
	}
}
