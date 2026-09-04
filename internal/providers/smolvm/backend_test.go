package smolvm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func TestProviderSpecAndAliases(t *testing.T) {
	p := Provider{}
	if p.Name() != providerName {
		t.Fatalf("Name=%q want %s", p.Name(), providerName)
	}
	for _, alias := range []string{"smol", "smolmachines", "smolfleet"} {
		got, err := core.ProviderFor(alias)
		if err != nil {
			t.Fatalf("ProviderFor(%q): %v", alias, err)
		}
		if got.Name() != providerName {
			t.Fatalf("ProviderFor(%q).Name=%q", alias, got.Name())
		}
	}
	spec := p.Spec()
	if spec.Kind != core.ProviderKindDelegatedRun {
		t.Fatalf("kind=%v want delegated-run", spec.Kind)
	}
	if spec.Coordinator != core.CoordinatorNever {
		t.Fatalf("coordinator=%v want never", spec.Coordinator)
	}
	if len(spec.Targets) != 1 || spec.Targets[0].OS != core.TargetLinux {
		t.Fatalf("targets=%#v want linux", spec.Targets)
	}
	if !hasFeature(spec.Features, core.FeatureArchiveSync) {
		t.Fatalf("features=%#v want archive sync", spec.Features)
	}
	if !hasFeature(spec.Features, core.FeatureRunSession) {
		t.Fatalf("features=%#v want run session", spec.Features)
	}
}

func TestSmolVMClientRedactsReflectedCredential(t *testing.T) {
	const secret = "smolvm-secret-token"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"message":"Bearer `+secret+` quota exceeded"}`)
	}))
	defer server.Close()

	client := &client{apiKey: secret, base: server.URL, http: server.Client()}
	_, err := client.GetMachine(context.Background(), "machine_1")
	if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "[redacted]") || !strings.Contains(err.Error(), "quota exceeded") {
		t.Fatalf("GetMachine error=%v, want redacted useful provider error", err)
	}
}

func TestClientUsesSmolvmRESTShape(t *testing.T) {
	var createBody map[string]any
	injectSeen := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") || strings.TrimPrefix(auth, "Bearer ") != "smk_key" {
			t.Fatalf("Authorization=%q want Bearer smk_key", auth)
		}
		switch r.URL.Path {
		case "/v1/machines":
			switch r.Method {
			case http.MethodPost:
				if err := json.NewDecoder(r.Body).Decode(&createBody); err != nil {
					t.Fatal(err)
				}
				_ = json.NewEncoder(w).Encode(testMachineResponse("mach_1", "crabbox-blue-123456789abc", "running"))
			case http.MethodGet:
				_ = json.NewEncoder(w).Encode([]map[string]any{testMachineResponse("mach_1", "crabbox-blue-123456789abc", "running")})
			default:
				t.Fatalf("unexpected method %s", r.Method)
			}
		case "/v1/machines/mach_1":
			if r.Method == http.MethodGet {
				_ = json.NewEncoder(w).Encode(testMachineResponse("mach_1", "crabbox-blue-123456789abc", "running"))
				return
			}
			if r.Method == http.MethodDelete {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			t.Fatalf("unexpected method %s on /v1/machines/mach_1", r.Method)
		case "/v1/machines/mach_1/exec":
			var body struct {
				Command string `json:"command"`
				CWD     string `json:"cwd"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			// Direct heredoc-based archive inject.
			if strings.Contains(body.Command, "tar -xzf") {
				injectSeen = true
				_ = json.NewEncoder(w).Encode(map[string]any{"exitCode": 0, "stdout": "smolvm-direct-archive-extract: ok\n"})
				return
			}
			// Direct write (env profile etc) also uses base64 heredoc /exec.
			if strings.Contains(body.Command, "mv -f") {
				_ = json.NewEncoder(w).Encode(map[string]any{"exitCode": 0, "stdout": "smolvm-direct-write: ok\n"})
				return
			}
			if !strings.Contains(body.Command, "echo hi") || (body.CWD != "crabbox" && body.CWD != "/crabbox") {
				t.Fatalf("exec body=%#v", body)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"exitCode": 0, "stdout": "hi\n", "stderr": "warn\n"})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	client := &client{apiKey: "smk_key", base: srv.URL, http: srv.Client()}
	mach, err := client.CreateMachine(context.Background(), createRequest{Name: "crabbox-blue-123456789abc", Source: smolvmMachineSource{Type: "image", Reference: "ubuntu:24.04"}})
	if err != nil {
		t.Fatal(err)
	}
	if mach.ID != "mach_1" || createBody["name"] != "crabbox-blue-123456789abc" {
		t.Fatalf("create mach=%#v body=%v", mach, createBody)
	}
	if _, err := client.ListMachines(context.Background()); err != nil {
		t.Fatal(err)
	}
	if result, err := client.Exec(context.Background(), "mach_1", "echo hi", "crabbox"); err != nil || result.Output != "hi\n" {
		t.Fatalf("exec result=%#v err=%v", result, err)
	}
	var stdout bytes.Buffer
	code, err := client.ExecStream(context.Background(), "mach_1", "echo hi", "crabbox", &stdout)
	if err != nil || code != 0 || stdout.String() != "hi\nwarn\n" {
		t.Fatalf("stream code=%d stdout=%q err=%v", code, stdout.String(), err)
	}
	archive := filepath.Join(t.TempDir(), "archive.tgz")
	if err := os.WriteFile(archive, []byte("archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := client.InjectArchive(context.Background(), "mach_1", archive, "crabbox"); err != nil {
		t.Fatal(err)
	}
	if !injectSeen {
		t.Fatal("inject archive not seen")
	}
	if err := client.WriteFile(context.Background(), "mach_1", "/tmp/env.sh", "export A=1\n"); err != nil {
		t.Fatal(err)
	}
	if err := client.DeleteMachine(context.Background(), "mach_1"); err != nil {
		t.Fatal(err)
	}
}

