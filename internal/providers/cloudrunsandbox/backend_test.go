package cloudrunsandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type cloudRunSandboxFixedClock struct{ now time.Time }

func (c cloudRunSandboxFixedClock) Now() time.Time { return c.now }

func TestCloudRunSandboxCreateTimeoutRetainsRecoveryClaim(t *testing.T) {
	isolateLeaseHome(t)
	var sandboxID string
	createErr := context.DeadlineExceeded
	transport := &fakeTransport{
		mode: "remote",
		onCreate: func(id string) error {
			sandboxID = id
			return createErr
		},
	}
	b := NewBackend(Provider{}.Spec(), Config{
		CloudRunSandbox: CloudRunSandboxConfig{CLIPath: defaultCLIPath, Workdir: defaultWorkdir},
		IdleTimeout:     time.Minute,
	}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)

	_, _, _, _, err := b.createSandbox(context.Background(), transport, Repo{Root: t.TempDir()}, false, "timeout-recovery")
	if !errors.Is(err, createErr) || !strings.Contains(err.Error(), "recovery claim retained") {
		t.Fatalf("create error=%v, want indeterminate recovery", err)
	}
	if sandboxID == "" {
		t.Fatal("create did not receive a sandbox id")
	}
	claim, readErr := readLeaseClaim(leasePrefix + sandboxID)
	if readErr != nil {
		t.Fatalf("read recovery claim: %v", readErr)
	}
	if claim.LeaseID != leasePrefix+sandboxID || claim.Provider != providerName || claim.Slug != "timeout-recovery" {
		t.Fatalf("recovery claim=%#v", claim)
	}
	if claim.Labels[claimStateLabel] != "recovery" {
		t.Fatalf("recovery state=%q", claim.Labels[claimStateLabel])
	}
	status, statusErr := b.Status(context.Background(), StatusRequest{ID: claim.LeaseID})
	if statusErr != nil || status.Ready || status.State != "recovery" {
		t.Fatalf("recovery status=%#v err=%v", status, statusErr)
	}
	leases, listErr := b.List(context.Background(), ListRequest{})
	if listErr != nil || len(leases) != 1 || leases[0].Status != "recovery" {
		t.Fatalf("recovery list=%#v err=%v", leases, listErr)
	}
	previousTransport := newTransport
	newTransport = func(Config, Runtime) (sandboxTransport, error) { return transport, nil }
	t.Cleanup(func() { newTransport = previousTransport })
	if _, runErr := b.Run(context.Background(), RunRequest{ID: claim.LeaseID, Repo: Repo{Root: claim.RepoRoot}, Command: []string{"true"}, NoSync: true}); runErr == nil || !strings.Contains(runErr.Error(), "not ready") {
		t.Fatalf("recovery run error=%v", runErr)
	}
}

func TestCloudRunSandboxStatusProbesProviderLiveness(t *testing.T) {
	isolateLeaseHome(t)
	transport := &fakeTransport{mode: "direct", onProbe: func(string, string) error {
		return errSandboxNotFound
	}}
	previousTransport := newTransport
	newTransport = func(Config, Runtime) (sandboxTransport, error) { return transport, nil }
	t.Cleanup(func() { newTransport = previousTransport })
	b := NewBackend(Provider{}.Spec(), Config{
		CloudRunSandbox: CloudRunSandboxConfig{CLIPath: defaultCLIPath, Workdir: defaultWorkdir},
		IdleTimeout:     time.Minute,
	}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)
	scope, err := b.claimScope()
	if err != nil {
		t.Fatal(err)
	}
	const leaseID = leasePrefix + "crabbox-missing"
	if err := claimTestCloudRunSandboxLease(leaseID, "missing", scope, t.TempDir(), time.Minute); err != nil {
		t.Fatal(err)
	}
	status, err := b.Status(context.Background(), StatusRequest{ID: leaseID})
	if err != nil || status.Ready || status.State != "missing" {
		t.Fatalf("status=%#v err=%v", status, err)
	}
	leases, err := b.List(context.Background(), ListRequest{})
	if err != nil || len(leases) != 1 || leases[0].Status != "missing" {
		t.Fatalf("leases=%#v err=%v", leases, err)
	}
}

