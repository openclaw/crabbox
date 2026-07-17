package localcontainer

// Repro for a cross-process TOCTOU race in (*backend).Cleanup, the same class
// hardened for other providers in https://github.com/openclaw/crabbox/pull/1124.
//
// Cleanup snapshots containers via listContainers() BEFORE it reads lease claims
// via core.ListLeaseClaims(). Its orphan sweep then removes — via the unguarded
// core.RemoveLeaseClaim — any provider claim whose ProviderScope matches the
// current runtime/context/host scope but is absent from the stale container
// view. A claim registered by a concurrent Acquire on the SAME scope (a second
// crabbox run against the same docker context on the same host) for a live
// container is visible to the newer claim read but absent from the older
// container view, so it is swept as "missing container" and that live lease's
// claim is destroyed. Trigger is two concurrent same-scope crabbox runs; no
// attacker.
//
// The scope is forced non-empty by faking `docker context show` (runtimeContext
// resolves the scope from it). The fake `docker ps` registers a new same-scope
// claim then returns a container list that predates it. On correct code the
// freshly claimed lease survives Cleanup; on current code the sweep removes it
// and the test FAILS.

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestCleanupOrphanSweepDeletesConcurrentAcquireClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("DOCKER_HOST", "")
	repoRoot := t.TempDir()

	const (
		newContainer = "cbxfreshcontainer01"
		newLease     = "cbx_lcfresh12345"
	)

	var out bytes.Buffer
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.LocalContainer = core.LocalContainerConfig{
		Runtime:  "docker",
		Image:    "ubuntu:24.04",
		User:     "runner",
		WorkRoot: "/workspace/crabbox",
		CPUs:     4,
		Memory:   "8g",
		Network:  "bridge",
	}
	runner := &recordingRunner{}
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: &out, Stderr: io.Discard, Exec: runner}).(*backend)

	var once sync.Once
	runner.run = func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		switch {
		case len(req.Args) >= 2 && req.Args[0] == "context" && req.Args[1] == "show":
			// runtimeContext -> a real, non-empty docker context => non-empty scope
			return core.LocalCommandResult{Stdout: "test-context\n"}, nil
		case len(req.Args) > 0 && req.Args[0] == "ps":
			// The concurrent Acquire (process B): register its claim, on the SAME
			// scope Cleanup will compute, for a container that is genuinely running
			// by then but was not in Cleanup's earlier (stale) container snapshot.
			once.Do(func() {
				scope := b.claimScope(context.Background())
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
					newLease, "fresh-slug", providerName, scope, "", repoRoot,
					30*time.Minute, false, server, core.SSHTarget{},
				); err != nil {
					t.Errorf("concurrent Acquire claim registration failed: %v", err)
				}
			})
			return core.LocalCommandResult{Stdout: ""}, nil // stale snapshot: no containers
		default:
			return core.LocalCommandResult{}, nil
		}
	}

	// Sanity: the scope must be non-empty, otherwise the sweep filter would spare
	// the claim for a different reason and the test would not exercise the bug.
	if scope := b.claimScope(context.Background()); scope == "" {
		t.Fatalf("test setup: claimScope resolved empty; scope-gated sweep would not be exercised")
	}

	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	claim, ok, err := core.ResolveLeaseClaimForProvider(newLease, providerName)
	if err != nil {
		t.Fatalf("ResolveLeaseClaimForProvider(%s): %v", newLease, err)
	}
	if !ok || claim.LeaseID != newLease {
		t.Fatalf("Cleanup's orphan sweep deleted the claim registered by a concurrent same-scope Acquire for running container %s: present=%v (want present)\ncleanup output:\n%s",
			newContainer, ok, out.String())
	}
	if strings.Contains(out.String(), "remove claim lease="+newLease) {
		t.Fatalf("Cleanup removed the healthy concurrent claim:\n%s", out.String())
	}
}

