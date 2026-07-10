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
const testUserUUID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

type fakeUnikraftCloudAPI struct {
	baseURL   string
	userUUID  string
	userErr   error
	userCalls int

	created               []createInstanceRequest
	createErr             error
	createResult          ukcInstance
	createBeforeError     bool
	preserveCreateName    bool
	onCreate              func(createInstanceRequest)
	getErr                error
	getResults            []ukcInstance
	getCalls              int
	listErr               error
	listResult            []ukcInstance
	deleteErr             error
	deletedIDs            []string
	deleted               map[string]bool
	retainAfterDelete     bool
	removeBeforeDeleteErr bool
}

func (f *fakeUnikraftCloudAPI) BaseURL() string { return f.baseURL }

func (f *fakeUnikraftCloudAPI) UserUUID(_ context.Context) (string, error) {
	f.userCalls++
	if f.userErr != nil {
		return "", f.userErr
	}
	if f.userUUID == "" {
		return testUserUUID, nil
	}
	return f.userUUID, nil
}

func (f *fakeUnikraftCloudAPI) CreateInstance(_ context.Context, req createInstanceRequest) (ukcInstance, error) {
	f.created = append(f.created, req)
	result := f.createResult
	if !f.preserveCreateName {
		result.Name = req.Name
	}
	f.createResult = result
	if f.onCreate != nil {
		f.onCreate(req)
	}
	if f.createErr != nil {
		if !f.createBeforeError {
			f.createResult = ukcInstance{}
		}
		return ukcInstance{}, f.createErr
	}
	return result, nil
}

func (f *fakeUnikraftCloudAPI) GetInstance(_ context.Context, id string) (ukcInstance, error) {
	f.getCalls++
	if f.getErr != nil {
		return ukcInstance{}, f.getErr
	}
	if f.deleted != nil && (f.deleted[id] || ((f.createResult.UUID == id || f.createResult.Name == id) && f.deleted[f.createResult.UUID])) {
		return ukcInstance{}, notFoundErr()
	}
	if len(f.getResults) == 0 {
		if f.createResult.UUID == id || f.createResult.Name == id {
			result := f.createResult
			if result.State == "" {
				result.State = "running"
			}
			return result, nil
		}
		return ukcInstance{}, notFoundErr()
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
	if f.listResult != nil {
		out := make([]ukcInstance, 0, len(f.listResult))
		for _, instance := range f.listResult {
			if f.deleted == nil || !f.deleted[instance.UUID] {
				out = append(out, instance)
			}
		}
		return out, nil
	}
	if f.createResult.UUID != "" && (f.deleted == nil || !f.deleted[f.createResult.UUID]) {
		return []ukcInstance{f.createResult}, nil
	}
	return nil, nil
}

func (f *fakeUnikraftCloudAPI) DeleteInstance(_ context.Context, id string) (ukcInstance, error) {
	f.deletedIDs = append(f.deletedIDs, id)
	if f.deleteErr != nil {
		if f.removeBeforeDeleteErr {
			if f.deleted == nil {
				f.deleted = make(map[string]bool)
			}
			f.deleted[id] = true
		}
		return ukcInstance{}, f.deleteErr
	}
	if !f.retainAfterDelete {
		if f.deleted == nil {
			f.deleted = make(map[string]bool)
		}
		f.deleted[id] = true
	}
	name := f.createResult.Name
	if name == "" {
		name = id
	}
	return ukcInstance{UUID: id, Name: name, State: "deleted", ItemStatus: "success"}, nil
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
		spec:                      Provider{}.Spec(),
		cfg:                       cfg,
		rt:                        Runtime{Stdout: stdout, Stderr: stderr},
		newClient:                 func(Config, Runtime) (unikraftCloudAPI, error) { return api, nil },
		pollInterval:              time.Millisecond,
		deleteConfirmationTimeout: 20 * time.Millisecond,
	}
}

func notFoundErr() error {
	return &unikraftCloudAPIError{StatusCode: http.StatusNotFound, Message: "instance not found"}
}

func testClaimScope(t *testing.T, baseURL string) string {
	t.Helper()
	scope, err := unikraftCloudClaimScope(baseURL, testUserUUID)
	if err != nil {
		t.Fatalf("claim scope: %v", err)
	}
	return scope
}

