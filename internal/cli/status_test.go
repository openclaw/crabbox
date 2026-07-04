package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStatusWaitDoneTreatsTerminalStatesAsDone(t *testing.T) {
	for _, state := range []string{"deleting", "expired", "failed", "missing", "released", "stopped", "stopped_with_code", "terminated"} {
		if !statusWaitDone(statusView{State: state}) {
			t.Fatalf("statusWaitDone(%q) = false, want true", state)
		}
	}
	if statusWaitDone(statusView{State: "provisioning"}) {
		t.Fatal("statusWaitDone(provisioning) = true, want false")
	}
	if !statusWaitDone(statusView{State: "provisioning", Ready: true}) {
		t.Fatal("statusWaitDone(ready provisioning) = false, want true")
	}
}

func TestStatusAllowsServiceControlProviderToResolveConfiguredID(t *testing.T) {
	t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	t.Setenv("CRABBOX_COORDINATOR", "")
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "")
	testServiceControlStatusHook = func(req StatusRequest) (StatusView, error) {
		if req.ID != "" {
			t.Fatalf("status ID = %q, want provider-resolved empty ID", req.ID)
		}
		return StatusView{
			ID:       "configured-app",
			Provider: "service-control-test",
			TargetOS: targetLinux,
			State:    "ready",
			Ready:    true,
		}, nil
	}
	defer func() { testServiceControlStatusHook = nil }()

	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: io.Discard}
	if err := app.status(context.Background(), []string{"--provider", "service-control-test"}); err != nil {
		t.Fatalf("status returned error: %v", err)
	}
	if !strings.Contains(stdout.String(), "configured-app") {
		t.Fatalf("status output = %q, want configured app", stdout.String())
	}
}

func TestStatusWaitTerminalErrorFailsNonReadyTerminalState(t *testing.T) {
	err := statusWaitTerminalError("cbx_123", statusView{State: "stopped"})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 5 {
		t.Fatalf("statusWaitTerminalError = %#v, want exit 5", err)
	}
	if err := statusWaitTerminalError("cbx_123", statusView{State: "stopped", Ready: true}); err != nil {
		t.Fatalf("ready terminal state returned error: %v", err)
	}
	if err := statusWaitTerminalError("cbx_123", statusView{State: "provisioning"}); err != nil {
		t.Fatalf("non-terminal state returned error: %v", err)
	}
}

func TestLeaseStatusStateCanBeReadyRejectsTerminalStates(t *testing.T) {
	for _, state := range []string{"deleting", "stopped", "released", "terminated"} {
		if leaseStatusStateCanBeReady(LeaseTarget{}, state) {
			t.Fatalf("leaseStatusStateCanBeReady(%q) = true, want false", state)
		}
	}
	if leaseStatusStateCanBeReady(LeaseTarget{}, "provisioning") {
		t.Fatal("leaseStatusStateCanBeReady(provisioning) = true, want false")
	}
	if !leaseStatusStateCanBeReady(LeaseTarget{}, "ready") {
		t.Fatal("leaseStatusStateCanBeReady(ready) = false, want true")
	}
}

func TestStatusWaitRequestsReadyProbe(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	t.Setenv("CRABBOX_COORDINATOR", "")
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "")
	backend := &statusResolveRecordingBackend{}
	testAWSBackendOverride = backend
	defer func() { testAWSBackendOverride = nil }()

	app := App{Stdout: io.Discard, Stderr: &bytes.Buffer{}}
	if err := app.status(context.Background(), []string{"--provider", "aws", "--id", "cbx_status"}); err != nil {
		t.Fatalf("status returned error: %v", err)
	}
	if len(backend.requests) != 1 {
		t.Fatalf("resolve calls=%d want 1", len(backend.requests))
	}
	if !backend.requests[0].StatusOnly {
		t.Fatal("plain status should use status-only resolve")
	}
	if !backend.requests[0].NoLocalStateMutations {
		t.Fatal("plain status should not mutate local claims")
	}
	if backend.requests[0].ReadyProbe {
		t.Fatal("plain status should not request a readiness probe")
	}

	backend.requests = nil
	err := app.status(context.Background(), []string{"--provider", "aws", "--id", "cbx_status", "--wait", "--wait-timeout", "1ns"})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 5 {
		t.Fatalf("status --wait error = %#v, want timeout exit 5", err)
	}
	if len(backend.requests) != 1 {
		t.Fatalf("resolve calls=%d want 1", len(backend.requests))
	}
	if !backend.requests[0].StatusOnly {
		t.Fatal("status --wait should still use status-only resolve")
	}
	if !backend.requests[0].NoLocalStateMutations {
		t.Fatal("status --wait should not mutate local claims")
	}
	if !backend.requests[0].ReadyProbe {
		t.Fatal("status --wait should request SSH readiness data")
	}
	if len(backend.touches) != 0 {
		t.Fatalf("claimless status --wait touch calls=%d want 0", len(backend.touches))
	}

	cfg := defaultConfig()
	cfg.Provider = "aws"
	claimServer := Server{
		Provider: "aws",
		CloudID:  "i-status",
		Labels: map[string]string{
			"lease":    "cbx_status",
			"slug":     "status",
			"provider": "aws",
		},
	}
	if err := claimLeaseTargetForRepoConfig("cbx_status", "status", cfg, claimServer, SSHTarget{}, "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	providerScope := leaseOptionsFromConfig(cfg).ProviderScope
	claimed, err := statusLeaseHasExactClaim(backend, LeaseTarget{LeaseID: "cbx_status", Server: claimServer}, "aws", providerScope)
	if err != nil || !claimed {
		t.Fatalf("matching exact claim allowed=%t err=%v", claimed, err)
	}
	rejecting := &statusTouchClaimRejectingBackend{statusResolveRecordingBackend: backend}
	if claimed, err := statusLeaseHasExactClaim(rejecting, LeaseTarget{LeaseID: "cbx_status", Server: claimServer}, "aws", providerScope); err != nil || claimed {
		t.Fatalf("provider identity-rejected claim allowed=%t err=%v", claimed, err)
	}
	if claimed, err := statusLeaseHasExactClaim(backend, LeaseTarget{LeaseID: "cbx_status", Server: claimServer}, "aws", providerScope+"-other"); err != nil || claimed {
		t.Fatalf("scope-mismatched claim allowed=%t err=%v", claimed, err)
	}
	wrongResource := claimServer
	wrongResource.CloudID = "i-other"
	if claimed, err := statusLeaseHasExactClaim(backend, LeaseTarget{LeaseID: "cbx_status", Server: wrongResource}, "aws", providerScope); err != nil || claimed {
		t.Fatalf("resource-mismatched claim allowed=%t err=%v", claimed, err)
	}
	err = app.status(context.Background(), []string{"--provider", "aws", "--id", "cbx_status", "--wait", "--wait-timeout", "1ns"})
	if !AsExitError(err, &exitErr) || exitErr.Code != 5 {
		t.Fatalf("claimed status --wait error = %#v, want timeout exit 5", err)
	}
	if len(backend.touches) != 1 {
		t.Fatalf("claimed status --wait touch calls=%d want 1", len(backend.touches))
	}
}

