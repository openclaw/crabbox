package islo

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	gosdk "github.com/islo-labs/go-sdk"
	core "github.com/openclaw/crabbox/internal/cli"
)

const heartbeatSandboxName = "crabbox-repo-abcdef"

func TestIsloProviderDeclaresHeartbeat(t *testing.T) {
	if !(Provider{}).Spec().Features.Has(core.FeatureLeaseHeartbeat) {
		t.Fatal("islo provider must declare lease-heartbeat")
	}
	backend := NewIsloBackend((Provider{}).Spec(), Config{}, Runtime{})
	if _, ok := backend.(core.LeaseHeartbeatBackend); !ok {
		t.Fatalf("backend=%T does not implement the heartbeat capability", backend)
	}
}

func TestIsloHeartbeatExecsCheapNoOp(t *testing.T) {
	client, backend, stdout, stderr := newIsloHeartbeatTest(t, "running")
	// Exec noise on both streams: if the implementation stopped discarding
	// either stream, it would land in the caller's terminal below.
	client.execOut = "stdout noise\n"
	client.execErrOut = "stderr noise\n"
	client.getSandbox.Lifecycle = &gosdk.LifecyclePolicy{PauseAfterIdle: int64Value(900)}
	// Records the exec's context deadline so the client-side bound is provable.
	client.execDeadlineCommand = "true"

	result, err := backend.Heartbeat(context.Background(), core.LeaseHeartbeatRequest{ID: heartbeatSandboxName})
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if len(client.execRequests) != 1 {
		t.Fatalf("exec requests=%#v, want exactly one heartbeat exec", client.prepareCommands)
	}
	exec := client.execRequests[0]
	if !reflect.DeepEqual(exec.GetCommand(), []string{"true"}) {
		t.Fatalf("heartbeat command=%#v, want the cheapest no-op", exec.GetCommand())
	}
	// The exec must target the sandbox NAME. A lease id or slug would 404 on
	// POST /sandboxes/{name}/exec/stream.
	if !reflect.DeepEqual(client.execNames, []string{heartbeatSandboxName}) {
		t.Fatalf("heartbeat exec targets=%#v, want the sandbox name %q", client.execNames, heartbeatSandboxName)
	}
	// No filesystem mutation: the heartbeat exec carries no workdir, no env,
	// and runs as the unprivileged workload user.
	if exec.Workdir != nil || len(exec.Env) != 0 || exec.User == nil || *exec.User != isloWorkloadUser {
		t.Fatalf("heartbeat exec=%#v", exec)
	}
	if exec.TimeoutSecs == nil || *exec.TimeoutSecs != isloHeartbeatTimeoutSecs {
		t.Fatalf("heartbeat exec timeout=%#v, want a bounded server-side hint", exec.TimeoutSecs)
	}
	// The server-side hint does not bound a hung stream, so the client call
	// carries its own deadline; a heartbeat loop must not be able to wedge.
	if client.execDeadline.IsZero() {
		t.Fatal("heartbeat exec ran without a client-side deadline")
	}
	// This code path issues no create and no lifecycle write of any kind - one
	// GET plus one exec is the whole contract - so it cannot move any deadline
	// in either direction. Pinned by the observed calls, not by any claim about
	// how Islo treats activity.
	if client.createRequest != nil || client.pausedName != "" || client.resumedName != "" || client.deleteCalls != 0 {
		t.Fatalf("heartbeat mutated lifecycle: create=%#v paused=%q resumed=%q deletes=%d",
			client.createRequest, client.pausedName, client.resumedName, client.deleteCalls)
	}
	// Success is silent even though the exec produced output on both streams.
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("heartbeat leaked exec output: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	// The reported idle window is the sandbox's echoed
	// lifecycle.pause_after_idle, never a Crabbox-side default.
	if result.LeaseID != isloLeasePrefix+heartbeatSandboxName || result.State != "running" ||
		result.IdleTimeout != 15*time.Minute || result.LastTouchedAt.IsZero() {
		t.Fatalf("heartbeat result=%#v", result)
	}

	// Safe to replay: a second heartbeat is another no-op exec, nothing else.
	if _, err := backend.Heartbeat(context.Background(), core.LeaseHeartbeatRequest{ID: heartbeatSandboxName}); err != nil {
		t.Fatalf("replayed heartbeat: %v", err)
	}
	if len(client.execRequests) != 2 || client.createRequest != nil || client.deleteCalls != 0 {
		t.Fatalf("replayed heartbeat side effects: execs=%d create=%#v deletes=%d",
			len(client.execRequests), client.createRequest, client.deleteCalls)
	}
}

// TestIsloHeartbeatReportsOnlyTheSandboxIdlePolicy pins the honest-reporting
// rule: the idle window comes from the live sandbox or is not reported at all.
// Islo's activity semantics are undocumented, so a heartbeat that cannot name a
// real idle deadline says so instead of printing a number.
func TestIsloHeartbeatReportsOnlyTheSandboxIdlePolicy(t *testing.T) {
	tests := []struct {
		name            string
		lifecycle       *gosdk.LifecyclePolicy
		wantIdleTimeout time.Duration
		wantWarning     bool
	}{
		{
			name:            "echoed pause_after_idle is reported verbatim",
			lifecycle:       &gosdk.LifecyclePolicy{PauseAfterIdle: int64Value(120)},
			wantIdleTimeout: 2 * time.Minute,
		},
		{
			name:            "no lifecycle at all reports nothing and warns",
			lifecycle:       nil,
			wantIdleTimeout: 0,
			wantWarning:     true,
		},
		{
			name:            "a lifecycle without an idle policy reports nothing and warns",
			lifecycle:       &gosdk.LifecyclePolicy{DeleteAfter: int64Value(3600)},
			wantIdleTimeout: 0,
			wantWarning:     true,
		},
		{
			name:            "a null idle policy reports nothing and warns",
			lifecycle:       &gosdk.LifecyclePolicy{PauseAfterIdle: int64Value(0)},
			wantIdleTimeout: 0,
			wantWarning:     true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client, backend, stdout, stderr := newIsloHeartbeatTest(t, "running")
			client.getSandbox.Lifecycle = test.lifecycle

			result, err := backend.Heartbeat(context.Background(), core.LeaseHeartbeatRequest{ID: heartbeatSandboxName})
			if err != nil {
				t.Fatalf("heartbeat: %v", err)
			}
			if result.IdleTimeout != test.wantIdleTimeout {
				t.Fatalf("idle timeout=%s, want %s", result.IdleTimeout, test.wantIdleTimeout)
			}
			if stdout.Len() != 0 {
				t.Fatalf("heartbeat wrote stdout=%q", stdout.String())
			}
			warned := strings.Contains(stderr.String(), "no lifecycle.pause_after_idle")
			if warned != test.wantWarning {
				t.Fatalf("warned=%t want %t; stderr=%q", warned, test.wantWarning, stderr.String())
			}
			// Warning or not, the heartbeat still ran its no-op exec.
			if len(client.execRequests) != 1 {
				t.Fatalf("exec requests=%d, want 1", len(client.execRequests))
			}
		})
	}
}

