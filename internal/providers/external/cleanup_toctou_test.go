package external

// Repro for a cross-process TOCTOU race in (*leaseBackend).Cleanup, the same
// class hardened for other providers in https://github.com/openclaw/crabbox/pull/1124.
//
// Cleanup snapshots live leases via a protocol "list" call BEFORE it reads lease
// claims via core.ListLeaseClaims(). Its orphan sweep then removes — via the
// unguarded core.RemoveLeaseClaim — any provider claim on the current scope whose
// lease is absent from that stale "list" snapshot. A claim registered by a
// concurrent Acquire on the same scope (a second crabbox run against the same
// external provider) for a live lease is visible to the newer claim read but
// absent from the older list snapshot, so it is swept as missing and that live
// lease's claim (and external routing) is destroyed. Trigger is two concurrent
// same-scope crabbox runs; no attacker.
//
// The fake external command registers a new same-scope claim while answering the
// "list" call, then returns an empty lease list that predates it. On correct code
// the freshly claimed lease survives Cleanup; on current code the sweep removes it
// and the test FAILS.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type cleanupTOCTOURunner struct {
	once   sync.Once
	onList func()
}

func (r *cleanupTOCTOURunner) Run(_ context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	if req.Stdin != nil {
		body, _ := io.ReadAll(req.Stdin)
		var pr struct {
			Operation string `json:"operation"`
		}
		_ = json.Unmarshal(body, &pr)
		if pr.Operation == "list" && r.onList != nil {
			r.once.Do(r.onList)
		}
	}
	return core.LocalCommandResult{Stdout: `{"protocolVersion":1,"leases":[]}`}, nil
}

