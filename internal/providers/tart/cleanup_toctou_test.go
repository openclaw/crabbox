package tart

// Repro for a TOCTOU race in (*backend).Cleanup (backend.go:311-382):
//
// Cleanup snapshots instances once via listInstances() (:313) and builds the
// `live` lease-ID set (:321-331) from that stale snapshot, but the orphan-claim
// sweep (:354-377) reads a FRESH listLeaseClaims(). A claim registered by a
// concurrent Acquire (backend.go:184, core.ClaimLeaseForRepoProviderScopePondEndpoint)
// while Cleanup is stuck inside its deletion loop (stopVM/deleteVM can take a
// full VM shutdown each) is therefore visible to the sweep but absent from
// `live`, and gets deleted as "missing instance" — orphaning a healthy,
// running VM and destroying its lease claim + stored SSH key.
//
// The test drives the race deterministically: the fake tart runner blocks in
// `tart stop <stale-vm>` (exactly where a real Cleanup dwells) until a second
// goroutine has registered a new claim through the same production entrypoint
// Acquire uses. On correct code the freshly claimed lease must survive
// Cleanup; on current code the sweep removes it and the test FAILS.

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

type toctouRunner struct {
	mu        sync.Mutex
	responses map[string]core.LocalCommandResult
	onceStop  sync.Once
	// duringStop simulates work happening while `tart stop` is executing
	// (a real VM shutdown takes seconds-to-minutes).
	duringStop func()
}

func (r *toctouRunner) Run(_ context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	if len(req.Args) >= 2 && req.Args[0] == "stop" && r.duringStop != nil {
		r.onceStop.Do(r.duringStop)
	}
	r.mu.Lock()
	resp := r.responses[commandKey(req.Args)]
	r.mu.Unlock()
	return resp, nil
}

func TestCleanupOrphanSweepDeletesConcurrentAcquireClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repoRoot := t.TempDir()

	const (
		staleVM  = "crabbox-stale-old1" // stopped, claimless: makes Cleanup enter its deletion loop
		newVM    = "crabbox-new-fresh1" // cloned+booted by the concurrent Acquire AFTER Cleanup's snapshot
		newLease = "cbx_racefresh12345"
	)

	// Cleanup's one-and-only instance snapshot: taken before the concurrent
	// Acquire clones its VM, so it contains only the stale VM.
	listJSON := `[{"Name":"` + staleVM + `","State":"stopped","Running":false,"Disk":50,"Size":12,"Source":"ghcr.io/test:latest"}]`

	// The concurrent Acquire (process B). It registers its lease claim via the
	// exact production call Acquire performs at backend.go:184, for a VM that
	// is genuinely running by then — it just was not in Cleanup's snapshot.
	acquireErr := make(chan error, 1)
	registerConcurrentClaim := func() {
		go func() {
			server := core.Server{
				CloudID:  newVM,
				Provider: providerName,
				Name:     newVM,
				Status:   "ready",
				Labels: map[string]string{
					"crabbox":  "true",
					"provider": providerName,
					"instance": newVM,
					"lease":    newLease,
					"slug":     "fresh-slug",
					"state":    "ready",
				},
			}
			acquireErr <- core.ClaimLeaseForRepoProviderScopePondEndpoint(
				newLease, "fresh-slug", providerName, instanceScope(newVM), "",
				repoRoot, 30*time.Minute, false, server, core.SSHTarget{},
			)
		}()
		// `tart stop` does not return until the concurrent Acquire has
		// finished claiming — the realistic ordering (VM shutdowns are slow).
		if err := <-acquireErr; err != nil {
			t.Errorf("concurrent Acquire claim registration failed: %v", err)
		}
	}

	runner := &toctouRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"list", "--source", "local", "--format", "json"}): {Stdout: listJSON},
		},
		duringStop: registerConcurrentClaim,
	}

	var out bytes.Buffer
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: &out, Stderr: io.Discard, Exec: runner}).(*backend)

	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	// The invariant: a claim registered for a live, running VM during Cleanup
	// must survive Cleanup. Current code deletes it as "missing instance".
	claim, ok, err := core.ResolveLeaseClaimForProvider(newLease, providerName)
	if err != nil {
		t.Fatalf("ResolveLeaseClaimForProvider(%s): %v", newLease, err)
	}
	if !ok || claim.LeaseID != newLease {
		t.Fatalf("Cleanup's orphan sweep deleted the claim registered by a concurrent Acquire for running VM %s: claim present=%v (want present)\ncleanup output:\n%s",
			newVM, ok, out.String())
	}
	if strings.Contains(out.String(), "remove claim lease="+newLease) {
		t.Fatalf("Cleanup removed the healthy concurrent claim:\n%s", out.String())
	}
}