func TestIsloHeartbeatRejectsUnusableSandbox(t *testing.T) {
	tests := []struct {
		name     string
		status   string
		wantCode int
		wantErr  string
		wantHint bool
	}{
		// An exec against a paused sandbox resumes it and the resume is
		// billed, so a heartbeat must refuse instead.
		{name: "paused", status: "paused", wantCode: 5, wantErr: "is not running (state=paused)", wantHint: true},
		// Nothing to resume while it is still coming up: do not advise it.
		{name: "starting", status: "starting", wantCode: 5, wantErr: "is not running (state=starting)"},
		{name: "unknown state", status: "", wantCode: 5, wantErr: "is not running (state=unknown)"},
		{name: "terminal", status: "failed", wantCode: 5, wantErr: "terminal state=failed"},
		{name: "stopping", status: "stopping", wantCode: 5, wantErr: "terminal state=stopping"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client, backend, _, _ := newIsloHeartbeatTest(t, test.status)
			_, err := backend.Heartbeat(context.Background(), core.LeaseHeartbeatRequest{ID: heartbeatSandboxName})
			assertIsloHeartbeatExit(t, err, test.wantCode, test.wantErr)
			hinted := strings.Contains(err.Error(), "resume it before heartbeat")
			if hinted != test.wantHint {
				t.Fatalf("resume hint=%t want %t in %q", hinted, test.wantHint, err.Error())
			}
			if len(client.execRequests) != 0 {
				t.Fatalf("state=%s heartbeat still exec'd: %#v", test.status, client.prepareCommands)
			}
		})
	}
}

