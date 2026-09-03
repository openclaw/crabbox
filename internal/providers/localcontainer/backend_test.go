package localcontainer

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const testRecoveredContainerID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type recordingRunner struct {
	calls     []core.LocalCommandRequest
	responses map[string]core.LocalCommandResult
	run       func(core.LocalCommandRequest) (core.LocalCommandResult, error)
}

type recordedCommandSummary struct {
	Name string
	Args []string
}

func (r *recordingRunner) commandSummary() []recordedCommandSummary {
	// Keep captured requests intact, but never format their environment or streams.
	summary := make([]recordedCommandSummary, len(r.calls))
	for i, req := range r.calls {
		summary[i] = recordedCommandSummary{Name: req.Name, Args: req.Args}
	}
	return summary
}

type localContainerTestClock struct{ now time.Time }

func (c localContainerTestClock) Now() time.Time { return c.now }

func (r *recordingRunner) Run(_ context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	r.calls = append(r.calls, req)
	if r.run != nil {
		return r.run(req)
	}
	if result, ok := r.responses[commandKey(req.Args)]; ok {
		return result, nil
	}
	if len(req.Args) > 0 {
		if result, ok := r.responses[req.Args[0]]; ok {
			return result, nil
		}
	}
	return core.LocalCommandResult{}, nil
}

func commandKey(args []string) string {
	return strings.Join(args, "\x00")
}

func recordedArgsForCommand(t *testing.T, runner *recordingRunner, command string) string {
	t.Helper()
	for i := len(runner.calls) - 1; i >= 0; i-- {
		if len(runner.calls[i].Args) > 0 && runner.calls[i].Args[0] == command {
			return strings.Join(runner.calls[i].Args, "\n")
		}
	}
	t.Fatalf("%s command was not recorded: %#v", command, runner.commandSummary())
	return ""
}

func listenUnixSocketOrSkip(t *testing.T, path string) net.Listener {
	t.Helper()
	listener, err := net.Listen("unix", path)
	if err != nil {
		if errors.Is(err, os.ErrPermission) || strings.Contains(err.Error(), "invalid argument") {
			t.Skipf("unix sockets are not permitted in this environment: %v", err)
		}
		t.Fatal(err)
	}
	return listener
}

func testBackend(runner *recordingRunner) *backend {
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
	core.MarkLocalContainerRuntimeExplicit(&cfg)
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)
	b.captureRuntimeScope = func(context.Context, core.Config) (checkpointScope, error) {
		return checkpointScope{Runtime: "docker", Context: "default", Endpoint: "unix:///tmp/docker-test.sock", DaemonID: "daemon-test"}, nil
	}
	b.validateRuntimeScope = func(context.Context, checkpointScope) error { return nil }
	b.confirmContainerAbsent = func(context.Context, string) (bool, error) { return true, nil }
	return b
}

func TestBackendAdvertisesAcquireCapabilities(t *testing.T) {
	b := testBackend(&recordingRunner{})
	capability, ok := any(b).(core.ExclusiveOneShotAcquireBackend)
	if !ok || !capability.AcquireIsExclusiveOneShot() {
		t.Fatal("local-container must advertise exclusive fresh acquisitions")
	}
	fixed, ok := any(b).(core.IdempotentLeaseIDBackend)
	if !ok || !fixed.SupportsRequestedLeaseID() {
		t.Fatal("local-container must advertise idempotent fixed lease acquisitions")
	}
}

func TestFixedLocalContainerFingerprintPreservesSubsecondLifecycleDurations(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*core.Config)
	}{
		{name: "ttl", mutate: func(cfg *core.Config) { cfg.TTL += time.Nanosecond }},
		{name: "idle timeout", mutate: func(cfg *core.Config) { cfg.IdleTimeout += time.Nanosecond }},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := testBackend(&recordingRunner{}).cfg
			cfg.TTL = 90*time.Second + 100*time.Millisecond
			cfg.IdleTimeout = 30*time.Second + 100*time.Millisecond
			req := core.AcquireRequest{Keep: true, RequestedSlug: "precise-duration"}
			original, err := fixedLocalContainerFingerprint(cfg, req, "ssh-ed25519 precise-duration")
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(&cfg)
			changed, err := fixedLocalContainerFingerprint(cfg, req, "ssh-ed25519 precise-duration")
			if err != nil {
				t.Fatal(err)
			}
			if changed == original {
				t.Fatalf("%s nanosecond drift did not change fixed fingerprint %q", test.name, original)
			}
		})
	}
}

func TestFixedLocalContainerFingerprintNormalizesPond(t *testing.T) {
	cfg := testBackend(&recordingRunner{}).cfg
	cfg.Pond = "  Alpha__Pond  "
	req := core.AcquireRequest{Keep: true, RequestedSlug: "pond-fingerprint"}
	original, err := fixedLocalContainerFingerprint(cfg, req, "ssh-ed25519 pond-fingerprint")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		pond string
		same bool
	}{
		{name: "equivalent spelling", pond: "alpha-pond", same: true},
		{name: "different pond", pond: "beta", same: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := cfg
			changed.Pond = test.pond
			fingerprint, fingerprintErr := fixedLocalContainerFingerprint(changed, req, "ssh-ed25519 pond-fingerprint")
			if fingerprintErr != nil {
				t.Fatal(fingerprintErr)
			}
			if (fingerprint == original) != test.same {
				t.Fatalf("pond %q fingerprint=%q original=%q same=%t", test.pond, fingerprint, original, test.same)
			}
		})
	}
}

func TestFixedAcquireCreatesRequestedIDAndReplays(t *testing.T) {
	b, runner, _, createdLeaseID, _, _ := pendingAcquireBackend(t)
	b.waitForSSHReady = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error { return nil }
	req := core.AcquireRequest{
		Repo: core.Repo{Root: t.TempDir()}, Keep: true,
		RequestedLeaseID: "cbx_abcdef123451", RequestedSlug: "pending-test",
	}
	// Generate the test-owned key before hiding system tools; replay reuses it.
	if _, _, err := core.EnsureTestboxKeyForConfig(b.cfg, req.RequestedLeaseID); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	writeExecutable(t, filepath.Join(dir, "podman"))
	t.Setenv("PATH", dir)

	first, err := b.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if first.LeaseID != req.RequestedLeaseID || *createdLeaseID != req.RequestedLeaseID {
		t.Fatalf("fixed lease=%q created=%q, want %q", first.LeaseID, *createdLeaseID, req.RequestedLeaseID)
	}
	claim, err := core.ReadLeaseClaim(req.RequestedLeaseID)
	if err != nil || claim.Provider != core.FixedLocalContainerClaimProvider ||
		claim.FixedCreateIntent == nil || claim.FixedCreateIntent.State != "acquired" {
		t.Fatalf("fixed claim=%#v err=%v", claim, err)
	}
	if claim.FixedCreateIntent.Fingerprint != first.Server.Labels["fixed_intent_sha256"] {
		t.Fatalf("fixed claim fingerprint does not match container: %#v", claim.FixedCreateIntent)
	}
	if first.Server.Provider != providerName || first.Server.Labels["provider"] != providerName {
		t.Fatalf("fixed lease exposed persisted provider marker: %#v", first.Server)
	}
	resolvedClaim, ok, err := core.ResolveLeaseClaimForProvider(req.RequestedLeaseID, providerName)
	if err != nil || !ok || resolvedClaim.Provider != core.FixedLocalContainerClaimProvider {
		t.Fatalf("canonical fixed claim=%#v ok=%t err=%v", resolvedClaim, ok, err)
	}
	if bySlug, found, lookupErr := localContainerClaimByIDOrSlug(req.RequestedSlug); lookupErr != nil ||
		!found || bySlug.LeaseID != req.RequestedLeaseID {
		t.Fatalf("acquired fixed slug claim=%#v found=%t err=%v", bySlug, found, lookupErr)
	}
	claims, err := core.ListLeaseClaims()
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range claims {
		if candidate.Provider == providerName {
			t.Fatalf("legacy-style local-container cleanup can see fixed claim: %#v", candidate)
		}
	}

	restarted := testBackend(runner)
	restarted.waitForSSHReady = b.waitForSSHReady
	views, err := restarted.List(context.Background(), core.ListRequest{})
	if err != nil || len(views) != 1 || views[0].Provider != providerName || views[0].CloudID != first.Server.CloudID {
		t.Fatalf("fixed lease views=%#v err=%v", views, err)
	}
	for _, identifier := range []string{req.RequestedLeaseID, req.RequestedSlug} {
		resolved, resolveErr := restarted.Resolve(context.Background(), core.ResolveRequest{
			ID: identifier, StatusOnly: true, NoLocalStateMutations: true,
		})
		if resolveErr != nil || resolved.LeaseID != first.LeaseID || resolved.Server.Provider != providerName {
			t.Fatalf("resolved fixed lease %q=%#v err=%v", identifier, resolved, resolveErr)
		}
		if err := restarted.AuthorizeStatusTouchClaim(context.Background(), resolved, claim); err != nil {
			t.Fatalf("authorize fixed lease %q: %v", identifier, err)
		}
		if identifier == req.RequestedLeaseID {
			touched, touchErr := restarted.Touch(context.Background(), core.TouchRequest{Lease: resolved, State: "busy"})
			updated, readErr := core.ReadLeaseClaim(req.RequestedLeaseID)
			if touchErr != nil || readErr != nil || touched.Provider != providerName || updated.Provider != core.FixedLocalContainerClaimProvider {
				t.Fatalf("fixed touch server=%#v claim=%#v touchErr=%v readErr=%v", touched, updated, touchErr, readErr)
			}
			claim = updated
		}
	}
	if err := restarted.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if current, readErr := core.ReadLeaseClaim(req.RequestedLeaseID); readErr != nil || current.Provider != core.FixedLocalContainerClaimProvider {
		t.Fatalf("cleanup altered live fixed claim: claim=%#v err=%v", current, readErr)
	}
	replayed, err := restarted.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.LeaseID != first.LeaseID || replayed.Server.CloudID != first.Server.CloudID || replayed.SSH.Host != first.SSH.Host || replayed.SSH.Port != first.SSH.Port {
		t.Fatalf("replayed target=%#v, original=%#v", replayed, first)
	}
	if creates := localContainerCreateCalls(runner); creates != 1 {
		t.Fatalf("container creates=%d, want 1", creates)
	}
	// Fixed acquisition, replay, and Touch are not presentation requests. Neither
	// the persisted intent nor the runtime actuator may gain diagnostic fields.
	for _, value := range []any{claim, replayed, runner.calls} {
		data, err := json.Marshal(value)
		if err != nil || strings.Contains(string(data), memoryDiagnosticPrefix) {
			t.Fatalf("fixed authority contains presentation diagnostics: %v", err)
		}
	}
	for _, call := range runner.calls {
		if strings.Contains(strings.Join(call.Args, " "), "MemTotal") {
			t.Fatal("fixed lifecycle acquired a capacity probe")
		}
	}
	if err := restarted.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: replayed}); err != nil {
		t.Fatal(err)
	}
}

func TestFixedAcquireRejectsChangedImage(t *testing.T) {
	b, runner, _, _, _, _ := pendingAcquireBackend(t)
	b.waitForSSHReady = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error { return nil }
	req := core.AcquireRequest{
		Repo: core.Repo{Root: t.TempDir()}, Keep: true,
		RequestedLeaseID: "cbx_abcdef123452", RequestedSlug: "pending-test",
	}
	lease, err := b.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	drifted := testBackend(runner)
	drifted.cfg.LocalContainer.Image = "debian:bookworm"
	drifted.waitForSSHReady = b.waitForSSHReady
	_, err = drifted.Acquire(context.Background(), req)
	requireFixedLocalContainerConflict(t, err)
	if creates := localContainerCreateCalls(runner); creates != 1 {
		t.Fatalf("container creates after image drift=%d, want 1", creates)
	}
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
}

func TestFixedAcquirePondReplayAndDrift(t *testing.T) {
	for _, test := range []struct {
		name     string
		original string
		replayed string
		conflict bool
	}{
		{name: "equivalent spelling", original: "  Alpha__Pond  ", replayed: "alpha-pond"},
		{name: "different pond", original: "alpha", replayed: "beta", conflict: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			b, runner, _, _, _, _ := pendingAcquireBackend(t)
			b.cfg.Pond = test.original
			b.waitForSSHReady = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error { return nil }
			req := core.AcquireRequest{
				Repo: core.Repo{Root: t.TempDir()}, Keep: true,
				RequestedLeaseID: "cbx_abcdef123466", RequestedSlug: "fixed-pond",
			}
			lease, err := b.Acquire(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			if pond := lease.Server.Labels["pond"]; pond != core.NormalizePondName(test.original) {
				t.Fatalf("acquired pond=%q, want %q", pond, core.NormalizePondName(test.original))
			}
			restarted := testBackend(runner)
			restarted.cfg.Pond = test.replayed
			restarted.waitForSSHReady = b.waitForSSHReady
			replayed, err := restarted.Acquire(context.Background(), req)
			releaseLease := lease
			if test.conflict {
				requireFixedLocalContainerConflict(t, err)
			} else if err != nil || replayed.LeaseID != lease.LeaseID || replayed.Server.CloudID != lease.Server.CloudID {
				t.Fatalf("equivalent pond replay=%#v err=%v", replayed, err)
			} else {
				releaseLease = replayed
			}
			if creates := localContainerCreateCalls(runner); creates != 1 {
				t.Fatalf("container creates after pond replay=%d, want 1", creates)
			}
			if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: releaseLease}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestValidateFixedLocalContainerRejectsMismatchedPond(t *testing.T) {
	b, _, _, _, _, _ := pendingAcquireBackend(t)
	b.cfg.Pond = " Alpha "
	b.waitForSSHReady = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error { return nil }
	req := core.AcquireRequest{
		Repo: core.Repo{Root: t.TempDir()}, Keep: true,
		RequestedLeaseID: "cbx_abcdef123467", RequestedSlug: "fixed-pond-validation",
	}
	lease, err := b.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := core.ReadLeaseClaim(req.RequestedLeaseID)
	if err != nil {
		t.Fatal(err)
	}
	container, err := b.inspectContainer(context.Background(), lease.Server.CloudID)
	if err != nil {
		t.Fatal(err)
	}
	for _, pond := range []string{"beta", ""} {
		container.Config.Labels["pond"] = pond
		err = validateFixedLocalContainer(container, b.configForRun(), req.RequestedLeaseID,
			req.RequestedSlug, claim.FixedCreateIntent.Fingerprint)
		requireFixedLocalContainerConflict(t, err)
	}
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
}

func TestFixedAcquireRejectsSubsecondLifecycleDrift(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*core.Config)
	}{
		{name: "ttl", mutate: func(cfg *core.Config) { cfg.TTL += time.Nanosecond }},
		{name: "idle timeout", mutate: func(cfg *core.Config) { cfg.IdleTimeout += time.Nanosecond }},
	} {
		t.Run(test.name, func(t *testing.T) {
			b, runner, _, _, _, _ := pendingAcquireBackend(t)
			b.cfg.TTL = 90*time.Second + 100*time.Millisecond
			b.cfg.IdleTimeout = 30*time.Second + 100*time.Millisecond
			b.waitForSSHReady = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error { return nil }
			req := core.AcquireRequest{
				Repo: core.Repo{Root: t.TempDir()}, Keep: true,
				RequestedLeaseID: "cbx_abcdef123462", RequestedSlug: "precise-duration",
			}
			lease, err := b.Acquire(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			drifted := testBackend(runner)
			drifted.cfg.TTL = b.cfg.TTL
			drifted.cfg.IdleTimeout = b.cfg.IdleTimeout
			test.mutate(&drifted.cfg)
			drifted.waitForSSHReady = b.waitForSSHReady
			_, err = drifted.Acquire(context.Background(), req)
			requireFixedLocalContainerConflict(t, err)
			if creates := localContainerCreateCalls(runner); creates != 1 {
				t.Fatalf("container creates after %s drift=%d, want 1", test.name, creates)
			}
			if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestFixedAcquireReleasedClaimRemainsTerminal(t *testing.T) {
	b, runner, _, _, _, _ := pendingAcquireBackend(t)
	b.waitForSSHReady = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error { return nil }
	req := core.AcquireRequest{
		Repo: core.Repo{Root: t.TempDir()}, Keep: true,
		RequestedLeaseID: "cbx_abcdef123453", RequestedSlug: "pending-test",
	}
	lease, err := b.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if outcome, err := b.ReleaseLeaseWithOutcome(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil || !outcome.Terminal {
		t.Fatalf("fixed deletion outcome=%+v err=%v", outcome, err)
	}
	claim, exists, err := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
	if err != nil || !exists || claim.Provider != core.FixedLocalContainerClaimProvider ||
		claim.FixedCreateIntent == nil || claim.FixedCreateIntent.State != "released" {
		t.Fatalf("terminal fixed claim=%#v exists=%t err=%v", claim, exists, err)
	}
	if claim.CloudID != "" || len(claim.Labels) != 0 || claim.SSHHost != "" {
		t.Fatalf("terminal fixed claim retains live identity: %#v", claim)
	}
	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if retained, exists, err := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID); err != nil ||
		!exists || retained.Provider != core.FixedLocalContainerClaimProvider {
		t.Fatalf("cleanup altered terminal fixed claim: claim=%#v exists=%t err=%v", retained, exists, err)
	}
	_, err = b.Acquire(context.Background(), req)
	requireFixedLocalContainerConflict(t, err)
	if !strings.Contains(err.Error(), "terminal") {
		t.Fatalf("terminal replay error=%v", err)
	}
	if creates := localContainerCreateCalls(runner); creates != 1 {
		t.Fatalf("container creates after terminal replay=%d, want 1", creates)
	}
}

func TestReleasedFixedLeaseTombstoneDoesNotReserveSlugRouting(t *testing.T) {
	b, _, _, _, _, _ := pendingAcquireBackend(t)
	b.waitForSSHReady = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error { return nil }
	req := core.AcquireRequest{
		Repo: core.Repo{Root: t.TempDir()}, Keep: true,
		RequestedLeaseID: "cbx_abcdef123460", RequestedSlug: "reusable-local-slug",
	}
	lease, err := b.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if claim, found, lookupErr := localContainerClaimByIDOrSlug(req.RequestedSlug); lookupErr != nil || found {
		t.Fatalf("released fixed slug remained routable: claim=%#v found=%t err=%v", claim, found, lookupErr)
	}
	if exact, found, lookupErr := localContainerClaimByIDOrSlug(req.RequestedLeaseID); lookupErr != nil ||
		!found || !isReleasedFixedLocalContainerClaim(exact) {
		t.Fatalf("released fixed ID disappeared: claim=%#v found=%t err=%v", exact, found, lookupErr)
	}
	if _, _, _, resolveErr := b.resolveContainer(context.Background(), req.RequestedSlug); !isExitCode(resolveErr, 4) {
		t.Fatalf("released fixed slug resolved without a live container: %v", resolveErr)
	}

	const ordinaryLeaseID = "cbx_abcdef123461"
	const ordinaryContainerID = "reused-local-container"
	labels := testCapturedScopeLabels(map[string]string{
		"crabbox": "true", "provider": providerName, "lease": ordinaryLeaseID,
		"slug": req.RequestedSlug, "state": "ready", "runtime": "docker",
		"server_type": "ubuntu:24.04", "ssh_user": "runner", "work_root": "/workspace/crabbox",
	})
	server := core.Server{CloudID: ordinaryContainerID, Provider: providerName, Labels: labels}
	if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(
		ordinaryLeaseID, req.RequestedSlug, providerName, "runtime:docker/context:default", "",
		req.Repo.Root, time.Minute, false, server,
		core.SSHTarget{Host: "127.0.0.1", Port: "49170", User: "runner"},
	); err != nil {
		t.Fatal(err)
	}
	if claim, found, lookupErr := localContainerClaimByIDOrSlug(req.RequestedSlug); lookupErr != nil ||
		!found || claim.LeaseID != ordinaryLeaseID || claim.Provider != providerName {
		t.Fatalf("reused ordinary slug claim=%#v found=%t err=%v", claim, found, lookupErr)
	}
	container := inspectContainer{
		ID: ordinaryContainerID, Name: "/crabbox-reused-local-slug",
		Config: inspectConfig{Image: "ubuntu:24.04", Labels: labels},
		State:  inspectState{Status: "running", Running: true},
	}
	data, err := json.Marshal([]inspectContainer{container})
	if err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
		commandKey([]string{"ps", "-a", "--filter", "label=crabbox=true", "--filter", "label=provider=local-container", "--format", "{{.ID}}"}): {Stdout: ordinaryContainerID + "\n"},
		commandKey([]string{"inspect", ordinaryContainerID}): {Stdout: string(data)},
	}}
	addDefaultLocalContainerScopeResponses(runner)
	resolved, resolvedLeaseID, resolvedSlug, resolveErr := testBackend(runner).resolveContainer(context.Background(), req.RequestedSlug)
	if resolveErr != nil || resolved.ID != ordinaryContainerID || resolvedLeaseID != ordinaryLeaseID || resolvedSlug != req.RequestedSlug {
		t.Fatalf("reused slug container=%#v lease=%q slug=%q err=%v", resolved, resolvedLeaseID, resolvedSlug, resolveErr)
	}
	if tombstone, exists, readErr := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID); readErr != nil ||
		!exists || !isReleasedFixedLocalContainerClaim(tombstone) {
		t.Fatalf("reused slug altered fixed tombstone: claim=%#v exists=%t err=%v", tombstone, exists, readErr)
	}
}

func TestFixedAcquireRecoversPreparedLiveContainer(t *testing.T) {
	b, runner, _, _, _, _ := pendingAcquireBackend(t)
	sshErr := errors.New("temporary SSH transport failure")
	b.waitForSSHReady = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error {
		return sshErr
	}
	req := core.AcquireRequest{
		Repo: core.Repo{Root: t.TempDir()}, Keep: true,
		RequestedLeaseID: "cbx_abcdef123454", RequestedSlug: "pending-test",
	}
	if _, err := b.Acquire(context.Background(), req); !errors.Is(err, sshErr) {
		t.Fatalf("initial fixed acquisition error=%v, want temporary SSH transport failure", err)
	}
	claim, err := core.ReadLeaseClaim(req.RequestedLeaseID)
	if err != nil || claim.Provider != core.FixedLocalContainerClaimProvider ||
		claim.FixedCreateIntent == nil || claim.FixedCreateIntent.State != "prepared" || claim.CloudID == "" ||
		!isPendingLocalContainerClaim(claim) {
		t.Fatalf("prepared fixed claim=%#v err=%v", claim, err)
	}
	if pending, found, lookupErr := localContainerClaimByIDOrSlug(req.RequestedSlug); lookupErr != nil ||
		!found || pending.LeaseID != req.RequestedLeaseID {
		t.Fatalf("prepared fixed slug claim=%#v found=%t err=%v", pending, found, lookupErr)
	}
	if container, leaseID, _, resolveErr := b.resolveContainer(context.Background(), req.RequestedSlug); resolveErr != nil ||
		container.ID != claim.CloudID || leaseID != req.RequestedLeaseID {
		t.Fatalf("prepared fixed slug container=%#v lease=%q err=%v", container, leaseID, resolveErr)
	}
	b.waitForSSHReady = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error { return nil }
	lease, err := b.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if creates := localContainerCreateCalls(runner); creates != 1 {
		t.Fatalf("container creates after prepared recovery=%d, want 1", creates)
	}
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
}

func TestFixedAcquireRejectsMissingBoundContainer(t *testing.T) {
	b, runner, _, _, bootstrapDir, containerPresent := pendingAcquireBackend(t)
	b.waitForSSHReady = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error { return nil }
	req := core.AcquireRequest{
		Repo: core.Repo{Root: t.TempDir()}, Keep: true,
		RequestedLeaseID: "cbx_abcdef123455", RequestedSlug: "pending-test",
	}
	if _, err := b.Acquire(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(*bootstrapDir) })
	*containerPresent = false
	_, err := b.Acquire(context.Background(), req)
	requireFixedLocalContainerConflict(t, err)
	if !strings.Contains(err.Error(), "missing its bound container") {
		t.Fatalf("missing container error=%v", err)
	}
	if creates := localContainerCreateCalls(runner); creates != 1 {
		t.Fatalf("container creates after bound container vanished=%d, want 1", creates)
	}
}

func TestFixedAcquireMissingContainerReleasePreservesTerminalClaim(t *testing.T) {
	b, _, _, _, _, containerPresent := pendingAcquireBackend(t)
	b.waitForSSHReady = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error { return nil }
	req := core.AcquireRequest{
		Repo: core.Repo{Root: t.TempDir()}, Keep: true,
		RequestedLeaseID: "cbx_abcdef123459", RequestedSlug: "pending-test",
	}
	if _, err := b.Acquire(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	*containerPresent = false
	lease, err := b.Resolve(context.Background(), core.ResolveRequest{ID: req.RequestedLeaseID, ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if outcome, err := b.ReleaseLeaseWithOutcome(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil || !outcome.Terminal {
		t.Fatalf("terminal release outcome=%+v err=%v", outcome, err)
	}
	claim, exists, err := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
	if err != nil || !exists || claim.Provider != core.FixedLocalContainerClaimProvider ||
		claim.FixedCreateIntent == nil || claim.FixedCreateIntent.State != "released" {
		t.Fatalf("missing-container terminal claim=%#v exists=%t err=%v", claim, exists, err)
	}
	_, err = b.Acquire(context.Background(), req)
	requireFixedLocalContainerConflict(t, err)
}

func TestFixedAcquireRecreatesUnattemptedPreparedClaim(t *testing.T) {
	b, runner, _, _, bootstrapDir, containerPresent := pendingAcquireBackend(t)
	b.waitForSSHReady = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error { return nil }
	req := core.AcquireRequest{
		Repo: core.Repo{Root: t.TempDir()}, Keep: true,
		RequestedLeaseID: "cbx_abcdef123456", RequestedSlug: "pending-test",
	}
	if _, err := b.Acquire(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	firstBootstrapDir := *bootstrapDir
	t.Cleanup(func() { _ = os.RemoveAll(firstBootstrapDir) })
	*containerPresent = false
	if err := core.WithDurableLeaseClaimLock(req.RequestedLeaseID, func(claim *core.LeaseClaim, exists bool, persist func() error) error {
		if !exists {
			return errors.New("fixed claim disappeared")
		}
		claim.CloudID = ""
		claim.CloudImmutableID = ""
		claim.Labels = nil
		claim.SSHHost = ""
		claim.SSHPort = 0
		claim.FixedCreateIntent.State = "prepared"
		claim.FixedCreateIntent.Attempt = nil
		return persist()
	}); err != nil {
		t.Fatal(err)
	}
	lease, err := b.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if creates := localContainerCreateCalls(runner); creates != 2 {
		t.Fatalf("container creates after unattempted prepared recovery=%d, want 2", creates)
	}
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
}

func TestFixedAcquireRefusesUnresolvedCreateAttempt(t *testing.T) {
	b, runner, _, _, _, _ := pendingAcquireBackend(t)
	originalRun := runner.run
	runner.run = func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		if firstArg(req.Args) == "run" {
			return core.LocalCommandResult{Stderr: "daemon transport lost", ExitCode: 1}, errors.New("daemon transport lost")
		}
		return originalRun(req)
	}
	req := core.AcquireRequest{
		Repo: core.Repo{Root: t.TempDir()}, Keep: true,
		RequestedLeaseID: "cbx_abcdef123457", RequestedSlug: "pending-test",
	}
	if _, err := b.Acquire(context.Background(), req); err == nil {
		t.Fatal("ambiguous fixed create unexpectedly succeeded")
	}
	claim, err := core.ReadLeaseClaim(req.RequestedLeaseID)
	if err != nil || claim.FixedCreateIntent == nil || claim.FixedCreateIntent.Attempt["container_name"] == "" || claim.CloudID != "" {
		t.Fatalf("unresolved create claim=%#v err=%v", claim, err)
	}
	_, err = b.Acquire(context.Background(), req)
	requireFixedLocalContainerConflict(t, err)
	if !strings.Contains(err.Error(), "unresolved create attempt") {
		t.Fatalf("unresolved create error=%v", err)
	}
	if creates := localContainerCreateCalls(runner); creates != 1 {
		t.Fatalf("container creates after ambiguous failure=%d, want 1", creates)
	}
}

func TestFixedAcquireBindsStoppedRecoveredContainerForExplicitRelease(t *testing.T) {
	b, runner, containerPresent := stoppedFixedAcquireBackend(t, nil)
	req := core.AcquireRequest{
		Repo: core.Repo{Root: t.TempDir()}, Keep: true,
		RequestedLeaseID: "cbx_abcdef123468", RequestedSlug: "stopped-recovery",
	}
	if _, err := b.Acquire(context.Background(), req); err == nil {
		t.Fatal("ambiguous stopped-container creation unexpectedly succeeded")
	}
	initial, err := core.ReadLeaseClaim(req.RequestedLeaseID)
	if err != nil || initial.Provider != core.FixedLocalContainerClaimProvider ||
		initial.FixedCreateIntent == nil || initial.FixedCreateIntent.State != "prepared" ||
		initial.FixedCreateIntent.Attempt["container_name"] != core.LeaseProviderName(req.RequestedLeaseID, req.RequestedSlug) ||
		initial.FixedCreateIntent.Attempt["container_id"] != "" || initial.CloudID != "" {
		t.Fatalf("unresolved stopped-container claim=%#v err=%v", initial, err)
	}
	if !*containerPresent {
		t.Fatal("ambiguous creation did not retain its stopped container")
	}
	_, err = b.Acquire(context.Background(), req)
	requireFixedLocalContainerConflict(t, err)
	if !strings.Contains(err.Error(), "non-running container") {
		t.Fatalf("stopped-container replay error=%v", err)
	}
	bound, err := core.ReadLeaseClaim(req.RequestedLeaseID)
	if err != nil || bound.Provider != core.FixedLocalContainerClaimProvider ||
		bound.FixedCreateIntent == nil || bound.FixedCreateIntent.State != "prepared" ||
		bound.CloudID != "container-pending" || bound.FixedCreateIntent.Attempt["container_id"] != bound.CloudID ||
		bound.Labels["fixed_intent_sha256"] != bound.FixedCreateIntent.Fingerprint ||
		!isPendingLocalContainerClaim(bound) {
		t.Fatalf("recovered stopped-container claim=%#v err=%v", bound, err)
	}
	if !*containerPresent {
		t.Fatal("replay unexpectedly removed the stopped container")
	}
	if creates := localContainerCreateCalls(runner); creates != 1 {
		t.Fatalf("container creates after stopped recovery=%d, want 1", creates)
	}
	lease, err := b.Resolve(context.Background(), core.ResolveRequest{ID: req.RequestedLeaseID, ReleaseOnly: true})
	if err != nil || lease.LeaseID != req.RequestedLeaseID || lease.Server.CloudID != bound.CloudID {
		t.Fatalf("exact stopped-container release target=%#v err=%v", lease, err)
	}
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatalf("release stopped fixed container: %v", err)
	}
	if *containerPresent {
		t.Fatal("explicit release did not remove stopped container")
	}
	tombstone, exists, err := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
	if err != nil || !exists || fixedLocalContainerLeaseKind.ValidateTerminalClaim(tombstone, bound, req.RequestedLeaseID, nil) != nil {
		t.Fatalf("stopped-container terminal claim=%#v exists=%t err=%v", tombstone, exists, err)
	}
	_, err = b.Acquire(context.Background(), req)
	requireFixedLocalContainerConflict(t, err)
	if !strings.Contains(err.Error(), "terminal") {
		t.Fatalf("terminal stopped-container replay error=%v", err)
	}
	if creates := localContainerCreateCalls(runner); creates != 1 {
		t.Fatalf("container creates after terminal replay=%d, want 1", creates)
	}
}

func TestFixedAcquireStoppedContainerNeverBindsMismatchedIdentity(t *testing.T) {
	for _, test := range []struct {
		name            string
		mutateContainer func(*inspectContainer)
		mutateAttempt   bool
	}{
		{name: "fingerprint", mutateContainer: func(container *inspectContainer) {
			container.Config.Labels["fixed_intent_sha256"] = "different-fingerprint"
		}},
		{name: "durable attempt identity", mutateAttempt: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			b, runner, containerPresent := stoppedFixedAcquireBackend(t, test.mutateContainer)
			req := core.AcquireRequest{
				Repo: core.Repo{Root: t.TempDir()}, Keep: true,
				RequestedLeaseID: "cbx_abcdef123469", RequestedSlug: "stopped-mismatch",
			}
			if _, err := b.Acquire(context.Background(), req); err == nil {
				t.Fatal("ambiguous stopped-container creation unexpectedly succeeded")
			}
			if test.mutateAttempt {
				if err := core.WithDurableLeaseClaimLock(req.RequestedLeaseID, func(claim *core.LeaseClaim, exists bool, persist func() error) error {
					if !exists {
						return errors.New("fixed claim disappeared")
					}
					claim.FixedCreateIntent.Attempt["container_id"] = "different-container"
					return persist()
				}); err != nil {
					t.Fatal(err)
				}
			}
			_, err := b.Acquire(context.Background(), req)
			requireFixedLocalContainerConflict(t, err)
			if test.mutateAttempt {
				if !errors.Is(err, errContainerIdentityMismatch) {
					t.Fatalf("stopped-container replay error=%v, want durable container identity mismatch", err)
				}
			} else if errors.Is(err, errContainerIdentityMismatch) || !strings.Contains(err.Error(), "does not match its fixed create intent") {
				t.Fatalf("stopped-container replay error=%v, want fixed create intent mismatch", err)
			}
			claim, err := core.ReadLeaseClaim(req.RequestedLeaseID)
			if err != nil || claim.CloudID != "" || len(claim.Labels) != 0 ||
				(test.mutateAttempt && claim.FixedCreateIntent.Attempt["container_id"] != "different-container") {
				t.Fatalf("mismatched stopped container was bound: claim=%#v err=%v", claim, err)
			}
			if !*containerPresent {
				t.Fatal("mismatched container was unexpectedly removed")
			}
			if creates := localContainerCreateCalls(runner); creates != 1 {
				t.Fatalf("container creates after mismatched recovery=%d, want 1", creates)
			}
		})
	}
}

func stoppedFixedAcquireBackend(t *testing.T, mutateContainer func(*inspectContainer)) (*backend, *recordingRunner, *bool) {
	t.Helper()
	b, runner, _, _, bootstrapDir, containerPresent := pendingAcquireBackend(t)
	t.Cleanup(func() {
		if *bootstrapDir != "" {
			_ = os.RemoveAll(*bootstrapDir)
		}
	})
	b.waitForSSHReady = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error {
		t.Fatal("stopped container was incorrectly adopted as ready")
		return nil
	}
	originalRun := runner.run
	runner.run = func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		result, err := originalRun(req)
		if err != nil {
			return result, err
		}
		switch firstArg(req.Args) {
		case "run":
			return core.LocalCommandResult{Stderr: "daemon transport lost", ExitCode: 1}, errors.New("daemon transport lost")
		case "inspect":
			var containers []inspectContainer
			if err := json.Unmarshal([]byte(result.Stdout), &containers); err != nil {
				t.Fatal(err)
			}
			containers[0].State = inspectState{Status: "exited", Running: false}
			if mutateContainer != nil {
				mutateContainer(&containers[0])
			}
			data, marshalErr := json.Marshal(containers)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			result.Stdout = string(data)
		}
		return result, nil
	}
	return b, runner, containerPresent
}

func TestFixedAcquireUnresolvedAttemptReservesRequestedSlug(t *testing.T) {
	b, runner, _, _, _, _ := pendingAcquireBackend(t)
	b.waitForSSHReady = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error { return nil }
	originalRun := runner.run
	firstAttempt := true
	runner.run = func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		if firstArg(req.Args) == "run" && firstAttempt {
			firstAttempt = false
			return core.LocalCommandResult{Stderr: "daemon transport lost", ExitCode: 1}, errors.New("daemon transport lost")
		}
		return originalRun(req)
	}
	first := core.AcquireRequest{
		Repo: core.Repo{Root: t.TempDir()}, Keep: true,
		RequestedLeaseID: "cbx_abcdef123463", RequestedSlug: "reserved-local-slug",
	}
	if _, err := b.Acquire(context.Background(), first); err == nil {
		t.Fatal("ambiguous fixed create unexpectedly succeeded")
	}
	reserved, err := core.ReadLeaseClaim(first.RequestedLeaseID)
	if err != nil || reserved.FixedCreateIntent == nil || reserved.FixedCreateIntent.State != "prepared" ||
		reserved.FixedCreateIntent.Attempt["container_name"] == "" || reserved.CloudID != "" {
		t.Fatalf("unresolved fixed claim=%#v err=%v", reserved, err)
	}
	second := first
	second.RequestedLeaseID = "cbx_abcdef123464"
	lease, err := b.Acquire(context.Background(), second)
	if err != nil {
		t.Fatal(err)
	}
	slug := lease.Server.Labels["slug"]
	if slug == first.RequestedSlug || !strings.HasPrefix(slug, first.RequestedSlug+"-") {
		t.Fatalf("second lease slug=%q, want collision suffix for reserved %q", slug, first.RequestedSlug)
	}
	if claim, found, lookupErr := localContainerClaimByIDOrSlug(first.RequestedSlug); lookupErr != nil ||
		!found || claim.LeaseID != first.RequestedLeaseID {
		t.Fatalf("reserved slug claim=%#v found=%t err=%v", claim, found, lookupErr)
	}
	if resolved, resolveErr := b.Resolve(context.Background(), core.ResolveRequest{ID: slug, StatusOnly: true}); resolveErr != nil ||
		resolved.LeaseID != second.RequestedLeaseID {
		t.Fatalf("suffixed slug resolved=%#v err=%v", resolved, resolveErr)
	}
	if creates := localContainerCreateCalls(runner); creates != 2 {
		t.Fatalf("container create attempts=%d, want 2", creates)
	}
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
}

func TestFixedAcquireOnAcquiredCanReadClaimAfterLockReleased(t *testing.T) {
	b, _, _, _, _, _ := pendingAcquireBackend(t)
	b.waitForSSHReady = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error { return nil }
	req := core.AcquireRequest{
		Repo: core.Repo{Root: t.TempDir()}, Keep: true,
		RequestedLeaseID: "cbx_abcdef123458", RequestedSlug: "pending-test",
	}
	called := false
	req.OnAcquired = func(lease core.LeaseTarget) error {
		called = true
		return core.WithDurableLeaseClaimLock(lease.LeaseID, func(claim *core.LeaseClaim, exists bool, _ func() error) error {
			if !exists || claim.CloudID != lease.Server.CloudID || claim.FixedCreateIntent.State != "acquired" {
				return errors.New("fixed claim was not committed before acquisition callback")
			}
			return nil
		})
	}
	lease, err := b.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("acquisition callback was not called")
	}
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
}

func TestNonFixedAcquireStillCreatesDistinctLeases(t *testing.T) {
	b, _, _, _, _, _ := pendingAcquireBackend(t)
	b.waitForSSHReady = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error { return nil }
	lease, err := b.Acquire(context.Background(), core.AcquireRequest{Keep: true, Repo: core.Repo{Root: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := core.ReadLeaseClaim(lease.LeaseID)
	if err != nil || claim.Provider != providerName || claim.FixedCreateIntent != nil ||
		lease.Server.Labels["fixed_intent_sha256"] != "" {
		t.Fatalf("non-fixed acquisition changed: lease=%#v claim=%#v err=%v", lease, claim, err)
	}
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(lease.LeaseID); err != nil || exists {
		t.Fatalf("non-fixed release retained a claim: exists=%t err=%v", exists, err)
	}
}

func localContainerCreateCalls(runner *recordingRunner) int {
	count := 0
	for _, call := range runner.calls {
		if firstArg(call.Args) == "run" {
			count++
		}
	}
	return count
}

func requireFixedLocalContainerConflict(t *testing.T, err error) {
	t.Helper()
	var exitErr core.ExitError
	if err == nil || !core.AsExitError(err, &exitErr) || exitErr.Code != 4 || !strings.Contains(err.Error(), "lease_id_conflict") {
		t.Fatalf("fixed lease error=%v, want lease_id_conflict exit 4", err)
	}
}

func TestRunFailureEvidenceUsesPerRunOOMKillIncrement(t *testing.T) {
	tests := []struct {
		name              string
		counts            []string
		baselineOOMKilled bool
		expected          core.ResourceExhaustionReason
	}{
		{name: "increment", counts: []string{"oom 3\noom_kill 4\n", "oom 4\noom_kill 5\n"}, expected: core.ResourceExhaustionMemory},
		{name: "no increment", counts: []string{"oom 3\noom_kill 4\n", "oom 3\noom_kill 4\n"}},
		{name: "counter reset", counts: []string{"oom 8\noom_kill 9\n", "oom 0\noom_kill 0\n"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			index := 0
			runner := &recordingRunner{run: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
				if req.Args[0] == "inspect" {
					return core.LocalCommandResult{Stdout: fmt.Sprintf(`[{"Id":"container-123","State":{"Running":true,"OOMKilled":%t}}]`, tt.baselineOOMKilled)}, nil
				}
				if len(req.Args) != 4 || req.Args[0] != "exec" || req.Args[1] != "container-123" || req.Args[2] != "cat" || req.Args[3] != cgroupOOMCounterPaths[0] {
					t.Fatalf("unexpected runtime request: %#v", req.Args)
				}
				if index >= len(tt.counts) {
					t.Fatalf("unexpected extra OOM counter read")
				}
				result := core.LocalCommandResult{Stdout: tt.counts[index]}
				index++
				return result, nil
			}}
			b := testBackend(runner)
			core.MarkLocalContainerRuntimeExplicit(&b.cfg)
			collector, err := b.BeginRunFailureEvidence(context.Background(), core.RunFailureEvidenceRequest{
				Lease: core.LeaseTarget{Server: core.Server{CloudID: "container-123"}},
			})
			if err != nil {
				t.Fatal(err)
			}
			evidence, err := collector(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if evidence.ResourceExhaustion != tt.expected {
				t.Fatalf("resource exhaustion=%q, want %q", evidence.ResourceExhaustion, tt.expected)
			}
			if index != 2 {
				t.Fatalf("counter reads=%d, want 2", index)
			}
		})
	}
}

func TestRunFailureEvidenceReusedLeaseGetsFreshBaseline(t *testing.T) {
	counts := []string{
		"oom_kill 4\n",
		"oom_kill 5\n",
		"oom_kill 5\n",
		"oom_kill 5\n",
	}
	index := 0
	runner := &recordingRunner{run: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		if req.Args[0] == "inspect" {
			return core.LocalCommandResult{Stdout: `[{"Id":"container-reused","State":{"Running":true,"OOMKilled":false}}]`}, nil
		}
		if req.Args[0] != "exec" || req.Args[1] != "container-reused" {
			t.Fatalf("unexpected runtime request: %#v", req.Args)
		}
		result := core.LocalCommandResult{Stdout: counts[index]}
		index++
		return result, nil
	}}
	b := testBackend(runner)
	core.MarkLocalContainerRuntimeExplicit(&b.cfg)
	req := core.RunFailureEvidenceRequest{Lease: core.LeaseTarget{Server: core.Server{CloudID: "container-reused"}}}

	first, err := b.BeginRunFailureEvidence(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	firstEvidence, err := first(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if firstEvidence.ResourceExhaustion != core.ResourceExhaustionMemory {
		t.Fatalf("first run evidence=%#v, want memory exhaustion", firstEvidence)
	}

	second, err := b.BeginRunFailureEvidence(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	secondEvidence, err := second(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if secondEvidence.ResourceExhaustion != "" {
		t.Fatalf("historical OOM misclassified reused run: %#v", secondEvidence)
	}
}

func TestRunFailureEvidenceReadErrorsRemainDiagnostic(t *testing.T) {
	t.Run("baseline", func(t *testing.T) {
		runner := &recordingRunner{run: func(core.LocalCommandRequest) (core.LocalCommandResult, error) {
			return core.LocalCommandResult{ExitCode: 1}, errors.New("counter unavailable")
		}}
		b := testBackend(runner)
		core.MarkLocalContainerRuntimeExplicit(&b.cfg)
		collector, err := b.BeginRunFailureEvidence(context.Background(), core.RunFailureEvidenceRequest{
			Lease: core.LeaseTarget{Server: core.Server{CloudID: "container-123"}},
		})
		if err == nil || collector != nil {
			t.Fatalf("collector=%v err=%v, want baseline error", collector != nil, err)
		}
	})

	t.Run("collection", func(t *testing.T) {
		reads := 0
		runner := &recordingRunner{run: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
			if req.Args[0] == "inspect" && reads == 1 {
				return core.LocalCommandResult{Stdout: `[{"Id":"container-123","State":{"Running":true,"OOMKilled":false}}]`}, nil
			}
			reads++
			if reads == 1 {
				return core.LocalCommandResult{Stdout: "oom_kill 2\n"}, nil
			}
			return core.LocalCommandResult{ExitCode: 1}, errors.New("counter unavailable")
		}}
		b := testBackend(runner)
		core.MarkLocalContainerRuntimeExplicit(&b.cfg)
		collector, err := b.BeginRunFailureEvidence(context.Background(), core.RunFailureEvidenceRequest{
			Lease: core.LeaseTarget{Server: core.Server{CloudID: "container-123"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if evidence, err := collector(context.Background()); err == nil || evidence.ResourceExhaustion != "" {
			t.Fatalf("evidence=%#v err=%v, want collection error without classification", evidence, err)
		}
	})
}

func TestRunFailureEvidenceUsesContainerOOMStateWhenCounterBecomesUnreadable(t *testing.T) {
	reads := 0
	inspects := 0
	runner := &recordingRunner{run: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		if req.Args[0] == "exec" {
			reads++
			if reads == 1 {
				return core.LocalCommandResult{Stdout: "oom_kill 0\n"}, nil
			}
			return core.LocalCommandResult{ExitCode: 128}, errors.New("container is not running")
		}
		if req.Args[0] == "inspect" {
			inspects++
			oomKilled := inspects > 1
			return core.LocalCommandResult{Stdout: fmt.Sprintf(`[{"Id":"container-123","State":{"Status":"exited","Running":false,"OOMKilled":%t}}]`, oomKilled)}, nil
		}
		t.Fatalf("unexpected runtime request: %#v", req.Args)
		return core.LocalCommandResult{}, nil
	}}
	b := testBackend(runner)
	core.MarkLocalContainerRuntimeExplicit(&b.cfg)
	collector, err := b.BeginRunFailureEvidence(context.Background(), core.RunFailureEvidenceRequest{
		Lease: core.LeaseTarget{Server: core.Server{CloudID: "container-123"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := collector(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if evidence.ResourceExhaustion != core.ResourceExhaustionMemory {
		t.Fatalf("evidence=%#v, want memory exhaustion", evidence)
	}
}

func TestRunFailureEvidenceDoesNotReuseHistoricalContainerOOMState(t *testing.T) {
	reads := 0
	runner := &recordingRunner{run: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		if req.Args[0] == "exec" {
			reads++
			if reads == 1 {
				return core.LocalCommandResult{Stdout: "oom_kill 3\n"}, nil
			}
			return core.LocalCommandResult{ExitCode: 128}, errors.New("container is not running")
		}
		if req.Args[0] == "inspect" {
			return core.LocalCommandResult{Stdout: `[{"Id":"container-123","State":{"Status":"running","Running":true,"OOMKilled":true}}]`}, nil
		}
		t.Fatalf("unexpected runtime request: %#v", req.Args)
		return core.LocalCommandResult{}, nil
	}}
	b := testBackend(runner)
	core.MarkLocalContainerRuntimeExplicit(&b.cfg)
	collector, err := b.BeginRunFailureEvidence(context.Background(), core.RunFailureEvidenceRequest{
		Lease: core.LeaseTarget{Server: core.Server{CloudID: "container-123"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := collector(context.Background())
	if err == nil {
		t.Fatal("unreadable post-run counter should remain a diagnostic error")
	}
	if evidence.ResourceExhaustion != "" {
		t.Fatalf("historical container OOM state was reused: %#v", evidence)
	}
}

func TestReadOOMKillCountFallsBackToCgroupV1(t *testing.T) {
	runner := &recordingRunner{run: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		switch req.Args[3] {
		case cgroupOOMCounterPaths[0]:
			return core.LocalCommandResult{ExitCode: 1}, errors.New("not found")
		case cgroupOOMCounterPaths[1]:
			return core.LocalCommandResult{Stdout: "oom_kill_disable 0\nunder_oom 0\noom_kill 7\n"}, nil
		default:
			t.Fatalf("unexpected cgroup path: %#v", req.Args)
			return core.LocalCommandResult{}, nil
		}
	}}
	b := testBackend(runner)
	core.MarkLocalContainerRuntimeExplicit(&b.cfg)
	count, err := b.readOOMKillCount(context.Background(), "container-123")
	if err != nil {
		t.Fatal(err)
	}
	if count != 7 {
		t.Fatalf("count=%d, want 7", count)
	}
}

func TestRunFailureMemoryContext(t *testing.T) {
	for _, tc := range []struct {
		name, memory, swap, kind, bytes, swapKind, hint string
	}{
		{"below", "1073741824", "2147483648", "finite", "1073741824", "finite", "recreate"},
		{"equal", "8589934592", "0", "finite", "8589934592", "default", "does not add RAM"},
		{"above", "12884901888", "-1", "finite", "12884901888", "unlimited", "does not add RAM"},
		{"unlimited", "0", "0", "unlimited", "", "default", "No finite"},
		{"missing", "null", "null", "unknown", "", "unknown", "Check"},
		{"malformed", `"12g"`, `{}`, "unknown", "", "unknown", "Check"},
		{"malformed swap only", "1073741824", `{}`, "finite", "1073741824", "unknown", "recreate"},
		{"malformed limit only", `"12g"`, "2147483648", "unknown", "", "finite", "Check"},
		{"negative", "-1", "-2", "unknown", "", "unknown", "Check"},
		{"overflow", "18446744073709551616", "18446744073709551616", "unknown", "", "unknown", "Check"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads, infos := 0, 0
			runner := &recordingRunner{run: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
				switch req.Args[0] {
				case "exec":
					reads++
					return core.LocalCommandResult{Stdout: fmt.Sprintf("oom_kill %d\n", reads)}, nil
				case "inspect":
					return core.LocalCommandResult{Stdout: fmt.Sprintf(`[{"Id":"container-123","State":{"Running":true},"HostConfig":{"Memory":%s,"MemorySwap":%s}}]`, tc.memory, tc.swap)}, nil
				case "info":
					infos++
					if req.MaxCapturedOutputBytes <= 0 || req.MaxCapturedOutputBytes > 4096 {
						t.Fatal("capacity probe must bound output at capture")
					}
					return core.LocalCommandResult{Stdout: "daemon-test\n8589934592\n"}, nil
				default:
					t.Fatalf("unexpected runtime command %v", req.Args)
					return core.LocalCommandResult{}, nil
				}
			}}
			b := testBackend(runner)
			b.cfg.LocalContainer.Memory = "24g" // Invocation config is not the retained container's setting.
			b.cfg.LocalContainer.CheckpointMetadata = testCapturedScopeLabels(nil)
			collector, err := b.BeginRunFailureEvidence(t.Context(), core.RunFailureEvidenceRequest{Lease: core.LeaseTarget{Server: core.Server{CloudID: "container-123"}}})
			if err != nil {
				t.Fatal(err)
			}
			if infos != 0 {
				t.Fatal("capacity queried before OOM")
			}
			evidence, err := collector(t.Context())
			if err != nil || evidence.ResourceExhaustion != core.ResourceExhaustionMemory {
				t.Fatalf("OOM lost: evidence=%+v err=%v", evidence, err)
			}
			data, err := json.Marshal(evidence)
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				Hint    string
				Details map[string]string
			}
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatal(err)
			}
			for key, want := range map[string]string{
				"container_memory_limit": tc.kind, "container_memory_limit_bytes": tc.bytes,
				"container_memory_swap": tc.swapKind, "runtime_memory_total_bytes": "8589934592",
				"settings_phase": "before-command", "container_id": "container-123",
			} {
				if got.Details[key] != want {
					t.Errorf("%s=%q want %q", key, got.Details[key], want)
				}
			}
			if infos != 1 || !strings.Contains(got.Hint, tc.hint) || !strings.Contains(got.Hint, "swap") || !strings.Contains(got.Hint, "parent") {
				t.Errorf("capacity calls=%d hint=%q", infos, got.Hint)
			}
			if strings.Contains(string(data), "docker-test.sock") || strings.Contains(string(data), "24g") {
				t.Fatal("private route or invocation setting leaked into evidence")
			}
		})
	}
}

func TestMemoryDiagnosticsResolveDoesNotPersist(t *testing.T) {
	leaseID, _, initial, runner := createLocalContainerTouchClaim(t, time.Minute)
	labels := cloneLabels(initial.Labels)
	labels["diagnostic.memory.stale"] = "old-observation"
	var err error
	initial, err = core.UpdateLeaseClaimLabelsIfUnchanged(leaseID, initial, labels)
	if err != nil {
		t.Fatal(err)
	}
	original := &recordingRunner{responses: runner.responses}
	infos := 0
	runner.run = func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		if req.Args[0] == "info" && strings.Contains(strings.Join(req.Args, " "), "MemTotal") {
			infos++
			return core.LocalCommandResult{Stdout: "daemon-test\n8589934592\n"}, nil
		}
		result, err := original.Run(t.Context(), req)
		if req.Args[0] == "inspect" && err == nil {
			var containers []map[string]any
			if err := json.Unmarshal([]byte(result.Stdout), &containers); err != nil {
				t.Fatal(err)
			}
			containers[0]["HostConfig"] = map[string]any{"Memory": 1073741824, "MemorySwap": 2147483648}
			data, err := json.Marshal(containers)
			if err != nil {
				t.Fatal(err)
			}
			result.Stdout = string(data)
		}
		return result, err
	}
	b := testBackend(runner)
	req := core.ResolveRequest{ID: leaseID, StatusOnly: true, NoLocalStateMutations: true}
	req.IncludeDiagnostics = true
	lease, err := b.Resolve(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Server.Labels["diagnostic.memory.container_memory_limit_bytes"] != "1073741824" || infos != 1 {
		t.Fatalf("fresh memory diagnostics missing; info calls=%d", infos)
	}
	if _, exists := lease.Server.Labels["diagnostic.memory.stale"]; exists {
		t.Fatal("later claim merge resurrected stale diagnostic")
	}
	after, err := core.ReadLeaseClaim(leaseID)
	if err != nil || !reflect.DeepEqual(after, initial) {
		t.Fatal("inspection changed stored claim")
	}
	snapshot, _, _ := core.ServerLeaseClaimSnapshot(lease.Server)
	if !reflect.DeepEqual(snapshot, initial) {
		t.Fatal("inspection changed claim snapshot")
	}
	req.IncludeDiagnostics = false
	plain, err := b.Resolve(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if infos != 1 {
		t.Fatal("non-presentation resolve queried memory")
	}
	if plain.Server.Labels["diagnostic.memory.container_memory_limit_bytes"] != "" {
		t.Fatal("fresh diagnostics escaped presentation")
	}
	// An opt-in alone must not authorize observation on ordinary or ready-probe paths.
	for _, guarded := range []core.ResolveRequest{
		{ID: leaseID, IncludeDiagnostics: true, StatusOnly: true},
		{ID: leaseID, IncludeDiagnostics: true, StatusOnly: true, NoLocalStateMutations: true, ReadyProbe: true},
		{ID: leaseID, IncludeDiagnostics: true, StatusOnly: true, NoLocalStateMutations: true, ReleaseOnly: true},
	} {
		if _, err := b.Resolve(t.Context(), guarded); err != nil {
			t.Fatal(err)
		}
		if infos != 1 {
			t.Fatal("non-presentation path acquired a capacity probe")
		}
	}
}

type memoryEvidenceCommandRunner func(context.Context, core.LocalCommandRequest) (core.LocalCommandResult, error)

func (f memoryEvidenceCommandRunner) Run(ctx context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	return f(ctx, req)
}

func TestRunFailureMemoryContextFreezesRuntime(t *testing.T) {
	for _, runtimeName := range []string{"docker", "podman"} {
		t.Run(runtimeName, func(t *testing.T) {
			scope := checkpointScope{Runtime: runtimeName, Context: "owned", Endpoint: "unix:///synthetic-owned.sock", DaemonID: "daemon-test"}
			identity := scope.DaemonID
			if runtimeName == "podman" {
				identity = "host|store|run|socket|true"
				sum := sha256.Sum256([]byte(identity))
				scope.DaemonID = fmt.Sprintf("podman-%x", sum[:16])
			}
			reads, infos := 0, 0
			runner := &recordingRunner{run: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
				if req.Name != runtimeName || len(req.Args) < 3 || req.Args[1] != "owned" {
					t.Fatal("command escaped the captured runtime/connection")
				}
				for _, value := range req.Env {
					if strings.Contains(value, "synthetic-ambient-changed") {
						t.Fatal("ambient route escaped snapshot")
					}
				}
				args := req.Args[2:]
				switch args[0] {
				case "exec":
					reads++
					return core.LocalCommandResult{Stdout: fmt.Sprintf("oom_kill %d\n", reads)}, nil
				case "inspect":
					return core.LocalCommandResult{Stdout: `[{"Id":"container-123","HostConfig":{"Memory":1024,"MemorySwap":0}}]`}, nil
				case "context":
					return core.LocalCommandResult{Stdout: scope.Endpoint}, nil
				case "system":
					return core.LocalCommandResult{Stdout: `[{"Name":"owned","URI":"unix:///synthetic-owned.sock"}]`}, nil
				case "info":
					infos++
					return core.LocalCommandResult{Stdout: identity + "\n8388608\n"}, nil
				}
				t.Fatalf("unexpected runtime command %v", args)
				return core.LocalCommandResult{}, nil
			}}
			b := testBackend(runner)
			b.cfg.LocalContainer.Runtime = runtimeName
			b.cfg.LocalContainer.CheckpointMetadata = checkpointScopeMetadata(scope)
			collector, err := b.BeginRunFailureEvidence(t.Context(), core.RunFailureEvidenceRequest{Lease: core.LeaseTarget{Server: core.Server{CloudID: "container-123"}}})
			if err != nil {
				t.Fatal(err)
			}
			b.cfg.LocalContainer.Runtime = "wrong-runtime"
			b.cfg.LocalContainer.CheckpointMetadata[checkpointMetadataContext] = "wrong-context"
			t.Setenv("DOCKER_HOST", "synthetic-ambient-changed")
			t.Setenv("CONTAINER_HOST", "synthetic-ambient-changed")
			evidence, err := collector(t.Context())
			if err != nil || evidence.ResourceExhaustion != core.ResourceExhaustionMemory || infos != 1 {
				t.Fatalf("frozen collection failed: OOM=%s infos=%d err=%v", evidence.ResourceExhaustion, infos, err)
			}
		})
	}
}

func TestRunFailureMemoryContextOptionalFailures(t *testing.T) {
	for _, failure := range []string{"error", "empty", "malformed", "overflow", "oversized", "identity", "timeout", "unknown route", "budget spent", "ordinary", "reset"} {
		t.Run(failure, func(t *testing.T) {
			b := testBackend(&recordingRunner{})
			b.cfg.LocalContainer.CheckpointMetadata = testCapturedScopeLabels(nil)
			if failure == "unknown route" {
				b.cfg.LocalContainer.CheckpointMetadata = nil
			}
			reads, infos := 0, 0
			b.rt.Exec = memoryEvidenceCommandRunner(func(ctx context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
				switch req.Args[0] {
				case "exec":
					reads++
					count := reads + 4
					if failure == "ordinary" {
						count = 5
					}
					if failure == "reset" && reads > 1 {
						count = 0
					}
					return core.LocalCommandResult{Stdout: fmt.Sprintf("oom_kill %d\n", count)}, nil
				case "inspect":
					return core.LocalCommandResult{Stdout: `[{"Id":"container-123","HostConfig":{"Memory":1024,"MemorySwap":0}}]`}, nil
				case "info":
					infos++
					deadline, ok := ctx.Deadline()
					if !ok || time.Until(deadline) > 500*time.Millisecond {
						t.Fatal("optional probe has no bounded sub-budget")
					}
					switch failure {
					case "error":
						return core.LocalCommandResult{}, errors.New("synthetic optional error")
					case "empty":
						return core.LocalCommandResult{}, nil
					case "malformed":
						return core.LocalCommandResult{Stdout: "daemon-test\nnot-a-number"}, nil
					case "overflow":
						return core.LocalCommandResult{Stdout: "daemon-test\n18446744073709551616"}, nil
					case "oversized":
						return core.LocalCommandResult{Stdout: strings.Repeat("x", 4097)}, nil
					case "identity":
						return core.LocalCommandResult{Stdout: "different-daemon\n8388608"}, nil
					case "timeout":
						<-ctx.Done()
						return core.LocalCommandResult{}, ctx.Err()
					}
				}
				t.Fatalf("unexpected capacity probe after %s", failure)
				return core.LocalCommandResult{}, nil
			})
			collector, err := b.BeginRunFailureEvidence(t.Context(), core.RunFailureEvidenceRequest{Lease: core.LeaseTarget{Server: core.Server{CloudID: "container-123"}}})
			if err != nil {
				t.Fatal(err)
			}
			budget := time.Second
			if failure == "budget spent" {
				budget = 30 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(t.Context(), budget)
			defer cancel()
			evidence, err := collector(ctx)
			if err != nil {
				t.Fatalf("optional failure replaced primary evidence: %v", err)
			}
			if failure == "ordinary" || failure == "reset" {
				if evidence.ResourceExhaustion != "" || infos != 0 {
					t.Fatal("non-OOM acquired capacity evidence")
				}
				return
			}
			if evidence.ResourceExhaustion != core.ResourceExhaustionMemory {
				t.Fatal("lost verified OOM")
			}
			wantInfos := 1
			if failure == "budget spent" || failure == "unknown route" {
				wantInfos = 0
			}
			if infos != wantInfos {
				t.Fatalf("optional calls=%d want %d", infos, wantInfos)
			}
			data, _ := json.Marshal(evidence)
			var got struct{ Details map[string]string }
			_ = json.Unmarshal(data, &got)
			if got.Details["capacity_status"] != "unknown" || got.Details["runtime_memory_total_bytes"] != "" {
				t.Fatalf("optional failure manufactured capacity: %s", data)
			}
		})
	}
}

func TestRunFailureMemoryContextChangedRoute(t *testing.T) {
	for _, runtimeName := range []string{"docker", "podman"} {
		t.Run(runtimeName, func(t *testing.T) {
			reads, infos := 0, 0
			runner := &recordingRunner{run: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
				args := req.Args[2:]
				switch args[0] {
				case "exec":
					reads++
					return core.LocalCommandResult{Stdout: fmt.Sprintf("oom_kill %d\n", reads)}, nil
				case "inspect":
					return core.LocalCommandResult{Stdout: `[{"Id":"container-123","HostConfig":{"Memory":1024,"MemorySwap":0}}]`}, nil
				case "context":
					return core.LocalCommandResult{Stdout: "unix:///changed.sock"}, nil
				case "system":
					return core.LocalCommandResult{Stdout: `[{"Name":"owned","URI":"unix:///changed.sock"}]`}, nil
				case "info":
					infos++
				}
				return core.LocalCommandResult{}, nil
			}}
			b := testBackend(runner)
			b.cfg.LocalContainer.Runtime = runtimeName
			b.cfg.LocalContainer.CheckpointMetadata = checkpointScopeMetadata(checkpointScope{Runtime: runtimeName, Context: "owned", Endpoint: "unix:///original.sock", DaemonID: "original"})
			collector, err := b.BeginRunFailureEvidence(t.Context(), core.RunFailureEvidenceRequest{Lease: core.LeaseTarget{Server: core.Server{CloudID: "container-123"}}})
			if err != nil {
				t.Fatal(err)
			}
			evidence, err := collector(t.Context())
			if err != nil || evidence.ResourceExhaustion != core.ResourceExhaustionMemory || evidence.Details["capacity_status"] != "unknown" || infos != 0 {
				t.Fatalf("changed route was adopted or OOM lost: %+v calls=%d err=%v", evidence, infos, err)
			}
		})
	}
}

func TestDockerPinsCheckpointForkScope(t *testing.T) {
	runner := &recordingRunner{}
	b := testBackend(runner)
	b.cfg.LocalContainer.CheckpointMetadata = map[string]string{
		checkpointMetadataRuntime: "docker",
		checkpointMetadataContext: "remote-context",
		checkpointMetadataConfig:  "/tmp/docker-config",
	}
	core.MarkLocalContainerRuntimeExplicit(&b.cfg)
	t.Setenv("DOCKER_HOST", "tcp://ambient.invalid:2376")
	if _, err := b.docker(context.Background(), []string{"image", "inspect", "checkpoint"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	last := runner.calls[len(runner.calls)-1]
	if len(last.Args) < 3 || last.Args[0] != "--context" || last.Args[1] != "remote-context" {
		t.Fatalf("args=%v", last.Args)
	}
	foundConfig := false
	for _, value := range last.Env {
		if value == "DOCKER_CONFIG=/tmp/docker-config" {
			foundConfig = true
		}
		if strings.HasPrefix(value, "DOCKER_HOST=") {
			t.Fatalf("ambient Docker host leaked into pinned context: %q", value)
		}
	}
	if !foundConfig {
		t.Fatal("Docker config was not pinned")
	}
}

func TestClaimScopePinsCheckpointForkDaemon(t *testing.T) {
	runner := &recordingRunner{}
	b := testBackend(runner)
	b.cfg.LocalContainer.CheckpointMetadata = map[string]string{
		checkpointMetadataRuntime: "docker",
		checkpointMetadataContext: "remote-context",
	}
	t.Setenv("DOCKER_HOST", "tcp://ambient.invalid:2376")

	got := b.claimScope(context.Background())
	want := "runtime:docker/context:remote-context"
	if got != want {
		t.Fatalf("scope=%q, want %q", got, want)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("recorded fork scope should not probe ambient Docker state: calls=%d", len(runner.calls))
	}
}

func TestResolveContainerUsesCheckpointScopeFromClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("DOCKER_HOST", "tcp://ambient.invalid:2376")
	binDir := writeCheckpointScopeDocker(t, "unix:///tmp/docker.sock", "daemon-123")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	labels := map[string]string{
		"crabbox":                  "true",
		"provider":                 providerName,
		"lease":                    "cbx_scope",
		"slug":                     "scope-fork",
		"state":                    "ready",
		"runtime":                  "docker",
		"image":                    "crabbox-checkpoint-demo-123",
		checkpointMetadataContext:  "remote-context",
		checkpointMetadataConfig:   "/tmp/docker-config",
		checkpointMetadataEndpoint: "unix:///tmp/docker.sock",
		checkpointMetadataDaemonID: "daemon-123",
		checkpointMetadataForkName: "crabbox-checkpoint-demo-123",
	}
	if err := core.ClaimLeaseForRepoProviderScopePond("cbx_scope", "scope-fork", providerName, "runtime:docker/context:remote-context", "", "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if err := core.UpdateLeaseClaimEndpoint("cbx_scope", core.Server{Labels: labels}, core.SSHTarget{}); err != nil {
		t.Fatal(err)
	}
	containerJSON, err := json.Marshal([]inspectContainer{{
		ID:     "container-123",
		Name:   "/crabbox-scope-fork",
		Config: inspectConfig{Image: "sha256:123", Labels: labels},
		State:  inspectState{Status: "running", Running: true},
	}})
	if err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{run: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		switch {
		case slices.Contains(req.Args, "ps"):
			if len(req.Args) < 2 || req.Args[0] != "--context" {
				return core.LocalCommandResult{}, errors.New("ambient Docker daemon unavailable")
			}
			return core.LocalCommandResult{Stdout: "container-123\n"}, nil
		case slices.Contains(req.Args, "inspect"):
			return core.LocalCommandResult{Stdout: string(containerJSON)}, nil
		default:
			return core.LocalCommandResult{}, nil
		}
	}}
	b := testBackend(runner)
	container, leaseID, slug, err := b.resolveContainer(context.Background(), "scope-fork")
	if err != nil {
		t.Fatal(err)
	}
	if container.ID != "container-123" || leaseID != "cbx_scope" || slug != "scope-fork" {
		t.Fatalf("resolved container=%q lease=%q slug=%q", container.ID, leaseID, slug)
	}
	var scopedCall *core.LocalCommandRequest
	for i := range runner.calls {
		if len(runner.calls[i].Args) >= 2 && runner.calls[i].Args[0] == "--context" {
			scopedCall = &runner.calls[i]
			break
		}
	}
	if scopedCall == nil {
		t.Fatal("claim-scoped lookup did not call Docker")
	}
	if scopedCall.Args[1] != "remote-context" {
		t.Fatalf("claim scope was not applied to Docker lookup: args=%v", scopedCall.Args)
	}
	for _, value := range scopedCall.Env {
		if value == "DOCKER_HOST=tcp://ambient.invalid:2376" {
			t.Fatal("ambient Docker host leaked into claim-scoped lookup")
		}
	}
}

func TestResolveContainerHydratesCustomRuntimeRouteBeforeLookup(t *testing.T) {
	for _, tc := range []struct {
		name          string
		runtime       string
		context       string
		host          string
		providerScope string
	}{
		{name: "docker host", runtime: "docker", host: "unix:///captured-docker.sock", providerScope: "runtime:docker/context:default/host:unix:///captured-docker.sock"},
		{name: "podman connection", runtime: "podman", context: "captured-podman", providerScope: "runtime:podman/context:captured-podman"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			t.Setenv("DOCKER_HOST", "unix:///ambient.sock")
			leaseID := "cbx_hydrated_route"
			labels := checkpointScopeMetadata(checkpointScope{
				Runtime: tc.runtime, Context: tc.context, Host: tc.host, Endpoint: firstNonBlank(tc.host, "connection://captured-podman"), DaemonID: "daemon-captured",
			})
			labels["provider"] = providerName
			labels["lease"] = leaseID
			labels["slug"] = "hydrated-route"
			labels["state"] = pendingClaimState
			if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(
				leaseID, "hydrated-route", providerName, tc.providerScope, "", t.TempDir(), time.Minute, false,
				core.Server{CloudID: "hydrated-container", Provider: providerName, Labels: labels}, core.SSHTarget{},
			); err != nil {
				t.Fatal(err)
			}
			inspectJSON := `[{"Id":"hydrated-container","Name":"/hydrated-container","Config":{"Image":"ubuntu:24.04","Labels":{"provider":"local-container","lease":"cbx_hydrated_route","slug":"hydrated-route","state":"provisioning"}},"State":{"Status":"running","Running":true}}]`
			runner := &recordingRunner{run: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
				args := req.Args
				if tc.runtime == "podman" {
					if req.Name != "podman" || len(args) < 3 || args[0] != "--connection" || args[1] != tc.context {
						t.Fatalf("Podman route not hydrated: name=%s args=%v", req.Name, args)
					}
					args = args[2:]
				} else {
					if req.Name != "docker" {
						t.Fatalf("runtime=%s", req.Name)
					}
					foundHost := false
					for _, value := range req.Env {
						if value == "DOCKER_HOST="+tc.host {
							foundHost = true
						}
						if value == "DOCKER_HOST=unix:///ambient.sock" {
							t.Fatal("ambient Docker host reached lookup")
						}
					}
					if !foundHost {
						t.Fatalf("captured Docker host missing: command=%s args=%q", req.Name, req.Args)
					}
				}
				switch firstArg(args) {
				case "ps":
					return core.LocalCommandResult{Stdout: "hydrated-container\n"}, nil
				case "inspect":
					return core.LocalCommandResult{Stdout: inspectJSON}, nil
				default:
					return core.LocalCommandResult{}, nil
				}
			}}
			b := testBackend(runner)
			b.validateRuntimeScope = func(context.Context, checkpointScope) error { return nil }
			container, gotLease, _, err := b.resolveContainer(context.Background(), leaseID)
			if err != nil {
				t.Fatal(err)
			}
			if container.ID != "hydrated-container" || gotLease != leaseID {
				t.Fatalf("container=%q lease=%q", container.ID, gotLease)
			}
		})
	}
}

func TestResolveContainerRejectsAmbiguousClaimSlug(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	for _, leaseID := range []string{"cbx_scope_a", "cbx_scope_b"} {
		if err := core.ClaimLeaseForRepoProviderScopePond(leaseID, "shared-slug", providerName, "runtime:docker/context:remote-context", "", "/repo", time.Minute, false); err != nil {
			t.Fatal(err)
		}
	}
	runner := &recordingRunner{}
	b := testBackend(runner)
	if _, _, _, err := b.resolveContainer(context.Background(), "shared-slug"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("err=%v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("ambiguous slug should fail before Docker lookup: calls=%d", len(runner.calls))
	}
}

func TestResolveContainerExactLeaseIgnoresMalformedUnrelatedClaim(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	if err := writeLocalContainerClaim(t, "cbx_exact", "exact-slug", "", time.Now().UTC(), time.Minute); err != nil {
		t.Fatal(err)
	}
	claimDir := filepath.Join(stateHome, "crabbox", "claims")
	if err := os.WriteFile(filepath.Join(claimDir, "cbx_unrelated.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	inspectJSON := `[{
		"Id":"container-exact",
		"Name":"/crabbox-exact",
		"Config":{"Image":"ubuntu:24.04","Labels":{"crabbox":"true","provider":"local-container","lease":"cbx_exact","slug":"exact-slug","state":"ready"}},
		"State":{"Status":"running","Running":true}
	}]`
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
		commandKey([]string{"ps", "-a", "--filter", "label=crabbox=true", "--filter", "label=provider=local-container", "--format", "{{.ID}}"}): {Stdout: "container-exact\n"},
		commandKey([]string{"inspect", "container-exact"}): {Stdout: inspectJSON},
	}}
	b := testBackend(runner)
	container, leaseID, _, err := b.resolveContainer(context.Background(), "cbx_exact")
	if err != nil {
		t.Fatal(err)
	}
	if container.ID != "container-exact" || leaseID != "cbx_exact" {
		t.Fatalf("container=%q lease=%q", container.ID, leaseID)
	}
}

func TestServerFromContainerDropsEmptyInheritedLabels(t *testing.T) {
	b := testBackend(&recordingRunner{})
	server := b.serverFromContainer(inspectContainer{
		ID: "container-123",
		Config: inspectConfig{
			Image: "checkpoint-image",
			Labels: map[string]string{
				"crabbox":                  "true",
				"provider":                 providerName,
				"lease":                    "cbx_123",
				"tailscale_exit_node":      "",
				checkpointMetadataHost:     "tcp://user:password@example.invalid",
				checkpointMetadataEndpoint: "tcp://user:password@example.invalid",
				checkpointMetadataConfig:   "/private/docker-config",
			},
		},
	}, b.cfg)
	if _, ok := server.Labels["tailscale_exit_node"]; ok {
		t.Fatalf("empty inherited label survived: %#v", server.Labels)
	}
	for _, key := range []string{checkpointMetadataHost, checkpointMetadataEndpoint, checkpointMetadataConfig} {
		if server.Labels[key] != "" {
			t.Fatalf("private inherited label %s survived: %#v", key, server.Labels)
		}
	}
}

func TestTerminalContainerStateOverridesStaleLabels(t *testing.T) {
	for _, tc := range []struct {
		name         string
		runtimeState string
		claimState   string
	}{
		{name: "exited provisioning", runtimeState: "exited", claimState: pendingClaimState},
		{name: "dead provisioning", runtimeState: "dead", claimState: pendingClaimState},
		{name: "podman stopped provisioning", runtimeState: "stopped", claimState: pendingClaimState},
		{name: "previously ready exited", runtimeState: "exited", claimState: "ready"},
		{name: "previously ready dead", runtimeState: "dead", claimState: "ready"},
		{name: "previously ready podman stopped", runtimeState: "stopped", claimState: "ready"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := testBackend(&recordingRunner{})
			container := inspectContainer{
				ID: "stopped-container",
				Config: inspectConfig{Labels: map[string]string{
					"state": tc.claimState, "recovery": pendingRecoveryKind,
				}},
				State: inspectState{Status: tc.runtimeState},
			}
			server := b.serverFromContainer(container, b.cfg)
			if server.Status != tc.runtimeState || server.Labels["state"] != tc.runtimeState {
				t.Fatalf("observed server=%#v, want runtime state %q", server, tc.runtimeState)
			}
			claim := core.LeaseClaim{
				CloudID: container.ID,
				Labels: map[string]string{
					"state": tc.claimState, "recovery": pendingRecoveryKind,
					checkpointMetadataHost: "tcp://user:password@example.invalid", checkpointMetadataEndpoint: "tcp://user:password@example.invalid", checkpointMetadataConfig: "/private/docker-config",
				},
			}
			merged := mergeLocalContainerClaim(server, claim)
			if merged.Status != tc.runtimeState || merged.Labels["state"] != tc.runtimeState {
				t.Fatalf("merged server=%#v, want runtime state %q", merged, tc.runtimeState)
			}
			if merged.Labels["recovery"] != pendingRecoveryKind || claim.Labels["state"] != tc.claimState {
				t.Fatalf("recovery claim changed: merged=%#v claim=%#v", merged.Labels, claim.Labels)
			}
			for _, key := range []string{checkpointMetadataHost, checkpointMetadataEndpoint, checkpointMetadataConfig} {
				if merged.Labels[key] != "" {
					t.Fatalf("private claim label %s projected: %#v", key, merged.Labels)
				}
			}
		})
	}
}

func TestProviderAliases(t *testing.T) {
	for _, name := range []string{"local-container", "docker", "container", "local-docker"} {
		provider, err := core.ProviderFor(name)
		if err != nil {
			t.Fatalf("ProviderFor(%q): %v", name, err)
		}
		if provider.Name() != providerName {
			t.Fatalf("ProviderFor(%q).Name=%q", name, provider.Name())
		}
	}
	spec := Provider{}.Spec()
	if !spec.Features.Has(core.FeatureDesktop) || !spec.Features.Has(core.FeatureBrowser) {
		t.Fatalf("local-container features=%v, want desktop and browser", spec.Features)
	}
	if !spec.Features.Has(core.FeatureCleanup) {
		t.Fatalf("local-container features=%v, want cleanup", spec.Features)
	}
	if !spec.Features.Has(core.FeatureCacheVolume) {
		t.Fatalf("local-container features=%v, want cache-volume", spec.Features)
	}
	if _, ok := newBackend(spec, core.BaseConfig(), core.Runtime{Exec: &recordingRunner{}}).(core.SSHRunFailureEvidenceBackend); !ok {
		t.Fatal("local-container backend does not expose failed-run evidence capability")
	}
	capability, ok := any(Provider{}).(core.ProviderArchitectureCapability)
	if !ok {
		t.Fatal("local-container provider does not expose architecture capability")
	}
	for _, architecture := range []string{core.ArchitectureAMD64, core.ArchitectureARM64} {
		cfg := core.BaseConfig()
		cfg.TargetOS = core.TargetLinux
		if !capability.SupportsArchitecture(cfg, architecture) {
			t.Fatalf("architecture capability rejected %s", architecture)
		}
	}
}

func TestAssertRequestedArchitectureNormalizesDockerAndPodman(t *testing.T) {
	tests := []struct {
		runtime   string
		requested string
		available string
		want      string
	}{
		{runtime: "docker", requested: core.ArchitectureAMD64, available: "amd64", want: core.ArchitectureAMD64},
		{runtime: "docker", requested: core.ArchitectureAMD64, available: "x86_64", want: core.ArchitectureAMD64},
		{runtime: "docker", requested: core.ArchitectureARM64, available: "arm64", want: core.ArchitectureARM64},
		{runtime: "docker", requested: core.ArchitectureARM64, available: "aarch64", want: core.ArchitectureARM64},
		{runtime: "podman", requested: core.ArchitectureAMD64, available: "amd64", want: core.ArchitectureAMD64},
		{runtime: "podman", requested: core.ArchitectureAMD64, available: "x86_64", want: core.ArchitectureAMD64},
		{runtime: "podman", requested: core.ArchitectureARM64, available: "arm64", want: core.ArchitectureARM64},
		{runtime: "podman", requested: core.ArchitectureARM64, available: "aarch64", want: core.ArchitectureARM64},
	}
	for _, tc := range tests {
		t.Run(tc.runtime+"_"+tc.available, func(t *testing.T) {
			runner := &recordingRunner{run: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
				wantFormat := "{{.Architecture}}"
				if tc.runtime == "podman" {
					wantFormat = "{{.Host.Arch}}"
				}
				if req.Name != tc.runtime || !slices.Equal(req.Args, []string{"info", "--format", wantFormat}) {
					t.Fatalf("runtime request name=%q args=%v", req.Name, req.Args)
				}
				return core.LocalCommandResult{Stdout: tc.available + "\n"}, nil
			}}
			b := testBackend(runner)
			cfg := b.cfg
			cfg.LocalContainer.Runtime = tc.runtime
			cfg.Architecture = tc.requested
			core.MarkArchitectureExplicit(&cfg)
			got, err := b.assertRequestedArchitecture(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("architecture=%q, want %q", got, tc.want)
			}
		})
	}
}

func TestAssertRequestedArchitectureRejectsMismatchAndUnrecognizedOutput(t *testing.T) {
	tests := []struct {
		name          string
		runtime       string
		requested     string
		available     string
		wantAvailable string
	}{
		{name: "docker mismatch", runtime: "docker", requested: core.ArchitectureARM64, available: "x86_64", wantAvailable: "available=amd64"},
		{name: "podman mismatch", runtime: "podman", requested: core.ArchitectureAMD64, available: "aarch64", wantAvailable: "available=arm64"},
		{name: "docker unrecognized", runtime: "docker", requested: core.ArchitectureAMD64, available: "ppc64le", wantAvailable: `available="ppc64le"`},
		{name: "podman malformed", runtime: "podman", requested: core.ArchitectureARM64, available: "amd64\narm64", wantAvailable: `available="amd64\narm64"`},
		{name: "docker empty", runtime: "docker", requested: core.ArchitectureAMD64, available: "", wantAvailable: `available=""`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runner := &recordingRunner{run: func(core.LocalCommandRequest) (core.LocalCommandResult, error) {
				return core.LocalCommandResult{Stdout: tc.available}, nil
			}}
			b := testBackend(runner)
			cfg := b.cfg
			cfg.LocalContainer.Runtime = tc.runtime
			cfg.Architecture = tc.requested
			core.MarkArchitectureExplicit(&cfg)
			_, err := b.assertRequestedArchitecture(context.Background(), cfg)
			if err == nil || !strings.Contains(err.Error(), "requested="+tc.requested) || !strings.Contains(err.Error(), tc.wantAvailable) {
				t.Fatalf("err=%v, want requested and available architecture", err)
			}
		})
	}
}

func TestAssertRequestedArchitectureUsesCapturedRemoteRuntimeRoute(t *testing.T) {
	tests := []struct {
		name       string
		runtime    string
		context    string
		wantPrefix []string
		wantFormat string
	}{
		{name: "docker context", runtime: "docker", context: "remote-docker", wantPrefix: []string{"--context", "remote-docker"}, wantFormat: "{{.Architecture}}"},
		{name: "podman connection", runtime: "podman", context: "remote-podman", wantPrefix: []string{"--connection", "remote-podman"}, wantFormat: "{{.Host.Arch}}"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runner := &recordingRunner{run: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
				want := append(append([]string{}, tc.wantPrefix...), "info", "--format", tc.wantFormat)
				if req.Name != tc.runtime || !slices.Equal(req.Args, want) {
					t.Fatalf("runtime request name=%q args=%v want=%v", req.Name, req.Args, want)
				}
				return core.LocalCommandResult{Stdout: "arm64\n"}, nil
			}}
			b := testBackend(runner)
			cfg := b.cfg
			cfg.LocalContainer.Runtime = tc.runtime
			cfg.LocalContainer.CheckpointMetadata = checkpointScopeMetadata(checkpointScope{
				Runtime: tc.runtime, Context: tc.context, Endpoint: "remote", DaemonID: "daemon-remote",
			})
			cfg.Architecture = core.ArchitectureARM64
			core.MarkArchitectureExplicit(&cfg)
			if _, err := b.assertRequestedArchitecture(context.Background(), cfg); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAssertRequestedArchitectureOmittedDoesNotProbe(t *testing.T) {
	describer, ok := any(Provider{}).(core.ProviderConfigArchitectureDescriber)
	if !ok || describer.DescribeImplicitArchitecture(core.BaseConfig()) != "native" {
		t.Fatal("omitted architecture diagnostics must describe native runtime selection")
	}
	runner := &recordingRunner{}
	b := testBackend(runner)
	if got, err := b.assertRequestedArchitecture(context.Background(), b.cfg); err != nil || got != "" {
		t.Fatalf("architecture=%q err=%v", got, err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("omitted architecture probed runtime: %#v", runner.commandSummary())
	}
}

func TestAcquireArchitectureMismatchFailsBeforeContainerCreation(t *testing.T) {
	for _, runtimeName := range []string{"docker", "podman"} {
		t.Run(runtimeName, func(t *testing.T) {
			runner := &recordingRunner{run: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
				if slices.Contains(req.Args, "run") {
					t.Fatalf("container creation attempted after architecture mismatch: %v", req.Args)
				}
				return core.LocalCommandResult{Stdout: "amd64\n"}, nil
			}}
			b := testBackend(runner)
			b.cfg.LocalContainer.Runtime = runtimeName
			b.cfg.Architecture = core.ArchitectureARM64
			core.MarkArchitectureExplicit(&b.cfg)
			_, err := b.Acquire(context.Background(), core.AcquireRequest{})
			if err == nil || !strings.Contains(err.Error(), "requested=arm64 available=amd64") {
				t.Fatalf("err=%v", err)
			}
			if len(runner.calls) != 1 {
				t.Fatalf("runtime calls=%d, want architecture probe only: %#v", len(runner.calls), runner.commandSummary())
			}
		})
	}
}

func TestResolveArchitectureAssertionMatchesAndNormalizesAliases(t *testing.T) {
	tests := []struct {
		name      string
		runtime   string
		requested string
		available string
		want      string
		context   string
	}{
		{name: "docker canonical", runtime: "docker", requested: core.ArchitectureAMD64, available: "amd64", want: core.ArchitectureAMD64, context: "default"},
		{name: "docker daemon alias", runtime: "docker", requested: core.ArchitectureAMD64, available: "x86_64", want: core.ArchitectureAMD64, context: "default"},
		{name: "docker request alias", runtime: "docker", requested: "aarch64", available: "arm64", want: core.ArchitectureARM64, context: "default"},
		{name: "podman aliases", runtime: "podman", requested: "x86_64", available: "amd64", want: core.ArchitectureAMD64, context: "default"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, runner, leaseID, _ := resolveArchitectureFixture(t, tc.runtime, tc.context, tc.requested, tc.available)
			lease, err := b.Resolve(context.Background(), core.ResolveRequest{ID: leaseID})
			if err != nil {
				t.Fatal(err)
			}
			if got := lease.Server.Labels["architecture"]; got != tc.want {
				t.Fatalf("resolved architecture=%q, want %q", got, tc.want)
			}
			if len(runner.calls) != 3 {
				t.Fatalf("runtime calls=%d, want info/list/inspect: %#v", len(runner.calls), runner.commandSummary())
			}
			for _, call := range runner.calls {
				if slices.Contains(call.Args, "--platform") {
					t.Fatalf("reuse architecture assertion requested emulation: %#v", call.Args)
				}
			}
		})
	}
}

func TestResolveArchitectureAssertionUsesCapturedRemoteRuntimeRoute(t *testing.T) {
	tests := []struct {
		name      string
		runtime   string
		context   string
		requested string
		available string
	}{
		{name: "docker context", runtime: "docker", context: "remote-docker", requested: core.ArchitectureAMD64, available: "x86_64"},
		{name: "podman connection", runtime: "podman", context: "remote-podman", requested: core.ArchitectureARM64, available: "aarch64"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, runner, leaseID, _ := resolveArchitectureFixture(t, tc.runtime, tc.context, tc.requested, tc.available)
			lease, err := b.Resolve(context.Background(), core.ResolveRequest{ID: leaseID})
			if err != nil {
				t.Fatal(err)
			}
			want, err := core.NormalizeArchitecture(tc.requested)
			if err != nil {
				t.Fatal(err)
			}
			if got := lease.Server.Labels["architecture"]; got != want {
				t.Fatalf("resolved architecture=%q, want %q", got, want)
			}
			if len(runner.calls) != 3 {
				t.Fatalf("runtime calls=%d, want info/list/inspect: %#v", len(runner.calls), runner.commandSummary())
			}
		})
	}
}

func TestResolveArchitectureAssertionFailurePrecedesContainerUseAndClaimMutation(t *testing.T) {
	tests := []struct {
		name      string
		runtime   string
		context   string
		requested string
		available string
		wantError string
	}{
		{name: "docker mismatch", runtime: "docker", context: "default", requested: core.ArchitectureARM64, available: "x86_64", wantError: "requested=arm64 available=amd64"},
		{name: "malformed remote docker response", runtime: "docker", context: "remote-docker", requested: core.ArchitectureAMD64, available: "amd64\narm64", wantError: `available="amd64\narm64"`},
		{name: "remote podman mismatch", runtime: "podman", context: "remote-podman", requested: core.ArchitectureAMD64, available: "aarch64", wantError: "requested=amd64 available=arm64"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, runner, leaseID, before := resolveArchitectureFixture(t, tc.runtime, tc.context, tc.requested, tc.available)
			_, err := b.Resolve(context.Background(), core.ResolveRequest{ID: leaseID, Repo: core.Repo{Root: t.TempDir()}})
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("Resolve error=%v, want %q", err, tc.wantError)
			}
			if len(runner.calls) != 1 {
				t.Fatalf("runtime calls=%d, want architecture probe only: %#v", len(runner.calls), runner.commandSummary())
			}
			for _, forbidden := range []string{"ps", "inspect", "exec", "run", "rm"} {
				if slices.Contains(runner.calls[0].Args, forbidden) {
					t.Fatalf("architecture failure reached container %s: %#v", forbidden, runner.calls[0].Args)
				}
			}
			after, readErr := core.ReadLeaseClaim(leaseID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("architecture failure mutated claim:\nbefore=%#v\nafter=%#v", before, after)
			}
		})
	}
}

func resolveArchitectureFixture(t *testing.T, runtimeName, contextName, requested, available string) (*backend, *recordingRunner, string, core.LeaseClaim) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_resolve_arch"
	slug := "resolve-arch"
	containerID := "resolve-arch-container"
	scope := checkpointScope{
		Runtime:  runtimeName,
		Context:  contextName,
		Endpoint: "unix:///tmp/resolve-arch.sock",
		DaemonID: "resolve-arch-daemon",
	}
	labels := map[string]string{
		"crabbox": "true", "provider": providerName, "lease": leaseID, "slug": slug,
		"state": "ready", "runtime": runtimeName, "ssh_user": "runner", "work_root": "/workspace/crabbox",
	}
	for key, value := range checkpointScopeMetadata(scope) {
		if value != "" {
			labels[key] = value
		}
	}
	if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(
		leaseID,
		slug,
		providerName,
		localContainerClaimScope(runtimeName, contextName),
		"",
		t.TempDir(),
		time.Minute,
		false,
		core.Server{CloudID: containerID, Provider: providerName, Labels: labels},
		core.SSHTarget{Host: "127.0.0.1", Port: "49153"},
	); err != nil {
		t.Fatal(err)
	}
	before, err := core.ReadLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	inspectJSON := fmt.Sprintf(`[{"Id":%q,"Name":"/crabbox-resolve-arch","Config":{"Image":"ubuntu:24.04","Labels":{"crabbox":"true","provider":"local-container","lease":%q,"slug":%q,"state":"ready","runtime":%q,"ssh_user":"runner","work_root":"/workspace/crabbox"}},"State":{"Status":"running","Running":true},"NetworkSettings":{"Ports":{"2222/tcp":[{"HostIp":"127.0.0.1","HostPort":"49153"}]}}}]`, containerID, leaseID, slug, runtimeName)
	prefix := []string{}
	if contextName != "" && contextName != "default" {
		if runtimeName == "podman" {
			prefix = []string{"--connection", contextName}
		} else {
			prefix = []string{"--context", contextName}
		}
	}
	runner := &recordingRunner{run: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		if req.Name != runtimeName || len(req.Args) < len(prefix)+1 || !slices.Equal(req.Args[:len(prefix)], prefix) {
			t.Fatalf("runtime request name=%q args=%v, want %s prefix=%v", req.Name, req.Args, runtimeName, prefix)
		}
		args := req.Args[len(prefix):]
		switch args[0] {
		case "info":
			wantFormat := "{{.Architecture}}"
			if runtimeName == "podman" {
				wantFormat = "{{.Host.Arch}}"
			}
			if !slices.Equal(args, []string{"info", "--format", wantFormat}) {
				t.Fatalf("architecture probe args=%v", req.Args)
			}
			return core.LocalCommandResult{Stdout: available + "\n"}, nil
		case "ps":
			return core.LocalCommandResult{Stdout: containerID + "\n"}, nil
		case "inspect":
			return core.LocalCommandResult{Stdout: inspectJSON}, nil
		default:
			t.Fatalf("unexpected runtime request: %#v", req.Args)
			return core.LocalCommandResult{}, nil
		}
	}}
	b := testBackend(runner)
	b.cfg.LocalContainer.Runtime = runtimeName
	core.MarkLocalContainerRuntimeExplicit(&b.cfg)
	b.cfg.Architecture = requested
	core.MarkArchitectureExplicit(&b.cfg)
	b.validateRuntimeScope = func(_ context.Context, got checkpointScope) error {
		if !sameCheckpointScope(got, scope) {
			t.Fatalf("validated scope=%#v, want %#v", got, scope)
		}
		return nil
	}
	return b, runner, leaseID, before
}

func TestCreateContainerNeverRequestsEmulationPlatform(t *testing.T) {
	tests := []struct {
		runtime      string
		architecture string
	}{
		{runtime: "docker", architecture: core.ArchitectureAMD64},
		{runtime: "docker", architecture: core.ArchitectureARM64},
		{runtime: "podman", architecture: core.ArchitectureAMD64},
		{runtime: "podman", architecture: core.ArchitectureARM64},
	}
	for _, tc := range tests {
		t.Run(tc.runtime+"_"+tc.architecture, func(t *testing.T) {
			runner := &recordingRunner{responses: map[string]core.LocalCommandResult{"run": {Stdout: "container123456\n"}}}
			b := testBackend(runner)
			b.cfg.LocalContainer.Runtime = tc.runtime
			core.MarkLocalContainerRuntimeExplicit(&b.cfg)
			cfg := b.configForRun()
			cfg.Architecture = tc.architecture
			core.MarkArchitectureExplicit(&cfg)
			_, bootstrapDir, err := b.createContainer(context.Background(), cfg, "crabbox-arch", "cbx_arch", "arch-test", "ssh-ed25519 AAAA test", true)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(bootstrapDir) })
			args := recordedArgsForCommand(t, runner, "run")
			if strings.Contains(args, "--platform") {
				t.Fatalf("native architecture assertion emitted platform selection:\n%s", args)
			}
			if !strings.Contains(args, "--label\narchitecture="+tc.architecture) {
				t.Fatalf("normalized architecture label missing:\n%s", args)
			}
		})
	}
}

func TestCreateContainerUsesDockerCompatibleSSHLease(t *testing.T) {
	dir := t.TempDir()
	writeExecutable(t, filepath.Join(dir, "docker"))
	t.Setenv("PATH", dir)
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{},
	}
	b := testBackend(runner)
	cfg := b.configForRun()
	runner.responses[commandKey([]string{"run"})] = core.LocalCommandResult{Stdout: "container123456\n"}

	id, bootstrapDir, err := b.createContainer(context.Background(), cfg, "crabbox-blue", "cbx_123", "blue-lobster", "ssh-ed25519 AAAA test", true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(bootstrapDir) })
	if id != "container123456" {
		t.Fatalf("id=%q", id)
	}
	if parent := filepath.Dir(bootstrapDir); parent != localContainerBootstrapRoot() {
		t.Fatalf("bootstrap parent=%q want %q", parent, localContainerBootstrapRoot())
	}
	if len(runner.calls) != 1 {
		t.Fatalf("calls=%d", len(runner.calls))
	}
	call := runner.calls[0]
	if call.Name != "docker" {
		t.Fatalf("runtime=%q", call.Name)
	}
	args := strings.Join(call.Args, "\n")
	for _, want := range []string{
		"run",
		"--name\ncrabbox-blue",
		"--user\nroot",
		"--network\nbridge",
		"-p\n127.0.0.1::2222",
		"-e\nCRABBOX_SSH_USER=runner",
		"-e\nCRABBOX_WORK_ROOT=/workspace/crabbox",
		"-e\nCRABBOX_DESKTOP=0",
		"-e\nCRABBOX_BROWSER=0",
		"-e\nCRABBOX_DOCKER_SOCKET=0",
		"--cpus\n4",
		"--memory\n8g",
		"--label\nprovider=local-container",
		"--label\nlease=cbx_123",
		"--label\nslug=blue-lobster",
		"--label\nssh_user=runner",
		"--label\nwork_root=/workspace/crabbox",
		":/tmp/crabbox-bootstrap:ro",
		"ubuntu:24.04",
		"/bin/sh\n/tmp/crabbox-bootstrap/bootstrap.sh",
	} {
		if !strings.Contains(args, want) {
			t.Fatalf("docker run args missing %q:\n%s", want, args)
		}
	}
	if strings.Contains(args, "-v\n/var/run/docker.sock:/var/run/docker.sock") {
		t.Fatalf("docker socket should be opt-in:\n%s", args)
	}
}

func TestCreateContainerDoesNotLabelPrivateRuntimeScope(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	runner.run = func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		if len(req.Args) >= 3 && req.Args[0] == "--context" && req.Args[1] == "named" && req.Args[2] == "run" {
			return core.LocalCommandResult{Stdout: "private-scope-container\n"}, nil
		}
		return core.LocalCommandResult{}, nil
	}
	b := testBackend(runner)
	cfg := b.configForRun()
	cfg.LocalContainer.CheckpointMetadata = checkpointScopeMetadata(checkpointScope{
		Runtime: "docker", Context: "named", Config: "/private/docker-config", Host: "tcp://user:password@example.invalid:2376", Endpoint: "tcp://user:password@example.invalid:2376", DaemonID: "daemon-private",
	})
	b.cfg.LocalContainer.CheckpointMetadata = cloneLabels(cfg.LocalContainer.CheckpointMetadata)
	_, bootstrapDir, err := b.createContainer(context.Background(), cfg, "crabbox-private", "cbx_private", "private", "ssh-ed25519 AAAA test", true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(bootstrapDir) })
	args := strings.Join(runner.calls[len(runner.calls)-1].Args, "\n")
	for _, forbidden := range []string{"password", "example.invalid", "docker_endpoint=", "docker_host=", "docker_config="} {
		if strings.Contains(args, forbidden) {
			t.Fatalf("container labels disclosed %q:\n%s", forbidden, args)
		}
	}
	for _, want := range []string{"docker_context=named", "docker_daemon_id=daemon-private"} {
		if !strings.Contains(args, want) {
			t.Fatalf("safe scope label %q missing:\n%s", want, args)
		}
	}
}

func TestCreateContainerReturnsOwnedIDBeforeRollbackOnRunFailure(t *testing.T) {
	var bootstrapDir string
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	runner.run = func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		switch firstArg(req.Args) {
		case "run":
			bootstrapDir = bootstrapDirFromRunArgs(t, []core.LocalCommandRequest{req})
			return core.LocalCommandResult{Stderr: "run failed"}, errors.New("run failed")
		case "ps":
			return core.LocalCommandResult{Stdout: testRecoveredContainerID + "\n"}, nil
		case "inspect":
			t.Fatal("known ID was inspected before claim publication")
			return core.LocalCommandResult{}, nil
		case "rm":
			t.Fatal("createContainer removed a known-ID resource before claim publication")
			return core.LocalCommandResult{}, nil
		default:
			return core.LocalCommandResult{}, nil
		}
	}
	b := testBackend(runner)

	containerID, _, err := b.createContainer(context.Background(), b.configForRun(), "crabbox-blue", "cbx_123", "blue-lobster", "ssh-ed25519 AAAA test", false)
	if err == nil {
		t.Fatal("createContainer succeeded")
	}
	if containerID != testRecoveredContainerID {
		t.Fatalf("container id=%q, want exact owned id", containerID)
	}
	if _, err := os.Stat(bootstrapDir); err != nil {
		t.Fatalf("bootstrap directory removed before claim publication: %v", err)
	}
	if args := recordedArgsForCommand(t, runner, "ps"); !strings.Contains(args, "--no-trunc") {
		t.Fatalf("exact-ID discovery omitted --no-trunc:\n%s", args)
	}
	t.Cleanup(func() { _ = os.RemoveAll(bootstrapDir) })
}

func TestFullContainerIDValidation(t *testing.T) {
	for _, tc := range []struct {
		id   string
		want bool
	}{
		{id: testRecoveredContainerID, want: true},
		{id: strings.ToUpper(testRecoveredContainerID), want: true},
		{id: "aaaaaaaaaaaa"},
		{id: strings.Repeat("g", 64)},
	} {
		if got := isFullContainerID(tc.id); got != tc.want {
			t.Fatalf("isFullContainerID(%q)=%v want=%v", tc.id, got, tc.want)
		}
	}
}

func TestCreateContainerPreservesBootstrapDirWhenRollbackFails(t *testing.T) {
	var bootstrapDir string
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	runner.run = func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		switch firstArg(req.Args) {
		case "run":
			bootstrapDir = bootstrapDirFromRunArgs(t, []core.LocalCommandRequest{req})
			return core.LocalCommandResult{Stderr: "run failed"}, errors.New("run failed")
		case "ps":
			return core.LocalCommandResult{Stdout: testRecoveredContainerID + "\n"}, nil
		case "inspect":
			return core.LocalCommandResult{Stdout: `[{"Id":` + strconv.Quote(testRecoveredContainerID) + `,"Config":{"Labels":{"lease":"cbx_123","bootstrap_dir":` + strconv.Quote(bootstrapDir) + `}}}]`}, nil
		case "rm":
			return core.LocalCommandResult{Stderr: "daemon unavailable"}, errors.New("remove failed")
		default:
			return core.LocalCommandResult{}, nil
		}
	}
	b := testBackend(runner)

	if _, _, err := b.createContainer(context.Background(), b.configForRun(), "crabbox-blue", "cbx_123", "blue-lobster", "ssh-ed25519 AAAA test", false); err == nil {
		t.Fatal("createContainer succeeded")
	}
	if _, err := os.Stat(bootstrapDir); err != nil {
		t.Fatalf("bootstrap directory missing after failed container rollback: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(bootstrapDir) })
}

func TestCreateContainerRunFailurePreservesUnownedContainer(t *testing.T) {
	var bootstrapDir string
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	runner.run = func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		switch firstArg(req.Args) {
		case "run":
			bootstrapDir = bootstrapDirFromRunArgs(t, []core.LocalCommandRequest{req})
			return core.LocalCommandResult{Stderr: "name conflict"}, errors.New("run failed")
		case "ps":
			return core.LocalCommandResult{}, nil
		case "rm":
			t.Fatal("removed unowned container after run failure")
			return core.LocalCommandResult{}, nil
		default:
			return core.LocalCommandResult{}, nil
		}
	}
	b := testBackend(runner)

	if _, _, err := b.createContainer(context.Background(), b.configForRun(), "crabbox-blue", "cbx_123", "blue-lobster", "ssh-ed25519 AAAA test", false); err == nil {
		t.Fatal("createContainer succeeded")
	}
	if _, err := os.Stat(bootstrapDir); !os.IsNotExist(err) {
		t.Fatalf("bootstrap directory still exists after run failure: %v", err)
	}
}

func TestCreateContainerRunFailureKeepsOwnedContainer(t *testing.T) {
	var bootstrapDir string
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	runner.run = func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		switch firstArg(req.Args) {
		case "run":
			bootstrapDir = bootstrapDirFromRunArgs(t, []core.LocalCommandRequest{req})
			return core.LocalCommandResult{Stderr: "connection lost"}, errors.New("run failed")
		case "ps":
			return core.LocalCommandResult{Stdout: testRecoveredContainerID + "\n"}, nil
		case "inspect":
			return core.LocalCommandResult{Stdout: `[{"Id":` + strconv.Quote(testRecoveredContainerID) + `,"Config":{"Labels":{"lease":"cbx_123","bootstrap_dir":` + strconv.Quote(bootstrapDir) + `}}}]`}, nil
		case "rm":
			t.Fatal("removed kept container after run failure")
			return core.LocalCommandResult{}, nil
		default:
			return core.LocalCommandResult{}, nil
		}
	}
	b := testBackend(runner)

	containerID, _, err := b.createContainer(context.Background(), b.configForRun(), "crabbox-blue", "cbx_123", "blue-lobster", "ssh-ed25519 AAAA test", true)
	if err == nil {
		t.Fatal("createContainer succeeded")
	}
	if containerID != testRecoveredContainerID {
		t.Fatalf("kept container id = %q, want %s", containerID, testRecoveredContainerID)
	}
	if _, err := os.Stat(bootstrapDir); err != nil {
		t.Fatalf("kept bootstrap directory missing after run failure: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(bootstrapDir) })
}

func TestCreateContainerRunFailureKeepsBootstrapDirWhenOwnershipUnknown(t *testing.T) {
	var bootstrapDir string
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	runner.run = func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		switch firstArg(req.Args) {
		case "run":
			bootstrapDir = bootstrapDirFromRunArgs(t, []core.LocalCommandRequest{req})
			return core.LocalCommandResult{Stderr: "connection lost"}, errors.New("run failed")
		case "ps":
			return core.LocalCommandResult{Stderr: "daemon unavailable"}, errors.New("inspect failed")
		case "rm":
			t.Fatal("removed container with unknown ownership after run failure")
			return core.LocalCommandResult{}, nil
		default:
			return core.LocalCommandResult{}, nil
		}
	}
	b := testBackend(runner)

	if _, _, err := b.createContainer(context.Background(), b.configForRun(), "crabbox-blue", "cbx_123", "blue-lobster", "ssh-ed25519 AAAA test", true); err == nil {
		t.Fatal("createContainer succeeded")
	}
	if _, err := os.Stat(bootstrapDir); err != nil {
		t.Fatalf("bootstrap directory missing with unknown container ownership: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(bootstrapDir) })
}

func TestAcquireRunFailureKeepsExactPendingClaim(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	var leaseID string
	var bootstrapDir string
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	runner.run = func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		switch firstArg(req.Args) {
		case "run":
			bootstrapDir = bootstrapDirFromRunArgs(t, []core.LocalCommandRequest{req})
			leaseID = labelFromRunArgs(t, req.Args, "lease")
			return core.LocalCommandResult{Stderr: "connection lost"}, errors.New("run failed")
		case "ps":
			if strings.Contains(strings.Join(req.Args, "\n"), "label=lease=") {
				return core.LocalCommandResult{Stdout: testRecoveredContainerID + "\n"}, nil
			}
			return core.LocalCommandResult{}, nil
		case "inspect":
			return core.LocalCommandResult{Stdout: `[{"Id":` + strconv.Quote(testRecoveredContainerID) + `,"Config":{"Labels":{"lease":` + strconv.Quote(leaseID) + `,"bootstrap_dir":` + strconv.Quote(bootstrapDir) + `}}}]`}, nil
		default:
			return core.LocalCommandResult{}, nil
		}
	}
	var stderr strings.Builder
	b := testBackend(runner)
	b.rt.Stderr = &stderr

	if _, err := b.Acquire(context.Background(), core.AcquireRequest{Keep: true, Repo: core.Repo{Root: t.TempDir()}}); err == nil {
		t.Fatal("Acquire succeeded")
	}
	keyPath, err := core.TestboxKeyPath(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("retained container SSH key missing: %v", err)
	}
	if _, err := os.Stat(bootstrapDir); err != nil {
		t.Fatalf("retained container bootstrap directory missing: %v", err)
	}
	claim, err := core.ReadLeaseClaim(leaseID)
	if err != nil || claim.CloudID != testRecoveredContainerID || claim.Labels["state"] != pendingClaimState {
		t.Fatalf("pending claim=%#v err=%v", claim, err)
	}
	if !strings.Contains(stderr.String(), "cleanup:") {
		t.Fatalf("recovery output missing: %s", stderr.String())
	}
	core.RemoveLeaseClaim(leaseID)
	core.RemoveStoredTestboxKey(leaseID)
	_ = os.RemoveAll(bootstrapDir)
}

func TestAcquireInspectFailureKeepsExactPendingClaim(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	var leaseID string
	var bootstrapDir string
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	runner.run = func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		switch firstArg(req.Args) {
		case "run":
			leaseID = labelFromRunArgs(t, req.Args, "lease")
			bootstrapDir = bootstrapDirFromRunArgs(t, []core.LocalCommandRequest{req})
			return core.LocalCommandResult{Stdout: testRecoveredContainerID + "\n"}, nil
		case "inspect":
			return core.LocalCommandResult{Stderr: "inspect failed"}, errors.New("inspect failed")
		default:
			return core.LocalCommandResult{}, nil
		}
	}
	var stderr strings.Builder
	b := testBackend(runner)
	b.rt.Stderr = &stderr

	if _, err := b.Acquire(context.Background(), core.AcquireRequest{Keep: true, Repo: core.Repo{Root: t.TempDir()}}); err == nil {
		t.Fatal("Acquire succeeded")
	}
	keyPath, err := core.TestboxKeyPath(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("retained post-create SSH key missing: %v", err)
	}
	claim, err := core.ReadLeaseClaim(leaseID)
	if err != nil || claim.CloudID != testRecoveredContainerID || claim.Labels["bootstrap_dir"] != bootstrapDir {
		t.Fatalf("pending claim=%#v err=%v", claim, err)
	}
	if !strings.Contains(stderr.String(), "inspect:") {
		t.Fatalf("recovery output missing: %s", stderr.String())
	}
	core.RemoveLeaseClaim(leaseID)
	core.RemoveStoredTestboxKey(leaseID)
	_ = os.RemoveAll(bootstrapDir)
}

func TestPostIDRollbackFailureRetainsExactPendingClaim(t *testing.T) {
	for _, tc := range []struct {
		name      string
		createErr bool
	}{
		{name: "create error", createErr: true},
		{name: "inspect error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			var leaseID string
			var bootstrapDir string
			runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
			runner.run = func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
				switch firstArg(req.Args) {
				case "run":
					leaseID = labelFromRunArgs(t, req.Args, "lease")
					bootstrapDir = bootstrapDirFromRunArgs(t, []core.LocalCommandRequest{req})
					if tc.createErr {
						return core.LocalCommandResult{Stderr: "connection lost"}, errors.New("run failed")
					}
					return core.LocalCommandResult{Stdout: testRecoveredContainerID + "\n"}, nil
				case "ps":
					if tc.createErr && strings.Contains(strings.Join(req.Args, "\n"), "label=lease=") {
						return core.LocalCommandResult{Stdout: testRecoveredContainerID + "\n"}, nil
					}
					return core.LocalCommandResult{}, nil
				case "inspect":
					if tc.createErr {
						return core.LocalCommandResult{Stdout: `[{"Id":` + strconv.Quote(testRecoveredContainerID) + `,"Config":{"Labels":{"lease":` + strconv.Quote(leaseID) + `,"bootstrap_dir":` + strconv.Quote(bootstrapDir) + `}}}]`}, nil
					}
					return core.LocalCommandResult{Stderr: "inspect failed", ExitCode: 1}, errors.New("inspect failed")
				case "rm":
					return core.LocalCommandResult{Stderr: "daemon unavailable", ExitCode: 1}, errors.New("remove failed")
				default:
					return core.LocalCommandResult{}, nil
				}
			}
			var stderr strings.Builder
			b := testBackend(runner)
			b.rt.Stderr = &stderr
			_, err := b.Acquire(context.Background(), core.AcquireRequest{Keep: false, Repo: core.Repo{Root: t.TempDir()}})
			if err == nil || !strings.Contains(err.Error(), "remove failed") {
				t.Fatalf("Acquire error=%v", err)
			}
			claim, err := core.ReadLeaseClaim(leaseID)
			if err != nil {
				t.Fatal(err)
			}
			if claim.CloudID != testRecoveredContainerID || claim.ProviderScope != "runtime:docker/context:default" || claim.Labels["bootstrap_dir"] != bootstrapDir {
				t.Fatalf("retained claim=%#v", claim)
			}
			if !strings.Contains(stderr.String(), "cleanup:") {
				t.Fatalf("recovery output missing: %s", stderr.String())
			}
			keyPath, err := core.TestboxKeyPath(leaseID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(keyPath); err != nil {
				t.Fatalf("retained key missing: %v", err)
			}
			core.RemoveLeaseClaim(leaseID)
			core.RemoveStoredTestboxKey(leaseID)
			_ = os.RemoveAll(bootstrapDir)
		})
	}
}

func TestAcquireInspectFailureRemovesDockerSocketHostWorkRoot(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	socketPath := filepath.Join(t.TempDir(), "docker.sock")
	listener := listenUnixSocketOrSkip(t, socketPath)
	defer listener.Close()
	t.Setenv("DOCKER_HOST", "unix://"+socketPath)
	hostRoot := t.TempDir()
	var leaseID string
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	runner.run = func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		switch firstArg(req.Args) {
		case "ps":
			return core.LocalCommandResult{}, nil
		case "run":
			leaseID = labelFromRunArgs(t, req.Args, "lease")
			return core.LocalCommandResult{Stdout: "socket-inspect-failure\n"}, nil
		case "inspect":
			return core.LocalCommandResult{Stderr: "inspect failed", ExitCode: 1}, errors.New("inspect failed")
		case "rm":
			return core.LocalCommandResult{}, nil
		default:
			return core.LocalCommandResult{}, nil
		}
	}
	b := testBackend(runner)
	b.cfg.LocalContainer.DockerSocket = true
	b.cfg.LocalContainer.WorkRoot = hostRoot
	b.cfg.WorkRoot = hostRoot
	if _, err := b.Acquire(context.Background(), core.AcquireRequest{Keep: false, Repo: core.Repo{Root: t.TempDir()}}); err == nil {
		t.Fatal("Acquire succeeded")
	}
	if leaseID == "" {
		t.Fatal("lease id was not captured")
	}
	if _, err := os.Stat(filepath.Join(hostRoot, leaseID)); !os.IsNotExist(err) {
		t.Fatalf("host work root still exists after inspect rollback: %v", err)
	}
}

func TestCreateContainerRecoversEmptyContainerID(t *testing.T) {
	var bootstrapDir string
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	runner.run = func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		switch firstArg(req.Args) {
		case "run":
			bootstrapDir = bootstrapDirFromRunArgs(t, []core.LocalCommandRequest{req})
			return core.LocalCommandResult{}, nil
		case "ps":
			return core.LocalCommandResult{Stdout: testRecoveredContainerID + "\n"}, nil
		case "inspect":
			return core.LocalCommandResult{Stdout: `[{"Id":` + strconv.Quote(testRecoveredContainerID) + `,"Config":{"Labels":{"lease":"cbx_123","bootstrap_dir":` + strconv.Quote(bootstrapDir) + `}}}]`}, nil
		default:
			return core.LocalCommandResult{}, nil
		}
	}
	b := testBackend(runner)

	containerID, gotBootstrapDir, err := b.createContainer(context.Background(), b.configForRun(), "crabbox-blue", "cbx_123", "blue-lobster", "ssh-ed25519 AAAA test", false)
	if err != nil {
		t.Fatal(err)
	}
	if containerID != testRecoveredContainerID {
		t.Fatalf("container id = %q, want %s", containerID, testRecoveredContainerID)
	}
	if gotBootstrapDir != bootstrapDir {
		t.Fatalf("bootstrap directory = %q, want %q", gotBootstrapDir, bootstrapDir)
	}
	t.Cleanup(func() { _ = os.RemoveAll(bootstrapDir) })
}

func TestCreateContainerRemovesBootstrapDirWhenEmptyContainerIDIsNotFound(t *testing.T) {
	var bootstrapDir string
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	runner.run = func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		switch firstArg(req.Args) {
		case "run":
			bootstrapDir = bootstrapDirFromRunArgs(t, []core.LocalCommandRequest{req})
			return core.LocalCommandResult{}, nil
		case "ps":
			return core.LocalCommandResult{}, nil
		default:
			return core.LocalCommandResult{}, nil
		}
	}
	b := testBackend(runner)

	if _, _, err := b.createContainer(context.Background(), b.configForRun(), "crabbox-blue", "cbx_123", "blue-lobster", "ssh-ed25519 AAAA test", true); err == nil {
		t.Fatal("createContainer succeeded")
	}
	if _, err := os.Stat(bootstrapDir); !os.IsNotExist(err) {
		t.Fatalf("bootstrap directory still exists without a container: %v", err)
	}
}

func TestAcquireRollbackRemovesContainerBeforeBootstrapDir(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	var bootstrapDir string
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	runner.run = func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		switch firstArg(req.Args) {
		case "ps":
			return core.LocalCommandResult{}, nil
		case "run":
			bootstrapDir = bootstrapDirFromRunArgs(t, []core.LocalCommandRequest{req})
			return core.LocalCommandResult{Stdout: "container123456\n"}, nil
		case "inspect":
			return core.LocalCommandResult{Stdout: "{"}, nil
		case "rm":
			if _, err := os.Stat(bootstrapDir); err != nil {
				t.Fatalf("bootstrap directory removed before container rollback: %v", err)
			}
			return core.LocalCommandResult{}, nil
		default:
			return core.LocalCommandResult{}, nil
		}
	}
	b := testBackend(runner)

	if _, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}}); err == nil {
		t.Fatal("Acquire succeeded")
	}
	if _, err := os.Stat(bootstrapDir); !os.IsNotExist(err) {
		t.Fatalf("bootstrap directory still exists after acquire rollback: %v", err)
	}
	if args := recordedArgsForCommand(t, runner, "rm"); !strings.Contains(args, "container123456") {
		t.Fatalf("acquire rollback did not remove container:\n%s", args)
	}
}

func TestAcquireSSHReadinessFailurePolicy(t *testing.T) {
	tests := []struct {
		name    string
		keep    bool
		waitErr error
	}{
		{name: "keep cancellation", keep: true, waitErr: context.Canceled},
		{name: "keep timeout", keep: true, waitErr: core.Exit(5, "timed out waiting for SSH during local container ssh")},
		{name: "rollback cancellation", keep: false, waitErr: context.Canceled},
		{name: "rollback timeout", keep: false, waitErr: core.Exit(5, "timed out waiting for SSH during local container ssh")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, runner, stderr, leaseID, bootstrapDir, containerPresent := pendingAcquireBackend(t)
			b.waitForSSHReady = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error {
				return tc.waitErr
			}
			_, err := b.Acquire(context.Background(), core.AcquireRequest{
				Keep: tc.keep,
				Repo: core.Repo{Root: t.TempDir()},
			})
			if err == nil || !errors.Is(err, tc.waitErr) {
				t.Fatalf("Acquire error=%v, want %v", err, tc.waitErr)
			}
			claim, claimErr := core.ReadLeaseClaim(*leaseID)
			if claimErr != nil {
				t.Fatal(claimErr)
			}
			keyPath, keyErr := core.TestboxKeyPath(*leaseID)
			if keyErr != nil {
				t.Fatal(keyErr)
			}
			if tc.keep {
				if claim.LeaseID != *leaseID || claim.CloudID != "container-pending" || claim.ProviderScope != "runtime:docker/context:default" {
					t.Fatalf("pending claim identity=%#v", claim)
				}
				if claim.Labels["state"] != pendingClaimState || claim.Labels["recovery"] != pendingRecoveryKind || claim.Labels["ssh_key_owned"] != "true" || claim.Labels["bootstrap_owned"] != "true" {
					t.Fatalf("pending claim labels=%#v", claim.Labels)
				}
				if _, statErr := os.Stat(keyPath); statErr != nil {
					t.Fatalf("pending key missing: %v", statErr)
				}
				if _, statErr := os.Stat(*bootstrapDir); statErr != nil {
					t.Fatalf("pending bootstrap directory missing: %v", statErr)
				}
				if !*containerPresent {
					t.Fatal("kept pending container was removed")
				}
				for _, want := range []string{
					"inspect: CRABBOX_LOCAL_CONTAINER_RUNTIME='docker' DOCKER_CONTEXT='default' crabbox inspect --provider local-container --id '" + *leaseID + "' --json",
					"reclaim: CRABBOX_LOCAL_CONTAINER_RUNTIME='docker' DOCKER_CONTEXT='default' crabbox run --provider local-container --id '" + *leaseID + "' --reclaim --keep --sync-only",
					"cleanup: CRABBOX_LOCAL_CONTAINER_RUNTIME='docker' DOCKER_CONTEXT='default' crabbox stop --provider local-container --id '" + *leaseID + "'",
				} {
					if !strings.Contains(stderr.String(), want) {
						t.Fatalf("recovery output missing %q:\n%s", want, stderr.String())
					}
				}
				views, listErr := b.List(context.Background(), core.ListRequest{})
				if listErr != nil {
					t.Fatal(listErr)
				}
				if len(views) != 1 || views[0].Status == "ready" || views[0].Labels["state"] != pendingClaimState {
					t.Fatalf("pending inventory=%#v", views)
				}
				*containerPresent = false
				views, listErr = b.List(context.Background(), core.ListRequest{})
				if listErr != nil {
					t.Fatal(listErr)
				}
				if len(views) != 1 || views[0].Status != "missing" || views[0].Labels["state"] != pendingClaimState {
					t.Fatalf("stale pending inventory=%#v", views)
				}
				if cleanupErr := b.Cleanup(context.Background(), core.CleanupRequest{}); cleanupErr != nil {
					t.Fatal(cleanupErr)
				}
				if cleanupErr := b.Cleanup(context.Background(), core.CleanupRequest{}); cleanupErr != nil {
					t.Fatalf("repeated pending cleanup: %v", cleanupErr)
				}
				if afterCleanup, readErr := core.ReadLeaseClaim(*leaseID); readErr != nil || afterCleanup.LeaseID != *leaseID {
					t.Fatalf("cleanup removed pending recovery claim=%#v err=%v", afterCleanup, readErr)
				}
				lease, resolveErr := b.Resolve(context.Background(), core.ResolveRequest{ID: *leaseID, ReleaseOnly: true})
				if resolveErr != nil {
					t.Fatal(resolveErr)
				}
				if releaseErr := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); releaseErr != nil {
					t.Fatal(releaseErr)
				}
			} else {
				if claim.LeaseID != "" {
					t.Fatalf("rollback retained claim=%#v", claim)
				}
				if *containerPresent {
					t.Fatal("rollback retained container")
				}
				if _, statErr := os.Stat(keyPath); !os.IsNotExist(statErr) {
					t.Fatalf("rollback retained key: %v", statErr)
				}
				if _, statErr := os.Stat(*bootstrapDir); !os.IsNotExist(statErr) {
					t.Fatalf("rollback retained bootstrap directory: %v", statErr)
				}
				if args := recordedArgsForCommand(t, runner, "rm"); !strings.Contains(args, "container-pending") {
					t.Fatalf("rollback did not remove exact container:\n%s", args)
				}
			}
		})
	}
}

func TestAcquireTerminalContainerFailsPromptlyAndPreservesRecoveryPolicy(t *testing.T) {
	for _, tc := range []struct {
		name              string
		runtimeState      string
		terminalAtInspect int
		keep              bool
		fixed             bool
		hostWorkRoot      bool
		restart           bool
		ready             bool
		sshFailure        bool
	}{
		{name: "exited before endpoint retained", runtimeState: "exited", terminalAtInspect: 1, keep: true},
		{name: "dead before SSH retained", runtimeState: "dead", terminalAtInspect: 3, keep: true},
		{name: "podman stopped before SSH retained", runtimeState: "stopped", terminalAtInspect: 3, keep: true},
		{name: "exited during SSH retained and restarted", runtimeState: "exited", terminalAtInspect: 4, keep: true, restart: true},
		{name: "dead immediately after SSH readiness retained", runtimeState: "dead", terminalAtInspect: 4, keep: true, ready: true},
		{name: "exited SSH failure observes terminal state", runtimeState: "exited", terminalAtInspect: 4, keep: true, sshFailure: true},
		{name: "dead during SSH rolled back", runtimeState: "dead", terminalAtInspect: 4},
		{name: "exited before endpoint rolled back", runtimeState: "exited", terminalAtInspect: 1},
		{name: "exited before endpoint rolls back owned host work root", runtimeState: "exited", terminalAtInspect: 1, hostWorkRoot: true},
		{name: "fixed exited before endpoint retained", runtimeState: "exited", terminalAtInspect: 1, keep: true, fixed: true},
		{name: "fixed dead before SSH retained", runtimeState: "dead", terminalAtInspect: 3, keep: true, fixed: true},
		{name: "fixed podman stopped before SSH retained", runtimeState: "stopped", terminalAtInspect: 3, keep: true, fixed: true},
		{name: "fixed exited during SSH retained and restarted", runtimeState: "exited", terminalAtInspect: 4, keep: true, fixed: true, restart: true},
		{name: "fixed dead immediately after SSH readiness retained", runtimeState: "dead", terminalAtInspect: 4, keep: true, fixed: true, ready: true},
		{name: "fixed dead SSH failure observes terminal state", runtimeState: "dead", terminalAtInspect: 4, keep: true, fixed: true, sshFailure: true},
		{name: "fixed dead during SSH rolls back to terminal tombstone", runtimeState: "dead", terminalAtInspect: 4, fixed: true},
		{name: "fixed exited before endpoint rolls back owned host work root", runtimeState: "exited", terminalAtInspect: 1, fixed: true, hostWorkRoot: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, runner, stderr, leaseID, bootstrapDir, containerPresent := pendingAcquireBackend(t)
			var hostWorkRoot string
			if tc.hostWorkRoot {
				socketRoot, err := os.MkdirTemp("", "cbx-socket-")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.RemoveAll(socketRoot) })
				socketPath := filepath.Join(socketRoot, "docker.sock")
				listener := listenUnixSocketOrSkip(t, socketPath)
				defer listener.Close()
				t.Setenv("DOCKER_HOST", "unix://"+socketPath)
				hostWorkRoot = t.TempDir()
				b.cfg.LocalContainer.DockerSocket = true
				b.cfg.LocalContainer.WorkRoot = hostWorkRoot
				b.cfg.WorkRoot = hostWorkRoot
			}
			originalRun := runner.run
			inspectCount := 0
			terminal := true
			runner.run = func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
				result, err := originalRun(req)
				if err != nil || firstArg(req.Args) != "inspect" {
					return result, err
				}
				inspectCount++
				if !terminal || inspectCount < tc.terminalAtInspect {
					return result, nil
				}
				var containers []inspectContainer
				if err := json.Unmarshal([]byte(result.Stdout), &containers); err != nil {
					return core.LocalCommandResult{}, err
				}
				containers[0].State = inspectState{Status: tc.runtimeState}
				if tc.terminalAtInspect == 1 {
					containers[0].NetworkSettings.Ports = nil
				}
				data, err := json.Marshal(containers)
				result.Stdout = string(data)
				return result, err
			}
			waitCalled := false
			b.waitForSSHReady = func(ctx context.Context, _ *core.SSHTarget, _ io.Writer, _ string, _ time.Duration) error {
				waitCalled = true
				if tc.ready {
					return nil
				}
				if tc.sshFailure {
					return errors.New("SSH transport closed")
				}
				<-ctx.Done()
				return context.Cause(ctx)
			}

			request := core.AcquireRequest{
				Keep: tc.keep,
				Repo: core.Repo{Root: t.TempDir()},
			}
			if tc.fixed {
				request.RequestedLeaseID = "cbx_abcdef123470"
				request.RequestedSlug = "pending-test"
			}
			started := time.Now()
			_, err := b.Acquire(context.Background(), request)
			if elapsed := time.Since(started); elapsed > 5*time.Second {
				t.Fatalf("terminal acquisition took %s", elapsed)
			}
			if err == nil || !strings.Contains(err.Error(), "terminal runtime state "+tc.runtimeState) || !strings.Contains(err.Error(), shortID("container-pending")) {
				t.Fatalf("Acquire error=%v, want exact terminal container state %q", err, tc.runtimeState)
			}
			if !isExitCode(err, 5) {
				t.Fatalf("terminal acquisition returned the wrong exit classification: %v", err)
			}
			if got, want := waitCalled, tc.terminalAtInspect >= 4; got != want {
				t.Fatalf("SSH waiter called=%t, want %t", got, want)
			}
			claim, claimErr := core.ReadLeaseClaim(*leaseID)
			if claimErr != nil {
				t.Fatal(claimErr)
			}
			keyPath, keyErr := core.TestboxKeyPath(*leaseID)
			if keyErr != nil {
				t.Fatal(keyErr)
			}
			if !tc.keep {
				if *containerPresent {
					t.Fatalf("terminal rollback retained claim=%#v container=%t", claim, *containerPresent)
				}
				if tc.fixed {
					if err := fixedLocalContainerLeaseKind.ValidateTerminalClaim(claim, core.LeaseClaim{}, *leaseID, nil); err != nil {
						t.Fatalf("fixed rollback did not preserve a single-use terminal tombstone: claim=%#v err=%v", claim, err)
					}
					if _, err := b.Acquire(context.Background(), request); err == nil || !strings.Contains(err.Error(), "terminal") {
						t.Fatalf("rolled-back fixed operation was replayed: %v", err)
					}
					if creates := localContainerCreateCalls(runner); creates != 1 {
						t.Fatalf("terminal fixed tombstone permitted %d container creates", creates)
					}
				} else if claim.LeaseID != "" {
					t.Fatalf("ordinary terminal rollback retained claim=%#v", claim)
				}
				if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
					t.Fatalf("terminal rollback retained SSH key: %v", err)
				}
				if _, err := os.Stat(*bootstrapDir); !os.IsNotExist(err) {
					t.Fatalf("terminal rollback retained bootstrap directory: %v", err)
				}
				if args := recordedArgsForCommand(t, runner, "rm"); !strings.Contains(args, "container-pending") {
					t.Fatalf("terminal rollback did not remove exact container:\n%s", args)
				}
				if hostWorkRoot != "" {
					if _, err := os.Stat(filepath.Join(hostWorkRoot, *leaseID)); !os.IsNotExist(err) {
						t.Fatalf("terminal rollback retained owned host work root: %v", err)
					}
				}
				return
			}

			if claim.CloudID != "container-pending" || claim.Labels["state"] != pendingClaimState || claim.Labels["recovery"] != pendingRecoveryKind {
				t.Fatalf("retained terminal claim=%#v", claim)
			}
			if tc.fixed && (claim.Provider != core.FixedLocalContainerClaimProvider ||
				claim.FixedCreateIntent == nil || claim.FixedCreateIntent.State != "prepared" ||
				claim.FixedCreateIntent.Attempt["container_id"] != claim.CloudID ||
				claim.FixedCreateIntent.Fingerprint != claim.Labels["fixed_intent_sha256"]) {
				t.Fatalf("retained fixed claim lost its downgrade-safe marker or exact create intent: %#v", claim)
			}
			if tc.fixed && tc.terminalAtInspect >= 3 && (claim.SSHHost != "127.0.0.1" || claim.SSHPort != 49170) {
				t.Fatalf("retained fixed claim lost its discovered SSH recovery endpoint: %#v", claim)
			}
			if _, err := os.Stat(keyPath); err != nil {
				t.Fatalf("retained terminal SSH key missing: %v", err)
			}
			if _, err := os.Stat(*bootstrapDir); err != nil {
				t.Fatalf("retained terminal bootstrap directory missing: %v", err)
			}
			for _, want := range []string{
				"runtime_state=" + tc.runtimeState,
				"inspect:",
				"cleanup:",
			} {
				if !strings.Contains(stderr.String(), want) {
					t.Fatalf("terminal recovery output missing %q:\n%s", want, stderr.String())
				}
			}
			if tc.runtimeState == "dead" {
				for _, command := range []string{"restart:", "reclaim:"} {
					if strings.Contains(stderr.String(), command) {
						t.Fatalf("dead container advertised impossible %s recovery:\n%s", command, stderr.String())
					}
				}
			} else {
				for _, want := range []string{
					"restart: CRABBOX_LOCAL_CONTAINER_RUNTIME='docker' DOCKER_CONTEXT='default' 'docker' start 'container-pending'",
					"reclaim:",
				} {
					if !strings.Contains(stderr.String(), want) {
						t.Fatalf("restartable terminal recovery output missing %q:\n%s", want, stderr.String())
					}
				}
			}
			views, err := b.List(context.Background(), core.ListRequest{})
			if err != nil || len(views) != 1 || views[0].Status != tc.runtimeState || views[0].Labels["state"] != tc.runtimeState {
				t.Fatalf("terminal inventory=%#v err=%v", views, err)
			}
			statusLease, err := b.Resolve(context.Background(), core.ResolveRequest{ID: *leaseID, StatusOnly: true})
			if err != nil || statusLease.Server.Status != tc.runtimeState || statusLease.Server.Labels["state"] != tc.runtimeState {
				t.Fatalf("terminal status lease=%#v err=%v", statusLease, err)
			}
			if tc.fixed {
				bySlug, slugErr := b.Resolve(context.Background(), core.ResolveRequest{ID: request.RequestedSlug, StatusOnly: true})
				if slugErr != nil || bySlug.LeaseID != *leaseID || bySlug.Server.Status != tc.runtimeState {
					t.Fatalf("terminal fixed slug no longer routed to its exact claim: lease=%#v err=%v", bySlug, slugErr)
				}
			}
			if current, err := core.ReadLeaseClaim(*leaseID); err != nil || current.Revision != claim.Revision || current.Labels["state"] != pendingClaimState {
				t.Fatalf("terminal inventory/status changed recovery claim=%#v err=%v", current, err)
			}
			if tc.restart {
				if _, err := b.Resolve(context.Background(), core.ResolveRequest{
					ID: *leaseID, Repo: request.Repo, Reclaim: true, Prepare: true,
				}); err == nil || !strings.Contains(err.Error(), "terminal runtime state "+tc.runtimeState) {
					t.Fatalf("reclaim before exact-container restart error=%v", err)
				}
				if current, err := core.ReadLeaseClaim(*leaseID); err != nil || current.CloudID != claim.CloudID || current.Labels["state"] != pendingClaimState {
					t.Fatalf("failed terminal reclaim changed exact recovery claim=%#v err=%v", current, err)
				}
				terminal = false
				b.waitForSSHReady = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error { return nil }
				lease, err := b.Resolve(context.Background(), core.ResolveRequest{
					ID: *leaseID, Repo: request.Repo, Reclaim: true, Prepare: true,
				})
				if err != nil || lease.Server.Status != "ready" {
					t.Fatalf("restarted terminal recovery lease=%#v err=%v", lease, err)
				}
				if tc.fixed {
					if recovered, readErr := core.ReadLeaseClaim(*leaseID); readErr != nil ||
						recovered.Provider != core.FixedLocalContainerClaimProvider ||
						recovered.FixedCreateIntent.Fingerprint != claim.FixedCreateIntent.Fingerprint {
						t.Fatalf("fixed reclaim changed its protected identity: claim=%#v err=%v", recovered, readErr)
					}
				}
				if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
					t.Fatal(err)
				}
				if tc.fixed {
					tombstone, readErr := core.ReadLeaseClaim(*leaseID)
					if readErr != nil || fixedLocalContainerLeaseKind.ValidateTerminalClaim(tombstone, claim, *leaseID, nil) != nil {
						t.Fatalf("restarted fixed lease lost its single-use tombstone: claim=%#v err=%v", tombstone, readErr)
					}
				}
				return
			}
			lease, err := b.Resolve(context.Background(), core.ResolveRequest{ID: *leaseID, ReleaseOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestExactContainerReadinessRejectsReplacementIdentity(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
		commandKey([]string{"inspect", "claimed-container"}): {
			Stdout: `[{"Id":"replacement-container","State":{"Status":"running","Running":true}}]`,
		},
	}}
	b := testBackend(runner)
	b.waitForSSHReady = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error {
		t.Fatal("SSH waiter accepted a different container identity")
		return nil
	}
	lease := core.LeaseTarget{LeaseID: "cbx_exact_container", Server: core.Server{CloudID: "claimed-container"}}
	if err := b.waitForExactContainerSSHReady(context.Background(), &lease, time.Minute); err == nil ||
		!strings.Contains(err.Error(), "refusing readiness for container replacement-") {
		t.Fatalf("replacement readiness error=%v", err)
	}
	if _, err := b.waitForContainerEndpoint(context.Background(), b.configForRun(), lease.Server.CloudID, lease.LeaseID, "exact"); err == nil ||
		!strings.Contains(err.Error(), "refusing readiness for container replacement-") {
		t.Fatalf("replacement endpoint error=%v", err)
	}
}

func TestAcquireReadinessRejectsReplacementContainerIdentity(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		fixed                bool
		keep                 bool
		replacementAtInspect int
		wantError            string
	}{
		{name: "ordinary kept", keep: true, replacementAtInspect: 3, wantError: "refusing readiness for container replacement-"},
		{name: "ordinary non-kept fails closed", replacementAtInspect: 3, wantError: "refusing readiness for container replacement-"},
		{name: "ordinary first inspection fails closed", replacementAtInspect: 1, wantError: "refusing readiness for container replacement-"},
		{name: "fixed kept", fixed: true, keep: true, replacementAtInspect: 3, wantError: "refusing readiness for container replacement-"},
		{name: "fixed non-kept fails closed", fixed: true, replacementAtInspect: 3, wantError: "refusing readiness for container replacement-"},
		{name: "fixed first inspection fails closed", fixed: true, replacementAtInspect: 1, wantError: "does not match its durable container identity"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, runner, _, leaseID, bootstrapDir, containerPresent := pendingAcquireBackend(t)
			originalRun := runner.run
			inspectCount := 0
			replacement := true
			runner.run = func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
				result, err := originalRun(req)
				if err != nil || firstArg(req.Args) != "inspect" {
					return result, err
				}
				inspectCount++
				if !replacement || inspectCount < tc.replacementAtInspect {
					return result, nil
				}
				var containers []inspectContainer
				if err := json.Unmarshal([]byte(result.Stdout), &containers); err != nil {
					return core.LocalCommandResult{}, err
				}
				containers[0].ID = "replacement-container"
				data, err := json.Marshal(containers)
				result.Stdout = string(data)
				return result, err
			}
			b.waitForSSHReady = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error {
				t.Fatal("SSH waiter accepted a replacement container")
				return nil
			}
			request := core.AcquireRequest{Keep: tc.keep, Repo: core.Repo{Root: t.TempDir()}}
			if tc.fixed {
				request.RequestedLeaseID = "cbx_abcdef123471"
				request.RequestedSlug = "pending-test"
			}
			if _, err := b.Acquire(context.Background(), request); err == nil ||
				!strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("replacement acquisition error=%v", err)
			}
			claim, err := core.ReadLeaseClaim(*leaseID)
			if err != nil || claim.CloudID != "container-pending" || !*containerPresent {
				t.Fatalf("replacement changed exact ownership: claim=%#v container=%t err=%v", claim, *containerPresent, err)
			}
			if tc.fixed && claim.Provider != core.FixedLocalContainerClaimProvider {
				t.Fatalf("replacement rewrote fixed provider marker: %#v", claim)
			}
			for _, call := range runner.calls {
				if firstArg(call.Args) == "rm" {
					t.Fatalf("replacement identity triggered destructive cleanup: %v", call.Args)
				}
			}
			keyPath, err := core.TestboxKeyPath(*leaseID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(keyPath); err != nil {
				t.Fatalf("replacement identity removed the original SSH key: %v", err)
			}
			if _, err := os.Stat(*bootstrapDir); err != nil {
				t.Fatalf("replacement identity removed the original bootstrap directory: %v", err)
			}
			replacement = false
			lease, err := b.Resolve(context.Background(), core.ResolveRequest{ID: *leaseID, ReleaseOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPendingRecoveryCommandsUseSafeExactRoute(t *testing.T) {
	for _, tc := range []struct {
		name           string
		runtime        string
		context        string
		host           string
		config         string
		forbidden      []string
		wantAssignment string
	}{
		{name: "default docker", runtime: "docker", context: "default", wantAssignment: "DOCKER_CONTEXT='default'"},
		{name: "named docker context quoting", runtime: "docker", context: "team's context", wantAssignment: "DOCKER_CONTEXT=" + core.ShellQuote("team's context")},
		{name: "custom docker endpoint hidden", runtime: "docker", host: "tcp://user:password@example.invalid:2376", forbidden: []string{"password", "example.invalid", "DOCKER_HOST="}},
		{name: "custom docker config hidden", runtime: "docker", context: "private-context", config: "/private/docker/config", forbidden: []string{"/private/docker/config", "DOCKER_CONFIG="}, wantAssignment: "DOCKER_CONTEXT='private-context'"},
		{name: "named podman connection", runtime: "podman", context: "remote podman", wantAssignment: "CONTAINER_CONNECTION='remote podman'"},
		{name: "custom podman endpoint hidden", runtime: "podman", host: "ssh://user:secret@example.invalid/run/podman.sock", forbidden: []string{"secret", "example.invalid", "CONTAINER_HOST="}},
		{name: "runtime path quoting", runtime: "/opt/My Runtime/docker", context: "default", wantAssignment: "CRABBOX_LOCAL_CONTAINER_RUNTIME='/opt/My Runtime/docker'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := testBackend(&recordingRunner{})
			var stderr strings.Builder
			b.rt.Stderr = &stderr
			labels := checkpointScopeMetadata(checkpointScope{
				Runtime: tc.runtime, Context: tc.context, Host: tc.host, Config: tc.config, Endpoint: firstNonBlank(tc.host, "local"), DaemonID: "daemon-test",
			})
			labels["runtime"] = tc.runtime
			claim := core.LeaseClaim{LeaseID: "cbx_recovery_route", Provider: providerName, CloudID: "container-route", Labels: labels}
			b.printPendingRecovery(claim.LeaseID, "recovery-route", claim, errors.New("readiness failed via tcp://user:password@example.invalid and /private/docker/config"))
			output := stderr.String()
			if !strings.Contains(output, "CRABBOX_LOCAL_CONTAINER_RUNTIME="+core.ShellQuote(tc.runtime)) {
				t.Fatalf("runtime assignment missing:\n%s", output)
			}
			if tc.wantAssignment != "" && !strings.Contains(output, tc.wantAssignment) {
				t.Fatalf("route assignment %q missing:\n%s", tc.wantAssignment, output)
			}
			for _, forbidden := range append(tc.forbidden, "readiness failed", "password", "example.invalid", "/private/docker/config") {
				if strings.Contains(output, forbidden) {
					t.Fatalf("recovery output disclosed %q:\n%s", forbidden, output)
				}
			}
			for _, command := range []string{"inspect:", "reclaim:", "cleanup:"} {
				if !strings.Contains(output, command) {
					t.Fatalf("%s command missing:\n%s", command, output)
				}
			}
			if strings.Contains(output, "restart:") {
				t.Fatalf("nonterminal failure unexpectedly suggested container restart:\n%s", output)
			}

			for _, state := range []string{"exited", "stopped", "dead"} {
				stderr.Reset()
				b.printPendingRecovery(claim.LeaseID, "recovery-route", claim, &terminalContainerError{
					err: core.Exit(5, "terminal runtime state %s", state), state: state,
				})
				output = stderr.String()
				if !strings.Contains(output, "runtime_state="+state) || !strings.Contains(output, "cleanup:") {
					t.Fatalf("terminal state or cleanup command missing:\n%s", output)
				}
				wantRestart := state != "dead" && tc.host == "" && tc.config == ""
				if gotRestart := strings.Contains(output, "restart:"); gotRestart != wantRestart {
					t.Fatalf("state=%s restart=%t, want %t:\n%s", state, gotRestart, wantRestart, output)
				}
				if wantRestart && !strings.Contains(output, core.ShellQuote(tc.runtime)+" start 'container-route'") {
					t.Fatalf("exact terminal restart command missing:\n%s", output)
				}
				if gotReclaim := strings.Contains(output, "reclaim:"); gotReclaim != (state != "dead") {
					t.Fatalf("state=%s reclaim=%t, want %t:\n%s", state, gotReclaim, state != "dead", output)
				}
				for _, forbidden := range append(tc.forbidden, "password", "example.invalid", "/private/docker/config") {
					if strings.Contains(output, forbidden) {
						t.Fatalf("terminal restart output disclosed %q:\n%s", forbidden, output)
					}
				}
			}
		})
	}
}

func TestAcquireSSHReadinessSuccessFencesReadyClaim(t *testing.T) {
	b, _, _, leaseID, _, containerPresent := pendingAcquireBackend(t)
	b.waitForSSHReady = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error { return nil }
	lease, err := b.Acquire(context.Background(), core.AcquireRequest{Keep: true, Repo: core.Repo{Root: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := core.ReadLeaseClaim(*leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.Labels["state"] != "ready" || claim.Labels["recovery"] != "" {
		t.Fatalf("ready claim labels=%#v", claim.Labels)
	}
	if expected, exists, set := core.ServerLeaseClaimSnapshot(lease.Server); !set || !exists || expected.Revision != claim.Revision {
		t.Fatalf("ready snapshot set=%v exists=%v expected=%#v claim=%#v", set, exists, expected, claim)
	}
	views, err := b.List(context.Background(), core.ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].Status != "ready" || views[0].Labels["state"] != "ready" || views[0].Labels["recovery"] != "" {
		t.Fatalf("ready inventory=%#v", views)
	}
	if !*containerPresent {
		t.Fatal("ready container missing")
	}
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
}

func TestResolvePrepareRecoversPendingClaimToReady(t *testing.T) {
	b, _, _, leaseID, _, _ := pendingAcquireBackend(t)
	b.waitForSSHReady = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error {
		return core.Exit(5, "timed out waiting for SSH during local container ssh")
	}
	if _, err := b.Acquire(context.Background(), core.AcquireRequest{Keep: true, Repo: core.Repo{Root: t.TempDir()}}); err == nil {
		t.Fatal("Acquire succeeded")
	}
	b.waitForSSHReady = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error { return nil }
	lease, err := b.Resolve(context.Background(), core.ResolveRequest{
		ID:      *leaseID,
		Repo:    core.Repo{Root: t.TempDir()},
		Reclaim: true,
		Prepare: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := core.ReadLeaseClaim(*leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Server.Status != "ready" || claim.Labels["state"] != "ready" || claim.Labels["recovery"] != "" {
		t.Fatalf("recovered lease=%#v claim=%#v", lease, claim)
	}
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
}

func TestAcquireSSHReadinessSuccessCannotOverwriteNewerClaim(t *testing.T) {
	b, runner, _, leaseID, bootstrapDir, _ := pendingAcquireBackend(t)
	b.waitForSSHReady = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error {
		claim, err := core.ReadLeaseClaim(*leaseID)
		if err != nil {
			return err
		}
		labels := cloneLabels(claim.Labels)
		labels["state"] = "superseded"
		_, err = core.UpdateLeaseClaimLabelsIfUnchanged(*leaseID, claim, labels)
		return err
	}
	_, err := b.Acquire(context.Background(), core.AcquireRequest{Keep: true, Repo: core.Repo{Root: t.TempDir()}})
	if err == nil || !strings.Contains(err.Error(), "claim changed") {
		t.Fatalf("Acquire error=%v, want fenced claim change", err)
	}
	claim, err := core.ReadLeaseClaim(*leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.Labels["state"] != "superseded" {
		t.Fatalf("newer claim overwritten=%#v", claim)
	}
	for _, call := range runner.calls {
		if firstArg(call.Args) == "rm" {
			t.Fatalf("stale acquisition removed newer claim resource: %#v", call.Args)
		}
	}
	lease := core.LeaseTarget{LeaseID: *leaseID, Server: core.Server{CloudID: claim.CloudID, Labels: claim.Labels}}
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	_ = os.RemoveAll(*bootstrapDir)
}

func TestAcquireReadyCASRollsBackContainerClaimedByNewerIdentity(t *testing.T) {
	b, runner, _, leaseID, bootstrapDir, containerPresent := pendingAcquireBackend(t)
	b.waitForSSHReady = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error {
		claim, err := core.ReadLeaseClaim(*leaseID)
		if err != nil {
			return err
		}
		labels := cloneLabels(claim.Labels)
		labels["state"] = "ready"
		replacement := claim
		replacement.CloudID = "newer-container"
		replacement.Labels = labels
		return core.ReplaceLeaseClaimIfUnchanged(*leaseID, claim, replacement)
	}
	_, err := b.Acquire(context.Background(), core.AcquireRequest{Keep: true, Repo: core.Repo{Root: t.TempDir()}})
	if err == nil || !strings.Contains(err.Error(), "claim changed") {
		t.Fatalf("Acquire error=%v, want fenced claim change", err)
	}
	claim, err := core.ReadLeaseClaim(*leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.CloudID != "newer-container" {
		t.Fatalf("newer claim overwritten=%#v", claim)
	}
	if *containerPresent {
		t.Fatal("CAS loser retained its unclaimed container")
	}
	if _, err := os.Stat(*bootstrapDir); err != nil {
		t.Fatalf("CAS loser removed bootstrap directory still referenced by winner: %v", err)
	}
	if args := recordedArgsForCommand(t, runner, "rm"); !strings.Contains(args, "container-pending") {
		t.Fatalf("CAS loser did not remove exact old container:\n%s", args)
	}
	core.RemoveLeaseClaim(*leaseID)
	core.RemoveStoredTestboxKey(*leaseID)
	_ = os.RemoveAll(*bootstrapDir)
}

func TestAcquireReadinessFailureReconcilesDifferentClaim(t *testing.T) {
	b, runner, stderr, leaseID, bootstrapDir, containerPresent := pendingAcquireBackend(t)
	b.waitForSSHReady = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error {
		claim, err := core.ReadLeaseClaim(*leaseID)
		if err != nil {
			return err
		}
		labels := cloneLabels(claim.Labels)
		replacement := claim
		replacement.CloudID = "newer-container"
		replacement.Labels = labels
		if err := core.ReplaceLeaseClaimIfUnchanged(*leaseID, claim, replacement); err != nil {
			return err
		}
		return context.Canceled
	}
	_, err := b.Acquire(context.Background(), core.AcquireRequest{Keep: true, Repo: core.Repo{Root: t.TempDir()}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Acquire error=%v", err)
	}
	claim, err := core.ReadLeaseClaim(*leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.CloudID != "newer-container" {
		t.Fatalf("winning claim=%#v", claim)
	}
	if *containerPresent {
		t.Fatal("readiness failure retained displaced container")
	}
	if strings.Contains(stderr.String(), "retained provider=local-container") {
		t.Fatalf("reported displaced container as recoverable:\n%s", stderr.String())
	}
	if _, err := os.Stat(*bootstrapDir); err != nil {
		t.Fatalf("removed bootstrap directory referenced by winning claim: %v", err)
	}
	if args := recordedArgsForCommand(t, runner, "rm"); !strings.Contains(args, "container-pending") {
		t.Fatalf("readiness failure did not remove displaced container:\n%s", args)
	}
	core.RemoveLeaseClaim(*leaseID)
	core.RemoveStoredTestboxKey(*leaseID)
	_ = os.RemoveAll(*bootstrapDir)
}

func TestKeepFalseReadinessRollbackFailurePrintsRecovery(t *testing.T) {
	b, _, stderr, leaseID, _, containerPresent := pendingAcquireBackend(t)
	b.waitForSSHReady = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error {
		claim, err := core.ReadLeaseClaim(*leaseID)
		if err != nil {
			return err
		}
		labels := cloneLabels(claim.Labels)
		labels["concurrent_touch"] = "true"
		if _, err := core.UpdateLeaseClaimLabelsIfUnchanged(*leaseID, claim, labels); err != nil {
			return err
		}
		return context.Canceled
	}
	_, err := b.Acquire(context.Background(), core.AcquireRequest{Keep: false, Repo: core.Repo{Root: t.TempDir()}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Acquire error=%v", err)
	}
	if !*containerPresent {
		t.Fatal("same-identity replacement claim did not retain container")
	}
	if !strings.Contains(stderr.String(), "cleanup:") {
		t.Fatalf("keep=false retained failure omitted recovery commands:\n%s", stderr.String())
	}
	claim, err := core.ReadLeaseClaim(*leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.CloudID != "container-pending" || claim.Labels["concurrent_touch"] != "true" {
		t.Fatalf("retained claim=%#v", claim)
	}
	lease := core.LeaseTarget{LeaseID: *leaseID, Server: core.Server{CloudID: claim.CloudID, Labels: claim.Labels}}
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
}

func TestAcquirePendingClaimCannotAuthorizeReplacementContainer(t *testing.T) {
	b, runner, _, leaseID, bootstrapDir, containerPresent := pendingAcquireBackend(t)
	originalRun := runner.run
	injected := false
	runner.run = func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		result, err := originalRun(req)
		if err != nil || firstArg(req.Args) != "run" || injected {
			return result, err
		}
		injected = true
		server := core.Server{
			CloudID:  "newer-container",
			Provider: providerName,
			Labels: map[string]string{
				"provider": providerName,
				"lease":    *leaseID,
				"slug":     "newer-owner",
				"state":    "ready",
			},
		}
		claimErr := core.ClaimLeaseForRepoProviderScopePondEndpoint(
			*leaseID,
			"newer-owner",
			providerName,
			"runtime:docker/context:default",
			"",
			t.TempDir(),
			time.Minute,
			false,
			server,
			core.SSHTarget{Host: "127.0.0.1", Port: "49171"},
		)
		return result, claimErr
	}
	b.waitForSSHReady = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error {
		t.Fatal("readiness wait started before exact pending claim publication")
		return nil
	}
	_, err := b.Acquire(context.Background(), core.AcquireRequest{Keep: true, Repo: core.Repo{Root: t.TempDir()}})
	if err == nil || !strings.Contains(err.Error(), "claim changed") {
		t.Fatalf("Acquire error=%v, want claim collision", err)
	}
	claim, err := core.ReadLeaseClaim(*leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.CloudID != "newer-container" || claim.Labels["state"] != "ready" {
		t.Fatalf("newer claim was replaced=%#v", claim)
	}
	if *containerPresent {
		t.Fatal("unclaimed replacement container survived")
	}
	if _, err := os.Stat(*bootstrapDir); !os.IsNotExist(err) {
		t.Fatalf("replacement bootstrap directory survived: %v", err)
	}
	core.RemoveLeaseClaim(*leaseID)
	core.RemoveStoredTestboxKey(*leaseID)
}

func pendingAcquireBackend(t *testing.T) (*backend, *recordingRunner, *strings.Builder, *string, *string, *bool) {
	t.Helper()
	return pendingAcquireBackendWithImageFixedIntent(t, "")
}

func pendingAcquireBackendWithImageFixedIntent(t *testing.T, imageFixedIntent string) (*backend, *recordingRunner, *strings.Builder, *string, *string, *bool) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var leaseID string
	var bootstrapDir string
	var containerName string
	var slug string
	// Docker inherits image labels unless the new container overrides them.
	fixedFingerprint := imageFixedIntent
	var containerImage string
	var containerRuntimeImage string
	var containerPond string
	var containerDaemonID string
	var containerContext string
	containerPresent := false
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	runner.run = func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		if result, ok := defaultLocalContainerScopeResponse(req); ok {
			return result, nil
		}
		switch firstArg(req.Args) {
		case "ps":
			if containerPresent {
				return core.LocalCommandResult{Stdout: "container-pending\n"}, nil
			}
			return core.LocalCommandResult{}, nil
		case "run":
			leaseID = labelFromRunArgs(t, req.Args, "lease")
			slug = labelFromRunArgs(t, req.Args, "slug")
			containerRuntimeImage = req.Args[len(req.Args)-3]
			for i := 0; i+1 < len(req.Args); i++ {
				if req.Args[i] == "--name" {
					containerName = req.Args[i+1]
				}
				if req.Args[i] == "--label" {
					key, value, _ := strings.Cut(req.Args[i+1], "=")
					switch key {
					case "fixed_intent_sha256":
						fixedFingerprint = value
					case "image":
						containerImage = value
					case "pond":
						containerPond = value
					case checkpointMetadataDaemonID:
						containerDaemonID = value
					case checkpointMetadataContext:
						containerContext = value
					}
				}
			}
			bootstrapDir = bootstrapDirFromRunArgs(t, []core.LocalCommandRequest{req})
			containerPresent = true
			return core.LocalCommandResult{Stdout: "container-pending\n"}, nil
		case "inspect":
			if !containerPresent {
				return core.LocalCommandResult{Stderr: "not found", ExitCode: 1}, errors.New("not found")
			}
			labels := map[string]string{
				"crabbox": "true", "provider": providerName, "lease": leaseID, "slug": slug,
				"state": pendingClaimState, "recovery": pendingRecoveryKind, "runtime": "docker",
				"image": containerImage, "server_type": "ubuntu:24.04", "ssh_user": "runner", "work_root": "/workspace/crabbox",
				"bootstrap_dir": bootstrapDir, "ssh_key_owned": "true", "bootstrap_owned": "true", "keep": "true",
			}
			if containerDaemonID != "" {
				labels[checkpointMetadataDaemonID] = containerDaemonID
			}
			if containerContext != "" {
				labels[checkpointMetadataContext] = containerContext
			}
			if containerPond != "" {
				labels["pond"] = containerPond
			}
			if fixedFingerprint != "" {
				labels["fixed_intent_sha256"] = fixedFingerprint
			}
			data, err := json.Marshal([]inspectContainer{{
				ID: "container-pending", Name: "/" + containerName, Config: inspectConfig{Image: containerRuntimeImage, Labels: labels},
				State:           inspectState{Status: "running", Running: true},
				NetworkSettings: inspectNetworking{Ports: map[string][]inspectPort{"2222/tcp": {{HostIP: "127.0.0.1", HostPort: "49170"}}}},
			}})
			return core.LocalCommandResult{Stdout: string(data)}, err
		case "rm":
			containerPresent = false
			return core.LocalCommandResult{}, nil
		default:
			return core.LocalCommandResult{}, nil
		}
	}
	stderr := &strings.Builder{}
	b := testBackend(runner)
	b.rt.Stderr = stderr
	return b, runner, stderr, &leaseID, &bootstrapDir, &containerPresent
}

func TestAcquirePinsDockerLifecycleToCapturedContext(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var leaseID string
	var bootstrapDir string
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	runner.run = func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		if len(req.Args) < 3 || req.Args[0] != "--context" || req.Args[1] != "pinned-context" {
			t.Fatalf("Docker lifecycle command was not pinned: %v", req.Args)
		}
		args := req.Args[2:]
		switch firstArg(args) {
		case "ps":
			return core.LocalCommandResult{}, nil
		case "run":
			leaseID = labelFromRunArgs(t, args, "lease")
			bootstrapDir = bootstrapDirFromRunArgs(t, []core.LocalCommandRequest{{Args: args}})
			return core.LocalCommandResult{Stdout: "pinned-container\n"}, nil
		case "inspect":
			labels := map[string]string{
				"crabbox": "true", "provider": providerName, "lease": leaseID, "slug": "pinned",
				"state": pendingClaimState, "recovery": pendingRecoveryKind, "runtime": "docker",
				"server_type": "ubuntu:24.04", "ssh_user": "runner", "work_root": "/workspace/crabbox",
				"bootstrap_dir": bootstrapDir, checkpointMetadataContext: "pinned-context",
				checkpointMetadataEndpoint: "unix:///pinned.sock", checkpointMetadataDaemonID: "daemon-pinned",
			}
			data, err := json.Marshal([]inspectContainer{{
				ID: "pinned-container", Name: "/pinned", Config: inspectConfig{Image: "ubuntu:24.04", Labels: labels},
				State:           inspectState{Status: "running", Running: true},
				NetworkSettings: inspectNetworking{Ports: map[string][]inspectPort{"2222/tcp": {{HostIP: "127.0.0.1", HostPort: "49173"}}}},
			}})
			return core.LocalCommandResult{Stdout: string(data)}, err
		case "rm":
			return core.LocalCommandResult{}, nil
		default:
			return core.LocalCommandResult{}, nil
		}
	}
	b := testBackend(runner)
	b.captureRuntimeScope = func(context.Context, core.Config) (checkpointScope, error) {
		return checkpointScope{Runtime: "docker", Context: "pinned-context", Endpoint: "unix:///pinned.sock", DaemonID: "daemon-pinned"}, nil
	}
	b.waitForSSHReady = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error {
		return context.Canceled
	}
	if _, err := b.Acquire(context.Background(), core.AcquireRequest{Keep: true, Repo: core.Repo{Root: t.TempDir()}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Acquire error=%v", err)
	}
	claim, err := core.ReadLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.ProviderScope != "runtime:docker/context:pinned-context" || claim.Labels[checkpointMetadataDaemonID] != "daemon-pinned" {
		t.Fatalf("pinned claim=%#v", claim)
	}
	lease := core.LeaseTarget{LeaseID: leaseID, Server: core.Server{CloudID: claim.CloudID, Labels: claim.Labels}}
	if err := b.rollbackPendingLease(claim, lease, bootstrapDir); err != nil {
		t.Fatal(err)
	}
}

func TestAcquireCheckpointForkCompletesClaimRuntimeScope(t *testing.T) {
	const (
		checkpointImageID  = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
		checkpointImage    = "crabbox-checkpoint-pinned"
		checkpointUser     = "checkpoint-user"
		checkpointWorkRoot = "/checkpoint/work"
	)
	binDir := t.TempDir()
	dockerScript := `#!/bin/sh
if [ "$1" = "--context" ]; then
  shift 2
fi
case "$1" in
  info) printf '%s\n' 'daemon-pinned' ;;
  image) printf '%s\n' 'sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef' ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "docker"), []byte(dockerScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	b, runner, _, leaseID, _, _ := pendingAcquireBackendWithImageFixedIntent(t, "source-fixed-intent")
	originalRun := runner.run
	var dockerRunArgs []string
	runner.run = func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		if len(req.Args) < 3 || req.Args[0] != "--context" || req.Args[1] != "pinned-context" {
			t.Fatalf("Docker lifecycle command was not pinned: %v", req.Args)
		}
		req.Args = req.Args[2:]
		if firstArg(req.Args) == "run" {
			dockerRunArgs = append([]string(nil), req.Args...)
		}
		return originalRun(req)
	}
	checkpointMetadata := map[string]string{
		checkpointMetadataRuntime:  "docker",
		checkpointMetadataContext:  "pinned-context",
		checkpointMetadataDaemonID: "daemon-pinned",
		checkpointMetadataUser:     checkpointUser,
		checkpointMetadataWorkRoot: checkpointWorkRoot,
	}
	if err := (Provider{}).ApplyNativeCheckpointForkConfig(core.NativeCheckpointForkRequest{
		Config: &b.cfg,
		Record: core.NativeCheckpointForkRecord{
			Kind:     core.CheckpointKindDockerCommit,
			ImageID:  checkpointImageID,
			Name:     checkpointImage,
			Metadata: checkpointMetadata,
		},
	}); err != nil {
		t.Fatal(err)
	}
	applyDefaults(&b.cfg)
	captures := 0
	b.captureRuntimeScope = func(context.Context, core.Config) (checkpointScope, error) {
		captures++
		return checkpointScope{
			Runtime:  "docker",
			Context:  "pinned-context",
			Config:   "/tmp/docker-config",
			Endpoint: "unix:///pinned.sock",
			DaemonID: "daemon-pinned",
		}, nil
	}
	b.waitForSSHReady = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error { return nil }
	lease, err := b.Acquire(context.Background(), core.AcquireRequest{Keep: true, Repo: core.Repo{Root: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	if captures != 1 {
		t.Fatalf("runtime scope captures=%d, want 1", captures)
	}
	for key, want := range map[string]string{
		checkpointMetadataForkID:   checkpointImageID,
		checkpointMetadataForkName: checkpointImage,
		checkpointMetadataUser:     checkpointUser,
		checkpointMetadataWorkRoot: checkpointWorkRoot,
	} {
		if got := b.cfg.LocalContainer.CheckpointMetadata[key]; got != want {
			t.Fatalf("checkpoint metadata %s=%q, want %q", key, got, want)
		}
	}
	if !slices.Contains(dockerRunArgs, checkpointImageID) {
		t.Fatalf("Docker run did not use checkpoint image id: %v", dockerRunArgs)
	}
	for key, want := range map[string]string{
		"image":     checkpointImage,
		"ssh_user":  checkpointUser,
		"work_root": checkpointWorkRoot,
	} {
		if got := labelFromRunArgs(t, dockerRunArgs, key); got != want {
			t.Fatalf("Docker run label %s=%q, want %q", key, got, want)
		}
	}
	claim, err := core.ReadLeaseClaim(*leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if !hasCompleteCapturedRuntimeScope(claim.Labels) || claim.Labels[checkpointMetadataConfig] != "/tmp/docker-config" || claim.Labels[checkpointMetadataEndpoint] != "unix:///pinned.sock" {
		t.Fatalf("checkpoint fork claim scope=%#v", claim.Labels)
	}
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatalf("release checkpoint fork: %v", err)
	}
}

func TestFixedAcquireCheckpointForkUsesImageIDAndReplays(t *testing.T) {
	const (
		checkpointImageID  = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
		checkpointImage    = "crabbox-checkpoint-fixed"
		checkpointUser     = "checkpoint-user"
		checkpointWorkRoot = "/checkpoint/work"
	)
	binDir := t.TempDir()
	dockerScript := `#!/bin/sh
if [ "$1" = "--context" ]; then
  shift 2
fi
case "$1" in
  context) printf '%s\n' 'unix:///pinned.sock' ;;
  info) printf '%s\n' 'daemon-pinned' ;;
  image) printf '%s\n' 'sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef' ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "docker"), []byte(dockerScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	b, runner, _, _, _, _ := pendingAcquireBackendWithImageFixedIntent(t, "source-fixed-intent")
	originalRun := runner.run
	var dockerRunArgs []string
	creates := 0
	runner.run = func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		if len(req.Args) < 3 || req.Args[0] != "--context" || req.Args[1] != "pinned-context" {
			t.Fatalf("Docker lifecycle command was not pinned: %v", req.Args)
		}
		req.Args = req.Args[2:]
		if firstArg(req.Args) == "run" {
			creates++
			dockerRunArgs = append([]string(nil), req.Args...)
		}
		return originalRun(req)
	}
	if err := (Provider{}).ApplyNativeCheckpointForkConfig(core.NativeCheckpointForkRequest{
		Config: &b.cfg,
		Record: core.NativeCheckpointForkRecord{
			Kind:    core.CheckpointKindDockerCommit,
			ImageID: checkpointImageID,
			Name:    checkpointImage,
			Metadata: map[string]string{
				checkpointMetadataRuntime:  "docker",
				checkpointMetadataContext:  "pinned-context",
				checkpointMetadataDaemonID: "daemon-pinned",
				checkpointMetadataUser:     checkpointUser,
				checkpointMetadataWorkRoot: checkpointWorkRoot,
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	applyDefaults(&b.cfg)
	b.captureRuntimeScope = func(context.Context, core.Config) (checkpointScope, error) {
		return checkpointScope{
			Runtime: "docker", Context: "pinned-context", Endpoint: "unix:///pinned.sock", DaemonID: "daemon-pinned",
		}, nil
	}
	b.waitForSSHReady = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error { return nil }
	req := core.AcquireRequest{
		Repo: core.Repo{Root: t.TempDir()}, Keep: true,
		RequestedLeaseID: "cbx_abcdef123465", RequestedCheckpointID: "chk_fixed_container", RequestedSlug: "fixed-checkpoint-fork",
	}
	lease, err := b.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if image := dockerRunArgs[len(dockerRunArgs)-3]; image != checkpointImageID {
		t.Fatalf("Docker run image=%q, want captured image id %q", image, checkpointImageID)
	}
	if image := labelFromRunArgs(t, dockerRunArgs, "image"); image != checkpointImage {
		t.Fatalf("Docker run image label=%q, want checkpoint name %q", image, checkpointImage)
	}
	claim, err := core.ReadLeaseClaim(req.RequestedLeaseID)
	if err != nil || claim.Provider != core.FixedLocalContainerClaimProvider ||
		claim.FixedCreateIntent == nil || claim.FixedCreateIntent.State != "acquired" ||
		claim.FixedCreateIntent.CheckpointID != req.RequestedCheckpointID ||
		claim.FixedCreateIntent.Fingerprint == "" ||
		claim.FixedCreateIntent.Fingerprint != labelFromRunArgs(t, dockerRunArgs, "fixed_intent_sha256") {
		t.Fatalf("fixed checkpoint fork claim=%#v err=%v", claim, err)
	}
	container, err := b.inspectContainer(context.Background(), lease.Server.CloudID)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateFixedLocalContainer(container, b.configForRun(), req.RequestedLeaseID,
		req.RequestedSlug, claim.FixedCreateIntent.Fingerprint); err != nil {
		t.Fatalf("validate fixed checkpoint fork: %v", err)
	}
	replayed, err := b.Acquire(context.Background(), req)
	if err != nil || replayed.LeaseID != lease.LeaseID || replayed.Server.CloudID != lease.Server.CloudID {
		t.Fatalf("fixed checkpoint replay=%#v err=%v", replayed, err)
	}
	if creates != 1 {
		t.Fatalf("checkpoint container creates=%d, want 1", creates)
	}
	drifted := req
	drifted.RequestedCheckpointID = "chk_other_container"
	if _, err := b.Acquire(context.Background(), drifted); err == nil ||
		!strings.Contains(err.Error(), req.RequestedCheckpointID) ||
		!strings.Contains(err.Error(), drifted.RequestedCheckpointID) {
		t.Fatalf("checkpoint container mismatch err=%v", err)
	}
	if creates != 1 {
		t.Fatalf("checkpoint container creates=%d after mismatch, want 1", creates)
	}
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: replayed}); err != nil {
		t.Fatalf("release fixed checkpoint fork: %v", err)
	}
	tombstone, exists, err := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
	if err != nil || !exists || tombstone.Provider != core.FixedLocalContainerClaimProvider ||
		tombstone.FixedCreateIntent == nil || tombstone.FixedCreateIntent.State != "released" ||
		tombstone.FixedCreateIntent.Fingerprint != claim.FixedCreateIntent.Fingerprint {
		t.Fatalf("fixed checkpoint fork tombstone=%#v exists=%t err=%v", tombstone, exists, err)
	}
}

func TestUnclaimedRollbackContinuesSidecarCleanupAfterError(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_rollback_sidecars"
	keyPath, err := core.TestboxKeyPath(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	hostRoot := t.TempDir()
	markTestLocalContainerWorkRoot(t, hostRoot)
	hostLeaseRoot := filepath.Join(hostRoot, leaseID)
	if err := os.MkdirAll(hostLeaseRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	bootstrapDir, err := os.MkdirTemp("", "crabbox-bootstrap-sidecar-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(bootstrapDir) })
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
		commandKey([]string{"rm", "-f", "sidecar-container"}): {},
	}}
	b := testBackend(runner)
	b.removeAll = func(path string) error {
		if path == hostLeaseRoot {
			return errors.New("host cleanup failed")
		}
		return os.RemoveAll(path)
	}
	lease := core.LeaseTarget{LeaseID: leaseID, Server: core.Server{CloudID: "sidecar-container", Labels: map[string]string{
		"docker_socket": "1", "host_work_root": hostRoot, "work_root": "/work/crabbox",
	}}}
	retained, err := b.reconcileChangedClaim(lease, bootstrapDir)
	if retained || err == nil || !strings.Contains(err.Error(), "host cleanup failed") {
		t.Fatalf("retained=%v err=%v", retained, err)
	}
	if _, err := os.Stat(bootstrapDir); !os.IsNotExist(err) {
		t.Fatalf("bootstrap cleanup did not continue: %v", err)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("key should remain after incomplete sidecar cleanup: %v", err)
	}
}

func TestPendingRollbackContinuesSidecarCleanupAndRetainsKeyAfterError(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_pending_sidecars"
	keyPath, err := core.TestboxKeyPath(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	hostRoot := t.TempDir()
	markTestLocalContainerWorkRoot(t, hostRoot)
	hostLeaseRoot := filepath.Join(hostRoot, leaseID)
	if err := os.MkdirAll(hostLeaseRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	bootstrapDir, err := os.MkdirTemp("", "crabbox-bootstrap-pending-sidecar-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(bootstrapDir) })
	labels := map[string]string{
		"provider": providerName, "lease": leaseID, "slug": "pending-sidecars",
		"state": pendingClaimState, "recovery": pendingRecoveryKind, "keep": "false",
		"docker_socket": "1", "host_work_root": hostRoot, "work_root": "/work/crabbox",
		"bootstrap_dir": bootstrapDir,
	}
	if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(
		leaseID,
		"pending-sidecars",
		providerName,
		"runtime:docker/context:default",
		"",
		t.TempDir(),
		time.Minute,
		false,
		core.Server{CloudID: "pending-sidecar-container", Provider: providerName, Labels: labels},
		core.SSHTarget{},
	); err != nil {
		t.Fatal(err)
	}
	claim, err := core.ReadLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
		commandKey([]string{"rm", "-f", "pending-sidecar-container"}): {},
	}}
	b := testBackend(runner)
	b.removeAll = func(path string) error {
		if path == hostLeaseRoot {
			return errors.New("host cleanup failed")
		}
		return os.RemoveAll(path)
	}
	lease := core.LeaseTarget{LeaseID: leaseID, Server: core.Server{CloudID: claim.CloudID, Labels: claim.Labels}}
	err = b.rollbackPendingLease(claim, lease, bootstrapDir)
	if err == nil || !strings.Contains(err.Error(), "host cleanup failed") {
		t.Fatalf("rollback error=%v", err)
	}
	if _, err := os.Stat(bootstrapDir); !os.IsNotExist(err) {
		t.Fatalf("bootstrap cleanup did not continue: %v", err)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("key should remain after incomplete sidecar cleanup: %v", err)
	}
	if current, err := core.ReadLeaseClaim(leaseID); err != nil || current.LeaseID != leaseID {
		t.Fatalf("claim should remain for sidecar retry: %#v err=%v", current, err)
	}
	core.RemoveLeaseClaim(leaseID)
}

func TestClaimCleanupDoesNotDeleteReplacementKeyAfterFence(t *testing.T) {
	for _, action := range []string{"rollback", "release", "missing-release"} {
		t.Run(action, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			leaseID := "cbx_key_fence_" + strings.ReplaceAll(action, "-", "_")
			labels := map[string]string{"provider": providerName, "lease": leaseID, "slug": "key-fence", "state": pendingClaimState, "keep": "false"}
			labels = testCapturedScopeLabels(labels)
			cloudID := "old-container"
			if action == "missing-release" {
				cloudID = ""
			}
			if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(
				leaseID, "key-fence", providerName, "runtime:docker/context:default", "", t.TempDir(), time.Minute, false,
				core.Server{CloudID: cloudID, Provider: providerName, Labels: labels}, core.SSHTarget{},
			); err != nil {
				t.Fatal(err)
			}
			keyPath, err := core.TestboxKeyPath(leaseID)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(keyPath, []byte("old"), 0o600); err != nil {
				t.Fatal(err)
			}
			runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
			if cloudID != "" {
				runner.responses[commandKey([]string{"rm", "-f", cloudID})] = core.LocalCommandResult{}
			}
			addDefaultLocalContainerScopeResponses(runner)
			b := testBackend(runner)
			b.afterClaimCleanup = func(id string) {
				replacementLabels := map[string]string{"provider": providerName, "lease": id, "slug": "replacement", "state": "ready"}
				if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(
					id, "replacement", providerName, "runtime:docker/context:default", "", t.TempDir(), time.Minute, false,
					core.Server{CloudID: "replacement-container", Provider: providerName, Labels: replacementLabels}, core.SSHTarget{},
				); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(keyPath, []byte("replacement"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			claim, err := core.ReadLeaseClaim(leaseID)
			if err != nil {
				t.Fatal(err)
			}
			switch action {
			case "rollback":
				lease := core.LeaseTarget{LeaseID: leaseID, Server: core.Server{CloudID: cloudID, Labels: claim.Labels}}
				if err := b.rollbackPendingLease(claim, lease, ""); err != nil {
					t.Fatal(err)
				}
			case "release":
				server := core.Server{CloudID: cloudID, Labels: claim.Labels}
				core.SetServerLeaseClaimSnapshot(&server, claim, true)
				if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{LeaseID: leaseID, Server: server}}); err != nil {
					t.Fatal(err)
				}
			case "missing-release":
				lease, err := b.Resolve(context.Background(), core.ResolveRequest{ID: leaseID, ReleaseOnly: true})
				if err != nil {
					t.Fatal(err)
				}
				if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
					t.Fatal(err)
				}
			}
			data, err := os.ReadFile(keyPath)
			if err != nil || string(data) != "replacement" {
				t.Fatalf("replacement key=%q err=%v", data, err)
			}
			replacement, err := core.ReadLeaseClaim(leaseID)
			if err != nil || replacement.CloudID != "replacement-container" {
				t.Fatalf("replacement claim=%#v err=%v", replacement, err)
			}
			core.RemoveLeaseClaim(leaseID)
			core.RemoveStoredTestboxKey(leaseID)
		})
	}
}

func firstArg(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

func bootstrapDirFromRunArgs(t *testing.T, calls []core.LocalCommandRequest) string {
	t.Helper()
	for _, call := range calls {
		for i := 0; i+1 < len(call.Args); i++ {
			if call.Args[i] != "--label" || !strings.HasPrefix(call.Args[i+1], "bootstrap_dir=") {
				continue
			}
			return strings.TrimPrefix(call.Args[i+1], "bootstrap_dir=")
		}
	}
	t.Fatal("bootstrap_dir label not found in docker run args")
	return ""
}

func labelFromRunArgs(t *testing.T, args []string, key string) string {
	t.Helper()
	prefix := key + "="
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--label" && strings.HasPrefix(args[i+1], prefix) {
			return strings.TrimPrefix(args[i+1], prefix)
		}
	}
	t.Fatalf("%s label not found in docker run args", key)
	return ""
}

func TestConfigForRunFallsBackToPodmanWhenDockerIsUnavailable(t *testing.T) {
	dir := t.TempDir()
	writeExecutable(t, filepath.Join(dir, "podman"))
	t.Setenv("PATH", dir)
	b := newBackend(Provider{}.Spec(), core.BaseConfig(), core.Runtime{}).(*backend)
	got := b.configForRun()
	if got.LocalContainer.Runtime != "podman" {
		t.Fatalf("runtime=%q, want podman", got.LocalContainer.Runtime)
	}
}

func TestConfigForRunPrefersDockerWhenBothRuntimesExist(t *testing.T) {
	dir := t.TempDir()
	writeExecutable(t, filepath.Join(dir, "docker"))
	writeExecutable(t, filepath.Join(dir, "podman"))
	t.Setenv("PATH", dir)
	b := newBackend(Provider{}.Spec(), core.BaseConfig(), core.Runtime{}).(*backend)
	got := b.configForRun()
	if got.LocalContainer.Runtime != "docker" {
		t.Fatalf("runtime=%q, want docker", got.LocalContainer.Runtime)
	}
}

func TestConfigForRunHonorsExplicitRuntime(t *testing.T) {
	dir := t.TempDir()
	writeExecutable(t, filepath.Join(dir, "docker"))
	t.Setenv("PATH", dir)
	b := newBackend(Provider{}.Spec(), core.BaseConfig(), core.Runtime{}).(*backend)
	b.cfg.LocalContainer.Runtime = "podman"
	core.MarkLocalContainerRuntimeExplicit(&b.cfg)
	got := b.configForRun()
	if got.LocalContainer.Runtime != "podman" {
		t.Fatalf("runtime=%q, want explicit podman", got.LocalContainer.Runtime)
	}
}

func TestConfigForRunHonorsExplicitDockerRuntime(t *testing.T) {
	dir := t.TempDir()
	writeExecutable(t, filepath.Join(dir, "podman"))
	t.Setenv("PATH", dir)
	b := newBackend(Provider{}.Spec(), core.BaseConfig(), core.Runtime{}).(*backend)
	b.cfg.LocalContainer.Runtime = "docker"
	core.MarkLocalContainerRuntimeExplicit(&b.cfg)
	got := b.configForRun()
	if got.LocalContainer.Runtime != "docker" {
		t.Fatalf("runtime=%q, want explicit docker", got.LocalContainer.Runtime)
	}
}

func TestClaimScopeSkipsDockerContextForPodman(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	b := testBackend(runner)
	b.cfg.LocalContainer.Runtime = "podman"

	scope := b.claimScope(context.Background())
	if scope != "runtime:podman/context:default" {
		t.Fatalf("scope=%q, want podman default scope", scope)
	}
	for _, call := range runner.calls {
		if len(call.Args) > 0 && call.Args[0] == "context" {
			t.Fatalf("podman claim scope should not call context command: %#v", call.Args)
		}
	}
}

func TestRuntimeInfoSkipsDockerContextForPodman(t *testing.T) {
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"version", "--format", "{{.Client.Version}}"}): {Stdout: "5.8.2\n"},
		},
	}
	b := testBackend(runner)
	b.cfg.LocalContainer.Runtime = "podman"

	version, contextName := b.runtimeInfo(context.Background())
	if version != "5.8.2" || contextName != "default" {
		t.Fatalf("version=%q context=%q", version, contextName)
	}
	for _, call := range runner.calls {
		if len(call.Args) > 0 && call.Args[0] == "context" {
			t.Fatalf("podman runtime info should not call context command: %#v", call.Args)
		}
	}
}

func writeExecutable(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestCreateContainerPassesDesktopEnv(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	b := testBackend(runner)
	cfg := b.configForRun()
	cfg.Desktop = true
	cfg.DesktopEnv = "wayland"
	runner.responses[commandKey([]string{"run"})] = core.LocalCommandResult{Stdout: "container123456\n"}

	if _, _, err := b.createContainer(context.Background(), cfg, "crabbox-blue", "cbx_123", "blue-lobster", "ssh-ed25519 AAAA test", true); err != nil {
		t.Fatal(err)
	}
	args := recordedArgsForCommand(t, runner, "run")
	for _, want := range []string{
		"-e\nCRABBOX_DESKTOP=1",
		"-e\nCRABBOX_DESKTOP_ENV=wayland",
		"--label\ndesktop=true",
		"--label\ndesktop_env=wayland",
	} {
		if !strings.Contains(args, want) {
			t.Fatalf("docker run args missing %q:\n%s", want, args)
		}
	}
}

func TestCreateContainerMountsCacheVolumes(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	b := testBackend(runner)
	cfg := b.configForRun()
	cfg.Cache.Volumes = []core.CacheVolumeConfig{
		{Key: "my-app/linux node24 lock", Path: "/var/cache/crabbox/pnpm"},
		{Key: "npm-cache", Path: "/var/cache/crabbox/npm"},
	}
	runner.responses[commandKey([]string{"run"})] = core.LocalCommandResult{Stdout: "container123456\n"}

	if _, _, err := b.createContainer(context.Background(), cfg, "crabbox-blue", "cbx_123", "blue-lobster", "ssh-ed25519 AAAA test", true); err != nil {
		t.Fatal(err)
	}
	args := recordedArgsForCommand(t, runner, "run")
	for _, volume := range cfg.Cache.Volumes {
		want := "-v\n" + localContainerCacheVolumeName(volume.Key) + ":" + volume.Path
		if !strings.Contains(args, want) {
			t.Fatalf("cache volume mount missing %q:\n%s", want, args)
		}
	}
	for i, volume := range cfg.Cache.Volumes {
		want := "-e\nCRABBOX_CACHE_VOLUME_PATH_" + strconv.Itoa(i) + "=" + volume.Path
		if !strings.Contains(args, want) {
			t.Fatalf("cache volume path env missing %q:\n%s", want, args)
		}
	}
}

func TestLocalContainerCacheVolumeNameIsStableAndDockerSafe(t *testing.T) {
	got := localContainerCacheVolumeName("My App/linux node24 lock")
	again := localContainerCacheVolumeName("My App/linux node24 lock")
	if got != again {
		t.Fatalf("cache volume name unstable: %q then %q", got, again)
	}
	if !strings.HasPrefix(got, "crabbox-cache-my-app-linux-node24-lock-") {
		t.Fatalf("cache volume name=%q, want sanitized prefix", got)
	}
	if strings.ContainsAny(got, " /:") {
		t.Fatalf("cache volume name contains unsafe characters: %q", got)
	}
}

func TestCreateContainerCanMountDockerSocket(t *testing.T) {
	if _, err := os.Stat("/var/run/docker.sock"); err != nil {
		t.Skipf("docker socket not available: %v", err)
	}
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{},
	}
	b := testBackend(runner)
	cfg := b.configForRun()
	cfg.LocalContainer.DockerSocket = true
	cfg.LocalContainer.WorkRoot = t.TempDir()
	cfg.WorkRoot = cfg.LocalContainer.WorkRoot
	runner.responses[commandKey([]string{"run"})] = core.LocalCommandResult{Stdout: "container123456\n"}

	_, _, err := b.createContainer(context.Background(), cfg, "crabbox-blue", "cbx_123", "blue-lobster", "ssh-ed25519 AAAA test", true)
	if err != nil {
		t.Fatal(err)
	}
	args := recordedArgsForCommand(t, runner, "run")
	for _, want := range []string{
		"--label\ndocker_socket=1",
		"--label\nhost_work_root=" + cfg.LocalContainer.WorkRoot,
		"-e\nCRABBOX_DOCKER_SOCKET=1",
		"-v\n" + cfg.LocalContainer.WorkRoot + ":" + cfg.LocalContainer.WorkRoot,
		"-v\n/var/run/docker.sock:/var/run/docker.sock",
	} {
		if !strings.Contains(args, want) {
			t.Fatalf("docker socket run args missing %q:\n%s", want, args)
		}
	}
}

func TestCreateContainerMapsDefaultDockerSocketWorkRootToContainerPath(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("default Docker socket cache-root traversal bug is Linux-specific")
	}
	cacheRoot := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cacheRoot)
	socketDir := t.TempDir()
	socketPath := filepath.Join(socketDir, "docker.sock")
	listener := listenUnixSocketOrSkip(t, socketPath)
	defer listener.Close()
	t.Setenv("DOCKER_HOST", "unix://"+socketPath)
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{},
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.LocalContainer.DockerSocket = true
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)
	cfg = b.configForRun()
	hostWorkRoot := defaultDockerSocketWorkRoot()
	if cfg.LocalContainer.WorkRoot != hostWorkRoot || cfg.WorkRoot != hostWorkRoot {
		t.Fatalf("default host work root local=%q global=%q want %q", cfg.LocalContainer.WorkRoot, cfg.WorkRoot, hostWorkRoot)
	}
	runner.responses[commandKey([]string{"run"})] = core.LocalCommandResult{Stdout: "container123456\n"}

	_, _, err := b.createContainer(context.Background(), cfg, "crabbox-blue", "cbx_123", "blue-lobster", "ssh-ed25519 AAAA test", true)
	if err != nil {
		t.Fatal(err)
	}
	args := recordedArgsForCommand(t, runner, "run")
	for _, want := range []string{
		"--label\nhost_work_root=" + hostWorkRoot,
		"--label\nwork_root=/work/crabbox",
		"-e\nCRABBOX_WORK_ROOT=/work/crabbox",
		"-v\n" + hostWorkRoot + ":/work/crabbox",
	} {
		if !strings.Contains(args, want) {
			t.Fatalf("default docker socket work root args missing %q:\n%s", want, args)
		}
	}
	if strings.Contains(args, "-v\n"+hostWorkRoot+":"+hostWorkRoot) {
		t.Fatalf("default docker socket work root mounted to host path inside container:\n%s", args)
	}

	container := inspectContainer{
		ID:   "container1234567890",
		Name: "/crabbox-blue",
		Config: inspectConfig{
			Image: "ubuntu:24.04",
			Labels: map[string]string{
				"crabbox":        "true",
				"provider":       providerName,
				"lease":          "cbx_123",
				"slug":           "blue-lobster",
				"state":          "ready",
				"server_type":    "ubuntu:24.04",
				"ssh_user":       "crabbox",
				"docker_socket":  "1",
				"host_work_root": hostWorkRoot,
				"work_root":      "/work/crabbox",
			},
		},
		State: inspectState{Status: "running", Running: true},
		NetworkSettings: inspectNetworking{
			Ports: map[string][]inspectPort{"2222/tcp": {{HostIP: "127.0.0.1", HostPort: "49153"}}},
		},
	}
	lease, err := b.prepareLease(context.Background(), cfg, container, "cbx_123", "blue-lobster", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(lease.SSH.ReadyCheck, "test -d '/work/crabbox'") {
		t.Fatalf("ready check did not use container work root: %q", lease.SSH.ReadyCheck)
	}
	if strings.Contains(lease.SSH.ReadyCheck, hostWorkRoot) || strings.Contains(lease.SSH.ReadyCheck, "/root/") {
		t.Fatalf("ready check used host-only work root: %q", lease.SSH.ReadyCheck)
	}
}

func TestCreateContainerMountsDockerHostUnixSocket(t *testing.T) {
	socketDir := t.TempDir()
	socketPath := filepath.Join(socketDir, "docker.sock")
	listener := listenUnixSocketOrSkip(t, socketPath)
	defer listener.Close()
	t.Setenv("DOCKER_HOST", "unix://"+socketPath)
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{},
	}
	b := testBackend(runner)
	cfg := b.configForRun()
	cfg.LocalContainer.DockerSocket = true
	cfg.LocalContainer.WorkRoot = t.TempDir()
	cfg.WorkRoot = cfg.LocalContainer.WorkRoot
	rootInfo, err := os.Stat(cfg.LocalContainer.WorkRoot)
	if err != nil {
		t.Fatal(err)
	}
	rootMode := rootInfo.Mode().Perm()
	runner.responses[commandKey([]string{"run"})] = core.LocalCommandResult{Stdout: "container123456\n"}

	_, _, err = b.createContainer(context.Background(), cfg, "crabbox-blue", "cbx_123", "blue-lobster", "ssh-ed25519 AAAA test", true)
	if err != nil {
		t.Fatal(err)
	}
	args := recordedArgsForCommand(t, runner, "run")
	wantSocketPath := socketPath
	if runtime.GOOS != "linux" {
		wantSocketPath = "/var/run/docker.sock"
	}
	if !strings.Contains(args, "-v\n"+wantSocketPath+":/var/run/docker.sock") {
		t.Fatalf("docker host socket was not mounted:\n%s", args)
	}
	rootInfo, err = os.Stat(cfg.LocalContainer.WorkRoot)
	if err != nil {
		t.Fatal(err)
	}
	if rootInfo.Mode().Perm() != rootMode {
		t.Fatalf("host work root mode=%#o want preserved %#o", rootInfo.Mode().Perm(), rootMode)
	}
	leaseRoot := filepath.Join(cfg.LocalContainer.WorkRoot, "cbx_123")
	info, err := os.Stat(leaseRoot)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o777 {
		t.Fatalf("lease work root mode=%#o want 0777", info.Mode().Perm())
	}
}

func TestCreateContainerMountsPodmanSocketWithSecurityOpt(t *testing.T) {
	socketDir := t.TempDir()
	socketPath := filepath.Join(socketDir, "podman.sock")
	listener := listenUnixSocketOrSkip(t, socketPath)
	defer listener.Close()
	t.Setenv("DOCKER_HOST", "unix://"+socketPath)
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{},
	}
	b := testBackend(runner)
	cfg := b.configForRun()
	cfg.LocalContainer.Runtime = "podman"
	cfg.LocalContainer.DockerSocket = true
	cfg.LocalContainer.WorkRoot = t.TempDir()
	cfg.WorkRoot = cfg.LocalContainer.WorkRoot
	runner.responses[commandKey([]string{"run"})] = core.LocalCommandResult{Stdout: "container123456\n"}

	_, _, err := b.createContainer(context.Background(), cfg, "crabbox-blue", "cbx_123", "blue-lobster", "ssh-ed25519 AAAA test", true)
	if err != nil {
		t.Fatal(err)
	}
	args := recordedArgsForCommand(t, runner, "run")
	wantSocketPath := socketPath
	if runtime.GOOS != "linux" {
		wantSocketPath = "/var/run/docker.sock"
	}
	for _, want := range []string{
		"-v\n" + wantSocketPath + ":/var/run/docker.sock",
		"--security-opt\nlabel=disable",
	} {
		if !strings.Contains(args, want) {
			t.Fatalf("podman socket run args missing %q:\n%s", want, args)
		}
	}
}

func TestCreateContainerCleansDockerSocketLeaseRootOnMountError(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:2375")
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{},
	}
	b := testBackend(runner)
	cfg := b.configForRun()
	cfg.LocalContainer.DockerSocket = true
	cfg.LocalContainer.WorkRoot = t.TempDir()
	cfg.WorkRoot = cfg.LocalContainer.WorkRoot

	_, _, err := b.createContainer(context.Background(), cfg, "crabbox-blue", "cbx_123", "blue-lobster", "ssh-ed25519 AAAA test", true)
	if err == nil {
		t.Fatal("createContainer succeeded")
	}
	if _, statErr := os.Stat(filepath.Join(cfg.LocalContainer.WorkRoot, "cbx_123")); !os.IsNotExist(statErr) {
		t.Fatalf("host lease root still exists after docker socket mount error: %v", statErr)
	}
	if _, statErr := os.Stat(cfg.LocalContainer.WorkRoot); statErr != nil {
		t.Fatalf("host work root parent removed after docker socket mount error: %v", statErr)
	}
	if !trustedLocalContainerWorkRoot(cfg.LocalContainer.WorkRoot) {
		t.Fatalf("host work root marker missing after docker socket mount error")
	}
	if len(runner.calls) != 0 {
		t.Fatalf("docker should not be invoked after mount path rejection: %#v", runner.commandSummary())
	}
}

func TestCreateContainerDoesNotDeletePreexistingDockerSocketLeasePath(t *testing.T) {
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{},
	}
	b := testBackend(runner)
	cfg := b.configForRun()
	cfg.LocalContainer.DockerSocket = true
	cfg.LocalContainer.WorkRoot = t.TempDir()
	cfg.WorkRoot = cfg.LocalContainer.WorkRoot
	leasePath := filepath.Join(cfg.LocalContainer.WorkRoot, "cbx_123")
	if err := os.WriteFile(leasePath, []byte("preexisting"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, err := b.createContainer(context.Background(), cfg, "crabbox-blue", "cbx_123", "blue-lobster", "ssh-ed25519 AAAA test", true)
	if err == nil {
		t.Fatal("createContainer succeeded")
	}
	data, readErr := os.ReadFile(leasePath)
	if readErr != nil {
		t.Fatalf("preexisting lease path was removed: %v", readErr)
	}
	if string(data) != "preexisting" {
		t.Fatalf("preexisting lease path content = %q", data)
	}
}

func TestDockerSocketMountRejectsRemoteDockerHost(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:2375")
	b := testBackend(&recordingRunner{responses: map[string]core.LocalCommandResult{}})
	if _, err := b.dockerSocketMountPath(context.Background()); err == nil {
		t.Fatal("remote docker host accepted")
	}
}

func TestDockerSocketMountUsesDaemonSocketForNonLinuxClient(t *testing.T) {
	path, err := dockerSocketMountPathFromHostForGOOS("unix:///var/run/docker.sock", "darwin")
	if err != nil {
		t.Fatal(err)
	}
	if path != "/var/run/docker.sock" {
		t.Fatalf("path=%q, want daemon-visible socket", path)
	}
}

func TestDockerSocketMountUsesDaemonSocketForWindowsPipe(t *testing.T) {
	path, err := dockerSocketMountPathFromHostForGOOS(`npipe:////./pipe/docker_engine`, "windows")
	if err != nil {
		t.Fatal(err)
	}
	if path != "/var/run/docker.sock" {
		t.Fatalf("path=%q, want daemon-visible socket", path)
	}
}

func TestDockerSocketWorkRootsUseLinuxGuestPathForWindows(t *testing.T) {
	host, guest := dockerSocketWorkRootsForGOOS(`C:\crabbox\local-container-work`, "windows")
	if host != `C:\crabbox\local-container-work` {
		t.Fatalf("host root=%q", host)
	}
	if guest != "/work/crabbox" {
		t.Fatalf("guest root=%q, want /work/crabbox", guest)
	}

	host, guest = dockerSocketWorkRootsForGOOS("/work/custom", "windows")
	if !strings.Contains(host, "crabbox") || guest != "/work/custom" {
		t.Fatalf("default Windows socket roots host=%q guest=%q", host, guest)
	}
}

func TestDockerSocketWorkRootsUseContainerPathForDefaultLinuxRoot(t *testing.T) {
	hostDefault := defaultDockerSocketWorkRoot()
	host, guest := dockerSocketWorkRootsForGOOS(hostDefault, "linux")
	if host != hostDefault {
		t.Fatalf("host root=%q, want %q", host, hostDefault)
	}
	if guest != "/work/crabbox" {
		t.Fatalf("guest root=%q, want /work/crabbox", guest)
	}

	host, guest = dockerSocketWorkRootsForGOOSExplicit(hostDefault, "linux", true)
	if host != hostDefault || guest != hostDefault {
		t.Fatalf("explicit default-path work root should preserve existing behavior, host=%q guest=%q", host, guest)
	}

	host, guest = dockerSocketWorkRootsForGOOS("/tmp/custom-crabbox", "linux")
	if host != "/tmp/custom-crabbox" || guest != "/tmp/custom-crabbox" {
		t.Fatalf("custom Linux socket roots host=%q guest=%q", host, guest)
	}
}

func TestPrepareLeaseUsesLabeledGuestWorkRootForReadyCheck(t *testing.T) {
	b := testBackend(&recordingRunner{responses: map[string]core.LocalCommandResult{}})
	cfg := b.configForRun()
	cfg.LocalContainer.DockerSocket = true
	cfg.LocalContainer.WorkRoot = `C:\crabbox\local-container-work`
	cfg.WorkRoot = cfg.LocalContainer.WorkRoot
	container := inspectContainer{
		ID:   "container1234567890",
		Name: "/crabbox-windows-root",
		Config: inspectConfig{
			Image: "ubuntu:24.04",
			Labels: map[string]string{
				"crabbox":        "true",
				"provider":       providerName,
				"lease":          "cbx_windows",
				"slug":           "windows-root",
				"state":          "ready",
				"server_type":    "ubuntu:24.04",
				"ssh_user":       "runner",
				"docker_socket":  "1",
				"host_work_root": `C:\crabbox\local-container-work`,
				"work_root":      "/work/crabbox",
			},
		},
		State: inspectState{Status: "running", Running: true},
		NetworkSettings: inspectNetworking{
			Ports: map[string][]inspectPort{"2222/tcp": []inspectPort{{HostIP: "127.0.0.1", HostPort: "49153"}}},
		},
	}

	lease, err := b.prepareLease(context.Background(), cfg, container, "cbx_windows", "windows-root", false)
	if err != nil {
		t.Fatal(err)
	}
	if lease.SSH.ReadyCheck == "" || !strings.Contains(lease.SSH.ReadyCheck, "test -d '/work/crabbox'") {
		t.Fatalf("ready check did not use guest work root: %q", lease.SSH.ReadyCheck)
	}
	if strings.Contains(lease.SSH.ReadyCheck, `C:\crabbox`) {
		t.Fatalf("ready check used host work root: %q", lease.SSH.ReadyCheck)
	}
	if lease.SSH.User != "runner" || lease.SSH.Port != "49153" {
		t.Fatalf("unexpected lease target: %#v", lease.SSH)
	}
}

func TestConfigForRunUsesHostVisibleWorkRootWithDockerSocket(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.LocalContainer.DockerSocket = true
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)
	got := b.configForRun()
	if runtime.GOOS == "windows" {
		if got.LocalContainer.WorkRoot != "/work/crabbox" || got.WorkRoot != "/work/crabbox" {
			t.Fatalf("windows docker socket work root should stay Linux-visible: %#v", got.LocalContainer)
		}
		return
	}
	if got.LocalContainer.WorkRoot == "/work/crabbox" || got.WorkRoot == "/work/crabbox" {
		t.Fatalf("docker socket work root should be host-visible: %#v", got.LocalContainer)
	}
	if !strings.Contains(got.LocalContainer.WorkRoot, "crabbox") || got.WorkRoot != got.LocalContainer.WorkRoot {
		t.Fatalf("unexpected docker socket work root: workRoot=%q local=%q", got.WorkRoot, got.LocalContainer.WorkRoot)
	}
}

func TestConfigForRunPreservesExplicitDockerSocketWorkRoot(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.LocalContainer.DockerSocket = true
	cfg.LocalContainer.WorkRoot = "/work/crabbox"
	cfg.WorkRoot = "/work/crabbox"
	core.MarkWorkRootExplicit(&cfg)
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)
	got := b.configForRun()
	if got.LocalContainer.WorkRoot != "/work/crabbox" || got.WorkRoot != "/work/crabbox" {
		t.Fatalf("explicit work root was replaced: workRoot=%q local=%q", got.WorkRoot, got.LocalContainer.WorkRoot)
	}
}

func TestConfigForRunPreservesProviderSpecificExplicitDockerSocketWorkRoot(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.LocalContainer.DockerSocket = true
	cfg.LocalContainer.WorkRoot = "/work/crabbox"
	cfg.WorkRoot = "/work/crabbox"
	core.MarkLocalContainerWorkRootExplicit(&cfg)
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)
	got := b.configForRun()
	if got.LocalContainer.WorkRoot != "/work/crabbox" || got.WorkRoot != "/work/crabbox" {
		t.Fatalf("provider-specific explicit work root was replaced: workRoot=%q local=%q", got.WorkRoot, got.LocalContainer.WorkRoot)
	}
}

func TestLocalContainerWorkRootFlagMarksWorkRootExplicit(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	values := registerFlags(fs, cfg)
	if err := fs.Set("local-container-work-root", "/work/crabbox"); err != nil {
		t.Fatal(err)
	}
	if err := applyFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if !core.IsWorkRootExplicit(&cfg) {
		t.Fatal("local-container work root flag did not mark work root explicit")
	}
	if !core.LocalContainerWorkRootExplicit(cfg) {
		t.Fatal("local-container work root flag did not mark provider work root explicit")
	}
}

func TestLocalContainerReadyCheckReportsFailureDiagnostics(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.LocalContainer.WorkRoot = "/work/crabbox"
	got := localContainerReadyCheck(cfg)
	for _, want := range []string{
		"local-container ready-check failed:",
		"/tmp/crabbox-ready.log",
		"cat /tmp/crabbox-ready.log >&2",
		"test -d '/work/crabbox'",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("ready check missing diagnostic %q: %s", want, got)
		}
	}
}

func TestBootstrapScriptUsesAccountHomeDirectory(t *testing.T) {
	for _, want := range []string{
		`home_dir="$(getent passwd "$user" | cut -d: -f6)"`,
		`image_path="${PATH:-/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin}"`,
		`printf '%s' "$image_path" > "$home_dir/.config/crabbox/image-path"`,
		`chmod 0600 "$home_dir/.config/crabbox/image-path"`,
		`image_path_hook=/etc/profile.d/crabbox-image-path.sh`,
		`image_path_hook_tmp="$(mktemp "${image_path_hook}.tmp.XXXXXX")"`,
		`chown 0:0 "$image_path_hook_tmp"`,
		`chmod 0644 "$image_path_hook_tmp"`,
		`mv -f "$image_path_hook_tmp" "$image_path_hook"`,
		`for candidate in "$home_dir/.bash_profile" "$home_dir/.bash_login" "$home_dir/.profile"; do`,
		`python3 - "$login_profile" <<'PY'`,
		`def find_line_sequence(content, sequence, offset=0):`,
		`data = data[:remove_from] + suffix`,
		`PATH="$(/bin/cat "$HOME/.config/crabbox/image-path"; printf '\001')"`,
		`PATH="${PATH%?}"`,
		`path.write_bytes(data + block)`,
		`# end crabbox managed image PATH`,
		`"$home_dir/.ssh/authorized_keys"`,
		`sed -i 's/^[#[:space:]]*UsePAM[[:space:]].*/UsePAM no/' /etc/ssh/sshd_config`,
		`printf '\nUsePAM no\n' >> /etc/ssh/sshd_config`,
		`sed -i 's/^[#[:space:]]*PasswordAuthentication[[:space:]].*/PasswordAuthentication no/' /etc/ssh/sshd_config`,
		`passwd -d "$user" >/dev/null 2>&1 || true`,
		`if [ "${CRABBOX_DOCKER_SOCKET:-0}" = "1" ]; then`,
		`chown -R "$user" "$home_dir/.ssh"`,
		`chown -R "$user" "$home_dir/.ssh" "$work_root"`,
		`arc-theme`,
		`"$config_dir/xfce4/xfconf/xfce-perchannel-xml/xsettings.xml"`,
		`mode="${CRABBOX_DESKTOP_THEME:-}"`,
		`"$config_dir/crabbox/desktop-theme"`,
		`gtk_theme=Adwaita-dark`,
		`gtk_candidates="Arc-Dark Greybird-dark Adwaita-dark Greybird"`,
		`gtk_candidates="Arc Greybird Adwaita"`,
		`xfwm_theme=Default`,
		`xfwm_candidates="Arc-Dark Greybird-dark Daloa Default"`,
		`xfwm_candidates="Arc Greybird Daloa Default"`,
		`ThemeName" type="string" value="$gtk_theme"`,
		`"$config_dir/xfce4/xfconf/xfce-perchannel-xml/xfwm4.xml"`,
		`theme" type="string" value="$xfwm_theme"`,
		`gtk-application-prefer-dark-theme=$gtk_prefer_dark_ini`,
		`xfconf-query -c xsettings -p /Gtk/ApplicationPreferDarkTheme`,
		`xfconf-query -c xfwm4 -p /general/theme`,
		`xfconf-query -c xfwm4 -p /general/box_move`,
		`xfconf-query -c xfwm4 -p /general/box_resize`,
		`xfconf-query -c xfwm4 -p /general/move_opacity`,
		`xfconf-query -c xfwm4 -p /general/resize_opacity`,
		`xfconf-query -c xfwm4 -p /general/snap_to_border`,
		`xfconf-query -c xfwm4 -p /general/snap_width`,
		`xfconf-query -c xfwm4 -p /general/tile_on_move`,
		`xfconf-query -c xfwm4 -p /general/use_compositing`,
		`xfconf-query -c xfwm4 -p /general/wrap_windows`,
		`xfconf-query -c xfce4-panel -p /panels/dark-mode`,
		`/panels/$panel_id/background-rgba`,
		`crabbox desktop theme start`,
		`crabbox-xfce4-panel-$user.log`,
		`pkill -TERM -x xfce4-panel`,
		`xfwm4 --replace --compositor=off`,
		`-wait 16 -defer 8 -nowait_bog`,
		`wayvnc --config '$home_dir/.config/wayvnc/config' --render-cursor --max-fps=60`,
		`gsettings set org.gnome.desktop.interface color-scheme '$gsettings_scheme'`,
		`if [ "$(id -u)" -eq 0 ]; then`,
		`mkdir -p "$config_dir/crabbox" "$config_dir/gtk-3.0" "$config_dir/gtk-4.0" "$config_dir/labwc"`,
		`dbus_address="${DBUS_SESSION_BUS_ADDRESS:-}"`,
		`DBUS_SESSION_BUS_ADDRESS='$dbus_address' GDK_BACKEND=x11 gsettings set org.gnome.desktop.interface color-scheme`,
		`DISPLAY="$display" XDG_RUNTIME_DIR="$runtime" DBUS_SESSION_BUS_ADDRESS="$dbus_address" GDK_BACKEND=x11 gsettings set org.gnome.desktop.interface color-scheme "$gsettings_scheme"`,
		`"$config_dir/labwc/themerc-override"`,
		`window.active.title.bg.color`,
		`window.active.button.unpressed.image.color`,
		`LABWC_PID="$labwc_pid"`,
		`labwc --reconfigure`,
		`kill -HUP "$labwc_pid"`,
		`"$config_dir/gtk-3.0/gtk.css"`,
		`menubar menuitem`,
		`desktop-background-$mode.svg`,
		`swaybg -i "$wallpaper_file" -m fill`,
		`status=$?`,
		`[ "$status" -lt 128 ] || exit "$status"`,
		`exec env XDG_RUNTIME_DIR="$runtime"`,
		`) </dev/null >/tmp/crabbox-swaybg.log 2>&1 &`,
		`nohup gnome-panel >/tmp/crabbox-gnome-panel.log 2>&1 &`,
		`elif [ "$(id -u)" -ne 0 ] && pgrep -x gnome-panel`,
	} {
		if !strings.Contains(bootstrapScript, want) {
			t.Fatalf("bootstrap script missing %q", want)
		}
	}
	if strings.Contains(bootstrapScript, "2>&1) &") {
		t.Fatal("bootstrap script leaves a swaybg wrapper attached to its caller")
	}
	if strings.Contains(bootstrapScript, `|| XDG_RUNTIME_DIR="$runtime"`) {
		t.Fatal("bootstrap script can launch a stale fallback swaybg after termination")
	}
}

func TestBootstrapImagePathHookContract(t *testing.T) {
	if strings.Count(bootstrapScript, localContainerImagePathRestoreBlock) != 2 {
		t.Fatalf("restore block should be reused exactly by system hook and selected profile")
	}
	if strings.Contains(localContainerImagePathRestoreBlock, "eval") {
		t.Fatal("image PATH restore block must not evaluate saved content")
	}
	for _, want := range []string{
		"# crabbox managed image PATH\n",
		`PATH="$(/bin/cat "$HOME/.config/crabbox/image-path"; printf '\001')"`,
		`PATH="${PATH%?}"`,
		"# end crabbox managed image PATH\n",
	} {
		if !strings.Contains(localContainerImagePathRestoreBlock, want) {
			t.Fatalf("restore block missing %q", want)
		}
	}

	install := strings.Join([]string{
		`image_path_hook=/etc/profile.d/crabbox-image-path.sh`,
		`install -d -m 0755 /etc/profile.d`,
		`image_path_hook_tmp="$(mktemp "${image_path_hook}.tmp.XXXXXX")"`,
		`cat > "$image_path_hook_tmp" <<'CRABBOX_IMAGE_PATH_HOOK'`,
		localContainerImagePathRestoreBlock + `CRABBOX_IMAGE_PATH_HOOK`,
		`chown 0:0 "$image_path_hook_tmp"`,
		`chmod 0644 "$image_path_hook_tmp"`,
		`mv -f "$image_path_hook_tmp" "$image_path_hook"`,
	}, "\n")
	if !strings.Contains(bootstrapScript, install) {
		t.Fatal("image PATH hook is not installed atomically with root ownership and mode 0644")
	}

	savedPath := strings.Index(bootstrapScript, `printf '%s' "$image_path" > "$home_dir/.config/crabbox/image-path"`)
	hook := strings.Index(bootstrapScript, `image_path_hook=/etc/profile.d/crabbox-image-path.sh`)
	profile := strings.Index(bootstrapScript, `login_profile=""`)
	sshd := strings.LastIndex(bootstrapScript, `exec /usr/sbin/sshd`)
	if savedPath < 0 || hook <= savedPath || profile <= hook || sshd <= profile {
		t.Fatalf("unexpected image PATH bootstrap order: saved=%d hook=%d profile=%d sshd=%d", savedPath, hook, profile, sshd)
	}
}

func TestBootstrapImagePathRestoreIsIdempotentAndPreservesLiteralBytes(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skipf("bash unavailable: %v", err)
	}
	home := t.TempDir()
	marker := filepath.Join(home, "must-not-exist")
	want := "/opt/tool dir:$HOME:$(touch " + marker + "):`false`:\n"
	pathDir := filepath.Join(home, ".config", "crabbox")
	if err := os.MkdirAll(pathDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pathDir, "image-path"), []byte(want), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bash, "-c", `set -eu
HOME="$1"
`+localContainerImagePathRestoreBlock+localContainerImagePathRestoreBlock+`
printf '%s' "$PATH"
`, "bash", home)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("read image PATH: %v: %s", err, out)
	}
	if got := string(out); got != want {
		t.Fatalf("image PATH = %q, want %q", got, want)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("image PATH content was evaluated: %v", err)
	}
}

func TestBootstrapImagePathProfileCanonicalization(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skipf("python3 unavailable: %v", err)
	}
	const opener = "python3 - \"$login_profile\" <<'PY'\n"
	const closer = "\nPY\nchown \"$user\" \"$login_profile\""
	start := strings.Index(bootstrapScript, opener)
	if start < 0 {
		t.Fatal("bootstrap script missing profile transformation opener")
	}
	start += len(opener)
	end := strings.Index(bootstrapScript[start:], closer)
	if end < 0 {
		t.Fatal("bootstrap script missing profile transformation closer")
	}
	script := bootstrapScript[start : start+end]
	profile := filepath.Join(t.TempDir(), ".bash_profile")
	run := func(input string) string {
		t.Helper()
		if err := os.WriteFile(profile, []byte(input), 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(python, "-", profile)
		cmd.Stdin = strings.NewReader(script)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("canonicalize login profile: %v: %s", err, out)
		}
		out, err := os.ReadFile(profile)
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}

	canonical := run("")
	if strings.Count(canonical, "# crabbox managed image PATH\n") != 1 ||
		!strings.HasSuffix(canonical, "# end crabbox managed image PATH\n") {
		t.Fatalf("unexpected canonical block: %q", canonical)
	}
	if got := run(canonical); got != canonical {
		t.Fatalf("canonicalization is not idempotent:\nfirst:  %q\nsecond: %q", canonical, got)
	}

	withLaterOverride := "PATH=/before\n" + canonical + "PATH=/after\n"
	got := run(withLaterOverride)
	if !strings.Contains(got, "PATH=/before\nPATH=/after\n") ||
		strings.Count(got, canonical) != 1 || !strings.HasSuffix(got, canonical) {
		t.Fatalf("later profile content was not preserved before one final managed block: %q", got)
	}

	heredoc := "cat <<'PROFILE'\n# crabbox managed image PATH\nnot the managed block\n# end crabbox managed image PATH\nPROFILE\n"
	got = run(heredoc)
	if !strings.HasPrefix(got, heredoc) || strings.Count(got, "# crabbox managed image PATH\n") != 2 {
		t.Fatalf("line-aligned marker text was modified: %q", got)
	}

	unmatched := "# crabbox managed image PATH\nPATH=/user\n"
	got = run(unmatched)
	if !strings.HasPrefix(got, unmatched) || strings.Count(got, "# crabbox managed image PATH\n") != 2 {
		t.Fatalf("unmatched marker was modified: %q", got)
	}
}

func TestBootstrapScriptSupportsWaylandDesktop(t *testing.T) {
	for _, want := range []string{
		`CRABBOX_DESKTOP_ENV:-xfce`,
		`labwc wayvnc foot grim slurp wtype wl-clipboard wlr-randr`,
		`xdg-desktop-portal-wlr`,
		`CRABBOX_DESKTOP_ENV=$desktop_env`,
		`.config/labwc/autostart`,
		`wlr-randr --output HEADLESS-1 --custom-mode 1920x1080`,
		`foot --title='Crabbox Desktop' >/tmp/crabbox-foot.log 2>&1 &`,
		`for socket in "$runtime"/wayland-*`,
		`display="${socket##*/}"`,
		`desktop_env="${CRABBOX_DESKTOP_ENV:-wayland}"`,
		`CRABBOX_DESKTOP_ENV='$desktop_env'`,
		`labwc wayvnc swaybg librsvg2-common gnome-panel wlr-randr grim slurp wtype wl-clipboard`,
		`swaybg librsvg2-common`,
		`gnome-terminal nautilus gsettings-desktop-schemas adwaita-icon-theme`,
		`DISPLAY=:0`,
		`export GDK_BACKEND=x11`,
		`export MOZ_ENABLE_WAYLAND=0`,
		`gnome-panel >/tmp/crabbox-gnome-panel.log 2>&1 &`,
		`gnome-terminal -- bash -l`,
		`nautilus --new-window "$HOME"`,
		`--user-data-dir=`,
		`if [ "$desktop_env" = "gnome" ]; then
    cat >/usr/local/bin/crabbox-configure-desktop-theme`,
		`crabbox-configure-desktop-theme`,
		`desktop-theme`,
		`gsettings set org.gnome.desktop.interface color-scheme '$gsettings_scheme'`,
		`--force-dark-mode --enable-features=WebUIDarkMode --blink-settings=preferredColorScheme=2`,
		`--blink-settings=preferredColorScheme=1`,
		`WLR_BACKENDS=headless`,
		`rm -f /var/lib/crabbox/display.env`,
		`dbus-run-session labwc`,
		`/tmp/crabbox-labwc.log`,
		`wayvnc --config`,
		`--ozone-platform=x11`,
		`--ozone-platform=wayland`,
	} {
		if !strings.Contains(bootstrapScript, want) {
			t.Fatalf("bootstrap script missing %q", want)
		}
	}
	for _, notWant := range []string{
		`waybar`,
		`"wlr/taskbar"`,
	} {
		if strings.Contains(bootstrapScript, notWant) {
			t.Fatalf("bootstrap script contains %q", notWant)
		}
	}

	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Desktop = true
	cfg.DesktopEnv = "wayland"
	cfg.LocalContainer.WorkRoot = "/work/crabbox"
	got := localContainerReadyCheck(cfg)
	for _, want := range []string{
		"pgrep -x labwc",
		"pgrep -x wayvnc",
		"127.0.0.1:5900",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("wayland ready check missing %q: %s", want, got)
		}
	}
	if strings.Contains(got, "Xvfb :99") || strings.Contains(got, "x11vnc") {
		t.Fatalf("wayland ready check contains XFCE checks: %s", got)
	}

	cfg.DesktopEnv = "gnome"
	got = localContainerReadyCheck(cfg)
	for _, want := range []string{
		"pgrep -x labwc",
		"pgrep -x wayvnc",
		"127.0.0.1:5900",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("gnome ready check missing %q: %s", want, got)
		}
	}
	if strings.Contains(got, "pgrep -x gnome-shell") || strings.Contains(got, "Xvfb :99") {
		t.Fatalf("gnome ready check contains wrong compositor checks: %s", got)
	}
}

func TestBootstrapScriptSupportsDockerSocketCLI(t *testing.T) {
	for _, want := range []string{
		`[ "${CRABBOX_DOCKER_SOCKET:-0}" = "1" ] && ! command -v docker`,
		`https://download.docker.com/linux/${ID}/gpg`,
		localContainerDockerSigningKeyFingerprint,
		`install_verified_apt_keyring`,
		`GNUPGHOME="$apt_key_home" gpg --batch --import`,
		`GNUPGHOME="$apt_key_home" gpg --batch --export`,
		`signed-by=/etc/apt/keyrings/docker.gpg`,
		`mv -f "$apt_key_output" "$apt_key_target"`,
		`rm -f /etc/apt/keyrings/docker.asc`,
		`Docker APT signing key verification failed; falling back to distro Docker CLI`,
		`if apt-get update && apt-get install -y --no-install-recommends docker-ce-cli; then`,
		`rm -f /etc/apt/sources.list.d/docker.list`,
		`apt-get install -y --no-install-recommends docker-ce-cli`,
		`apt-get install -y --no-install-recommends docker.io`,
		`Docker-compatible socket requested but docker CLI is not installed`,
		`stat -c '%g' /var/run/docker.sock`,
		`usermod -aG "$socket_group" "$user"`,
	} {
		if !strings.Contains(bootstrapScript, want) {
			t.Fatalf("bootstrap script missing %q", want)
		}
	}
	if strings.Contains(bootstrapScript, `curl -fsSL "https://download.docker.com/linux/${ID}/gpg" -o /etc/apt/keyrings/docker.asc`) {
		t.Fatal("bootstrap script downloads Docker trust directly into the final keyring")
	}
	cmd := exec.Command("sh", "-n")
	cmd.Stdin = strings.NewReader(bootstrapScript)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bootstrap script syntax: %v\n%s", err, output)
	}
}

func TestInstallVerifiedAPTKeyringScript(t *testing.T) {
	for _, tc := range []struct {
		name              string
		actualFingerprint string
		wantSuccess       bool
	}{
		{name: "matching primary", actualFingerprint: localContainerDockerSigningKeyFingerprint, wantSuccess: true},
		{name: "mismatched primary", actualFingerprint: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", wantSuccess: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			if err := os.Mkdir(bin, 0o755); err != nil {
				t.Fatal(err)
			}
			curlScript := "#!/bin/sh\nset -eu\nprintf 'fake-docker-key\\n'\n"
			if err := os.WriteFile(filepath.Join(bin, "curl"), []byte(curlScript), 0o755); err != nil {
				t.Fatal(err)
			}
			gpgScript := `#!/bin/sh
set -eu
mode=""
value=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --batch|--with-colons) shift ;;
    --import|--fingerprint|--export)
      mode="${1#--}"
      value="${2:-}"
      shift 2
      ;;
    *) exit 64 ;;
  esac
done
case "$mode" in
  import) [ -s "$value" ] ;;
  fingerprint) printf 'fpr:::::::::%s:\n' "$FAKE_GPG_FINGERPRINT" ;;
  export) printf 'fake-export:%s\n' "$value" ;;
  *) exit 65 ;;
esac
`
			if err := os.WriteFile(filepath.Join(bin, "gpg"), []byte(gpgScript), 0o755); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(dir, "keyrings", "docker.gpg")
			if err := os.Mkdir(filepath.Dir(target), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(target, []byte("existing-keyring\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			script := "set -eu\n" + installVerifiedAPTKeyringScript + `
install_verified_apt_keyring "$1" "$2" "$3"
`
			cmd := exec.Command("sh", "-c", script, "test", "https://download.docker.com/linux/ubuntu/gpg", target, localContainerDockerSigningKeyFingerprint)
			cmd.Env = append(os.Environ(),
				"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
				"FAKE_GPG_FINGERPRINT="+tc.actualFingerprint,
			)
			output, err := cmd.CombinedOutput()
			if tc.wantSuccess && err != nil {
				t.Fatalf("verified key install failed: %v\n%s", err, output)
			}
			if !tc.wantSuccess && err == nil {
				t.Fatal("mismatched key install succeeded")
			}
			got, readErr := os.ReadFile(target)
			if readErr != nil {
				t.Fatal(readErr)
			}
			want := "existing-keyring\n"
			if tc.wantSuccess {
				want = "fake-export:" + localContainerDockerSigningKeyFingerprint + "\n"
			}
			if string(got) != want {
				t.Fatalf("keyring = %q, want %q", got, want)
			}
			tempFiles, globErr := filepath.Glob(target + ".tmp.*")
			if globErr != nil {
				t.Fatal(globErr)
			}
			if len(tempFiles) != 0 {
				t.Fatalf("temporary key state remains: %v", tempFiles)
			}
		})
	}
}

func TestConfigForRunHonorsGlobalWorkRoot(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.WorkRoot = "/tmp/cbx"
	cfg.LocalContainer.WorkRoot = ""

	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)
	got := b.configForRun()
	if got.WorkRoot != "/tmp/cbx" || got.LocalContainer.WorkRoot != "/tmp/cbx" {
		t.Fatalf("work root = %q local=%q, want /tmp/cbx", got.WorkRoot, got.LocalContainer.WorkRoot)
	}
}

func TestApplyDefaultsDoesNotMaskUnsupportedTarget(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetWindows
	cfg.WindowsMode = "normal"

	applyDefaults(&cfg)
	if cfg.TargetOS != core.TargetWindows || cfg.WindowsMode != "normal" {
		t.Fatalf("target = %s windowsMode=%s, want explicit windows target preserved", cfg.TargetOS, cfg.WindowsMode)
	}
	if _, err := (Provider{}).Configure(cfg, core.Runtime{}); err == nil {
		t.Fatal("Configure accepted unsupported windows target")
	}
}

func TestListAndResolveContainers(t *testing.T) {
	inspectJSON := `[{
		"Id":"abcdef1234567890",
		"Name":"/crabbox-blue",
		"Config":{"Image":"ubuntu:24.04","Labels":{"crabbox":"true","provider":"local-container","lease":"cbx_123","slug":"blue-lobster","state":"ready","server_type":"ubuntu:24.04","ssh_user":"runner","work_root":"/workspace/crabbox"}},
		"State":{"Status":"running","Running":true},
		"NetworkSettings":{"Ports":{"2222/tcp":[{"HostIp":"127.0.0.1","HostPort":"49153"}]}}
	}]`
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"ps", "-a", "--filter", "label=crabbox=true", "--filter", "label=provider=local-container", "--format", "{{.ID}}"}): {Stdout: "abcdef1234567890\n"},
			commandKey([]string{"inspect", "abcdef1234567890"}): {Stdout: inspectJSON},
		},
	}
	addDefaultLocalContainerScopeResponses(runner)
	writeLocalContainerReleaseClaim(t, "cbx_123", "abcdef1234567890")
	b := testBackend(runner)

	views, err := b.List(context.Background(), core.ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 {
		t.Fatalf("views=%d", len(views))
	}
	if views[0].Provider != providerName || views[0].CloudID != "abcdef1234567890" || views[0].Labels["ssh_port"] != "49153" {
		t.Fatalf("unexpected view: %#v", views[0])
	}
	if owned, conflict, statusErr := b.localContainerClaimStatus(context.Background(), "cbx_123", "abcdef1234567890"); statusErr != nil || !owned || conflict {
		t.Fatalf("claim status owned=%v conflict=%v err=%v", owned, conflict, statusErr)
	}

	lease, err := b.Resolve(context.Background(), core.ResolveRequest{ID: "blue-lobster"})
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID != "cbx_123" || lease.SSH.Host != "127.0.0.1" || lease.SSH.Port != "49153" || lease.SSH.User != "runner" || lease.SSH.TargetOS != core.TargetLinux || len(lease.SSH.FallbackPorts) != 0 || !strings.Contains(lease.SSH.ReadyCheck, "rsync --version") {
		t.Fatalf("unexpected lease: %#v", lease)
	}
}

func TestResolvePendingClaimDoesNotRequirePublishedSSHPort(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_pending_no_port"
	labels := map[string]string{
		"crabbox": "true", "provider": providerName, "lease": leaseID, "slug": "pending-no-port",
		"state": pendingClaimState, "recovery": pendingRecoveryKind, "runtime": "docker",
		"server_type": "ubuntu:24.04", "ssh_user": "runner", "work_root": "/workspace/crabbox",
	}
	labels = testCapturedScopeLabels(labels)
	if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(
		leaseID,
		"pending-no-port",
		providerName,
		"runtime:docker/context:default",
		"",
		t.TempDir(),
		time.Minute,
		false,
		core.Server{CloudID: "pending-container", Provider: providerName, Labels: labels},
		core.SSHTarget{},
	); err != nil {
		t.Fatal(err)
	}
	inspectJSON := `[{"Id":"pending-container","Name":"/pending-container","Config":{"Image":"ubuntu:24.04","Labels":{"crabbox":"true","provider":"local-container","lease":"cbx_pending_no_port","slug":"pending-no-port","state":"provisioning","recovery":"ssh-readiness-pending","ssh_user":"runner","work_root":"/workspace/crabbox"}},"State":{"Status":"running","Running":true},"NetworkSettings":{"Ports":{}}}]`
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
		commandKey([]string{"ps", "-a", "--filter", "label=crabbox=true", "--filter", "label=provider=local-container", "--format", "{{.ID}}"}): {Stdout: "pending-container\n"},
		commandKey([]string{"inspect", "pending-container"}): {Stdout: inspectJSON},
	}}
	addDefaultLocalContainerScopeResponses(runner)
	b := testBackend(runner)
	lease, err := b.Resolve(context.Background(), core.ResolveRequest{ID: leaseID, StatusOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Server.Status != pendingClaimState || lease.Server.Labels["recovery"] != pendingRecoveryKind || lease.SSH.Host != "" || lease.SSH.Port != "" {
		t.Fatalf("pending lease=%#v", lease)
	}
}

func TestResolvePreviouslyReadyTerminalContainerWithoutSSHPort(t *testing.T) {
	for _, state := range []string{"exited", "dead", "stopped"} {
		t.Run(state, func(t *testing.T) {
			leaseID := "cbx_previously_ready_" + state
			containerID := "previously-ready-" + state
			writeLocalContainerReleaseClaim(t, leaseID, containerID)
			claim, err := core.ReadLeaseClaim(leaseID)
			if err != nil {
				t.Fatal(err)
			}
			labels := cloneLabels(claim.Labels)
			labels["state"] = "ready"
			claim, err = core.UpdateLeaseClaimLabelsIfUnchanged(leaseID, claim, labels)
			if err != nil {
				t.Fatal(err)
			}
			inspectJSON := fmt.Sprintf(`[{"Id":%q,"Name":"/previously-ready","Config":{"Image":"ubuntu:24.04","Labels":{"crabbox":"true","provider":"local-container","lease":%q,"slug":"previously-ready","state":"ready","ssh_user":"runner","work_root":"/workspace/crabbox"}},"State":{"Status":%q,"Running":false},"NetworkSettings":{"Ports":{}}}]`, containerID, leaseID, state)
			runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
				commandKey([]string{"ps", "-a", "--filter", "label=crabbox=true", "--filter", "label=provider=local-container", "--format", "{{.ID}}"}): {Stdout: containerID + "\n"},
				commandKey([]string{"inspect", containerID}): {Stdout: inspectJSON},
			}}
			addDefaultLocalContainerScopeResponses(runner)
			b := testBackend(runner)
			lease, err := b.Resolve(context.Background(), core.ResolveRequest{ID: leaseID, StatusOnly: true})
			if err != nil || lease.Server.Status != state || lease.Server.Labels["state"] != state || lease.SSH.Port != "" {
				t.Fatalf("previously ready terminal status lease=%#v err=%v", lease, err)
			}
			if current, err := core.ReadLeaseClaim(leaseID); err != nil || current.Revision != claim.Revision || current.Labels["state"] != "ready" {
				t.Fatalf("terminal status changed previously ready claim=%#v err=%v", current, err)
			}
		})
	}
}

func TestAuthorizeStatusTouchClaimHydratesDynamicRuntimeScope(t *testing.T) {
	leaseID, containerID, claim, runner := createLocalContainerTouchClaim(t, 37*time.Minute)
	b := testBackend(runner)
	if len(b.cfg.LocalContainer.CheckpointMetadata) != 0 {
		t.Fatal("test backend unexpectedly started with hydrated runtime metadata")
	}
	lease := core.LeaseTarget{LeaseID: leaseID, Server: core.Server{Provider: providerName, CloudID: containerID}}
	if err := b.AuthorizeStatusTouchClaim(context.Background(), lease, claim); err != nil {
		t.Fatal(err)
	}
	if got := b.claimScope(context.Background()); got != claim.ProviderScope {
		t.Fatalf("hydrated scope=%q want=%q", got, claim.ProviderScope)
	}
	if got := b.cfg.LocalContainer.CheckpointMetadata[checkpointMetadataEndpoint]; got != claim.Labels[checkpointMetadataEndpoint] {
		t.Fatalf("hydrated endpoint=%q want=%q", got, claim.Labels[checkpointMetadataEndpoint])
	}
}

func TestLocalContainerTouchPersistsExactClaimLifecycle(t *testing.T) {
	for _, test := range []struct {
		name        string
		override    *time.Duration
		wantTimeout time.Duration
		ttlLabel    string
	}{
		{name: "omitted override preserves stored timeout", wantTimeout: 37 * time.Minute},
		{name: "explicit override persists", override: durationPointer(45 * time.Minute), wantTimeout: 45 * time.Minute},
		{name: "duration-form TTL stays exact", wantTimeout: 37 * time.Minute, ttlLabel: "24h"},
	} {
		t.Run(test.name, func(t *testing.T) {
			leaseID, _, initial, runner := createLocalContainerTouchClaim(t, 37*time.Minute)
			if test.ttlLabel != "" {
				labels := cloneLabels(initial.Labels)
				labels["ttl_secs"] = test.ttlLabel
				updated, err := core.UpdateLeaseClaimLabelsIfUnchanged(leaseID, initial, labels)
				if err != nil {
					t.Fatal(err)
				}
				initial = updated
			}
			touchedAt := time.Date(2026, time.August, 17, 20, 30, 0, 0, time.UTC)
			b := testBackend(runner)
			b.cfg.IdleTimeout = 5 * time.Minute
			b.rt.Clock = localContainerTestClock{now: touchedAt}
			resolved, err := b.Resolve(context.Background(), core.ResolveRequest{ID: leaseID, StatusOnly: true, NoLocalStateMutations: true})
			if err != nil {
				t.Fatal(err)
			}

			touched, err := b.Touch(context.Background(), core.TouchRequest{
				Lease:               resolved,
				State:               "busy",
				IdleTimeout:         5 * time.Minute,
				IdleTimeoutOverride: test.override,
			})
			if err != nil {
				t.Fatal(err)
			}
			committed, exists, err := core.ReadLeaseClaimWithPresence(leaseID)
			if err != nil || !exists {
				t.Fatalf("read committed claim exists=%t err=%v", exists, err)
			}
			if committed.IdleTimeoutSeconds != int(test.wantTimeout/time.Second) {
				t.Fatalf("committed timeout=%d want=%d", committed.IdleTimeoutSeconds, int(test.wantTimeout/time.Second))
			}
			if committed.LastUsedAt != touchedAt.Format(time.RFC3339) || committed.Labels["last_touched_at"] != core.LeaseLabelTime(touchedAt) {
				t.Fatalf("committed touch lastUsed=%q labels=%#v", committed.LastUsedAt, committed.Labels)
			}
			if committed.Labels["idle_timeout_secs"] != strconv.Itoa(int(test.wantTimeout/time.Second)) {
				t.Fatalf("committed timeout label=%q", committed.Labels["idle_timeout_secs"])
			}
			if got := unixLocalContainerLabelTime(t, committed.Labels["expires_at"]); !got.Equal(touchedAt.Add(test.wantTimeout)) {
				t.Fatalf("expires=%s want=%s", got, touchedAt.Add(test.wantTimeout))
			}
			for _, key := range []string{"created_at", "ttl_secs", "bootstrap_dir", "host_work_root", "work_root", checkpointMetadataConfig, checkpointMetadataEndpoint, checkpointMetadataDaemonID} {
				if committed.Labels[key] != initial.Labels[key] {
					t.Fatalf("claim label %s=%q want exact %q", key, committed.Labels[key], initial.Labels[key])
				}
			}
			if touched.Status != "busy" || touched.Labels["state"] != "busy" {
				t.Fatalf("touch response status=%q labels=%#v", touched.Status, touched.Labels)
			}
			snapshot, snapshotExists, snapshotSet := core.ServerLeaseClaimSnapshot(touched)
			if !snapshotSet || !snapshotExists || !reflect.DeepEqual(snapshot, committed) {
				t.Fatalf("touch snapshot set=%t exists=%t snapshot=%#v committed=%#v", snapshotSet, snapshotExists, snapshot, committed)
			}

			freshRunner := localContainerTouchRunner(t, leaseID, committed.CloudID)
			fresh := testBackend(freshRunner)
			fresh.cfg.IdleTimeout = 3 * time.Minute
			freshResolved, err := fresh.Resolve(context.Background(), core.ResolveRequest{ID: leaseID, StatusOnly: true, NoLocalStateMutations: true})
			if err != nil {
				t.Fatal(err)
			}
			if freshResolved.Server.Labels["last_touched_at"] != committed.Labels["last_touched_at"] || freshResolved.Server.Labels["expires_at"] != committed.Labels["expires_at"] || freshResolved.Server.Labels["idle_timeout_secs"] != committed.Labels["idle_timeout_secs"] {
				t.Fatalf("fresh resolve lost committed lifecycle: fresh=%#v committed=%#v", freshResolved.Server.Labels, committed.Labels)
			}
			freshSnapshot, freshExists, freshSet := core.ServerLeaseClaimSnapshot(freshResolved.Server)
			if !freshSet || !freshExists || !reflect.DeepEqual(freshSnapshot, committed) {
				t.Fatalf("fresh resolve snapshot set=%t exists=%t snapshot=%#v", freshSet, freshExists, freshSnapshot)
			}
		})
	}
}

func TestLocalContainerTouchRejectsUnownedOrChangedClaimWithoutMutation(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, *backend, *core.LeaseTarget, core.LeaseClaim)
	}{
		{name: "missing carried snapshot", mutate: func(_ *testing.T, _ *backend, lease *core.LeaseTarget, _ core.LeaseClaim) {
			lease.Server = core.Server{Provider: lease.Server.Provider, CloudID: lease.Server.CloudID, Labels: cloneLabels(lease.Server.Labels)}
		}},
		{name: "wrong provider", mutate: func(t *testing.T, _ *backend, lease *core.LeaseTarget, claim core.LeaseClaim) {
			claim.Provider = "aws"
			core.SetServerLeaseClaimSnapshot(&lease.Server, claim, true)
		}},
		{name: "wrong dynamic runtime scope", mutate: func(t *testing.T, _ *backend, lease *core.LeaseTarget, claim core.LeaseClaim) {
			claim.ProviderScope = "runtime:docker/context:other"
			core.SetServerLeaseClaimSnapshot(&lease.Server, claim, true)
		}},
		{name: "wrong container", mutate: func(_ *testing.T, _ *backend, lease *core.LeaseTarget, _ core.LeaseClaim) {
			lease.Server.CloudID = strings.Repeat("b", 64)
		}},
		{name: "runtime metadata mismatch", mutate: func(t *testing.T, _ *backend, lease *core.LeaseTarget, claim core.LeaseClaim) {
			claim.Labels = cloneLabels(claim.Labels)
			claim.Labels[checkpointMetadataContext] = "other"
			core.SetServerLeaseClaimSnapshot(&lease.Server, claim, true)
		}},
		{name: "stale carried snapshot after CAS replacement", mutate: func(t *testing.T, _ *backend, lease *core.LeaseTarget, claim core.LeaseClaim) {
			labels := cloneLabels(claim.Labels)
			labels["concurrent_replacement"] = "true"
			if _, err := core.UpdateLeaseClaimLabelsIfUnchanged(lease.LeaseID, claim, labels); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "raced-away claim", mutate: func(_ *testing.T, _ *backend, lease *core.LeaseTarget, _ core.LeaseClaim) {
			core.RemoveLeaseClaim(lease.LeaseID)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			leaseID, _, initial, runner := createLocalContainerTouchClaim(t, 37*time.Minute)
			b := testBackend(runner)
			resolved, err := b.Resolve(context.Background(), core.ResolveRequest{ID: leaseID, StatusOnly: true, NoLocalStateMutations: true})
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(t, b, &resolved, initial)
			before, beforeExists, err := core.ReadLeaseClaimWithPresence(leaseID)
			if err != nil {
				t.Fatal(err)
			}
			callsBefore := len(runner.calls)
			if _, err := b.Touch(context.Background(), core.TouchRequest{Lease: resolved, State: "busy"}); err == nil {
				t.Fatal("touch unexpectedly succeeded")
			}
			after, afterExists, err := core.ReadLeaseClaimWithPresence(leaseID)
			if err != nil || beforeExists != afterExists || !reflect.DeepEqual(before, after) {
				t.Fatalf("rejected touch mutated claim: beforeExists=%t afterExists=%t err=%v before=%#v after=%#v", beforeExists, afterExists, err, before, after)
			}
			for _, call := range runner.calls[callsBefore:] {
				if len(call.Args) > 0 && (call.Args[0] == "rm" || call.Args[0] == "run" || call.Args[0] == "stop") {
					t.Fatalf("rejected touch reached container mutation: %#v", call.Args)
				}
			}
		})
	}
}

func TestHostLeaseWorkRootRequiresTrustedLabels(t *testing.T) {
	hostRoot := t.TempDir()
	leaseID := "cbx_trusted"
	if got := hostLeaseWorkRootFromLabels(leaseID, map[string]string{"docker_socket": "1", "work_root": hostRoot}); got != "" {
		t.Fatalf("unmarked work root accepted: %q", got)
	}
	if got := hostLeaseWorkRootFromLabels("Users", map[string]string{"docker_socket": "1", "work_root": "/"}); got != "" {
		t.Fatalf("spoofed lease root accepted: %q", got)
	}
	markTestLocalContainerWorkRoot(t, hostRoot)
	if got := hostLeaseWorkRootFromLabels(leaseID, map[string]string{"docker_socket": "1", "host_work_root": hostRoot, "work_root": "/work/crabbox"}); got != filepath.Join(hostRoot, leaseID) {
		t.Fatalf("work root=%q want %q", got, filepath.Join(hostRoot, leaseID))
	}
}

func TestTrustedBootstrapDir(t *testing.T) {
	trusted := []string{
		filepath.Join(localContainerBootstrapRoot(), "crabbox-bootstrap-abc123"),
		filepath.Join(os.TempDir(), "crabbox-bootstrap-legacy"),
	}
	for _, good := range trusted {
		if !trustedBootstrapDir(good) {
			t.Fatalf("should trust %q", good)
		}
	}
	for _, bad := range []string{
		"",
		"crabbox-bootstrap-abc123",
		filepath.Join(localContainerBootstrapRoot(), "not-crabbox-dir"),
		filepath.Join(localContainerBootstrapRoot(), "crabbox-bootstrap-abc123", ".."),
		filepath.Join("/some/other/path", "crabbox-bootstrap-abc123"),
		"/etc/passwd",
	} {
		if trustedBootstrapDir(bad) {
			t.Fatalf("should reject %q", bad)
		}
	}
}

func TestLocalContainerBootstrapRootUsesUserCache(t *testing.T) {
	cacheDir, err := os.UserCacheDir()
	if err != nil || strings.TrimSpace(cacheDir) == "" {
		t.Skip("user cache directory unavailable")
	}
	want := filepath.Join(cacheDir, "crabbox", "local-container-bootstrap")
	if got := localContainerBootstrapRoot(); got != want {
		t.Fatalf("bootstrap root=%q want %q", got, want)
	}
}

func TestFindContainerForClaimRejectsSlugOnlyMatch(t *testing.T) {
	inspectJSON := `[{
		"Id":"newcontainer123456",
		"Name":"/crabbox-blue-new",
		"Config":{"Image":"ubuntu:24.04","Labels":{"crabbox":"true","provider":"local-container","lease":"cbx_new","slug":"blue-lobster","state":"ready","server_type":"ubuntu:24.04","ssh_user":"runner","work_root":"/workspace/crabbox"}},
		"State":{"Status":"running","Running":true},
		"NetworkSettings":{"Ports":{"2222/tcp":[{"HostIp":"127.0.0.1","HostPort":"49154"}]}}
	}]`
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"ps", "-a", "--filter", "label=crabbox=true", "--filter", "label=provider=local-container", "--format", "{{.ID}}"}): {Stdout: "newcontainer123456\n"},
			commandKey([]string{"inspect", "newcontainer123456"}): {Stdout: inspectJSON},
		},
	}
	b := testBackend(runner)

	if _, _, _, err := b.findContainerForClaim(context.Background(), core.LeaseClaim{LeaseID: "cbx_old", Slug: "blue-lobster"}); err == nil {
		t.Fatal("expected stale claim lease id to reject a same-slug container")
	}
}

func TestReleaseLeaseRemovesStoredKey(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	keyPath, err := core.TestboxKeyPath("cbx_release")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"rm", "-f", "container123"}): {},
		},
	}
	addDefaultLocalContainerScopeResponses(runner)
	writeLocalContainerReleaseClaim(t, "cbx_release", "container123")
	b := testBackend(runner)
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{LeaseID: "cbx_release", Server: core.Server{CloudID: "container123"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatalf("stored key still exists after release: %v", err)
	}
}

func TestReleaseLeaseContinuesSidecarCleanupAndRetainsKeyAfterError(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_release_sidecars"
	keyPath, err := core.TestboxKeyPath(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	hostRoot := t.TempDir()
	markTestLocalContainerWorkRoot(t, hostRoot)
	hostLeaseRoot := filepath.Join(hostRoot, leaseID)
	if err := os.MkdirAll(hostLeaseRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	bootstrapDir, err := os.MkdirTemp("", "crabbox-bootstrap-release-sidecar-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(bootstrapDir) })
	labels := map[string]string{
		"provider": providerName, "lease": leaseID, "slug": "release-sidecars", "state": "ready",
		"docker_socket": "1", "host_work_root": hostRoot, "work_root": "/work/crabbox",
		"bootstrap_dir": bootstrapDir,
	}
	labels = testCapturedScopeLabels(labels)
	if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(
		leaseID,
		"release-sidecars",
		providerName,
		"runtime:docker/context:default",
		"",
		t.TempDir(),
		time.Minute,
		false,
		core.Server{CloudID: "release-sidecar-container", Provider: providerName, Labels: labels},
		core.SSHTarget{},
	); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
		commandKey([]string{"rm", "-f", "release-sidecar-container"}): {},
	}}
	addDefaultLocalContainerScopeResponses(runner)
	b := testBackend(runner)
	b.removeAll = func(path string) error {
		if path == hostLeaseRoot {
			return errors.New("host cleanup failed")
		}
		return os.RemoveAll(path)
	}
	lease := core.LeaseTarget{LeaseID: leaseID, Server: core.Server{CloudID: "release-sidecar-container", Labels: labels}}
	outcome, err := b.ReleaseLeaseWithOutcome(context.Background(), core.ReleaseLeaseRequest{Lease: lease})
	if !outcome.Terminal {
		t.Errorf("sidecar error hid confirmed container removal: %+v", outcome)
	}
	if err == nil || !strings.Contains(err.Error(), "host cleanup failed") {
		t.Fatalf("release error=%v", err)
	}
	if _, err := os.Stat(bootstrapDir); !os.IsNotExist(err) {
		t.Fatalf("bootstrap cleanup did not continue: %v", err)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("key should remain after incomplete sidecar cleanup: %v", err)
	}
	if claim, err := core.ReadLeaseClaim(leaseID); err != nil || claim.LeaseID != leaseID {
		t.Fatalf("claim should remain for sidecar retry: %#v err=%v", claim, err)
	}
	core.RemoveLeaseClaim(leaseID)
}

func TestReleaseLeaseRejectsUnclaimedContainer(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
		commandKey([]string{"rm", "-f", "unclaimed-container"}): {},
	}}
	b := testBackend(runner)
	err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{
		LeaseID: "cbx_unclaimed_local_container",
		Server:  core.Server{CloudID: "unclaimed-container"},
	}})
	if err == nil || !strings.Contains(err.Error(), "no exact local claim") {
		t.Fatalf("ReleaseLease error=%v, want exact-claim rejection", err)
	}
	for _, call := range runner.calls {
		if len(call.Args) > 0 && call.Args[0] == "rm" {
			t.Fatalf("unclaimed container reached rm: %#v", runner.commandSummary())
		}
	}
}

func TestReleaseLeaseRejectsClaimBoundToDifferentContainerOrScope(t *testing.T) {
	for _, tc := range []struct {
		name       string
		claimID    string
		claimScope string
		targetID   string
	}{
		{name: "container", claimID: "owned-container", claimScope: "runtime:docker/context:default", targetID: "other-container"},
		{name: "runtime scope", claimID: "owned-container", claimScope: "runtime:docker/context:remote", targetID: "owned-container"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
			addDefaultLocalContainerScopeResponses(runner)
			writeLocalContainerReleaseClaimWithScope(t, "cbx_bound_local_container", tc.claimID, tc.claimScope)
			b := testBackend(runner)
			err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{
				LeaseID: "cbx_bound_local_container",
				Server:  core.Server{CloudID: tc.targetID},
			}})
			if err == nil || !strings.Contains(err.Error(), "current runtime scope") {
				t.Fatalf("ReleaseLease error=%v, want resource-binding rejection", err)
			}
			for _, call := range runner.calls {
				if len(call.Args) > 0 && call.Args[0] == "rm" {
					t.Fatalf("mismatched claim reached rm: %#v", runner.commandSummary())
				}
			}
		})
	}
}

func TestExactClaimValidatesCapturedEndpointAndDaemon(t *testing.T) {
	base := checkpointScope{
		Runtime:  "docker",
		Context:  "named-context",
		Config:   "/tmp/docker-config",
		Endpoint: "unix:///captured.sock",
		DaemonID: "daemon-captured",
	}
	for _, tc := range []struct {
		name          string
		current       checkpointScope
		validationErr error
		wantErr       bool
	}{
		{name: "matching", current: base},
		{name: "endpoint mismatch", current: func() checkpointScope { value := base; value.Endpoint = "unix:///other.sock"; return value }(), wantErr: true},
		{name: "daemon metadata mismatch", current: func() checkpointScope { value := base; value.DaemonID = "daemon-other"; return value }(), wantErr: true},
		{name: "live daemon mismatch", current: base, validationErr: errors.New("daemon changed"), wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := testBackend(&recordingRunner{})
			b.cfg.LocalContainer.CheckpointMetadata = checkpointScopeMetadata(tc.current)
			b.validateRuntimeScope = func(context.Context, checkpointScope) error { return tc.validationErr }
			claim := core.LeaseClaim{
				LeaseID:       "cbx_exact_scope",
				Provider:      providerName,
				CloudID:       "container-scope",
				ProviderScope: "runtime:docker/context:named-context",
				Labels:        checkpointScopeMetadata(base),
			}
			err := b.validateExactLocalContainerClaim(context.Background(), claim, claim.LeaseID, claim.CloudID)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error=%v wantErr=%v", err, tc.wantErr)
			}
			if got := b.claimMatchesCurrentScope(context.Background(), claim); got == tc.wantErr {
				t.Fatalf("claimMatchesCurrentScope=%v want=%v", got, !tc.wantErr)
			}
		})
	}
}

func TestReleaseRejectsCapturedDaemonMismatchBeforeRemove(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_daemon_mismatch"
	scope := checkpointScope{Runtime: "docker", Context: "named", Config: "/tmp/docker-config", Endpoint: "unix:///captured.sock", DaemonID: "daemon-captured"}
	labels := checkpointScopeMetadata(scope)
	labels["provider"] = providerName
	labels["lease"] = leaseID
	labels["slug"] = "daemon-mismatch"
	labels["state"] = "ready"
	if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(
		leaseID, "daemon-mismatch", providerName, "runtime:docker/context:named", "", t.TempDir(), time.Minute, false,
		core.Server{CloudID: "daemon-container", Provider: providerName, Labels: labels}, core.SSHTarget{},
	); err != nil {
		t.Fatal(err)
	}
	claim, err := core.ReadLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	server := core.Server{CloudID: claim.CloudID, Labels: claim.Labels}
	core.SetServerLeaseClaimSnapshot(&server, claim, true)
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
		commandKey([]string{"rm", "-f", "daemon-container"}): {},
	}}
	b := testBackend(runner)
	b.validateRuntimeScope = func(context.Context, checkpointScope) error { return errors.New("daemon changed") }
	err = b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{LeaseID: leaseID, Server: server}})
	if err == nil {
		t.Fatal("release accepted changed daemon")
	}
	for _, call := range runner.calls {
		if firstArg(call.Args) == "rm" {
			t.Fatalf("daemon mismatch reached rm: %v", call.Args)
		}
	}
	core.RemoveLeaseClaim(leaseID)
}

func TestReleaseLeaseRejectsClaimChangedAfterResolution(t *testing.T) {
	leaseID := "cbx_release_snapshot"
	writeLocalContainerReleaseClaim(t, leaseID, "snapshot-container")
	claim, err := core.ReadLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	server := core.Server{CloudID: "snapshot-container", Labels: map[string]string{"lease": leaseID}}
	core.SetServerLeaseClaimSnapshot(&server, claim, true)
	labels := cloneLabels(claim.Labels)
	labels["state"] = "newer"
	if _, err := core.UpdateLeaseClaimLabelsIfUnchanged(leaseID, claim, labels); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
		commandKey([]string{"rm", "-f", "snapshot-container"}): {},
	}}
	addDefaultLocalContainerScopeResponses(runner)
	b := testBackend(runner)
	err = b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{LeaseID: leaseID, Server: server}})
	if err == nil || !strings.Contains(err.Error(), "claim changed") {
		t.Fatalf("release error=%v", err)
	}
	for _, call := range runner.calls {
		if firstArg(call.Args) == "rm" {
			t.Fatalf("stale release removed winning resource: %v", call.Args)
		}
	}
	current, err := core.ReadLeaseClaim(leaseID)
	if err != nil || current.Labels["state"] != "newer" {
		t.Fatalf("winning claim=%#v err=%v", current, err)
	}
	core.RemoveLeaseClaim(leaseID)
}

func TestResolveRawContainerRequiresExplicitReclaimAndPersistsBinding(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	inspectJSON := `[{
		"Id":"raw-container",
		"Name":"/crabbox-raw",
		"Config":{"Image":"ubuntu:24.04","Labels":{"crabbox":"true","provider":"local-container","lease":"cbx_raw_local","slug":"raw-local","state":"ready","ssh_user":"runner","work_root":"/workspace/crabbox"}},
		"State":{"Status":"running","Running":true},
		"NetworkSettings":{"Ports":{"2222/tcp":[{"HostIp":"127.0.0.1","HostPort":"49155"}]}}
	}]`
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
		commandKey([]string{"ps", "-a", "--filter", "label=crabbox=true", "--filter", "label=provider=local-container", "--format", "{{.ID}}"}): {Stdout: "raw-container\n"},
		commandKey([]string{"inspect", "raw-container"}):  {Stdout: inspectJSON},
		commandKey([]string{"rm", "-f", "raw-container"}): {},
	}}
	addDefaultLocalContainerScopeResponses(runner)
	b := testBackend(runner)
	repo := core.Repo{Root: t.TempDir()}
	if _, err := b.Resolve(context.Background(), core.ResolveRequest{ID: "raw-container", Repo: repo}); err == nil || !strings.Contains(err.Error(), "explicit --reclaim") {
		t.Fatalf("Resolve without reclaim error=%v", err)
	}
	if claim, err := core.ReadLeaseClaim("cbx_raw_local"); err != nil {
		t.Fatal(err)
	} else if claim.LeaseID != "" {
		t.Fatalf("non-reclaim resolve minted claim: %#v", claim)
	}
	lease, err := b.Resolve(context.Background(), core.ResolveRequest{ID: "raw-container", Repo: repo, Reclaim: true})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := core.ReadLeaseClaim("cbx_raw_local")
	if err != nil {
		t.Fatal(err)
	}
	if claim.CloudID != "raw-container" || claim.ProviderScope != "runtime:docker/context:default" {
		t.Fatalf("reclaim binding=%#v", claim)
	}
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(runner.calls, func(call core.LocalCommandRequest) bool {
		return commandKey(call.Args) == commandKey([]string{"rm", "-f", "raw-container"})
	}) {
		t.Fatalf("reclaimed container was not released: %#v", runner.commandSummary())
	}
}

func TestLegacyLocalContainerClaimRequiresReclaimBeforeStop(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_legacy_local"
	if err := writeLocalContainerClaim(t, leaseID, "legacy-local", "runtime:docker/context:default", time.Now().UTC(), time.Minute); err != nil {
		t.Fatal(err)
	}
	legacy, err := core.ReadLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	replacement := legacy
	replacement.CloudID = "legacy-container"
	replacement.Labels = nil
	if err := core.ReplaceLeaseClaimIfUnchanged(leaseID, legacy, replacement); err != nil {
		t.Fatal(err)
	}
	inspectJSON := `[{"Id":"legacy-container","Name":"/legacy-container","Config":{"Image":"ubuntu:24.04","Labels":{"crabbox":"true","provider":"local-container","lease":"cbx_legacy_local","slug":"legacy-local","state":"ready","ssh_user":"runner","work_root":"/workspace/crabbox","runtime":"docker","docker_context":"stale-context","docker_endpoint":"unix:///stale.sock","docker_daemon_id":"stale-daemon"}},"State":{"Status":"running","Running":true},"NetworkSettings":{"Ports":{"2222/tcp":[{"HostIp":"127.0.0.1","HostPort":"49160"}]}}}]`
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
		commandKey([]string{"ps", "-a", "--filter", "label=crabbox=true", "--filter", "label=provider=local-container", "--format", "{{.ID}}"}): {Stdout: "legacy-container\n"},
		commandKey([]string{"inspect", "legacy-container"}):  {Stdout: inspectJSON},
		commandKey([]string{"rm", "-f", "legacy-container"}): {},
	}}
	addDefaultLocalContainerScopeResponses(runner)
	b := testBackend(runner)
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{LeaseID: leaseID, Server: core.Server{CloudID: "legacy-container"}}}); err == nil || !strings.Contains(err.Error(), "explicit --reclaim") {
		t.Fatalf("legacy direct stop error=%v", err)
	}
	lease, err := b.Resolve(context.Background(), core.ResolveRequest{ID: leaseID, Repo: core.Repo{Root: t.TempDir()}, Reclaim: true})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := core.ReadLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.CloudID != "legacy-container" || claim.ProviderScope != "runtime:docker/context:default" || !hasCompleteCapturedRuntimeScope(claim.Labels) {
		t.Fatalf("legacy reclaim binding=%#v", claim)
	}
	if claim.Labels[checkpointMetadataEndpoint] != "unix:///tmp/docker-test.sock" || claim.Labels[checkpointMetadataDaemonID] != "daemon-test" {
		t.Fatalf("legacy reclaim trusted stale container scope: %#v", claim.Labels)
	}
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
}

func TestResolveReclaimDoesNotRetargetBoundLocalContainerClaim(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	addDefaultLocalContainerScopeResponses(runner)
	writeLocalContainerReleaseClaim(t, "cbx_bound_local", "container-a")
	inspectA := `[{"Id":"container-a","Name":"/container-a","Config":{"Image":"ubuntu:24.04","Labels":{"crabbox":"true","provider":"local-container","lease":"cbx_stale_label","slug":"stale-label","state":"ready","ssh_user":"runner","work_root":"/workspace/crabbox"}},"State":{"Status":"running","Running":true},"NetworkSettings":{"Ports":{"2222/tcp":[{"HostIp":"127.0.0.1","HostPort":"49156"}]}}}]`
	inspectB := `[{"Id":"container-b","Name":"/container-b","Config":{"Image":"ubuntu:24.04","Labels":{"crabbox":"true","provider":"local-container","lease":"cbx_bound_local","slug":"bound-local","state":"ready","ssh_user":"runner","work_root":"/workspace/crabbox"}},"State":{"Status":"running","Running":true},"NetworkSettings":{"Ports":{"2222/tcp":[{"HostIp":"127.0.0.1","HostPort":"49157"}]}}}]`
	runner.responses[commandKey([]string{"ps", "-a", "--filter", "label=crabbox=true", "--filter", "label=provider=local-container", "--format", "{{.ID}}"})] = core.LocalCommandResult{Stdout: "container-b\ncontainer-a\n"}
	runner.responses[commandKey([]string{"inspect", "container-a"})] = core.LocalCommandResult{Stdout: inspectA}
	runner.responses[commandKey([]string{"inspect", "container-b"})] = core.LocalCommandResult{Stdout: inspectB}
	b := testBackend(runner)
	repo := core.Repo{Root: t.TempDir()}
	lease, err := b.Resolve(context.Background(), core.ResolveRequest{ID: "cbx_bound_local", Repo: repo, Reclaim: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Server.CloudID != "container-a" || lease.LeaseID != "cbx_bound_local" {
		t.Fatalf("exact claim resolved container=%q lease=%q, want container-a/cbx_bound_local", lease.Server.CloudID, lease.LeaseID)
	}
	if _, err := b.Resolve(context.Background(), core.ResolveRequest{ID: "container-b", Repo: repo, Reclaim: true}); err == nil || !strings.Contains(err.Error(), "bound to container") {
		t.Fatalf("raw conflicting reclaim error=%v", err)
	}
	claim, err := core.ReadLeaseClaim("cbx_bound_local")
	if err != nil {
		t.Fatal(err)
	}
	if claim.CloudID != "container-a" || claim.ProviderScope != "runtime:docker/context:default" {
		t.Fatalf("bound claim was retargeted: %#v", claim)
	}
}

func TestResolveStatusOnlyAllowsClaimlessLocalContainerWithoutClaiming(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	inspectJSON := `[{"Id":"status-container","Name":"/status-container","Config":{"Image":"ubuntu:24.04","Labels":{"crabbox":"true","provider":"local-container","lease":"cbx_status_local","slug":"status-local","state":"ready","ssh_user":"runner","work_root":"/workspace/crabbox"}},"State":{"Status":"running","Running":true},"NetworkSettings":{"Ports":{"2222/tcp":[{"HostIp":"127.0.0.1","HostPort":"49158"}]}}}]`
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
		commandKey([]string{"ps", "-a", "--filter", "label=crabbox=true", "--filter", "label=provider=local-container", "--format", "{{.ID}}"}): {Stdout: "status-container\n"},
		commandKey([]string{"inspect", "status-container"}): {Stdout: inspectJSON},
	}}
	addDefaultLocalContainerScopeResponses(runner)
	b := testBackend(runner)
	lease, err := b.Resolve(context.Background(), core.ResolveRequest{ID: "status-container", Repo: core.Repo{Root: t.TempDir()}, StatusOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Server.CloudID != "status-container" {
		t.Fatalf("status lease=%#v", lease)
	}
	if claim, err := core.ReadLeaseClaim("cbx_status_local"); err != nil {
		t.Fatal(err)
	} else if claim.LeaseID != "" {
		t.Fatalf("status-only resolve minted claim: %#v", claim)
	}
	writeLocalContainerReleaseClaim(t, "cbx_status_local", "status-container")
	before, err := core.ReadLeaseClaim("cbx_status_local")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Resolve(context.Background(), core.ResolveRequest{ID: "status-container", Repo: core.Repo{Root: t.TempDir()}, StatusOnly: true}); err != nil {
		t.Fatalf("owned status-only resolve: %v", err)
	}
	after, err := core.ReadLeaseClaim("cbx_status_local")
	if err != nil {
		t.Fatal(err)
	}
	if after.RepoRoot != before.RepoRoot || after.LastUsedAt != before.LastUsedAt || after.CloudID != before.CloudID || after.ProviderScope != before.ProviderScope {
		t.Fatalf("status-only resolve mutated claim: before=%#v after=%#v", before, after)
	}
}

func TestResolveReclaimRejectsMetadataLessLocalContainer(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	inspectJSON := `[{"Id":"metadata-less","Name":"/metadata-less","Config":{"Image":"ubuntu:24.04","Labels":{"crabbox":"true","provider":"local-container","state":"ready","ssh_user":"runner","work_root":"/workspace/crabbox"}},"State":{"Status":"running","Running":true},"NetworkSettings":{"Ports":{"2222/tcp":[{"HostIp":"127.0.0.1","HostPort":"49159"}]}}}]`
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
		commandKey([]string{"ps", "-a", "--filter", "label=crabbox=true", "--filter", "label=provider=local-container", "--format", "{{.ID}}"}): {Stdout: "metadata-less\n"},
		commandKey([]string{"inspect", "metadata-less"}): {Stdout: inspectJSON},
	}}
	addDefaultLocalContainerScopeResponses(runner)
	b := testBackend(runner)
	if _, err := b.Resolve(context.Background(), core.ResolveRequest{ID: "metadata-less", Repo: core.Repo{Root: t.TempDir()}, Reclaim: true}); err == nil || !strings.Contains(err.Error(), "no exact local claim") {
		t.Fatalf("metadata-less reclaim error=%v", err)
	}
}

func TestReleaseOnlyResolveMissingContainerClaim(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	keyPath := writeLocalContainerClaimAndKey(t, "cbx_missing_release", "missing-release", localContainerClaimScope("docker", "default"))
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
		commandKey([]string{"ps", "-a", "--filter", "label=crabbox=true", "--filter", "label=provider=local-container", "--format", "{{.ID}}"}): {},
		commandKey([]string{"context", "show"}): {Stdout: "default\n"},
	}}
	b := testBackend(runner)

	lease, err := b.Resolve(context.Background(), core.ResolveRequest{ID: "missing-release", ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID != "cbx_missing_release" || lease.Server.Status != "missing" || lease.Server.Labels["missing_container"] != "1" {
		t.Fatalf("unexpected release-only lease: %#v", lease)
	}
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if claim, err := core.ReadLeaseClaim("cbx_missing_release"); err != nil {
		t.Fatal(err)
	} else if claim.LeaseID != "" {
		t.Fatalf("claim still exists after release: %#v", claim)
	}
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatalf("stored key still exists after release: %v", err)
	}
}

func TestMissingReleaseRejectsClaimChangedAfterResolution(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_missing_snapshot"
	keyPath := writeLocalContainerClaimAndKey(t, leaseID, "missing-snapshot", localContainerClaimScope("docker", "default"))
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
		commandKey([]string{"ps", "-a", "--filter", "label=crabbox=true", "--filter", "label=provider=local-container", "--format", "{{.ID}}"}): {},
		commandKey([]string{"context", "show"}): {Stdout: "default\n"},
	}}
	b := testBackend(runner)
	lease, err := b.Resolve(context.Background(), core.ResolveRequest{ID: leaseID, ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := core.ReadLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	labels := cloneLabels(claim.Labels)
	labels["state"] = "ready"
	if _, err := core.UpdateLeaseClaimEndpointIfUnchanged(
		leaseID,
		claim,
		core.Server{CloudID: "newer-live-container", Provider: providerName, Labels: labels},
		core.SSHTarget{Host: "127.0.0.1", Port: "49175"},
	); err != nil {
		t.Fatal(err)
	}
	err = b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease})
	if err == nil || !strings.Contains(err.Error(), "claim changed") {
		t.Fatalf("missing release error=%v", err)
	}
	current, err := core.ReadLeaseClaim(leaseID)
	if err != nil || current.CloudID != "newer-live-container" {
		t.Fatalf("winning claim=%#v err=%v", current, err)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("stale missing release removed winning key: %v", err)
	}
	core.RemoveLeaseClaim(leaseID)
	core.RemoveStoredTestboxKey(leaseID)
}

func TestMissingReleaseReconfirmsExactAbsenceInsideFence(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_missing_reappeared"
	labels := testCapturedScopeLabels(map[string]string{"provider": providerName, "lease": leaseID, "slug": "missing-reappeared", "state": pendingClaimState, "keep": "true"})
	if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(
		leaseID, "missing-reappeared", providerName, "runtime:docker/context:default", "", t.TempDir(), time.Minute, false,
		core.Server{CloudID: "reappeared-container", Provider: providerName, Labels: labels}, core.SSHTarget{},
	); err != nil {
		t.Fatal(err)
	}
	keyPath, err := core.TestboxKeyPath(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
		commandKey([]string{"ps", "-a", "--filter", "label=crabbox=true", "--filter", "label=provider=local-container", "--format", "{{.ID}}"}): {},
	}}
	addDefaultLocalContainerScopeResponses(runner)
	b := testBackend(runner)
	lease, err := b.Resolve(context.Background(), core.ResolveRequest{ID: leaseID, ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	b.confirmContainerAbsent = func(context.Context, string) (bool, error) { return false, nil }
	err = b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease})
	if err == nil || !strings.Contains(err.Error(), "still exists") {
		t.Fatalf("release error=%v", err)
	}
	claim, err := core.ReadLeaseClaim(leaseID)
	if err != nil || claim.LeaseID != leaseID {
		t.Fatalf("claim=%#v err=%v", claim, err)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("key removed despite reappeared container: %v", err)
	}
	core.RemoveLeaseClaim(leaseID)
	core.RemoveStoredTestboxKey(leaseID)
}

func TestExactContainerAbsenceRejectsRoutingNotFound(t *testing.T) {
	for _, tc := range []struct {
		name       string
		detail     string
		wantAbsent bool
		wantErr    bool
	}{
		{name: "missing object", detail: "Error: No such object: " + testRecoveredContainerID, wantAbsent: true},
		{name: "missing container", detail: "container " + testRecoveredContainerID + " not found", wantAbsent: true},
		{name: "missing podman container", detail: `Error: no container with name or ID "` + testRecoveredContainerID + `" found: no such container`, wantAbsent: true},
		{name: "missing docker context", detail: `context "captured" not found`, wantErr: true},
		{name: "missing podman connection", detail: `connection "captured" not found`, wantErr: true},
		{name: "container runtime endpoint missing", detail: `container runtime endpoint not found while inspecting ` + testRecoveredContainerID, wantErr: true},
		{name: "container connection missing", detail: `container connection "captured" does not exist for ` + testRecoveredContainerID, wantErr: true},
		{name: "endpoint failure wrapping missing object", detail: `container runtime endpoint not found: no such container: ` + testRecoveredContainerID, wantErr: true},
		{name: "connection failure wrapping missing container", detail: `container connection "captured" does not exist; container ` + testRecoveredContainerID + ` not found`, wantErr: true},
		{name: "unrelated missing container", detail: "container bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb not found", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &recordingRunner{run: func(core.LocalCommandRequest) (core.LocalCommandResult, error) {
				return core.LocalCommandResult{Stderr: tc.detail, ExitCode: 1}, errors.New("inspect failed")
			}}
			b := testBackend(runner)
			absent, err := b.exactContainerAbsent(context.Background(), testRecoveredContainerID)
			if absent != tc.wantAbsent || (err != nil) != tc.wantErr {
				t.Fatalf("absent=%v err=%v", absent, err)
			}
		})
	}
}

func TestResolveMissingContainerClaimStillFailsForNormalUse(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	_ = writeLocalContainerClaimAndKey(t, "cbx_missing_normal", "missing-normal", localContainerClaimScope("docker", "default"))
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
		commandKey([]string{"ps", "-a", "--filter", "label=crabbox=true", "--filter", "label=provider=local-container", "--format", "{{.ID}}"}): {},
	}}
	b := testBackend(runner)

	if _, err := b.Resolve(context.Background(), core.ResolveRequest{ID: "missing-normal"}); err == nil {
		t.Fatal("normal resolve succeeded for missing container claim")
	}
	if claim, err := core.ReadLeaseClaim("cbx_missing_normal"); err != nil {
		t.Fatal(err)
	} else if claim.LeaseID != "cbx_missing_normal" {
		t.Fatalf("normal resolve removed claim: %#v", claim)
	}
}

func TestReleaseOnlyResolveKeepsMissingClaimFromDifferentContext(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	keyPath := writeLocalContainerClaimAndKey(t, "cbx_missing_other", "missing-other", localContainerClaimScope("docker", "colima"))
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
		commandKey([]string{"ps", "-a", "--filter", "label=crabbox=true", "--filter", "label=provider=local-container", "--format", "{{.ID}}"}): {},
		commandKey([]string{"context", "show"}): {Stdout: "default\n"},
	}}
	b := testBackend(runner)

	if _, err := b.Resolve(context.Background(), core.ResolveRequest{ID: "missing-other", ReleaseOnly: true}); err == nil {
		t.Fatal("release-only resolve accepted claim from another Docker context")
	}
	if claim, err := core.ReadLeaseClaim("cbx_missing_other"); err != nil {
		t.Fatal(err)
	} else if claim.LeaseID != "cbx_missing_other" {
		t.Fatalf("foreign context claim was removed: %#v", claim)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("foreign context stored key was removed: %v", err)
	}
}

func TestReleaseLeaseRemovesBootstrapDirAfterContainer(t *testing.T) {
	bootstrapDir, err := os.MkdirTemp("", "crabbox-bootstrap-release-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(bootstrapDir) })
	if err := os.WriteFile(filepath.Join(bootstrapDir, "bootstrap.sh"), []byte("test"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	runner.run = func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		if result, ok := defaultLocalContainerScopeResponse(req); ok {
			return result, nil
		}
		if commandKey(req.Args) == commandKey([]string{"rm", "-f", "container123"}) {
			if _, err := os.Stat(bootstrapDir); err != nil {
				t.Fatalf("bootstrap directory removed before container teardown: %v", err)
			}
		}
		return core.LocalCommandResult{}, nil
	}
	writeLocalContainerReleaseClaim(t, "cbx_release", "container123")
	b := testBackend(runner)
	lease := core.LeaseTarget{
		LeaseID: "cbx_release",
		Server: core.Server{
			CloudID: "container123",
			Labels:  map[string]string{"bootstrap_dir": bootstrapDir},
		},
	}

	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(bootstrapDir); !os.IsNotExist(err) {
		t.Fatalf("bootstrap directory still exists after release: %v", err)
	}
}

func TestReleaseLeaseDoesNotRemoveUntrustedBootstrapDir(t *testing.T) {
	parent := t.TempDir()
	bootstrapDir := filepath.Join(parent, "crabbox-bootstrap-forged")
	if err := os.MkdirAll(bootstrapDir, 0o700); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"rm", "-f", "container123"}): {},
		},
	}
	addDefaultLocalContainerScopeResponses(runner)
	writeLocalContainerReleaseClaim(t, "cbx_release", "container123")
	b := testBackend(runner)
	lease := core.LeaseTarget{
		LeaseID: "cbx_release",
		Server: core.Server{
			CloudID: "container123",
			Labels:  map[string]string{"bootstrap_dir": bootstrapDir},
		},
	}

	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err == nil || !strings.Contains(err.Error(), "outside the current cache/temp roots") {
		t.Fatalf("expected actionable bootstrap cleanup error, got %v", err)
	}
	if _, err := os.Stat(bootstrapDir); err != nil {
		t.Fatalf("untrusted bootstrap directory removed: %v", err)
	}
}

func TestBootstrapCleanupAfterCacheChangeRetainsRecoveryState(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("XDG_CACHE_HOME controls the user cache on Linux")
	}
	for _, action := range []string{"release", "rollback"} {
		t.Run(action, func(t *testing.T) {
			cache := t.TempDir()
			t.Setenv("XDG_CACHE_HOME", cache)
			f := newCleanupClaimFixture(t, "cbx_cache_change", "cache-change-container", "running", false)
			if err := os.MkdirAll(localContainerBootstrapRoot(), 0o700); err != nil {
				t.Fatal(err)
			}
			dir, err := os.MkdirTemp(localContainerBootstrapRoot(), "crabbox-bootstrap-*")
			if err != nil {
				t.Fatal(err)
			}
			if err := core.WithDurableLeaseClaimLock(f.leaseID, func(claim *core.LeaseClaim, exists bool, save func() error) error {
				if !exists {
					t.Fatal("missing fixture claim")
				}
				claim.Labels["bootstrap_dir"] = dir
				return save()
			}); err != nil {
				t.Fatal(err)
			}
			f.claim, err = core.ReadLeaseClaim(f.leaseID)
			if err != nil {
				t.Fatal(err)
			}
			lease := core.LeaseTarget{LeaseID: f.leaseID, Server: core.Server{CloudID: f.containerID, Labels: f.claim.Labels}}
			cleanup := func() error {
				if action == "rollback" {
					return f.b.rollbackPendingLease(f.claim, lease, dir)
				}
				return f.b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease})
			}
			t.Setenv("XDG_CACHE_HOME", t.TempDir())
			if err := cleanup(); err == nil || !strings.Contains(err.Error(), "outside the current cache/temp roots") {
				t.Fatalf("cleanup error=%v", err)
			}
			for _, path := range []string{dir, f.keyPath} {
				if _, err := os.Lstat(path); err != nil {
					t.Fatalf("recovery path %s missing: %v", path, err)
				}
			}
			if claim, err := core.ReadLeaseClaim(f.leaseID); err != nil || claim.LeaseID != f.leaseID {
				t.Fatalf("claim lost: %#v %v", claim, err)
			}
			t.Setenv("XDG_CACHE_HOME", cache)
			if err := cleanup(); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{dir, f.keyPath} {
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Fatalf("recovery path %s remains: %v", path, err)
				}
			}
		})
	}
}

func TestRemoveBootstrapDirHandlesAbsentAndDanglingUntrustedPaths(t *testing.T) {
	path := filepath.Join(t.TempDir(), "crabbox-bootstrap-old-cache")
	b := testBackend(&recordingRunner{})
	if err := b.removeBootstrapDir(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "missing-target"), path); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := b.removeBootstrapDir(path); err == nil {
		t.Fatal("dangling untrusted symlink was treated as absent")
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("symlink removed: %v", err)
	}
}

func TestCleanupRetainsOrphanClaimWithBootstrapResidue(t *testing.T) {
	f := newCleanupClaimFixture(t, "cbx_orphan_bootstrap", "missing-bootstrap-container", "running", false)
	f.runner.run = func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		if result, ok := defaultLocalContainerScopeResponse(req); ok {
			return result, nil
		}
		if firstArg(req.Args) == "inspect" {
			return core.LocalCommandResult{Stderr: "Error: No such object: missing-bootstrap-container", ExitCode: 1}, errors.New("exit status 1")
		}
		return core.LocalCommandResult{}, nil
	}
	if err := f.b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if claim, err := core.ReadLeaseClaim(f.leaseID); err != nil || claim.LeaseID != f.leaseID {
		t.Fatalf("orphan recovery claim lost: %#v %v", claim, err)
	}
	for _, path := range []string{f.bootstrapDir, f.keyPath} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("recovery path missing: %v", err)
		}
	}
	if !strings.Contains(f.output.String(), "bootstrap-cleanup-pending") {
		t.Fatalf("missing recovery diagnostic: %s", f.output.String())
	}
}

func TestReleaseLeaseRemovesDockerSocketHostWorkRoot(t *testing.T) {
	hostRoot := t.TempDir()
	markTestLocalContainerWorkRoot(t, hostRoot)
	leaseRoot := filepath.Join(hostRoot, "cbx_release")
	if err := os.MkdirAll(filepath.Join(leaseRoot, "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"rm", "-f", "container123"}): {},
		},
	}
	addDefaultLocalContainerScopeResponses(runner)
	writeLocalContainerReleaseClaim(t, "cbx_release", "container123")
	b := testBackend(runner)
	lease := core.LeaseTarget{
		LeaseID: "cbx_release",
		Server: core.Server{
			CloudID: "container123",
			Labels: map[string]string{
				"docker_socket": "1",
				"work_root":     hostRoot,
			},
		},
	}
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(leaseRoot); !os.IsNotExist(err) {
		t.Fatalf("host lease root still exists after release: %v", err)
	}
	if _, err := os.Stat(hostRoot); err != nil {
		t.Fatalf("host work root parent removed: %v", err)
	}
}

func TestReleaseLeaseWithIDResolvesHostWorkRoot(t *testing.T) {
	hostRoot := t.TempDir()
	markTestLocalContainerWorkRoot(t, hostRoot)
	leaseRoot := filepath.Join(hostRoot, "cbx_release")
	if err := os.MkdirAll(filepath.Join(leaseRoot, "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	inspectJSON := `[{
		"Id":"container1234567890",
		"Name":"/crabbox-release",
		"Config":{"Image":"ubuntu:24.04","Labels":{"crabbox":"true","provider":"local-container","lease":"cbx_release","slug":"release-root","state":"ready","server_type":"ubuntu:24.04","ssh_user":"runner","work_root":"` + hostRoot + `","docker_socket":"1"}},
		"State":{"Status":"running","Running":true},
		"NetworkSettings":{"Ports":{"2222/tcp":[{"HostIp":"127.0.0.1","HostPort":"49153"}]}}
	}]`
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"ps", "-a", "--filter", "label=crabbox=true", "--filter", "label=provider=local-container", "--format", "{{.ID}}"}): {Stdout: "container1234567890\n"},
			commandKey([]string{"inspect", "container1234567890"}):  {Stdout: inspectJSON},
			commandKey([]string{"rm", "-f", "container1234567890"}): {},
		},
	}
	addDefaultLocalContainerScopeResponses(runner)
	writeLocalContainerReleaseClaim(t, "cbx_release", "container1234567890")
	b := testBackend(runner)
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{LeaseID: "cbx_release"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(leaseRoot); !os.IsNotExist(err) {
		t.Fatalf("host lease root still exists after release by id: %v", err)
	}
}

type cleanupClaimFixture struct {
	b             *backend
	runner        *recordingRunner
	claim         core.LeaseClaim
	leaseID       string
	containerID   string
	keyPath       string
	bootstrapDir  string
	hostLeaseRoot string
	output        *strings.Builder
}

func newCleanupClaimFixture(t *testing.T, leaseID, containerID, status string, keep bool) *cleanupClaimFixture {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	hostRoot := t.TempDir()
	markTestLocalContainerWorkRoot(t, hostRoot)
	hostLeaseRoot := filepath.Join(hostRoot, leaseID)
	if err := os.MkdirAll(filepath.Join(hostLeaseRoot, "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	bootstrapDir, err := os.MkdirTemp("", "crabbox-bootstrap-cleanup-fence-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(bootstrapDir) })
	labels := testCapturedScopeLabels(map[string]string{
		"crabbox": "true", "provider": providerName, "lease": leaseID, "slug": "cleanup-fence",
		"state": "ready", "keep": strconv.FormatBool(keep), "docker_socket": "1",
		"host_work_root": hostRoot, "bootstrap_dir": bootstrapDir, "bootstrap_owned": "true", "ssh_key_owned": "true",
	})
	if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(
		leaseID, "cleanup-fence", providerName, "runtime:docker/context:default", "", t.TempDir(), time.Minute, false,
		core.Server{CloudID: containerID, Provider: providerName, Labels: labels}, core.SSHTarget{},
	); err != nil {
		t.Fatal(err)
	}
	claim, err := core.ReadLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	keyPath, err := core.TestboxKeyPath(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		core.RemoveLeaseClaim(leaseID)
		core.RemoveStoredTestboxKey(leaseID)
	})
	containerLabels := publicLocalContainerClaimLabels(labels)
	running := status == "running"
	inspectData, err := json.Marshal([]inspectContainer{{
		ID: containerID, Name: "/cleanup-fence", Config: inspectConfig{Image: "ubuntu:24.04", Labels: containerLabels},
		State: inspectState{Status: status, Running: running},
	}})
	if err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
		commandKey([]string{"ps", "-a", "--filter", "label=crabbox=true", "--filter", "label=provider=local-container", "--format", "{{.ID}}"}): {Stdout: containerID + "\n"},
		commandKey([]string{"inspect", containerID}):  {Stdout: string(inspectData)},
		commandKey([]string{"rm", "-f", containerID}): {},
	}}
	addDefaultLocalContainerScopeResponses(runner)
	output := &strings.Builder{}
	b := testBackend(runner)
	b.rt.Stdout = output
	b.rt.Stderr = output
	return &cleanupClaimFixture{
		b: b, runner: runner, claim: claim, leaseID: leaseID, containerID: containerID,
		keyPath: keyPath, bootstrapDir: bootstrapDir, hostLeaseRoot: hostLeaseRoot, output: output,
	}
}

func (f *cleanupClaimFixture) expire(t *testing.T) {
	t.Helper()
	updated, err := core.UpdateLeaseClaimLabelsAndLastUsedIfUnchanged(
		f.leaseID, f.claim, f.claim.Labels, time.Now().Add(-48*time.Hour),
	)
	if err != nil {
		t.Fatal(err)
	}
	f.claim = updated
}

func (f *cleanupClaimFixture) assertOwnershipPresent(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(f.keyPath); err != nil {
		t.Fatalf("key missing: %v", err)
	}
	if _, err := os.Stat(f.bootstrapDir); err != nil {
		t.Fatalf("bootstrap directory missing: %v", err)
	}
	if _, err := os.Stat(f.hostLeaseRoot); err != nil {
		t.Fatalf("host lease root missing: %v", err)
	}
}

func recordedCommandCount(runner *recordingRunner, command string) int {
	count := 0
	for _, call := range runner.calls {
		if len(call.Args) > 0 && call.Args[0] == command {
			count++
		}
	}
	return count
}

func TestCleanupLiveClaimChangeFencesMutation(t *testing.T) {
	for _, dryRun := range []bool{false, true} {
		t.Run(fmt.Sprintf("dry_run_%t", dryRun), func(t *testing.T) {
			fixture := newCleanupClaimFixture(t, "cbx_cleanup_reclaimed", "cleanup-old-container", "exited", false)
			replacement := fixture.claim
			replacement.CloudID = "cleanup-replacement-container"
			replacement.Labels = cloneLabels(replacement.Labels)
			replaced := false
			fixture.b.beforeCleanupMutation = func(leaseID string) {
				if leaseID != fixture.leaseID || replaced {
					return
				}
				replaced = true
				if err := core.ReplaceLeaseClaimIfUnchanged(fixture.leaseID, fixture.claim, replacement); err != nil {
					t.Fatalf("replace cleanup claim: %v", err)
				}
			}
			if err := fixture.b.Cleanup(context.Background(), core.CleanupRequest{DryRun: dryRun}); err != nil {
				t.Fatal(err)
			}
			if recordedCommandCount(fixture.runner, "rm") != 0 {
				t.Fatalf("cleanup removed container after claim rewrite: %s", fixture.output.String())
			}
			got, err := core.ReadLeaseClaim(fixture.leaseID)
			if err != nil || got.CloudID != replacement.CloudID {
				t.Fatalf("replacement claim=%#v err=%v", got, err)
			}
			fixture.assertOwnershipPresent(t)
			if !strings.Contains(fixture.output.String(), "reason=changed-during-cleanup") {
				t.Fatalf("cleanup did not report changed claim: %s", fixture.output.String())
			}
			if strings.Contains(fixture.output.String(), "remove container") {
				t.Fatalf("cleanup reported removal after claim rewrite: %s", fixture.output.String())
			}
		})
	}
}

func TestCleanupLiveClaimScopeChangeFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "daemon identity", err: errors.New("daemon identity changed")},
		{name: "endpoint identity", err: errors.New("endpoint identity changed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newCleanupClaimFixture(t, "cbx_cleanup_scope_change", "cleanup-scope-container", "exited", false)
			fixture.b.validateRuntimeScope = func(context.Context, checkpointScope) error { return tc.err }
			if err := fixture.b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
				t.Fatal(err)
			}
			if recordedCommandCount(fixture.runner, "rm") != 0 {
				t.Fatalf("cleanup removed container after scope mismatch: %s", fixture.output.String())
			}
			got, err := core.ReadLeaseClaim(fixture.leaseID)
			if err != nil || got.CloudID != fixture.containerID {
				t.Fatalf("claim=%#v err=%v", got, err)
			}
			fixture.assertOwnershipPresent(t)
			if !strings.Contains(fixture.output.String(), "reason=captured-scope-unavailable") {
				t.Fatalf("cleanup did not report scope failure: %s", fixture.output.String())
			}
		})
	}
}

func TestCleanupClaimlessContainerSkipsClaimCreatedBeforeMutation(t *testing.T) {
	fixture := newCleanupClaimFixture(t, "cbx_cleanup_claim_appears", "cleanup-claimless-container", "exited", false)
	core.RemoveLeaseClaim(fixture.leaseID)
	appeared := false
	fixture.b.beforeCleanupMutation = func(leaseID string) {
		if leaseID != fixture.leaseID || appeared {
			return
		}
		appeared = true
		labels := testCapturedScopeLabels(map[string]string{
			"crabbox": "true", "provider": providerName, "lease": leaseID, "slug": "new-owner", "state": "ready",
		})
		if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(
			leaseID, "new-owner", providerName, "runtime:docker/context:default", "", t.TempDir(), time.Minute, false,
			core.Server{CloudID: "new-owner-container", Provider: providerName, Labels: labels}, core.SSHTarget{},
		); err != nil {
			t.Fatalf("create concurrent claim: %v", err)
		}
	}
	if err := fixture.b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if recordedCommandCount(fixture.runner, "rm") != 0 {
		t.Fatalf("cleanup removed claimless container after claim appeared: %s", fixture.output.String())
	}
	claim, err := core.ReadLeaseClaim(fixture.leaseID)
	if err != nil || claim.CloudID != "new-owner-container" {
		t.Fatalf("concurrent claim=%#v err=%v", claim, err)
	}
	fixture.assertOwnershipPresent(t)
	if !strings.Contains(fixture.output.String(), "reason=changed-during-cleanup") {
		t.Fatalf("cleanup did not report appearing claim: %s", fixture.output.String())
	}
}

func TestCleanupExactExpiredClaimRemovesOwnershipOnce(t *testing.T) {
	fixture := newCleanupClaimFixture(t, "cbx_cleanup_exact_expired", "cleanup-exact-container", "running", false)
	fixture.expire(t)
	if err := fixture.b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if got := recordedCommandCount(fixture.runner, "rm"); got != 1 {
		t.Fatalf("container remove calls=%d, want 1", got)
	}
	if claim, err := core.ReadLeaseClaim(fixture.leaseID); err != nil || claim.LeaseID != "" {
		t.Fatalf("claim=%#v err=%v", claim, err)
	}
	for name, path := range map[string]string{"key": fixture.keyPath, "bootstrap": fixture.bootstrapDir, "host root": fixture.hostLeaseRoot} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s remains after cleanup: %v", name, err)
		}
	}
	if got := strings.Count(fixture.output.String(), "remove container id="); got != 1 {
		t.Fatalf("remove output count=%d, want 1: %s", got, fixture.output.String())
	}
}

func TestCleanupMissingPendingRechecksAbsenceInsideFence(t *testing.T) {
	for _, tc := range []struct {
		name     string
		absent   bool
		checkErr error
	}{
		{name: "container reappears", absent: false},
		{name: "captured route fails", checkErr: errors.New("captured route unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newCleanupClaimFixture(t, "cbx_cleanup_missing_pending", "cleanup-missing-pending", "exited", false)
			fixture.runner.responses[commandKey([]string{"ps", "-a", "--filter", "label=crabbox=true", "--filter", "label=provider=local-container", "--format", "{{.ID}}"})] = core.LocalCommandResult{}
			labels := cloneLabels(fixture.claim.Labels)
			labels["state"] = pendingClaimState
			labels["recovery"] = pendingRecoveryKind
			updated, err := core.UpdateLeaseClaimLabelsIfUnchanged(fixture.leaseID, fixture.claim, labels)
			if err != nil {
				t.Fatal(err)
			}
			fixture.claim = updated
			checks := 0
			fixture.b.confirmContainerAbsent = func(context.Context, string) (bool, error) {
				checks++
				return tc.absent, tc.checkErr
			}
			if err := fixture.b.Cleanup(context.Background(), core.CleanupRequest{}); err == nil {
				t.Fatal("cleanup succeeded without an in-fence absence proof")
			}
			if checks != 1 {
				t.Fatalf("in-fence absence checks=%d, want 1", checks)
			}
			claim, err := core.ReadLeaseClaim(fixture.leaseID)
			if err != nil || claim.LeaseID != fixture.leaseID {
				t.Fatalf("claim=%#v err=%v", claim, err)
			}
			fixture.assertOwnershipPresent(t)
			if strings.Contains(fixture.output.String(), "remove claim") {
				t.Fatalf("cleanup reported pending claim removal: %s", fixture.output.String())
			}
		})
	}
}

func TestCleanupPreservesKeepAndActiveClaimedContainers(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status string
		keep   bool
		reason string
	}{
		{name: "keep", status: "exited", keep: true, reason: "keep=true"},
		{name: "active claim", status: "running", reason: "claim active"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newCleanupClaimFixture(t, "cbx_cleanup_preserve_"+strings.ReplaceAll(tc.name, " ", "_"), "cleanup-preserve-container", tc.status, tc.keep)
			if err := fixture.b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
				t.Fatal(err)
			}
			if recordedCommandCount(fixture.runner, "rm") != 0 {
				t.Fatalf("cleanup removed preserved container: %s", fixture.output.String())
			}
			claim, err := core.ReadLeaseClaim(fixture.leaseID)
			if err != nil || claim.LeaseID != fixture.leaseID {
				t.Fatalf("claim=%#v err=%v", claim, err)
			}
			fixture.assertOwnershipPresent(t)
			if !strings.Contains(fixture.output.String(), "reason="+tc.reason) {
				t.Fatalf("cleanup skip reason missing: %s", fixture.output.String())
			}
		})
	}
}

func TestCleanupRemovesExpiredLocalContainers(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	bootstrapDir, err := os.MkdirTemp("", "crabbox-bootstrap-cleanup-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(bootstrapDir) })
	hostRoot := t.TempDir()
	markTestLocalContainerWorkRoot(t, hostRoot)
	leaseRoot := filepath.Join(hostRoot, "cbx_cleanup")
	if err := os.MkdirAll(filepath.Join(leaseRoot, "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	created := time.Now().Add(-48 * time.Hour).Unix()
	inspectJSON := `[{
		"Id":"abcdef1234567890",
		"Name":"/crabbox-cleanup",
		"Config":{"Image":"ubuntu:24.04","Labels":{"crabbox":"true","provider":"local-container","lease":"cbx_cleanup","slug":"old-cleanup","state":"ready","server_type":"ubuntu:24.04","ssh_user":"runner","work_root":"` + hostRoot + `","docker_socket":"1","bootstrap_dir":` + strconv.Quote(bootstrapDir) + `,"expires_at":"` + strconv.FormatInt(created, 10) + `"}},
		"State":{"Status":"running","Running":true},
		"NetworkSettings":{"Ports":{"2222/tcp":[{"HostIp":"127.0.0.1","HostPort":"49153"}]}}
	}]`
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"ps", "-a", "--filter", "label=crabbox=true", "--filter", "label=provider=local-container", "--format", "{{.ID}}"}): {Stdout: "abcdef1234567890\n"},
			commandKey([]string{"inspect", "abcdef1234567890"}):  {Stdout: inspectJSON},
			commandKey([]string{"rm", "-f", "abcdef1234567890"}): {},
		},
	}
	b := testBackend(runner)
	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(leaseRoot); !os.IsNotExist(err) {
		t.Fatalf("host lease root still exists after cleanup: %v", err)
	}
	if _, err := os.Stat(bootstrapDir); !os.IsNotExist(err) {
		t.Fatalf("bootstrap directory still exists after cleanup: %v", err)
	}
	args := recordedArgsForCommand(t, runner, "rm")
	if !strings.Contains(args, "abcdef1234567890") {
		t.Fatalf("cleanup did not remove container:\n%s", args)
	}
}

func TestCleanupSkipsIncompleteClaimBeforeHostWorkRootMutation(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	claimDir := filepath.Join(stateHome, "crabbox", "claims")
	if err := os.MkdirAll(claimDir, 0o700); err != nil {
		t.Fatal(err)
	}
	expired := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)
	claimData := []byte(`{"leaseID":"cbx_cleanup","slug":"old-cleanup","provider":"` + providerName + `","repoRoot":` + strconv.Quote(t.TempDir()) + `,"claimedAt":` + strconv.Quote(expired) + `,"lastUsedAt":` + strconv.Quote(expired) + `,"idleTimeoutSeconds":60}`)
	if err := os.WriteFile(filepath.Join(claimDir, "cbx_cleanup.json"), claimData, 0o600); err != nil {
		t.Fatal(err)
	}
	keyPath, err := core.TestboxKeyPath("cbx_cleanup")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		core.RemoveLeaseClaim("cbx_cleanup")
		core.RemoveStoredTestboxKey("cbx_cleanup")
	})
	hostRoot := t.TempDir()
	markTestLocalContainerWorkRoot(t, hostRoot)
	leaseRoot := filepath.Join(hostRoot, "cbx_cleanup")
	if err := os.MkdirAll(filepath.Join(leaseRoot, "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	probeRoot := filepath.Join(hostRoot, "permission-probe")
	if err := os.MkdirAll(probeRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(hostRoot, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(hostRoot, 0o700)
	})
	if err := os.RemoveAll(probeRoot); err == nil {
		t.Skip("filesystem permissions do not block RemoveAll for this user")
	}
	created := time.Now().Add(-48 * time.Hour).Unix()
	inspectJSON := `[{
		"Id":"abcdef1234567890",
		"Name":"/crabbox-cleanup",
		"Config":{"Image":"ubuntu:24.04","Labels":{"crabbox":"true","provider":"local-container","lease":"cbx_cleanup","slug":"old-cleanup","state":"ready","server_type":"ubuntu:24.04","ssh_user":"runner","work_root":"` + hostRoot + `","docker_socket":"1","expires_at":"` + strconv.FormatInt(created, 10) + `"}},
		"State":{"Status":"running","Running":true},
		"NetworkSettings":{"Ports":{"2222/tcp":[{"HostIp":"127.0.0.1","HostPort":"49153"}]}}
	}]`
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"ps", "-a", "--filter", "label=crabbox=true", "--filter", "label=provider=local-container", "--format", "{{.ID}}"}): {Stdout: "abcdef1234567890\n"},
			commandKey([]string{"inspect", "abcdef1234567890"}):  {Stdout: inspectJSON},
			commandKey([]string{"rm", "-f", "abcdef1234567890"}): {},
		},
	}
	b := testBackend(runner)
	var stderr strings.Builder
	b.rt.Stderr = &stderr
	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if claim, err := core.ReadLeaseClaim("cbx_cleanup"); err != nil {
		t.Fatal(err)
	} else if claim.LeaseID != "cbx_cleanup" {
		t.Fatalf("incomplete claim was removed: %#v", claim)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("stored key removed for incomplete claim: %v", err)
	}
	if _, err := os.Stat(leaseRoot); err != nil {
		t.Fatalf("host lease root removed for incomplete claim: %v", err)
	}
	if got := recordedCommandCount(runner, "rm"); got != 0 {
		t.Fatalf("container remove calls=%d, want 0", got)
	}
	if !strings.Contains(stderr.String(), "reason=captured-scope-unavailable") {
		t.Fatalf("cleanup did not report incomplete captured scope: %s", stderr.String())
	}
}

func TestCleanupRemovesClaimWithoutContainer(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	scope := localContainerClaimScope("docker", "default")
	keyPath := writeLocalContainerClaimAndKey(t, "cbx_missing", "missing-container", scope)

	b := testBackend(&recordingRunner{responses: map[string]core.LocalCommandResult{
		commandKey([]string{"context", "show"}): {Stdout: "default\n"},
	}})
	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if claim, err := core.ReadLeaseClaim("cbx_missing"); err != nil {
		t.Fatal(err)
	} else if claim.LeaseID != "" {
		t.Fatalf("claim still exists after cleanup: %#v", claim)
	}
	// The orphan-claim sweep RETAINS the stored key (a concurrent Acquire/reclaim
	// may have prepared it before publishing a replacement claim); deleting it here
	// would strand a live lease. See the retention note in (*backend).Cleanup.
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("stored key should be retained after orphan-claim cleanup, got: %v", err)
	}
}

func TestCleanupRemovesMissingNonKeepPendingClaimAndSidecars(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_missing_nonkeep_pending"
	bootstrapDir, err := os.MkdirTemp("", "crabbox-bootstrap-nonkeep-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(bootstrapDir) })
	keyPath, err := core.TestboxKeyPath(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	labels := map[string]string{
		"crabbox": "true", "provider": providerName, "lease": leaseID, "slug": "missing-nonkeep",
		"state": pendingClaimState, "recovery": pendingRecoveryKind, "keep": "false",
		"bootstrap_dir": bootstrapDir, "bootstrap_owned": "true", "ssh_key_owned": "true",
	}
	labels = testCapturedScopeLabels(labels)
	if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(
		leaseID,
		"missing-nonkeep",
		providerName,
		"runtime:docker/context:default",
		"",
		t.TempDir(),
		time.Minute,
		false,
		core.Server{CloudID: "missing-container", Provider: providerName, Labels: labels},
		core.SSHTarget{},
	); err != nil {
		t.Fatal(err)
	}
	var stdout strings.Builder
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
		commandKey([]string{"ps", "-a", "--filter", "label=crabbox=true", "--filter", "label=provider=local-container", "--format", "{{.ID}}"}): {},
	}}
	addDefaultLocalContainerScopeResponses(runner)
	b := testBackend(runner)
	b.rt.Stdout = &stdout
	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if claim, err := core.ReadLeaseClaim(leaseID); err != nil || claim.LeaseID != "" {
		t.Fatalf("claim=%#v err=%v", claim, err)
	}
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatalf("key still exists: %v", err)
	}
	if _, err := os.Stat(bootstrapDir); !os.IsNotExist(err) {
		t.Fatalf("bootstrap directory still exists: %v", err)
	}
	if !strings.Contains(stdout.String(), "reason=missing non-keep pending container") {
		t.Fatalf("cleanup output=%q", stdout.String())
	}
}

func TestCleanupRetainsMissingPendingClaimWhenCapturedDaemonChanges(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_missing_changed_daemon"
	labels := testCapturedScopeLabels(map[string]string{
		"crabbox": "true", "provider": providerName, "lease": leaseID, "slug": "changed-daemon",
		"state": pendingClaimState, "recovery": pendingRecoveryKind, "keep": "false", "ssh_key_owned": "true",
	})
	if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(
		leaseID, "changed-daemon", providerName, "runtime:docker/context:default", "", t.TempDir(), time.Minute, false,
		core.Server{CloudID: "container-on-old-daemon", Provider: providerName, Labels: labels}, core.SSHTarget{},
	); err != nil {
		t.Fatal(err)
	}
	keyPath, err := core.TestboxKeyPath(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
		commandKey([]string{"ps", "-a", "--filter", "label=crabbox=true", "--filter", "label=provider=local-container", "--format", "{{.ID}}"}): {},
	}}
	addDefaultLocalContainerScopeResponses(runner)
	b := testBackend(runner)
	b.rt.Stderr = &stderr
	b.validateRuntimeScope = func(context.Context, checkpointScope) error { return errors.New("daemon changed") }
	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	claim, err := core.ReadLeaseClaim(leaseID)
	if err != nil || claim.LeaseID != leaseID {
		t.Fatalf("claim=%#v err=%v", claim, err)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("key removed after daemon mismatch: %v", err)
	}
	if !strings.Contains(stderr.String(), "captured-scope-unavailable") {
		t.Fatalf("cleanup did not report scope mismatch: %s", stderr.String())
	}
	core.RemoveLeaseClaim(leaseID)
	core.RemoveStoredTestboxKey(leaseID)
}

func TestCleanupRetainsReadyAndLegacyClaimsWithoutValidatedDaemon(t *testing.T) {
	for _, tc := range []struct {
		name      string
		fullScope bool
		validator error
	}{
		{name: "ready daemon mismatch", fullScope: true, validator: errors.New("daemon changed")},
		{name: "legacy missing identity"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			leaseID := "cbx_cleanup_scope_" + strings.ReplaceAll(tc.name, " ", "_")
			labels := map[string]string{"provider": providerName, "lease": leaseID, "slug": "cleanup-scope", "state": "ready"}
			if tc.fullScope {
				labels = testCapturedScopeLabels(labels)
			}
			if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(
				leaseID, "cleanup-scope", providerName, "runtime:docker/context:default", "", t.TempDir(), time.Minute, false,
				core.Server{CloudID: "container-on-captured-daemon", Provider: providerName, Labels: labels}, core.SSHTarget{},
			); err != nil {
				t.Fatal(err)
			}
			keyPath, err := core.TestboxKeyPath(leaseID)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(keyPath, []byte("private"), 0o600); err != nil {
				t.Fatal(err)
			}
			var stderr strings.Builder
			runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
				commandKey([]string{"ps", "-a", "--filter", "label=crabbox=true", "--filter", "label=provider=local-container", "--format", "{{.ID}}"}): {},
			}}
			addDefaultLocalContainerScopeResponses(runner)
			b := testBackend(runner)
			b.rt.Stderr = &stderr
			b.validateRuntimeScope = func(context.Context, checkpointScope) error { return tc.validator }
			if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
				t.Fatal(err)
			}
			claim, err := core.ReadLeaseClaim(leaseID)
			if err != nil || claim.LeaseID != leaseID {
				t.Fatalf("claim=%#v err=%v", claim, err)
			}
			if _, err := os.Stat(keyPath); err != nil {
				t.Fatalf("key removed without validated daemon: %v", err)
			}
			if !strings.Contains(stderr.String(), "captured-scope-unavailable") {
				t.Fatalf("cleanup did not retain evidence: %s", stderr.String())
			}
			core.RemoveLeaseClaim(leaseID)
			core.RemoveStoredTestboxKey(leaseID)
		})
	}
}

func TestCleanupDryRunKeepsClaimWithoutContainer(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	scope := localContainerClaimScope("docker", "default")
	keyPath := writeLocalContainerClaimAndKey(t, "cbx_missing", "missing-container", scope)

	b := testBackend(&recordingRunner{responses: map[string]core.LocalCommandResult{
		commandKey([]string{"context", "show"}): {Stdout: "default\n"},
	}})
	if err := b.Cleanup(context.Background(), core.CleanupRequest{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if claim, err := core.ReadLeaseClaim("cbx_missing"); err != nil {
		t.Fatal(err)
	} else if claim.LeaseID != "cbx_missing" {
		t.Fatalf("claim removed during dry-run: %#v", claim)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("stored key removed during dry-run: %v", err)
	}
}

func TestCleanupKeepsClaimFromDifferentRuntimeContext(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	keyPath := writeLocalContainerClaimAndKey(t, "cbx_other", "other-context", localContainerClaimScope("docker", "colima"))

	b := testBackend(&recordingRunner{responses: map[string]core.LocalCommandResult{
		commandKey([]string{"context", "show"}): {Stdout: "desktop-linux\n"},
	}})
	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if claim, err := core.ReadLeaseClaim("cbx_other"); err != nil {
		t.Fatal(err)
	} else if claim.LeaseID != "cbx_other" {
		t.Fatalf("claim from another context was removed: %#v", claim)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("stored key from another context was removed: %v", err)
	}
}

func TestCleanupKeepsClaimFromDifferentDockerHostSameContext(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	keyPath := writeLocalContainerClaimAndKey(t, "cbx_other_host", "other-host", localContainerClaimScope("docker", "default", "unix:///tmp/docker-a.sock"))
	t.Setenv("DOCKER_HOST", "unix:///tmp/docker-b.sock")

	b := testBackend(&recordingRunner{responses: map[string]core.LocalCommandResult{
		commandKey([]string{"context", "show"}): {Stdout: "default\n"},
	}})
	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if claim, err := core.ReadLeaseClaim("cbx_other_host"); err != nil {
		t.Fatal(err)
	} else if claim.LeaseID != "cbx_other_host" {
		t.Fatalf("claim from another Docker host was removed: %#v", claim)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("stored key from another Docker host was removed: %v", err)
	}
}

func TestCleanupRemovesStaleLegacyUnscopedClaimWithoutContainer(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	lastUsed := time.Now().Add(-48 * time.Hour).UTC()
	keyPath := writeLocalContainerClaimAndKeyAt(t, "cbx_legacy", "legacy-missing", "", lastUsed, time.Minute)

	b := testBackend(&recordingRunner{responses: map[string]core.LocalCommandResult{
		commandKey([]string{"context", "show"}): {Stdout: "default\n"},
	}})
	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if claim, err := core.ReadLeaseClaim("cbx_legacy"); err != nil {
		t.Fatal(err)
	} else if claim.LeaseID != "" {
		t.Fatalf("stale legacy claim still exists after cleanup: %#v", claim)
	}
	// The stored key is intentionally RETAINED (not deleted) by the sweep: a
	// concurrent Acquire/reclaim can prepare this lease's key before publishing a
	// replacement claim, so deleting it in cleanup would risk a live lease losing
	// its SSH credentials. See the retention note in (*backend).Cleanup. A genuinely
	// dead lease's key is a harmless small residue.
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("stored key should be retained after cleanup, got: %v", err)
	}
}

func writeLocalContainerClaimAndKey(t *testing.T, leaseID, slug string, scopes ...string) string {
	t.Helper()
	scope := ""
	if len(scopes) > 0 {
		scope = scopes[0]
	}
	return writeLocalContainerClaimAndKeyAt(t, leaseID, slug, scope, time.Now().UTC(), time.Minute)
}

func createLocalContainerTouchClaim(t *testing.T, idleTimeout time.Duration) (string, string, core.LeaseClaim, *recordingRunner) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_local_touch"
	containerID := strings.Repeat("a", 64)
	labels := testCapturedScopeLabels(map[string]string{
		"bootstrap_dir":   filepath.Join(os.TempDir(), "crabbox-bootstrap-touch test"),
		"container_id":    containerID[:12],
		"crabbox":         "true",
		"docker_socket":   "1",
		"host_work_root":  filepath.Join(os.TempDir(), "crabbox local touch"),
		"image":           "ubuntu:24.04",
		"lease":           leaseID,
		"provider":        providerName,
		"runtime_context": "default",
		"server_type":     "ubuntu:24.04",
		"slug":            "local-touch",
		"ssh_port":        "49153",
		"ssh_user":        "runner",
		"state":           "ready",
		"created_at":      core.LeaseLabelTime(time.Date(2026, time.August, 17, 20, 0, 0, 0, time.UTC)),
		"ttl_secs":        "86400",
		"work_root":       "/workspace/crabbox touch",
	})
	labels[checkpointMetadataConfig] = filepath.Join(os.TempDir(), "docker config touch")
	labels[checkpointMetadataEndpoint] = "unix:///tmp/docker touch.sock"
	server := core.Server{Provider: providerName, CloudID: containerID, Status: "ready", Labels: labels}
	server.PublicNet.IPv4.IP = "127.0.0.1"
	if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(
		leaseID,
		"local-touch",
		providerName,
		"runtime:docker/context:default",
		"",
		t.TempDir(),
		idleTimeout,
		false,
		server,
		core.SSHTarget{Host: "127.0.0.1", Port: "49153", User: "runner"},
	); err != nil {
		t.Fatal(err)
	}
	claim, exists, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil || !exists {
		t.Fatalf("read touch claim exists=%t err=%v", exists, err)
	}
	return leaseID, containerID, claim, localContainerTouchRunner(t, leaseID, containerID)
}

func localContainerTouchRunner(t *testing.T, leaseID, containerID string) *recordingRunner {
	t.Helper()
	container := inspectContainer{
		ID:   containerID,
		Name: "/crabbox-local-touch",
		Config: inspectConfig{Image: "ubuntu:24.04", Labels: map[string]string{
			"crabbox": "true", "provider": providerName, "lease": leaseID, "slug": "local-touch",
			"state": "ready", "server_type": "ubuntu:24.04", "ssh_user": "runner", "work_root": "/workspace/crabbox touch",
		}},
		State: inspectState{Status: "running", Running: true},
		NetworkSettings: inspectNetworking{Ports: map[string][]inspectPort{
			sshPort + "/tcp": {{HostIP: "127.0.0.1", HostPort: "49153"}},
		}},
	}
	data, err := json.Marshal([]inspectContainer{container})
	if err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
		commandKey([]string{"ps", "-a", "--filter", "label=crabbox=true", "--filter", "label=provider=local-container", "--format", "{{.ID}}"}): {Stdout: containerID + "\n"},
		commandKey([]string{"inspect", containerID}): {Stdout: string(data)},
	}}
	addDefaultLocalContainerScopeResponses(runner)
	return runner
}

func durationPointer(value time.Duration) *time.Duration { return &value }

func unixLocalContainerLabelTime(t *testing.T, value string) time.Time {
	t.Helper()
	seconds, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		t.Fatalf("parse timestamp label %q: %v", value, err)
	}
	return time.Unix(seconds, 0).UTC()
}

func writeLocalContainerReleaseClaim(t *testing.T, leaseID, containerID string) {
	writeLocalContainerReleaseClaimWithScope(t, leaseID, containerID, "runtime:docker/context:default")
}

func writeLocalContainerReleaseClaimWithScope(t *testing.T, leaseID, containerID, scope string) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	stateHome := os.Getenv("XDG_STATE_HOME")
	claimDir := filepath.Join(stateHome, "crabbox", "claims")
	if err := os.MkdirAll(claimDir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	data := []byte(`{"leaseID":` + strconv.Quote(leaseID) + `,"slug":"release","provider":` + strconv.Quote(providerName) + `,"cloudID":` + strconv.Quote(containerID) + `,"providerScope":` + strconv.Quote(scope) + `,"repoRoot":` + strconv.Quote(t.TempDir()) + `,"claimedAt":` + strconv.Quote(now) + `,"lastUsedAt":` + strconv.Quote(now) + `,"idleTimeoutSeconds":60,"labels":{"runtime":"docker","docker_context":"default","docker_endpoint":"unix:///tmp/docker-test.sock","docker_daemon_id":"daemon-test"}}`)
	if err := os.WriteFile(filepath.Join(claimDir, leaseID+".json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func defaultLocalContainerScopeResponse(req core.LocalCommandRequest) (core.LocalCommandResult, bool) {
	switch commandKey(req.Args) {
	case commandKey([]string{"context", "show"}):
		return core.LocalCommandResult{Stdout: "default\n"}, true
	case commandKey([]string{"context", "inspect", "--format", "{{json .Endpoints.docker.Host}}"}):
		return core.LocalCommandResult{}, true
	default:
		return core.LocalCommandResult{}, false
	}
}

func addDefaultLocalContainerScopeResponses(runner *recordingRunner) {
	if runner.responses == nil {
		runner.responses = map[string]core.LocalCommandResult{}
	}
	for _, args := range [][]string{
		{"context", "show"},
		{"context", "inspect", "--format", "{{json .Endpoints.docker.Host}}"},
	} {
		result, _ := defaultLocalContainerScopeResponse(core.LocalCommandRequest{Args: args})
		runner.responses[commandKey(args)] = result
	}
}

func testCapturedScopeLabels(labels map[string]string) map[string]string {
	out := cloneLabels(labels)
	for key, value := range checkpointScopeMetadata(checkpointScope{
		Runtime: "docker", Context: "default", Endpoint: "unix:///tmp/docker-test.sock", DaemonID: "daemon-test",
	}) {
		if value != "" {
			out[key] = value
		}
	}
	return out
}

func writeLocalContainerClaimAndKeyAt(t *testing.T, leaseID, slug, scope string, lastUsed time.Time, idle time.Duration) string {
	t.Helper()
	if err := writeLocalContainerClaim(t, leaseID, slug, scope, lastUsed, idle); err != nil {
		t.Fatal(err)
	}
	keyPath, err := core.TestboxKeyPath(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	return keyPath
}

func writeLocalContainerClaim(t *testing.T, leaseID, slug, scope string, lastUsed time.Time, idle time.Duration) error {
	t.Helper()
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		t.Fatal("XDG_STATE_HOME must be set before writing test claims")
	}
	claimDir := filepath.Join(stateHome, "crabbox", "claims")
	if err := os.MkdirAll(claimDir, 0o700); err != nil {
		return err
	}
	scopeField := ""
	if scope != "" {
		scopeField = `,"providerScope":` + strconv.Quote(scope)
	}
	data := []byte(`{"leaseID":` + strconv.Quote(leaseID) + `,"slug":` + strconv.Quote(slug) + `,"provider":` + strconv.Quote(providerName) + scopeField + `,"repoRoot":` + strconv.Quote(t.TempDir()) + `,"claimedAt":` + strconv.Quote(lastUsed.Format(time.RFC3339)) + `,"lastUsedAt":` + strconv.Quote(lastUsed.Format(time.RFC3339)) + `,"idleTimeoutSeconds":` + strconv.Itoa(int(idle.Seconds())) + `,"labels":{"runtime":"docker","docker_context":"default","docker_endpoint":"unix:///tmp/docker-test.sock","docker_daemon_id":"daemon-test"}}`)
	return os.WriteFile(filepath.Join(claimDir, leaseID+".json"), data, 0o600)
}

func markTestLocalContainerWorkRoot(t *testing.T, root string) {
	t.Helper()
	if err := markLocalContainerWorkRoot(root); err != nil {
		t.Fatal(err)
	}
}

func TestCreateContainerMountsHostVolumes(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	b := testBackend(runner)
	cfg := b.configForRun()
	cfg.LocalContainer.Volumes = []string{
		"/home/user/.config/myapp:/mnt/myapp-config:ro",
		"/var/cache/models:/cache",
		`C:\Users\alice\source:/mnt/windows-source:ro`,
	}
	runner.responses[commandKey([]string{"run"})] = core.LocalCommandResult{Stdout: "container123456\n"}

	if _, _, err := b.createContainer(context.Background(), cfg, "crabbox-test", "cbx_vol", "vol-test", "ssh-ed25519 AAAA test", true); err != nil {
		t.Fatal(err)
	}
	args := recordedArgsForCommand(t, runner, "run")
	for _, vol := range cfg.LocalContainer.Volumes {
		want := "-v\n" + vol
		if !strings.Contains(args, want) {
			t.Fatalf("host volume mount missing %q:\n%s", want, args)
		}
	}
}

func TestCreateContainerRejectsHostVolumeOverlapWithBootstrapPaths(t *testing.T) {
	tests := []struct {
		name    string
		volume  string
		desktop bool
		cache   []core.CacheVolumeConfig
	}{
		{name: "work root", volume: "/host:/workspace/crabbox:ro"},
		{name: "work root parent", volume: "/host:/workspace"},
		{name: "work root child", volume: "/host:/workspace/crabbox/repo"},
		{name: "ssh config", volume: "/host:/home/runner/.ssh/known_hosts"},
		{name: "cache root", volume: "/host:/var/cache/crabbox/models"},
		{name: "bootstrap state", volume: "/host:/var/lib/crabbox"},
		{name: "system config", volume: "/host:/etc/ssh"},
		{name: "desktop config windows source", volume: `C:\Users\alice\config:/home/runner/.config/app:ro`, desktop: true},
		{
			name:   "configured cache path",
			volume: "/host:/opt/shared/cache/models",
			cache:  []core.CacheVolumeConfig{{Key: "models", Path: "/opt/shared/cache"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
			b := testBackend(runner)
			cfg := b.configForRun()
			cfg.Desktop = tc.desktop
			cfg.Cache.Volumes = tc.cache
			cfg.LocalContainer.Volumes = []string{tc.volume}

			_, _, err := b.createContainer(context.Background(), cfg, "crabbox-test", "cbx_overlap", "overlap-test", "ssh-ed25519 AAAA test", true)
			if err == nil || !strings.Contains(err.Error(), "overlaps bootstrap-managed path") {
				t.Fatalf("err=%v, want bootstrap-managed path rejection", err)
			}
			if len(runner.calls) != 0 {
				t.Fatalf("runtime invoked before volume validation: %#v", runner.commandSummary())
			}
		})
	}
}

func TestCreateContainerRejectsRelativeWorkRootWithHostVolume(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	b := testBackend(runner)
	cfg := b.configForRun()
	cfg.LocalContainer.WorkRoot = "mnt/app"
	cfg.LocalContainer.Volumes = []string{"/host/project:/mnt/app"}

	_, _, err := b.createContainer(context.Background(), cfg, "crabbox-test", "cbx_relative", "relative-test", "ssh-ed25519 AAAA test", true)
	if err == nil || !strings.Contains(err.Error(), "work root") || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("err=%v, want absolute work root rejection", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("runtime invoked before work root validation: %#v", runner.commandSummary())
	}
}

func TestLocalContainerVolumeDestinationRejectsRelativeTarget(t *testing.T) {
	_, err := localContainerVolumeDestination("/host:relative:ro")
	if err == nil || !strings.Contains(err.Error(), "absolute container path") {
		t.Fatalf("err=%v, want absolute destination rejection", err)
	}
}

func TestBootstrapChecksHostVolumesAgainstResolvedHomeBeforeMutation(t *testing.T) {
	guard := strings.Index(bootstrapScript, "CRABBOX_HOST_VOLUME_PATH_")
	install := strings.Index(bootstrapScript, "apt-get update")
	if guard < 0 || install < 0 || guard > install {
		t.Fatalf("host volume guard must run before package or filesystem mutation: guard=%d install=%d", guard, install)
	}
	for _, want := range []string{
		`while IFS=: read -r account _ _ _ _ account_home _`,
		`check_host_volume_path "$host_path"`,
		`resolved="$(readlink -f "$probe"`,
		`host_path="$(resolve_container_path "$1")"`,
		`managed_path="$(resolve_container_path "$managed_path")"`,
		`"$work_root" "$home_dir"`,
		`CRABBOX_CACHE_VOLUME_PATH_`,
		`useradd -m -d "$home_dir" -s /bin/bash "$user"`,
	} {
		if !strings.Contains(bootstrapScript, want) {
			t.Fatalf("bootstrap host volume guard missing %q", want)
		}
	}
}

func TestValidateCheckpointForkWorkdirUsesContainerResolvedPaths(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	b := testBackend(runner)
	b.cfg.LocalContainer.Volumes = []string{
		"/host/data:/mnt/data:ro",
		`C:\Users\alice\cache:/cache`,
	}
	lease := core.LeaseTarget{Server: core.Server{CloudID: "container123"}}

	if err := b.ValidateCheckpointForkWorkdir(context.Background(), lease, "/safe/link/.."); err != nil {
		t.Fatal(err)
	}
	args := recordedArgsForCommand(t, runner, "exec")
	for _, want := range []string{
		"container123",
		"crabbox-validate-checkpoint-workdir",
		"/safe/link/..",
		"/mnt/data",
		"/cache",
		"checkpoint fork workdir",
		"os.path.realpath",
	} {
		if !strings.Contains(args, want) {
			t.Fatalf("checkpoint workdir validation command missing %q:\n%s", want, args)
		}
	}
}

func TestValidateCheckpointForkWorkdirPropagatesOverlapFailure(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	runner.run = func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		if slices.Contains(req.Args, "exec") {
			return core.LocalCommandResult{Stderr: "checkpoint fork workdir /mnt/data overlaps local-container host volume target /mnt/data\n"}, errors.New("exit status 2")
		}
		return core.LocalCommandResult{}, nil
	}
	b := testBackend(runner)
	b.cfg.LocalContainer.Volumes = []string{"/host/data:/mnt/data"}

	err := b.ValidateCheckpointForkWorkdir(context.Background(), core.LeaseTarget{Server: core.Server{CloudID: "container123"}}, "/mnt/data")
	if err == nil || !strings.Contains(err.Error(), "overlaps local-container host volume") {
		t.Fatalf("err=%v, want overlap failure", err)
	}
}

func TestCreateContainerNoVolumesWhenEmpty(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	b := testBackend(runner)
	cfg := b.configForRun()
	cfg.LocalContainer.Volumes = nil
	runner.responses[commandKey([]string{"run"})] = core.LocalCommandResult{Stdout: "container123456\n"}

	if _, _, err := b.createContainer(context.Background(), cfg, "crabbox-test", "cbx_novol", "novol-test", "ssh-ed25519 AAAA test", true); err != nil {
		t.Fatal(err)
	}
	args := recordedArgsForCommand(t, runner, "run")
	lines := strings.Split(args, "\n")
	for i, line := range lines {
		if line == "-v" && i+1 < len(lines) {
			mount := lines[i+1]
			if !strings.Contains(mount, "docker.sock") &&
				!strings.HasPrefix(mount, "crabbox-cache-") &&
				!strings.HasSuffix(mount, ":/tmp/crabbox-bootstrap:ro") {
				t.Fatalf("unexpected volume mount when Volumes is empty: %s", mount)
			}
		}
	}
}

func TestVolumeListFlagParsesRepeated(t *testing.T) {
	var vols volumeListFlag
	if err := vols.Set("/host/a:/guest/a:ro"); err != nil {
		t.Fatal(err)
	}
	if err := vols.Set("/host/b:/guest/b"); err != nil {
		t.Fatal(err)
	}
	if len(vols) != 2 {
		t.Fatalf("expected 2 volumes, got %d", len(vols))
	}
	if vols[0] != "/host/a:/guest/a:ro" {
		t.Fatalf("volume[0]=%q", vols[0])
	}
	if vols[1] != "/host/b:/guest/b" {
		t.Fatalf("volume[1]=%q", vols[1])
	}
}

func TestVolumeFlagRejectsLeaseReuse(t *testing.T) {
	for _, tc := range []struct {
		name string
		flag string
	}{
		{name: "existing lease", flag: "id"},
		{name: "ready pool", flag: "pool"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			fs.String(tc.flag, "", "lease reuse")
			values := registerFlags(fs, core.Config{})
			if err := fs.Parse([]string{
				"--" + tc.flag, "cbx_existing",
				"--local-container-volume", "/host/a:/guest/a:ro",
			}); err != nil {
				t.Fatal(err)
			}
			cfg := core.Config{Provider: providerName}
			err := applyFlags(&cfg, fs, values)
			if err == nil || !strings.Contains(err.Error(), "only applies when creating a new lease") {
				t.Fatalf("err=%v, want lease reuse rejection", err)
			}
		})
	}
}
