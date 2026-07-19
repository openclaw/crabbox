package applevm

// Repro for a cross-process TOCTOU race in (*backend).Cleanup, the same class
// hardened for other providers in https://github.com/openclaw/crabbox/pull/1124
// (tart), https://github.com/openclaw/crabbox/pull/446 (aws/azure), and
// https://github.com/openclaw/crabbox/pull/781 (freestyle/islo).
//
// Before this fix, Cleanup snapshotted instances before it re-read claims via
// core.ListLeaseClaims(), then its orphan sweep removed — via the unguarded
// core.RemoveLeaseClaim — any claim in that (newer) claim snapshot
// whose lease was absent from the live claim set built from the (older)
// instance snapshot. A claim registered by a concurrent Acquire
// (core.ClaimLeaseForRepoProviderScopePondEndpoint) while Cleanup was still
// listing instances was therefore visible to the claim read but absent from live,
// and was deleted as "missing instance" — destroying a healthy concurrent
// lease's claim. Trigger is two concurrent crabbox runs on one Mac (apple-vm is
// the local provider); no attacker required.
//
// The tests drive it deterministically. apple-vm additionally skips claims within
// unclaimedInstanceGrace (3h) before reaching any guard, so the orphan candidates
// are backdated past that grace; the fake `helper list` then reclaims one through
// the exact core entrypoint Acquire uses (which rewrites LastUsedAt=now) and returns
// an instance list that predates the reclaim.
//
// Against the pre-fix Cleanup the reclaimed claim is read only after listInstances —
// by then LastUsedAt=now pulls it back inside the startup grace — so base grace-skips
// it (prints "reason=startup grace period") and the claim survives; the destructive
// base defect these tests actually pin is the stored-key deletion (see
// TestCleanupRetainsKeyPreparedByConcurrentAcquire, whose orphan is not reclaimed).
// With the fix, candidates are snapshotted before listInstances, so the still-aged
// candidate reaches the CAS guard, which declines the changed claim
// ("reason=changed-during-cleanup") and retains the key. All three FAIL on base and
// PASS with the fix.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openclaw/crabbox/internal/applevmhelper"
	core "github.com/openclaw/crabbox/internal/cli"
)

func writeAgedAppleVMOrphanClaim(t *testing.T, leaseID, slug, name string) {
	t.Helper()
	writeAgedOrphanClaim(t, leaseID, slug, name, providerName, instanceScope(name))
}

// writeAgedOrphanClaim registers an orphan claim for an arbitrary provider/scope and
// backdates it past the startup grace, so cleanup reaches its guarded removal path
// rather than the grace skip. Used to plant both apple-vm orphans and foreign-provider
// claims (the latter must be skipped by the sweep's provider filter).
func writeAgedOrphanClaim(t *testing.T, leaseID, slug, name, provider, scope string) {
	t.Helper()

	server := core.Server{CloudID: name, Provider: provider, Name: name, Labels: map[string]string{
		"instance": name,
		"lease":    leaseID,
		"provider": provider,
		"slug":     slug,
	}}
	if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(leaseID, slug, provider, scope, "", t.TempDir(), 5*time.Minute, false, server, core.SSHTarget{}); err != nil {
		t.Fatalf("setup orphan claim: %v", err)
	}

	before, err := core.ReadLeaseClaim(leaseID)
	if err != nil {
		t.Fatalf("read claim %s before rewrite: %v", leaseID, err)
	}

	stateHome := os.Getenv("XDG_STATE_HOME")
	if strings.TrimSpace(stateHome) == "" {
		t.Fatal("XDG_STATE_HOME is not set")
	}
	path := filepath.Join(stateHome, "crabbox", "claims", leaseID+".json")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read claim file %s: %v", path, err)
	}

	var claim map[string]any
	if err := json.Unmarshal(data, &claim); err != nil {
		t.Fatalf("parse claim file %s: %v", path, err)
	}

	backdate := time.Now().UTC().Add(-4 * time.Hour).Format(time.RFC3339)
	claim["claimedAt"] = backdate
	claim["lastUsedAt"] = backdate

	rewritten, err := json.Marshal(claim)
	if err != nil {
		t.Fatalf("marshal claim file %s: %v", path, err)
	}
	if err := os.WriteFile(path, rewritten, 0o600); err != nil {
		t.Fatalf("rewrite claim file %s: %v", path, err)
	}

	readback, err := core.ReadLeaseClaim(leaseID)
	if err != nil {
		t.Fatalf("read back claim %s: %v", leaseID, err)
	}
	if readback.ClaimedAt != backdate || readback.LastUsedAt != backdate {
		t.Fatalf("failed to rewrite claim %s: claimedAt=%q lastUsedAt=%q want %q", leaseID, readback.ClaimedAt, readback.LastUsedAt, backdate)
	}
	if readback.ClaimedAt == before.ClaimedAt {
		t.Fatalf("claim %s was not backdated", leaseID)
	}
	if claimWithinStartupGrace(readback, time.Now().UTC()) {
		t.Fatalf("backdated claim %s is still within startup grace", leaseID)
	}
}

