//go:build smoke

package applevm

// Instrumented LIVE smoke for the Cleanup orphan-sweep guard, opt-in behind the
// `smoke` build tag and CRABBOX_LIVE=1. Unlike the hermetic tests in
// cleanup_toctou_test.go (which fake the helper via recordingRunner), this builds
// and drives the REAL apple-vm helper binary (cmd/crabbox-apple-vm-helper) that
// Cleanup shells out to. Cleanup's only helper calls are `list` and `delete`, both
// of which read/remove on-disk instance metadata under the state root — no
// Virtualization.framework VM is booted, so the guarded orphan-sweep path runs end
// to end against the real helper subprocess on any Apple-silicon Mac, entitlements
// or base image absent.
//
// The genuine orphan is a claim whose lease has NO instance metadata in the real
// state root, so the REAL `crabbox-apple-vm-helper list` reports {"instances":null}
// — a real "missing instance" by the helper's own view, exactly the condition the
// sweep is designed to detect. Two sub-proofs, both against the real helper:
//
//   Guard  — during Cleanup's real `list` window we deliberately reclaim the lease
//            (rewriting the claim so it no longer matches the pre-list snapshot); the
//            CAS guard must observe the change and DECLINE the removal, so the
//            reclaiming process keeps its live claim.
//   Retain — a genuine orphan that is NOT reclaimed is removed, but its stored SSH
//            key (which Acquire prepares before publishing its claim) is RETAINED.
//
// This is the "instrumented live proof" a timing race cannot reliably produce on its
// own: Acquire publishes its claim only after the multi-second VM creation, far
// outside Cleanup's sub-200ms snapshot->remove window, so two blind concurrent CLI
// processes never interleave into it. The reclaim injection here is a controlled
// rebind at the exact window against real helper state, which the hermetic tests
// reproduce deterministically.
//
// Safety: HOME, XDG_CONFIG_HOME, XDG_STATE_HOME and the state root are all isolated
// temp dirs, so the real Cleanup can only ever see this test's own claim over an
// empty state root — it can never touch host claims, keys, or VMs. A host-wide
// advisory lock serializes concurrent smoke runs.
//
// Run: CRABBOX_LIVE=1 go test -tags smoke -run TestLiveAppleVMCleanup -v ./internal/providers/applevm/

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/openclaw/crabbox/internal/applevmhelper"
	core "github.com/openclaw/crabbox/internal/cli"
)

// liveHelperRunner execs the real crabbox-apple-vm-helper binary for every command,
// and exactly once — while Cleanup is inside its real `list` snapshot window — fires
// duringList to inject the deliberate reclaim.
type liveHelperRunner struct {
	once       sync.Once
	duringList func()
}

func (r *liveHelperRunner) Run(ctx context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	if len(req.Args) > 0 && req.Args[0] == "list" && r.duringList != nil {
		r.once.Do(r.duringList)
	}
	cmd := exec.CommandContext(ctx, req.Name, req.Args...)
	cmd.Env = append(os.Environ(), req.Env...)
	if req.Dir != "" {
		cmd.Dir = req.Dir
	}
	if req.Stdin != nil {
		cmd.Stdin = req.Stdin
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	res := core.LocalCommandResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if exit, ok := err.(*exec.ExitError); ok {
		res.ExitCode = exit.ExitCode()
		return res, nil
	}
	return res, err
}

// buildRealAppleVMHelper builds the actual helper binary (list/delete are state-only,
// no VF) and sanity-checks that it lists an empty state root; skips if the toolchain
// or platform can't produce/run it.
func buildRealAppleVMHelper(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("apple-vm helper requires macOS on Apple silicon")
	}
	bin := filepath.Join(t.TempDir(), applevmhelper.ManagedHelperName)
	build := exec.Command("go", "build", "-o", bin, "github.com/openclaw/crabbox/cmd/crabbox-apple-vm-helper")
	build.Env = os.Environ()
	if out, err := build.CombinedOutput(); err != nil {
		t.Skipf("cannot build apple-vm helper: %v\n%s", err, out)
	}
	// Sanity: the real helper lists an empty state root without VF.
	probeRoot := t.TempDir()
	out, err := exec.Command(bin, "list", "--state-root", probeRoot).Output()
	if err != nil {
		t.Skipf("real apple-vm helper cannot list (runtime unavailable?): %v", err)
	}
	var resp applevmhelper.ListResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("real helper list returned invalid JSON: %v\n%s", err, out)
	}
	return bin
}

// newLiveAppleVMBackend wires a backend that shells out to the real helper binary
// with runner, over an isolated state root, capturing Cleanup output into out.
func newLiveAppleVMBackend(t *testing.T, out *bytes.Buffer, root, helperBin string, runner core.CommandRunner) *backend {
	t.Helper()
	oldGOOS, oldGOARCH := hostGOOS, hostGOARCH
	oldMacOSVersion := hostMacOSVersion
	hostGOOS, hostGOARCH = "darwin", "arm64"
	hostMacOSVersion = func() (string, error) { return "26.5", nil }
	t.Cleanup(func() {
		hostGOOS, hostGOARCH = oldGOOS, oldGOARCH
		hostMacOSVersion = oldMacOSVersion
	})
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir state root %s: %v", root, err)
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.AppleVM = core.AppleVMConfig{
		HelperPath:  helperBin,
		Image:       "https://cloud-images.ubuntu.com/releases/noble/release-20260518/ubuntu-24.04-server-cloudimg-arm64.img",
		ImageSHA256: "6a61b967ba4a27dd1966f835a67643073ed55c2860ce3dc1cb0517282e6b8bec",
		User:        "runner",
		WorkRoot:    "/workspace/crabbox",
		CPUs:        4,
		MemoryMiB:   8192,
		DiskGiB:     40,
	}
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: out, Stderr: out, Exec: runner}).(*backend)
	b.prepareHelper = func(context.Context, core.Config) (string, error) { return helperBin, nil }
	b.stateRoot = func() (string, error) { return root, nil }
	b.waitForSSH = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error { return nil }
	return b
}

