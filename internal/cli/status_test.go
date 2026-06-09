package cli

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"testing"
	"time"
)

func TestStatusWaitDoneTreatsTerminalStatesAsDone(t *testing.T) {
	for _, state := range []string{"expired", "failed", "released", "stopped", "stopped_with_code", "terminated"} {
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
	for _, state := range []string{"stopped", "released", "terminated"} {
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
	if !backend.requests[0].ReadyProbe {
		t.Fatal("status --wait should request SSH readiness data")
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

type statusResolveRecordingBackend struct {
	testSSHBackend
	requests []ResolveRequest
	block    bool
}

func (b *statusResolveRecordingBackend) Resolve(ctx context.Context, req ResolveRequest) (LeaseTarget, error) {
	b.requests = append(b.requests, req)
	if b.block {
		<-ctx.Done()
		return LeaseTarget{}, ctx.Err()
	}
	return LeaseTarget{
		Server: Server{
			Provider: "aws",
			Status:   "running",
			Labels: map[string]string{
				"state":             "ready",
				"idle_timeout_secs": "60",
			},
		},
		LeaseID: "cbx_status",
	}, nil
}
