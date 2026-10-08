package cli

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

type directHeartbeatTestBackend struct {
	mu      sync.Mutex
	touches int
	touched chan struct{}
	server  Server
}

func (b *directHeartbeatTestBackend) Spec() ProviderSpec {
	return ProviderSpec{Name: "heartbeat-test"}
}

func (b *directHeartbeatTestBackend) Touch(context.Context, TouchRequest) (Server, error) {
	b.mu.Lock()
	b.touches++
	b.mu.Unlock()
	select {
	case b.touched <- struct{}{}:
	default:
	}
	return b.server, nil
}

func (b *directHeartbeatTestBackend) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.touches
}

func TestStartDirectLeaseHeartbeatRefreshesPersistedClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const leaseID = "cbx_heartbeat_claim"
	expected, err := ClaimLeaseForRepoProviderScopePondWithLabels(
		leaseID,
		"heartbeat-claim",
		"hyperv",
		"instance:test",
		"",
		"/repo",
		30*time.Minute,
		map[string]string{"state": "ready"},
	)
	if err != nil {
		t.Fatal(err)
	}
	expected, err = UpdateLeaseClaimLabelsAndLastUsedIfUnchanged(
		leaseID,
		expected,
		expected.Labels,
		time.Now().Add(-time.Hour),
	)
	if err != nil {
		t.Fatal(err)
	}
	originalLastUsed := expected.LastUsedAt
	backend := &directHeartbeatTestBackend{
		touched: make(chan struct{}, 1),
		server:  Server{Labels: map[string]string{"state": "ready", "instance": "vm-test"}},
	}
	stop := startDirectLeaseHeartbeatWithInterval(
		context.Background(),
		backend,
		LeaseTarget{LeaseID: leaseID},
		expected,
		nil,
		"ready",
		30*time.Minute,
		time.Hour,
		io.Discard,
	)
	select {
	case <-backend.touched:
	case <-time.After(200 * time.Millisecond):
		stop()
		t.Fatal("timed out waiting for initial direct lease heartbeat")
	}
	stop()

	updated, exists, err := ResolveLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("persisted claim was removed")
	}
	if updated.LastUsedAt == "" || updated.LastUsedAt == originalLastUsed {
		t.Fatalf("last used was not refreshed: before=%q after=%q", originalLastUsed, updated.LastUsedAt)
	}
	if updated.Labels["instance"] != "vm-test" {
		t.Fatalf("claim labels were not refreshed: %#v", updated.Labels)
	}
}