func TestCleanupOrphanSweepGuardDeclinesReclaimedCandidate(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	var out bytes.Buffer

	oldGOOS, oldGOARCH := hostGOOS, hostGOARCH
	oldMacOSVersion := hostMacOSVersion
	hostGOOS, hostGOARCH = "darwin", "arm64"
	hostMacOSVersion = func() (string, error) { return "26.5", nil }
	t.Cleanup(func() {
		hostGOOS, hostGOARCH = oldGOOS, oldGOARCH
		hostMacOSVersion = oldMacOSVersion
	})
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".state"))
	root := t.TempDir()

	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.AppleVM = core.AppleVMConfig{
		HelperPath:  "/tmp/helper-source",
		Image:       "https://cloud-images.ubuntu.com/releases/noble/release-20260518/ubuntu-24.04-server-cloudimg-arm64.img",
		ImageSHA256: "6a61b967ba4a27dd1966f835a67643073ed55c2860ce3dc1cb0517282e6b8bec",
		User:        "runner",
		WorkRoot:    "/workspace/crabbox",
		CPUs:        4,
		MemoryMiB:   8192,
		DiskGiB:     40,
	}
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: &out, Stderr: &out, Exec: runner}).(*backend)
	b.prepareHelper = func(context.Context, core.Config) (string, error) { return "helper", nil }
	b.stateRoot = func() (string, error) { return root, nil }
	b.waitForSSH = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error { return nil }

	leaseID := "cbx_orphan67890"
	slug := "orphan-slug"
	name := core.LeaseProviderName(leaseID, slug)
	writeAgedAppleVMOrphanClaim(t, leaseID, slug, name)

	var once sync.Once
	runner.hook = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		if len(req.Args) > 0 && req.Args[0] == "list" {
			once.Do(func() {
				existing, err := core.ReadLeaseClaim(leaseID)
				if err != nil {
					t.Errorf("read existing claim %s before reclaim: %v", leaseID, err)
					return
				}
				server := core.Server{
					CloudID:  name,
					Provider: providerName,
					Name:     name,
					Status:   "ready",
					Labels: map[string]string{
						"crabbox":  "true",
						"provider": providerName,
						"lease":    leaseID,
						"slug":     slug,
						"state":    "ready",
					},
				}
				if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(leaseID, slug, providerName, instanceScope(name), "", existing.RepoRoot, 5*time.Minute, false, server, core.SSHTarget{}); err != nil {
					t.Errorf("concurrent reclaim registration failed: %v", err)
				}
			})
		}
		return core.LocalCommandResult{}, nil, false
	}

	runner.responses[commandKey("helper", []string{"list", "--state-root", root})] = core.LocalCommandResult{Stdout: mustJSON(t, applevmhelper.ListResponse{})}

	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	claim, ok, err := core.ResolveLeaseClaimForProvider(leaseID, providerName)
	if err != nil {
		t.Fatalf("ResolveLeaseClaimForProvider(%s): %v", leaseID, err)
	}
	if !ok || claim.LeaseID != leaseID {
		t.Fatalf("orphan sweep removed a claim reclaimed during Cleanup: present=%v (want present)\ncleanup output:\n%s", ok, out.String())
	}
	if !strings.Contains(out.String(), "skip claim lease="+leaseID) || !strings.Contains(out.String(), "changed-during-cleanup") {
		t.Fatalf("expected reclaim skip output, got:\n%s", out.String())
	}
	if strings.Contains(out.String(), "remove claim lease="+leaseID) {
		t.Fatalf("orphan sweep removed a reclaimed claim:\n%s", out.String())
	}
}

