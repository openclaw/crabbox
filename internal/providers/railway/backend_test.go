package railway

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRailwayProviderSpec(t *testing.T) {
	spec := Provider{}.Spec()
	if spec.Name != providerName {
		t.Fatalf("spec.Name = %q, want %q", spec.Name, providerName)
	}
	if spec.Kind != "service-control" {
		t.Fatalf("spec.Kind = %q, want service-control", spec.Kind)
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
		gotQueries     []string
		gotVariables   []map[string]any
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
		gotQueries = append(gotQueries, payload.Query)
		gotVariables = append(gotVariables, payload.Variables)
		if strings.Contains(payload.Query, "deployments") {
			_, _ = io.WriteString(w, `{"data":{"deployments":{"edges":[{"node":{"id":"dep-old","status":"SUCCESS","url":"","createdAt":"2026-05-18T12:00:00Z"}}]}}}`)
			return
		}
		_, _ = io.WriteString(w, `{"data":{"deploymentRedeploy":{"id":"dep-1","status":"QUEUED","url":"","createdAt":"2026-05-18T12:01:00Z"}}}`)
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
	if len(gotQueries) != 2 {
		t.Fatalf("queries len=%d, want latest query + redeploy mutation", len(gotQueries))
	}
	if !strings.Contains(gotQueries[0], "deployments") || !strings.Contains(gotQueries[1], "deploymentRedeploy") {
		t.Fatalf("queries = %#v, want latest deployment then deploymentRedeploy", gotQueries)
	}
	input, _ := gotVariables[0]["input"].(map[string]any)
	if input["projectId"] != "proj-1" || input["environmentId"] != "env-1" || input["serviceId"] != "svc-1" {
		t.Fatalf("latest variables = %#v, want proj-1/env-1/svc-1", gotVariables[0])
	}
	if gotVariables[1]["id"] != "dep-old" || gotVariables[1]["usePreviousImageTag"] != true {
		t.Fatalf("redeploy variables = %#v, want dep-old/usePreviousImageTag", gotVariables[1])
	}
	if deployID != "dep-1" {
		t.Fatalf("deployID = %q, want dep-1", deployID)
	}
}

func TestRailwayClientRequiresLatestDeploymentBeforeRedeploy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":{"deployments":{"edges":[]}}}`)
	}))
	defer server.Close()

	cfg := Config{}
	cfg.Railway.APIToken = "test-token"
	cfg.Railway.APIURL = server.URL
	client, err := newRailwayClient(cfg, Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.TriggerDeploy(context.Background(), "proj-1", "env-1", "svc-1")
	if err == nil || !strings.Contains(err.Error(), "latest deployment not found") {
		t.Fatalf("err = %v, want latest deployment error", err)
	}
}

func TestRailwayClientRejectsEmptyRedeployResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "deployments") {
			_, _ = io.WriteString(w, `{"data":{"deployments":{"edges":[{"node":{"id":"dep-old","status":"SUCCESS","url":"","createdAt":"2026-05-18T12:00:00Z"}}]}}}`)
			return
		}
		_, _ = io.WriteString(w, `{"data":{"deploymentRedeploy":{"id":"","status":"QUEUED","url":"","createdAt":"2026-05-18T12:01:00Z"}}}`)
	}))
	defer server.Close()

	cfg := Config{}
	cfg.Railway.APIToken = "test-token"
	cfg.Railway.APIURL = server.URL
	client, err := newRailwayClient(cfg, Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.TriggerDeploy(context.Background(), "proj-1", "env-1", "svc-1")
	if err == nil || !strings.Contains(err.Error(), "deploymentRedeploy returned empty deployment id") {
		t.Fatalf("err = %v, want empty redeploy id error", err)
	}
}

func TestRailwayClientSurfacesNon2xxAsAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden by wrong-token", http.StatusForbidden)
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
	if strings.Contains(apiErr.Body, "wrong-token") || !strings.Contains(apiErr.Body, "forbidden by [redacted]") {
		t.Fatalf("body = %q, want redacted forbidden snippet", apiErr.Body)
	}
}