func TestNewAPIRejectsBareHTTPBaseURL(t *testing.T) {
	cfg := Config{}
	cfg.Smolvm.APIKey = "smk_key"
	cfg.Smolvm.BaseURL = "http://api.smolmachines.com"
	if _, err := newAPI(cfg, Runtime{}); err == nil {
		t.Fatal("newAPI accepted plaintext http URL")
	}
}

func TestNewAPIRejectsUserinfoQueryAndFragment(t *testing.T) {
	for _, base := range []string{
		"https://user:pass@api.smolmachines.com",
		"https://api.smolmachines.com?key=value",
		"https://api.smolmachines.com#fragment",
	} {
		cfg := Config{}
		cfg.Smolvm.APIKey = "smk_key"
		cfg.Smolvm.BaseURL = base
		if _, err := newAPI(cfg, Runtime{}); err == nil {
			t.Fatalf("newAPI accepted %q", base)
		}
	}
}

func TestNewAPIAllowsLoopbackHTTPBaseURL(t *testing.T) {
	cfg := Config{}
	cfg.Smolvm.APIKey = "smk_key"
	cfg.Smolvm.BaseURL = "http://127.0.0.1:8080"
	if _, err := newAPI(cfg, Runtime{}); err != nil {
		t.Fatalf("loopback http rejected: %v", err)
	}
}

func TestNewAPIRejectsUntrustedHTTPSBaseURLByDefault(t *testing.T) {
	cfg := Config{}
	cfg.Smolvm.APIKey = "smk_key"
	cfg.Smolvm.BaseURL = "https://smolvm.attacker.example"
	if _, err := newAPI(cfg, Runtime{}); err == nil || !strings.Contains(err.Error(), "ALLOW_CUSTOM_BASE_URL") {
		t.Fatalf("newAPI error=%v, want custom endpoint opt-in requirement", err)
	}
}

func TestNewAPIAllowsExplicitCustomHTTPSBaseURL(t *testing.T) {
	t.Setenv("CRABBOX_SMOLVM_ALLOW_CUSTOM_BASE_URL", "1")
	cfg := Config{}
	cfg.Smolvm.APIKey = "smk_key"
	cfg.Smolvm.BaseURL = "https://smolvm.example.test"
	if _, err := newAPI(cfg, Runtime{}); err != nil {
		t.Fatalf("explicit custom endpoint rejected: %v", err)
	}
}

func TestNewAPINormalizesBaseURL(t *testing.T) {
	cfg := Config{}
	cfg.Smolvm.APIKey = "smk_key"
	cfg.Smolvm.BaseURL = " https://eu.smolmachines.com/base/ "
	apiClient, err := newAPI(cfg, Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	c, ok := apiClient.(*client)
	if !ok {
		t.Fatalf("api client=%T, want *client", apiClient)
	}
	if c.http == nil || c.dataHTTP == nil || c.http == c.dataHTTP {
		t.Fatalf("fallback clients=control:%p data:%p, want separate clients", c.http, c.dataHTTP)
	}
	if c.http.Timeout != smolvmControlTimeout || c.dataHTTP.Timeout != 0 {
		t.Fatalf("timeouts=control:%s data:%s", c.http.Timeout, c.dataHTTP.Timeout)
	}
	if c.base != "https://eu.smolmachines.com/base" {
		t.Fatalf("base = %q, want normalized base URL", c.base)
	}
}

func TestSmolVMFallbackBoundsControlAndPreservesCommand(t *testing.T) {
	const controlTimeout = 30 * time.Millisecond
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/machines/mach_1":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"id":`)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case "/v1/machines/mach_1/exec":
			time.Sleep(3 * controlTimeout)
			_ = json.NewEncoder(w).Encode(map[string]any{"stdout": "ok", "exitCode": 0})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	control, data := smolvmHTTPClients(nil, controlTimeout)
	trusted, _ := url.Parse(server.URL)
	client := &client{
		apiKey:   "smk_key",
		base:     server.URL,
		http:     shared.SecureHTTPClient(control, trusted, smolvmRedirectError),
		dataHTTP: shared.SecureHTTPClient(data, trusted, smolvmRedirectError),
	}
	started := time.Now()
	_, err := client.GetMachine(context.Background(), "mach_1")
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("GetMachine error=%v, want whole-request deadline", err)
	}
	controlElapsed := time.Since(started)
	if controlElapsed >= time.Second {
		t.Fatalf("stalled control response bounded after %s, want under 1s", controlElapsed)
	}

	started = time.Now()
	result, err := client.Exec(context.Background(), "mach_1", "true", "/workspace")
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("Exec result=%#v err=%v", result, err)
	}
	dataElapsed := time.Since(started)
	if dataElapsed <= controlTimeout {
		t.Fatalf("command completed in %s, want beyond %s", dataElapsed, controlTimeout)
	}
	t.Logf("SmolVM control body bounded in %s; synchronous command completed in %s beyond %s control deadline", controlElapsed.Round(time.Millisecond), dataElapsed.Round(time.Millisecond), controlTimeout)
}

func TestSmolVMInjectedHTTPSettingsArePreservedForBothPlanes(t *testing.T) {
	transport := &http.Transport{DisableKeepAlives: true}
	injected := &http.Client{Transport: transport, Timeout: 17 * time.Second}
	cfg := Config{}
	cfg.Smolvm.APIKey = "smk_key"
	cfg.Smolvm.BaseURL = "http://127.0.0.1:8787"
	api, err := newAPI(cfg, Runtime{HTTP: injected})
	if err != nil {
		t.Fatal(err)
	}
	client := api.(*client)
	if client.http.Transport != transport || client.dataHTTP.Transport != transport || client.http.Timeout != injected.Timeout || client.dataHTTP.Timeout != injected.Timeout {
		t.Fatalf("settings=control:(%T,%s) data:(%T,%s)", client.http.Transport, client.http.Timeout, client.dataHTTP.Transport, client.dataHTTP.Timeout)
	}
	if injected.CheckRedirect != nil {
		t.Fatal("constructor mutated injected redirect policy")
	}
}

