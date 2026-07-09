package unikraftcloud

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const testInstanceUUID = "11111111-2222-3333-4444-555555555555"

type fakeUnikraftCloudAPI struct {
	baseURL string

	created      []createInstanceRequest
	createErr    error
	createResult ukcInstance
	getErr       error
	getResults   []ukcInstance
	getCalls     int
	listErr      error
	listResult   []ukcInstance
	stopErr      error
	stoppedIDs   []string
	deleteErr    error
	deletedIDs   []string
}

func (f *fakeUnikraftCloudAPI) BaseURL() string { return f.baseURL }

func (f *fakeUnikraftCloudAPI) CreateInstance(_ context.Context, req createInstanceRequest) (ukcInstance, error) {
	f.created = append(f.created, req)
	if f.createErr != nil {
		return ukcInstance{}, f.createErr
	}
	return f.createResult, nil
}

func (f *fakeUnikraftCloudAPI) GetInstance(_ context.Context, id string) (ukcInstance, error) {
	f.getCalls++
	if f.getErr != nil {
		return ukcInstance{}, f.getErr
	}
	if len(f.getResults) == 0 {
		return ukcInstance{UUID: id, State: "running"}, nil
	}
	result := f.getResults[0]
	if len(f.getResults) > 1 {
		f.getResults = f.getResults[1:]
	}
	return result, nil
}

func (f *fakeUnikraftCloudAPI) ListInstances(_ context.Context) ([]ukcInstance, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.listResult, nil
}

func (f *fakeUnikraftCloudAPI) StopInstance(_ context.Context, id string) (ukcInstance, error) {
	f.stoppedIDs = append(f.stoppedIDs, id)
	if f.stopErr != nil {
		return ukcInstance{}, f.stopErr
	}
	return ukcInstance{UUID: id, State: "stopped"}, nil
}

func (f *fakeUnikraftCloudAPI) DeleteInstance(_ context.Context, id string) error {
	f.deletedIDs = append(f.deletedIDs, id)
	return f.deleteErr
}

func testBackend(api unikraftCloudAPI, stdout, stderr *bytes.Buffer) *backend {
	cfg := Config{Provider: providerName}
	cfg.UnikraftCloud.Image = "unikraft.org/nginx:latest"
	cfg.UnikraftCloud.MemoryMB = 256
	if stdout == nil {
		stdout = &bytes.Buffer{}
	}
	if stderr == nil {
		stderr = &bytes.Buffer{}
	}
	return &backend{
		spec:      Provider{}.Spec(),
		cfg:       cfg,
		rt:        Runtime{Stdout: stdout, Stderr: stderr},
		newClient: func(Config, Runtime) (unikraftCloudAPI, error) { return api, nil },
	}
}

func notFoundErr() error {
	return &unikraftCloudAPIError{StatusCode: http.StatusNotFound, Message: "instance not found"}
}

func TestWarmupCreatesInstanceAndClaimsLease(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	api := &fakeUnikraftCloudAPI{
		baseURL: "https://api.fra.unikraft.cloud",
		createResult: ukcInstance{
			UUID:  testInstanceUUID,
			Name:  "funky-town",
			State: "running",
			ServiceGroup: &ukcServiceGroup{
				Domains: []ukcDomain{{FQDN: "funky-town.fra.unikraft.app"}},
			},
		},
	}
	var stdout, stderr bytes.Buffer
	b := testBackend(api, &stdout, &stderr)

	if err := b.Warmup(context.Background(), WarmupRequest{Repo: Repo{Root: t.TempDir(), Name: "demo"}}); err != nil {
		t.Fatalf("Warmup: %v", err)
	}
	if len(api.created) != 1 {
		t.Fatalf("created = %#v", api.created)
	}
	if api.created[0].Image != "unikraft.org/nginx:latest" || !api.created[0].Autostart || api.created[0].MemoryMB != 256 {
		t.Fatalf("create request = %#v", api.created[0])
	}
	leaseID := leasePrefix + testInstanceUUID
	for _, want := range []string{"leased " + leaseID, "provider=" + providerName, "instance=" + testInstanceUUID, "state=running", "fqdn=funky-town.fra.unikraft.app"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("stdout = %q, want %q", stdout.String(), want)
		}
	}
	if !strings.Contains(stderr.String(), "keeps the instance until explicit stop") {
		t.Fatalf("stderr = %q, want keep warning", stderr.String())
	}
	claim, err := readLeaseClaim(leaseID)
	if err != nil || claim.LeaseID != leaseID || claim.Provider != providerName || claim.CloudID != testInstanceUUID {
		t.Fatalf("claim = %#v err = %v", claim, err)
	}
	if claim.ProviderScope != "endpoint:https://api.fra.unikraft.cloud" {
		t.Fatalf("claim scope = %q", claim.ProviderScope)
	}
	if claim.Labels["lease"] != leaseID || claim.Labels["provider"] != providerName || claim.Labels["slug"] == "" {
		t.Fatalf("claim labels = %#v", claim.Labels)
	}
}