func TestRailwayClientSurfacesGraphQLErrorsAsAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"errors":[{"message":"Bearer test-token Project not found"}]}`)
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
	if strings.Contains(apiErr.Body, "test-token") || !strings.Contains(apiErr.Body, "Bearer [redacted] Project not found") {
		t.Fatalf("err body = %q, want redacted Project not found", apiErr.Body)
	}
}

func TestRailwayClientListServicesPaginatesProjectsAndServices(t *testing.T) {
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		first, _ := payload.Variables["first"].(float64)
		if int(first) != railwayListServicesPageSize {
			http.Error(w, fmt.Sprintf("first = %v", payload.Variables["first"]), http.StatusBadRequest)
			return
		}
		switch {
		case strings.Contains(payload.Query, "project(id:"):
			calls = append(calls, "project-services")
			if payload.Variables["projectId"] != "proj-1" || payload.Variables["after"] != "svc-cursor-1" {
				http.Error(w, fmt.Sprintf("service vars = %#v", payload.Variables), http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"project": map[string]any{
						"services": serviceConnection(false, "", serviceNode("svc-2", "worker")),
					},
				},
			})
		case strings.Contains(payload.Query, "projects("):
			after, _ := payload.Variables["after"].(string)
			calls = append(calls, "projects:"+after)
			switch after {
			case "":
				if serviceFirst, _ := payload.Variables["serviceFirst"].(float64); int(serviceFirst) != railwayListServicesPageSize {
					http.Error(w, fmt.Sprintf("serviceFirst = %v", payload.Variables["serviceFirst"]), http.StatusBadRequest)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"data": map[string]any{
						"projects": map[string]any{
							"pageInfo": pageInfo(true, "proj-cursor-1"),
							"edges": []map[string]any{{
								"node": map[string]any{
									"id":       "proj-1",
									"name":     "api-project",
									"services": serviceConnection(true, "svc-cursor-1", serviceNode("svc-1", "api")),
								},
							}},
						},
					},
				})
			case "proj-cursor-1":
				_ = json.NewEncoder(w).Encode(map[string]any{
					"data": map[string]any{
						"projects": map[string]any{
							"pageInfo": pageInfo(false, ""),
							"edges": []map[string]any{{
								"node": map[string]any{
									"id":       "proj-2",
									"name":     "jobs-project",
									"services": serviceConnection(false, "", serviceNode("svc-3", "jobs")),
								},
							}},
						},
					},
				})
			default:
				http.Error(w, "unexpected project cursor "+after, http.StatusBadRequest)
			}
		default:
			http.Error(w, "unexpected query", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	cfg := Config{}
	cfg.Railway.APIToken = "test-token"
	cfg.Railway.APIURL = server.URL
	client, err := newRailwayClient(cfg, Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	services, err := client.ListServices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := calls, []string{"projects:", "project-services", "projects:proj-cursor-1"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("calls = %#v, want %#v", got, want)
	}
	want := map[string]railwayService{
		"svc-1": {ID: "svc-1", Name: "api", ProjectID: "proj-1"},
		"svc-2": {ID: "svc-2", Name: "worker", ProjectID: "proj-1"},
		"svc-3": {ID: "svc-3", Name: "jobs", ProjectID: "proj-2"},
	}
	if len(services) != len(want) {
		t.Fatalf("services = %#v, want %d services", services, len(want))
	}
	for _, service := range services {
		if want[service.ID] != service {
			t.Fatalf("service %q = %#v, want %#v", service.ID, service, want[service.ID])
		}
		delete(want, service.ID)
	}
}

func pageInfo(hasNext bool, endCursor string) map[string]any {
	return map[string]any{"hasNextPage": hasNext, "endCursor": endCursor}
}

func serviceConnection(hasNext bool, endCursor string, services ...map[string]any) map[string]any {
	edges := make([]map[string]any, 0, len(services))
	for _, service := range services {
		edges = append(edges, map[string]any{"node": service})
	}
	return map[string]any{
		"pageInfo": pageInfo(hasNext, endCursor),
		"edges":    edges,
	}
}

func serviceNode(id, name string) map[string]any {
	return map[string]any{"id": id, "name": name}
}

func TestRailwayClientDecodesLargeLogResponse(t *testing.T) {
	message := strings.Repeat("x", 2<<20)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"deploymentLogs": []map[string]string{{"message": message}},
			},
		})
	}))
	defer server.Close()

	cfg := Config{}
	cfg.Railway.APIToken = "test-token"
	cfg.Railway.APIURL = server.URL
	client, err := newRailwayClient(cfg, Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	logs, err := client.DeploymentLogs(context.Background(), "dep-1", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 {
		t.Fatalf("logs len=%d, want 1", len(logs))
	}
	if logs[0] != message {
		t.Fatalf("log len=%d, want %d", len(logs[0]), len(message))
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

func TestRailwayRunRejectsArbitraryCommandBeforeDeploy(t *testing.T) {
	api := &fakeRailwayAPI{}
	cfg := Config{}
	cfg.Railway.ProjectID = "proj-1"
	cfg.Railway.EnvironmentID = "env-1"
	backend := &railwayBackend{cfg: cfg, rt: Runtime{Stdout: io.Discard, Stderr: io.Discard}, client: api}
	_, err := backend.Run(context.Background(), RunRequest{NoSync: true, ID: "svc-1", Command: []string{"false"}})
	if err == nil || !strings.Contains(err.Error(), "cannot execute arbitrary run commands") {
		t.Fatalf("err = %v, want unsupported command rejection", err)
	}
	if api.triggerServiceID != "" || api.latestCalls != 0 || api.pollCalls != 0 {
		t.Fatalf("Run touched Railway API: trigger=%q latest=%d poll=%d", api.triggerServiceID, api.latestCalls, api.pollCalls)
	}
}

func TestRailwayRedeployRequiresProjectAndEnvironment(t *testing.T) {
	backend := &railwayBackend{rt: Runtime{Stdout: io.Discard, Stderr: io.Discard}}
	_, err := backend.redeployService(context.Background(), "svc-1")
	if err == nil || !strings.Contains(err.Error(), "--railway-project") {
		t.Fatalf("err = %v, want --railway-project rejection", err)
	}
	cfg := Config{}
	cfg.Railway.ProjectID = "proj-1"
	backend2 := &railwayBackend{cfg: cfg, rt: Runtime{Stdout: io.Discard, Stderr: io.Discard}}
	_, err = backend2.redeployService(context.Background(), "svc-1")
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
		{name: "shell", req: RunRequest{ID: "svc-1", ShellMode: true, NoSync: true, Command: []string{"pnpm test"}}, want: "--shell"},
		{name: "env summary", req: RunRequest{ID: "svc-1", NoSync: true, Env: map[string]string{"TOKEN": "secret"}, EnvSummary: true, Command: []string{"pnpm", "test"}}, want: "environment"},
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

func TestRailwayRunAllowsImplicitDefaultEnv(t *testing.T) {
	err := rejectRailwayRunOptions(RunRequest{
		ID:      "svc-1",
		NoSync:  true,
		Env:     map[string]string{"CI": "true"},
		Command: []string{"pnpm", "test"},
	})
	if err != nil {
		t.Fatalf("rejectRailwayRunOptions err: %v", err)
	}
}

func TestRailwayClientRejectsFalseStopDeploymentResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":{"deploymentStop":false}}`)
	}))
	defer server.Close()

	cfg := Config{}
	cfg.Railway.APIToken = "test-token"
	cfg.Railway.APIURL = server.URL
	client, err := newRailwayClient(cfg, Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	err = client.StopDeployment(context.Background(), "dep-1")
	if err == nil || !strings.Contains(err.Error(), "deploymentStop returned false") {
		t.Fatalf("err = %v, want false stop error", err)
	}
}

