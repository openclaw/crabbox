package utm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestExplicitReleaseRejectsReclaimedRepositoryAuthority(t *testing.T) {
	b, f := setup(t)
	ctx := context.Background()
	oldRepo := core.Repo{Root: t.TempDir()}
	lease, err := b.Acquire(ctx, core.AcquireRequest{Repo: oldRepo, Keep: true})
	if err != nil {
		t.Fatal(err)
	}
	current, err := b.Resolve(ctx, core.ResolveRequest{ID: lease.LeaseID, Repo: core.Repo{Root: t.TempDir()}, Reclaim: true})
	if err != nil {
		t.Fatal(err)
	}
	expected, err := core.ReadLeaseClaim(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	teardowns := 0
	err = b.ReleaseLease(ctx, core.ReleaseLeaseRequest{Lease: lease, GuardedRemoteCleanup: func(context.Context, core.LeaseTarget) { teardowns++ }})
	if _, present := findVM(f.vms, lease.Server.CloudID); !present {
		t.Fatalf("stale release destroyed reclaimed VM; error=%v", err)
	}
	if err == nil || teardowns != 0 {
		t.Fatalf("stale release authority accepted: err=%v teardowns=%d", err, teardowns)
	}
	after, err := core.ReadLeaseClaim(lease.LeaseID)
	if err != nil || !reflect.DeepEqual(after, expected) {
		t.Fatal("stale release changed current claim")
	}
	if err = b.ReleaseLease(ctx, core.ReleaseLeaseRequest{Lease: current}); err != nil {
		t.Fatal(err)
	}
}

type releaseMutationRunner struct {
	next   core.CommandRunner
	mutate func() error
	done   bool
}

func (r *releaseMutationRunner) Run(ctx context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	if !r.done && req.Name == "/usr/bin/osascript" && len(req.Args) > 0 && req.Args[0] == "-l" {
		r.done = true
		if err := r.mutate(); err != nil {
			return core.LocalCommandResult{}, err
		}
	}
	return r.next.Run(ctx, req)
}

func TestExplicitReleasePreservesPresentAbsentAndGenerationAuthority(t *testing.T) {
	for _, scenario := range []string{"current", "missing-snapshot", "absent-snapshot", "missing-current-claim", "same-owner-new-generation", "change-before-fence"} {
		t.Run(scenario, func(t *testing.T) {
			b, f := setup(t)
			ctx := context.Background()
			lease, err := b.Acquire(ctx, core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, Keep: true})
			if err != nil {
				t.Fatal(err)
			}
			original, err := core.ReadLeaseClaim(lease.LeaseID)
			if err != nil {
				t.Fatal(err)
			}
			expected := original
			expectedExists := true
			r, err := b.lookup(lease.LeaseID)
			if err != nil {
				t.Fatal(err)
			}
			journalPath, _ := recordPath(r.LeaseID)
			before, err := os.ReadFile(journalPath)
			if err != nil {
				t.Fatal(err)
			}
			advance := func() error {
				next, err := core.ReplaceLeaseClaimIfUnchangedDurableReturning(lease.LeaseID, original, original)
				if err != nil {
					return err
				}
				if next.Revision == original.Revision {
					return fmt.Errorf("claim mutation did not advance generation")
				}
				expected = next
				return nil
			}
			switch scenario {
			case "missing-snapshot":
				lease.Server = core.Server{Provider: lease.Server.Provider, CloudID: lease.Server.CloudID, Name: lease.Server.Name, Labels: lease.Server.Labels}
			case "absent-snapshot":
				core.SetServerLeaseClaimSnapshot(&lease.Server, core.LeaseClaim{}, false)
			case "missing-current-claim":
				if err = core.RemoveLeaseClaimIfUnchanged(lease.LeaseID, original); err != nil {
					t.Fatal(err)
				}
				expected = core.LeaseClaim{}
				expectedExists = false
			case "same-owner-new-generation":
				if err = advance(); err != nil {
					t.Fatal(err)
				}
			case "change-before-fence":
				b.rt.Exec = &releaseMutationRunner{next: f, mutate: advance}
			}
			teardowns := 0
			actions := len(f.actions)
			err = b.ReleaseLease(ctx, core.ReleaseLeaseRequest{Lease: lease, GuardedRemoteCleanup: func(context.Context, core.LeaseTarget) { teardowns++ }})
			if scenario == "current" {
				if err != nil {
					t.Fatal(err)
				}
				if _, present := findVM(f.vms, r.VMID); present {
					t.Fatal("current release did not remove VM")
				}
				return
			}
			if err == nil || teardowns != 0 {
				t.Fatalf("invalid carried authority reached teardown: err=%v teardown=%d", err, teardowns)
			}
			for _, action := range f.actions[actions:] {
				if action == "stop" || action == "delete" {
					t.Fatalf("invalid carried authority reached %s", action)
				}
			}
			if _, present := findVM(f.vms, r.VMID); !present {
				t.Fatal("invalid carried authority deleted VM")
			}
			after, exists, err := core.ReadLeaseClaimWithPresence(r.LeaseID)
			if err != nil || exists != expectedExists || !reflect.DeepEqual(after, expected) {
				t.Fatal("invalid release changed current claim")
			}
			journal, err := os.ReadFile(journalPath)
			if err != nil || string(journal) != string(before) {
				t.Fatal("invalid release changed ownership journal")
			}
			// A fresh release resolution grants only the current present/absent state.
			fresh, err := b.Resolve(ctx, core.ResolveRequest{ID: lease.LeaseID, ReleaseOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			if err = b.ReleaseLease(ctx, core.ReleaseLeaseRequest{Lease: fresh}); err != nil {
				t.Fatalf("fresh resolution did not restore release: %v", err)
			}
		})
	}
}

func TestIncompleteReleaseWithoutSnapshotRequiresClaimAbsence(t *testing.T) {
	for _, published := range []bool{false, true} {
		t.Run(fmt.Sprint(published), func(t *testing.T) {
			b, f := setup(t)
			ctx := context.Background()
			waitForSSHReady = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error {
				return errors.New("fixture SSH not ready")
			}
			if _, err := b.Acquire(ctx, core.AcquireRequest{}); err == nil {
				t.Fatal("fixture acquisition should be incomplete")
			}
			records, err := b.records()
			if err != nil || len(records) != 1 {
				t.Fatal("missing incomplete ownership journal")
			}
			r := records[0]
			lease := b.target(r, f.vms)
			if _, _, set := core.ServerLeaseClaimSnapshot(lease.Server); set {
				t.Fatal("partial acquisition unexpectedly has carried claim")
			}
			var expected core.LeaseClaim
			if published {
				expected, err = b.claimTarget(lease, r.Slug, t.TempDir(), true, core.LeaseClaim{}, false)
				if err != nil {
					t.Fatal(err)
				}
			}
			actions := len(f.actions)
			err = b.ReleaseLease(ctx, core.ReleaseLeaseRequest{Lease: lease})
			if !published {
				if err != nil {
					t.Fatal(err)
				}
				if _, present := findVM(f.vms, r.VMID); present {
					t.Fatal("unclaimed incomplete clone not released")
				}
				return
			}
			if err == nil {
				t.Fatal("snapshot-free incomplete release adopted a later claim")
			}
			for _, action := range f.actions[actions:] {
				if action == "stop" || action == "delete" {
					t.Fatal("later claim reached destructive effects")
				}
			}
			after, err := core.ReadLeaseClaim(r.LeaseID)
			if err != nil || !reflect.DeepEqual(after, expected) {
				t.Fatal("later published claim modified")
			}
			// Automatic cleanup has its own freshly acquired authority, independently
			// of this stale explicit target, and can dispose the incomplete allocation.
			if err = b.Cleanup(ctx, core.CleanupRequest{}); err != nil {
				t.Fatal(err)
			}
			if _, present := findVM(f.vms, r.VMID); present {
				t.Fatal("fresh cleanup did not dispose incomplete clone")
			}
		})
	}
}
