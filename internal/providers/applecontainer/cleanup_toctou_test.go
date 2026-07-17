package applecontainer

// Repro for a cross-process TOCTOU race in (*backend).Cleanup, the same class
// hardened for other providers in https://github.com/openclaw/crabbox/pull/1124
// (tart), https://github.com/openclaw/crabbox/pull/446 (aws/azure), and
// https://github.com/openclaw/crabbox/pull/781 (freestyle/islo).
//
// Before this fix, Cleanup snapshotted containers via listContainers() BEFORE it
// read the claim set via core.ListLeaseClaims(), then its orphan sweep removed —
// via the unguarded core.RemoveLeaseClaim — any claim in that (newer) claim
// snapshot whose lease was absent from the liveLeases set built from the (older)
// container snapshot. A claim registered by a concurrent Acquire
// (core.ClaimLeaseForRepoProviderScopePondEndpoint) while Cleanup was still
// listing containers was therefore visible to the claim read but absent from
// liveLeases, and was deleted as "missing container" — destroying a healthy
// concurrent lease's claim. Trigger is two concurrent crabbox runs on one Mac
// (apple-container is the local provider); no attacker required.
//
// The test drives it deterministically: the fake `container ls` registers a new
// claim through the exact core entrypoint Acquire uses, then returns a container
// list that predates it. It FAILS against the pre-fix Cleanup (the fresh claim is
// swept) and PASSES with the fix (the fresh claim postdates the pre-container
// snapshot and survives).

import (
	"bytes"
	"context"
	"io"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type cleanupTOCTOURunner struct {
	once       sync.Once
	duringList func()
	lsJSON     string
}

func (r *cleanupTOCTOURunner) Run(_ context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	if len(req.Args) > 0 && req.Args[0] == "ls" {
		if r.duringList != nil {
			r.once.Do(r.duringList)
		}
		return core.LocalCommandResult{Stdout: r.lsJSON}, nil
	}
	return core.LocalCommandResult{}, nil
}

func TestCleanupOrphanSweepDeletesConcurrentAcquireClaim(t *testing.T) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("apple-container Cleanup requires macOS on Apple silicon")
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repoRoot := t.TempDir()

	const (
		newContainer = "crabbox-fresh-01"
		newLease     = "cbx_acfresh12345"
	)

	// The concurrent Acquire (process B): registers its claim through the exact
	// core entrypoint applecontainer's Acquire uses (backend.go:203), for a
	// container that is genuinely running by then -- it just was not present in
	// Cleanup's earlier container snapshot.
	duringList := func() {
		server := core.Server{
			CloudID:  newContainer,
			Provider: providerName,
			Name:     newContainer,
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
			newLease, "fresh-slug", providerName, "", "", repoRoot,
			30*time.Minute, false, server, core.SSHTarget{},
		); err != nil {
			t.Errorf("concurrent Acquire claim registration failed: %v", err)
		}
	}

	// Cleanup's container snapshot predates the concurrent Acquire's container,
	// so it lists no crabbox containers.
	runner := &cleanupTOCTOURunner{lsJSON: "[]", duringList: duringList}

	var out bytes.Buffer
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.AppleContainer = core.AppleContainerConfig{
		CLIPath:  "container",
		Image:    "debian:bookworm",
		User:     "runner",
		WorkRoot: "/work/crabbox",
		CPUs:     4,
		Memory:   "8g",
	}
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: &out, Stderr: io.Discard, Exec: runner}).(*backend)

	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	// Invariant: a claim registered for a live container during Cleanup must
	// survive Cleanup. Current code deletes it as "missing container".
	claim, ok, err := core.ResolveLeaseClaimForProvider(newLease, providerName)
	if err != nil {
		t.Fatalf("ResolveLeaseClaimForProvider(%s): %v", newLease, err)
	}
	if !ok || claim.LeaseID != newLease {
		t.Fatalf("Cleanup's orphan sweep deleted the claim registered by a concurrent Acquire for running container %s: present=%v (want present)\ncleanup output:\n%s",
			newContainer, ok, out.String())
	}
	if strings.Contains(out.String(), "remove claim lease="+newLease) {
		t.Fatalf("Cleanup removed the healthy concurrent claim:\n%s", out.String())
	}
}