type fakeRailwayAPI struct {
	mu                   sync.Mutex
	triggerProjectID     string
	triggerEnvironmentID string
	triggerServiceID     string
	deployID             string
	buildLogs            []string
	buildLogsForID       string
	logs                 []string
	logsForID            string
	deployment           railwayDeployment
	latestDeployments    []railwayDeployment
	latestCalls          int
	// pollStatuses, when non-empty, is the sequence of statuses returned by
	// Deployment() one call at a time. The last entry is replayed forever so
	// callers can model "many non-terminal polls then a terminal one".
	pollStatuses    []railwayDeploymentStatus
	pollCalls       int
	deploymentErr   error
	deploymentBlock chan struct{}
	services        []railwayService
	service         railwayService
	stopID          string
	triggerErr      error
	logsErr         error
	listErr         error
	latestErr       error
}

func (f *fakeRailwayAPI) TriggerDeploy(_ context.Context, projectID, environmentID, serviceID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.triggerProjectID = projectID
	f.triggerEnvironmentID = environmentID
	f.triggerServiceID = serviceID
	return f.deployID, f.triggerErr
}

func (f *fakeRailwayAPI) BuildLogs(_ context.Context, deploymentID string, _ int) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.buildLogsForID = deploymentID
	return f.buildLogs, nil
}

