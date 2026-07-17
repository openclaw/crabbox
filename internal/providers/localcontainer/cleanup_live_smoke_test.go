//go:build smoke

package localcontainer

// Instrumented LIVE smoke for the Cleanup orphan-sweep, opt-in behind the `smoke`
// build tag and CRABBOX_LIVE=1. Unlike the hermetic tests in cleanup_toctou_test.go
// (which fake `docker ps`), this drives a REAL Docker-compatible runtime end to end
// and proves both facets of the fix against real runtime state:
//
//  (A) key-retention (the availability P1): a concurrent Acquire prepares a lease's
//      stored key before publishing its replacement claim, so Cleanup must remove a
//      genuine orphan claim but RETAIN the key, or the live container is left
//      unreachable over SSH; and
//  (B) guard-decline: a reclaim injected during Cleanup's real `docker ps` snapshot
//      window makes the guard log `skip claim ... changed-during-cleanup` and the
//      live lease's claim survives.
//
// Safety/isolation:
//   - HOME, XDG_CONFIG_HOME and XDG_STATE_HOME are redirected to temp dirs so claims
//     and the stored testbox key (core.TestboxKeyPath uses os.UserConfigDir, which is
//     HOME-based on macOS) never touch the developer's real config; DOCKER_CONFIG is
//     pinned to the real ~/.docker first so the docker CLI still finds its context.
//   - test containers carry a dedicated `crabbox-smoke-test=1` label; preflight SKIPS
//     (never force-removes) if any real crabbox container is present, and only removes
//     leftovers carrying our own marker.
//
// Run: CRABBOX_LIVE=1 go test -tags smoke -run TestLiveLocalContainer -v ./internal/providers/localcontainer/

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const smokeTestLabel = "crabbox-smoke-test=1"

// liveDockerRunner execs the real Docker CLI for every command, and exactly once —
// while Cleanup is inside its real `docker ps` snapshot window — fires duringList to
// inject the deliberate reclaim.
type liveDockerRunner struct {
	runtime    string
	once       sync.Once
	duringList func()
}