func TestStatusWaitBoundsResolveByTimeout(t *testing.T) {
	t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	t.Setenv("CRABBOX_COORDINATOR", "")
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "")
	backend := &statusResolveRecordingBackend{block: true}
	testAWSBackendOverride = backend
	defer func() { testAWSBackendOverride = nil }()

	app := App{Stdout: io.Discard, Stderr: &bytes.Buffer{}}
	start := time.Now()
	err := app.status(context.Background(), []string{"--provider", "aws", "--id", "cbx_status", "--wait", "--wait-timeout", "20ms"})
	elapsed := time.Since(start)
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 5 {
		t.Fatalf("status --wait error = %#v, want timeout exit 5", err)
	}
	if elapsed > time.Second {
		t.Fatalf("status --wait ignored timeout: elapsed=%s", elapsed)
	}
	if len(backend.requests) != 1 {
		t.Fatalf("resolve calls=%d want 1", len(backend.requests))
	}
}

func TestStatusWaitPreservesBackendTimeoutDetail(t *testing.T) {
	t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	t.Setenv("CRABBOX_COORDINATOR", "")
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "")
	backend := &statusResolveRecordingBackend{block: true, timeoutDetail: "auth denied"}
	testAWSBackendOverride = backend
	defer func() { testAWSBackendOverride = nil }()

	app := App{Stdout: io.Discard, Stderr: &bytes.Buffer{}}
	err := app.status(context.Background(), []string{"--provider", "aws", "--id", "cbx_status", "--wait", "--wait-timeout", "20ms"})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 5 || !strings.Contains(err.Error(), "auth denied") {
		t.Fatalf("status --wait error = %#v, want timeout exit 5 with backend detail", err)
	}
}

type statusResolveRecordingBackend struct {
	testSSHBackend
	requests      []ResolveRequest
	touches       []TouchRequest
	block         bool
	timeoutDetail string
}

type statusTouchClaimRejectingBackend struct {
	*statusResolveRecordingBackend
}

func (*statusTouchClaimRejectingBackend) StatusTouchClaimMatches(LeaseTarget, LeaseClaim) bool {
	return false
}

func (b *statusResolveRecordingBackend) Resolve(ctx context.Context, req ResolveRequest) (LeaseTarget, error) {
	b.requests = append(b.requests, req)
	if b.block {
		<-ctx.Done()
		if b.timeoutDetail != "" {
			return LeaseTarget{}, errors.Join(ctx.Err(), errors.New(b.timeoutDetail))
		}
		return LeaseTarget{}, ctx.Err()
	}
	return LeaseTarget{
		Server: Server{
			Provider: "aws",
			CloudID:  "i-status",
			Status:   "running",
			Labels: map[string]string{
				"lease":             "cbx_status",
				"slug":              "status",
				"provider":          "aws",
				"state":             "ready",
				"idle_timeout_secs": "60",
			},
		},
		LeaseID: "cbx_status",
	}, nil
}

func (b *statusResolveRecordingBackend) Touch(_ context.Context, req TouchRequest) (Server, error) {
	b.touches = append(b.touches, req)
	return req.Lease.Server, nil
}