func (f *fakeRailwayAPI) DeploymentLogs(_ context.Context, deploymentID string, _ int) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logsForID = deploymentID
	return f.logs, f.logsErr
}

func (f *fakeRailwayAPI) LatestDeployment(_ context.Context, _, _, _ string) (railwayDeployment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.latestCalls++
	if f.latestErr != nil {
		return railwayDeployment{}, f.latestErr
	}
	if len(f.latestDeployments) > 0 {
		idx := f.latestCalls - 1
		if idx >= len(f.latestDeployments) {
			idx = len(f.latestDeployments) - 1
		}
		return f.latestDeployments[idx], nil
	}
	return f.deployment, nil
}

func (f *fakeRailwayAPI) Deployment(ctx context.Context, deploymentID string) (railwayDeployment, error) {
	// Optional gate that lets a test hold the call open so the polling loop can
	// observe context-deadline cancellation.
	f.mu.Lock()
	block := f.deploymentBlock
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return railwayDeployment{}, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pollCalls++
	if f.deploymentErr != nil {
		return railwayDeployment{}, f.deploymentErr
	}
	if len(f.pollStatuses) == 0 {
		return railwayDeployment{ID: deploymentID, Status: f.deployment.Status}, nil
	}
	idx := f.pollCalls - 1
	if idx >= len(f.pollStatuses) {
		idx = len(f.pollStatuses) - 1
	}
	return railwayDeployment{ID: deploymentID, Status: f.pollStatuses[idx]}, nil
}

func (f *fakeRailwayAPI) StopDeployment(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopID = id
	return nil
}

func (f *fakeRailwayAPI) ListServices(_ context.Context) ([]railwayService, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.services, f.listErr
}

func (f *fakeRailwayAPI) GetService(_ context.Context, _ string) (railwayService, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.service, nil
}

func newRailwayBackendForTest(api *fakeRailwayAPI) *railwayBackend {
	cfg := Config{Provider: providerName}
	cfg.Railway.APIToken = "test-token"
	cfg.Railway.APIURL = "https://backboard.railway.com/graphql/v2"
	cfg.Railway.ProjectID = "proj-1"
	cfg.Railway.EnvironmentID = "env-1"
	rt := Runtime{Stdout: io.Discard, Stderr: io.Discard}
	return &railwayBackend{
		spec:                  Provider{}.Spec(),
		cfg:                   cfg,
		rt:                    rt,
		client:                api,
		pollInitialOverride:   time.Millisecond,
		pollOverallOverride:   5 * time.Second,
		deployResolveOverride: 5 * time.Second,
	}
}

