package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

type runLeaseObserverFixtureProvider struct{}

func (runLeaseObserverFixtureProvider) Spec() ProviderSpec {
	return ProviderSpec{Name: "run-lease-observer-test", Kind: ProviderKindSSHLease, Targets: []TargetSpec{{OS: targetLinux}}, Features: FeatureSet{FeatureSSH, FeatureCrabboxSync}, Coordinator: CoordinatorNever}
}
func (runLeaseObserverFixtureProvider) RegisterFlags(*flag.FlagSet, Config) any {
	return noProviderFlags{}
}
func (runLeaseObserverFixtureProvider) ApplyFlags(*Config, *flag.FlagSet, any) error { return nil }
func (runLeaseObserverFixtureProvider) Configure(Config, Runtime) (Backend, error) {
	if currentRunLeaseObserverFixture == nil {
		return nil, errors.New("run lease observer fixture is not configured")
	}
	if currentRunLeaseObserverFixture.transactional {
		return runLeaseObserverTransactionFixture{currentRunLeaseObserverFixture}, nil
	}
	return currentRunLeaseObserverFixture, nil
}

func init() { RegisterProvider(runLeaseObserverFixtureProvider{}) }

var currentRunLeaseObserverFixture *runLeaseObserverFixtureBackend

type runLeaseObserverFixtureBackend struct {
	lease          LeaseTarget
	events         []string
	allowAcquire   bool
	reportedTarget string
	transactional  bool
}

type runLeaseObserverTransactionFixture struct {
	*runLeaseObserverFixtureBackend
}

func (b runLeaseObserverTransactionFixture) ResolveRunLeaseUnderClaim(_ context.Context, _ ResolveRequest, original LeaseClaim) (LeaseTarget, error) {
	lease := cloneRunLeaseTarget(b.lease)
	lease.Server.Labels = cloneStringMap(original.Labels)
	if b.reportedTarget != "" {
		lease.Server.Labels["target"] = b.reportedTarget
		lease.Server.Labels["windows_mode"] = windowsModeNormal
	}
	SetServerLeaseClaimSnapshot(&lease.Server, original, true)
	return lease, nil
}

func (*runLeaseObserverFixtureBackend) Spec() ProviderSpec {
	return runLeaseObserverFixtureProvider{}.Spec()
}
func (b *runLeaseObserverFixtureBackend) Acquire(context.Context, AcquireRequest) (LeaseTarget, error) {
	if !b.allowAcquire {
		return LeaseTarget{}, errors.New("unexpected fixture acquisition")
	}
	return cloneRunLeaseTarget(b.lease), nil
}
func (b *runLeaseObserverFixtureBackend) Resolve(context.Context, ResolveRequest) (LeaseTarget, error) {
	lease := cloneRunLeaseTarget(b.lease)
	claim, err := ReadLeaseClaim(lease.LeaseID)
	if err != nil {
		return LeaseTarget{}, err
	}
	lease.Server.ImmutableID = claim.CloudImmutableID
	lease.Server.Labels = cloneStringMap(claim.Labels)
	if b.reportedTarget != "" {
		lease.Server.Labels["target"] = b.reportedTarget
	}
	SetServerLeaseClaimSnapshot(&lease.Server, claim, true)
	return lease, nil
}
func (*runLeaseObserverFixtureBackend) List(context.Context, ListRequest) ([]LeaseView, error) {
	return nil, nil
}
func (*runLeaseObserverFixtureBackend) ReleaseLease(context.Context, ReleaseLeaseRequest) error {
	return errors.New("reused fixture lease must remain owned by its caller")
}
func (b *runLeaseObserverFixtureBackend) Touch(ctx context.Context, req TouchRequest) (Server, error) {
	claim, exists, set := ServerLeaseClaimSnapshot(req.Lease.Server)
	if !set || !exists {
		return Server{}, errors.New("fixture touch lacks committed authority")
	}
	labels := cloneStringMap(req.Lease.Server.Labels)
	labels["state"] = req.State
	updated, err := UpdateLeaseClaimTouchIfUnchanged(ctx, req.Lease.LeaseID, claim, labels, time.Now().UTC(), nil)
	if err != nil {
		return Server{}, err
	}
	b.events = append(b.events, "touch-"+req.State)
	server := req.Lease.Server
	server.Labels = labels
	SetServerLeaseClaimSnapshot(&server, updated, true)
	return server, nil
}