func TestCloudRunSandboxRunPreservesCancellation(t *testing.T) {
	isolateLeaseHome(t)
	transport := &fakeTransport{
		mode: "direct",
		onExec: func(_ string, command string) (int, string, string, error) {
			if strings.Contains(command, "cancel-me") {
				return 130, "", "", context.Canceled
			}
			return 0, "", "", nil
		},
	}
	previousTransport := newTransport
	newTransport = func(Config, Runtime) (sandboxTransport, error) { return transport, nil }
	t.Cleanup(func() { newTransport = previousTransport })
	b := NewBackend(Provider{}.Spec(), Config{
		CloudRunSandbox: CloudRunSandboxConfig{CLIPath: defaultCLIPath, Workdir: defaultWorkdir},
		IdleTimeout:     time.Minute,
	}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)
	_, err := b.Run(context.Background(), RunRequest{
		Repo:    Repo{Root: t.TempDir()},
		Command: []string{"cancel-me"},
		NoSync:  true,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("run err=%v", err)
	}
	var exitErr ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 130 {
		t.Fatalf("exit err=%#v err=%v", exitErr, err)
	}
}

func TestCloudRunSandboxCreateConflictDropsProvisionalClaim(t *testing.T) {
	isolateLeaseHome(t)
	var sandboxID string
	transport := &fakeTransport{mode: "remote", onCreate: func(id string) error {
		sandboxID = id
		return errSandboxAlreadyExists
	}}
	b := NewBackend(Provider{}.Spec(), Config{
		CloudRunSandbox: CloudRunSandboxConfig{CLIPath: defaultCLIPath, Workdir: defaultWorkdir},
		IdleTimeout:     time.Minute,
	}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)

	_, _, _, _, err := b.createSandbox(context.Background(), transport, Repo{Root: t.TempDir()}, false, "conflict")
	if !errors.Is(err, errSandboxAlreadyExists) || !strings.Contains(err.Error(), "without taking ownership") {
		t.Fatalf("create error=%v", err)
	}
	if _, exists, readErr := readLeaseClaimWithPresence(leasePrefix + sandboxID); readErr != nil || exists {
		t.Fatal("definitive conflict retained a destructive ownership claim")
	}
}

func TestCloudRunSandboxCreateConflictRemovalPrecedesWaitingStop(t *testing.T) {
	isolateLeaseHome(t)
	createStarted := make(chan string, 1)
	allowConflict := make(chan struct{})
	destroyed := false
	transport := &fakeTransport{
		mode: "remote",
		onCreate: func(id string) error {
			createStarted <- id
			<-allowConflict
			return errSandboxAlreadyExists
		},
		onDestroy: func(string) error {
			destroyed = true
			return nil
		},
	}
	previousTransport := newTransport
	newTransport = func(Config, Runtime) (sandboxTransport, error) { return transport, nil }
	t.Cleanup(func() { newTransport = previousTransport })
	b := NewBackend(Provider{}.Spec(), Config{
		CloudRunSandbox: CloudRunSandboxConfig{CLIPath: defaultCLIPath, Workdir: defaultWorkdir},
		IdleTimeout:     time.Minute,
	}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)
	createDone := make(chan error, 1)
	go func() {
		_, _, _, _, err := b.createSandbox(context.Background(), transport, Repo{Root: t.TempDir()}, false, "conflict-race")
		createDone <- err
	}()
	sandboxID := <-createStarted
	stopDone := make(chan error, 1)
	go func() { stopDone <- b.Stop(context.Background(), StopRequest{ID: leasePrefix + sandboxID}) }()
	select {
	case err := <-stopDone:
		t.Fatalf("stop bypassed create claim lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(allowConflict)
	if err := <-createDone; !errors.Is(err, errSandboxAlreadyExists) {
		t.Fatalf("create err=%v", err)
	}
	if err := <-stopDone; err == nil {
		t.Fatal("waiting stop acquired ownership of pre-existing sandbox")
	}
	if destroyed {
		t.Fatal("waiting stop destroyed a pre-existing sandbox")
	}
}

func TestCloudRunSandboxCleanupSerializesCreateInFlight(t *testing.T) {
	isolateLeaseHome(t)
	createStarted := make(chan struct{})
	allowCreate := make(chan struct{})
	var mu sync.Mutex
	destroyed := false
	transport := &fakeTransport{
		mode: "remote",
		onCreate: func(string) error {
			close(createStarted)
			<-allowCreate
			return nil
		},
		onDestroy: func(string) error {
			mu.Lock()
			destroyed = true
			mu.Unlock()
			return nil
		},
	}
	previousTransport := newTransport
	newTransport = func(Config, Runtime) (sandboxTransport, error) { return transport, nil }
	t.Cleanup(func() { newTransport = previousTransport })
	b := NewBackend(Provider{}.Spec(), Config{
		CloudRunSandbox: CloudRunSandboxConfig{CLIPath: defaultCLIPath, Workdir: defaultWorkdir},
		IdleTimeout:     time.Nanosecond,
	}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)

	createDone := make(chan error, 1)
	repoRoot := t.TempDir()
	go func() {
		_, _, _, _, err := b.createSandbox(context.Background(), transport, Repo{Root: repoRoot}, false, "creating")
		createDone <- err
	}()
	<-createStarted
	cleanupDone := make(chan error, 1)
	go func() { cleanupDone <- b.Cleanup(context.Background(), CleanupRequest{}) }()
	select {
	case err := <-cleanupDone:
		t.Fatalf("cleanup bypassed the in-flight create lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(allowCreate)
	if err := <-createDone; err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := <-cleanupDone; err != nil {
		t.Fatalf("cleanup after create: %v", err)
	}
	claims, err := listCloudRunSandboxLeaseClaims()
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	wasDestroyed := destroyed
	mu.Unlock()
	if len(claims) == 0 && !wasDestroyed {
		t.Fatal("cleanup removed the claim without destroying the completed sandbox")
	}
}

func TestCloudRunSandboxCleanupReconcilesOnlyOwnedStaleCreate(t *testing.T) {
	t.Run("owned", func(t *testing.T) {
		isolateLeaseHome(t)
		now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
		b := NewBackend(Provider{}.Spec(), Config{
			CloudRunSandbox: CloudRunSandboxConfig{CLIPath: defaultCLIPath, Workdir: defaultWorkdir},
			IdleTimeout:     time.Hour,
		}, Runtime{Clock: cloudRunSandboxFixedClock{now: now}, Stdout: io.Discard, Stderr: io.Discard}).(*backend)
		scope, err := b.claimScope()
		if err != nil {
			t.Fatal(err)
		}
		const sandboxID = "crabbox-stale-owned"
		claim, err := claimLeaseForRepoProviderScopePondWithLabels(leasePrefix+sandboxID, "stale-owned", providerName, scope, "", t.TempDir(), time.Hour, map[string]string{
			claimStateLabel:       "creating",
			claimActiveUntilLabel: now.Add(-time.Minute).Format(time.RFC3339Nano),
			claimOwnershipLabel:   sandboxID,
		})
		if err != nil {
			t.Fatal(err)
		}
		destroyed := false
		transport := &fakeTransport{mode: "direct", onDestroyOwned: func(id, token string) error {
			if id != sandboxID || token != sandboxID {
				t.Fatalf("destroy id=%q token=%q", id, token)
			}
			destroyed = true
			return nil
		}}
		previousTransport := newTransport
		newTransport = func(Config, Runtime) (sandboxTransport, error) { return transport, nil }
		t.Cleanup(func() { newTransport = previousTransport })
		if err := b.Cleanup(context.Background(), CleanupRequest{}); err != nil {
			t.Fatal(err)
		}
		if !destroyed {
			t.Fatal("stale owned create was not destroyed")
		}
		if _, exists, err := readLeaseClaimWithPresence(claim.LeaseID); err != nil || exists {
			t.Fatalf("claim exists=%v err=%v", exists, err)
		}
	})

	t.Run("missing-token-fails-closed", func(t *testing.T) {
		isolateLeaseHome(t)
		now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
		b := NewBackend(Provider{}.Spec(), Config{
			CloudRunSandbox: CloudRunSandboxConfig{CLIPath: defaultCLIPath, Workdir: defaultWorkdir},
			IdleTimeout:     time.Hour,
		}, Runtime{Clock: cloudRunSandboxFixedClock{now: now}, Stdout: io.Discard, Stderr: io.Discard}).(*backend)
		scope, err := b.claimScope()
		if err != nil {
			t.Fatal(err)
		}
		claim, err := claimLeaseForRepoProviderScopePondWithLabels(leasePrefix+"crabbox-stale-unknown", "stale-unknown", providerName, scope, "", t.TempDir(), time.Hour, map[string]string{
			claimStateLabel:       "creating",
			claimActiveUntilLabel: now.Add(-time.Minute).Format(time.RFC3339Nano),
		})
		if err != nil {
			t.Fatal(err)
		}
		destroyed := false
		transport := &fakeTransport{mode: "direct", onDestroy: func(string) error {
			destroyed = true
			return nil
		}}
		previousTransport := newTransport
		newTransport = func(Config, Runtime) (sandboxTransport, error) { return transport, nil }
		t.Cleanup(func() { newTransport = previousTransport })
		if err := b.Cleanup(context.Background(), CleanupRequest{}); err == nil || !strings.Contains(err.Error(), "no ownership token") {
			t.Fatalf("cleanup err=%v", err)
		}
		if destroyed {
			t.Fatal("cleanup destroyed a sandbox without ownership proof")
		}
		if _, exists, err := readLeaseClaimWithPresence(claim.LeaseID); err != nil || !exists {
			t.Fatalf("claim exists=%v err=%v", exists, err)
		}
	})
}

func TestCloudRunSandboxCleanupDryRunNeedsNoTransportCredentials(t *testing.T) {
	isolateLeaseHome(t)
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	var stdout bytes.Buffer
	b := NewBackend(Provider{}.Spec(), Config{
		CloudRunSandbox: CloudRunSandboxConfig{CLIPath: defaultCLIPath, Workdir: defaultWorkdir},
		IdleTimeout:     time.Hour,
	}, Runtime{Clock: cloudRunSandboxFixedClock{now: now}, Stdout: &stdout, Stderr: io.Discard}).(*backend)
	scope, err := b.claimScope()
	if err != nil {
		t.Fatal(err)
	}
	const sandboxID = "crabbox-dry-run"
	if _, err := claimLeaseForRepoProviderScopePondWithLabels(leasePrefix+sandboxID, "dry-run", providerName, scope, "", t.TempDir(), time.Hour, map[string]string{
		claimOwnershipLabel: sandboxID,
		claimExpiresAtLabel: now.Add(-time.Minute).Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.Cleanup(context.Background(), CleanupRequest{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "would delete sandbox="+sandboxID) {
		t.Fatalf("stdout=%q", stdout.String())
	}
}

func TestCloudRunSandboxCreatePersistsTTL(t *testing.T) {
	isolateLeaseHome(t)
	b := NewBackend(Provider{}.Spec(), Config{
		CloudRunSandbox: CloudRunSandboxConfig{CLIPath: defaultCLIPath, Workdir: defaultWorkdir},
		IdleTimeout:     time.Hour,
		TTL:             10 * time.Minute,
	}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)
	_, _, _, claim, err := b.createSandbox(context.Background(), &fakeTransport{mode: "direct"}, Repo{Root: t.TempDir()}, false, "ttl")
	if err != nil {
		t.Fatal(err)
	}
	expires, err := time.Parse(time.RFC3339Nano, claim.Labels[claimExpiresAtLabel])
	if err != nil {
		t.Fatalf("ttl label=%q err=%v", claim.Labels[claimExpiresAtLabel], err)
	}
	if due, reason := claimCleanupDue(claim, expires.Add(-time.Second)); due || reason != "idle-timeout-remaining" {
		t.Fatalf("before ttl due=%v reason=%s", due, reason)
	}
	if due, reason := claimCleanupDue(claim, expires); !due || reason != "ttl-expired" {
		t.Fatalf("at ttl due=%v reason=%s", due, reason)
	}
}

func TestCloudRunSandboxCleanupSerializesActiveRun(t *testing.T) {
	isolateLeaseHome(t)
	runStarted := make(chan struct{})
	allowRun := make(chan struct{})
	var mu sync.Mutex
	destroyed := false
	transport := &fakeTransport{
		mode: "direct",
		onExec: func(_ string, command string) (int, string, string, error) {
			if strings.Contains(command, "active") {
				close(runStarted)
				<-allowRun
			}
			return 0, "", "", nil
		},
		onDestroy: func(string) error {
			mu.Lock()
			destroyed = true
			mu.Unlock()
			return nil
		},
	}
	previousTransport := newTransport
	newTransport = func(Config, Runtime) (sandboxTransport, error) { return transport, nil }
	t.Cleanup(func() { newTransport = previousTransport })
	repoRoot := t.TempDir()
	b := NewBackend(Provider{}.Spec(), Config{
		CloudRunSandbox: CloudRunSandboxConfig{CLIPath: defaultCLIPath, Workdir: defaultWorkdir},
		IdleTimeout:     time.Nanosecond,
	}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)
	leaseID, _, _, _, err := b.createSandbox(context.Background(), transport, Repo{Root: repoRoot}, false, "active")
	if err != nil {
		t.Fatal(err)
	}

	runDone := make(chan error, 1)
	go func() {
		_, runErr := b.Run(context.Background(), RunRequest{
			ID:      leaseID,
			Repo:    Repo{Root: repoRoot},
			Command: []string{"echo", "active"},
			NoSync:  true,
		})
		runDone <- runErr
	}()
	<-runStarted
	cleanupDone := make(chan error, 1)
	go func() { cleanupDone <- b.Cleanup(context.Background(), CleanupRequest{}) }()
	select {
	case err := <-cleanupDone:
		t.Fatalf("cleanup bypassed the active-run claim lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(allowRun)
	if err := <-runDone; err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := <-cleanupDone; err != nil {
		t.Fatalf("cleanup after run: %v", err)
	}
	claims, err := listCloudRunSandboxLeaseClaims()
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	wasDestroyed := destroyed
	mu.Unlock()
	if len(claims) == 0 && !wasDestroyed {
		t.Fatal("cleanup removed the claim without destroying the completed sandbox")
	}
}

func TestRunMissingCommandHasNoSideEffects(t *testing.T) {
	transportCalls := 0
	previousTransport := newTransport
	newTransport = func(Config, Runtime) (sandboxTransport, error) {
		transportCalls++
		return &fakeTransport{mode: "direct"}, nil
	}
	t.Cleanup(func() { newTransport = previousTransport })
	b := NewBackend(Provider{}.Spec(), Config{
		CloudRunSandbox: CloudRunSandboxConfig{CLIPath: defaultCLIPath, Workdir: defaultWorkdir},
	}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)
	_, err := b.Run(context.Background(), RunRequest{Repo: Repo{Root: t.TempDir()}})
	if err == nil || !strings.Contains(err.Error(), "missing command") {
		t.Fatalf("run error=%v", err)
	}
	if transportCalls != 0 {
		t.Fatalf("transport created %d times", transportCalls)
	}
}

func TestCloudRunSandboxCleanupSkipsClaimReclaimedAfterSnapshot(t *testing.T) {
	isolateLeaseHome(t)
	const sandboxID = "crabbox-reclaimed-123456"
	leaseID := leasePrefix + sandboxID
	b := NewBackend(Provider{}.Spec(), Config{
		CloudRunSandbox: CloudRunSandboxConfig{CLIPath: defaultCLIPath, Workdir: defaultWorkdir},
		IdleTimeout:     time.Minute,
	}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)
	scope, err := b.claimScope()
	if err != nil {
		t.Fatal(err)
	}
	if err := claimTestCloudRunSandboxLease(leaseID, "reclaimed", scope, t.TempDir(), time.Minute); err != nil {
		t.Fatal(err)
	}
	snapshot, err := readLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	newRepo := t.TempDir()
	if err := claimLeaseForRepoProviderScopePond(leaseID, "reclaimed", providerName, scope, "", newRepo, time.Minute, true); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	destroyed := false
	transport := &fakeTransport{mode: "remote", onDestroy: func(string) error {
		destroyed = true
		return nil
	}}

	removed, err := b.destroyClaimedSandboxIfUnchanged(context.Background(), transport, sandboxID, snapshot)
	if err != nil {
		t.Fatalf("cleanup stale candidate: %v", err)
	}
	if removed {
		t.Fatal("cleanup reported a stale candidate removed")
	}
	if destroyed {
		t.Fatal("cleanup destroyed a sandbox reclaimed after its snapshot")
	}
	claim, err := readLeaseClaim(leaseID)
	if err != nil || claim.RepoRoot != newRepo {
		t.Fatalf("reclaimed claim not preserved: claim=%#v err=%v", claim, err)
	}
}

func TestCloudRunSandboxCleanupDestroyFailureRetainsClaim(t *testing.T) {
	isolateLeaseHome(t)
	const sandboxID = "crabbox-destroy-failure-123456"
	leaseID := leasePrefix + sandboxID
	b := NewBackend(Provider{}.Spec(), Config{
		CloudRunSandbox: CloudRunSandboxConfig{CLIPath: defaultCLIPath, Workdir: defaultWorkdir},
		IdleTimeout:     time.Minute,
	}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)
	scope, err := b.claimScope()
	if err != nil {
		t.Fatal(err)
	}
	if err := claimTestCloudRunSandboxLease(leaseID, "destroy-failure", scope, t.TempDir(), time.Minute); err != nil {
		t.Fatal(err)
	}
	claim, err := readLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	destroyErr := errors.New("gateway unavailable")
	transport := &fakeTransport{mode: "remote", onDestroy: func(string) error { return destroyErr }}

	removed, err := b.destroyClaimedSandboxIfUnchanged(context.Background(), transport, sandboxID, claim)
	if !errors.Is(err, destroyErr) {
		t.Fatalf("cleanup destroy failure=%v, want %v", err, destroyErr)
	}
	if removed {
		t.Fatal("cleanup reported a failed destroy removed")
	}
	retained, err := readLeaseClaim(leaseID)
	if err != nil || retained.LeaseID != leaseID {
		t.Fatalf("failed destroy lost claim: claim=%#v err=%v", retained, err)
	}
}

func TestCloudRunSandboxCleanupContinuesAfterDestroyFailure(t *testing.T) {
	isolateLeaseHome(t)
	now := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	var destroyed []string
	destroyErr := errors.New("first destroy failed")
	transport := &fakeTransport{mode: "remote", onDestroy: func(id string) error {
		destroyed = append(destroyed, id)
		if strings.Contains(id, "a-fail") {
			return destroyErr
		}
		return nil
	}}
	previousTransport := newTransport
	newTransport = func(Config, Runtime) (sandboxTransport, error) { return transport, nil }
	t.Cleanup(func() { newTransport = previousTransport })
	b := NewBackend(Provider{}.Spec(), Config{
		CloudRunSandbox: CloudRunSandboxConfig{CLIPath: defaultCLIPath, Workdir: defaultWorkdir},
		IdleTimeout:     time.Second,
	}, Runtime{Stdout: io.Discard, Stderr: io.Discard, Clock: cloudRunSandboxFixedClock{now: now}}).(*backend)
	scope, err := b.claimScope()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"crabbox-a-fail", "crabbox-z-success"} {
		if err := claimTestCloudRunSandboxLease(leasePrefix+id, id, scope, t.TempDir(), time.Second); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Cleanup(context.Background(), CleanupRequest{}); !errors.Is(err, destroyErr) {
		t.Fatalf("cleanup err=%v, want %v", err, destroyErr)
	}
	if len(destroyed) != 2 {
		t.Fatalf("destroyed=%v", destroyed)
	}
	if _, exists, err := readLeaseClaimWithPresence(leasePrefix + "crabbox-a-fail"); err != nil || !exists {
		t.Fatalf("failed claim exists=%v err=%v", exists, err)
	}
	if _, exists, err := readLeaseClaimWithPresence(leasePrefix + "crabbox-z-success"); err != nil || exists {
		t.Fatalf("successful claim exists=%v err=%v", exists, err)
	}
}

func TestCloudRunSandboxRunPropagatesAutomaticTeardownFailure(t *testing.T) {
	isolateLeaseHome(t)
	destroyErr := errors.New("delete unavailable")
	transport := &fakeTransport{
		mode:      "remote",
		onExec:    func(string, string) (int, string, string, error) { return 0, "", "", nil },
		onDestroy: func(string) error { return destroyErr },
	}
	previousTransport := newTransport
	newTransport = func(Config, Runtime) (sandboxTransport, error) { return transport, nil }
	t.Cleanup(func() { newTransport = previousTransport })
	var stderr bytes.Buffer
	b := NewBackend(Provider{}.Spec(), Config{
		CloudRunSandbox: CloudRunSandboxConfig{CLIPath: defaultCLIPath, Workdir: defaultWorkdir},
		IdleTimeout:     time.Minute,
	}, Runtime{Stdout: io.Discard, Stderr: &stderr}).(*backend)
	result, err := b.Run(context.Background(), RunRequest{
		Repo:       Repo{Root: t.TempDir()},
		Command:    []string{"true"},
		NoSync:     true,
		TimingJSON: true,
	})
	if !errors.Is(err, destroyErr) || result.Session == nil || !result.Session.Kept {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if _, exists, readErr := readLeaseClaimWithPresence(result.LeaseID); readErr != nil || !exists {
		t.Fatalf("recovery claim exists=%v err=%v", exists, readErr)
	}
	lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
	var report map[string]any
	if jsonErr := json.Unmarshal([]byte(lines[len(lines)-1]), &report); jsonErr != nil {
		t.Fatalf("final stderr line is not timing JSON: %q: %v", lines[len(lines)-1], jsonErr)
	}
	if report["runStatus"] != "failed" || report["errorKind"] != "provider-error" {
		t.Fatalf("timing report=%#v", report)
	}
}

func TestCloudRunSandboxRunEmitsTimingJSONOnWorkspaceSetupFailure(t *testing.T) {
	isolateLeaseHome(t)
	setupErr := errors.New("workspace unavailable")
	transport := &fakeTransport{
		mode: "direct",
		onExec: func(_ string, command string) (int, string, string, error) {
			if strings.Contains(command, "mkdir -p") {
				return 1, "", "", setupErr
			}
			return 0, "", "", nil
		},
	}
	previousTransport := newTransport
	newTransport = func(Config, Runtime) (sandboxTransport, error) { return transport, nil }
	t.Cleanup(func() { newTransport = previousTransport })
	var stderr bytes.Buffer
	b := NewBackend(Provider{}.Spec(), Config{
		CloudRunSandbox: CloudRunSandboxConfig{CLIPath: defaultCLIPath, Workdir: defaultWorkdir},
		IdleTimeout:     time.Minute,
	}, Runtime{Stdout: io.Discard, Stderr: &stderr}).(*backend)
	_, err := b.Run(context.Background(), RunRequest{
		Repo:       Repo{Root: t.TempDir()},
		Command:    []string{"true"},
		NoSync:     true,
		TimingJSON: true,
	})
	if !errors.Is(err, setupErr) {
		t.Fatalf("run err=%v", err)
	}
	lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
	var report map[string]any
	if jsonErr := json.Unmarshal([]byte(lines[len(lines)-1]), &report); jsonErr != nil {
		t.Fatalf("final stderr line is not timing JSON: %q: %v", lines[len(lines)-1], jsonErr)
	}
	if report["runStatus"] != "failed" || report["syncSkipped"] != true {
		t.Fatalf("timing report=%#v", report)
	}
}

func TestCloudRunSandboxStatusReportsExpiredClaim(t *testing.T) {
	isolateLeaseHome(t)
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	b := NewBackend(Provider{}.Spec(), Config{
		CloudRunSandbox: CloudRunSandboxConfig{CLIPath: defaultCLIPath, Workdir: defaultWorkdir},
	}, Runtime{Stdout: io.Discard, Stderr: io.Discard, Clock: cloudRunSandboxFixedClock{now: now}}).(*backend)
	scope, err := b.claimScope()
	if err != nil {
		t.Fatal(err)
	}
	leaseID := leasePrefix + "expired-status"
	if err := claimTestCloudRunSandboxLease(leaseID, "expired", scope, t.TempDir(), time.Minute); err != nil {
		t.Fatal(err)
	}
	claim, err := readLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	labels := cloneLabels(claim.Labels)
	labels[claimExpiresAtLabel] = now.Add(-time.Second).Format(time.RFC3339Nano)
	if _, err := updateLeaseClaimLabelsIfUnchanged(leaseID, claim, labels); err != nil {
		t.Fatal(err)
	}
	status, err := b.Status(context.Background(), StatusRequest{ID: leaseID})
	if err != nil || status.Ready || status.State != "expired" {
		t.Fatalf("status=%#v err=%v", status, err)
	}
	leases, err := b.List(context.Background(), ListRequest{})
	if err != nil || len(leases) != 1 || leases[0].Status != "expired" {
		t.Fatalf("expired list=%#v err=%v", leases, err)
	}
}

func TestCloudRunSandboxClearActivityTouchesLastUsed(t *testing.T) {
	isolateLeaseHome(t)
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	b := NewBackend(Provider{}.Spec(), Config{
		CloudRunSandbox: CloudRunSandboxConfig{CLIPath: defaultCLIPath, Workdir: defaultWorkdir},
		IdleTimeout:     time.Minute,
	}, Runtime{Stdout: io.Discard, Stderr: io.Discard, Clock: cloudRunSandboxFixedClock{now: now}}).(*backend)
	scope, err := b.claimScope()
	if err != nil {
		t.Fatal(err)
	}
	leaseID := leasePrefix + "touch-last-used"
	if err := claimTestCloudRunSandboxLease(leaseID, "touch", scope, t.TempDir(), time.Minute); err != nil {
		t.Fatal(err)
	}
	claim, err := readLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	claim, err = b.markClaimActivity(claim, "running", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	cleared, err := b.clearClaimActivity(claim)
	if err != nil {
		t.Fatal(err)
	}
	if cleared.LastUsedAt != now.Format(time.RFC3339) {
		t.Fatalf("lastUsedAt=%q want=%q", cleared.LastUsedAt, now.Format(time.RFC3339))
	}
	if due, reason := claimCleanupDue(cleared, now); due || reason != "idle-timeout-remaining" {
		t.Fatalf("freshly completed claim due=%v reason=%s", due, reason)
	}
}

func TestCloudRunSandboxReclaimCannotRepublishAfterCleanupWins(t *testing.T) {
	isolateLeaseHome(t)
	const sandboxID = "crabbox-cleanup-wins-123456"
	leaseID := leasePrefix + sandboxID
	destroyStarted := make(chan struct{})
	allowDestroy := make(chan struct{})
	transport := &fakeTransport{mode: "remote", onDestroy: func(string) error {
		close(destroyStarted)
		<-allowDestroy
		return nil
	}}
	previousTransport := newTransport
	newTransport = func(Config, Runtime) (sandboxTransport, error) { return transport, nil }
	t.Cleanup(func() { newTransport = previousTransport })

	b := NewBackend(Provider{}.Spec(), Config{
		CloudRunSandbox: CloudRunSandboxConfig{CLIPath: defaultCLIPath, Workdir: defaultWorkdir},
		IdleTimeout:     time.Second,
	}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)
	scope, err := b.claimScope()
	if err != nil {
		t.Fatal(err)
	}
	if err := claimTestCloudRunSandboxLease(leaseID, "cleanup-wins", scope, t.TempDir(), time.Second); err != nil {
		t.Fatal(err)
	}
	claim, err := readLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	expired := claim
	expired.LastUsedAt = time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	if err := core.ReplaceLeaseClaimIfUnchanged(leaseID, claim, expired); err != nil {
		t.Fatalf("expire claim: %v", err)
	}

	cleanupDone := make(chan error, 1)
	go func() { cleanupDone <- b.Cleanup(context.Background(), CleanupRequest{}) }()
	<-destroyStarted

	reclaimDone := make(chan error, 1)
	newRepo := t.TempDir()
	go func() {
		_, _, _, _, resolveErr := b.resolveLeaseID(leaseID, newRepo, true)
		reclaimDone <- resolveErr
	}()
	select {
	case reclaimErr := <-reclaimDone:
		t.Fatalf("reclaim bypassed cleanup claim lock: %v", reclaimErr)
	case <-time.After(100 * time.Millisecond):
	}
	close(allowDestroy)
	if err := <-cleanupDone; err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if err := <-reclaimDone; err == nil || (!strings.Contains(err.Error(), "claim changed") && !strings.Contains(err.Error(), "not claimed by Crabbox")) {
		t.Fatalf("reclaim error=%v, want guarded missing-claim failure", err)
	}
	if _, exists, err := readLeaseClaimWithPresence(leaseID); err != nil || exists {
		t.Fatalf("reclaim republished deleted sandbox claim: exists=%v err=%v", exists, err)
	}
}

func TestProviderSpec(t *testing.T) {
	t.Parallel()
	spec := Provider{}.Spec()
	if spec.Name != providerName {
		t.Fatalf("name=%q", spec.Name)
	}
	if spec.Kind != core.ProviderKindDelegatedRun {
		t.Fatalf("kind=%v", spec.Kind)
	}
	for _, feature := range []core.Feature{core.FeatureArchiveSync, core.FeatureRunSession, core.FeatureCleanup} {
		if !spec.Features.Has(feature) {
			t.Fatalf("missing feature %s in %v", feature, spec.Features)
		}
	}
	if spec.Coordinator != core.CoordinatorNever {
		t.Fatalf("coordinator=%v", spec.Coordinator)
	}
}

func TestRunWithFakeTransport(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	fake := &fakeTransport{
		mode: "remote",
		onCreate: func(id string) error {
			mu.Lock()
			calls = append(calls, "create:"+id)
			mu.Unlock()
			return nil
		},
		onExec: func(_ string, command string) (int, string, string, error) {
			mu.Lock()
			calls = append(calls, "exec:"+command)
			mu.Unlock()
			if strings.Contains(command, "mkdir") {
				return 0, "", "", nil
			}
			return 0, "hello\n", "", nil
		},
		onDestroy: func(id string) error {
			mu.Lock()
			calls = append(calls, "destroy:"+id)
			mu.Unlock()
			return nil
		},
	}
	prev := newTransport
	newTransport = func(Config, Runtime) (sandboxTransport, error) { return fake, nil }
	t.Cleanup(func() { newTransport = prev })

	// Isolate lease claims to a temp dir.
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "state"))

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	b := NewBackend(Provider{}.Spec(), Config{
		CloudRunSandbox: CloudRunSandboxConfig{
			CLIPath: defaultCLIPath,
			Workdir: defaultWorkdir,
			Write:   true,
		},
		IdleTimeout: 30 * time.Minute,
	}, Runtime{
		Stdout: &stdout,
		Stderr: &stderr,
	}).(*backend)

	result, err := b.Run(context.Background(), RunRequest{
		Repo:    Repo{Root: root},
		Command: []string{"echo", "hello"},
		NoSync:  true,
		Keep:    false,
	})
	if err != nil {
		t.Fatalf("Run: %v\nstderr=%s", err, stderr.String())
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit=%d", result.ExitCode)
	}
	if !strings.Contains(stdout.String(), "hello") {
		t.Fatalf("stdout=%q", stdout.String())
	}
	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(calls, ",")
	if !strings.Contains(joined, "create:") || !strings.Contains(joined, "exec:") || !strings.Contains(joined, "destroy:") {
		t.Fatalf("unexpected calls: %v", calls)
	}
}

func TestValidateConfig(t *testing.T) {
	t.Parallel()
	cfg := Config{CloudRunSandbox: CloudRunSandboxConfig{CLIPath: defaultCLIPath, Workdir: defaultWorkdir}}
	if err := validateConfig(cfg); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	cfg.CloudRunSandbox.Workdir = "relative"
	if err := validateConfig(cfg); err == nil {
		t.Fatal("expected relative workdir rejection")
	}
	cfg.CloudRunSandbox.Workdir = defaultWorkdir
	cfg.CloudRunSandbox.CLIPath = ""
	if err := validateConfig(cfg); err == nil {
		t.Fatal("expected empty cliPath rejection")
	}
}

type fakeTransport struct {
	mode           string
	onCreate       func(string) error
	onProbe        func(string, string) error
	onExec         func(string, string) (int, string, string, error)
	onDestroy      func(string) error
	onDestroyOwned func(string, string) error
	onWrite        func(string, string) error
}

func claimTestCloudRunSandboxLease(leaseID, slug, scope, repoRoot string, idleTimeout time.Duration) error {
	_, err := claimLeaseForRepoProviderScopePondWithLabels(leaseID, slug, providerName, scope, "", repoRoot, idleTimeout, map[string]string{
		claimOwnershipLabel: strings.TrimPrefix(leaseID, leasePrefix),
	})
	return err
}

func (f *fakeTransport) Mode() string                 { return f.mode }
func (f *fakeTransport) Health(context.Context) error { return nil }

func (f *fakeTransport) Probe(_ context.Context, sandboxID, ownershipToken string) error {
	if f.onProbe != nil {
		return f.onProbe(sandboxID, ownershipToken)
	}
	return nil
}
func (f *fakeTransport) Create(_ context.Context, sandboxID string, _ runOptions) error {
	if f.onCreate != nil {
		return f.onCreate(sandboxID)
	}
	return nil
}
func (f *fakeTransport) Exec(_ context.Context, sandboxID, command string, _ execOptions, stdout, stderr io.Writer) (int, error) {
	if f.onExec != nil {
		code, out, errOut, err := f.onExec(sandboxID, command)
		if stdout != nil && out != "" {
			_, _ = io.WriteString(stdout, out)
		}
		if stderr != nil && errOut != "" {
			_, _ = io.WriteString(stderr, errOut)
		}
		return code, err
	}
	return 0, nil
}
func (f *fakeTransport) Destroy(_ context.Context, sandboxID, ownershipToken string) error {
	if f.onDestroyOwned != nil {
		return f.onDestroyOwned(sandboxID, ownershipToken)
	}
	if f.onDestroy != nil {
		return f.onDestroy(sandboxID)
	}
	return nil
}
func (f *fakeTransport) WriteFile(_ context.Context, sandboxID, path, _ string, _ bool) error {
	if f.onWrite != nil {
		return f.onWrite(sandboxID, path)
	}
	return nil
}
