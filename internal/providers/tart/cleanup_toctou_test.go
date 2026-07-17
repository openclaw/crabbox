package tart

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type cleanupRaceRunner struct {
	responses map[string]core.LocalCommandResult
	onceStop  sync.Once
	onStop    func()
}

func (r *cleanupRaceRunner) Run(_ context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	if len(req.Args) >= 2 && req.Args[0] == "stop" && r.onStop != nil {
		r.onceStop.Do(r.onStop)
	}
	return r.responses[commandKey(req.Args)], nil
}

func TestCleanupPreservesClaimCreatedAfterInstanceSnapshot(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repoRoot := t.TempDir()
	const (
		staleVM  = "crabbox-stale-old1"
		newVM    = "crabbox-new-fresh1"
		newLease = "cbx_racefresh12345"
	)

	out := runCleanupRace(t, staleVM, func() {
		claimTartLease(t, repoRoot, newLease, newVM, "ready")
	})
	assertTartClaim(t, newLease, "ready")
	if strings.Contains(out, "remove claim lease="+newLease) {
		t.Fatalf("cleanup reported removing a claim created after its instance snapshot:\n%s", out)
	}
}

func TestCleanupPreservesReclaimedOrphanCandidate(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repoRoot := t.TempDir()
	const (
		staleVM     = "crabbox-stale-old2"
		orphanVM    = "crabbox-orphan-cand2"
		orphanLease = "cbx_orphancand67890"
	)

	claimTartLease(t, repoRoot, orphanLease, orphanVM, "idle")
	out := runCleanupRace(t, staleVM, func() {
		claimTartLease(t, repoRoot, orphanLease, orphanVM, "ready")
	})
	assertTartClaim(t, orphanLease, "ready")
	if strings.Contains(out, "remove claim lease="+orphanLease) {
		t.Fatalf("cleanup reported removing a concurrently reclaimed claim:\n%s", out)
	}
}

func runCleanupRace(t *testing.T, staleVM string, onStop func()) string {
	t.Helper()
	listJSON := `[{"Name":"` + staleVM + `","State":"stopped","Running":false,"Disk":50,"Size":12,"Source":"ghcr.io/test:latest"}]`
	runner := &cleanupRaceRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"list", "--source", "local", "--format", "json"}): {Stdout: listJSON},
		},
		onStop: onStop,
	}
	var out bytes.Buffer
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: &out, Stderr: io.Discard, Exec: runner}).(*backend)
	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	return out.String()
}

func claimTartLease(t *testing.T, repoRoot, leaseID, instance, state string) {
	t.Helper()
	server := core.Server{
		CloudID:  instance,
		Provider: providerName,
		Name:     instance,
		Status:   state,
		Labels: map[string]string{
			"crabbox":  "true",
			"provider": providerName,
			"instance": instance,
			"lease":    leaseID,
			"slug":     "race-slug",
			"state":    state,
		},
	}
	if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(
		leaseID, "race-slug", providerName, instanceScope(instance), "", repoRoot,
		30*time.Minute, false, server, core.SSHTarget{},
	); err != nil {
		t.Fatalf("claim Tart lease %s: %v", leaseID, err)
	}
}

func assertTartClaim(t *testing.T, leaseID, state string) {
	t.Helper()
	claim, ok, err := core.ResolveLeaseClaimForProvider(leaseID, providerName)
	if err != nil {
		t.Fatalf("ResolveLeaseClaimForProvider(%s): %v", leaseID, err)
	}
	if !ok {
		t.Fatalf("cleanup removed live claim %s", leaseID)
	}
	if got := claim.Labels["state"]; got != state {
		t.Fatalf("claim %s state = %q, want %q", leaseID, got, state)
	}
}
