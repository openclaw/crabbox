//go:build smoke

package applecontainer

// Instrumented LIVE smoke for the Cleanup orphan-sweep guard, opt-in behind the
// `smoke` build tag and CRABBOX_LIVE=1. Unlike the hermetic tests in
// cleanup_toctou_test.go (which fake `container ls`), this drives the REAL Apple
// `container` runtime end to end: it creates a real container for a lease,
// registers the lease claim, deletes the real container so the claim is a genuine
// missing-container orphan by the real runtime's own view, then runs the real
// Cleanup while deliberately reclaiming the lease during Cleanup's real
// `container ls` snapshot window. It asserts the guard observes the reclaim and
// declines the removal, so the reclaiming process keeps its live claim.
//
// This is the "instrumented live proof" a timing race cannot reliably produce on
// its own: the apple-container Acquire publishes its claim only after the ~12s
// real-container creation, far outside Cleanup's sub-200ms snapshot->remove
// window, so two blind concurrent CLI processes never interleave into it. The
// injection here is deliberate (a controlled rebind at the exact window) against
// real runtime state, which is what the hermetic tests reproduce deterministically.
//
// Safety: the smoke SKIPS if any other crabbox-labeled container is present, so a
// real Cleanup pass can never sweep an unrelated container; the claim store is an
// isolated XDG_STATE_HOME temp dir, so only this test's claim is ever visible.
//
// Run: CRABBOX_LIVE=1 go test -tags smoke -run TestLiveAppleContainerCleanupGuardPreservesReclaimedClaim -v ./internal/providers/applecontainer/

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/flock"
	core "github.com/openclaw/crabbox/internal/cli"
)

// liveContainerRunner execs the real Apple `container` CLI for every command, and
// exactly once — while Cleanup is inside its real `container ls` snapshot window —
// fires duringList to inject the deliberate reclaim.
type liveContainerRunner struct {
	once       sync.Once
	duringList func()
}