func TestWarmupErrors(t *testing.T) {
	for _, test := range []struct {
		name            string
		mutate          func(b *backend, api *fakeUnikraftCloudAPI)
		req             WarmupRequest
		wantErrContains string
		wantExitCode    int
	}{
		{
			name:            "missing image",
			mutate:          func(b *backend, _ *fakeUnikraftCloudAPI) { b.cfg.UnikraftCloud.Image = "" },
			wantErrContains: "warmup requires an OCI image",
			wantExitCode:    2,
		},
		{
			name:            "actions runner rejected",
			req:             WarmupRequest{ActionsRunner: true},
			wantErrContains: "--actions-runner is not supported",
			wantExitCode:    2,
		},
		{
			name: "tailscale rejected",
			req: WarmupRequest{Options: core.LeaseOptions{
				Tailscale: core.TailscaleConfig{Enabled: true},
			}},
			wantErrContains: "does not support Tailscale",
			wantExitCode:    2,
		},
		{
			name: "create API error",
			mutate: func(_ *backend, api *fakeUnikraftCloudAPI) {
				api.createErr = &unikraftCloudAPIError{StatusCode: http.StatusUnauthorized, Message: "invalid token"}
			},
			wantErrContains: "invalid token",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			api := &fakeUnikraftCloudAPI{baseURL: "https://api.fra.unikraft.cloud"}
			b := testBackend(api, nil, nil)
			if test.mutate != nil {
				test.mutate(b, api)
			}
			req := test.req
			if req.Repo.Root == "" {
				req.Repo = Repo{Root: t.TempDir(), Name: "demo"}
			}
			err := b.Warmup(context.Background(), req)
			if err == nil {
				t.Fatal("Warmup succeeded, want error")
			}
			if !strings.Contains(err.Error(), test.wantErrContains) {
				t.Fatalf("err = %v, want containing %q", err, test.wantErrContains)
			}
			if test.wantExitCode != 0 {
				var exitErr ExitError
				if !errors.As(err, &exitErr) || exitErr.Code != test.wantExitCode {
					t.Fatalf("err = %#v, want exit code %d", err, test.wantExitCode)
				}
			}
		})
	}
}

func TestWarmupCleansUpInstanceWhenClaimFails(t *testing.T) {
	stateRoot := filepath.Join(t.TempDir(), "state-file")
	if err := os.WriteFile(stateRoot, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write state sentinel: %v", err)
	}
	t.Setenv("XDG_STATE_HOME", stateRoot)
	api := &fakeUnikraftCloudAPI{
		baseURL:      "https://api.fra.unikraft.cloud",
		createResult: ukcInstance{UUID: testInstanceUUID, State: "running"},
	}
	b := testBackend(api, nil, nil)
	err := b.Warmup(context.Background(), WarmupRequest{
		Repo: Repo{Root: t.TempDir(), Name: "demo"},
	})
	if err == nil {
		t.Fatal("Warmup succeeded, want claim write failure")
	}
	if len(api.deletedIDs) != 1 || api.deletedIDs[0] != testInstanceUUID {
		t.Fatalf("deletedIDs = %#v, want created instance cleaned up", api.deletedIDs)
	}
	if claim, readErr := readLeaseClaim(leasePrefix + testInstanceUUID); readErr == nil && claim.LeaseID != "" {
		t.Fatalf("claim = %#v, want none", claim)
	}
}