func lockLiveAppleVMSmoke(t *testing.T) {
	t.Helper()
	hostLock := flock.New(filepath.Join(os.TempDir(), "crabbox-applevm-cleanup-smoke.lock"), flock.SetPermissions(0o600))
	locked, err := hostLock.TryLock()
	if err != nil {
		t.Fatalf("acquire live cleanup smoke lock: %v", err)
	}
	if !locked {
		t.Skip("another apple-vm cleanup smoke is running")
	}
	t.Cleanup(func() {
		if err := hostLock.Unlock(); err != nil {
			t.Errorf("release live cleanup smoke lock: %v", err)
		}
	})
}

func requireLiveAppleVM(t *testing.T) {
	t.Helper()
	if os.Getenv("CRABBOX_LIVE") != "1" {
		t.Skip("set CRABBOX_LIVE=1 to run the live apple-vm cleanup smoke")
	}
}

// TestLiveAppleVMCleanupGuardPreservesReclaimedClaim proves the TOCTOU guard against
// the real helper: a lease reclaimed during Cleanup's real `list` window keeps its
// claim.
func TestLiveAppleVMCleanupGuardPreservesReclaimedClaim(t *testing.T) {
	requireLiveAppleVM(t)
	lockLiveAppleVMSmoke(t)
	helperBin := buildRealAppleVMHelper(t)

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".state"))
	root := filepath.Join(t.TempDir(), "state")

	leaseID := "cbx_livesmoke_guard"
	slug := "livesmoke-guard"
	name := core.LeaseProviderName(leaseID, slug)
	writeAgedAppleVMOrphanClaim(t, leaseID, slug, name)

	// During Cleanup's real `list`, reclaim the lease so it no longer matches the
	// pre-list snapshot (rewrites content + LastUsedAt=now, exactly as a concurrent
	// Acquire would).
	reclaim := func() {
		existing, err := core.ReadLeaseClaim(leaseID)
		if err != nil {
			t.Errorf("read existing claim %s before reclaim: %v", leaseID, err)
			return
		}
		server := core.Server{
			CloudID: name, Provider: providerName, Name: name, Status: "ready",
			Labels: map[string]string{"crabbox": "true", "provider": providerName, "lease": leaseID, "slug": slug, "state": "ready"},
		}
		if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(
			leaseID, slug, providerName, instanceScope(name), "", existing.RepoRoot, 5*time.Minute, false, server, core.SSHTarget{},
		); err != nil {
			t.Errorf("concurrent reclaim registration failed: %v", err)
		}
	}
	runner := &liveHelperRunner{duringList: reclaim}

	var out bytes.Buffer
	b := newLiveAppleVMBackend(t, &out, root, helperBin, runner)
	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	claim, ok, err := core.ResolveLeaseClaimForProvider(leaseID, providerName)
	if err != nil {
		t.Fatalf("ResolveLeaseClaimForProvider(%s): %v", leaseID, err)
	}
	if !ok || claim.LeaseID != leaseID {
		t.Fatalf("LIVE: Cleanup removed the reclaimed claim against the real apple-vm helper: present=%v (want present)\ncleanup output:\n%s", ok, out.String())
	}
	if !strings.Contains(out.String(), "skip claim lease="+leaseID+" reason=changed-during-cleanup") {
		t.Fatalf("LIVE: expected the guard to log a decline for the reclaimed claim; got:\n%s", out.String())
	}
	if strings.Contains(out.String(), "remove claim lease="+leaseID) {
		t.Fatalf("LIVE: Cleanup removed the reclaimed lease instead of declining:\n%s", out.String())
	}
	t.Logf("LIVE PROOF (real crabbox-apple-vm-helper): guard declined the reclaimed claim during Cleanup's real `list` window:\n%s", strings.TrimSpace(out.String()))
}

// TestLiveAppleVMCleanupRetainsKeyOnGenuineOrphanRemoval proves the availability fix
// against the real helper: a genuine orphan (no live instance, not reclaimed) is
// removed, but its stored SSH key is retained.
func TestLiveAppleVMCleanupRetainsKeyOnGenuineOrphanRemoval(t *testing.T) {
	requireLiveAppleVM(t)
	lockLiveAppleVMSmoke(t)
	helperBin := buildRealAppleVMHelper(t)

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".state"))
	root := filepath.Join(t.TempDir(), "state")

	leaseID := "cbx_livesmoke_retain"
	slug := "livesmoke-retain"
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

	runner := &liveHelperRunner{} // no reclaim: the orphan is genuinely removed
	var out bytes.Buffer
	b := newLiveAppleVMBackend(t, &out, root, helperBin, runner)
	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	if _, ok, err := core.ResolveLeaseClaimForProvider(leaseID, providerName); err != nil {
		t.Fatalf("ResolveLeaseClaimForProvider(%s): %v", leaseID, err)
	} else if ok {
		t.Fatalf("LIVE: cleanup should remove the genuine orphan claim %s\n%s", leaseID, out.String())
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("LIVE: prepared key was removed by cleanup against the real helper: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "remove claim lease="+leaseID) {
		t.Fatalf("LIVE: expected a genuine orphan removal to be logged; got:\n%s", out.String())
	}
	t.Logf("LIVE PROOF (real crabbox-apple-vm-helper): genuine orphan removed, prepared key retained:\n%s", strings.TrimSpace(out.String()))
}
