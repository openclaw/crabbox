package railway

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRailwayProviderSpec(t *testing.T) {
	spec := Provider{}.Spec()
	if spec.Name != providerName {
		t.Fatalf("spec.Name = %q, want %q", spec.Name, providerName)
	}
	if spec.Kind != "delegated-run" {
		t.Fatalf("spec.Kind = %q, want delegated-run", spec.Kind)
	}
	aliases := Provider{}.Aliases()
	if len(aliases) != 2 || aliases[0] != "rail" || aliases[1] != "railwayapp" {
		t.Fatalf("aliases = %#v, want [rail railwayapp]", aliases)
	}
}

func TestRailwayClientRequiresAPIToken(t *testing.T) {
	cfg := Config{}
	cfg.Railway.APIURL = "https://backboard.railway.com/graphql/v2"
	if _, err := newRailwayClient(cfg, Runtime{}); err == nil {
		t.Fatal("newRailwayClient accepted empty API token")
	}
}

func TestRailwayClientRejectsBareHTTPURL(t *testing.T) {
	cfg := Config{}
	cfg.Railway.APIToken = "test-token"
	cfg.Railway.APIURL = "http://backboard.railway.com/graphql/v2"
	if _, err := newRailwayClient(cfg, Runtime{}); err == nil {
		t.Fatal("newRailwayClient accepted plaintext http URL")
	}
}

func TestRailwayTokenFlagIsNotRegistered(t *testing.T) {
	cfg := Config{}
	cfg.Railway.APIToken = "secret-token"
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	RegisterRailwayProviderFlags(fs, cfg)
	for _, name := range []string{"railway-token", "railway-api-token", "railway-key", "railway-api-key"} {
		if fs.Lookup(name) != nil {
			t.Fatalf("railway API token surfaced as a flag --%s", name)
		}
	}
	if fs.Lookup("railway-url") == nil {
		t.Fatal("railway-url flag missing")
	}
	if fs.Lookup("railway-project") == nil {
		t.Fatal("railway-project flag missing")
	}
	if fs.Lookup("railway-environment") == nil {
		t.Fatal("railway-environment flag missing")
	}
}

func TestRailwayClientSendsBearerAndGraphQLBody(t *testing.T) {
	var (
		gotAuth        string
		gotContentType string
		gotMethod      string
		gotQuery       string
		gotVariables   map[string]any
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		gotMethod = r.Method
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.Unmarshal(body, &payload)
		gotQuery = payload.Query
		gotVariables = payload.Variables
		_, _ = io.WriteString(w, `{"data":{"environmentTriggersDeploy":"dep-1"}}`)
	}))
	defer server.Close()

	cfg := Config{}
	cfg.Railway.APIToken = "test-token"
	cfg.Railway.APIURL = server.URL
	client, err := newRailwayClient(cfg, Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	deployID, err := client.TriggerDeploy(context.Background(), "proj-1", "env-1", "svc-1")
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("method = %q, want POST", gotMethod)
	}
	if gotAuth != "Bearer test-token" {
		t.Fatalf("auth header = %q, want Bearer test-token", gotAuth)
	}
	if gotContentType != "application/json" {
		t.Fatalf("content-type = %q, want application/json", gotContentType)
	}
	if !strings.Contains(gotQuery, "environmentTriggersDeploy") {
		t.Fatalf("query missing environmentTriggersDeploy mutation: %s", gotQuery)
	}
	input, _ := gotVariables["input"].(map[string]any)
	if input["projectId"] != "proj-1" || input["environmentId"] != "env-1" || input["serviceId"] != "svc-1" {
		t.Fatalf("variables = %#v, want proj-1/env-1/svc-1", gotVariables)
	}
	if deployID != "dep-1" {
		t.Fatalf("deployID = %q, want dep-1", deployID)
	}
}

