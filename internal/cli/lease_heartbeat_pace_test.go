package cli

import (
	"context"
	"io"
	"strconv"
	"sync"
	"testing"
	"time"
)

// claimLabelTouchBackend renews like a claim-backed provider: expiry policy comes
// from the persisted claim labels, not from the request.
type claimLabelTouchBackend struct {
	mu       sync.Mutex
	requests []time.Duration
}

func (b *claimLabelTouchBackend) Spec() ProviderSpec { return ProviderSpec{Name: "heartbeat-test"} }

func (b *claimLabelTouchBackend) Touch(_ context.Context, req TouchRequest) (Server, error) {
	b.mu.Lock()
	b.requests = append(b.requests, req.IdleTimeout)
	b.mu.Unlock()
	current, _, err := ReadLeaseClaimWithPresence(req.Lease.LeaseID)
	if err != nil {
		return Server{}, err
	}
	labels := cloneStringMap(current.Labels)
	if labels == nil {
		labels = map[string]string{}
	}
	labels["state"] = "ready"
	if current.IdleTimeoutSeconds > 0 {
		labels["idle_timeout"] = strconv.Itoa(current.IdleTimeoutSeconds)
		labels["idle_timeout_secs"] = labels["idle_timeout"]
	}
	updated, err := UpdateLeaseClaimLabelsAndLastUsedIfUnchanged(req.Lease.LeaseID, current, labels, time.Now().Add(-time.Hour))
	if err != nil {
		return Server{}, err
	}
	server := Server{Labels: cloneStringMap(updated.Labels)}
	SetServerLeaseClaimSnapshot(&server, updated, true)
	return server, nil
}

func TestDirectHeartbeatPacesRenewalsFromLeaseSavedIdleTimeout(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const leaseID = "cbx_heartbeat_pace"
	// The lease was warmed with a 90s idle timeout; the caller's config says 30m.
	claim, err := ClaimLeaseForRepoProviderScopePondWithLabels(leaseID, "pace", "hyperv", "instance:test", "", "/repo", 90*time.Second, map[string]string{"state": "ready"})
	if err != nil {
		t.Fatal(err)
	}
	server := Server{Labels: map[string]string{"state": "ready"}}
	SetServerLeaseClaimSnapshot(&server, claim, true)
	backend := &claimLabelTouchBackend{}
	var waits []time.Duration
	extendErr := make(chan error, 1)
	finished := make(chan struct{})
	wait := func(ctx context.Context, d time.Duration) bool {
		waits = append(waits, d)
		if len(waits) == 1 {
			// An extend between renewals shortens the saved idle timeout to 30s.
			current, _, err := ReadLeaseClaimWithPresence(leaseID)
			if err == nil {
				shorter := 30 * time.Second
				labels := cloneStringMap(current.Labels)
				labels["idle_timeout"], labels["idle_timeout_secs"] = "30", "30"
				_, err = UpdateLeaseClaimTouchIfUnchanged(ctx, leaseID, current, labels, time.Now(), &shorter)
			}
			extendErr <- err
			return err == nil
		}
		close(finished)
		return false
	}
	var cancelCause error
	cancelOperation := func(cause error) { cancelCause = cause }
	stop := startDirectLeaseHeartbeatPaced(context.Background(), backend, LeaseTarget{LeaseID: leaseID, Server: server}, claim, cancelOperation, "ready", 30*time.Minute, 0, wait, io.Discard)
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		stop()
		t.Fatalf("heartbeat did not complete two renewals; waits=%v", waits)
	}
	stop()
	if err := <-extendErr; err != nil {
		t.Fatalf("simulate extend: %v", err)
	}
	if cancelCause != nil {
		t.Fatalf("heartbeat cancelled the operation: %v", cancelCause)
	}
	if want := []time.Duration{30 * time.Second, 10 * time.Second}; len(waits) != len(want) || waits[0] != want[0] || waits[1] != want[1] {
		t.Fatalf("renewal pace = %v, want %v from the saved idle timeout (config 30m would pace at 1m)", waits, want)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if want := []time.Duration{90 * time.Second, 30 * time.Second}; len(backend.requests) != len(want) || backend.requests[0] != want[0] || backend.requests[1] != want[1] {
		t.Fatalf("touch idle timeouts = %v, want %v from the saved idle timeout", backend.requests, want)
	}
}