func TestCleanupOrphanSweepDeletesConcurrentAcquireClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repoRoot := t.TempDir()

	const newLease = "cbx_extfresh12345"

	cfg := testConfig()

	var scope string
	runner := &cleanupTOCTOURunner{
		onList: func() {
			// The concurrent Acquire (process B): register its claim on the SAME
			// scope Cleanup computes, for a lease that is genuinely live by then but
			// was not in Cleanup's earlier (stale) list snapshot.
			server := core.Server{
				CloudID:  "provider/node-fresh",
				Provider: providerName,
				Name:     "crabbox-fresh",
				Status:   "ready",
				Labels: map[string]string{
					"crabbox":  "true",
					"provider": providerName,
					"lease":    newLease,
					"slug":     "fresh-slug",
					"state":    "ready",
				},
			}
			if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(
				newLease, "fresh-slug", providerName, scope, "", repoRoot,
				30*time.Minute, false, server, core.SSHTarget{},
			); err != nil {
				t.Errorf("concurrent Acquire claim registration failed: %v", err)
			}
		},
	}
	var out bytes.Buffer
	backend, err := (Provider{}).Configure(cfg, core.Runtime{Exec: runner, Stdout: &out, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	b := backend.(*leaseBackend)
	scope = b.claimScope()
	if scope == "" {
		t.Fatalf("test setup: external scope resolved empty; scope-gated sweep would not be exercised")
	}

	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	claim, ok, err := core.ResolveLeaseClaimForProvider(newLease, providerName)
	if err != nil {
		t.Fatalf("ResolveLeaseClaimForProvider(%s): %v", newLease, err)
	}
	if !ok || claim.LeaseID != newLease {
		t.Fatalf("Cleanup's orphan sweep deleted the claim registered by a concurrent same-scope Acquire: present=%v (want present)\ncleanup output:\n%s",
			ok, out.String())
	}
}

// TestCleanupOrphanSweepGuardDeclinesReclaimedCandidate covers the fix's second
// defense: a claim that IS a legitimate same-scope orphan candidate (present in
// the pre-list snapshot, absent from the live list) but is reclaimed/rebound after
// the snapshot while Cleanup is still listing. RemoveLeaseClaimIfUnchanged must
// observe the change and decline the removal, so the reclaiming process keeps its
// lease. On current code the unguarded sweep removes the rebound claim.
func TestCleanupOrphanSweepGuardDeclinesReclaimedCandidate(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repoRoot := t.TempDir()

	const orphanLease = "cbx_extorphan67890"

	cfg := testConfig()

	var scope string
	runner := &cleanupTOCTOURunner{
		onList: func() {
			// A concurrent process reclaims the same lease, rewriting the claim so
			// it no longer matches the snapshot the sweep holds.
			rebound := core.Server{
				CloudID:  "provider/node-orphan",
				Provider: providerName,
				Name:     "crabbox-orphan",
				Status:   "ready",
				Labels: map[string]string{
					"crabbox":  "true",
					"provider": providerName,
					"lease":    orphanLease,
					"slug":     "orphan-slug",
					"state":    "ready",
				},
			}
			if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(
				orphanLease, "orphan-slug", providerName, scope, "", repoRoot,
				30*time.Minute, false, rebound, core.SSHTarget{},
			); err != nil {
				t.Errorf("concurrent reclaim registration failed: %v", err)
			}
		},
	}
	var out bytes.Buffer
	backend, err := (Provider{}).Configure(cfg, core.Runtime{Exec: runner, Stdout: &out, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	b := backend.(*leaseBackend)
	scope = b.claimScope()
	if scope == "" {
		t.Fatalf("test setup: external scope resolved empty")
	}

	// Registered BEFORE Cleanup with the current scope, so it is in the pre-list
	// orphan snapshot and is a genuine sweep candidate.
	origServer := core.Server{
		CloudID:  "provider/node-orphan",
		Provider: providerName,
		Name:     "crabbox-orphan",
		Status:   "idle",
		Labels: map[string]string{
			"crabbox":  "true",
			"provider": providerName,
			"lease":    orphanLease,
			"slug":     "orphan-slug",
			"state":    "idle",
		},
	}
	if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(
		orphanLease, "orphan-slug", providerName, scope, "", repoRoot,
		30*time.Minute, false, origServer, core.SSHTarget{},
	); err != nil {
		t.Fatalf("setup orphan-candidate claim: %v", err)
	}

	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	claim, ok, err := core.ResolveLeaseClaimForProvider(orphanLease, providerName)
	if err != nil {
		t.Fatalf("ResolveLeaseClaimForProvider(%s): %v", orphanLease, err)
	}
	if !ok || claim.LeaseID != orphanLease {
		t.Fatalf("orphan sweep removed a lease reclaimed during Cleanup: present=%v (want present)\ncleanup output:\n%s", ok, out.String())
	}
}

// TestCleanupRetainsRoutingForConcurrentAcquire covers the availability side of
// the fix. A concurrent Acquire persists this lease's external routing
// (PersistValidatedExternalRouting, early in Acquire) BEFORE it publishes its
// replacement claim (claimLeaseForRepo). If Cleanup removes the still-stale orphan
// claim AND deletes the routing, the concurrent Acquire then publishes a live
// claim whose lease is unroutable. The sweep must therefore RETAIN the routing
// even when it removes a genuine orphan claim — matching the merged tart fix
// (https://github.com/openclaw/crabbox/pull/1124); the apple-container
// (https://github.com/openclaw/crabbox/pull/1146) and local-container
// (https://github.com/openclaw/crabbox/pull/1147) siblings apply the same retention.
func TestCleanupRetainsRoutingForConcurrentAcquire(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	// Isolate the external routing location (core.ExternalRoutingPath uses
	// os.UserConfigDir, which is HOME-based on macOS and XDG_CONFIG_HOME-based on
	// Linux) so the test never writes routing state into the developer's real config.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	repoRoot := t.TempDir()

	const lease = "cbx_extroutingretain01"

	cfg := testConfig()
	// onList nil: the orphan claim stays unchanged and is legitimately removed.
	runner := &cleanupTOCTOURunner{}
	var out bytes.Buffer
	backend, err := (Provider{}).Configure(cfg, core.Runtime{Exec: runner, Stdout: &out, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	b := backend.(*leaseBackend)
	scope := b.claimScope()
	if scope == "" {
		t.Fatalf("test setup: external scope resolved empty")
	}

	// A genuine orphan claim (its lease is absent from the live "list"), unchanged
	// during Cleanup, so the guarded sweep legitimately removes the CLAIM.
	server := core.Server{
		CloudID: "provider/node-orphan", Provider: providerName, Name: "crabbox-orphan", Status: "idle",
		Labels: map[string]string{"crabbox": "true", "provider": providerName, "lease": lease, "slug": "routingretain-slug", "state": "idle"},
	}
	if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(
		lease, "routingretain-slug", providerName, scope, "", repoRoot, 30*time.Minute, false, server, core.SSHTarget{},
	); err != nil {
		t.Fatalf("setup orphan claim: %v", err)
	}

	// A concurrent Acquire of the SAME lease persisted its routing before
	// publishing a replacement claim.
	routingPath, err := core.PersistExternalRouting(lease, cfg.External)
	if err != nil {
		t.Fatalf("persist routing: %v", err)
	}

	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	// The genuine orphan claim is removed...
	if _, ok, err := core.ResolveLeaseClaimForProvider(lease, providerName); err != nil {
		t.Fatalf("ResolveLeaseClaimForProvider: %v", err)
	} else if ok {
		t.Fatalf("expected the genuine orphan claim to be removed, but it survived\ncleanup output:\n%s", out.String())
	}
	// ...but the routing persisted by the concurrent Acquire MUST survive, or its
	// live lease is left unroutable.
	if _, err := os.Stat(routingPath); err != nil {
		t.Fatalf("cleanup deleted external routing persisted by a concurrent Acquire (availability regression): %v\ncleanup output:\n%s", err, out.String())
	}
}