func TestStartDirectLeaseHeartbeatCancelsOperationWhenClaimOwnershipChanges(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const leaseID = "cbx_heartbeat_owner"
	expected, err := ClaimLeaseForRepoProviderScopePondWithLabels(
		leaseID,
		"heartbeat-owner",
		"hyperv",
		"instance:test",
		"",
		"/repo-a",
		30*time.Minute,
		map[string]string{"state": "ready"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := ClaimLeaseForRepoProviderScopePond(
		leaseID,
		"heartbeat-owner",
		"hyperv",
		"instance:test",
		"",
		"/repo-b",
		30*time.Minute,
		true,
	); err != nil {
		t.Fatal(err)
	}

	operationCtx, cancelOperation := context.WithCancelCause(context.Background())
	backend := &directHeartbeatTestBackend{
		touched: make(chan struct{}, 1),
		server:  Server{Labels: map[string]string{"provider": "hyperv", "state": "ready"}},
	}
	stop := startDirectLeaseHeartbeatWithInterval(
		operationCtx,
		backend,
		LeaseTarget{LeaseID: leaseID},
		expected,
		cancelOperation,
		"ready",
		30*time.Minute,
		time.Hour,
		io.Discard,
	)
	defer stop()
	select {
	case <-operationCtx.Done():
	case <-time.After(200 * time.Millisecond):
		t.Fatal("operation was not canceled after claim ownership changed")
	}
	if cause := context.Cause(operationCtx); cause == nil || !strings.Contains(cause.Error(), "changed ownership") {
		t.Fatalf("operation cause=%v", cause)
	}
}

func TestRefreshActionsHeartbeatClaimRetainsOwnershipBaselineAfterCASFailure(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const leaseID = "cbx_heartbeat_cas"
	expected, err := ClaimLeaseForRepoProviderScopePondWithLabels(
		leaseID,
		"heartbeat-cas",
		"hyperv",
		"instance:test",
		"",
		"/repo",
		30*time.Minute,
		map[string]string{"state": "ready"},
	)
	if err != nil {
		t.Fatal(err)
	}
	latest, err := UpdateLeaseClaimLabelsAndLastUsedIfUnchanged(
		leaseID,
		expected,
		map[string]string{"state": "ready", "touch": "concurrent"},
		time.Now().UTC(),
	)
	if err != nil {
		t.Fatal(err)
	}
	server := Server{Labels: map[string]string{"state": "ready", "instance": "vm-test"}}
	next, updateErr, fatalErr := refreshActionsHeartbeatClaim(
		leaseID,
		expected,
		expected,
		server,
		time.Now().UTC(),
	)
	if updateErr == nil {
		t.Fatal("expected stale claim update to fail")
	}
	if fatalErr != nil {
		t.Fatalf("same-owner CAS failure became fatal: %v", fatalErr)
	}
	if next.LeaseID != leaseID || next.Revision != latest.Revision {
		t.Fatalf("ownership baseline was not refreshed: next=%#v latest=%#v", next, latest)
	}
}

func TestRefreshActionsHeartbeatClaimTreatsOwnershipCASFailureAsFatal(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const leaseID = "cbx_heartbeat_cas_owner"
	expected, err := ClaimLeaseForRepoProviderScopePondWithLabels(
		leaseID,
		"heartbeat-cas-owner",
		"hyperv",
		"instance:test",
		"",
		"/repo-a",
		30*time.Minute,
		map[string]string{"state": "ready"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := ClaimLeaseForRepoProviderScopePond(
		leaseID,
		"heartbeat-cas-owner",
		"hyperv",
		"instance:test",
		"",
		"/repo-b",
		30*time.Minute,
		true,
	); err != nil {
		t.Fatal(err)
	}
	server := Server{Labels: map[string]string{"state": "ready", "instance": "vm-test"}}
	next, updateErr, fatalErr := refreshActionsHeartbeatClaim(
		leaseID,
		expected,
		expected,
		server,
		time.Now().UTC(),
	)
	if updateErr == nil {
		t.Fatal("expected stale claim update to fail")
	}
	if fatalErr == nil || !strings.Contains(fatalErr.Error(), "changed ownership") {
		t.Fatalf("fatal error=%v", fatalErr)
	}
	if next.Revision != expected.Revision {
		t.Fatalf("fatal ownership change replaced baseline: next=%#v expected=%#v", next, expected)
	}
}

func TestStartDirectLeaseHeartbeatTouchesUntilStopped(t *testing.T) {
	backend := &directHeartbeatTestBackend{touched: make(chan struct{}, 8)}
	stop := startDirectLeaseHeartbeatWithInterval(
		context.Background(),
		backend,
		LeaseTarget{LeaseID: "cbx_heartbeat"},
		leaseClaim{},
		nil,
		"ready",
		30*time.Minute,
		2*time.Millisecond,
		io.Discard,
	)

	for range 3 {
		select {
		case <-backend.touched:
		case <-time.After(200 * time.Millisecond):
			stop()
			t.Fatal("timed out waiting for direct lease heartbeat")
		}
	}
	stop()
	count := backend.count()
	time.Sleep(10 * time.Millisecond)
	if got := backend.count(); got != count {
		t.Fatalf("touches continued after stop: before=%d after=%d", count, got)
	}
}