func TestRailwayRedeployHappyPath(t *testing.T) {
	api := &fakeRailwayAPI{
		deployID: "dep-1",
		// Trigger returns dep-1; the poll loop sees one non-terminal status before
		// terminating on SUCCESS; logs are then fetched against that exact id.
		pollStatuses: []railwayDeploymentStatus{railwayStatusDeploying, railwayStatusSuccess},
		buildLogs:    []string{"building image"},
		logs:         []string{"+ pnpm test", "PASS suite (1.2s)"},
	}
	backend := newRailwayBackendForTest(api)
	result, err := backend.redeployService(context.Background(), "svc-1")
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", result.ExitCode)
	}
	if api.triggerServiceID != "svc-1" || api.triggerProjectID != "proj-1" || api.triggerEnvironmentID != "env-1" {
		t.Fatalf("trigger called with svc=%q proj=%q env=%q", api.triggerServiceID, api.triggerProjectID, api.triggerEnvironmentID)
	}
	if api.logsForID != "dep-1" {
		t.Fatalf("logs fetched for id=%q, want dep-1 (new deployment, not stale)", api.logsForID)
	}
	if api.buildLogsForID != "dep-1" {
		t.Fatalf("build logs fetched for id=%q, want dep-1 (new deployment, not stale)", api.buildLogsForID)
	}
	if api.pollCalls < 2 {
		t.Fatalf("poll calls = %d, want at least 2 (DEPLOYING then SUCCESS)", api.pollCalls)
	}
}

func TestRailwayRedeployFailedDeploymentMapsToExit1(t *testing.T) {
	api := &fakeRailwayAPI{
		deployID:     "dep-1",
		pollStatuses: []railwayDeploymentStatus{railwayStatusFailed},
	}
	backend := newRailwayBackendForTest(api)
	result, err := backend.redeployService(context.Background(), "svc-1")
	if err == nil {
		t.Fatal("Run accepted FAILED deployment status")
	}
	if result.ExitCode != 1 {
		t.Fatalf("exit code = %d, want 1", result.ExitCode)
	}
}

func TestRailwayRedeployPollsDeployingThenSuccess(t *testing.T) {
	api := &fakeRailwayAPI{
		deployID:     "dep-new",
		pollStatuses: []railwayDeploymentStatus{railwayStatusQueued, railwayStatusBuilding, railwayStatusDeploying, railwayStatusSuccess},
		logs:         []string{"ok"},
	}
	backend := newRailwayBackendForTest(api)
	result, err := backend.redeployService(context.Background(), "svc-1")
	if err != nil {
		t.Fatalf("Run err: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", result.ExitCode)
	}
	if api.pollCalls != 4 {
		t.Fatalf("poll calls = %d, want 4 (queued, building, deploying, success)", api.pollCalls)
	}
}

func TestRailwayRedeployResolvesDeploymentWhenTriggerReturnsNoID(t *testing.T) {
	api := &fakeRailwayAPI{
		deployID: "",
		latestDeployments: []railwayDeployment{
			{ID: "dep-old", Status: railwayStatusSuccess},
			{ID: "dep-old", Status: railwayStatusSuccess},
			{ID: "dep-new", Status: railwayStatusQueued},
		},
		pollStatuses: []railwayDeploymentStatus{railwayStatusSuccess},
		logs:         []string{"ok"},
	}
	backend := newRailwayBackendForTest(api)
	result, err := backend.redeployService(context.Background(), "svc-1")
	if err != nil {
		t.Fatalf("Run err: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", result.ExitCode)
	}
	if api.logsForID != "dep-new" {
		t.Fatalf("logs fetched for id=%q, want dep-new", api.logsForID)
	}
	if api.latestCalls < 3 {
		t.Fatalf("latest calls = %d, want fallback polling", api.latestCalls)
	}
}