func onlyTestClaim(t *testing.T) LeaseClaim {
	t.Helper()
	claims, err := listUnikraftCloudLeaseClaims()
	if err != nil {
		t.Fatalf("list claims: %v", err)
	}
	if len(claims) != 1 {
		t.Fatalf("claims = %#v, want one", claims)
	}
	return claims[0]
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
	claim := onlyTestClaim(t)
	leaseID := claim.LeaseID
	for _, want := range []string{"leased " + leaseID, "provider=" + providerName, "instance=" + testInstanceUUID, "state=running", "fqdn=funky-town.fra.unikraft.app"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("stdout = %q, want %q", stdout.String(), want)
		}
	}
	if !strings.Contains(stderr.String(), "keeps the instance until explicit stop or eligible cleanup") {
		t.Fatalf("stderr = %q, want keep warning", stderr.String())
	}
	if claim.LeaseID != leaseID || claim.Provider != providerName || claim.CloudID != testInstanceUUID {
		t.Fatalf("claim = %#v", claim)
	}
	if claim.ProviderScope != testClaimScope(t, api.BaseURL()) {
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

func TestWarmupDoesNotCreateWhenIntentCannotBePersisted(t *testing.T) {
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
	if len(api.created) != 0 || len(api.deletedIDs) != 0 {
		t.Fatalf("provider mutations = created %#v deleted %#v, want none", api.created, api.deletedIDs)
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
	claim := onlyTestClaim(t)
	if view.ID != claim.LeaseID || view.Slug != "my-ukc" || view.ServerID != testInstanceUUID {
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
	api.listResult[0].Name = api.createResult.Name
	servers, err := b.List(context.Background(), ListRequest{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(servers) != 1 {
		t.Fatalf("servers = %#v", servers)
	}
	if servers[0].CloudID != testInstanceUUID || servers[0].Status != "running" {
		t.Fatalf("servers[0] = %#v", servers[0])
	}
	claim := onlyTestClaim(t)
	if servers[0].Labels["lease"] != claim.LeaseID || servers[0].Labels["slug"] != "my-ukc" {
		t.Fatalf("servers[0].Labels = %#v", servers[0].Labels)
	}
	all, err := b.List(context.Background(), ListRequest{All: true})
	if err != nil || len(all) != 2 || all[1].Labels["lease"] != "" {
		t.Fatalf("all = %#v err=%v, want claimed plus unclaimed", all, err)
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
	leaseID := onlyTestClaim(t).LeaseID
	core.RemoveLeaseClaim(leaseID)
	if err := core.ClaimLeaseForRepoProviderScopePond(leaseID, "legacy-claim", providerName, testClaimScope(t, api.BaseURL()), "", repoRoot, time.Hour, false); err != nil {
		t.Fatalf("write unbound claim: %v", err)
	}

	if _, err := b.List(context.Background(), ListRequest{}); err == nil || !strings.Contains(err.Error(), "ownership labels") {
		t.Fatalf("List err = %v, want invalid legacy claim rejection", err)
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
	claim := onlyTestClaim(t)
	if err := b.Stop(context.Background(), StopRequest{ID: "my-ukc"}); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if len(api.deletedIDs) != 1 || api.deletedIDs[0] != testInstanceUUID {
		t.Fatalf("deletedIDs = %#v", api.deletedIDs)
	}
	if !strings.Contains(stderr.String(), "released lease="+claim.LeaseID) {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if stored, err := readLeaseClaim(claim.LeaseID); err == nil && stored.LeaseID != "" {
		t.Fatalf("claim = %#v, want removed", stored)
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
	if len(api.deletedIDs) != 0 {
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
	leaseID := onlyTestClaim(t).LeaseID
	core.RemoveLeaseClaim(leaseID)
	if err := core.ClaimLeaseForRepoProviderScopePond(leaseID, "legacy-claim", providerName, testClaimScope(t, api.BaseURL()), "", repoRoot, time.Hour, false); err != nil {
		t.Fatalf("write unbound claim: %v", err)
	}

	err := b.Stop(context.Background(), StopRequest{ID: leaseID})
	if err == nil || !strings.Contains(err.Error(), "ownership labels") {
		t.Fatalf("Stop err = %v, want incomplete legacy claim rejection", err)
	}
	if len(api.deletedIDs) != 0 {
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
	claim := onlyTestClaim(t)
	api.deleted = map[string]bool{testInstanceUUID: true}
	if err := b.Stop(context.Background(), StopRequest{ID: claim.LeaseID}); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if len(api.deletedIDs) != 0 {
		t.Fatalf("deletedIDs = %#v, want no delete after 404 stop", api.deletedIDs)
	}
	if !strings.Contains(stderr.String(), "already gone") {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if stored, err := readLeaseClaim(claim.LeaseID); err == nil && stored.LeaseID != "" {
		t.Fatalf("claim = %#v, want removed", stored)
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
	claim := onlyTestClaim(t)
	err := b.Stop(context.Background(), StopRequest{ID: claim.LeaseID})
	if err == nil || !strings.Contains(err.Error(), "backend unavailable") {
		t.Fatalf("err = %v, want delete failure", err)
	}
	// The claim must survive so the user can retry stop.
	if stored, readErr := readLeaseClaim(claim.LeaseID); readErr != nil || stored.LeaseID == "" {
		t.Fatalf("claim = %#v err = %v, want retained claim", stored, readErr)
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
	claim := onlyTestClaim(t)
	api.baseURL = "https://api.dal.unikraft.cloud"
	err := b.Stop(context.Background(), StopRequest{ID: claim.LeaseID})
	if err == nil || !strings.Contains(err.Error(), "different API endpoint or account") {
		t.Fatalf("err = %v, want scope mismatch", err)
	}
	if len(api.deletedIDs) != 0 {
		t.Fatalf("deletedIDs = %#v, want none", api.deletedIDs)
	}
}

func TestWarmupPublishesCreateIntentBeforeProviderMutation(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	api := &fakeUnikraftCloudAPI{
		baseURL:      "https://api.fra.unikraft.cloud",
		createResult: ukcInstance{UUID: testInstanceUUID, State: "running"},
	}
	api.onCreate = func(req createInstanceRequest) {
		claims, err := listUnikraftCloudLeaseClaims()
		if err != nil || len(claims) != 1 {
			t.Fatalf("claims at create = %#v err=%v", claims, err)
		}
		intent := claims[0]
		if intent.CloudID != "" || intent.Labels["state"] != ukcStateCreateIntent || intent.Labels[ukcLabelResourceName] != req.Name {
			t.Fatalf("intent at create = %#v", intent)
		}
		if intent.Labels[ukcLabelAccountUUID] != testUserUUID || intent.Labels[ukcLabelRequestHash] == "" {
			t.Fatalf("intent labels at create = %#v", intent.Labels)
		}
	}
	b := testBackend(api, nil, nil)
	if err := b.Warmup(context.Background(), WarmupRequest{Repo: Repo{Root: t.TempDir(), Name: "demo"}}); err != nil {
		t.Fatalf("Warmup: %v", err)
	}
	if len(api.created) != 1 || !strings.HasPrefix(api.created[0].Name, "crabbox-ukc-") {
		t.Fatalf("create requests = %#v", api.created)
	}
}

func TestWarmupReconcilesAmbiguousCreateWithoutSecondPost(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	api := &fakeUnikraftCloudAPI{
		baseURL:           "https://api.fra.unikraft.cloud",
		createResult:      ukcInstance{UUID: testInstanceUUID, State: "running"},
		createErr:         errors.New("connection reset after request"),
		createBeforeError: true,
	}
	b := testBackend(api, nil, nil)
	if err := b.Warmup(context.Background(), WarmupRequest{Repo: Repo{Root: t.TempDir(), Name: "demo"}}); err != nil {
		t.Fatalf("Warmup: %v", err)
	}
	if len(api.created) != 1 {
		t.Fatalf("created = %#v, want exactly one POST", api.created)
	}
	claim := onlyTestClaim(t)
	if claim.CloudID != testInstanceUUID || claim.Labels["state"] != ukcStateReady {
		t.Fatalf("claim = %#v", claim)
	}
}

func TestWarmupRetainsIntentWhenCreateOutcomeIsAmbiguous(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	api := &fakeUnikraftCloudAPI{
		baseURL:   "https://api.fra.unikraft.cloud",
		createErr: errors.New("connection reset after request"),
	}
	b := testBackend(api, nil, nil)
	err := b.Warmup(context.Background(), WarmupRequest{Repo: Repo{Root: t.TempDir(), Name: "demo"}})
	if err == nil || !strings.Contains(err.Error(), "recovery claim") {
		t.Fatalf("Warmup err = %v", err)
	}
	if len(api.created) != 1 {
		t.Fatalf("created = %#v, want one POST", api.created)
	}
	claim := onlyTestClaim(t)
	if claim.CloudID != "" || claim.Labels["state"] != ukcStateCreateIntent {
		t.Fatalf("claim = %#v, want retained intent", claim)
	}
}

func TestWarmupRejectsNegativeMemoryBeforeProviderAccess(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	api := &fakeUnikraftCloudAPI{baseURL: "https://api.fra.unikraft.cloud"}
	b := testBackend(api, nil, nil)
	b.cfg.UnikraftCloud.MemoryMB = -1
	err := b.Warmup(context.Background(), WarmupRequest{Repo: Repo{Root: t.TempDir(), Name: "demo"}})
	if err == nil || !strings.Contains(err.Error(), "memory must be zero or greater") {
		t.Fatalf("Warmup err = %v", err)
	}
	if api.userCalls != 0 || len(api.created) != 0 {
		t.Fatalf("provider accessed before validation: userCalls=%d created=%#v", api.userCalls, api.created)
	}
}

func TestStopRejectsClaimFromDifferentAccount(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	api := &fakeUnikraftCloudAPI{
		baseURL:      "https://api.fra.unikraft.cloud",
		userUUID:     testUserUUID,
		createResult: ukcInstance{UUID: testInstanceUUID, State: "running"},
	}
	b := testBackend(api, nil, nil)
	if err := b.Warmup(context.Background(), WarmupRequest{Repo: Repo{Root: t.TempDir(), Name: "demo"}}); err != nil {
		t.Fatalf("Warmup: %v", err)
	}
	claim := onlyTestClaim(t)
	api.userUUID = "bbbbbbbb-cccc-dddd-eeee-ffffffffffff"
	err := b.Stop(context.Background(), StopRequest{ID: claim.LeaseID})
	if err == nil || !strings.Contains(err.Error(), "different API endpoint or account") {
		t.Fatalf("Stop err = %v", err)
	}
	if len(api.deletedIDs) != 0 {
		t.Fatalf("deletedIDs = %#v, want none", api.deletedIDs)
	}
	if stored, exists, readErr := readLeaseClaimWithPresence(claim.LeaseID); readErr != nil || !exists || stored.CloudID != testInstanceUUID {
		t.Fatalf("stored claim = %#v exists=%v err=%v", stored, exists, readErr)
	}
}

func TestStopRetainsAcceptedClaimUntilStrongAbsenceProof(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	api := &fakeUnikraftCloudAPI{
		baseURL:           "https://api.fra.unikraft.cloud",
		createResult:      ukcInstance{UUID: testInstanceUUID, State: "running"},
		retainAfterDelete: true,
	}
	b := testBackend(api, nil, nil)
	if err := b.Warmup(context.Background(), WarmupRequest{Repo: Repo{Root: t.TempDir(), Name: "demo"}}); err != nil {
		t.Fatalf("Warmup: %v", err)
	}
	claim := onlyTestClaim(t)
	if err := b.Stop(context.Background(), StopRequest{ID: claim.LeaseID}); err == nil || !strings.Contains(err.Error(), "absence is unconfirmed") {
		t.Fatalf("first Stop err = %v", err)
	}
	accepted := onlyTestClaim(t)
	if accepted.Labels["state"] != ukcStateDeleteAccepted || len(api.deletedIDs) != 1 {
		t.Fatalf("accepted = %#v deleted=%#v", accepted, api.deletedIDs)
	}
	if err := b.Stop(context.Background(), StopRequest{ID: claim.LeaseID}); err == nil {
		t.Fatal("second Stop succeeded while instance remains visible")
	}
	if len(api.deletedIDs) != 1 {
		t.Fatalf("accepted delete replayed: %#v", api.deletedIDs)
	}
	api.deleted = map[string]bool{testInstanceUUID: true}
	if err := b.Stop(context.Background(), StopRequest{ID: claim.LeaseID}); err != nil {
		t.Fatalf("final Stop: %v", err)
	}
	if _, exists, err := readLeaseClaimWithPresence(claim.LeaseID); err != nil || exists {
		t.Fatalf("claim exists=%v err=%v, want removed", exists, err)
	}
}

func TestStopDoesNotTrustNotFoundWhenInventoryStillContainsInstance(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	api := &fakeUnikraftCloudAPI{
		baseURL:      "https://api.fra.unikraft.cloud",
		createResult: ukcInstance{UUID: testInstanceUUID, State: "running"},
	}
	b := testBackend(api, nil, nil)
	if err := b.Warmup(context.Background(), WarmupRequest{Repo: Repo{Root: t.TempDir(), Name: "demo"}}); err != nil {
		t.Fatalf("Warmup: %v", err)
	}
	claim := onlyTestClaim(t)
	api.listResult = []ukcInstance{api.createResult}
	api.getErr = notFoundErr()
	err := b.Stop(context.Background(), StopRequest{ID: claim.LeaseID})
	if err == nil {
		t.Fatal("Stop succeeded on contradictory absence evidence")
	}
	if len(api.deletedIDs) != 0 {
		t.Fatalf("deletedIDs = %#v, want none", api.deletedIDs)
	}
	if _, exists, readErr := readLeaseClaimWithPresence(claim.LeaseID); readErr != nil || !exists {
		t.Fatalf("claim exists=%v err=%v, want retained", exists, readErr)
	}
}

func TestStopReconcilesAmbiguousDeleteAfterStrongAbsenceProof(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	api := &fakeUnikraftCloudAPI{
		baseURL:               "https://api.fra.unikraft.cloud",
		createResult:          ukcInstance{UUID: testInstanceUUID, State: "running"},
		deleteErr:             errors.New("connection reset after delete"),
		removeBeforeDeleteErr: true,
	}
	b := testBackend(api, nil, nil)
	if err := b.Warmup(context.Background(), WarmupRequest{Repo: Repo{Root: t.TempDir(), Name: "demo"}}); err != nil {
		t.Fatalf("Warmup: %v", err)
	}
	claim := onlyTestClaim(t)
	if err := b.Stop(context.Background(), StopRequest{ID: claim.LeaseID}); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if len(api.deletedIDs) != 1 {
		t.Fatalf("deletedIDs = %#v", api.deletedIDs)
	}
	if _, exists, err := readLeaseClaimWithPresence(claim.LeaseID); err != nil || exists {
		t.Fatalf("claim exists=%v err=%v, want removed", exists, err)
	}
}

func TestConcurrentWarmupsReserveDistinctRequestedSlugs(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	apis := []*fakeUnikraftCloudAPI{
		{baseURL: "https://api.fra.unikraft.cloud", createResult: ukcInstance{UUID: testInstanceUUID, State: "running"}},
		{baseURL: "https://api.fra.unikraft.cloud", createResult: ukcInstance{UUID: "66666666-7777-8888-9999-000000000000", State: "running"}},
	}
	errCh := make(chan error, len(apis))
	for _, api := range apis {
		api := api
		go func() {
			b := testBackend(api, nil, nil)
			errCh <- b.Warmup(context.Background(), WarmupRequest{Repo: Repo{Root: t.TempDir(), Name: "demo"}, RequestedSlug: "same-slug"})
		}()
	}
	for range apis {
		if err := <-errCh; err != nil {
			t.Fatalf("Warmup: %v", err)
		}
	}
	claims, err := listUnikraftCloudLeaseClaims()
	if err != nil || len(claims) != 2 {
		t.Fatalf("claims = %#v err=%v", claims, err)
	}
	if claims[0].Slug == claims[1].Slug {
		t.Fatalf("duplicate slugs: %#v", claims)
	}
}

func TestStatusWaitFailsImmediatelyOnTerminalState(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	api := &fakeUnikraftCloudAPI{
		baseURL:    "https://api.fra.unikraft.cloud",
		getResults: []ukcInstance{{UUID: testInstanceUUID, Name: testInstanceUUID, State: "stopped"}},
	}
	b := testBackend(api, nil, nil)
	started := time.Now()
	_, err := b.Status(context.Background(), StatusRequest{ID: testInstanceUUID, Wait: true, WaitTimeout: time.Second})
	if err == nil || !strings.Contains(err.Error(), "terminal state=stopped") {
		t.Fatalf("Status err = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 200*time.Millisecond {
		t.Fatalf("terminal status took %s", elapsed)
	}
}

func TestCleanupResumesAcceptedDeletionWithoutReissuingDelete(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	api := &fakeUnikraftCloudAPI{
		baseURL:           "https://api.fra.unikraft.cloud",
		createResult:      ukcInstance{UUID: testInstanceUUID, State: "running"},
		retainAfterDelete: true,
	}
	b := testBackend(api, nil, nil)
	if err := b.Warmup(context.Background(), WarmupRequest{Repo: Repo{Root: t.TempDir(), Name: "demo"}}); err != nil {
		t.Fatalf("Warmup: %v", err)
	}
	claim := onlyTestClaim(t)
	if err := b.Stop(context.Background(), StopRequest{ID: claim.LeaseID}); err == nil {
		t.Fatal("Stop succeeded without absence proof")
	}
	api.deleted = map[string]bool{testInstanceUUID: true}
	if err := b.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if len(api.deletedIDs) != 1 {
		t.Fatalf("accepted delete replayed: %#v", api.deletedIDs)
	}
	if _, exists, err := readLeaseClaimWithPresence(claim.LeaseID); err != nil || exists {
		t.Fatalf("claim exists=%v err=%v, want removed", exists, err)
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