func (r *liveContainerRunner) Run(ctx context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	if len(req.Args) > 0 && req.Args[0] == "ls" && r.duringList != nil {
		r.once.Do(r.duringList)
	}
	cmd := exec.CommandContext(ctx, req.Name, req.Args...)
	cmd.Env = append(os.Environ(), req.Env...)
	if req.Dir != "" {
		cmd.Dir = req.Dir
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

func TestLiveAppleContainerCleanupGuardPreservesReclaimedClaim(t *testing.T) {
	if os.Getenv("CRABBOX_LIVE") != "1" {
		t.Skip("set CRABBOX_LIVE=1 to run the live apple-container cleanup smoke")
	}
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("apple-container Cleanup requires macOS on Apple silicon")
	}
	cli := os.Getenv("CRABBOX_APPLE_CONTAINER_CLI")
	if cli == "" {
		cli = "container"
	}
	if _, err := exec.LookPath(cli); err != nil {
		t.Skipf("Apple container CLI %q not installed", cli)
	}

	realLs := func() string {
		out, err := exec.Command(cli, "ls", "--all", "--format", "json").Output()
		if err != nil {
			t.Fatalf("real %s ls: %v", cli, err)
		}
		return string(out)
	}

	const (
		lease     = "cbx_livesmoke_guard"
		slug      = "livesmoke-guard"
		name      = "crabbox-livesmoke-guard"
		smokeMark = "applecontainer-cleanup-guard"
	)

	// Serialize the destructive live smoke across processes. Advisory locks are
	// released by the kernel after a crash, while the fixed name and ownership
	// label below let the next locked run safely remove only our stale container.
	hostLock := flock.New(filepath.Join(os.TempDir(), "crabbox-applecontainer-cleanup-smoke.lock"), flock.SetPermissions(0o600))
	locked, err := hostLock.TryLock()
	if err != nil {
		t.Fatalf("acquire live cleanup smoke lock: %v", err)
	}
	if !locked {
		t.Skip("another apple-container cleanup smoke is running")
	}
	t.Cleanup(func() {
		if err := hostLock.Unlock(); err != nil {
			t.Errorf("release live cleanup smoke lock: %v", err)
		}
	})

	// Safety precondition: refuse to run a real Cleanup pass while any other
	// crabbox-labeled container exists, so the sweep can never touch one. A
	// crashed prior run is the sole exception, proven by both fixed name and a
	// dedicated ownership label while holding the host-wide lock.
	containers, err := decodeInspect([]byte(realLs()))
	if err != nil {
		t.Fatalf("decode real %s ls: %v", cli, err)
	}
	staleSmoke := false
	for _, container := range containers {
		labels := container.labels()
		if labels["crabbox"] != "true" {
			continue
		}
		if container.id() == name && labels["crabbox-smoke"] == smokeMark {
			staleSmoke = true
			continue
		}
		t.Skip("other crabbox containers present; skipping live cleanup smoke to avoid touching them")
	}
	if staleSmoke {
		if out, err := exec.Command(cli, "delete", "--force", name).CombinedOutput(); err != nil {
			t.Fatalf("delete stale live-smoke container: %v\n%s", err, out)
		}
	}

	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repoRoot := t.TempDir()

	image := os.Getenv("CRABBOX_APPLE_CONTAINER_IMAGE")
	if image == "" {
		image = "ubuntu:26.04"
	}

	// 1) Create a REAL container for the lease.
	create := exec.Command(cli, "run", "-d", "--name", name,
		"--label", "crabbox=true", "--label", "provider="+providerName,
		"--label", "lease="+lease, "--label", "slug="+slug, "--label", "state=ready",
		"--label", "crabbox-smoke="+smokeMark,
		image, "sleep", "infinity")
	if out, err := create.CombinedOutput(); err != nil {
		t.Skipf("cannot create live container (runtime unavailable?): %v\n%s", err, out)
	}
	t.Cleanup(func() { _, _ = exec.Command(cli, "delete", "--force", name).CombinedOutput() })

	// 2) Register the lease claim for the real container (the entrypoint Acquire uses).
	server := core.Server{
		CloudID: name, Provider: providerName, Name: name, Status: "ready",
		Labels: map[string]string{"crabbox": "true", "provider": providerName, "lease": lease, "slug": slug, "state": "ready"},
	}
	if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(
		lease, slug, providerName, "", "", repoRoot, 30*time.Minute, false, server, core.SSHTarget{},
	); err != nil {
		t.Fatalf("register lease claim: %v", err)
	}
	if !strings.Contains(realLs(), name) {
		t.Fatalf("real runtime does not show the container we just created (%s)", name)
	}

	// 3) Delete the REAL container out from under the claim -> genuine missing-container orphan.
	if out, err := exec.Command(cli, "delete", "--force", name).CombinedOutput(); err != nil {
		t.Fatalf("delete real container: %v\n%s", err, out)
	}
	if strings.Contains(realLs(), name) {
		t.Fatalf("real runtime still shows %s after delete", name)
	}

	// 4) Run the real Cleanup; during its real `container ls`, reclaim the lease
	//    (rewrite the claim so it no longer matches the pre-list snapshot).
	reclaim := func() {
		rebound := server
		rebound.Status = "reclaimed"
		rebound.Labels = map[string]string{"crabbox": "true", "provider": providerName, "lease": lease, "slug": slug, "state": "reclaimed"}
		if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(
			lease, slug, providerName, "", "", repoRoot, 30*time.Minute, false, rebound, core.SSHTarget{},
		); err != nil {
			t.Errorf("concurrent reclaim registration failed: %v", err)
		}
	}
	runner := &liveContainerRunner{duringList: reclaim}

	var out bytes.Buffer
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.AppleContainer = core.AppleContainerConfig{CLIPath: cli, Image: image, User: "crabbox", WorkRoot: "/work/crabbox"}
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: &out, Stderr: &out, Exec: runner}).(*backend)

	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	// 5) The guard must have observed the reclaim and declined the removal, so the
	//    reclaiming process keeps its live claim — proven against the real runtime.
	claim, ok, err := core.ResolveLeaseClaimForProvider(lease, providerName)
	if err != nil {
		t.Fatalf("ResolveLeaseClaimForProvider(%s): %v", lease, err)
	}
	if !ok || claim.LeaseID != lease {
		t.Fatalf("LIVE: Cleanup removed the reclaimed claim against the real Apple runtime: present=%v (want present)\ncleanup output:\n%s", ok, out.String())
	}
	if !strings.Contains(out.String(), "skip claim lease="+lease+" reason=changed-during-cleanup") {
		t.Fatalf("LIVE: expected the guard to log a decline for the reclaimed claim; got:\n%s", out.String())
	}
	if strings.Contains(out.String(), "remove claim lease="+lease) {
		t.Fatalf("LIVE: Cleanup removed the reclaimed lease instead of declining:\n%s", out.String())
	}
	t.Logf("LIVE PROOF (real Apple container runtime): guard declined the reclaimed claim during Cleanup's real `container ls` window:\n%s", strings.TrimSpace(out.String()))
}