func (r *liveDockerRunner) Run(ctx context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	if len(req.Args) > 0 && req.Args[0] == "ps" && r.duringList != nil {
		r.once.Do(r.duringList)
	}
	bin := req.Name
	if bin == "" {
		bin = r.runtime
	}
	cmd := exec.CommandContext(ctx, bin, req.Args...)
	cmd.Env = append(os.Environ(), req.Env...)
	if req.Dir != "" {
		cmd.Dir = req.Dir
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	res := core.LocalCommandResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if exit, ok := err.(*exec.ExitError); ok {
		// Surface the exit code but do NOT swallow the failure: a smoke that treats a
		// failed docker command as success would report a false pass. Propagate with
		// command context so a broken runtime fails the test loudly.
		res.ExitCode = exit.ExitCode()
		return res, fmt.Errorf("%s %s: exit %d: %s", bin, strings.Join(req.Args, " "), exit.ExitCode(), strings.TrimSpace(stderr.String()))
	}
	return res, err
}

// liveDockerSetup checks the opt-in gate, isolates config/state to temp dirs (while
// keeping docker's real context reachable), and returns the runtime binary.
func liveDockerSetup(t *testing.T) string {
	t.Helper()
	if os.Getenv("CRABBOX_LIVE") != "1" {
		t.Skip("set CRABBOX_LIVE=1 to run the live local-container cleanup smoke")
	}
	// Pin docker's config (context/auth) to the real ~/.docker BEFORE relocating HOME,
	// so os.UserConfigDir (the testbox key path) lands in the temp HOME but the docker
	// CLI still resolves its context (e.g. colima).
	if os.Getenv("DOCKER_CONFIG") == "" {
		if realHome, err := os.UserHomeDir(); err == nil && realHome != "" {
			t.Setenv("DOCKER_CONFIG", filepath.Join(realHome, ".docker"))
		}
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	runtime := os.Getenv("CRABBOX_LOCAL_CONTAINER_RUNTIME")
	if runtime == "" {
		runtime = "docker"
	}
	if _, err := exec.LookPath(runtime); err != nil {
		t.Skipf("%s CLI not installed", runtime)
	}
	if out, err := exec.Command(runtime, "ps").CombinedOutput(); err != nil {
		t.Skipf("%s daemon not reachable: %v\n%s", runtime, err, out)
	}
	return runtime
}

// liveDockerPreflight refuses to run while any real crabbox container is present (so
// the destructive-cleanup smoke can never interact with a real lease) and removes only
// leftovers carrying our own smoke-test marker.
func liveDockerPreflight(t *testing.T, runtime string) {
	t.Helper()
	out, err := exec.Command(runtime, "ps", "-a", "--filter", "label=crabbox=true", "--format", "{{.Names}}\t{{.Label \"crabbox-smoke-test\"}}").CombinedOutput()
	if err != nil {
		t.Skipf("cannot list %s containers: %v\n%s", runtime, err, out)
	}
	var real, mine []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.SplitN(line, "\t", 2)
		name := strings.TrimSpace(fields[0])
		marker := ""
		if len(fields) > 1 {
			marker = strings.TrimSpace(fields[1])
		}
		if marker == "1" {
			mine = append(mine, name)
		} else {
			real = append(real, name)
		}
	}
	if len(real) > 0 {
		t.Skipf("real crabbox containers present (%v); skipping the live cleanup smoke so it never interacts with a real lease", real)
	}
	for _, name := range mine {
		_, _ = exec.Command(runtime, "rm", "-f", name).CombinedOutput()
	}
}

func liveDockerConfig(runtime string) core.Config {
	image := os.Getenv("CRABBOX_LOCAL_CONTAINER_IMAGE")
	if image == "" {
		image = "ubuntu:24.04"
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.LocalContainer = core.LocalContainerConfig{
		Runtime:  runtime,
		Image:    image,
		User:     "runner",
		WorkRoot: "/workspace/crabbox",
		CPUs:     2,
		Memory:   "2g",
		Network:  "bridge",
	}
	return cfg
}

// TestLiveLocalContainerCleanupRetainsKeyForConcurrentAcquire — facet (A). Against
// the real Docker runtime, a genuine missing-container orphan claim is removed while
// a key prepared by a concurrent Acquire is RETAINED.
func TestLiveLocalContainerCleanupRetainsKeyForConcurrentAcquire(t *testing.T) {
	runtime := liveDockerSetup(t)
	liveDockerPreflight(t, runtime)
	const (
		lease = "cbx_livelckey01"
		slug  = "livelckey"
		name  = "crabbox-livelckey"
	)
	repoRoot := t.TempDir()
	cfg := liveDockerConfig(runtime)
	_, _ = exec.Command(runtime, "pull", cfg.LocalContainer.Image).CombinedOutput()

	// 1) Create a REAL container for the lease (tagged with our smoke-test marker).
	create := exec.Command(runtime, "run", "-d", "--name", name,
		"--label", "crabbox=true", "--label", smokeTestLabel, "--label", "provider="+providerName,
		"--label", "lease="+lease, "--label", "slug="+slug, "--label", "state=ready",
		cfg.LocalContainer.Image, "sleep", "infinity")
	if out, err := create.CombinedOutput(); err != nil {
		t.Skipf("cannot create live container (runtime unavailable?): %v\n%s", err, out)
	}
	t.Cleanup(func() { _, _ = exec.Command(runtime, "rm", "-f", name).CombinedOutput() })

	runner := &liveDockerRunner{runtime: runtime}
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: &bytes.Buffer{}, Stderr: os.Stderr, Exec: runner}).(*backend)
	scope := b.claimScope(context.Background())
	if scope == "" {
		t.Fatalf("test setup: claimScope resolved empty against real docker context")
	}

	// 2) Register the lease claim (the entrypoint Acquire uses).
	server := core.Server{
		CloudID: name, Provider: providerName, Name: name, Status: "ready",
		Labels: map[string]string{"crabbox": "true", "provider": providerName, "lease": lease, "slug": slug, "state": "ready"},
	}
	if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(
		lease, slug, providerName, scope, "", repoRoot, 30*time.Minute, false, server, core.SSHTarget{},
	); err != nil {
		t.Fatalf("register lease claim: %v", err)
	}

	// 3) A concurrent Acquire prepared this lease's key before publishing its claim.
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

	// 4) Delete the REAL container -> genuine missing-container orphan by the runtime's own view.
	if out, err := exec.Command(runtime, "rm", "-f", name).CombinedOutput(); err != nil {
		t.Fatalf("delete real container: %v\n%s", err, out)
	}

	// 5) Run the real Cleanup (real docker ps shows it gone).
	var out bytes.Buffer
	b = newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: &out, Stderr: &out, Exec: &liveDockerRunner{runtime: runtime}}).(*backend)
	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	// 6) The genuine orphan claim is removed...
	if _, ok, err := core.ResolveLeaseClaimForProvider(lease, providerName); err != nil {
		t.Fatalf("ResolveLeaseClaimForProvider: %v", err)
	} else if ok {
		t.Fatalf("LIVE: expected the genuine orphan claim to be removed, but it survived\ncleanup output:\n%s", out.String())
	}
	// ...but the key prepared by the concurrent Acquire MUST survive.
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("LIVE: cleanup deleted a key prepared by a concurrent Acquire (availability regression): %v\ncleanup output:\n%s", err, out.String())
	}
	t.Logf("LIVE PROOF (real Docker runtime): genuine orphan claim removed, key RETAINED for the concurrent Acquire:\n%s", strings.TrimSpace(out.String()))
}