// TestCleanupOrphanSweepGuardDeclinesReclaimedCandidate exercises the fix's
// SECOND defense. The reorder above stops a brand-new claim from being swept;
// this covers a claim that IS a legitimate orphan candidate (present in the
// pre-instance snapshot, no live instance) but is reclaimed/rebound after the
// snapshot while Cleanup dwells in its stop loop. RemoveLeaseClaimIfUnchanged
// must observe the change and decline the removal, so the reclaiming process
// keeps its lease. Inverting or short-circuiting that guard branch makes the
// sweep report "remove claim" for the rebound lease, which this test rejects.
func TestCleanupOrphanSweepGuardDeclinesReclaimedCandidate(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repoRoot := t.TempDir()

	const (
		staleVM     = "crabbox-stale-old2" // claimless, stopped: makes Cleanup enter its stop loop
		orphanVM    = "crabbox-orphan-cand2"
		orphanLease = "cbx_orphancand67890"
	)

	// Only the stale VM is listed; the candidate's VM is absent, so its claim is
	// a genuine sweep target on the snapshot.
	listJSON := `[{"Name":"` + staleVM + `","State":"stopped","Running":false,"Disk":50,"Size":12,"Source":"ghcr.io/test:latest"}]`

	origServer := core.Server{
		CloudID: orphanVM, Provider: providerName, Name: orphanVM, Status: "idle",
		Labels: map[string]string{
			"crabbox": "true", "provider": providerName, "instance": orphanVM,
			"lease": orphanLease, "slug": "orphan-slug", "state": "idle",
		},
	}
	// Registered BEFORE Cleanup, so it is in the pre-instance orphan snapshot.
	if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(
		orphanLease, "orphan-slug", providerName, instanceScope(orphanVM), "",
		repoRoot, 30*time.Minute, false, origServer, core.SSHTarget{},
	); err != nil {
		t.Fatalf("setup orphan-candidate claim: %v", err)
	}

	// During the stale VM's stop (inside the loop, before the orphan sweep) a
	// concurrent process reclaims the same lease, rewriting the claim so it no
	// longer matches the snapshot the sweep holds.
	reclaimErr := make(chan error, 1)
	reclaim := func() {
		go func() {
			rebound := origServer
			rebound.Status = "ready"
			rebound.Labels = map[string]string{
				"crabbox": "true", "provider": providerName, "instance": orphanVM,
				"lease": orphanLease, "slug": "orphan-slug", "state": "ready",
			}
			reclaimErr <- core.ClaimLeaseForRepoProviderScopePondEndpoint(
				orphanLease, "orphan-slug", providerName, instanceScope(orphanVM), "",
				repoRoot, 30*time.Minute, false, rebound, core.SSHTarget{},
			)
		}()
		if err := <-reclaimErr; err != nil {
			t.Errorf("concurrent reclaim registration failed: %v", err)
		}
	}

	runner := &toctouRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"list", "--source", "local", "--format", "json"}): {Stdout: listJSON},
		},
		duringStop: reclaim,
	}

	var out bytes.Buffer
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: &out, Stderr: io.Discard, Exec: runner}).(*backend)

	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	claim, ok, err := core.ResolveLeaseClaimForProvider(orphanLease, providerName)
	if err != nil {
		t.Fatalf("ResolveLeaseClaimForProvider(%s): %v", orphanLease, err)
	}
	if !ok || claim.LeaseID != orphanLease {
		t.Fatalf("guard failed: a reclaimed orphan-candidate claim was deleted: present=%v\noutput:\n%s", ok, out.String())
	}
	if strings.Contains(out.String(), "remove claim lease="+orphanLease) {
		t.Fatalf("guard branch bypassed: sweep reported removing a reclaimed claim:\n%s", out.String())
	}
}
