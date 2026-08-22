package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"path/filepath"
	"reflect"
	"runtime"
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

func TestStatusViewIncludesProviderMetadata(t *testing.T) {
	cfg := defaultConfig()
	cfg.Provider = "aws"
	cfg.Network = NetworkPublic
	metadata := map[string]any{
		"instanceProfileAttached": false,
		"internal":                "must-not-leak",
	}
	view, err := statusViewFromLeaseTarget(context.Background(), cfg, LeaseTarget{
		LeaseID: "cbx_status",
		Server: Server{
			Provider:         "aws",
			Status:           "running",
			Labels:           map[string]string{},
			ProviderMetadata: metadata,
		},
	})
	if err != nil {
		t.Fatalf("statusViewFromLeaseTarget returned error: %v", err)
	}
	if got := view.ProviderMetadata["instanceProfileAttached"]; got != false {
		t.Fatalf("instanceProfileAttached=%v, want false", got)
	}
	if _, ok := view.ProviderMetadata["internal"]; ok {
		t.Fatal("providerMetadata unexpectedly exposed an internal field")
	}
	metadata["instanceProfileAttached"] = "false"
	view, err = statusViewFromLeaseTarget(context.Background(), cfg, LeaseTarget{
		LeaseID: "cbx_status_invalid",
		Server: Server{
			Provider:         "aws",
			Status:           "running",
			Labels:           map[string]string{},
			ProviderMetadata: metadata,
		},
	})
	if err != nil {
		t.Fatalf("statusViewFromLeaseTarget returned error: %v", err)
	}
	if view.ProviderMetadata != nil {
		t.Fatalf("providerMetadata=%v, want omitted invalid metadata", view.ProviderMetadata)
	}
}

