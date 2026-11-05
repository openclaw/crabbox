package cli

import (
	"context"
	"io"
	"reflect"
	"sync"
	"testing"
	"time"
)

// contendedTouchBackend loses its first touch to another claim writer, then
// behaves like a claim-committing provider that enforces exact snapshots.
type contendedTouchBackend struct {
	mu        sync.Mutex
	calls     int
	successes int
	done      chan struct{}
}

func (b *contendedTouchBackend) Spec() ProviderSpec { return ProviderSpec{Name: "heartbeat-test"} }

func (b *contendedTouchBackend) Touch(_ context.Context, req TouchRequest) (Server, error) {
	current, _, err := ReadLeaseClaimWithPresence(req.Lease.LeaseID)
	if err != nil {
		return Server{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	if b.calls == 1 {
		if _, err := UpdateLeaseClaimLabelsAndLastUsedIfUnchanged(req.Lease.LeaseID, current, map[string]string{"state": "ready", "other": "writer"}, time.Now().Add(-time.Hour)); err != nil {
			return Server{}, err
		}
		return Server{}, Exit(2, "lease %s claim changed; retry", req.Lease.LeaseID)
	}
	snapshot, _, _ := ServerLeaseClaimSnapshot(req.Lease.Server)
	if !reflect.DeepEqual(snapshot, current) {
		return Server{}, Exit(2, "lease %s claim changed; retry", req.Lease.LeaseID)
	}
	updated, err := UpdateLeaseClaimLabelsAndLastUsedIfUnchanged(req.Lease.LeaseID, current, map[string]string{"state": "ready", "other": "writer"}, time.Now().Add(-time.Minute))
	if err != nil {
		return Server{}, err
	}
	server := Server{Labels: map[string]string{"state": "ready"}}
	SetServerLeaseClaimSnapshot(&server, updated, true)
	b.successes++
	if b.successes == 2 {
		close(b.done)
	}
	return server, nil
}

func TestDirectHeartbeatRecoversAfterRejectedTouchFromConcurrentClaimWriter(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const leaseID = "cbx_heartbeat_contended"
	claim, err := ClaimLeaseForRepoProviderScopePondWithLabels(leaseID, "contended", "hyperv", "instance:test", "", "/repo", 30*time.Minute, map[string]string{"state": "ready"})
	if err != nil {
		t.Fatal(err)
	}
	server := Server{Labels: map[string]string{"state": "ready"}}
	SetServerLeaseClaimSnapshot(&server, claim, true)
	backend := &contendedTouchBackend{done: make(chan struct{})}
	stop := startDirectLeaseHeartbeatWithInterval(context.Background(), backend, LeaseTarget{LeaseID: leaseID, Server: server}, claim, nil, "ready", 30*time.Minute, 5*time.Millisecond, io.Discard)
	defer stop()
	select {
	case <-backend.done:
	case <-time.After(10 * time.Second):
		t.Fatalf("heartbeat never recovered from a rejected touch (%d calls, %d successes)", backend.calls, backend.successes)
	}
}

func TestDirectHeartbeatCancelsWhenClaimDisappearsAfterRejectedTouch(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const leaseID = "cbx_heartbeat_lost"
	claim, err := ClaimLeaseForRepoProviderScopePondWithLabels(leaseID, "lost", "hyperv", "instance:test", "", "/repo", 30*time.Minute, map[string]string{"state": "ready"})
	if err != nil {
		t.Fatal(err)
	}
	RemoveLeaseClaim(leaseID)
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	stop := startDirectLeaseHeartbeatWithInterval(ctx, failingTouchBackend{}, LeaseTarget{LeaseID: leaseID}, claim, cancel, "ready", 30*time.Minute, 5*time.Millisecond, io.Discard)
	defer stop()
	select {
	case <-ctx.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("heartbeat did not cancel the operation after the claim disappeared")
	}
}

type failingTouchBackend struct{}

func (failingTouchBackend) Spec() ProviderSpec { return ProviderSpec{Name: "heartbeat-test"} }

func (failingTouchBackend) Touch(context.Context, TouchRequest) (Server, error) {
	return Server{}, Exit(2, "touch rejected")
}