// TestCleanupOrphanSweepGuardDeclinesReclaimedCandidate covers the fix's second
// defense: a claim that IS a legitimate same-scope orphan candidate (present in
// the pre-container snapshot, no live container) but is reclaimed/rebound after
// the snapshot while Cleanup is still listing containers. RemoveLeaseClaimIfUnchanged
// must observe the change and decline the removal, so the reclaiming process keeps
// its lease. On current code the unguarded sweep removes the rebound claim.
func TestCleanupOrphanSweepGuardDeclinesReclaimedCandidate(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("DOCKER_HOST", "")
	repoRoot := t.TempDir()

	const (
		orphanContainer = "cbxorphancontainer01"
		orphanLease     = "cbx_lcorphan67890"
	)

	var out bytes.Buffer
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.LocalContainer = core.LocalContainerConfig{
		Runtime:  "docker",
		Image:    "ubuntu:24.04",
		User:     "runner",
		WorkRoot: "/workspace/crabbox",
		CPUs:     4,
		Memory:   "8g",
		Network:  "bridge",
	}
	runner := &recordingRunner{}
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: &out, Stderr: io.Discard, Exec: runner}).(*backend)

	var scope string
	var once sync.Once
	runner.run = func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		switch {
		case len(req.Args) >= 2 && req.Args[0] == "context" && req.Args[1] == "show":
			return core.LocalCommandResult{Stdout: "test-context\n"}, nil
		case len(req.Args) > 0 && req.Args[0] == "ps":
			// A concurrent process reclaims the same lease, rewriting the claim so
			// it no longer matches the snapshot the sweep holds.
			once.Do(func() {
				rebound := core.Server{
					CloudID:  orphanContainer,
					Provider: providerName,
					Name:     orphanContainer,
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
			})
			return core.LocalCommandResult{Stdout: ""}, nil
		default:
			return core.LocalCommandResult{}, nil
		}
	}
	scope = b.claimScope(context.Background())
	if scope == "" {
		t.Fatalf("test setup: claimScope resolved empty")
	}

	// Registered BEFORE Cleanup with the current scope, so it is in the
	// pre-container orphan snapshot and matches the sweep's scope filter.
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
	if strings.Contains(out.String(), "remove claim lease="+orphanLease) {
		t.Fatalf("orphan sweep removed the reclaimed lease instead of declining:\n%s", out.String())
	}
}

// TestCleanupRetainsKeyPreparedByConcurrentAcquire covers the availability side of
// the fix. A concurrent Acquire/reclaim of the same lease prepares or reuses that
// lease's stored testbox key (EnsureTestboxKeyForConfig) BEFORE it publishes its
// replacement claim. If Cleanup removes the still-stale orphan claim AND deletes
// the key, the concurrent Acquire then publishes a live claim whose container is
// unreachable over SSH because its key is gone. The sweep must therefore RETAIN
// the key even when it removes a genuine orphan claim — matching the merged tart
// fix (https://github.com/openclaw/crabbox/pull/1124); the apple-container sibling
// (https://github.com/openclaw/crabbox/pull/1146) applies the same retention.
func TestCleanupRetainsKeyPreparedByConcurrentAcquire(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	// Isolate the stored-key location (core.TestboxKeyPath uses os.UserConfigDir,
	// which is HOME-based on macOS and XDG_CONFIG_HOME-based on Linux) so the test
	// never writes a key into the developer's real config.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("DOCKER_HOST", "")
	repoRoot := t.TempDir()

	const (
		orphanContainer = "cbxkeyretaincontainer01"
		lease           = "cbx_lckeyretain01"
	)

	var out bytes.Buffer
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.LocalContainer = core.LocalContainerConfig{
		Runtime:  "docker",
		Image:    "ubuntu:24.04",
		User:     "runner",
		WorkRoot: "/workspace/crabbox",
		CPUs:     4,
		Memory:   "8g",
		Network:  "bridge",
	}
	runner := &recordingRunner{}
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: &out, Stderr: io.Discard, Exec: runner}).(*backend)

	runner.run = func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		switch {
		case len(req.Args) >= 2 && req.Args[0] == "context" && req.Args[1] == "show":
			return core.LocalCommandResult{Stdout: "test-context\n"}, nil
		case len(req.Args) > 0 && req.Args[0] == "ps":
			// No live container for this lease -> it is a genuine orphan candidate.
			return core.LocalCommandResult{Stdout: ""}, nil
		default:
			return core.LocalCommandResult{}, nil
		}
	}
	scope := b.claimScope(context.Background())
	if scope == "" {
		t.Fatalf("test setup: claimScope resolved empty")
	}

	// A genuine orphan claim (its container is gone), unchanged during Cleanup, so
	// the guarded sweep legitimately removes the CLAIM.
	server := core.Server{
		CloudID: orphanContainer, Provider: providerName, Name: orphanContainer, Status: "idle",
		Labels: map[string]string{"crabbox": "true", "provider": providerName, "lease": lease, "slug": "keyretain-slug", "state": "idle"},
	}
	if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(
		lease, "keyretain-slug", providerName, scope, "", repoRoot, 30*time.Minute, false, server, core.SSHTarget{},
	); err != nil {
		t.Fatalf("setup orphan claim: %v", err)
	}

	// A concurrent Acquire/reclaim of the SAME lease prepared its key before
	// publishing a replacement claim.
	keyPath, err := core.TestboxKeyPath(lease)
	if err != nil {
		t.Fatalf("TestboxKeyPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		t.Fatalf("mkdir key dir: %v", err)
	}
	if err := os.WriteFile(keyPath, []byte("private-key-prepared-by-concurrent-acquire"), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
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
	// ...but the key prepared by the concurrent Acquire MUST survive, or its live
	// container is left unreachable over SSH.
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("cleanup deleted a stored key prepared by a concurrent Acquire (availability regression): %v\ncleanup output:\n%s", err, out.String())
	}
}