func TestRunIsRejected(t *testing.T) {
	for _, test := range []struct {
		name            string
		req             RunRequest
		wantErrContains string
	}{
		{
			name:            "sync required off",
			req:             RunRequest{Command: []string{"true"}},
			wantErrContains: "pass --no-sync",
		},
		{
			name:            "command rejected",
			req:             RunRequest{NoSync: true, Command: []string{"true"}},
			wantErrContains: "cannot execute arbitrary run commands",
		},
		{
			name:            "missing command",
			req:             RunRequest{NoSync: true},
			wantErrContains: "missing command",
		},
		{
			name:            "keep rejected",
			req:             RunRequest{NoSync: true, Keep: true, Command: []string{"true"}},
			wantErrContains: "--keep is not supported",
		},
		{
			name:            "shell rejected",
			req:             RunRequest{NoSync: true, ShellMode: true, Command: []string{"true"}},
			wantErrContains: "--shell is not supported",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			api := &fakeUnikraftCloudAPI{baseURL: "https://api.fra.unikraft.cloud"}
			b := testBackend(api, nil, nil)
			_, err := b.Run(context.Background(), test.req)
			if err == nil {
				t.Fatal("Run succeeded, want rejection")
			}
			if !strings.Contains(err.Error(), test.wantErrContains) {
				t.Fatalf("err = %v, want containing %q", err, test.wantErrContains)
			}
			var exitErr ExitError
			if !errors.As(err, &exitErr) || exitErr.Code != 2 {
				t.Fatalf("err = %#v, want exit code 2", err)
			}
			if len(api.created) != 0 || len(api.deletedIDs) != 0 {
				t.Fatalf("Run touched the API: %#v", api)
			}
		})
	}
}

func TestStatusReportsInstanceState(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	api := &fakeUnikraftCloudAPI{
		baseURL: "https://api.fra.unikraft.cloud",
		getResults: []ukcInstance{{
			UUID:        testInstanceUUID,
			Name:        "funky-town",
			State:       "Running",
			PrivateFQDN: "funky-town.internal",
			MemoryMB:    256,
			ServiceGroup: &ukcServiceGroup{
				Domains: []ukcDomain{{FQDN: "funky-town.fra.unikraft.app"}},
			},
			NetworkInterfaces: []ukcNetworkInterface{{PrivateIP: "10.0.0.5"}},
		}},
	}
	b := testBackend(api, nil, nil)
	view, err := b.Status(context.Background(), StatusRequest{ID: testInstanceUUID})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if view.Provider != providerName || view.ServerID != testInstanceUUID || view.State != "running" || !view.Ready {
		t.Fatalf("view = %#v", view)
	}
	if view.Host != "funky-town.fra.unikraft.app" || view.ServerType != "unikraft-cloud-instance" {
		t.Fatalf("view host/type = %q/%q", view.Host, view.ServerType)
	}
	if view.Labels["privateIp"] != "10.0.0.5" || view.Labels["memoryMB"] != "256" || view.Labels["privateFqdn"] != "funky-town.internal" {
		t.Fatalf("labels = %#v", view.Labels)
	}
}

func TestStatusResolvesClaimedSlug(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	api := &fakeUnikraftCloudAPI{
		baseURL:      "https://api.fra.unikraft.cloud",
		createResult: ukcInstance{UUID: testInstanceUUID, State: "running"},
	}
	var stdout bytes.Buffer
	b := testBackend(api, &stdout, nil)
	if err := b.Warmup(context.Background(), WarmupRequest{Repo: Repo{Root: t.TempDir(), Name: "demo"}, RequestedSlug: "my-ukc"}); err != nil {
		t.Fatalf("Warmup: %v", err)
	}
	view, err := b.Status(context.Background(), StatusRequest{ID: "my-ukc"})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if view.ID != leasePrefix+testInstanceUUID || view.Slug != "my-ukc" || view.ServerID != testInstanceUUID {
		t.Fatalf("view = %#v", view)
	}
}

func TestStatusErrors(t *testing.T) {
	for _, test := range []struct {
		name            string
		id              string
		getErr          error
		wantErrContains string
	}{
		{name: "missing id", id: "", wantErrContains: "requires --id"},
		{name: "not found", id: testInstanceUUID, getErr: notFoundErr(), wantErrContains: "instance not found"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			api := &fakeUnikraftCloudAPI{baseURL: "https://api.fra.unikraft.cloud", getErr: test.getErr}
			b := testBackend(api, nil, nil)
			_, err := b.Status(context.Background(), StatusRequest{ID: test.id})
			if err == nil {
				t.Fatal("Status succeeded, want error")
			}
			if !strings.Contains(err.Error(), test.wantErrContains) {
				t.Fatalf("err = %v, want containing %q", err, test.wantErrContains)
			}
		})
	}
}