func TestRailwayRedeployRequiresPreviousDeploymentReadWhenTriggerReturnsNoID(t *testing.T) {
	api := &fakeRailwayAPI{
		deployID:  "",
		latestErr: fmt.Errorf("latest unavailable"),
	}
	backend := newRailwayBackendForTest(api)
	result, err := backend.redeployService(context.Background(), "svc-1")
	if err == nil {
		t.Fatal("Run accepted boolean trigger fallback without a trusted previous deployment")
	}
	if result.ExitCode == 0 {
		t.Fatalf("exit code = %d, want non-zero on failed previous deployment read", result.ExitCode)
	}
	if !strings.Contains(err.Error(), "read latest deployment before trigger failed") {
		t.Fatalf("err = %v, want previous deployment read message", err)
	}
	if api.triggerServiceID != "svc-1" {
		t.Fatalf("trigger service = %q, want svc-1", api.triggerServiceID)
	}
}

func TestRailwayRedeployIgnoresPreviousDeploymentReadErrorWhenTriggerReturnsID(t *testing.T) {
	api := &fakeRailwayAPI{
		deployID:     "dep-1",
		latestErr:    fmt.Errorf("latest unavailable"),
		pollStatuses: []railwayDeploymentStatus{railwayStatusSuccess},
	}
	backend := newRailwayBackendForTest(api)
	result, err := backend.redeployService(context.Background(), "svc-1")
	if err != nil {
		t.Fatalf("Run err: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", result.ExitCode)
	}
}

func TestRailwayRedeployTreatsSleepingAsSuccessfulTerminalStatus(t *testing.T) {
	api := &fakeRailwayAPI{
		deployID:     "dep-1",
		pollStatuses: []railwayDeploymentStatus{railwayStatusSleeping},
		logs:         []string{"service is sleeping"},
	}
	backend := newRailwayBackendForTest(api)
	result, err := backend.redeployService(context.Background(), "svc-1")
	if err != nil {
		t.Fatalf("Run err: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", result.ExitCode)
	}
	if api.pollCalls != 1 {
		t.Fatalf("poll calls = %d, want 1 for terminal SLEEPING", api.pollCalls)
	}
}

func TestRailwayRedeployStreamsBuildAndDeploymentLogsWithoutDuplicates(t *testing.T) {
	var stdout strings.Builder
	api := &fakeRailwayAPI{
		deployID:     "dep-1",
		pollStatuses: []railwayDeploymentStatus{railwayStatusDeploying, railwayStatusSuccess},
		buildLogs:    []string{"build line"},
		logs:         []string{"deploy line"},
	}
	backend := newRailwayBackendForTest(api)
	backend.rt.Stdout = &stdout
	if _, err := backend.redeployService(context.Background(), "svc-1"); err != nil {
		t.Fatalf("Run err: %v", err)
	}
	out := stdout.String()
	if strings.Count(out, "build line") != 1 {
		t.Fatalf("stdout = %q, want build line once", out)
	}
	if strings.Count(out, "deploy line") != 1 {
		t.Fatalf("stdout = %q, want deploy line once", out)
	}
}

func TestRailwayLogStreamerPrintsRollingWindowsOnce(t *testing.T) {
	var stdout strings.Builder
	var seen []string
	seen = printNewRailwayLogs(&stdout, []string{"build 1", "build 2", "build 3"}, seen)
	seen = printNewRailwayLogs(&stdout, []string{"build 1", "build 2", "build 3"}, seen)
	seen = printNewRailwayLogs(&stdout, []string{"build 2", "build 3", "build 4"}, seen)
	seen = printNewRailwayLogs(&stdout, []string{"build 3", "build 4", "build 5"}, seen)
	if got := stdout.String(); got != "build 1\nbuild 2\nbuild 3\nbuild 4\nbuild 5\n" {
		t.Fatalf("stdout = %q", got)
	}
}

func TestRailwayRedeployReturnsErrorWhenTriggerYieldsEmptyID(t *testing.T) {
	api := &fakeRailwayAPI{deployID: ""}
	backend := newRailwayBackendForTest(api)
	backend.deployResolveOverride = 25 * time.Millisecond
	result, err := backend.redeployService(context.Background(), "svc-1")
	if err == nil {
		t.Fatal("Run accepted empty deployment id from TriggerDeploy")
	}
	if result.ExitCode == 0 {
		t.Fatalf("exit code = %d, want non-zero on empty deployment id", result.ExitCode)
	}
	if !strings.Contains(err.Error(), "resolve triggered deployment") {
		t.Fatalf("err = %v, want deployment resolution message", err)
	}
}

