package tart

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
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

type cleanupWaitContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *cleanupWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func TestCleanupCancellationWhileClaimLocked(t *testing.T) {
	for _, mode := range []string{"live", "missing", "missing-with-later"} {
		t.Run(mode, func(t *testing.T) {
			b, runner, claim := cleanupFixture(t)
			if mode != "live" {
				if err := os.RemoveAll(filepath.Join(os.Getenv("TART_HOME"), "vms", cleanupVM)); err != nil {
					t.Fatal(err)
				}
				runner.responses["list"] = core.LocalCommandResult{Stdout: "[]"}
			}
			const laterLease = "cbx_zzlater"
			if mode == "missing-with-later" {
				claimTartLease(t, t.TempDir(), laterLease, "crabbox-later", "stopped")
				if err := os.RemoveAll(filepath.Join(os.Getenv("TART_HOME"), "vms", "crabbox-later")); err != nil {
					t.Fatal(err)
				}
			}
			key, err := core.PrepareStoredTestboxKeyPath(claim.LeaseID)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(key, []byte("synthetic SSH key"), 0o600); err != nil {
				t.Fatal(err)
			}
			held, release, ownerDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var ownerErr error
			var unlock sync.Once
			go func() {
				defer close(ownerDone)
				ownerErr = core.WithDurableLeaseClaimLock(claim.LeaseID, func(*core.LeaseClaim, bool, func() error) error {
					close(held)
					<-release
					return nil
				})
			}()
			defer func() { unlock.Do(func() { close(release) }); <-ownerDone }()
			select {
			case <-held:
			case <-ownerDone:
				t.Fatalf("claim owner failed: %v", ownerErr)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			observed := &cleanupWaitContext{Context: ctx, waiting: make(chan struct{})}
			done := make(chan struct{})
			var cleanupErr error
			go func() {
				defer close(done)
				cleanupErr = b.Cleanup(observed, core.CleanupRequest{})
			}()
			defer func() { unlock.Do(func() { close(release) }); <-done }()
			waitObserved := false
			select {
			case <-observed.waiting:
				waitObserved = true
			case <-time.After(time.Second):
			}
			cancel()
			returnedWhileHeld := false
			select {
			case <-done:
				returnedWhileHeld = true
			case <-time.After(time.Second):
			}
			unlock.Do(func() { close(release) })
			<-ownerDone
			<-done
			if ownerErr != nil || !waitObserved || !returnedWhileHeld || !errors.Is(cleanupErr, context.Canceled) {
				t.Fatalf("wait observed=%v returned while held=%v owner=%v cleanup=%v", waitObserved, returnedWhileHeld, ownerErr, cleanupErr)
			}
			if len(runner.calls) != 1 || runner.calls[0].Args[0] != "list" {
				t.Fatalf("commands entered after canceled lock admission: %v", runner.calls)
			}
			current, exists, err := core.ReadLeaseClaimWithPresence(claim.LeaseID)
			if err != nil || !exists || !reflect.DeepEqual(current, claim) {
				t.Fatalf("canceled cleanup changed claim: exists=%v err=%v", exists, err)
			}
			if _, err := os.Stat(key); err != nil {
				t.Fatalf("canceled cleanup removed SSH key: %v", err)
			}
			if mode == "missing-with-later" {
				assertTartClaim(t, laterLease, "stopped")
			}
		})
	}
}

func (r *cleanupRaceRunner) Run(_ context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	if len(req.Args) >= 2 && req.Args[0] == "delete" && r.onStop != nil {
		r.onceStop.Do(r.onStop)
	}
	return r.responses[commandKey(req.Args)], nil
}

func TestCleanupPreservesClaimCreatedAfterInstanceSnapshot(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("TART_HOME", t.TempDir())
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
	t.Setenv("TART_HOME", t.TempDir())
	repoRoot := t.TempDir()
	const (
		staleVM     = "crabbox-stale-old2"
		orphanVM    = "crabbox-orphan-cand2"
		orphanLease = "cbx_orphancand67890"
	)

	claimTartLease(t, repoRoot, orphanLease, orphanVM, "idle")
	root, err := tartStorageRoot()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, "vms", orphanVM)); err != nil {
		t.Fatal(err)
	}
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
	claimTartLease(t, t.TempDir(), "cbx_trigger", staleVM, "stopped")
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
	root, err := tartStorageRoot()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "vms", instance), 0o700); err != nil {
		t.Fatal(err)
	}
	identity, err := readTartVMIdentity(root, instance)
	if os.IsNotExist(err) {
		_, identity, err = createTartVMIdentity(instance)
	}
	if err != nil {
		t.Fatal(err)
	}
	server := core.Server{
		CloudID:     instance,
		ImmutableID: identity,
		Provider:    providerName,
		Name:        instance,
		Status:      state,
		Labels: map[string]string{
			"crabbox":      "true",
			"provider":     providerName,
			"instance":     instance,
			"lease":        leaseID,
			"slug":         "race-slug",
			"state":        state,
			"tart_storage": root,
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
