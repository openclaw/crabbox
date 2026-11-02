package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

type watchAuthorityBackend struct {
	SSHLeaseBackend
	released int
	final    LeaseTarget
}

func (b *watchAuthorityBackend) Acquire(ctx context.Context, req AcquireRequest) (LeaseTarget, error) {
	lease, err := b.SSHLeaseBackend.Acquire(ctx, req)
	lease.Server.CloudID = "fixture-resource"
	lease.Server.ImmutableID = "fixture-immutable-resource"
	return lease, err
}

func (*watchAuthorityBackend) ReleaseLeaseConnectionCleanupSafe() bool { return false }

func (b *watchAuthorityBackend) ReleaseLease(ctx context.Context, req ReleaseLeaseRequest) error {
	claim, exists, set := ServerLeaseClaimSnapshot(req.Lease.Server)
	if !set || !exists {
		return fmt.Errorf("fixture release missing authority")
	}
	return CleanupLeaseClaimIfUnchangedAfterContext(ctx, req.Lease.LeaseID, claim, true, func() error {
		b.released++
		b.final = req.Lease
		return nil
	})
}

func watchOwnedMutation(ctx context.Context, leaseID string) (LeaseTarget, error) {
	claim, err := ReadLeaseClaim(leaseID)
	if err != nil {
		return LeaseTarget{}, err
	}
	labels := cloneStringMap(claim.Labels)
	labels["state"] = "ready"
	updated, err := UpdateLeaseClaimTouchIfUnchanged(ctx, leaseID, claim, labels, time.Now().UTC(), nil)
	if err != nil {
		return LeaseTarget{}, err
	}
	lease := LeaseTarget{LeaseID: leaseID, Server: Server{Provider: claim.Provider, CloudID: claim.CloudID, ImmutableID: claim.CloudImmutableID, Labels: labels}, SSH: SSHTarget{Host: "updated.example.test"}}
	SetServerLeaseClaimSnapshot(&lease.Server, updated, true)
	return lease, nil
}

func TestWatchCleanupAfterOwnedClaimUpdate(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CRABBOX_COORDINATOR", "")
	root := newWatchGitRepo(t)
	backend := &watchAuthorityBackend{SSHLeaseBackend: newWatchTestBackend()}
	app := App{Stdout: io.Discard, Stderr: io.Discard}
	opts := watchOptions{Debounce: time.Millisecond, IdleExit: 100 * time.Millisecond, IdleExitSet: true, Command: []string{"true"}}
	cfg := Config{Provider: "watch-test", IdleTimeout: time.Minute, TTL: time.Hour}
	execute := func(ctx context.Context, _ []string, observe func(LeaseTarget)) error {
		lease, err := watchOwnedMutation(ctx, watchTestLeaseID)
		if err == nil {
			observe(lease)
		}
		return err
	}
	if err := app.watchWithBackend(context.Background(), opts, Repo{Root: root, Name: "fixture"}, cfg, backend, execute); err != nil {
		t.Fatal(err)
	}
	if backend.released != 1 {
		t.Fatalf("owned iteration left stale cleanup authority: releases=%d", backend.released)
	}
	if backend.final.SSH.Host != "updated.example.test" {
		t.Fatal("cleanup lost the owned iteration's connection metadata")
	}
}

func TestWatchFinalAuthorityOnIterationExit(t *testing.T) {
	for _, mode := range []string{"setup failure", "nonzero workload", "cancellation", "before mutation"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			t.Setenv("CRABBOX_COORDINATOR", "")
			root := newWatchGitRepo(t)
			backend := &watchAuthorityBackend{SSHLeaseBackend: newWatchTestBackend()}
			app := App{Stdout: io.Discard, Stderr: io.Discard}
			opts := watchOptions{Debounce: time.Millisecond, IdleExit: 100 * time.Millisecond, IdleExitSet: true, Command: []string{"true"}}
			cfg := Config{Provider: "watch-test", IdleTimeout: time.Minute, TTL: time.Hour}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			execute := func(ctx context.Context, _ []string, observe func(LeaseTarget)) error {
				if mode == "before mutation" {
					return errors.New("fixture failed before admission")
				}
				lease, err := watchOwnedMutation(ctx, watchTestLeaseID)
				if err != nil {
					return err
				}
				defer func() {
					// A ready touch after command failure/cancellation is still owned
					// authority. Publish it before the iteration exit is delivered.
					final, err := watchOwnedMutation(context.WithoutCancel(ctx), lease.LeaseID)
					if err != nil {
						t.Error(err)
						return
					}
					observe(final)
				}()
				switch mode {
				case "cancellation":
					cancel()
					return ctx.Err()
				case "nonzero workload":
					return Exit(7, "remote command exited with status 1")
				default:
					return errors.New("fixture setup failed after admission")
				}
			}
			_ = app.watchWithBackend(ctx, opts, Repo{Root: root, Name: "fixture"}, cfg, backend, execute)
			if backend.released != 1 {
				t.Fatalf("cleanup after %s did not use its last committed authority", mode)
			}
		})
	}
}

