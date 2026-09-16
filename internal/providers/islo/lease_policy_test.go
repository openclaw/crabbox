package islo

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	gosdk "github.com/islo-labs/go-sdk"
	core "github.com/openclaw/crabbox/internal/cli"
)

func TestIsloCreateSandboxSendsMappedLeasePolicy(t *testing.T) {
	tests := []struct {
		name        string
		idlePause   bool
		idleTimeout time.Duration
		ttl         time.Duration
		want        map[string]any
	}{
		{
			// The default path: the 30m --idle-timeout default must not become a
			// provider-enforced pause for an operator who never opted in.
			name:        "default idle timeout sends no lifecycle without the knob",
			idleTimeout: 30 * time.Minute,
			ttl:         90 * time.Minute,
			want:        nil,
		},
		{
			name:        "opted in idle timeout maps to pause_after_idle",
			idlePause:   true,
			idleTimeout: 30 * time.Minute,
			ttl:         90 * time.Minute,
			want:        map[string]any{"auto_resume": "never", "pause_after_idle": float64(1800)},
		},
		{
			name:        "opted in sub-second idle timeout rounds up",
			idlePause:   true,
			idleTimeout: 1500 * time.Millisecond,
			ttl:         2500 * time.Millisecond,
			want:        map[string]any{"auto_resume": "never", "pause_after_idle": float64(2)},
		},
		{
			name:        "maximum positive idle timeout rounds without overflow",
			idlePause:   true,
			idleTimeout: time.Duration(1<<63 - 1),
			want:        map[string]any{"auto_resume": "never", "pause_after_idle": float64(9223372037)},
		},
		{
			name:      "opted in with no idle timeout sends no policy at all",
			idlePause: true,
			ttl:       90 * time.Minute,
			want:      nil,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			var body []byte
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/auth/token" {
					io.WriteString(w, `{"session_token":"synthetic-token"}`)
					return
				}
				if r.Method != http.MethodPost || r.URL.Path != "/sandboxes" {
					http.NotFound(w, r)
					return
				}
				var err error
				body, err = io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				w.WriteHeader(http.StatusCreated)
				json.NewEncoder(w).Encode(map[string]string{
					"id": isloTestResourceID, "name": "crabbox-repo-abcdef", "status": "running",
				})
			}))
			defer srv.Close()

			cfg := core.Config{
				IdleTimeout: test.idleTimeout,
				TTL:         test.ttl,
				Islo:        core.IsloConfig{APIKey: "ak_test", BaseURL: srv.URL, Workdir: "crabbox", IdlePause: test.idlePause},
			}
			client, err := newIsloClient(cfg, core.Runtime{HTTP: srv.Client()})
			if err != nil {
				t.Fatal(err)
			}
			backend := &isloBackend{cfg: cfg, rt: core.Runtime{Stderr: io.Discard}}
			if _, _, _, _, err := backend.createSandbox(context.Background(), client, core.Repo{Root: t.TempDir(), Name: "repo"}, false, ""); err != nil {
				t.Fatal(err)
			}

			var payload map[string]any
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatalf("create body %q: %v", body, err)
			}
			// The generated name is random; everything else is the exact request.
			if name, _ := payload["name"].(string); !strings.HasPrefix(name, isloNamePrefix) {
				t.Fatalf("create name=%q want %s prefix", name, isloNamePrefix)
			}
			delete(payload, "name")
			want := map[string]any{}
			if test.want != nil {
				want["lifecycle"] = test.want
			}
			if !reflect.DeepEqual(payload, want) {
				t.Fatalf("create body=%s want %v and nothing else", body, want)
			}
		})
	}
}