func setupRunLeaseObserverFixture(t *testing.T) (*runLeaseObserverFixtureBackend, Config, LeaseClaim) {
	t.Helper()
	clearConfigEnv(t)
	root := newWatchGitRepo(t)
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	cfg := baseConfig()
	cfg.Provider = runLeaseObserverFixtureProvider{}.Spec().Name
	lease := LeaseTarget{LeaseID: watchTestLeaseID, Server: Server{Provider: cfg.Provider, CloudID: "fixture-resource", Labels: map[string]string{"lease": watchTestLeaseID, "state": "ready"}}, SSH: SSHTarget{Host: "127.0.0.1", Port: "22", User: "fixture", TargetOS: targetLinux}}
	if err := ClaimLeaseTargetForRepoConfig(lease.LeaseID, "", cfg, lease.Server, lease.SSH, root, cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}
	initial, err := ReadLeaseClaim(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	SetServerLeaseClaimSnapshot(&lease.Server, initial, true)
	backend := &runLeaseObserverFixtureBackend{lease: lease}
	currentRunLeaseObserverFixture = backend
	t.Cleanup(func() { currentRunLeaseObserverFixture = nil })
	return backend, cfg, initial
}

func TestRunFinalLeaseObserverAfterCommittedSetupFailure(t *testing.T) {
	for _, transactional := range []bool{false, true} {
		t.Run(fmt.Sprint("transactional=", transactional), func(t *testing.T) {
			checkRunFinalLeaseObserverAfterCommittedSetupFailure(t, transactional)
		})
	}
}

func checkRunFinalLeaseObserverAfterCommittedSetupFailure(t *testing.T, transactional bool) {
	t.Helper()
	backend, cfg, initial := setupRunLeaseObserverFixture(t)
	backend.transactional = transactional
	lease := backend.lease
	stopped := errors.New("fixture stops before SSH")
	app := App{Stdout: io.Discard, Stderr: io.Discard, sshReadinessWaiter: func(context.Context, *SSHTarget, io.Writer, string, time.Duration) error {
		backend.events = append(backend.events, "readiness-failure")
		return stopped
	}}
	observed := 0
	err := app.runCommandWithLeaseObserver(context.Background(), []string{"--provider", cfg.Provider, "--id", lease.LeaseID, "--no-sync", "--", "true"}, func(final LeaseTarget) {
		observed++
		backend.events = append(backend.events, "observer")
		claim, exists, set := ServerLeaseClaimSnapshot(final.Server)
		current, err := ReadLeaseClaim(lease.LeaseID)
		if err != nil || !set || !exists || !reflect.DeepEqual(claim, current) || claim.Revision == initial.Revision {
			t.Error("final callback missed the run's committed setup authority")
		}
	})
	if !errors.Is(err, stopped) || observed != 1 {
		t.Fatalf("producer result: error=%v observer-count=%d events=%v", err, observed, backend.events)
	}
	if !reflect.DeepEqual(backend.events, []string{"touch-ready", "readiness-failure", "observer"}) {
		t.Fatalf("producer callback preceded pipeline finalization: %v", backend.events)
	}
}

func TestRunFinalLeaseObserverRejectsUncommittedAdmission(t *testing.T) {
	backend, cfg, initial := setupRunLeaseObserverFixture(t)
	backend.transactional = true
	backend.reportedTarget = targetWindows
	app := App{Stdout: io.Discard, Stderr: io.Discard}
	observed := 0
	err := app.runCommandWithLeaseObserver(t.Context(), []string{"--provider", cfg.Provider, "--id", backend.lease.LeaseID, "--no-sync", "--artifact-glob", ".artifacts/**", "--", "true"}, func(LeaseTarget) { observed++ })
	if err == nil || !strings.Contains(err.Error(), "--artifact-glob is not supported") {
		t.Fatalf("fixture did not reject prepared admission: %v", err)
	}
	current, readErr := ReadLeaseClaim(backend.lease.LeaseID)
	if observed != 0 || readErr != nil || !reflect.DeepEqual(current, initial) || len(backend.events) != 0 {
		t.Fatalf("prepared-but-uncommitted admission handed off authority: callbacks=%d events=%v", observed, backend.events)
	}
}

func TestRunLeasePublicationSurvivesRegistrationError(t *testing.T) {
	backend, cfg, initial := setupRunLeaseObserverFixture(t)
	// This invalid adapter binding is checked after local claim publication and
	// before any coordinator request, so the regression needs no endpoint.
	t.Setenv("CRABBOX_ADAPTER_ID", "invalid fixture id")
	t.Setenv(controllerWorkspaceIDEnv, "fixture-workspace")
	var final runLeaseHandoff
	app := App{Stdout: io.Discard, Stderr: io.Discard, runLeaseTarget: &final}
	lease := cloneRunLeaseTarget(backend.lease)
	err := app.claimRunLeaseTargetForRepoAndRegister(t.Context(), lease.LeaseID, ServerSlug(lease.Server), &cfg, &lease.Server, lease.SSH, initial.RepoRoot, false, true, nil)
	if err == nil || !strings.Contains(err.Error(), "valid adapter and workspace IDs") {
		t.Fatalf("fixture did not fail after local publication: %v", err)
	}
	claim, exists, set := ServerLeaseClaimSnapshot(final.lease.Server)
	current, readErr := ReadLeaseClaim(lease.LeaseID)
	if !final.published || !exists || !set || readErr != nil || claim.Revision == initial.Revision || !reflect.DeepEqual(claim, current) {
		t.Fatal("post-publication registration error discarded known committed authority")
	}
}

func TestWatchRunProducerHandsOffFailedSetupAuthority(t *testing.T) {
	backend, cfg, initial := setupRunLeaseObserverFixture(t)
	backend.allowAcquire = true
	repo, err := findRepo()
	if err != nil {
		t.Fatal(err)
	}
	stopped := errors.New("fixture stops before SSH")
	app := App{Stdout: io.Discard, Stderr: io.Discard, sshReadinessWaiter: func(context.Context, *SSHTarget, io.Writer, string, time.Duration) error { return stopped }}
	owned := &watchAuthorityBackend{SSHLeaseBackend: backend}
	opts := watchOptions{Debounce: time.Millisecond, IdleExit: 100 * time.Millisecond, IdleExitSet: true, Command: []string{"true"}}
	err = app.watchWithBackend(t.Context(), opts, repo, cfg, owned, app.runCommandWithLeaseObserver)
	if !errors.Is(err, stopped) || owned.released != 1 {
		t.Fatalf("integrated owned-run handoff: error=%v releases=%d", err, owned.released)
	}
	claim, exists, set := ServerLeaseClaimSnapshot(owned.final.Server)
	if !exists || !set || claim.Revision == initial.Revision || !reflect.DeepEqual(backend.events, []string{"touch-ready"}) {
		t.Fatal("watch cleanup did not receive the real producer's committed failure target")
	}
}

func TestWatchRunProducerRejectsReadOnlyPreAdmissionAuthority(t *testing.T) {
	backend, cfg, _ := setupRunLeaseObserverFixture(t)
	backend.allowAcquire = true
	backend.reportedTarget = targetMacOS
	repo, err := findRepo()
	if err != nil {
		t.Fatal(err)
	}
	app := App{Stdout: io.Discard, Stderr: io.Discard}
	owned := &watchAuthorityBackend{SSHLeaseBackend: backend}
	opts := watchOptions{Debounce: time.Millisecond, IdleExit: 100 * time.Millisecond, IdleExitSet: true, RunArgs: []string{"--target", "linux"}, Command: []string{"true"}}
	var independent LeaseClaim
	handoffs := 0
	execute := func(ctx context.Context, args []string, observe func(LeaseTarget)) error {
		// Another writer changes only the current claim revision before this
		// iteration. Resolve merely reads it; platform rejection precedes any
		// claim admission, touch or transport by the actual run producer.
		other, err := watchOwnedMutation(ctx, watchTestLeaseID)
		if err != nil {
			return err
		}
		independent, _, _ = ServerLeaseClaimSnapshot(other.Server)
		return app.runCommandWithLeaseObserver(ctx, args, func(final LeaseTarget) {
			handoffs++
			observe(final)
		})
	}
	err = app.watchWithBackend(t.Context(), opts, repo, cfg, owned, execute)
	if err == nil || !strings.Contains(err.Error(), "lease target mismatch") {
		t.Fatalf("fixture did not fail before admission: %v", err)
	}
	if handoffs != 0 || owned.released != 0 || len(backend.events) != 0 {
		t.Fatalf("read-only pre-admission result adopted independent authority: handoffs=%d disposals=%d events=%v", handoffs, owned.released, backend.events)
	}
	current, err := ReadLeaseClaim(watchTestLeaseID)
	if err != nil || !reflect.DeepEqual(current, independent) {
		t.Fatal("pre-admission failure changed independently published authority")
	}
}

func TestRunFinalLeaseObserverAfterDeferredReadyTouch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("inert shell transport fixture")
	}
	backend, cfg, _ := setupRunLeaseObserverFixture(t)
	// Every transport subprocess is this local recording script, never an SSH
	// client. Readiness is injected too, so no network probe can reach a guest.
	bin := t.TempDir()
	path := os.Getenv("PATH")
	installRecordingSSH(t, bin)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+path)
	app := App{Stdout: io.Discard, Stderr: io.Discard, sshReadinessWaiter: func(context.Context, *SSHTarget, io.Writer, string, time.Duration) error { return nil }}
	observed := 0
	err := app.runCommandWithLeaseObserver(t.Context(), []string{"--provider", cfg.Provider, "--id", backend.lease.LeaseID, "--no-sync", "--no-hydrate", "--", "true"}, func(final LeaseTarget) {
		observed++
		backend.events = append(backend.events, "observer")
		claim, exists, set := ServerLeaseClaimSnapshot(final.Server)
		current, err := ReadLeaseClaim(final.LeaseID)
		if err != nil || !set || !exists || !reflect.DeepEqual(claim, current) || claim.Labels["state"] != "ready" {
			t.Error("final callback preceded the deferred ready touch")
		}
	})
	if err != nil || observed != 1 {
		t.Fatalf("producer result: error=%v observer-count=%d events=%v", err, observed, backend.events)
	}
	if !reflect.DeepEqual(backend.events, []string{"touch-ready", "touch-running", "touch-ready", "observer"}) {
		t.Fatalf("final producer ordering: %v", backend.events)
	}
}

func TestRunLeaseTargetHandoffClonesAuthority(t *testing.T) {
	lease := LeaseTarget{LeaseID: watchTestLeaseID, Server: Server{Labels: map[string]string{"state": "ready"}}}
	claim := LeaseClaim{LeaseID: lease.LeaseID, Labels: map[string]string{"state": "ready"}}
	SetServerLeaseClaimSnapshot(&lease.Server, claim, true)
	var final runLeaseHandoff
	(App{runLeaseTarget: &final}).recordRunLeasePublication(lease)
	lease.Server.Labels["state"] = "changed"
	SetServerLeaseClaimSnapshot(&lease.Server, LeaseClaim{}, false)
	carried, exists, set := ServerLeaseClaimSnapshot(final.lease.Server)
	if !set || !exists || final.lease.Server.Labels["state"] != "ready" || carried.Labels["state"] != "ready" {
		t.Fatal("handoff authority aliases mutable producer state")
	}
}
