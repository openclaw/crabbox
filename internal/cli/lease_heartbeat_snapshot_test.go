package cli

import (
	"context"
	"io"
	"reflect"
	"sync"
	"testing"
	"time"
)

type snapshotAttestingTouchBackend struct {
	mu         sync.Mutex
	touches    int
	mismatches int
	done       chan struct{}
	want       int
}

func (b *snapshotAttestingTouchBackend) Spec() ProviderSpec {
	return ProviderSpec{Name: "heartbeat-test"}
}

// Touch mimics a claim-writing provider: it requires the carried snapshot to
// equal the persisted claim, commits its own revision, and returns it.
func (b *snapshotAttestingTouchBackend) Touch(_ context.Context, req TouchRequest) (Server, error) {
	snapshot, _, _ := ServerLeaseClaimSnapshot(req.Lease.Server)
	current, _, err := ReadLeaseClaimWithPresence(req.Lease.LeaseID)
	if err != nil {
		return Server{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.touches++
	if !reflect.DeepEqual(snapshot, current) {
		b.mismatches++
		return Server{}, Exit(2, "lease %s claim changed; retry", req.Lease.LeaseID)
	}
	updated, err := UpdateLeaseClaimLabelsAndLastUsedIfUnchanged(req.Lease.LeaseID, current, map[string]string{"state": "ready"}, time.Now().Add(-time.Hour))
	if err != nil {
		return Server{}, err
	}
	server := Server{Labels: map[string]string{"state": "ready"}}
	SetServerLeaseClaimSnapshot(&server, updated, true)
	if b.touches == b.want {
		close(b.done)
	}
	return server, nil
}

func TestDirectHeartbeatKeepsRenewingWhenProviderCommitsClaimSnapshots(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const leaseID = "cbx_heartbeat_snapshot"
	claim, err := ClaimLeaseForRepoProviderScopePondWithLabels(leaseID, "snapshot", "hyperv", "instance:test", "", "/repo", 30*time.Minute, map[string]string{"state": "ready"})
	if err != nil {
		t.Fatal(err)
	}
	server := Server{Labels: map[string]string{"state": "ready"}}
	SetServerLeaseClaimSnapshot(&server, claim, true)
	backend := &snapshotAttestingTouchBackend{done: make(chan struct{}), want: 3}
	stop := startDirectLeaseHeartbeatWithInterval(context.Background(), backend, LeaseTarget{LeaseID: leaseID, Server: server}, claim, nil, "ready", 30*time.Minute, 5*time.Millisecond, io.Discard)
	select {
	case <-backend.done:
	case <-time.After(10 * time.Second):
		stop()
		t.Fatalf("heartbeat stopped renewing after %d touches (%d stale snapshots)", backend.touches, backend.mismatches)
	}
	stop()
	if backend.mismatches != 0 {
		t.Fatalf("heartbeat handed the provider %d stale claim snapshots", backend.mismatches)
	}
}