func TestIsloLeasePolicyConflictOnReclaim(t *testing.T) {
	seconds := func(value int64) *int64 { return &value }
	tests := []struct {
		name        string
		idlePause   bool
		idleTimeout time.Duration
		ttl         time.Duration
		lifecycle   *gosdk.LifecyclePolicy
		wantErr     string
	}{
		{
			// Without the knob Crabbox promises nothing about the provider-side
			// policy, so drift cannot be a conflict.
			name:        "drifted idle pause is reusable without the knob",
			idleTimeout: 10 * time.Minute,
			lifecycle:   &gosdk.LifecyclePolicy{PauseAfterIdle: seconds(1800)},
		},
		{
			name:      "unwanted idle pause is reusable without the knob",
			lifecycle: &gosdk.LifecyclePolicy{PauseAfterIdle: seconds(1800)},
		},
		{
			name:        "matching idle pause is reusable",
			idlePause:   true,
			idleTimeout: 30 * time.Minute,
			ttl:         90 * time.Minute,
			lifecycle:   &gosdk.LifecyclePolicy{PauseAfterIdle: seconds(1800)},
		},
		{
			name:        "unknown lifecycle is reusable",
			idlePause:   true,
			idleTimeout: 30 * time.Minute,
			lifecycle:   nil,
		},
		{
			name:        "ttl drift alone is not a conflict",
			idlePause:   true,
			idleTimeout: 30 * time.Minute,
			ttl:         6 * time.Hour,
			lifecycle:   &gosdk.LifecyclePolicy{PauseAfterIdle: seconds(1800), DeleteAfter: seconds(5400)},
		},
		{
			name:        "auto_resume drift alone is not a conflict",
			idlePause:   true,
			idleTimeout: 30 * time.Minute,
			lifecycle:   &gosdk.LifecyclePolicy{PauseAfterIdle: seconds(1800), AutoResume: gosdk.AutoResumePolicyOnActivity.Ptr()},
		},
		{
			name:        "idle timeout drift conflicts",
			idlePause:   true,
			idleTimeout: 10 * time.Minute,
			lifecycle:   &gosdk.LifecyclePolicy{PauseAfterIdle: seconds(1800)},
			wantErr:     "pause_after_idle=1800 but this run asks for 600",
		},
		{
			name:        "missing idle pause conflicts",
			idlePause:   true,
			idleTimeout: 30 * time.Minute,
			lifecycle:   &gosdk.LifecyclePolicy{DeleteAfter: seconds(5400)},
			wantErr:     "pause_after_idle=unset but this run asks for 1800",
		},
		{
			name:      "unwanted idle pause conflicts when opted in with no timeout",
			idlePause: true,
			lifecycle: &gosdk.LifecyclePolicy{PauseAfterIdle: seconds(1800)},
			wantErr:   "pause_after_idle=1800 but this run asks for unset",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			const name = "crabbox-repo-abcdef"
			const leaseID = "isb_" + name
			client := &fakeIsloSyncClient{getSandbox: &gosdk.SandboxResponse{
				ID: isloTestResourceID, Name: name, Status: "running", Lifecycle: test.lifecycle,
			}}
			backend := &isloBackend{
				cfg: core.Config{IdleTimeout: test.idleTimeout, TTL: test.ttl, Islo: core.IsloConfig{IdlePause: test.idlePause}},
				rt:  core.Runtime{Stdout: io.Discard, Stderr: io.Discard},
			}
			gotID, gotName, _, err := backend.resolveLeaseIDForRepo(context.Background(), client, name, t.TempDir(), true)
			if test.wantErr == "" {
				if err != nil || gotID != leaseID || gotName != name {
					t.Fatalf("reclaim id=%q name=%q err=%v", gotID, gotName, err)
				}
			} else {
				var exitErr core.ExitError
				if !core.AsExitError(err, &exitErr) || exitErr.Code != 2 || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("err=%v want exit 2 containing %q", err, test.wantErr)
				}
			}
			if _, claimed, claimErr := core.ResolveLeaseClaim(leaseID); claimErr != nil || claimed != (test.wantErr == "") {
				t.Fatalf("claim published=%v err=%v; want publication only on successful reclaim", claimed, claimErr)
			}
		})
	}
}