func TestSmolvmClientRefusesCrossOriginRedirectBeforeReplay(t *testing.T) {
	var targetRequests int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetRequests++
		t.Errorf("redirect target received %s %s auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}))
	defer target.Close()

	trusted := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/stolen", http.StatusTemporaryRedirect)
	}))
	defer trusted.Close()

	cfg := Config{Smolvm: SmolvmConfig{APIKey: "smk_key", BaseURL: trusted.URL}}
	apiClient, err := newAPI(cfg, Runtime{HTTP: trusted.Client()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = apiClient.ListMachines(context.Background())
	if err == nil || !strings.Contains(err.Error(), "refused cross-origin redirect") {
		t.Fatalf("ListMachines error = %v, want cross-origin refusal", err)
	}
	if targetRequests != 0 {
		t.Fatalf("redirect target received %d requests, want 0", targetRequests)
	}
}

func TestSmolvmClientFollowsSameOriginRedirect(t *testing.T) {
	var redirectedAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/machines":
			http.Redirect(w, r, "/redirected", http.StatusTemporaryRedirect)
		case "/redirected":
			redirectedAuth = r.Header.Get("Authorization")
			_ = json.NewEncoder(w).Encode([]machineData{})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := Config{Smolvm: SmolvmConfig{APIKey: "smk_key", BaseURL: server.URL}}
	apiClient, err := newAPI(cfg, Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := apiClient.ListMachines(context.Background()); err != nil {
		t.Fatal(err)
	}
	if redirectedAuth != "Bearer smk_key" {
		t.Fatalf("redirected auth = %q", redirectedAuth)
	}
}

func TestSmolvmClientPreservesCallerRedirectPolicy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/redirected", http.StatusFound)
	}))
	defer server.Close()

	callerErr := errors.New("caller refused redirect")
	callerChecks := 0
	httpClient := server.Client()
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		callerChecks++
		return callerErr
	}
	apiClient, err := newAPI(
		Config{Smolvm: SmolvmConfig{APIKey: "smk_key", BaseURL: server.URL}},
		Runtime{HTTP: httpClient},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = apiClient.ListMachines(context.Background())
	if !errors.Is(err, callerErr) || callerChecks != 1 {
		t.Fatalf("ListMachines error = %v, caller checks = %d", err, callerChecks)
	}
}

func TestSameSmolvmOrigin(t *testing.T) {
	base, err := url.Parse("https://api.smolmachines.com/v1")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		url  string
		want bool
	}{
		{name: "same origin", url: "https://api.smolmachines.com/redirected", want: true},
		{name: "explicit default port", url: "https://api.smolmachines.com:443/redirected", want: true},
		{name: "scheme downgrade", url: "http://api.smolmachines.com/redirected", want: false},
		{name: "different port", url: "https://api.smolmachines.com:8443/redirected", want: false},
		{name: "subdomain", url: "https://sub.api.smolmachines.com/redirected", want: false},
		{name: "different host", url: "https://attacker.example/redirected", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			redirected, err := url.Parse(tc.url)
			if err != nil {
				t.Fatal(err)
			}
			if got := shared.SameOrigin(base, redirected); got != tc.want {
				t.Fatalf("shared.SameOrigin(%q) = %v, want %v", tc.url, got, tc.want)
			}
		})
	}
}

func TestExecExitCodePropagatesCamelAndSnake(t *testing.T) {
	cases := []struct {
		name string
		resp map[string]any
		want int
	}{
		{"camelCase nonzero", map[string]any{"exitCode": 7, "stdout": "x"}, 7},
		{"snake_case nonzero", map[string]any{"exit_code": 9, "stdout": "x"}, 9},
		{"camelCase zero stays zero", map[string]any{"exitCode": 0, "stdout": "ok"}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/machines/mach_1/exec" {
					t.Fatalf("unexpected path %s", r.URL.Path)
				}
				_ = json.NewEncoder(w).Encode(tc.resp)
			}))
			defer srv.Close()
			c := &client{apiKey: "smk_key", base: srv.URL, http: srv.Client()}
			res, err := c.Exec(context.Background(), "mach_1", "false", "/workspace")
			if err != nil {
				t.Fatal(err)
			}
			if res.ExitCode != tc.want {
				t.Fatalf("Exec ExitCode=%d want %d", res.ExitCode, tc.want)
			}
			code, err := c.ExecStream(context.Background(), "mach_1", "false", "/workspace", &bytes.Buffer{})
			if err != nil || code != tc.want {
				t.Fatalf("ExecStream code=%d want %d err=%v", code, tc.want, err)
			}
		})
	}
}

func TestCleanWorkdirAndCommand(t *testing.T) {
	if got, err := cleanWorkdir(" /workspace "); err != nil || got != "/workspace" {
		t.Fatalf("workdir=%q err=%v", got, err)
	}
	if got, err := workspaceFolder("/workspace/repo"); err != nil || got != "/workspace/repo" {
		t.Fatalf("workspaceFolder=%q err=%v", got, err)
	}
	for _, value := range []string{"", "repo", "/", "/tmp", "/workspace/../etc"} {
		if _, err := cleanWorkdir(value); err == nil {
			t.Fatalf("cleanWorkdir(%q) succeeded unexpectedly", value)
		}
	}

}

func TestWarmupRejectsActionsRunner(t *testing.T) {
	backend := NewBackend(Provider{}.Spec(), testConfig(), testRuntime()).(*backend)
	err := backend.Warmup(context.Background(), WarmupRequest{ActionsRunner: true})
	if err == nil || !strings.Contains(err.Error(), "--actions-runner is not supported") {
		t.Fatalf("err=%v, want actions-runner rejection", err)
	}
}