// TestLiveLocalContainerCleanupGuardDeclinesReclaim — facet (B). Against the real
// Docker runtime, a reclaim injected during Cleanup's real `docker ps` window makes
// the guard decline; the live lease's claim survives.
func TestLiveLocalContainerCleanupGuardDeclinesReclaim(t *testing.T) {
	runtime := liveDockerSetup(t)
	liveDockerPreflight(t, runtime)
	const (
		lease = "cbx_livelcguard01"
		slug  = "livelcguard"
	)
	repoRoot := t.TempDir()
	cfg := liveDockerConfig(runtime)

	// Resolve the real docker scope up front.
	scopeBackend := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: &bytes.Buffer{}, Stderr: os.Stderr, Exec: &liveDockerRunner{runtime: runtime}}).(*backend)
	scope := scopeBackend.claimScope(context.Background())
	if scope == "" {
		t.Fatalf("test setup: claimScope resolved empty against real docker context")
	}

	origServer := core.Server{
		CloudID: "crabbox-livelcguard", Provider: providerName, Name: "crabbox-livelcguard", Status: "idle",
		Labels: map[string]string{"crabbox": "true", "provider": providerName, "lease": lease, "slug": slug, "state": "idle"},
	}
	// Registered BEFORE Cleanup, so it is in the pre-container orphan snapshot.
	if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(
		lease, slug, providerName, scope, "", repoRoot, 30*time.Minute, false, origServer, core.SSHTarget{},
	); err != nil {
		t.Fatalf("setup orphan-candidate claim: %v", err)
	}

	// During Cleanup's real `docker ps`, a concurrent process reclaims the same
	// lease, rewriting the claim so it no longer matches the pre-list snapshot.
	reclaim := func() {
		rebound := origServer
		rebound.Status = "ready"
		rebound.Labels = map[string]string{"crabbox": "true", "provider": providerName, "lease": lease, "slug": slug, "state": "ready"}
		if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(
			lease, slug, providerName, scope, "", repoRoot, 30*time.Minute, false, rebound, core.SSHTarget{},
		); err != nil {
			t.Errorf("concurrent reclaim registration failed: %v", err)
		}
	}

	var out bytes.Buffer
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: &out, Stderr: &out, Exec: &liveDockerRunner{runtime: runtime, duringList: reclaim}}).(*backend)
	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	claim, ok, err := core.ResolveLeaseClaimForProvider(lease, providerName)
	if err != nil {
		t.Fatalf("ResolveLeaseClaimForProvider: %v", err)
	}
	if !ok || claim.LeaseID != lease {
		t.Fatalf("LIVE: guard failed — Cleanup removed the reclaimed live claim against real docker: present=%v\ncleanup output:\n%s", ok, out.String())
	}
	if !strings.Contains(out.String(), "skip claim lease="+lease+" ") || !strings.Contains(out.String(), "changed-during-cleanup") {
		t.Fatalf("LIVE: expected the guard to log a decline for the reclaimed claim; got:\n%s", out.String())
	}
	t.Logf("LIVE PROOF (real Docker runtime): guard declined the reclaimed claim during Cleanup's real docker ps window:\n%s", strings.TrimSpace(out.String()))
}
