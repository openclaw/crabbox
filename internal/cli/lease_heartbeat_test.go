package cli

import (
	"context"
	"io"
	"os"
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
	// Stopping cancels in-flight claim publication, so wait for it to land first.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if current, ok, _ := ResolveLeaseClaim(leaseID); ok && current.LastUsedAt != originalLastUsed {
			break
		}
		time.Sleep(5 * time.Millisecond)
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
		context.Background(),
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
		context.Background(),
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

func TestStartDirectLeaseHeartbeatSkipsTouchWhenClaimWasReassigned(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const leaseID = "cbx_heartbeat_reassigned"
	expected, err := ClaimLeaseForRepoProviderScopePondWithLabels(leaseID, "hb-reassigned", "hyperv", "instance:test", "", "/repo-a", 30*time.Minute, map[string]string{"state": "ready"})
	if err != nil {
		t.Fatal(err)
	}
	RemoveLeaseClaim(leaseID)
	if _, err := ClaimLeaseForRepoProviderScopePondWithLabels(leaseID, "hb-reassigned", "hyperv", "instance:test", "", "/repo-b", 30*time.Minute, map[string]string{"state": "ready"}); err != nil {
		t.Fatal(err)
	}
	backend := &directHeartbeatTestBackend{touched: make(chan struct{}, 1)}
	cancelled := make(chan error, 1)
	stop := startDirectLeaseHeartbeatWithInterval(context.Background(), backend, LeaseTarget{LeaseID: leaseID}, expected,
		func(cause error) { cancelled <- cause }, "ready", 30*time.Minute, time.Hour, io.Discard)
	defer stop()
	select {
	case cause := <-cancelled:
		if cause == nil {
			t.Fatal("expected cancellation cause")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("heartbeat did not stop on reassigned claim")
	}
	if n := backend.count(); n != 0 {
		t.Fatalf("Touch called %d times for a reassigned claim", n)
	}
}

func TestWithClaimLifetimePolicyCarriesExtendedLifetimeIntoTouchLabels(t *testing.T) {
	got := withClaimLifetimePolicy(
		map[string]string{"state": "ready", "ttl_secs": "60", "expires_at": "old"},
		map[string]string{"ttl_secs": "7200", "expires_at": "new", "idle_timeout_secs": ""},
		nil,
	)
	if got["ttl_secs"] != "7200" || got["expires_at"] != "new" || got["state"] != "ready" {
		t.Fatalf("lifetime policy not carried: %#v", got)
	}
	if _, ok := got["idle_timeout_secs"]; ok {
		t.Fatalf("empty claim label overwrote server label: %#v", got)
	}
}

func TestRefreshActionsHeartbeatClaimReturnsPromptlyWhenContextCanceledUnderLockContention(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const leaseID = "cbx_heartbeat_ctx"
	claim, err := ClaimLeaseForRepoProviderScopePondWithLabels(leaseID, "heartbeat-ctx", "hyperv", "instance:test", "", "/repo", 30*time.Minute, map[string]string{"state": "ready"})
	if err != nil {
		t.Fatal(err)
	}
	held := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- WithLeaseClaimUnchanged(leaseID, claim, func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	defer func() { close(release); <-done }()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	_, updateErr, _ := refreshActionsHeartbeatClaim(ctx, leaseID, claim, claim, Server{Labels: map[string]string{"state": "ready"}}, time.Now().UTC())
	if updateErr == nil {
		t.Fatal("expected context error while claim lock is held")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("canceled publication blocked for %s", elapsed)
	}
}

type extendingTouchBackend struct {
	directHeartbeatTestBackend
	leaseID  string
	extended map[string]string
}

func (b *extendingTouchBackend) Touch(ctx context.Context, req TouchRequest) (Server, error) {
	claim, _, err := ResolveLeaseClaim(b.leaseID)
	if err != nil {
		return Server{}, err
	}
	labels := cloneStringMap(claim.Labels)
	for k, v := range b.extended {
		labels[k] = v
	}
	if _, err := UpdateLeaseClaimLabelsIfUnchanged(b.leaseID, claim, labels); err != nil {
		return Server{}, err
	}
	return b.directHeartbeatTestBackend.Touch(ctx, req)
}

type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestStartDirectLeaseHeartbeatKeepsExtensionLandingDuringTouch(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const leaseID = "cbx_heartbeat_extend"
	expected, err := ClaimLeaseForRepoProviderScopePondWithLabels(leaseID, "hb-extend", "hyperv", "instance:test", "", "/repo", 30*time.Minute, map[string]string{"state": "ready", "ttl_secs": "60"})
	if err != nil {
		t.Fatal(err)
	}
	expected, err = UpdateLeaseClaimLabelsAndLastUsedIfUnchanged(leaseID, expected, expected.Labels, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	backend := &extendingTouchBackend{
		directHeartbeatTestBackend: directHeartbeatTestBackend{touched: make(chan struct{}, 1), server: Server{Labels: map[string]string{"state": "ready", "ttl_secs": "60"}}},
		leaseID:                    leaseID,
		extended:                   map[string]string{"ttl_secs": "7200"},
	}
	stop := startDirectLeaseHeartbeatWithInterval(context.Background(), backend, LeaseTarget{LeaseID: leaseID}, expected, nil, "ready", 30*time.Minute, time.Hour, io.Discard)
	<-backend.touched
	deadline := time.Now().Add(10 * time.Second)
	var got leaseClaim
	for time.Now().Before(deadline) {
		got, _, _ = ResolveLeaseClaim(leaseID)
		if got.Labels["ttl_secs"] == "7200" && got.LastUsedAt != expected.LastUsedAt {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	stop()
	if got.Labels["ttl_secs"] != "7200" {
		t.Fatalf("heartbeat publication reverted concurrent extension: ttl_secs=%q", got.Labels["ttl_secs"])
	}
}

func TestStartDirectLeaseHeartbeatSkipsTouchWhenClaimIsUnreadable(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const leaseID = "cbx_heartbeat_unreadable"
	expected, err := ClaimLeaseForRepoProviderScopePondWithLabels(leaseID, "hb-unreadable", "hyperv", "instance:test", "", "/repo", 30*time.Minute, map[string]string{"state": "ready"})
	if err != nil {
		t.Fatal(err)
	}
	path, err := leaseClaimPath(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	backend := &directHeartbeatTestBackend{touched: make(chan struct{}, 1)}
	stderr := &lockedBuffer{}
	stop := startDirectLeaseHeartbeatWithInterval(context.Background(), backend, LeaseTarget{LeaseID: leaseID}, expected, nil, "ready", 30*time.Minute, time.Hour, stderr)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(stderr.String(), "warning") {
		time.Sleep(5 * time.Millisecond)
	}
	stop()
	if !strings.Contains(stderr.String(), "warning") {
		t.Fatal("heartbeat did not report the unreadable claim")
	}
	if n := backend.count(); n != 0 {
		t.Fatalf("Touch called %d times while the claim was unreadable", n)
	}
}

func TestStartDirectLeaseHeartbeatPublishesRenewedExpiryFromTouch(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const leaseID = "cbx_heartbeat_renew"
	expected, err := ClaimLeaseForRepoProviderScopePondWithLabels(leaseID, "hb-renew", "hyperv", "instance:test", "", "/repo", 30*time.Minute, map[string]string{"state": "ready", "expires_at": "2026-01-01T00:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	expected, err = UpdateLeaseClaimLabelsAndLastUsedIfUnchanged(leaseID, expected, expected.Labels, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	backend := &directHeartbeatTestBackend{touched: make(chan struct{}, 1), server: Server{Labels: map[string]string{"state": "ready", "expires_at": "2026-01-01T01:00:00Z"}}}
	stop := startDirectLeaseHeartbeatWithInterval(context.Background(), backend, LeaseTarget{LeaseID: leaseID}, expected, nil, "ready", 30*time.Minute, time.Hour, io.Discard)
	<-backend.touched
	deadline := time.Now().Add(10 * time.Second)
	var got leaseClaim
	for time.Now().Before(deadline) {
		got, _, _ = ResolveLeaseClaim(leaseID)
		if got.Labels["expires_at"] == "2026-01-01T01:00:00Z" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	stop()
	if got.Labels["expires_at"] != "2026-01-01T01:00:00Z" {
		t.Fatalf("claim kept the stale expiry %q instead of the touch result", got.Labels["expires_at"])
	}
}
