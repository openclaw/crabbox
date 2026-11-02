package utm

import (
	"context"
	"errors"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestExecResolvesUnderSharedFenceWithoutHostLockOrClaimWrites(t *testing.T) {
	b, _ := setup(t)
	repo := core.Repo{Root: t.TempDir()}
	lease, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: repo, Keep: true})
	if err != nil {
		t.Fatal(err)
	}
	original, err := core.ReadLeaseClaim(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	r, err := b.lookup(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	path, _ := recordPath(r.LeaseID)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// A pending release may already hold the host lock while waiting for this
	// shared fence. Resolution must finish without asking for that host lock.
	unlock, err := b.lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err = core.WithLeaseClaimUnchangedShared(ctx, lease.LeaseID, original, func() error {
		resolved, err := b.ResolveExecLeaseUnderClaim(ctx, core.ResolveRequest{ID: lease.LeaseID, Repo: repo, Options: core.LeaseOptions{TargetOS: core.TargetLinux}, Prepare: true}, original)
		if err != nil {
			return err
		}
		if resolved.Server.CloudID != lease.Server.CloudID || resolved.SSH.Port != lease.SSH.Port || !resolved.SSH.NoControlMaster {
			t.Fatal("execution target changed")
		}
		snapshot, exists, set := core.ServerLeaseClaimSnapshot(resolved.Server)
		if !set || !exists || !reflect.DeepEqual(snapshot, original) {
			t.Fatal("exact supplied snapshot not preserved")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	after, err := core.ReadLeaseClaim(lease.LeaseID)
	if err != nil || !reflect.DeepEqual(after, original) {
		t.Fatal("resolver mutated claim")
	}
	journal, err := os.ReadFile(path)
	if err != nil || string(journal) != string(before) {
		t.Fatal("resolver mutated ownership journal")
	}
}
func TestExecRejectsMismatchedClaimsAndCheckpointHoldsBeforeProviderAccess(t *testing.T) {
	for _, scenario := range []string{"provider", "scope", "resource", "repository", "checkpoint", "recovery", "protocol"} {
		t.Run(scenario, func(t *testing.T) {
			b, f := setup(t)
			repo := core.Repo{Root: t.TempDir()}
			lease, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: repo})
			if err != nil {
				t.Fatal(err)
			}
			original, err := core.ReadLeaseClaim(lease.LeaseID)
			if err != nil {
				t.Fatal(err)
			}
			req := core.ResolveRequest{ID: lease.LeaseID, Repo: repo, Prepare: true}
			switch scenario {
			case "provider":
				original.Provider = "other"
			case "scope":
				original.ProviderScope = "other"
			case "resource":
				original.CloudID = sourceUUID
			case "repository":
				req.Repo.Root = t.TempDir()
			case "checkpoint":
				original.CheckpointCapture = &core.CheckpointCaptureBinding{ID: "chk_held"}
			case "recovery":
				r, e := b.lookup(lease.LeaseID)
				if e != nil {
					t.Fatal(e)
				}
				r.State = "recovery"
				if e = saveRecord(r); e != nil {
					t.Fatal(e)
				}
			case "protocol":
				original.TargetOS = core.TargetMacOS
			}
			calls := len(f.calls)
			_, err = b.ResolveExecLeaseUnderClaim(context.Background(), req, original)
			if err == nil {
				t.Fatal("invalid execution authority accepted")
			}
			if scenario != "protocol" && len(f.calls) != calls {
				t.Fatal("rejected claim reached provider access")
			}
		})
	}
}
func TestExecProtocolRequiresMatchingLinuxOrMacOSPrivateSSH(t *testing.T) {
	for _, target := range []string{core.TargetLinux, core.TargetMacOS} {
		t.Run(target, func(t *testing.T) {
			claim := core.LeaseClaim{LeaseID: "cbx_123456789abc", CloudID: otherUUID, Provider: "utm", TargetOS: target, Labels: map[string]string{"target": target}}
			lease := core.LeaseTarget{LeaseID: claim.LeaseID, Server: core.Server{Provider: "utm", CloudID: otherUUID, Labels: claim.Labels}, SSH: core.SSHTarget{TargetOS: target, Host: "127.0.0.1", User: "prepared", Key: "prepared-key", Port: "2222", ReadyCheck: "test guest identity", NoControlMaster: true}}
			if err := validateExecTarget(lease, claim, target); err != nil {
				t.Fatal(err)
			}
			other := core.TargetLinux
			if target == other {
				other = core.TargetMacOS
			}
			lease.SSH.TargetOS = other
			if validateExecTarget(lease, claim, target) == nil {
				t.Fatal("cross-platform SSH protocol accepted")
			}
			lease.SSH.TargetOS = target
			lease.SSH.AuthSecret = true
			if validateExecTarget(lease, claim, target) == nil {
				t.Fatal("secret-as-username transport accepted")
			}
		})
	}
}
func TestExecSharedFenceBlocksExactReleaseUntilTransportCompletes(t *testing.T) {
	b, f := setup(t)
	repo := core.Repo{Root: t.TempDir()}
	lease, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: repo})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := core.ReadLeaseClaim(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	finish := make(chan struct{})
	execDone := make(chan error, 1)
	go func() {
		execDone <- core.WithLeaseClaimUnchangedShared(context.Background(), lease.LeaseID, claim, func() error {
			_, err := b.ResolveExecLeaseUnderClaim(context.Background(), core.ResolveRequest{ID: lease.LeaseID, Repo: repo}, claim)
			close(entered)
			if err != nil {
				return err
			}
			<-finish
			return nil
		})
	}()
	<-entered
	releaseDone := make(chan error, 1)
	go func() { releaseDone <- b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}) }()
	select {
	case err := <-releaseDone:
		t.Fatalf("release crossed execution fence: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	f.mu.Lock()
	_, present := findVM(f.vms, lease.Server.CloudID)
	f.mu.Unlock()
	if !present {
		t.Fatal("VM deleted during execution")
	}
	close(finish)
	if err = <-execDone; err != nil {
		t.Fatal(err)
	}
	if err = <-releaseDone; err != nil {
		t.Fatal(err)
	}
}

type activityClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *activityClock) Now() time.Time   { c.mu.Lock(); defer c.mu.Unlock(); return c.at }
func (c *activityClock) set(at time.Time) { c.mu.Lock(); defer c.mu.Unlock(); c.at = at }
func TestExecActivityRenewsIdleWithoutChangingClaimsOrMaximumTTL(t *testing.T) {
	b, _ := setup(t)
	start := time.Now().UTC()
	clock := &activityClock{at: start}
	b.rt.Clock = clock
	b.cfg.IdleTimeout = time.Minute
	b.cfg.TTL = time.Hour
	repo := core.Repo{Root: t.TempDir()}
	lease, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: repo})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := core.ReadLeaseClaim(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	clock.set(start.Add(2 * time.Minute))
	stop, err := b.BeginSSHRunActivity(context.Background(), lease)
	if err != nil {
		t.Fatal(err)
	}
	clock.set(start.Add(3 * time.Minute))
	stop()
	stop()
	after, err := core.ReadLeaseClaim(lease.LeaseID)
	if err != nil || !reflect.DeepEqual(after, claim) {
		t.Fatal("activity reentered claim publication")
	}
	r, err := b.lookup(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	server := lease.Server
	if eligible, _, err := b.cleanupEligible(r, server); err != nil || eligible {
		t.Fatalf("fresh idle activity expired: eligible=%v err=%v", eligible, err)
	}
	clock.set(start.Add(2 * time.Hour))
	if err = b.stampActivity(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if eligible, _, err := b.cleanupEligible(r, server); err != nil || !eligible {
		t.Fatalf("activity extended maximum lifetime: eligible=%v err=%v", eligible, err)
	}
	if err = b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	path, _ := activityPath(lease.LeaseID)
	if _, err = os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("activity sidecar retained: %v", err)
	}
}
func TestCleanupRechecksActivityAfterClaimFenceWait(t *testing.T) {
	b, f := setup(t)
	start := time.Now().UTC()
	clock := &activityClock{at: start}
	b.rt.Clock = clock
	b.cfg.IdleTimeout = time.Minute
	b.cfg.TTL = time.Hour
	repo := core.Repo{Root: t.TempDir()}
	lease, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: repo})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := core.ReadLeaseClaim(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	r, err := b.lookup(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	clock.set(start.Add(2 * time.Minute))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	held := make(chan struct{})
	resume := make(chan struct{})
	fenceDone := make(chan error, 1)
	go func() {
		fenceDone <- core.WithLeaseClaimUnchangedShared(ctx, lease.LeaseID, claim, func() error { close(held); <-resume; return b.stampActivity(ctx, r) })
	}()
	<-held
	cleanupDone := make(chan error, 1)
	go func() { cleanupDone <- b.Cleanup(ctx, core.CleanupRequest{}) }()
	// Observe cleanup waiting for the held exclusive claim fence, then renew
	// activity while the execution fence is still shared and unchanged.
	select {
	case err := <-cleanupDone:
		t.Fatalf("cleanup did not wait: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(resume)
	if err = <-fenceDone; err != nil {
		t.Fatal(err)
	}
	if err = <-cleanupDone; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	f.mu.Lock()
	_, present := findVM(f.vms, lease.Server.CloudID)
	f.mu.Unlock()
	if !present {
		t.Fatal("cleanup ignored renewed execution activity")
	}
}

func TestExecActivityCompletionAfterReleaseDoesNotRecreateSidecar(t *testing.T) {
	b, _ := setup(t)
	lease, err := b.Acquire(context.Background(), core.AcquireRequest{})
	if err != nil {
		t.Fatal(err)
	}
	stop, err := b.BeginSSHRunActivity(context.Background(), lease)
	if err != nil {
		t.Fatal(err)
	}
	if err = b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	stop()
	path, _ := activityPath(lease.LeaseID)
	if _, err = os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("retired activity sidecar recreated: %v", err)
	}
}