func TestRailwayRedeployPollingHonorsContextDeadline(t *testing.T) {
	block := make(chan struct{})
	api := &fakeRailwayAPI{
		deployID:        "dep-1",
		pollStatuses:    []railwayDeploymentStatus{railwayStatusBuilding},
		deploymentBlock: block,
	}
	backend := newRailwayBackendForTest(api)
	backend.pollInitialOverride = time.Millisecond
	backend.pollOverallOverride = 25 * time.Millisecond
	defer close(block) // unblock at the end of the test so the goroutine exits cleanly
	_, err := backend.redeployService(context.Background(), "svc-1")
	if err == nil {
		t.Fatal("Run accepted hung deployment status")
	}
	if !strings.Contains(err.Error(), "polling") && !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("err = %v, want polling/deadline message", err)
	}
}

func TestRailwayDeploymentStatusEnum(t *testing.T) {
	for _, tc := range []struct {
		status     railwayDeploymentStatus
		isTerminal bool
		exitCode   int
	}{
		{railwayStatusSuccess, true, 0},
		{railwayStatusFailed, true, 1},
		{railwayStatusCrashed, true, 1},
		{railwayStatusRemoved, true, 1},
		{railwayStatusSkipped, true, 1},
		{railwayStatusSleeping, true, 0},
		{railwayStatusQueued, false, 1},
		{railwayStatusInitializing, false, 1},
		{railwayStatusBuilding, false, 1},
		{railwayStatusDeploying, false, 1},
		{railwayStatusWaiting, false, 1},
		{railwayStatusNeedsApproval, false, 1},
		{railwayStatusRemoving, false, 1},
	} {
		t.Run(string(tc.status), func(t *testing.T) {
			if got := tc.status.IsTerminal(); got != tc.isTerminal {
				t.Fatalf("IsTerminal() = %v, want %v", got, tc.isTerminal)
			}
			if got := tc.status.ExitCode(); got != tc.exitCode {
				t.Fatalf("ExitCode() = %d, want %d", got, tc.exitCode)
			}
		})
	}
}

func TestRailwayDeploymentStatusStateMapsTerminalFailures(t *testing.T) {
	for _, status := range []railwayDeploymentStatus{railwayStatusFailed, railwayStatusCrashed, railwayStatusRemoved, railwayStatusSkipped} {
		if got := status.State(); got != "failed" {
			t.Fatalf("%s State() = %q, want failed", status, got)
		}
	}
	if got := railwayStatusSleeping.State(); got != "sleeping" {
		t.Fatalf("SLEEPING State() = %q, want sleeping", got)
	}
}

func TestRailwayDeploymentStatusNormalizesOnUnmarshal(t *testing.T) {
	var dep railwayDeployment
	if err := json.Unmarshal([]byte(`{"id":"d","status":"  success  "}`), &dep); err != nil {
		t.Fatal(err)
	}
	if dep.Status != railwayStatusSuccess {
		t.Fatalf("status = %q, want SUCCESS (trim+upper-cased)", dep.Status)
	}
	if !dep.Status.IsTerminal() {
		t.Fatal("normalized SUCCESS must be terminal")
	}
}

// failingDeploymentClient lets the polling-error test thread a fake error
// through Deployment() without bypassing the fakeRailwayAPI mutex.
type failingDeploymentClient struct {
	*fakeRailwayAPI
	err error
}

func (f *failingDeploymentClient) Deployment(_ context.Context, _ string) (railwayDeployment, error) {
	return railwayDeployment{}, f.err
}

