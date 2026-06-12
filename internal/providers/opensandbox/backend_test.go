package opensandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"regexp"
	"strings"
	"testing"
	"time"

	sdk "github.com/alibaba/OpenSandbox/sdks/sandbox/go"
	core "github.com/openclaw/crabbox/internal/cli"
)

func TestProviderSpec(t *testing.T) {
	p := Provider{}
	if p.Name() != "opensandbox" {
		t.Fatalf("Name=%q want opensandbox", p.Name())
	}
	if len(p.Aliases()) != 0 {
		t.Fatalf("v1 should not register aliases, got %#v", p.Aliases())
	}
	spec := p.Spec()
	if spec.Kind != core.ProviderKindDelegatedRun {
		t.Fatalf("kind=%v want delegated run", spec.Kind)
	}
	if spec.Coordinator != core.CoordinatorNever {
		t.Fatalf("coordinator=%v want never", spec.Coordinator)
	}
	if len(spec.Targets) != 1 || spec.Targets[0].OS != core.TargetLinux {
		t.Fatalf("targets=%#v want [{linux}]", spec.Targets)
	}
	if !spec.Features.Has(core.FeatureArchiveSync) {
		t.Fatalf("features=%#v want archive-sync", spec.Features)
	}
}

func TestProviderForResolvesNameOnly(t *testing.T) {
	got, err := core.ProviderFor("opensandbox")
	if err != nil {
		t.Fatalf("ProviderFor(opensandbox): %v", err)
	}
	if got.Name() != "opensandbox" {
		t.Fatalf("Name=%q want opensandbox", got.Name())
	}
	for _, alias := range []string{"osb", "open-sandbox"} {
		if got, err := core.ProviderFor(alias); err == nil && got.Name() == "opensandbox" {
			t.Fatalf("alias %q unexpectedly resolves to opensandbox", alias)
		}
	}
}

func TestOpenSandboxWorkdirRejectsBroadPaths(t *testing.T) {
	for _, workdir := range []string{"/", "/tmp", "/workspace", "/workspace/.."} {
		t.Run(workdir, func(t *testing.T) {
			cfg := testConfig()
			cfg.OpenSandbox.Workdir = workdir
			if _, err := openSandboxWorkdir(cfg); err == nil || !strings.Contains(err.Error(), "too broad") {
				t.Fatalf("err=%v, want too broad rejection", err)
			}
		})
	}
}