func TestCleanupRetainsKeyPreparedByConcurrentAcquire(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	var out bytes.Buffer

	oldGOOS, oldGOARCH := hostGOOS, hostGOARCH
	oldMacOSVersion := hostMacOSVersion
	hostGOOS, hostGOARCH = "darwin", "arm64"
	hostMacOSVersion = func() (string, error) { return "26.5", nil }
	t.Cleanup(func() {
		hostGOOS, hostGOARCH = oldGOOS, oldGOARCH
		hostMacOSVersion = oldMacOSVersion
	})
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".state"))
	root := t.TempDir()

	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.AppleVM = core.AppleVMConfig{
		HelperPath:  "/tmp/helper-source",
		Image:       "https://cloud-images.ubuntu.com/releases/noble/release-20260518/ubuntu-24.04-server-cloudimg-arm64.img",
		ImageSHA256: "6a61b967ba4a27dd1966f835a67643073ed55c2860ce3dc1cb0517282e6b8bec",
		User:        "runner",
		WorkRoot:    "/workspace/crabbox",
		CPUs:        4,
		MemoryMiB:   8192,
		DiskGiB:     40,
	}
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: &out, Stderr: &out, Exec: runner}).(*backend)
	b.prepareHelper = func(context.Context, core.Config) (string, error) { return "helper", nil }
	b.stateRoot = func() (string, error) { return root, nil }
	b.waitForSSH = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error { return nil }

	leaseID := "cbx_keyretained67890"
	slug := "key-retained-slug"
	name := core.LeaseProviderName(leaseID, slug)
	writeAgedAppleVMOrphanClaim(t, leaseID, slug, name)

	keyPath, err := core.TestboxKeyPath(leaseID)
	if err != nil {
		t.Fatalf("TestboxKeyPath(%s): %v", leaseID, err)
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		t.Fatalf("mkdir key dir %s: %v", filepath.Dir(keyPath), err)
	}
	if err := os.WriteFile(keyPath, []byte("key-prepared-by-concurrent-acquire"), 0o600); err != nil {
		t.Fatalf("write prepared key %s: %v", keyPath, err)
	}

	runner.responses[commandKey("helper", []string{"list", "--state-root", root})] = core.LocalCommandResult{Stdout: mustJSON(t, applevmhelper.ListResponse{})}

	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	if _, ok, err := core.ResolveLeaseClaimForProvider(leaseID, providerName); err != nil {
		t.Fatalf("ResolveLeaseClaimForProvider(%s): %v", leaseID, err)
	} else if ok {
		t.Fatalf("cleanup should remove orphan claim %s", leaseID)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("prepared key was removed by cleanup: %v", err)
	}
	if !strings.Contains(out.String(), "remove claim lease="+leaseID) {
		t.Fatalf("cleanup did not report claim removal:\n%s", out.String())
	}
}