func TestRailwayRedeploySurfacesPollingTransportError(t *testing.T) {
	api := &fakeRailwayAPI{deployID: "dep-1"}
	wrapped := &failingDeploymentClient{fakeRailwayAPI: api, err: fmt.Errorf("network broken")}
	backend := newRailwayBackendForTest(api)
	backend.client = wrapped
	_, err := backend.redeployService(context.Background(), "svc-1")
	if err == nil {
		t.Fatal("Run accepted polling transport failure")
	}
	if !strings.Contains(err.Error(), "network broken") {
		t.Fatalf("err = %v, want surfaced transport error", err)
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

func TestRailwayStatusMapsTerminalFailureState(t *testing.T) {
	api := &fakeRailwayAPI{
		service:    railwayService{ID: "svc-1", Name: "api", ProjectID: "proj-1"},
		deployment: railwayDeployment{ID: "dep-1", Status: railwayStatusCrashed},
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
	if view.State != "failed" {
		t.Fatalf("view.State = %q, want failed", view.State)
	}
	if view.Ready {
		t.Fatal("CRASHED deployment should not be ready")
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

func TestRailwayDoctorRequiresProjectEnvironment(t *testing.T) {
	cfg := Config{Provider: providerName}
	cfg.Railway.APIToken = "test-token"
	cfg.Railway.APIURL = "https://backboard.railway.com/graphql/v2"
	backend := &railwayBackend{cfg: cfg, rt: Runtime{Stdout: io.Discard, Stderr: io.Discard}, client: &fakeRailwayAPI{}}
	_, err := backend.Doctor(context.Background(), DoctorRequest{})
	if err == nil || !strings.Contains(err.Error(), "--railway-project") {
		t.Fatalf("err = %v, want missing project rejection", err)
	}
}

func TestRailwayDoctorRequiresToken(t *testing.T) {
	cfg := Config{Provider: providerName}
	cfg.Railway.APIURL = "https://backboard.railway.com/graphql/v2"
	cfg.Railway.ProjectID = "proj-1"
	cfg.Railway.EnvironmentID = "env-1"
	backend := &railwayBackend{cfg: cfg, rt: Runtime{Stdout: io.Discard, Stderr: io.Discard}}
	_, err := backend.Doctor(context.Background(), DoctorRequest{})
	if err == nil || !strings.Contains(err.Error(), "RAILWAY_API_TOKEN") {
		t.Fatalf("err = %v, want missing token rejection", err)
	}
}

func TestRailwayDoctorListsServices(t *testing.T) {
	api := &fakeRailwayAPI{services: []railwayService{
		{ID: "svc-1", Name: "api", ProjectID: "proj-1"},
	}}
	cfg := Config{Provider: providerName}
	cfg.Railway.APIToken = "test-token"
	cfg.Railway.APIURL = "https://backboard.railway.com/graphql/v2"
	cfg.Railway.ProjectID = "proj-1"
	cfg.Railway.EnvironmentID = "env-1"
	backend := &railwayBackend{cfg: cfg, rt: Runtime{Stdout: io.Discard, Stderr: io.Discard}, client: api}
	result, err := backend.Doctor(context.Background(), DoctorRequest{})
	if err != nil {
		t.Fatalf("Doctor err: %v", err)
	}
	if result.Provider != providerName || !strings.Contains(result.Message, "inventory=ready") || !strings.Contains(result.Message, "leases=1") {
		t.Fatalf("Doctor result = %#v", result)
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

func TestRailwayFlagsRejectUnsupportedSizingForAliases(t *testing.T) {
	for _, provider := range []string{providerName, "rail", "railwayapp"} {
		t.Run(provider, func(t *testing.T) {
			cfg := Config{Provider: provider}
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			fs.String("class", "", "class")
			values := RegisterRailwayProviderFlags(fs, cfg)
			if err := fs.Parse([]string{"--class", "beast"}); err != nil {
				t.Fatal(err)
			}
			err := ApplyRailwayProviderFlags(&cfg, fs, values)
			if err == nil || !strings.Contains(err.Error(), "--class is not supported") {
				t.Fatalf("err = %v, want class rejection", err)
			}
		})
	}
}