func TestRailwayClientSurfacesNon2xxAsAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden by token", http.StatusForbidden)
	}))
	defer server.Close()

	cfg := Config{}
	cfg.Railway.APIToken = "wrong-token"
	cfg.Railway.APIURL = server.URL
	client, err := newRailwayClient(cfg, Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.TriggerDeploy(context.Background(), "p", "e", "s")
	if err == nil {
		t.Fatal("TriggerDeploy accepted 403 response")
	}
	apiErr, ok := err.(*railwayAPIError)
	if !ok {
		t.Fatalf("err = %T, want *railwayAPIError", err)
	}
	if apiErr.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", apiErr.StatusCode)
	}
	if !strings.Contains(apiErr.Body, "forbidden by token") {
		t.Fatalf("body = %q, want forbidden snippet", apiErr.Body)
	}
}

func TestRailwayClientSurfacesGraphQLErrorsAsAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"errors":[{"message":"Project not found"}]}`)
	}))
	defer server.Close()

	cfg := Config{}
	cfg.Railway.APIToken = "test-token"
	cfg.Railway.APIURL = server.URL
	client, err := newRailwayClient(cfg, Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.TriggerDeploy(context.Background(), "p", "e", "s")
	if err == nil {
		t.Fatal("TriggerDeploy accepted GraphQL error envelope")
	}
	apiErr, ok := err.(*railwayAPIError)
	if !ok {
		t.Fatalf("err = %T, want *railwayAPIError", err)
	}
	if !strings.Contains(apiErr.Body, "Project not found") {
		t.Fatalf("err body = %q, want Project not found", apiErr.Body)
	}
}

func TestRailwayRunRequiresNoSync(t *testing.T) {
	backend := &railwayBackend{rt: Runtime{Stdout: io.Discard, Stderr: io.Discard}}
	_, err := backend.Run(context.Background(), RunRequest{ID: "svc-1", Command: []string{"pnpm", "test"}})
	if err == nil {
		t.Fatal("Run accepted request without --no-sync")
	}
	if !strings.Contains(err.Error(), "--no-sync") {
		t.Fatalf("err = %v, want --no-sync hint", err)
	}
}

func TestRailwayRunRequiresServiceID(t *testing.T) {
	backend := &railwayBackend{rt: Runtime{Stdout: io.Discard, Stderr: io.Discard}}
	_, err := backend.Run(context.Background(), RunRequest{NoSync: true, Command: []string{"pnpm", "test"}})
	if err == nil || !strings.Contains(err.Error(), "--id") {
		t.Fatalf("err = %v, want --id rejection", err)
	}
}

func TestRailwayRunRequiresProjectAndEnvironment(t *testing.T) {
	backend := &railwayBackend{rt: Runtime{Stdout: io.Discard, Stderr: io.Discard}}
	_, err := backend.Run(context.Background(), RunRequest{NoSync: true, ID: "svc-1", Command: []string{"pnpm", "test"}})
	if err == nil || !strings.Contains(err.Error(), "--railway-project") {
		t.Fatalf("err = %v, want --railway-project rejection", err)
	}
	cfg := Config{}
	cfg.Railway.ProjectID = "proj-1"
	backend2 := &railwayBackend{cfg: cfg, rt: Runtime{Stdout: io.Discard, Stderr: io.Discard}}
	_, err = backend2.Run(context.Background(), RunRequest{NoSync: true, ID: "svc-1", Command: []string{"pnpm", "test"}})
	if err == nil || !strings.Contains(err.Error(), "--railway-environment") {
		t.Fatalf("err = %v, want --railway-environment rejection", err)
	}
}

func TestRailwayRunRejectsLeaseFlags(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  RunRequest
		want string
	}{
		{name: "keep", req: RunRequest{ID: "svc-1", Keep: true, NoSync: true, Command: []string{"pnpm", "test"}}, want: "--keep"},
		{name: "reclaim", req: RunRequest{ID: "svc-1", Reclaim: true, NoSync: true, Command: []string{"pnpm", "test"}}, want: "--reclaim"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := &railwayBackend{rt: Runtime{Stdout: io.Discard, Stderr: io.Discard}}
			_, err := backend.Run(context.Background(), tc.req)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %s rejection", err, tc.want)
			}
		})
	}
}

type fakeRailwayAPI struct {
	triggerProjectID     string
	triggerEnvironmentID string
	triggerServiceID     string
	deployID             string
	logs                 []string
	deployment           railwayDeployment
	services             []railwayService
	service              railwayService
	stopID               string
	triggerErr           error
	logsErr              error
	listErr              error
}

func (f *fakeRailwayAPI) TriggerDeploy(_ context.Context, projectID, environmentID, serviceID string) (string, error) {
	f.triggerProjectID = projectID
	f.triggerEnvironmentID = environmentID
	f.triggerServiceID = serviceID
	return f.deployID, f.triggerErr
}

func (f *fakeRailwayAPI) DeploymentLogs(_ context.Context, _ string, _ int) ([]string, error) {
	return f.logs, f.logsErr
}

func (f *fakeRailwayAPI) LatestDeployment(_ context.Context, _, _, _ string) (railwayDeployment, error) {
	return f.deployment, nil
}

func (f *fakeRailwayAPI) StopDeployment(_ context.Context, id string) error {
	f.stopID = id
	return nil
}

func (f *fakeRailwayAPI) ListServices(_ context.Context) ([]railwayService, error) {
	return f.services, f.listErr
}

func (f *fakeRailwayAPI) GetService(_ context.Context, _ string) (railwayService, error) {
	return f.service, nil
}

func TestRailwayRunHappyPath(t *testing.T) {
	api := &fakeRailwayAPI{
		deployID:   "dep-1",
		deployment: railwayDeployment{ID: "dep-1", Status: "SUCCESS"},
		logs:       []string{"+ pnpm test", "PASS suite (1.2s)"},
	}
	cfg := Config{Provider: providerName}
	cfg.Railway.APIToken = "test-token"
	cfg.Railway.APIURL = "https://backboard.railway.com/graphql/v2"
	cfg.Railway.ProjectID = "proj-1"
	cfg.Railway.EnvironmentID = "env-1"
	rt := Runtime{Stdout: io.Discard, Stderr: io.Discard}
	backend := &railwayBackend{spec: Provider{}.Spec(), cfg: cfg, rt: rt, client: api}
	result, err := backend.Run(context.Background(), RunRequest{ID: "svc-1", NoSync: true, Command: []string{"pnpm", "test"}})
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", result.ExitCode)
	}
	if api.triggerServiceID != "svc-1" || api.triggerProjectID != "proj-1" || api.triggerEnvironmentID != "env-1" {
		t.Fatalf("trigger called with svc=%q proj=%q env=%q", api.triggerServiceID, api.triggerProjectID, api.triggerEnvironmentID)
	}
}

func TestRailwayRunFailedDeploymentMapsToExit1(t *testing.T) {
	api := &fakeRailwayAPI{
		deployID:   "dep-1",
		deployment: railwayDeployment{ID: "dep-1", Status: "FAILED"},
	}
	cfg := Config{Provider: providerName}
	cfg.Railway.APIToken = "test-token"
	cfg.Railway.APIURL = "https://backboard.railway.com/graphql/v2"
	cfg.Railway.ProjectID = "proj-1"
	cfg.Railway.EnvironmentID = "env-1"
	rt := Runtime{Stdout: io.Discard, Stderr: io.Discard}
	backend := &railwayBackend{spec: Provider{}.Spec(), cfg: cfg, rt: rt, client: api}
	result, err := backend.Run(context.Background(), RunRequest{ID: "svc-1", NoSync: true, Command: []string{"pnpm", "test"}})
	if err == nil {
		t.Fatal("Run accepted FAILED deployment status")
	}
	if result.ExitCode != 1 {
		t.Fatalf("exit code = %d, want 1", result.ExitCode)
	}
}

func TestRailwayWarmupRejected(t *testing.T) {
	backend := &railwayBackend{rt: Runtime{Stdout: io.Discard, Stderr: io.Discard}}
	err := backend.Warmup(context.Background(), WarmupRequest{})
	if err == nil || !strings.Contains(err.Error(), "warmup") {
		t.Fatalf("Warmup err = %v, want warmup rejection", err)
	}
}

func TestRailwayStopRequiresID(t *testing.T) {
	backend := &railwayBackend{rt: Runtime{Stdout: io.Discard, Stderr: io.Discard}}
	if err := backend.Stop(context.Background(), StopRequest{}); err == nil {
		t.Fatal("Stop accepted empty service id")
	}
}

func TestRailwayStopCallsDeploymentStop(t *testing.T) {
	api := &fakeRailwayAPI{deployment: railwayDeployment{ID: "dep-1", Status: "BUILDING"}}
	cfg := Config{Provider: providerName}
	cfg.Railway.APIToken = "test-token"
	cfg.Railway.APIURL = "https://backboard.railway.com/graphql/v2"
	cfg.Railway.ProjectID = "proj-1"
	cfg.Railway.EnvironmentID = "env-1"
	backend := &railwayBackend{cfg: cfg, rt: Runtime{Stdout: io.Discard, Stderr: io.Discard}, client: api}
	if err := backend.Stop(context.Background(), StopRequest{ID: "svc-1"}); err != nil {
		t.Fatalf("Stop err: %v", err)
	}
	if api.stopID != "dep-1" {
		t.Fatalf("stop called with id=%q, want dep-1", api.stopID)
	}
}

func TestRailwayStatusReturnsView(t *testing.T) {
	api := &fakeRailwayAPI{
		service:    railwayService{ID: "svc-1", Name: "api", ProjectID: "proj-1"},
		deployment: railwayDeployment{ID: "dep-1", Status: "SUCCESS"},
	}
	cfg := Config{Provider: providerName}
	cfg.Railway.APIToken = "test-token"
	cfg.Railway.APIURL = "https://backboard.railway.com/graphql/v2"
	cfg.Railway.ProjectID = "proj-1"
	cfg.Railway.EnvironmentID = "env-1"
	backend := &railwayBackend{cfg: cfg, rt: Runtime{Stdout: io.Discard, Stderr: io.Discard}, client: api}
	view, err := backend.Status(context.Background(), StatusRequest{ID: "svc-1"})
	if err != nil {
		t.Fatalf("Status err: %v", err)
	}
	if view.ID != "svc-1" || view.Slug != "api" || !view.Ready {
		t.Fatalf("view = %#v, want svc-1/api/ready", view)
	}
	if view.Provider != providerName {
		t.Fatalf("view.Provider = %q, want %q", view.Provider, providerName)
	}
}

func TestRailwayListEnumeratesServices(t *testing.T) {
	api := &fakeRailwayAPI{services: []railwayService{
		{ID: "svc-1", Name: "api", ProjectID: "proj-1"},
		{ID: "svc-2", Name: "worker", ProjectID: "proj-1"},
	}}
	cfg := Config{Provider: providerName}
	cfg.Railway.APIToken = "test-token"
	cfg.Railway.APIURL = "https://backboard.railway.com/graphql/v2"
	backend := &railwayBackend{cfg: cfg, rt: Runtime{Stdout: io.Discard, Stderr: io.Discard}, client: api}
	servers, err := backend.List(context.Background(), ListRequest{})
	if err != nil {
		t.Fatalf("List err: %v", err)
	}
	if len(servers) != 2 {
		t.Fatalf("List len = %d, want 2", len(servers))
	}
	if servers[0].CloudID != "svc-1" || servers[1].Name != "worker" {
		t.Fatalf("List = %#v", servers)
	}
}

func TestRailwayFlagsApply(t *testing.T) {
	cfg := Config{Provider: providerName}
	cfg.Railway.APIURL = "https://backboard.railway.com/graphql/v2"
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	values := RegisterRailwayProviderFlags(fs, cfg)
	if err := fs.Parse([]string{"--railway-url", "https://example.com/graphql/v2", "--railway-project", "proj-x", "--railway-environment", "env-x"}); err != nil {
		t.Fatal(err)
	}
	if err := ApplyRailwayProviderFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.Railway.APIURL != "https://example.com/graphql/v2" {
		t.Fatalf("APIURL = %q", cfg.Railway.APIURL)
	}
	if cfg.Railway.ProjectID != "proj-x" || cfg.Railway.EnvironmentID != "env-x" {
		t.Fatalf("project=%q env=%q", cfg.Railway.ProjectID, cfg.Railway.EnvironmentID)
	}
}