func TestCleanupDryRunDoesNotPlanReclaimedCandidateRemoval(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	var out bytes.Buffer

	oldGOOS, oldGOARCH := hostGOOS, hostGOARCH
	oldMacOSVersion := hostMacOSVersion
	hostGOOS, hostGOARCH = "darwin", "arm64"
	hostMacOSVersion = func() (string, error) { return "26.5", nil }
	t.Cleanup(func() {
		hostGOOS, hostGOARCH = oldGOOS, oldGOARCH
		hostMacOSVersion = oldMacOSVersion
	})
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".state"))
	root := t.TempDir()

	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.AppleVM = core.AppleVMConfig{
		HelperPath:  "/tmp/helper-source",
		Image:       "https://cloud-images.ubuntu.com/releases/noble/release-20260518/ubuntu-24.04-server-cloudimg-arm64.img",
		ImageSHA256: "6a61b967ba4a27dd1966f835a67643073ed55c2860ce3dc1cb0517282e6b8bec",
		User:        "runner",
		WorkRoot:    "/workspace/crabbox",
		CPUs:        4,
		MemoryMiB:   8192,
		DiskGiB:     40,
	}
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: &out, Stderr: &out, Exec: runner}).(*backend)
	b.prepareHelper = func(context.Context, core.Config) (string, error) { return "helper", nil }
	b.stateRoot = func() (string, error) { return root, nil }
	b.waitForSSH = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error { return nil }

	leaseID := "cbx_dryrun67890"
	slug := "dryrun-orphan-slug"
	name := core.LeaseProviderName(leaseID, slug)
	writeAgedAppleVMOrphanClaim(t, leaseID, slug, name)

	var once sync.Once
	runner.hook = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		if len(req.Args) > 0 && req.Args[0] == "list" {
			once.Do(func() {
				existing, err := core.ReadLeaseClaim(leaseID)
				if err != nil {
					t.Errorf("read existing claim %s before reclaim: %v", leaseID, err)
					return
				}
				server := core.Server{
					CloudID:  name,
					Provider: providerName,
					Name:     name,
					Status:   "ready",
					Labels: map[string]string{
						"crabbox":  "true",
						"provider": providerName,
						"lease":    leaseID,
						"slug":     slug,
						"state":    "ready",
					},
				}
				if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(leaseID, slug, providerName, instanceScope(name), "", existing.RepoRoot, 5*time.Minute, false, server, core.SSHTarget{}); err != nil {
					t.Errorf("concurrent reclaim registration failed: %v", err)
				}
			})
		}
		return core.LocalCommandResult{}, nil, false
	}

	runner.responses[commandKey("helper", []string{"list", "--state-root", root})] = core.LocalCommandResult{Stdout: mustJSON(t, applevmhelper.ListResponse{})}

	if err := b.Cleanup(context.Background(), core.CleanupRequest{DryRun: true}); err != nil {
		t.Fatalf("Cleanup dry-run: %v", err)
	}

	if strings.Contains(out.String(), "would remove claim lease="+leaseID) {
		t.Fatalf("dry-run planned removal of a claim reclaimed during Cleanup:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "skip claim lease="+leaseID) || !strings.Contains(out.String(), "changed-during-cleanup") {
		t.Fatalf("dry-run did not report reclaimed claim as skipped:\n%s", out.String())
	}
	claim, ok, err := core.ResolveLeaseClaimForProvider(leaseID, providerName)
	if err != nil {
		t.Fatalf("ResolveLeaseClaimForProvider(%s): %v", leaseID, err)
	}
	if !ok || claim.LeaseID != leaseID {
		t.Fatalf("orphan sweep removed a claim reclaimed during dry-run: present=%v (want present)\ncleanup output:\n%s", ok, out.String())
	}
}

// TestCleanupDryRunReportsGenuineOrphanRemoval covers the dry-run path for an
// UNCHANGED genuine orphan (no concurrent reclaim): VerifyLeaseClaimUnchanged returns
// nil, so cleanup must PLAN the removal ("would remove ...") rather than skip it, and
// must not actually remove the claim. Complements the reclaim case, which only
// exercises the skip branch of the same dry-run guard.
func TestCleanupDryRunReportsGenuineOrphanRemoval(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	var out bytes.Buffer

	oldGOOS, oldGOARCH := hostGOOS, hostGOARCH
	oldMacOSVersion := hostMacOSVersion
	hostGOOS, hostGOARCH = "darwin", "arm64"
	hostMacOSVersion = func() (string, error) { return "26.5", nil }
	t.Cleanup(func() {
		hostGOOS, hostGOARCH = oldGOOS, oldGOARCH
		hostMacOSVersion = oldMacOSVersion
	})
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".state"))
	root := t.TempDir()

	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.AppleVM = core.AppleVMConfig{
		HelperPath:  "/tmp/helper-source",
		Image:       "https://cloud-images.ubuntu.com/releases/noble/release-20260518/ubuntu-24.04-server-cloudimg-arm64.img",
		ImageSHA256: "6a61b967ba4a27dd1966f835a67643073ed55c2860ce3dc1cb0517282e6b8bec",
		User:        "runner",
		WorkRoot:    "/workspace/crabbox",
		CPUs:        4,
		MemoryMiB:   8192,
		DiskGiB:     40,
	}
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: &out, Stderr: &out, Exec: runner}).(*backend)
	b.prepareHelper = func(context.Context, core.Config) (string, error) { return "helper", nil }
	b.stateRoot = func() (string, error) { return root, nil }
	b.waitForSSH = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error { return nil }

	leaseID := "cbx_dryrun_genuine"
	slug := "dryrun-genuine-slug"
	name := core.LeaseProviderName(leaseID, slug)
	writeAgedAppleVMOrphanClaim(t, leaseID, slug, name)

	runner.responses[commandKey("helper", []string{"list", "--state-root", root})] = core.LocalCommandResult{Stdout: mustJSON(t, applevmhelper.ListResponse{})}

	if err := b.Cleanup(context.Background(), core.CleanupRequest{DryRun: true}); err != nil {
		t.Fatalf("Cleanup dry-run: %v", err)
	}

	if !strings.Contains(out.String(), "would remove claim lease="+leaseID+" reason=missing instance") {
		t.Fatalf("dry-run must plan removal of an unchanged genuine orphan:\n%s", out.String())
	}
	if strings.Contains(out.String(), "skip claim lease="+leaseID) {
		t.Fatalf("dry-run must not skip an unchanged genuine orphan:\n%s", out.String())
	}
	if _, ok, err := core.ResolveLeaseClaimForProvider(leaseID, providerName); err != nil {
		t.Fatalf("ResolveLeaseClaimForProvider(%s): %v", leaseID, err)
	} else if !ok {
		t.Fatalf("dry-run must not actually remove the claim %s:\n%s", leaseID, out.String())
	}
}