// TestCleanupOrphanSweepGuardDeclinesReclaimedCandidate covers the fix's second
// defense. The reorder above stops a brand-new claim from being swept; this
// covers a claim that IS a legitimate orphan candidate (present in the
// pre-container snapshot, no live container) but is reclaimed/rebound after the
// snapshot while Cleanup is still listing containers. RemoveLeaseClaimIfUnchanged
// must observe the change and decline the removal, so the reclaiming process
// keeps its lease. On current code the unguarded sweep removes the rebound claim.
func TestCleanupOrphanSweepGuardDeclinesReclaimedCandidate(t *testing.T) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("apple-container Cleanup requires macOS on Apple silicon")
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repoRoot := t.TempDir()

	const (
		orphanContainer = "crabbox-orphan-01"
		orphanLease     = "cbx_acorphan67890"
	)

	origServer := core.Server{
		CloudID:  orphanContainer,
		Provider: providerName,
		Name:     orphanContainer,
		Status:   "idle",
		Labels: map[string]string{
			"crabbox":  "true",
			"provider": providerName,
			"lease":    orphanLease,
			"slug":     "orphan-slug",
			"state":    "idle",
		},
	}
	// Registered BEFORE Cleanup, so it is in the pre-container orphan snapshot.
	if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(
		orphanLease, "orphan-slug", providerName, "", "", repoRoot,
		30*time.Minute, false, origServer, core.SSHTarget{},
	); err != nil {
		t.Fatalf("setup orphan-candidate claim: %v", err)
	}

	// During listContainers a concurrent process reclaims the same lease,
	// rewriting the claim so it no longer matches the snapshot the sweep holds.
	reclaim := func() {
		rebound := origServer
		rebound.Status = "ready"
		rebound.Labels = map[string]string{
			"crabbox":  "true",
			"provider": providerName,
			"lease":    orphanLease,
			"slug":     "orphan-slug",
			"state":    "ready",
		}
		if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(
			orphanLease, "orphan-slug", providerName, "", "", repoRoot,
			30*time.Minute, false, rebound, core.SSHTarget{},
		); err != nil {
			t.Errorf("concurrent reclaim registration failed: %v", err)
		}
	}

	runner := &cleanupTOCTOURunner{lsJSON: "[]", duringList: reclaim}

	var out bytes.Buffer
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.AppleContainer = core.AppleContainerConfig{
		CLIPath:  "container",
		Image:    "debian:bookworm",
		User:     "runner",
		WorkRoot: "/work/crabbox",
		CPUs:     4,
		Memory:   "8g",
	}
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: &out, Stderr: io.Discard, Exec: runner}).(*backend)

	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	// The guard must observe the reclaim and decline the removal, so the
	// reclaiming process keeps its lease.
	claim, ok, err := core.ResolveLeaseClaimForProvider(orphanLease, providerName)
	if err != nil {
		t.Fatalf("ResolveLeaseClaimForProvider(%s): %v", orphanLease, err)
	}
	if !ok || claim.LeaseID != orphanLease {
		t.Fatalf("orphan sweep removed a lease reclaimed during Cleanup: present=%v (want present)\ncleanup output:\n%s", ok, out.String())
	}
	if strings.Contains(out.String(), "remove claim lease="+orphanLease) {
		t.Fatalf("orphan sweep removed the reclaimed lease instead of declining:\n%s", out.String())
	}
}

func TestCleanupDryRunDoesNotPlanReclaimedCandidateRemoval(t *testing.T) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("apple-container Cleanup requires macOS on Apple silicon")
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repoRoot := t.TempDir()

	const (
		orphanContainer = "crabbox-dryrun-orphan-01"
		orphanLease     = "cbx_acdryrun67890"
	)
	origServer := core.Server{
		CloudID:  orphanContainer,
		Provider: providerName,
		Name:     orphanContainer,
		Status:   "idle",
		Labels: map[string]string{
			"crabbox":  "true",
			"provider": providerName,
			"lease":    orphanLease,
			"slug":     "dryrun-orphan-slug",
			"state":    "idle",
		},
	}
	if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(
		orphanLease, "dryrun-orphan-slug", providerName, "", "", repoRoot,
		30*time.Minute, false, origServer, core.SSHTarget{},
	); err != nil {
		t.Fatalf("setup dry-run orphan-candidate claim: %v", err)
	}

	reclaim := func() {
		rebound := origServer
		rebound.Status = "ready"
		rebound.Labels = map[string]string{
			"crabbox":  "true",
			"provider": providerName,
			"lease":    orphanLease,
			"slug":     "dryrun-orphan-slug",
			"state":    "ready",
		}
		if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(
			orphanLease, "dryrun-orphan-slug", providerName, "", "", repoRoot,
			30*time.Minute, false, rebound, core.SSHTarget{},
		); err != nil {
			t.Errorf("concurrent dry-run reclaim registration failed: %v", err)
		}
	}

	runner := &cleanupTOCTOURunner{lsJSON: "[]", duringList: reclaim}
	var out bytes.Buffer
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.AppleContainer = core.AppleContainerConfig{
		CLIPath:  "container",
		Image:    "debian:bookworm",
		User:     "runner",
		WorkRoot: "/work/crabbox",
		CPUs:     4,
		Memory:   "8g",
	}
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: &out, Stderr: &out, Exec: runner}).(*backend)

	if err := b.Cleanup(context.Background(), core.CleanupRequest{DryRun: true}); err != nil {
		t.Fatalf("Cleanup dry-run: %v", err)
	}
	if strings.Contains(out.String(), "would remove claim lease="+orphanLease) {
		t.Fatalf("dry-run planned removal of a claim reclaimed during Cleanup:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "skip claim lease="+orphanLease+" reason=changed-during-cleanup") {
		t.Fatalf("dry-run did not report the reclaimed claim as skipped:\n%s", out.String())
	}
}