func TestRunCreatesExecsAndDeletesOneShotMachine(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := &fakeAPI{}
	withFakeAPI(t, fake)
	backend := NewBackend(Provider{}.Spec(), testConfig(), testRuntime()).(*backend)
	result, err := backend.Run(context.Background(), RunRequest{
		Repo:    Repo{Name: "repo", Root: t.TempDir()},
		Command: []string{"echo", "hello"},
		NoSync:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || result.Provider != providerName {
		t.Fatalf("result=%#v", result)
	}
	if result.Session == nil {
		t.Fatal("result.Session is nil")
	}
	if result.Session.Provider != providerName || result.Session.LeaseID != result.LeaseID || result.Session.Slug != result.Slug || result.Session.Reused || result.Session.Kept {
		t.Fatalf("session=%#v result=%#v", result.Session, result)
	}
	if result.Session.CleanupCommand != "crabbox stop --provider smolvm --id "+shellQuote(result.LeaseID) {
		t.Fatalf("cleanup command=%q", result.Session.CleanupCommand)
	}
	if fake.createReq.Name == "" || !strings.HasPrefix(fake.createReq.Name, "crabbox-") {
		t.Fatalf("create req=%#v", fake.createReq)
	}
	if !reflect.DeepEqual(fake.verbs, []string{"create", "exec", "stream", "delete"}) {
		t.Fatalf("verbs=%v", fake.verbs)
	}
	if !fake.createReq.Ephemeral || fake.createReq.TTLSeconds == 0 {
		t.Fatalf("create req should be ephemeral for one-shot run: %#v", fake.createReq)
	}
	if !strings.Contains(fake.execCommands[0], "mkdir -p") || strings.Contains(fake.execCommands[0], "rm -rf") {
		t.Fatalf("prepare command=%q", fake.execCommands[0])
	}
	if fake.streamFolders[0] == "" || !strings.Contains(fake.streamCommands[0], "echo") {
		t.Fatalf("stream folder=%q command=%q", fake.streamFolders[0], fake.streamCommands[0])
	}
}

func TestRunHonorsProviderKeepConfig(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := &fakeAPI{}
	withFakeAPI(t, fake)
	cfg := testConfig()
	cfg.Smolvm.Keep = true
	backend := NewBackend(Provider{}.Spec(), cfg, testRuntime()).(*backend)
	result, err := backend.Run(context.Background(), RunRequest{
		Repo:    Repo{Name: "repo", Root: t.TempDir()},
		Command: []string{"echo", "hello"},
		NoSync:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Session == nil || result.Session.Reused || !result.Session.Kept {
		t.Fatalf("session=%#v, want retained new machine", result.Session)
	}
	if fake.createReq.Ephemeral || fake.createReq.TTLSeconds != 0 {
		t.Fatalf("provider keep should create retained machine: %#v", fake.createReq)
	}
	if reflect.DeepEqual(fake.verbs, []string{"create", "exec", "stream", "delete"}) || fake.deletedID != "" {
		t.Fatalf("provider keep should not delete machine: verbs=%v deleted=%q", fake.verbs, fake.deletedID)
	}
}

func TestRunReturnsReusedMachineSession(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repo := t.TempDir()
	leaseID := "cbx_123456789abc"
	seedSmolvmClaim(t, leaseID, "blue", "mach_1", repo, nil)
	fake := &fakeAPI{machine: machineData{ID: "mach_1", Name: "crabbox-blue-123456789abc", State: "running", CreatedAt: "2026-08-30T00:00:00Z"}}
	withFakeAPI(t, fake)
	backend := NewBackend(Provider{}.Spec(), testConfig(), testRuntime()).(*backend)
	result, err := backend.Run(context.Background(), RunRequest{
		ID:      leaseID,
		Repo:    Repo{Name: "repo", Root: repo},
		Command: []string{"echo", "hello"},
		NoSync:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Session == nil || result.Session.LeaseID != leaseID || result.Session.Slug != "blue" || !result.Session.Reused || !result.Session.Kept {
		t.Fatalf("session=%#v", result.Session)
	}
	if fake.deletedID != "" {
		t.Fatalf("reused machine should not be deleted, deleted=%q", fake.deletedID)
	}
}

func TestRunPreservesSessionAfterDeleteFailure(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := &fakeAPI{deleteErr: errors.New("delete denied")}
	withFakeAPI(t, fake)
	var stderr bytes.Buffer
	rt := testRuntime()
	rt.Stderr = &stderr
	backend := NewBackend(Provider{}.Spec(), testConfig(), rt).(*backend)
	result, err := backend.Run(context.Background(), RunRequest{
		Repo:    Repo{Name: "repo", Root: t.TempDir()},
		Command: []string{"echo", "hello"},
		NoSync:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Session == nil || !result.Session.Kept || result.Session.CleanupCommand == "" {
		t.Fatalf("session=%#v, want retained cleanup handle", result.Session)
	}
	if !strings.Contains(stderr.String(), "smolvm delete failed") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestWarmupCreatesRetainedMachine(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := &fakeAPI{}
	withFakeAPI(t, fake)
	backend := NewBackend(Provider{}.Spec(), testConfig(), testRuntime()).(*backend)
	if err := backend.Warmup(context.Background(), WarmupRequest{Repo: Repo{Name: "repo", Root: t.TempDir()}}); err != nil {
		t.Fatal(err)
	}
	if fake.createReq.Ephemeral || fake.createReq.TTLSeconds != 0 {
		t.Fatalf("warmup should create retained machine: %#v", fake.createReq)
	}
}

func TestRunKeepsWorkspaceSubdirectoryConsistent(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := &fakeAPI{}
	withFakeAPI(t, fake)
	cfg := testConfig()
	cfg.Smolvm.Workdir = "/workspace/repo"
	backend := NewBackend(Provider{}.Spec(), cfg, testRuntime()).(*backend)
	if _, err := backend.Run(context.Background(), RunRequest{
		Repo:    Repo{Name: "repo", Root: newGitRepo(t)},
		Command: []string{"pwd"},
		Env:     map[string]string{"A": "1"},
	}); err != nil {
		t.Fatal(err)
	}
	if len(fake.execCommands) == 0 || !strings.Contains(fake.execCommands[0], "mkdir -p '/workspace/repo'") {
		t.Fatalf("prepare command=%q", fake.execCommands)
	}
	if len(fake.injectTargets) != 1 || fake.injectTargets[0] != "/workspace/repo" {
		t.Fatalf("inject targets=%v", fake.injectTargets)
	}
	if len(fake.writePaths) != 1 || !strings.HasPrefix(fake.writePaths[0], "/workspace/repo/.crabbox-env-") {
		t.Fatalf("write paths=%v", fake.writePaths)
	}
	if len(fake.streamFolders) != 1 || fake.streamFolders[0] != "/workspace/repo" {
		t.Fatalf("stream folders=%v", fake.streamFolders)
	}
}

func TestSyncWorkspaceUsesInject(t *testing.T) {
	fake := &fakeAPI{}
	backend := NewBackend(Provider{}.Spec(), testConfig(), testRuntime()).(*backend)
	_, _, err := backend.syncWorkspace(context.Background(), fake, "mach_1", RunRequest{
		Repo: Repo{Name: "repo", Root: newGitRepo(t)},
	}, "/workspace", ".")
	if err != nil {
		t.Fatalf("sync err=%v", err)
	}
	if !reflect.DeepEqual(fake.verbs, []string{"exec", "inject"}) {
		t.Fatalf("verbs=%v", fake.verbs)
	}
}

func TestStatusMapsMachineName(t *testing.T) {
	fake := &fakeAPI{machine: machineData{
		ID: "mach_1", Name: "crabbox-blue-123456789abc", State: "running", CreatedAt: "2026-08-30T00:00:00Z",
		Source:    smolvmMachineSource{Type: "image", Reference: "alpine"},
		Resources: smolvmMachineResources{CPUs: 4, MemoryMB: 8192},
	}}
	withFakeAPI(t, fake)
	cfg := testConfig()
	cfg.Smolvm.BaseURL = "https://eu.smolmachines.com"
	backend := NewBackend(Provider{}.Spec(), cfg, testRuntime()).(*backend)
	view, err := backend.Status(context.Background(), StatusRequest{ID: "mach_1"})
	if err != nil {
		t.Fatal(err)
	}
	if view.ID != "cbx_123456789abc" || view.Slug != "blue" || view.ServerID != "mach_1" || !view.Ready {
		t.Fatalf("view=%#v", view)
	}
	if view.Labels["image"] != "alpine" {
		t.Fatalf("labels=%v", view.Labels)
	}
	server := machineToServer(cfg, fake.machine)
	if server.PublicNet.IPv4.IP != "eu.smolmachines.com" {
		t.Fatalf("host=%q", server.PublicNet.IPv4.IP)
	}
}

func TestStatusRejectsNonCrabboxRawMachine(t *testing.T) {
	fake := &fakeAPI{machine: machineData{ID: "mach_1", Name: "external-machine", State: "running"}}
	withFakeAPI(t, fake)
	backend := NewBackend(Provider{}.Spec(), testConfig(), testRuntime()).(*backend)
	if _, err := backend.Status(context.Background(), StatusRequest{ID: "mach_1"}); err == nil {
		t.Fatal("expected non-Crabbox raw machine id to be rejected")
	}
}

func TestRunRawMachineIDEnforcesRepositoryClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_123456789abc"
	repoA := t.TempDir()
	repoB := t.TempDir()
	seedSmolvmClaim(t, leaseID, "blue", "mach_1", repoA, nil)
	fake := &fakeAPI{machine: machineData{ID: "mach_1", Name: "crabbox-blue-123456789abc", State: "running", CreatedAt: "2026-08-30T00:00:00Z"}}
	withFakeAPI(t, fake)
	backend := NewBackend(Provider{}.Spec(), testConfig(), testRuntime()).(*backend)
	_, err := backend.Run(context.Background(), RunRequest{
		ID:      "mach_1",
		Repo:    Repo{Name: "repo-b", Root: repoB},
		Command: []string{"echo", "hello"},
		NoSync:  true,
	})
	if err == nil || !strings.Contains(err.Error(), "is claimed by repo") {
		t.Fatalf("Run error=%v, want cross-repository claim rejection", err)
	}
	if len(fake.verbs) != 0 {
		t.Fatalf("verbs=%v, want no machine mutation or execution", fake.verbs)
	}
}

func hasFeature(features core.FeatureSet, want core.Feature) bool {
	for _, feature := range features {
		if feature == want {
			return true
		}
	}
	return false
}

func testConfig() Config {
	return Config{
		Provider: providerName,
		Smolvm: SmolvmConfig{
			APIKey:   "smk_key",
			BaseURL:  "https://api.smolmachines.com",
			Image:    "ubuntu:24.04",
			Workdir:  "/workspace",
			CPUs:     2,
			MemoryMB: 4096,
			Network:  "blocked",
		},
	}
}

func testRuntime() Runtime {
	return Runtime{Stdout: io.Discard, Stderr: io.Discard}
}

func withFakeAPI(t *testing.T, fake *fakeAPI) {
	t.Helper()
	original := newAPI
	newAPI = func(Config, Runtime) (api, error) { return fake, nil }
	t.Cleanup(func() { newAPI = original })
}

type fakeAPI struct {
	verbs          []string
	createReq      createRequest
	machine        machineData
	execCommands   []string
	execFolders    []string
	streamCommands []string
	streamFolders  []string
	injectTargets  []string
	writePaths     []string
	writeContents  []string
	execResults    []execResult
	deletedID      string
	deleteErr      error
	injected       bool
	deleted        bool
	createHook     func(*fakeAPI)
	startHook      func(context.Context, string) error
	getHook        func(context.Context, string) (machineData, error)
	deleteHook     func(context.Context, string) error
	streamHook     func()
	writeHook      func(context.Context, string, string) error
	execHook       func(context.Context, string) (execResult, error)
}

func (f *fakeAPI) CreateMachine(_ context.Context, req createRequest) (machineData, error) {
	f.verbs = append(f.verbs, "create")
	f.createReq = req
	f.machine = machineData{ID: "mach_1", Name: req.Name, State: "running", CreatedAt: "2026-08-30T00:00:00Z"}
	if ref := strings.TrimSpace(req.Source.Reference); ref != "" {
		f.machine.Source = req.Source
	} else if req.Source.Type != "" {
		f.machine.Source.Reference = req.Source.Type
	}
	f.machine.Resources = req.Resources
	if f.createHook != nil {
		f.createHook(f)
	}
	return f.machine, nil
}

func (f *fakeAPI) GetMachine(ctx context.Context, id string) (machineData, error) {
	if f.getHook != nil {
		return f.getHook(ctx, id)
	}
	if f.deleted {
		return machineData{}, &smolvmAPIError{StatusCode: 404}
	}
	if f.machine.ID == "" {
		f.machine = machineData{ID: "mach_1", Name: "crabbox-blue-123456789abc", State: "running", CreatedAt: "2026-08-30T00:00:00Z"}
	}
	return f.machine, nil
}

func (f *fakeAPI) ListMachines(context.Context) ([]machineData, error) {
	if f.machine.ID == "" {
		f.machine = machineData{ID: "mach_1", Name: "crabbox-blue-123456789abc", State: "running", CreatedAt: "2026-08-30T00:00:00Z"}
	}
	return []machineData{f.machine}, nil
}

func (f *fakeAPI) DeleteMachine(ctx context.Context, id string) error {
	f.verbs = append(f.verbs, "delete")
	f.deletedID = id
	if f.deleteHook != nil {
		return f.deleteHook(ctx, id)
	}
	if f.deleteErr == nil {
		f.deleted = true
	}
	return f.deleteErr
}

func (f *fakeAPI) StartMachine(ctx context.Context, id string) error {
	if f.startHook != nil {
		return f.startHook(ctx, id)
	}
	return nil
}
func (f *fakeAPI) StopMachine(context.Context, string) error { return nil }

func (f *fakeAPI) Exec(ctx context.Context, _ string, command, folder string) (execResult, error) {
	f.verbs = append(f.verbs, "exec")
	f.execCommands = append(f.execCommands, command)
	f.execFolders = append(f.execFolders, folder)
	if f.execHook != nil {
		return f.execHook(ctx, command)
	}
	if len(f.execResults) == 0 {
		return execResult{ExitCode: 0}, nil
	}
	result := f.execResults[0]
	f.execResults = f.execResults[1:]
	return result, nil
}

func (f *fakeAPI) ExecStream(_ context.Context, _ string, command, folder string, stdout io.Writer) (int, error) {
	f.verbs = append(f.verbs, "stream")
	if f.streamHook != nil {
		f.streamHook()
	}
	f.streamCommands = append(f.streamCommands, command)
	f.streamFolders = append(f.streamFolders, folder)
	_, _ = io.WriteString(stdout, "ok\n")
	return 0, nil
}

func (f *fakeAPI) InjectArchive(_ context.Context, _, _, targetDir string) error {
	f.verbs = append(f.verbs, "inject")
	f.injectTargets = append(f.injectTargets, targetDir)
	f.injected = true
	return nil
}

func (f *fakeAPI) WriteFile(ctx context.Context, _ string, remotePath, content string) error {
	f.verbs = append(f.verbs, "write")
	f.writePaths = append(f.writePaths, remotePath)
	f.writeContents = append(f.writeContents, content)
	if f.writeHook != nil {
		return f.writeHook(ctx, remotePath, content)
	}
	return nil
}

func newGitRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	runGit(t, root, "init")
	runGit(t, root, "config", "user.email", "alice@example.com")
	runGit(t, root, "config", "user.name", "Alice")
	if err := os.WriteFile(filepath.Join(root, "hello.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", "hello.txt")
	runGit(t, root, "commit", "-m", "initial")
	return root
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}

func testMachineResponse(id, name, state string) map[string]any {
	return map[string]any{
		"id": id, "name": name, "state": state,
		"source":    map[string]any{"type": "image", "reference": "ubuntu:24.04"},
		"resources": map[string]any{"cpus": 2, "memoryMb": 4096},
		"network":   map[string]any{"mode": "blocked"},
		"ephemeral": false, "createdAt": "2026-06-12T20:00:00Z", "updatedAt": "2026-06-12T20:00:00Z",
	}
}

// Exercise the actual HTTP client and generated shell; only the hosted exec
// service is replaced with a local, credential-free subprocess fixture.
func TestClientNativeUploadFailureAndPublication(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("native upload fixture requires POSIX shell tools")
	}
	for _, tool := range []string{"sh", "base64", "tar", "mktemp"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s unavailable", tool)
		}
	}
	decoder, _ := exec.LookPath("base64")
	if runtime.GOOS == "darwin" {
		var err error
		decoder, err = exec.LookPath("gbase64")
		if err != nil {
			t.Skip("GNU base64 required to reproduce the Linux decoder contract")
		}
	}
	for _, tc := range []struct {
		name, operation, failure string
		wantError                bool
	}{
		{"write", "write", "", false},
		{"write-partial-decoder", "write", "decode", true},
		{"write-directory", "write", "directory", true},
		{"write-cleanup-failure", "write", "cleanup", true},
		{"write-decode-and-cleanup-failure", "write", "decode-cleanup", true},
		{"archive", "archive", "", false},
		{"archive-corrupt", "archive", "corrupt", true},
		{"archive-decoder-late-failure", "archive", "decode-after-output", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			workspace := filepath.Join(root, "work space'quoted")
			if err := os.Mkdir(workspace, 0o700); err != nil {
				t.Fatal(err)
			}
			destination := filepath.Join(workspace, "env.sh")
			if tc.failure == "directory" {
				if err := os.Mkdir(destination, 0o700); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(destination, []byte("old"), 0o600); err != nil {
				t.Fatal(err)
			}
			const content = "literal quote'\n$(must-not-execute)\x00bytes"
			scriptPrefix := "base64() { " + shellQuote(decoder) + " \"$@\"; };\n"
			if tc.failure == "decode" || tc.failure == "decode-cleanup" {
				scriptPrefix = "base64() { printf partial; return 23; };\n"
			} else if tc.failure == "decode-after-output" {
				scriptPrefix = "base64() { " + shellQuote(decoder) + " \"$@\"; return 23; };\n"
			}
			if strings.Contains(tc.failure, "cleanup") {
				scriptPrefix += "rm() { return 17; };\n"
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/v1/machines/fixture/exec" {
					http.Error(w, "unexpected request", 400)
					return
				}
				var request struct {
					Command string `json:"command"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					http.Error(w, err.Error(), 400)
					return
				}
				// Isolate the old fixed staging name when demonstrating the regression.
				script := strings.ReplaceAll(request.Command, "/tmp/crabbox-sync.tgz", shellQuote(filepath.Join(root, "baseline-sync.tgz")))
				script = strings.ReplaceAll(script, "/tmp/crabbox-write-", filepath.Join(root, "baseline-write-"))
				cmd := exec.Command("sh", "-c", "umask 022\n"+scriptPrefix+script)
				cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + root, "TMPDIR=" + root}
				out, err := cmd.CombinedOutput()
				code := 0
				if err != nil {
					var ee *exec.ExitError
					if !errors.As(err, &ee) {
						http.Error(w, err.Error(), 500)
						return
					}
					code = ee.ExitCode()
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"exitCode": code, "stdout": string(out)})
			}))
			defer server.Close()
			c := &client{base: server.URL, http: server.Client()}
			var uploadErr error
			if tc.operation == "write" {
				uploadErr = c.WriteFile(t.Context(), "fixture", destination, content)
			} else {
				repo := t.TempDir()
				if err := os.WriteFile(filepath.Join(repo, "uploaded"), []byte(content), 0o755); err != nil {
					t.Fatal(err)
				}
				archive, err := core.CreateSyncArchive(t.Context(), Repo{Root: repo}, SyncManifest{Files: []string{"uploaded"}}, "crabbox-smolvm-native-*.tgz")
				if err != nil {
					t.Fatal(err)
				}
				defer os.Remove(archive.Name())
				if err := archive.Close(); err != nil {
					t.Fatal(err)
				}
				if tc.failure == "corrupt" {
					if err := os.WriteFile(archive.Name(), []byte("not an archive"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				uploadErr = c.InjectArchive(t.Context(), "fixture", archive.Name(), workspace)
			}
			if (uploadErr != nil) != tc.wantError {
				t.Fatalf("upload error=%v, wantError=%v", uploadErr, tc.wantError)
			}
			if strings.HasPrefix(tc.failure, "decode") {
				var exitErr ExitError
				if !errors.As(uploadErr, &exitErr) || exitErr.Code != 23 {
					t.Fatalf("decoder status lost: %v", uploadErr)
				}
			}
			if strings.Contains(tc.failure, "cleanup") && !strings.Contains(uploadErr.Error(), "upload staging cleanup failed") {
				t.Fatalf("cleanup diagnostic lost: %v", uploadErr)
			}
			if tc.operation == "write" && tc.failure != "directory" {
				got, err := os.ReadFile(destination)
				want := content
				if tc.wantError && tc.failure != "cleanup" {
					want = "old"
				}
				if err != nil || string(got) != want {
					t.Fatalf("destination=%q err=%v, want %q", got, err, want)
				}
				info, err := os.Stat(destination)
				if err != nil || info.Mode().Perm() != 0o600 {
					t.Fatalf("private file mode: info=%v err=%v", info, err)
				}
			} else if tc.operation == "archive" {
				got, err := os.ReadFile(filepath.Join(workspace, "uploaded"))
				if tc.wantError {
					if !os.IsNotExist(err) {
						t.Fatalf("failed upload consumed archive: data=%q err=%v", got, err)
					}
				} else if err != nil || string(got) != content {
					t.Fatalf("archive payload=%q err=%v", got, err)
				}
				if !tc.wantError {
					info, err := os.Stat(filepath.Join(workspace, "uploaded"))
					if err != nil || info.Mode().Perm() != 0o755 {
						t.Fatalf("archive mode changed: info=%v err=%v", info, err)
					}
				}
			}
			for _, dir := range []string{root, workspace} {
				matches, err := filepath.Glob(filepath.Join(dir, ".crabbox-upload.*"))
				wantResidue := 0
				if dir == workspace && strings.Contains(tc.failure, "cleanup") {
					wantResidue = 1
				}
				if err != nil || len(matches) != wantResidue {
					t.Fatalf("staging residue=%v err=%v", matches, err)
				}
			}
		})
	}
}

func TestRunCleansPartialEnvironmentProfile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("native cleanup fixture requires POSIX shell")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh unavailable")
	}
	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprint(canceled), func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			root := t.TempDir()
			mapped := filepath.Join(root, "partial-profile")
			var remote string
			primary := errors.New("partial upload")
			fake := &fakeAPI{}
			fake.writeHook = func(_ context.Context, remotePath, content string) error {
				remote = remotePath
				if err := os.WriteFile(mapped, []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
				if canceled {
					cancel()
					return context.Canceled
				}
				return primary
			}
			cleanups := 0
			fake.execHook = func(cleanupCtx context.Context, command string) (execResult, error) {
				if !strings.HasPrefix(command, "rm -f ") {
					return execResult{}, nil
				}
				cleanups++
				if cleanupCtx.Err() != nil {
					t.Fatalf("cleanup canceled: %v", cleanupCtx.Err())
				}
				if _, ok := cleanupCtx.Deadline(); !ok {
					t.Fatal("cleanup is unbounded")
				}
				if !strings.Contains(command, shellQuote(remote)) {
					t.Fatal("wrong cleanup target")
				}
				cmd := exec.CommandContext(cleanupCtx, "sh", "-c", strings.ReplaceAll(command, shellQuote(remote), shellQuote(mapped)))
				out, err := cmd.CombinedOutput()
				return execResult{Output: string(out)}, err
			}
			withFakeAPI(t, fake)
			b := NewBackend(Provider{}.Spec(), testConfig(), testRuntime()).(*backend)
			_, err := b.Run(ctx, RunRequest{Repo: Repo{Root: root, Name: "fixture"}, NoSync: true, Keep: true, Command: []string{"true"}, Env: map[string]string{"FIXTURE": "synthetic"}})
			want := primary
			if canceled {
				want = context.Canceled
			}
			if !errors.Is(err, want) || cleanups != 1 || len(fake.streamCommands) != 0 || fake.deleted {
				t.Fatalf("err=%v cleanups=%d commands=%v deleted=%v", err, cleanups, fake.streamCommands, fake.deleted)
			}
			if _, err := os.Stat(mapped); !os.IsNotExist(err) {
				t.Fatalf("partial profile retained: %v", err)
			}
		})
	}
}

func TestEnvironmentProfileCleanupRejectsChangedOwnership(t *testing.T) {
	for _, changed := range []string{"claim", "machine"} {
		t.Run(changed, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			fake := &fakeAPI{}
			withFakeAPI(t, fake)
			var stderr bytes.Buffer
			rt := testRuntime()
			rt.Stderr = &stderr
			b := NewBackend(Provider{}.Spec(), testConfig(), rt).(*backend)
			fake.streamHook = func() {
				if changed == "machine" {
					fake.machine.CreatedAt = "2026-09-01T00:00:00Z"
					return
				}
				claims, err := core.ListLeaseClaims()
				if err != nil || len(claims) != 1 {
					t.Fatalf("claims=%v err=%v", claims, err)
				}
				labels := map[string]string{}
				for k, v := range claims[0].Labels {
					labels[k] = v
				}
				labels["fixture_changed"] = "true"
				if _, err := core.UpdateLeaseClaimLabelsIfUnchanged(claims[0].LeaseID, claims[0], labels); err != nil {
					t.Fatal(err)
				}
			}
			_, err := b.Run(t.Context(), RunRequest{Repo: Repo{Root: t.TempDir(), Name: "fixture"}, NoSync: true, Keep: true, Command: []string{"true"}, Env: map[string]string{"FIXTURE": "synthetic"}})
			if err != nil {
				t.Fatal(err)
			}
			for _, command := range fake.execCommands {
				if strings.HasPrefix(command, "rm -f ") {
					t.Fatalf("stale cleanup executed: %s", command)
				}
			}
			if !strings.Contains(stderr.String(), "env profile cleanup failed") {
				t.Fatalf("missing cleanup warning: %s", stderr.String())
			}
		})
	}
}

func TestRunSourceIntentSurvivesNativeShell(t *testing.T) {
	if os.PathSeparator != '/' {
		t.Skip("POSIX source transport")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh unavailable")
	}
	for _, tc := range []struct {
		name    string
		command []string
		literal map[int]bool
		want    string
		code    int
	}{
		{"literal separator", []string{"printf", "<%s>", ";", "touch", "sentinel"}, map[int]bool{2: true}, "<;><touch><sentinel>", 0},
		{"literal executable", []string{"FOO=x", "argument"}, map[int]bool{0: true}, "literal:argument", 42},
		{"mixed intent", []string{"printf", "%s", ";", "&&", "printf", "%s", "done"}, map[int]bool{2: true}, ";done", 0},
		{"inferred singleton", []string{"printf 'source value'"}, nil, "source value", 0},
		{"ordinary argv", []string{"printf", "%s", "value"}, nil, "value", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			root := t.TempDir()
			program := filepath.Join(root, "FOO=x")
			if err := os.WriteFile(program, []byte("#!/bin/sh\nprintf 'literal:%s' \"$*\"\nexit 42\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			fake := &fakeAPI{}
			withFakeAPI(t, fake)
			b := NewBackend(Provider{}.Spec(), testConfig(), testRuntime()).(*backend)
			_, err := b.Run(t.Context(), RunRequest{Repo: Repo{Root: root, Name: "fixture"}, NoSync: true, Keep: true, Command: tc.command, CommandLiteralArgs: tc.literal})
			if err != nil {
				t.Fatal(err)
			}
			if len(fake.streamCommands) != 1 {
				t.Fatalf("stream commands=%v", fake.streamCommands)
			}
			cmd := exec.Command(sh, "-c", fake.streamCommands[0])
			cmd.Dir = root
			cmd.Env = []string{"HOME=" + root, "PATH=" + root + ":/usr/bin:/bin", "ENV=" + os.DevNull}
			out, runErr := cmd.CombinedOutput()
			code := 0
			if runErr != nil {
				var ee *exec.ExitError
				if !errors.As(runErr, &ee) {
					t.Fatal(runErr)
				}
				code = ee.ExitCode()
			}
			if string(out) != tc.want || code != tc.code {
				t.Fatalf("source=%q output=%q code=%d want=%q/%d", fake.streamCommands[0], out, code, tc.want, tc.code)
			}
			if _, err := os.Stat(filepath.Join(root, "sentinel")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("literal created sentinel: %v", err)
			}
		})
	}
}