func TestOpenSandboxClaimScopeIsMetadataLabelSafe(t *testing.T) {
	scope, err := newOpenSandboxClaimScope("https://opensandbox.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if len(scope) > 63 {
		t.Fatalf("scope length=%d want <=63: %q", len(scope), scope)
	}
	if !regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9._-]*[A-Za-z0-9])?$`).MatchString(scope) {
		t.Fatalf("scope is not a valid metadata label value: %q", scope)
	}
}

func TestRunCreatesSandboxForwardsEnvAndCleansUp(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeClient()
	backend := newTestBackend(fake)
	result, err := backend.Run(context.Background(), RunRequest{
		Repo:    Repo{Name: "my-app", Root: tempGitRepo(t)},
		NoSync:  true,
		Command: []string{"printenv", "API_TOKEN"},
		Env:     map[string]string{"API_TOKEN": "secret-value"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || !result.SyncDelegated {
		t.Fatalf("result=%#v", result)
	}
	if fake.created.Image != "ubuntu:24.04" || fake.created.Metadata[openSandboxClaimKey] == "" || fake.created.Metadata[openSandboxNameKey] == "" {
		t.Fatalf("create request not populated: %#v", fake.created)
	}
	if got := fake.runs[len(fake.runs)-1].Env["API_TOKEN"]; got != "secret-value" {
		t.Fatalf("env forwarded as %q", got)
	}
	if len(fake.deleted) != 1 || fake.deleted[0] != fake.sandbox.ID {
		t.Fatalf("deleted=%#v want cleanup of created sandbox", fake.deleted)
	}
	if claims, err := listOpenSandboxLeaseClaims(); err != nil {
		t.Fatal(err)
	} else if len(claims) != 0 {
		t.Fatalf("claim not cleaned up: %#v", claims)
	}
}

func TestRunNoSyncEnsuresWorkspaceWithPortableShell(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeClient()
	backend := newTestBackend(fake)
	_, err := backend.Run(context.Background(), RunRequest{
		Repo:    Repo{Name: "my-app", Root: tempGitRepo(t)},
		NoSync:  true,
		Command: []string{"true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.runs) < 2 {
		t.Fatalf("runs=%#v, want workspace setup and user command", fake.runs)
	}
	if !strings.HasPrefix(fake.runs[0].Command, "sh -lc ") {
		t.Fatalf("workspace command=%q, want sh -lc", fake.runs[0].Command)
	}
}

func TestRunPreservesBashLoginShellForExplicitInvocation(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeClient()
	backend := newTestBackend(fake)
	_, err := backend.Run(context.Background(), RunRequest{
		Repo:    Repo{Name: "my-app", Root: tempGitRepo(t)},
		NoSync:  true,
		Command: []string{"bash", "-lc", "echo hello"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := fake.runs[len(fake.runs)-1].Command
	want := shellScriptFromArgv([]string{"bash", "-lc", "echo hello"})
	if got != want {
		t.Fatalf("command=%q want %q", got, want)
	}
}

func TestRunPreservesBashLoginShellForAutoWrappedMetachars(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeClient()
	backend := newTestBackend(fake)
	_, err := backend.Run(context.Background(), RunRequest{
		Repo:    Repo{Name: "my-app", Root: tempGitRepo(t)},
		NoSync:  true,
		Command: []string{"pnpm", "install", "&&", "pnpm", "test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := fake.runs[len(fake.runs)-1].Command
	inner := shellScriptFromArgv([]string{"pnpm", "install", "&&", "pnpm", "test"})
	want := shellScriptFromArgv([]string{"bash", "-lc", inner})
	if got != want {
		t.Fatalf("command=%q want %q", got, want)
	}
}

func TestRunKeepsSandboxOnFailureWhenRequested(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeClient()
	fake.runExit = 7
	backend := newTestBackend(fake)
	_, err := backend.Run(context.Background(), RunRequest{
		Repo:          Repo{Name: "my-app", Root: tempGitRepo(t)},
		NoSync:        true,
		Command:       []string{"false"},
		KeepOnFailure: true,
	})
	if err == nil || !strings.Contains(err.Error(), "exited 7") {
		t.Fatalf("err=%v, want exit error", err)
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("deleted=%#v, want kept on failure", fake.deleted)
	}
	claim, err := readLeaseClaim(leasePrefix + fake.sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.Provider != providerName {
		t.Fatalf("claim=%#v", claim)
	}
}

func TestRunRejectsNonLinuxPlatformOS(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeClient()
	backend := newTestBackend(fake)
	backend.cfg.OpenSandbox.PlatformOS = "windows"
	_, err := backend.Run(context.Background(), RunRequest{
		Repo:    Repo{Name: "my-app", Root: tempGitRepo(t)},
		NoSync:  true,
		Command: []string{"true"},
	})
	if err == nil || !strings.Contains(err.Error(), "only supports Linux") {
		t.Fatalf("err=%v, want Linux-only platform rejection", err)
	}
	if fake.created.Image != "" {
		t.Fatalf("created sandbox despite invalid platform: %#v", fake.created)
	}
}

func TestRunVerifiesOwnershipBeforeReclaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeClient()
	backend := newTestBackend(fake)
	leaseID := leasePrefix + fake.sandbox.ID
	if err := claimLeaseForRepoProviderScopePond(leaseID, "mine", providerName, openSandboxEndpointScope(fake.baseURL)+"-own-local", "", "/original", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	fake.sandbox.Metadata[openSandboxClaimKey] = "different"
	_, err := backend.Run(context.Background(), RunRequest{
		ID: leaseID, Repo: Repo{Name: "my-app", Root: "/replacement"}, Reclaim: true, NoSync: true, Command: []string{"true"},
	})
	if err == nil || !strings.Contains(err.Error(), "ownership metadata") {
		t.Fatalf("err=%v, want ownership mismatch", err)
	}
	claim, err := readLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.RepoRoot != "/original" {
		t.Fatalf("repo root=%q changed before ownership verification", claim.RepoRoot)
	}
}

func TestRunResumesPausedSandboxBeforeReuse(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeClient()
	fake.sandbox.State = "Paused"
	backend := newTestBackend(fake)
	leaseID := leasePrefix + fake.sandbox.ID
	if err := claimLeaseForRepoProviderScopePond(leaseID, "mine", providerName, openSandboxEndpointScope(fake.baseURL)+"-own-local", "", "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	fake.sandbox.Metadata[openSandboxClaimKey] = openSandboxEndpointScope(fake.baseURL) + "-own-local"
	_, err := backend.Run(context.Background(), RunRequest{
		ID: leaseID, Repo: Repo{Name: "my-app", Root: "/repo"}, NoSync: true, Command: []string{"true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.resumed) != 1 || fake.resumed[0] != fake.sandbox.ID {
		t.Fatalf("resumed=%#v", fake.resumed)
	}
}

func TestStopRejectsOwnershipMismatch(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeClient()
	backend := newTestBackend(fake)
	leaseID := leasePrefix + fake.sandbox.ID
	if err := claimLeaseForRepoProviderScopePond(leaseID, "mine", providerName, openSandboxEndpointScope(fake.baseURL)+"-own-local", "", "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	fake.sandbox.Metadata[openSandboxClaimKey] = "different"
	err := backend.Stop(context.Background(), StopRequest{ID: "mine"})
	if err == nil || !strings.Contains(err.Error(), "ownership metadata") {
		t.Fatalf("err=%v, want ownership mismatch", err)
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("deleted=%#v after ownership mismatch", fake.deleted)
	}
}

func TestStopForgetMissingRemovesClaimOnlyWhenExplicit(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeClient()
	fake.notFound = true
	backend := newTestBackend(fake)
	leaseID := leasePrefix + fake.sandbox.ID
	if err := claimLeaseForRepoProviderScopePond(leaseID, "stale", providerName, openSandboxEndpointScope(fake.baseURL)+"-own-local", "", "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if err := backend.Stop(context.Background(), StopRequest{ID: "stale"}); err == nil {
		t.Fatal("expected missing sandbox to fail without forget flag")
	}
	backend.cfg.OpenSandbox.ForgetMissing = true
	if err := backend.Stop(context.Background(), StopRequest{ID: "stale"}); err != nil {
		t.Fatal(err)
	}
	if claim, err := readLeaseClaim(leaseID); err != nil {
		t.Fatal(err)
	} else if claim.LeaseID != "" {
		t.Fatalf("claim still present after forget-missing: %#v", claim)
	}
}

func TestNewOpenSandboxClientRequiresExplicitAPIURL(t *testing.T) {
	t.Setenv("CRABBOX_OPENSANDBOX_API_KEY", "test-key")
	_, err := newOpenSandboxClient(testConfig(), Runtime{Stdout: io.Discard, Stderr: io.Discard})
	if err == nil || !strings.Contains(err.Error(), "trusted API URL") {
		t.Fatalf("err=%v, want trusted API URL requirement", err)
	}
}

func TestSDKClientCreateUsesHeadersAndRequestBody(t *testing.T) {
	t.Setenv("CRABBOX_OPENSANDBOX_API_KEY", "test-key")
	var createPath, gotAuth string
	var gotBody struct {
		Image struct {
			URI string `json:"uri"`
		} `json:"image"`
		ResourceLimits map[string]string `json:"resourceLimits"`
		Metadata       map[string]string `json:"metadata"`
		Entrypoint     []string          `json:"entrypoint"`
		Timeout        int               `json:"timeout"`
		Platform       struct {
			OS   string `json:"os"`
			Arch string `json:"arch"`
		} `json:"platform"`
	}
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes":
			createPath = r.URL.Path
			gotAuth = r.Header.Get("OPEN-SANDBOX-API-KEY")
			if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
				t.Errorf("decode body: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"sb-sdk","status":{"state":"Running"},"metadata":{"crabbox.claim":"scope"},"createdAt":"2026-06-11T00:00:00Z"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sandboxes/sb-sdk":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"sb-sdk","status":{"state":"Running"},"metadata":{"crabbox.claim":"scope"},"createdAt":"2026-06-11T00:00:00Z"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sandboxes/sb-sdk/endpoints/44772":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"endpoint":"`+server.URL+`","headers":{"X-EXECD-ACCESS-TOKEN":"exec-token"}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/ping":
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := testConfig()
	cfg.OpenSandbox.APIURL = server.URL
	client, err := newOpenSandboxClient(cfg, Runtime{HTTP: server.Client(), Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.CreateSandbox(context.Background(), createSandboxOptions{
		Image:        "ubuntu:test",
		CPU:          "500m",
		Memory:       "512Mi",
		PlatformOS:   "linux",
		PlatformArch: "amd64",
		Metadata:     map[string]string{openSandboxClaimKey: "scope"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if createPath != "/v1/sandboxes" || gotAuth != "test-key" {
		t.Fatalf("path=%q auth=%q", createPath, gotAuth)
	}
	if gotBody.Image.URI != "ubuntu:test" || gotBody.ResourceLimits["cpu"] != "500m" || gotBody.ResourceLimits["memory"] != "512Mi" || gotBody.Metadata[openSandboxClaimKey] != "scope" || gotBody.Platform.OS != "linux" || gotBody.Platform.Arch != "amd64" {
		t.Fatalf("body=%#v", gotBody)
	}
	if gotBody.Timeout != sdk.DefaultTimeoutSeconds {
		t.Fatalf("timeout=%d want SDK default %d", gotBody.Timeout, sdk.DefaultTimeoutSeconds)
	}
	if strings.Join(gotBody.Entrypoint, "\x00") != strings.Join(sdk.DefaultEntrypoint, "\x00") {
		t.Fatalf("entrypoint=%#v want %#v", gotBody.Entrypoint, sdk.DefaultEntrypoint)
	}
}

func TestSDKClientLifecycleRequestsAreBounded(t *testing.T) {
	t.Setenv("CRABBOX_OPENSANDBOX_API_KEY", "test-key")
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()

	cfg := testConfig()
	cfg.OpenSandbox.APIURL = server.URL
	client, err := newOpenSandboxClient(cfg, Runtime{HTTP: server.Client(), Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	sdkClient := client.(*sdkOpenSandboxClient)
	sdkClient.requestTimeoutOverride = 20 * time.Millisecond

	start := time.Now()
	err = client.Probe(context.Background())
	if err == nil {
		t.Fatal("expected stalled lifecycle request to time out")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("lifecycle timeout took %s, want under 1s", elapsed)
	}
}

func TestSDKClientCreateWaitsForRunningAndExecdPing(t *testing.T) {
	t.Setenv("CRABBOX_OPENSANDBOX_API_KEY", "test-key")
	statusPolls := 0
	endpointHits := 0
	pingHits := 0
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"sb-wait","status":{"state":"Pending"},"metadata":{"crabbox.claim":"scope"},"createdAt":"2026-06-11T00:00:00Z"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sandboxes/sb-wait":
			statusPolls++
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"sb-wait","status":{"state":"Running"},"metadata":{"crabbox.claim":"scope"},"createdAt":"2026-06-11T00:00:00Z"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sandboxes/sb-wait/endpoints/44772":
			endpointHits++
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"endpoint":"`+server.URL+`","headers":{"X-EXECD-ACCESS-TOKEN":"exec-token"}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/ping":
			pingHits++
			if got := r.Header.Get("X-EXECD-ACCESS-TOKEN"); got != "exec-token" {
				t.Errorf("ping auth=%q want exec-token", got)
			}
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := testConfig()
	cfg.OpenSandbox.APIURL = server.URL
	client, err := newOpenSandboxClient(cfg, Runtime{HTTP: server.Client(), Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	info, err := client.CreateSandbox(context.Background(), createSandboxOptions{
		Image:    "ubuntu:test",
		CPU:      "500m",
		Memory:   "512Mi",
		Metadata: map[string]string{openSandboxClaimKey: "scope"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if info.ID != "sb-wait" || info.State != "Running" {
		t.Fatalf("info=%#v", info)
	}
	if statusPolls == 0 {
		t.Fatal("expected create to poll sandbox status until running")
	}
	if endpointHits == 0 || pingHits == 0 {
		t.Fatalf("endpointHits=%d pingHits=%d, want readiness ping", endpointHits, pingHits)
	}
}

func TestSDKClientReadyTimeoutUsesProviderBudget(t *testing.T) {
	client := &sdkOpenSandboxClient{cfg: testConfig()}
	if got := client.readyTimeout(); got != openSandboxReadyTimeout {
		t.Fatalf("ready timeout=%s want %s", got, openSandboxReadyTimeout)
	}
	client.cfg.OpenSandbox.TimeoutSecs = 900
	if got := client.readyTimeout(); got != 15*time.Minute {
		t.Fatalf("ready timeout=%s want 15m", got)
	}
}

func TestSDKClientCreateDeletesSandboxWhenReadinessFails(t *testing.T) {
	t.Setenv("CRABBOX_OPENSANDBOX_API_KEY", "test-key")
	deleted := 0
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"sb-cleanup","status":{"state":"Running"},"metadata":{"crabbox.claim":"scope"},"createdAt":"2026-06-11T00:00:00Z"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sandboxes/sb-cleanup/endpoints/44772":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"endpoint":"`+server.URL+`","headers":{"X-EXECD-ACCESS-TOKEN":"exec-token"}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/ping":
			http.Error(w, "not ready", http.StatusServiceUnavailable)
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/sandboxes/sb-cleanup":
			deleted++
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := testConfig()
	cfg.OpenSandbox.APIURL = server.URL
	client, err := newOpenSandboxClient(cfg, Runtime{HTTP: server.Client(), Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = client.CreateSandbox(ctx, createSandboxOptions{
		Image:    "ubuntu:test",
		CPU:      "500m",
		Memory:   "512Mi",
		Metadata: map[string]string{openSandboxClaimKey: "scope"},
	})
	if err == nil || !strings.Contains(err.Error(), "wait until ready") {
		t.Fatalf("err=%v, want readiness failure", err)
	}
	if deleted != 1 {
		t.Fatalf("deleted=%d want 1 cleanup delete", deleted)
	}
}

func TestSDKClientProxyExecdAddsAccessTokenWhenEndpointOmitsIt(t *testing.T) {
	t.Setenv("CRABBOX_OPENSANDBOX_API_KEY", "proxy-key")
	var gotEndpointQuery, gotExecdAuth string
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sandboxes/sb-proxy/endpoints/44772":
			gotEndpointQuery = r.URL.RawQuery
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"endpoint":"`+server.URL+`","headers":{"X-Route-Hint":"sticky"}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/command":
			gotExecdAuth = r.Header.Get("X-EXECD-ACCESS-TOKEN")
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"type\":\"execution_complete\",\"exit_code\":0}\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := testConfig()
	cfg.OpenSandbox.APIURL = server.URL
	cfg.OpenSandbox.UseServerProxy = true
	client, err := newOpenSandboxClient(cfg, Runtime{HTTP: server.Client(), Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	exitCode, err := client.RunCommand(context.Background(), "sb-proxy", runCommandRequest{Command: "true"})
	if err != nil {
		t.Fatal(err)
	}
	if exitCode != 0 {
		t.Fatalf("exit=%d", exitCode)
	}
	if gotEndpointQuery != "use_server_proxy=true" {
		t.Fatalf("endpoint query=%q", gotEndpointQuery)
	}
	if gotExecdAuth != "proxy-key" {
		t.Fatalf("X-EXECD-ACCESS-TOKEN=%q want proxy-key", gotExecdAuth)
	}
}

func TestSDKClientRunCommandSendsTimeoutMillis(t *testing.T) {
	t.Setenv("CRABBOX_OPENSANDBOX_API_KEY", "test-key")
	var gotTimeout int64
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sandboxes/sb-timeout/endpoints/44772":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"endpoint":"`+server.URL+`","headers":{"X-EXECD-ACCESS-TOKEN":"exec-token"}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/command":
			var body struct {
				Timeout int64 `json:"timeout"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode body: %v", err)
			}
			gotTimeout = body.Timeout
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"type\":\"execution_complete\",\"exit_code\":0}\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := testConfig()
	cfg.OpenSandbox.APIURL = server.URL
	client, err := newOpenSandboxClient(cfg, Runtime{HTTP: server.Client(), Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	exitCode, err := client.RunCommand(context.Background(), "sb-timeout", runCommandRequest{
		Command:     "true",
		TimeoutSecs: 3600,
	})
	if err != nil {
		t.Fatal(err)
	}
	if exitCode != 0 {
		t.Fatalf("exit=%d", exitCode)
	}
	if gotTimeout != int64(time.Hour/time.Millisecond) {
		t.Fatalf("timeout sent=%d, want %d milliseconds for 3600 seconds", gotTimeout, int64(time.Hour/time.Millisecond))
	}
}

func TestSDKClientRunCommandAddsConfiguredSchemeToBareEndpoint(t *testing.T) {
	t.Setenv("CRABBOX_OPENSANDBOX_API_KEY", "test-key")
	commandHit := false
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sandboxes/sb-bare/endpoints/44772":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"endpoint":"`+strings.TrimPrefix(server.URL, "https://")+`","headers":{"X-EXECD-ACCESS-TOKEN":"exec-token"}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/command":
			commandHit = true
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"type\":\"execution_complete\",\"exit_code\":0}\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := testConfig()
	cfg.OpenSandbox.APIURL = server.URL
	client, err := newOpenSandboxClient(cfg, Runtime{HTTP: server.Client(), Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	exitCode, err := client.RunCommand(context.Background(), "sb-bare", runCommandRequest{Command: "true"})
	if err != nil {
		t.Fatal(err)
	}
	if exitCode != 0 || !commandHit {
		t.Fatalf("exit=%d commandHit=%v", exitCode, commandHit)
	}
}

func TestCommandEventErrorDefaultsToFailureExit(t *testing.T) {
	var stderr bytes.Buffer
	client := &sdkOpenSandboxClient{rt: Runtime{Stdout: io.Discard, Stderr: &stderr}}
	result, err := client.handleCommandEvent(streamEvent(`{"type":"error","error":{"evalue":"command failed"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !result.errorEvent || result.exitCode == nil || *result.exitCode != 1 {
		t.Fatalf("result=%#v, want default failure exit", result)
	}
	if !strings.Contains(stderr.String(), "command failed") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestCommandEventHonorsExplicitExitCode(t *testing.T) {
	client := &sdkOpenSandboxClient{rt: Runtime{Stdout: io.Discard, Stderr: io.Discard}}
	result, err := client.handleCommandEvent(streamEvent(`{"type":"execution_complete","exit_code":42}`))
	if err != nil {
		t.Fatal(err)
	}
	if result.exitCode == nil || *result.exitCode != 42 {
		t.Fatalf("result=%#v, want exit 42", result)
	}
}

func TestCommandEventRoutesRawStderrEvent(t *testing.T) {
	var stdout, stderr bytes.Buffer
	client := &sdkOpenSandboxClient{rt: Runtime{Stdout: &stdout, Stderr: &stderr}}
	if _, err := client.handleCommandEvent(sdk.StreamEvent{Event: "stderr", Data: "warn"}); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "" || stderr.String() != "warn" {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestCommandEventPreservesJSONLookingRawStdoutStderr(t *testing.T) {
	var stdout, stderr bytes.Buffer
	client := &sdkOpenSandboxClient{rt: Runtime{Stdout: &stdout, Stderr: &stderr}}
	if _, err := client.handleCommandEvent(sdk.StreamEvent{Event: "stdout", Data: `{"ok":true}`}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.handleCommandEvent(sdk.StreamEvent{Event: "stderr", Data: `{"warn":true}`}); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != `{"ok":true}` || stderr.String() != `{"warn":true}` {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestCommandEventUsesStructuredJSONWhenTypePresent(t *testing.T) {
	var stdout bytes.Buffer
	client := &sdkOpenSandboxClient{rt: Runtime{Stdout: &stdout, Stderr: io.Discard}}
	if _, err := client.handleCommandEvent(sdk.StreamEvent{Event: "stdout", Data: `{"type":"stdout","text":"hello"}`}); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "hello" {
		t.Fatalf("stdout=%q", stdout.String())
	}
}

func TestCommandEventUsesDataFieldForOutput(t *testing.T) {
	var stdout, stderr bytes.Buffer
	client := &sdkOpenSandboxClient{rt: Runtime{Stdout: &stdout, Stderr: &stderr}}
	if _, err := client.handleCommandEvent(streamEvent(`{"type":"stdout","data":"hello"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := client.handleCommandEvent(streamEvent(`{"type":"stderr","data":"warn"}`)); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "hello" || stderr.String() != "warn" {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestCommandEventPreservesOutputWhitespace(t *testing.T) {
	var stdout, stderr bytes.Buffer
	client := &sdkOpenSandboxClient{rt: Runtime{Stdout: &stdout, Stderr: &stderr}}
	if _, err := client.handleCommandEvent(streamEvent("{\"type\":\"stdout\",\"text\":\"  ok\\n\"}")); err != nil {
		t.Fatal(err)
	}
	if _, err := client.handleCommandEvent(streamEvent("{\"type\":\"stderr\",\"data\":\"\\n\"}")); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "  ok\n" || stderr.String() != "\n" {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func streamEvent(data string) sdk.StreamEvent {
	return sdk.StreamEvent{Data: data}
}

func newTestBackend(fake *fakeOpenSandboxClient) *openSandboxBackend {
	cfg := testConfig()
	return &openSandboxBackend{
		spec: Provider{}.Spec(),
		cfg:  cfg,
		rt: Runtime{
			Stdout: &bytes.Buffer{},
			Stderr: &bytes.Buffer{},
		},
		newClient: func(Config, Runtime) (openSandboxClient, error) {
			return fake, nil
		},
		cleanupTimeoutOverride: 10 * time.Millisecond,
	}
}

func testConfig() Config {
	cfg := Config{}
	cfg.Provider = providerName
	cfg.OpenSandbox.Image = "ubuntu:24.04"
	cfg.OpenSandbox.Workdir = "/workspace/crabbox"
	cfg.OpenSandbox.CPU = "1"
	cfg.OpenSandbox.Memory = "2Gi"
	cfg.OpenSandbox.ExecTimeoutSecs = 3600
	cfg.OpenSandbox.PlatformOS = "linux"
	cfg.OpenSandbox.PlatformArch = "amd64"
	return cfg
}

func tempGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(path.Join(dir, "go.mod"), []byte("module example.test/my-app\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

type fakeOpenSandboxClient struct {
	baseURL  string
	sandbox  sandboxInfo
	created  createSandboxOptions
	runs     []runCommandRequest
	deleted  []string
	resumed  []string
	uploads  []string
	runExit  int
	runErr   error
	notFound bool
}

func newFakeClient() *fakeOpenSandboxClient {
	return &fakeOpenSandboxClient{
		baseURL: "https://opensandbox.example.test",
		sandbox: sandboxInfo{
			ID:    "sb-test123",
			State: "Running",
			Metadata: map[string]string{
				openSandboxClaimKey: "pending",
			},
		},
	}
}

func (f *fakeOpenSandboxClient) BaseURL() string { return f.baseURL }

func (f *fakeOpenSandboxClient) CreateSandbox(_ context.Context, req createSandboxOptions) (sandboxInfo, error) {
	f.created = req
	f.sandbox.Metadata = cloneStringMap(req.Metadata)
	return f.sandbox, nil
}

func (f *fakeOpenSandboxClient) ListSandboxes(context.Context, map[string]string) ([]sandboxInfo, error) {
	return []sandboxInfo{f.sandbox}, nil
}

func (f *fakeOpenSandboxClient) GetSandbox(context.Context, string) (sandboxInfo, error) {
	if f.notFound {
		return sandboxInfo{}, errOpenSandboxNotFound
	}
	return f.sandbox, nil
}

func (f *fakeOpenSandboxClient) DeleteSandbox(_ context.Context, sandboxID string) error {
	f.deleted = append(f.deleted, sandboxID)
	if f.notFound {
		return errOpenSandboxNotFound
	}
	return nil
}

func (f *fakeOpenSandboxClient) ResumeSandbox(_ context.Context, sandboxID string) error {
	f.resumed = append(f.resumed, sandboxID)
	f.sandbox.State = "Running"
	return nil
}

func (f *fakeOpenSandboxClient) UploadFile(_ context.Context, _ string, remotePath string, _ io.Reader) error {
	f.uploads = append(f.uploads, remotePath)
	return nil
}

func (f *fakeOpenSandboxClient) RunCommand(_ context.Context, _ string, req runCommandRequest) (int, error) {
	f.runs = append(f.runs, req)
	return f.runExit, f.runErr
}

func (f *fakeOpenSandboxClient) Probe(context.Context) error { return nil }