// TestCleanupSweepSkipsForeignProviderClaim covers the provider filter: a claim owned
// by a DIFFERENT provider, aged past grace and absent from the apple-vm instance view,
// must be skipped (not swept), so a broken filter cannot delete another provider's live
// claim. Without the isAppleVMProviderName guard the sweep would treat it as a missing
// apple-vm instance and remove it.
func TestCleanupSweepSkipsForeignProviderClaim(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	var out bytes.Buffer

	oldGOOS, oldGOARCH := hostGOOS, hostGOARCH
	oldMacOSVersion := hostMacOSVersion
	hostGOOS, hostGOARCH = "darwin", "arm64"
	hostMacOSVersion = func() (string, error) { return "26.5", nil }
	t.Cleanup(func() {
		hostGOOS, hostGOARCH = oldGOOS, oldGOARCH
		hostMacOSVersion = oldMacOSVersion
	})
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".state"))
	root := t.TempDir()

	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.AppleVM = core.AppleVMConfig{
		HelperPath:  "/tmp/helper-source",
		Image:       "https://cloud-images.ubuntu.com/releases/noble/release-20260518/ubuntu-24.04-server-cloudimg-arm64.img",
		ImageSHA256: "6a61b967ba4a27dd1966f835a67643073ed55c2860ce3dc1cb0517282e6b8bec",
		User:        "runner",
		WorkRoot:    "/workspace/crabbox",
		CPUs:        4,
		MemoryMiB:   8192,
		DiskGiB:     40,
	}
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: &out, Stderr: &out, Exec: runner}).(*backend)
	b.prepareHelper = func(context.Context, core.Config) (string, error) { return "helper", nil }
	b.stateRoot = func() (string, error) { return root, nil }
	b.waitForSSH = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error { return nil }

	const foreignProvider = "aws"
	leaseID := "cbx_foreign_provider"
	slug := "foreign-slug"
	name := core.LeaseProviderName(leaseID, slug)
	writeAgedOrphanClaim(t, leaseID, slug, name, foreignProvider, "")

	runner.responses[commandKey("helper", []string{"list", "--state-root", root})] = core.LocalCommandResult{Stdout: mustJSON(t, applevmhelper.ListResponse{})}

	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	if _, ok, err := core.ResolveLeaseClaimForProvider(leaseID, foreignProvider); err != nil {
		t.Fatalf("ResolveLeaseClaimForProvider(%s, %s): %v", leaseID, foreignProvider, err)
	} else if !ok {
		t.Fatalf("apple-vm cleanup swept a FOREIGN-provider (%s) claim; the orphan sweep must be provider-scoped:\n%s", foreignProvider, out.String())
	}
	if strings.Contains(out.String(), "remove claim lease="+leaseID) {
		t.Fatalf("apple-vm cleanup removed a foreign-provider claim:\n%s", out.String())
	}
}