func TestIsloHeartbeatSurfacesExecFailures(t *testing.T) {
	t.Run("non-zero exit", func(t *testing.T) {
		client, backend, _, _ := newIsloHeartbeatTest(t, "running")
		client.execCode = 3
		_, err := backend.Heartbeat(context.Background(), core.LeaseHeartbeatRequest{ID: heartbeatSandboxName})
		assertIsloHeartbeatExit(t, err, 5, "heartbeat exec on sandbox "+heartbeatSandboxName+" exited 3")
	})

	t.Run("transport error", func(t *testing.T) {
		client, backend, _, _ := newIsloHeartbeatTest(t, "running")
		client.execErr = errors.New("boom")
		_, err := backend.Heartbeat(context.Background(), core.LeaseHeartbeatRequest{ID: heartbeatSandboxName})
		if err == nil || !strings.Contains(err.Error(), "heartbeat exec") || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("error=%v, want the exec transport failure surfaced", err)
		}
	})
}

func TestIsloHeartbeatRejectsMissingSandbox(t *testing.T) {
	t.Run("gone", func(t *testing.T) {
		client, backend, _, _ := newIsloHeartbeatTest(t, "running")
		client.getSandboxGone = true
		_, err := backend.Heartbeat(context.Background(), core.LeaseHeartbeatRequest{ID: heartbeatSandboxName})
		assertIsloHeartbeatExit(t, err, 4, "islo sandbox "+heartbeatSandboxName+" not found")
		if len(client.execRequests) != 0 {
			t.Fatalf("heartbeat exec'd a missing sandbox: %#v", client.prepareCommands)
		}
	})

	t.Run("lookup error", func(t *testing.T) {
		client, backend, _, _ := newIsloHeartbeatTest(t, "running")
		client.getSandboxErr = errors.New("upstream down")
		_, err := backend.Heartbeat(context.Background(), core.LeaseHeartbeatRequest{ID: heartbeatSandboxName})
		if err == nil || !strings.Contains(err.Error(), "get sandbox") || !strings.Contains(err.Error(), "upstream down") {
			t.Fatalf("error=%v, want the lookup failure surfaced", err)
		}
		if len(client.execRequests) != 0 {
			t.Fatalf("heartbeat exec'd after a failed lookup: %#v", client.prepareCommands)
		}
	})
}

func TestIsloHeartbeatRequiresExactClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	client := &fakeIsloSyncClient{}
	defer swapNewIsloClient(client)()
	backend := &isloBackend{
		cfg: Config{Islo: IsloConfig{APIKey: "test"}},
		rt:  Runtime{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}},
	}
	_, err := backend.Heartbeat(context.Background(), core.LeaseHeartbeatRequest{ID: heartbeatSandboxName})
	assertIsloHeartbeatExit(t, err, 4, "has no exact local claim")
	// A non-Crabbox sandbox id must be rejected before any provider call.
	_, err = backend.Heartbeat(context.Background(), core.LeaseHeartbeatRequest{ID: "production"})
	assertIsloHeartbeatExit(t, err, 4, "is not claimed by Crabbox")
	if len(client.execRequests) != 0 {
		t.Fatalf("claimless heartbeat reached the provider: %#v", client.prepareCommands)
	}
}

func assertIsloHeartbeatExit(t *testing.T, err error, code int, want string) {
	t.Helper()
	var exitErr ExitError
	if !core.AsExitError(err, &exitErr) {
		t.Fatalf("error=%v, want an ExitError containing %q", err, want)
	}
	if exitErr.Code != code || !strings.Contains(exitErr.Message, want) {
		t.Fatalf("error=exit %d %q, want exit %d containing %q", exitErr.Code, exitErr.Message, code, want)
	}
}

func int64Value(v int64) *int64 { return &v }

func newIsloHeartbeatTest(t *testing.T, status string) (*fakeIsloSyncClient, *isloBackend, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := claimLeaseForRepoProvider(isloLeasePrefix+heartbeatSandboxName, "web", isloProvider, t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}
	client := &fakeIsloSyncClient{getSandbox: &gosdk.SandboxResponse{Name: heartbeatSandboxName, Status: status}}
	restore := swapNewIsloClient(client)
	t.Cleanup(restore)
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	backend := &isloBackend{
		cfg: Config{Islo: IsloConfig{APIKey: "test"}},
		rt:  Runtime{Stdout: stdout, Stderr: stderr},
	}
	return client, backend, stdout, stderr
}