func TestStatusViewUsesWindowsSSHReadinessProfile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fake ssh helper is only reliable on Unix hosts")
	}
	logPath := installSSHArgsRecorder(t)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	host, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	cfg := baseConfig()
	cfg.Provider = "aws"
	cfg.TargetOS = targetWindows
	cfg.WindowsMode = windowsModeNormal
	cfg.Network = NetworkPublic
	view, err := statusViewFromLeaseTarget(context.Background(), cfg, LeaseTarget{
		LeaseID: "cbx_status_windows",
		Server: Server{
			Provider: "aws",
			Status:   "active",
			Labels:   map[string]string{"target": targetWindows, "windows_mode": windowsModeNormal},
		},
		SSH: SSHTarget{
			User:        "crabbox",
			Host:        host,
			Port:        port,
			TargetOS:    targetWindows,
			WindowsMode: windowsModeNormal,
			NetworkKind: NetworkPublic,
			ReadyCheck:  "true",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !view.Ready {
		t.Fatal("status Ready=false, want true with reachable fake Windows SSH")
	}
	args := readSSHArgsRecorder(t, logPath)
	assertSSHOption(t, args, "ConnectTimeout", "10")
	assertSSHOption(t, args, "ConnectionAttempts", "3")
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
	beforePlainStatus, err := readLeaseClaim("cbx_status")
	if err != nil {
		t.Fatal(err)
	}
	if err := app.status(context.Background(), []string{"--provider", "aws", "--id", "cbx_status"}); err != nil {
		t.Fatal(err)
	}
	afterPlainStatus, err := readLeaseClaim("cbx_status")
	if err != nil || !reflect.DeepEqual(afterPlainStatus, beforePlainStatus) || len(backend.touches) != 0 {
		t.Fatalf("plain status mutated claim: before=%#v after=%#v touches=%#v err=%v", beforePlainStatus, afterPlainStatus, backend.touches, err)
	}
	providerScope := leaseOptionsFromConfig(cfg).ProviderScope
	claim, claimed, err := statusLeaseExactClaim(context.Background(), backend, LeaseTarget{LeaseID: "cbx_status", Server: claimServer}, "aws", providerScope)
	if err != nil || !claimed || claim.LeaseID != "cbx_status" || claim.Revision == "" {
		t.Fatalf("matching exact claim=%#v allowed=%t err=%v", claim, claimed, err)
	}
	rejecting := &statusTouchClaimRejectingBackend{statusResolveRecordingBackend: backend}
	if _, claimed, err := statusLeaseExactClaim(context.Background(), rejecting, LeaseTarget{LeaseID: "cbx_status", Server: claimServer}, "aws", providerScope); err != nil || claimed {
		t.Fatalf("provider identity-rejected claim allowed=%t err=%v", claimed, err)
	}
	if _, claimed, err := statusLeaseExactClaim(context.Background(), backend, LeaseTarget{LeaseID: "cbx_status", Server: claimServer}, "aws", providerScope+"-other"); err != nil || claimed {
		t.Fatalf("scope-mismatched claim allowed=%t err=%v", claimed, err)
	}
	wrongResource := claimServer
	wrongResource.CloudID = "i-other"
	if _, claimed, err := statusLeaseExactClaim(context.Background(), backend, LeaseTarget{LeaseID: "cbx_status", Server: wrongResource}, "aws", providerScope); err != nil || claimed {
		t.Fatalf("resource-mismatched claim allowed=%t err=%v", claimed, err)
	}
	err = app.status(context.Background(), []string{"--provider", "aws", "--id", "cbx_status", "--wait", "--wait-timeout", "1ns"})
	if !AsExitError(err, &exitErr) || exitErr.Code != 5 {
		t.Fatalf("claimed status --wait error = %#v, want timeout exit 5", err)
	}
	if len(backend.touches) != 1 {
		t.Fatalf("claimed status --wait touch calls=%d want 1", len(backend.touches))
	}
	snapshot, exists, set := ServerLeaseClaimSnapshot(backend.touches[0].Lease.Server)
	if !set || !exists || snapshot.Revision != claim.Revision || backend.touches[0].IdleTimeoutOverride != nil {
		t.Fatalf("status touch snapshot=%#v exists=%t set=%t override=%v", snapshot, exists, set, backend.touches[0].IdleTimeoutOverride)
	}
}

func TestStatusWaitClaimReplacementPreventsProviderMutation(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	t.Setenv("CRABBOX_COORDINATOR", "")
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "")
	backend := &statusResolveRecordingBackend{}
	testAWSBackendOverride = backend
	t.Cleanup(func() { testAWSBackendOverride = nil })
	cfg := defaultConfig()
	cfg.Provider = "aws"
	server := Server{Provider: "aws", CloudID: "i-status", Labels: map[string]string{
		"lease": "cbx_status", "slug": "status", "provider": "aws",
	}}
	if err := claimLeaseTargetForRepoConfig("cbx_status", "status", cfg, server, SSHTarget{}, "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	var replacement leaseClaim
	providerWrites := 0
	backend.touchFn = func(req TouchRequest) (Server, error) {
		snapshot, exists, set := ServerLeaseClaimSnapshot(req.Lease.Server)
		if !set || !exists {
			return Server{}, errors.New("exact claim snapshot missing")
		}
		labels := cloneStringMap(snapshot.Labels)
		labels["owner"] = "replacement-owner"
		var err error
		replacement, err = updateLeaseClaimLabelsIfUnchanged(req.Lease.LeaseID, snapshot, labels)
		if err != nil {
			return Server{}, err
		}
		_, updated, _, err := UpdateLeaseClaimTouchIfUnchangedAction(req.Lease.LeaseID, snapshot, time.Now(), req.IdleTimeoutOverride, func() (Server, SSHTarget, bool, error) {
			providerWrites++
			return req.Lease.Server, req.Lease.SSH, true, nil
		})
		return updated, err
	}
	var stderr bytes.Buffer
	err := (App{Stdout: io.Discard, Stderr: &stderr}).status(context.Background(), []string{
		"--provider", "aws", "--id", "cbx_status", "--wait", "--wait-timeout", "1ns",
	})
	var exitErr ExitError
	persisted, readErr := readLeaseClaim("cbx_status")
	if !AsExitError(err, &exitErr) || exitErr.Code != 5 ||
		!strings.Contains(stderr.String(), "claim changed") || providerWrites != 0 ||
		readErr != nil || !reflect.DeepEqual(persisted, replacement) {
		t.Fatalf("status error=%v stderr=%q provider writes=%d persisted=%#v replacement=%#v readErr=%v", err, stderr.String(), providerWrites, persisted, replacement, readErr)
	}
}

func TestStatusLeaseExactClaimAuthorizerOwnsDynamicScopeValidation(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_dynamic_status"
	server := Server{Provider: "aws", CloudID: "dynamic-resource", Labels: map[string]string{
		"lease": leaseID, "provider": "aws", "runtime_scope": "runtime:dynamic/context:owned",
	}}
	if err := claimLeaseForRepoProviderScopePondEndpoint(
		leaseID,
		"dynamic-status",
		"aws",
		"runtime:dynamic/context:owned",
		"",
		t.TempDir(),
		time.Minute,
		false,
		server,
		SSHTarget{},
	); err != nil {
		t.Fatal(err)
	}

	ctx := context.WithValue(context.Background(), statusAuthorizerContextKey{}, "present")
	backend := &statusTouchClaimAuthorizingBackend{}
	claim, claimed, err := statusLeaseExactClaim(ctx, backend, LeaseTarget{LeaseID: leaseID, Server: server}, "aws", "")
	if err != nil || !claimed || claim.Revision == "" {
		t.Fatalf("dynamic claim=%#v authorized=%t err=%v", claim, claimed, err)
	}
	if backend.calls != 1 || backend.contextValue != "present" || backend.claim.ProviderScope != "runtime:dynamic/context:owned" {
		t.Fatalf("authorizer calls=%d context=%q claim=%#v", backend.calls, backend.contextValue, backend.claim)
	}

	backend.err = errors.New("dynamic ownership mismatch")
	_, claimed, err = statusLeaseExactClaim(ctx, backend, LeaseTarget{LeaseID: leaseID, Server: server}, "aws", "")
	if claimed || err == nil || !strings.Contains(err.Error(), "dynamic ownership mismatch") {
		t.Fatalf("rejected dynamic claim authorized=%t err=%v", claimed, err)
	}

	backend.calls = 0
	wrongProvider := server
	wrongProvider.Provider = "gcp"
	if _, claimed, err := statusLeaseExactClaim(ctx, backend, LeaseTarget{LeaseID: leaseID, Server: wrongProvider}, "aws", ""); err != nil || claimed {
		t.Fatalf("wrong-provider claim authorized=%t err=%v", claimed, err)
	}
	if backend.calls != 0 {
		t.Fatalf("authorizer called before exact canonical-provider claim check: %d", backend.calls)
	}
	if _, claimed, err := statusLeaseExactClaim(ctx, backend, LeaseTarget{LeaseID: "cbx_missing_dynamic", Server: server}, "aws", ""); err != nil || claimed {
		t.Fatalf("missing claim authorized=%t err=%v", claimed, err)
	}
	if backend.calls != 0 {
		t.Fatalf("authorizer called for missing claim: %d", backend.calls)
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
	touchFn       func(TouchRequest) (Server, error)
	block         bool
	timeoutDetail string
}

type statusTouchClaimRejectingBackend struct {
	*statusResolveRecordingBackend
}

func (*statusTouchClaimRejectingBackend) StatusTouchClaimMatches(LeaseTarget, LeaseClaim) bool {
	return false
}

type statusAuthorizerContextKey struct{}

type statusTouchClaimAuthorizingBackend struct {
	testSSHBackend
	calls        int
	contextValue string
	claim        LeaseClaim
	err          error
}

func (b *statusTouchClaimAuthorizingBackend) AuthorizeStatusTouchClaim(ctx context.Context, _ LeaseTarget, claim LeaseClaim) error {
	b.calls++
	b.contextValue, _ = ctx.Value(statusAuthorizerContextKey{}).(string)
	b.claim = claim
	return b.err
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
	if b.touchFn != nil {
		return b.touchFn(req)
	}
	return req.Lease.Server, nil
}
