package islo

import (
	"context"
	"encoding/json"
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

// newIsloCreateCaptureServer serves the auth handshake plus a single sandbox
// create, recording the exact JSON body Crabbox put on the wire.
func newIsloCreateCaptureServer(t *testing.T, body *[]byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/auth/token":
			_ = json.NewEncoder(w).Encode(map[string]any{"session_token": "jwt-from-test", "cookie_max_age": 3600})
		case r.Method == http.MethodPost && r.URL.Path == "/sandboxes":
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("read create body: %v", err)
			}
			*body = raw
			var request struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(raw, &request); err != nil {
				t.Fatalf("decode create body: %v", err)
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":         "01930000-0000-7000-8000-000000000000",
				"name":       request.Name,
				"status":     "running",
				"image":      "docker.io/library/ubuntu:26.04",
				"created_at": "2026-08-31T00:00:00Z",
			})
		default:
			http.NotFound(w, r)
		}
	}))
}

// TestIsloCreateSandboxSendsMappedLeasePolicy pins the wire contract on both
// sides of the opt-in: with islo.idlePause unset the create request carries no
// lifecycle object at all, and with it set the idle timeout is the only lease
// input that reaches Islo, as pause_after_idle seconds, with Crabbox never
// asking the provider to delete or auto-resume a sandbox it holds a claim on.
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
			srv := newIsloCreateCaptureServer(t, &body)
			defer srv.Close()

			cfg := Config{
				IdleTimeout: test.idleTimeout,
				TTL:         test.ttl,
				Islo:        IsloConfig{APIKey: "ak_test", BaseURL: srv.URL, Workdir: "crabbox", IdlePause: test.idlePause},
			}
			client, err := newIsloClient(cfg, Runtime{HTTP: srv.Client()})
			if err != nil {
				t.Fatal(err)
			}
			backend := &isloBackend{cfg: cfg, rt: Runtime{Stderr: io.Discard}}
			if _, _, _, err := backend.createSandbox(context.Background(), client, Repo{Root: t.TempDir(), Name: "repo"}, false, ""); err != nil {
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
			// A TTL must never become a provider-side deletion deadline.
			if strings.Contains(string(body), "delete_after") {
				t.Fatalf("create body=%s must not hand deletion to the provider", body)
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
			sandbox := &gosdk.SandboxResponse{Name: "crabbox-repo-abcdef", Lifecycle: test.lifecycle}
			cfg := Config{IdleTimeout: test.idleTimeout, TTL: test.ttl, Islo: IsloConfig{IdlePause: test.idlePause}}
			err := isloLifecycleConflict(sandbox.GetName(), sandbox, cfg)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected conflict: %v", err)
				}
				return
			}
			var exitErr ExitError
			if !core.AsExitError(err, &exitErr) || exitErr.Code != 2 {
				t.Fatalf("err=%v want exit 2 conflict", err)
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("err=%v want %q", err, test.wantErr)
			}
		})
	}
}

// TestIsloReclaimRejectsDriftedIdlePause exercises the conflict through the real
// reclaim entry point: the drift must abort before a local claim is written.
func TestIsloReclaimRejectsDriftedIdlePause(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	seconds := int64(1800)
	client := &fakeIsloSyncClient{getSandbox: &gosdk.SandboxResponse{
		Name:      "crabbox-repo-abcdef",
		Status:    "running",
		Lifecycle: &gosdk.LifecyclePolicy{PauseAfterIdle: &seconds},
	}}
	backend := &isloBackend{
		cfg: Config{IdleTimeout: 10 * time.Minute, Islo: IsloConfig{APIKey: "test", Workdir: "repo", IdlePause: true}},
		rt:  Runtime{Stdout: io.Discard, Stderr: io.Discard},
	}
	_, _, _, err := backend.resolveLeaseIDForRepo(context.Background(), client, "crabbox-repo-abcdef", t.TempDir(), true)
	var exitErr ExitError
	if !core.AsExitError(err, &exitErr) || exitErr.Code != 2 || !strings.Contains(err.Error(), "pause_after_idle=1800") {
		t.Fatalf("err=%v want exit 2 lifecycle conflict", err)
	}
	if _, ok, claimErr := resolveLeaseClaim("isb_crabbox-repo-abcdef"); claimErr != nil || ok {
		t.Fatalf("claim written despite conflict: ok=%v err=%v", ok, claimErr)
	}
}