func TestWatchFinalAuthorityDoesNotAdoptIndependentRevision(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CRABBOX_COORDINATOR", "")
	root := newWatchGitRepo(t)
	backend := &watchAuthorityBackend{SSHLeaseBackend: newWatchTestBackend()}
	opts := watchOptions{Debounce: time.Millisecond, IdleExit: 100 * time.Millisecond, IdleExitSet: true, Command: []string{"true"}}
	cfg := Config{Provider: "watch-test", IdleTimeout: time.Minute, TTL: time.Hour}
	var replacement LeaseClaim
	execute := func(ctx context.Context, _ []string, observe func(LeaseTarget)) error {
		owned, err := watchOwnedMutation(ctx, watchTestLeaseID)
		if err != nil {
			return err
		}
		observe(owned)
		// An independent same-repository/resource revision follows the handoff.
		other, err := watchOwnedMutation(ctx, watchTestLeaseID)
		replacement, _, _ = ServerLeaseClaimSnapshot(other.Server)
		return err
	}
	if err := (App{Stdout: io.Discard, Stderr: io.Discard}).watchWithBackend(context.Background(), opts, Repo{Root: root, Name: "fixture"}, cfg, backend, execute); err != nil {
		t.Fatal(err)
	}
	if backend.released != 0 {
		t.Fatal("watch adopted an independently changed revision at cleanup")
	}
	current, err := ReadLeaseClaim(watchTestLeaseID)
	if err != nil || !reflect.DeepEqual(current, replacement) {
		t.Fatal("watch changed independently replaced authority")
	}
}

func TestWatchFinalAuthorityAcrossSequentialIterations(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CRABBOX_COORDINATOR", "")
	root := newWatchGitRepo(t)
	backend := &watchAuthorityBackend{SSHLeaseBackend: newWatchTestBackend()}
	opts := watchOptions{Debounce: time.Millisecond, IdleExit: time.Second, IdleExitSet: true, Command: []string{"true"}}
	cfg := Config{Provider: "watch-test", IdleTimeout: time.Minute, TTL: time.Hour}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	completed := make(chan struct{}, 2)
	var active, iterations atomic.Int32
	execute := func(ctx context.Context, _ []string, observe func(LeaseTarget)) error {
		if active.Add(1) != 1 {
			t.Error("watch iterations overlapped")
		}
		defer active.Add(-1)
		lease, err := watchOwnedMutation(ctx, watchTestLeaseID)
		if err != nil {
			return err
		}
		n := iterations.Add(1)
		lease.SSH.Host = fmt.Sprintf("iteration-%d.example.test", n)
		observe(lease)
		completed <- struct{}{}
		if n == 2 {
			cancel()
		}
		return nil
	}
	done := make(chan error, 1)
	go func() {
		done <- (App{Stdout: io.Discard, Stderr: io.Discard}).watchWithBackend(ctx, opts, Repo{Root: root, Name: "fixture"}, cfg, backend, execute)
	}()
	select {
	case <-completed:
	case <-ctx.Done():
		t.Fatal("first owned iteration did not complete")
	}
	watchTestWrite(t, root, "next.txt", "trigger a second owned iteration")
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if iterations.Load() != 2 || backend.released != 1 || backend.final.SSH.Host != "iteration-2.example.test" {
		t.Fatalf("final handoff: iterations=%d releases=%d host=%q", iterations.Load(), backend.released, backend.final.SSH.Host)
	}
}

func TestWatchFinalAuthorityRequiresHeldIdentity(t *testing.T) {
	held := LeaseTarget{LeaseID: watchTestLeaseID, Server: Server{Provider: "watch-test", CloudID: "fixture-resource", ImmutableID: "fixture-immutable"}}
	claim := LeaseClaim{LeaseID: held.LeaseID, Provider: held.Server.Provider, CloudID: held.Server.CloudID, CloudImmutableID: held.Server.ImmutableID, RepoRoot: "fixture-repository", ProviderScope: "fixture-scope"}
	SetServerLeaseClaimSnapshot(&held.Server, claim, true)
	if !watchLeaseTargetMatches(held, held) {
		t.Fatal("held identity did not match itself")
	}
	for _, change := range []struct {
		name string
		edit func(*LeaseTarget, *LeaseClaim)
	}{
		{"lease", func(l *LeaseTarget, c *LeaseClaim) { l.LeaseID, c.LeaseID = "cbx_111111111111", "cbx_111111111111" }},
		{"provider", func(l *LeaseTarget, c *LeaseClaim) { l.Server.Provider, c.Provider = "other-test", "other-test" }},
		{"resource", func(l *LeaseTarget, c *LeaseClaim) { l.Server.CloudID, c.CloudID = "other-resource", "other-resource" }},
		{"immutable identity", func(l *LeaseTarget, c *LeaseClaim) {
			l.Server.ImmutableID, c.CloudImmutableID = "other-immutable", "other-immutable"
		}},
		{"scope", func(_ *LeaseTarget, c *LeaseClaim) { c.ProviderScope = "other-scope" }},
		{"repository", func(_ *LeaseTarget, c *LeaseClaim) { c.RepoRoot = "other-repository" }},
	} {
		t.Run(change.name, func(t *testing.T) {
			updated, changed := held, claim
			change.edit(&updated, &changed)
			SetServerLeaseClaimSnapshot(&updated.Server, changed, true)
			if watchLeaseTargetMatches(held, updated) {
				t.Fatal("watch accepted a target outside the held identity")
			}
		})
	}
	missing := held
	SetServerLeaseClaimSnapshot(&missing.Server, LeaseClaim{}, false)
	if watchLeaseTargetMatches(held, missing) {
		t.Fatal("watch accepted absent claim authority")
	}
}