func TestStatusWaitPollsUntilRunning(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	api := &fakeUnikraftCloudAPI{
		baseURL: "https://api.fra.unikraft.cloud",
		getResults: []ukcInstance{
			{UUID: testInstanceUUID, State: "starting"},
			{UUID: testInstanceUUID, State: "starting"},
			{UUID: testInstanceUUID, State: "running"},
		},
	}
	b := testBackend(api, nil, nil)
	view, err := b.Status(context.Background(), StatusRequest{ID: testInstanceUUID, Wait: true, WaitTimeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !view.Ready || view.State != "running" {
		t.Fatalf("view = %#v", view)
	}
	if api.getCalls < 3 {
		t.Fatalf("getCalls = %d, want at least 3", api.getCalls)
	}
}

func TestStatusWaitTimesOut(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	api := &fakeUnikraftCloudAPI{
		baseURL:    "https://api.fra.unikraft.cloud",
		getResults: []ukcInstance{{UUID: testInstanceUUID, State: "starting"}},
	}
	b := testBackend(api, nil, nil)
	_, err := b.Status(context.Background(), StatusRequest{ID: testInstanceUUID, Wait: true, WaitTimeout: 300 * time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "timed out waiting") {
		t.Fatalf("err = %v, want wait timeout", err)
	}
}

func TestListMergesRemoteInstancesWithLocalClaims(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	api := &fakeUnikraftCloudAPI{
		baseURL:      "https://api.fra.unikraft.cloud",
		createResult: ukcInstance{UUID: testInstanceUUID, State: "running"},
		listResult: []ukcInstance{
			{UUID: testInstanceUUID, Name: "funky-town", State: "running"},
			{UUID: "66666666-7777-8888-9999-000000000000", Name: "quiet-village", State: "stopped"},
		},
	}
	b := testBackend(api, nil, nil)
	if err := b.Warmup(context.Background(), WarmupRequest{Repo: Repo{Root: t.TempDir(), Name: "demo"}, RequestedSlug: "my-ukc"}); err != nil {
		t.Fatalf("Warmup: %v", err)
	}
	servers, err := b.List(context.Background(), ListRequest{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(servers) != 2 {
		t.Fatalf("servers = %#v", servers)
	}
	if servers[0].CloudID != testInstanceUUID || servers[0].Status != "running" {
		t.Fatalf("servers[0] = %#v", servers[0])
	}
	if servers[0].Labels["lease"] != leasePrefix+testInstanceUUID || servers[0].Labels["slug"] != "my-ukc" {
		t.Fatalf("servers[0].Labels = %#v", servers[0].Labels)
	}
	if servers[1].Labels["lease"] != "" {
		t.Fatalf("servers[1] unexpectedly claimed: %#v", servers[1].Labels)
	}
}

func TestListPropagatesAPIError(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	api := &fakeUnikraftCloudAPI{
		baseURL: "https://api.fra.unikraft.cloud",
		listErr: &unikraftCloudAPIError{StatusCode: http.StatusUnauthorized, Message: "invalid token"},
	}
	b := testBackend(api, nil, nil)
	if _, err := b.List(context.Background(), ListRequest{}); err == nil || !strings.Contains(err.Error(), "invalid token") {
		t.Fatalf("err = %v, want invalid token", err)
	}
}

func TestListDoesNotPresentUnboundClaimAsOwned(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	api := &fakeUnikraftCloudAPI{
		baseURL:      "https://api.fra.unikraft.cloud",
		createResult: ukcInstance{UUID: testInstanceUUID, State: "running"},
		listResult:   []ukcInstance{{UUID: testInstanceUUID, Name: "funky-town", State: "running"}},
	}
	b := testBackend(api, nil, nil)
	repoRoot := t.TempDir()
	if err := b.Warmup(context.Background(), WarmupRequest{Repo: Repo{Root: repoRoot, Name: "demo"}}); err != nil {
		t.Fatalf("Warmup: %v", err)
	}
	leaseID := leasePrefix + testInstanceUUID
	core.RemoveLeaseClaim(leaseID)
	if err := core.ClaimLeaseForRepoProviderScopePond(leaseID, "legacy-claim", providerName, claimScope(api.BaseURL()), "", repoRoot, time.Hour, false); err != nil {
		t.Fatalf("write unbound claim: %v", err)
	}

	servers, err := b.List(context.Background(), ListRequest{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(servers) != 1 || servers[0].Labels["lease"] != "" {
		t.Fatalf("servers = %#v, want remote instance without ownership labels", servers)
	}
}

func TestStopDeletesClaimedInstance(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	api := &fakeUnikraftCloudAPI{
		baseURL:      "https://api.fra.unikraft.cloud",
		createResult: ukcInstance{UUID: testInstanceUUID, State: "running"},
	}
	var stderr bytes.Buffer
	b := testBackend(api, nil, &stderr)
	if err := b.Warmup(context.Background(), WarmupRequest{Repo: Repo{Root: t.TempDir(), Name: "demo"}, RequestedSlug: "my-ukc"}); err != nil {
		t.Fatalf("Warmup: %v", err)
	}
	if err := b.Stop(context.Background(), StopRequest{ID: "my-ukc"}); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if len(api.stoppedIDs) != 1 || api.stoppedIDs[0] != testInstanceUUID {
		t.Fatalf("stoppedIDs = %#v", api.stoppedIDs)
	}
	if len(api.deletedIDs) != 1 || api.deletedIDs[0] != testInstanceUUID {
		t.Fatalf("deletedIDs = %#v", api.deletedIDs)
	}
	if !strings.Contains(stderr.String(), "released lease="+leasePrefix+testInstanceUUID) {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if claim, err := readLeaseClaim(leasePrefix + testInstanceUUID); err == nil && claim.LeaseID != "" {
		t.Fatalf("claim = %#v, want removed", claim)
	}
}

func TestStopRequiresLocalClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	api := &fakeUnikraftCloudAPI{baseURL: "https://api.fra.unikraft.cloud"}
	b := testBackend(api, nil, nil)
	err := b.Stop(context.Background(), StopRequest{ID: testInstanceUUID})
	if err == nil {
		t.Fatal("Stop succeeded, want unclaimed error")
	}
	var exitErr ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 4 {
		t.Fatalf("err = %#v, want exit code 4", err)
	}
	if len(api.stoppedIDs) != 0 || len(api.deletedIDs) != 0 {
		t.Fatalf("Stop touched the API for an unclaimed instance: %#v", api)
	}
}

func TestStopRejectsClaimWithoutExactInstanceBinding(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	api := &fakeUnikraftCloudAPI{
		baseURL:      "https://api.fra.unikraft.cloud",
		createResult: ukcInstance{UUID: testInstanceUUID, State: "running"},
	}
	b := testBackend(api, nil, nil)
	repoRoot := t.TempDir()
	if err := b.Warmup(context.Background(), WarmupRequest{Repo: Repo{Root: repoRoot, Name: "demo"}}); err != nil {
		t.Fatalf("Warmup: %v", err)
	}
	leaseID := leasePrefix + testInstanceUUID
	core.RemoveLeaseClaim(leaseID)
	if err := core.ClaimLeaseForRepoProviderScopePond(leaseID, "legacy-claim", providerName, claimScope(api.BaseURL()), "", repoRoot, time.Hour, false); err != nil {
		t.Fatalf("write unbound claim: %v", err)
	}

	err := b.Stop(context.Background(), StopRequest{ID: leaseID})
	if err == nil || !strings.Contains(err.Error(), "no exact instance binding") {
		t.Fatalf("Stop err = %v, want exact instance binding rejection", err)
	}
	if len(api.stoppedIDs) != 0 || len(api.deletedIDs) != 0 {
		t.Fatalf("Stop touched API for unbound claim: %#v", api)
	}
}

func TestStopRemovesClaimWhenInstanceAlreadyGone(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	api := &fakeUnikraftCloudAPI{
		baseURL:      "https://api.fra.unikraft.cloud",
		createResult: ukcInstance{UUID: testInstanceUUID, State: "running"},
	}
	var stderr bytes.Buffer
	b := testBackend(api, nil, &stderr)
	if err := b.Warmup(context.Background(), WarmupRequest{Repo: Repo{Root: t.TempDir(), Name: "demo"}}); err != nil {
		t.Fatalf("Warmup: %v", err)
	}
	api.stopErr = notFoundErr()
	if err := b.Stop(context.Background(), StopRequest{ID: leasePrefix + testInstanceUUID}); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if len(api.deletedIDs) != 0 {
		t.Fatalf("deletedIDs = %#v, want no delete after 404 stop", api.deletedIDs)
	}
	if !strings.Contains(stderr.String(), "already gone") {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if claim, err := readLeaseClaim(leasePrefix + testInstanceUUID); err == nil && claim.LeaseID != "" {
		t.Fatalf("claim = %#v, want removed", claim)
	}
}

func TestStopPropagatesDeleteError(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	api := &fakeUnikraftCloudAPI{
		baseURL:      "https://api.fra.unikraft.cloud",
		createResult: ukcInstance{UUID: testInstanceUUID, State: "running"},
		deleteErr:    &unikraftCloudAPIError{StatusCode: http.StatusInternalServerError, Message: "backend unavailable"},
	}
	b := testBackend(api, nil, nil)
	if err := b.Warmup(context.Background(), WarmupRequest{Repo: Repo{Root: t.TempDir(), Name: "demo"}}); err != nil {
		t.Fatalf("Warmup: %v", err)
	}
	err := b.Stop(context.Background(), StopRequest{ID: leasePrefix + testInstanceUUID})
	if err == nil || !strings.Contains(err.Error(), "backend unavailable") {
		t.Fatalf("err = %v, want delete failure", err)
	}
	// The claim must survive so the user can retry stop.
	if claim, readErr := readLeaseClaim(leasePrefix + testInstanceUUID); readErr != nil || claim.LeaseID == "" {
		t.Fatalf("claim = %#v err = %v, want retained claim", claim, readErr)
	}
}

func TestStopRejectsClaimFromDifferentEndpoint(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	api := &fakeUnikraftCloudAPI{
		baseURL:      "https://api.fra.unikraft.cloud",
		createResult: ukcInstance{UUID: testInstanceUUID, State: "running"},
	}
	b := testBackend(api, nil, nil)
	if err := b.Warmup(context.Background(), WarmupRequest{Repo: Repo{Root: t.TempDir(), Name: "demo"}}); err != nil {
		t.Fatalf("Warmup: %v", err)
	}
	api.baseURL = "https://api.dal.unikraft.cloud"
	err := b.Stop(context.Background(), StopRequest{ID: leasePrefix + testInstanceUUID})
	if err == nil || !strings.Contains(err.Error(), "different API endpoint or metro") {
		t.Fatalf("err = %v, want scope mismatch", err)
	}
	if len(api.deletedIDs) != 0 {
		t.Fatalf("deletedIDs = %#v, want none", api.deletedIDs)
	}
}

func TestDoctorReportsInventory(t *testing.T) {
	for _, test := range []struct {
		name            string
		listErr         error
		wantErr         bool
		wantErrContains string
	}{
		{name: "ok"},
		{
			name:            "unauthorized",
			listErr:         &unikraftCloudAPIError{StatusCode: http.StatusUnauthorized, Message: "invalid token"},
			wantErr:         true,
			wantErrContains: "API key was rejected",
		},
		{
			name:            "other error",
			listErr:         &unikraftCloudAPIError{StatusCode: http.StatusInternalServerError, Message: "backend unavailable"},
			wantErr:         true,
			wantErrContains: "backend unavailable",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			api := &fakeUnikraftCloudAPI{
				baseURL:    "https://api.fra.unikraft.cloud",
				listErr:    test.listErr,
				listResult: []ukcInstance{{UUID: testInstanceUUID, State: "running"}},
			}
			b := testBackend(api, nil, nil)
			result, err := b.Doctor(context.Background(), DoctorRequest{})
			if test.wantErr {
				if err == nil || !strings.Contains(err.Error(), test.wantErrContains) {
					t.Fatalf("err = %v, want containing %q", err, test.wantErrContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("Doctor: %v", err)
			}
			if result.Provider != providerName || !strings.Contains(result.Message, "leases=1") {
				t.Fatalf("result = %#v", result)
			}
		})
	}
}