// TestIsloReclaimAcceptsMatchingIdlePause is the counterpart: a matching policy
// still claims the lease, so the conflict check cannot be blocking every reclaim.
func TestIsloReclaimAcceptsMatchingIdlePause(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	seconds := int64(1800)
	client := &fakeIsloSyncClient{getSandbox: &gosdk.SandboxResponse{
		Name:      "crabbox-repo-abcdef",
		Status:    "running",
		Lifecycle: &gosdk.LifecyclePolicy{PauseAfterIdle: &seconds},
	}}
	backend := &isloBackend{
		cfg: Config{IdleTimeout: 30 * time.Minute, Islo: IsloConfig{APIKey: "test", Workdir: "repo", IdlePause: true}},
		rt:  Runtime{Stdout: io.Discard, Stderr: io.Discard},
	}
	leaseID, name, _, err := backend.resolveLeaseIDForRepo(context.Background(), client, "crabbox-repo-abcdef", t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	if name != "crabbox-repo-abcdef" {
		t.Fatalf("sandbox=%q want crabbox-repo-abcdef", name)
	}
	if _, ok, claimErr := resolveLeaseClaim(leaseID); claimErr != nil || !ok {
		t.Fatalf("claim missing after matching reclaim: ok=%v err=%v", ok, claimErr)
	}
}

// TestIsloRunResumesPausedReusedLease covers the failure mode the idle-pause
// mapping creates: a reused lease Islo paused must be resumed before sync and
// exec, without relying on any Islo auto-resume behaviour.
func TestIsloRunResumesPausedReusedLease(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "isb_crabbox-repo-abcdef"
	if err := claimLeaseForRepoProvider(leaseID, "repo", isloProvider, t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	client := &fakeIsloSyncClient{getSandbox: &gosdk.SandboxResponse{Name: "crabbox-repo-abcdef", Status: "paused"}}
	restore := swapNewIsloClient(client)
	defer restore()
	backend := &isloBackend{
		cfg: Config{IdleTimeout: 30 * time.Minute, Islo: IsloConfig{APIKey: "test", Workdir: "repo"}},
		rt:  Runtime{Stdout: io.Discard, Stderr: io.Discard},
	}

	result, err := backend.Run(context.Background(), RunRequest{
		ID:      leaseID,
		Keep:    true,
		NoSync:  true,
		Command: []string{"true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if client.resumeCalls != 1 || client.resumedName != "crabbox-repo-abcdef" {
		t.Fatalf("resume calls=%d name=%q want one resume of the reused sandbox", client.resumeCalls, client.resumedName)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit=%d want 0", result.ExitCode)
	}
	ran := false
	for _, req := range client.execRequests {
		if strings.Join(req.GetCommand(), " ") == "true" {
			ran = true
		}
	}
	if !ran {
		t.Fatalf("workload never ran on the resumed sandbox: %#v", client.execRequests)
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

// TestIsloReclaimWithoutIdlePauseAdoptsDriftedPolicy keeps the adoption conflict
// coherent with an unset knob: a sandbox carrying some pause policy Crabbox did
// not ask for is still adoptable, because with the knob off Crabbox promises
// nothing about the provider-side policy.
func TestIsloReclaimWithoutIdlePauseAdoptsDriftedPolicy(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	seconds := int64(1800)
	client := &fakeIsloSyncClient{getSandbox: &gosdk.SandboxResponse{
		Name:      "crabbox-repo-abcdef",
		Status:    "running",
		Lifecycle: &gosdk.LifecyclePolicy{PauseAfterIdle: &seconds},
	}}
	backend := &isloBackend{
		cfg: Config{IdleTimeout: 10 * time.Minute, Islo: IsloConfig{APIKey: "test", Workdir: "repo"}},
		rt:  Runtime{Stdout: io.Discard, Stderr: io.Discard},
	}
	leaseID, name, _, err := backend.resolveLeaseIDForRepo(context.Background(), client, "crabbox-repo-abcdef", t.TempDir(), true)
	if err != nil {
		t.Fatalf("reclaim without the idle-pause knob must not conflict: %v", err)
	}
	if name != "crabbox-repo-abcdef" {
		t.Fatalf("sandbox=%q want crabbox-repo-abcdef", name)
	}
	if _, ok, claimErr := resolveLeaseClaim(leaseID); claimErr != nil || !ok {
		t.Fatalf("claim missing after reclaim: ok=%v err=%v", ok, claimErr)
	}
}