// TestIsloRunResumesPausedReusedLease covers the failure mode the idle-pause
// mapping creates: a reused lease Islo paused must be resumed before sync and
// exec, without relying on any Islo auto-resume behaviour.
func TestIsloRunResumesPausedReusedLease(t *testing.T) {
	for _, failResume := range []bool{false, true} {
		name := "ready"
		if failResume {
			name = "resume failure retains session"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			leaseID := "isb_crabbox-repo-abcdef"
			repo := core.Repo{Root: t.TempDir(), Name: "repo"}
			if err := core.ClaimLeaseForRepoProvider(leaseID, "repo", isloProvider, repo.Root, time.Minute, false); err != nil {
				t.Fatal(err)
			}
			client := &fakeIsloSyncClient{getSandbox: &gosdk.SandboxResponse{Name: "crabbox-repo-abcdef", Status: "paused"}}
			cause := errors.New("fixture resume unavailable")
			if failResume {
				client.resumeErr = cause
			}
			restore := swapNewIsloClient(client)
			defer restore()
			backend := &isloBackend{
				cfg: core.Config{IdleTimeout: 30 * time.Minute, Islo: core.IsloConfig{APIKey: "test", Workdir: "repo"}},
				rt:  core.Runtime{Stdout: io.Discard, Stderr: io.Discard},
			}
			result, err := backend.Run(context.Background(), core.RunRequest{
				ID: leaseID, Repo: repo, Keep: true, NoSync: true, Command: []string{"true"},
			})
			if client.resumeCalls != 1 || client.resumedName != "crabbox-repo-abcdef" {
				t.Fatalf("resume calls=%d name=%q", client.resumeCalls, client.resumedName)
			}
			if failResume {
				if !errors.Is(err, cause) || result.Session == nil || !result.Session.Reused || !result.Session.Kept || result.Session.LeaseID != leaseID {
					t.Fatalf("resume failure lost its bound recovery session: result=%+v err=%v", result, err)
				}
				if len(client.execRequests) != 0 {
					t.Fatal("execution started after failed resume")
				}
				if _, ok, claimErr := core.ResolveLeaseClaim(leaseID); claimErr != nil || !ok || client.deleteCalls != 0 {
					t.Fatalf("failed resume lost the retained lease: claim=%v err=%v deletes=%d", ok, claimErr, client.deleteCalls)
				}
				return
			}
			if err != nil || result.ExitCode != 0 {
				t.Fatalf("exit=%d err=%v", result.ExitCode, err)
			}
			ran := false
			for _, req := range client.execRequests {
				if strings.Join(req.GetCommand(), " ") == "true" {
					ran = true
				}
			}
			if !ran {
				t.Fatal("workload never ran on the resumed sandbox")
			}
		})
	}
}

// TestIsloIdlePauseIsOptIn pins the knob itself: the shipped defaults produce no
// lifecycle policy even with the 30m default idle timeout, --islo-idle-pause
// turns it on, and --islo-idle-pause=false turns a config-file opt-in back off.
func TestIsloIdlePauseIsOptIn(t *testing.T) {
	base := core.BaseConfig()
	if base.IdleTimeout <= 0 {
		t.Fatalf("base idle timeout=%s want the positive shipped default", base.IdleTimeout)
	}
	if base.Islo.IdlePause {
		t.Fatal("islo idle pause must ship off")
	}
	if policy := isloLifecycleForConfig(base); policy != nil {
		t.Fatalf("shipped defaults produced lifecycle %#v want none", policy)
	}

	on := core.BaseConfig()
	fs := flag.NewFlagSet("islo", flag.ContinueOnError)
	values := RegisterIsloProviderFlags(fs, on)
	if err := fs.Parse([]string{"--islo-idle-pause"}); err != nil {
		t.Fatal(err)
	}
	if err := ApplyIsloProviderFlags(&on, fs, values); err != nil {
		t.Fatal(err)
	}
	if !on.Islo.IdlePause {
		t.Fatal("--islo-idle-pause did not opt in")
	}
	policy := isloLifecycleForConfig(on)
	if policy == nil || policy.PauseAfterIdle == nil || *policy.PauseAfterIdle != int64(on.IdleTimeout/time.Second) {
		t.Fatalf("opted-in lifecycle=%#v want pause_after_idle=%d", policy, int64(on.IdleTimeout/time.Second))
	}

	off := core.BaseConfig()
	off.Islo.IdlePause = true
	fsOff := flag.NewFlagSet("islo", flag.ContinueOnError)
	valuesOff := RegisterIsloProviderFlags(fsOff, off)
	if err := fsOff.Parse([]string{"--islo-idle-pause=false"}); err != nil {
		t.Fatal(err)
	}
	if err := ApplyIsloProviderFlags(&off, fsOff, valuesOff); err != nil {
		t.Fatal(err)
	}
	if off.Islo.IdlePause {
		t.Fatal("--islo-idle-pause=false did not opt back out")
	}
	if policy := isloLifecycleForConfig(off); policy != nil {
		t.Fatalf("opted-out lifecycle=%#v want none", policy)
	}
}
