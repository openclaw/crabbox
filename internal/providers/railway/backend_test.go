package railway

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/openclaw/crabbox/internal/providers/shared"
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

func TestRailwayClientRefusesCrossOriginRedirectBeforeReplay(t *testing.T) {
	targetRequests := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetRequests++
		t.Errorf("redirect target received %s %s auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
	}))
	defer target.Close()
	trusted := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/stolen?token=redirect-secret", http.StatusTemporaryRedirect)
	}))
	defer trusted.Close()

	api, err := newRailwayClient(
		Config{Railway: RailwayConfig{APIToken: "test-token", APIURL: trusted.URL}},
		Runtime{HTTP: trusted.Client()},
	)
	if err != nil {
		t.Fatal(err)
	}
	err = api.(*railwayClient).do(context.Background(), "query { me { id } }", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "refused cross-origin redirect") {
		t.Fatalf("do error = %v, want cross-origin refusal", err)
	}
	if strings.Contains(err.Error(), "redirect-secret") {
		t.Fatalf("do error leaked redirect query: %v", err)
	}
	if targetRequests != 0 {
		t.Fatalf("redirect target received %d requests, want 0", targetRequests)
	}
}

func TestRailwayClientFollowsSameOriginRedirect(t *testing.T) {
	var redirectedAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/graphql":
			http.Redirect(w, r, "/redirected", http.StatusTemporaryRedirect)
		case "/redirected":
			redirectedAuth = r.Header.Get("Authorization")
			_, _ = io.WriteString(w, `{"data":{"me":{"id":"user_1"}}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	api, err := newRailwayClient(
		Config{Railway: RailwayConfig{APIToken: "test-token", APIURL: server.URL + "/graphql"}},
		Runtime{HTTP: server.Client()},
	)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Me struct {
			ID string `json:"id"`
		} `json:"me"`
	}
	if err := api.(*railwayClient).do(context.Background(), "query { me { id } }", nil, &out); err != nil {
		t.Fatal(err)
	}
	if out.Me.ID != "user_1" || redirectedAuth != "Bearer test-token" {
		t.Fatalf("out=%#v auth=%q", out, redirectedAuth)
	}
}

func TestRailwayClientPreservesCallerRedirectPolicy(t *testing.T) {
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
	api, err := newRailwayClient(
		Config{Railway: RailwayConfig{APIToken: "test-token", APIURL: server.URL}},
		Runtime{HTTP: httpClient},
	)
	if err != nil {
		t.Fatal(err)
	}

	err = api.(*railwayClient).do(context.Background(), "query { me { id } }", nil, nil)
	if !errors.Is(err, callerErr) || callerChecks != 1 {
		t.Fatalf("do error = %v, caller checks = %d", err, callerChecks)
	}
}

func TestSameRailwayOrigin(t *testing.T) {
	trusted, _ := url.Parse("https://api.example.com:443/graphql")
	for _, test := range []struct {
		name string
		raw  string
		want bool
	}{
		{name: "default https port", raw: "https://api.example.com/next", want: true},
		{name: "case insensitive host", raw: "https://API.EXAMPLE.COM/next", want: true},
		{name: "scheme drift", raw: "http://api.example.com/next"},
		{name: "subdomain drift", raw: "https://redirect.api.example.com/next"},
		{name: "port drift", raw: "https://api.example.com:444/next"},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate, _ := url.Parse(test.raw)
			if got := shared.SameOrigin(trusted, candidate); got != test.want {
				t.Fatalf("shared.SameOrigin(%q)=%v, want %v", test.raw, got, test.want)
			}
		})
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
	result, err := backend.Run(context.Background(), RunRequest{NoSync: true, ID: "svc-1", Command: []string{"false"}})
	var exitErr ExitError
	wantMessage := "provider=railway cannot execute arbitrary run commands; Railway only runs the service's configured start command"
	if !errors.As(err, &exitErr) || exitErr.Code != 2 || exitErr.Message != wantMessage {
		t.Fatalf("err = %v, want unsupported command rejection", err)
	}
	if !reflect.DeepEqual(result, RunResult{}) {
		t.Fatalf("result = %#v, want zero result", result)
	}
	if len(api.calls) != 0 {
		t.Fatalf("Run touched Railway API: %v", api.calls)
	}
}

func TestRailwayRunRequiresCommand(t *testing.T) {
	api := &fakeRailwayAPI{}
	backend := newRailwayBackendForTest(api)
	result, err := backend.Run(context.Background(), RunRequest{NoSync: true, ID: "svc-1"})
	var exitErr ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 2 || exitErr.Message != "missing command" {
		t.Fatalf("err = %v, want missing command rejection", err)
	}
	if !reflect.DeepEqual(result, RunResult{}) {
		t.Fatalf("result = %#v, want zero result", result)
	}
	if len(api.calls) != 0 {
		t.Fatalf("Run touched Railway API: %v", api.calls)
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
	mu              sync.Mutex
	calls           []string
	deployment      railwayDeployment
	latestCalls     int
	pollCalls       int
	services        []railwayService
	service         railwayService
	getServiceCalls int
	stopID          string
	stopErr         error
	listErr         error
	latestErr       error
}

func (f *fakeRailwayAPI) TriggerDeploy(_ context.Context, projectID, environmentID, serviceID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "TriggerDeploy")
	return "", nil
}

func (f *fakeRailwayAPI) BuildLogs(_ context.Context, deploymentID string, _ int) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "BuildLogs")
	return nil, nil
}

func (f *fakeRailwayAPI) DeploymentLogs(_ context.Context, deploymentID string, _ int) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "DeploymentLogs")
	return nil, nil
}

func (f *fakeRailwayAPI) LatestDeployment(_ context.Context, _, _, _ string) (railwayDeployment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "LatestDeployment")
	f.latestCalls++
	if f.latestErr != nil {
		return railwayDeployment{}, f.latestErr
	}
	return f.deployment, nil
}

func (f *fakeRailwayAPI) Deployment(ctx context.Context, deploymentID string) (railwayDeployment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "Deployment")
	f.pollCalls++
	return railwayDeployment{ID: deploymentID, Status: f.deployment.Status}, nil
}

func (f *fakeRailwayAPI) StopDeployment(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "StopDeployment")
	f.stopID = id
	return f.stopErr
}

func (f *fakeRailwayAPI) ListServices(_ context.Context) ([]railwayService, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "ListServices")
	return f.services, f.listErr
}

func (f *fakeRailwayAPI) GetService(_ context.Context, _ string) (railwayService, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "GetService")
	f.getServiceCalls++
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
		spec:   Provider{}.Spec(),
		cfg:    cfg,
		rt:     rt,
		client: api,
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

func TestRailwayStopRejectsUnclaimedServiceBeforeRemoteReads(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	api := &fakeRailwayAPI{
		service:    railwayService{ID: "svc-1", Name: "api", ProjectID: "proj-1"},
		deployment: railwayDeployment{ID: "dep-1", Status: "BUILDING"},
	}
	cfg := Config{Provider: providerName}
	cfg.Railway.APIToken = "test-token"
	cfg.Railway.APIURL = "https://backboard.railway.com/graphql/v2"
	cfg.Railway.ProjectID = "proj-1"
	cfg.Railway.EnvironmentID = "env-1"
	backend := &railwayBackend{cfg: cfg, rt: Runtime{Stdout: io.Discard, Stderr: io.Discard}, client: api}
	err := backend.Stop(context.Background(), StopRequest{ID: "svc-1"})
	if err == nil || !strings.Contains(err.Error(), "not claimed") {
		t.Fatalf("Stop err=%v, want unclaimed rejection", err)
	}
	if api.getServiceCalls != 0 || api.latestCalls != 0 || api.stopID != "" {
		t.Fatalf("unclaimed stop touched Railway: service=%d latest=%d stop=%q", api.getServiceCalls, api.latestCalls, api.stopID)
	}
}

func TestRailwayReclaimAndStopBindsExactDeployment(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	api := &fakeRailwayAPI{
		service:    railwayService{ID: "svc-1", Name: "api", ProjectID: "proj-1"},
		deployment: railwayDeployment{ID: "dep-1", Status: "BUILDING"},
	}
	backend := newRailwayBackendForTest(api)
	if err := backend.ReclaimAndStop(context.Background(), StopRequest{ID: "svc-1"}); err != nil {
		t.Fatalf("ReclaimAndStop err: %v", err)
	}
	if api.stopID != "dep-1" {
		t.Fatalf("stop called with id=%q, want dep-1", api.stopID)
	}
	if _, ok, err := resolveLeaseClaimForProviderCloudID("svc-1"); err != nil || ok {
		t.Fatalf("successful stop left claim: ok=%t err=%v", ok, err)
	}
}

func TestRailwayFailedStopPreservesClaimForExactRetry(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	api := &fakeRailwayAPI{
		service:    railwayService{ID: "svc-1", Name: "api", ProjectID: "proj-1"},
		deployment: railwayDeployment{ID: "dep-1", Status: "BUILDING"},
		stopErr:    errors.New("stop unavailable"),
	}
	backend := newRailwayBackendForTest(api)
	err := backend.ReclaimAndStop(context.Background(), StopRequest{ID: "svc-1"})
	if err == nil || !strings.Contains(err.Error(), "stop unavailable") {
		t.Fatalf("ReclaimAndStop err=%v, want provider failure", err)
	}
	claim, ok, err := resolveLeaseClaimForProviderCloudID("svc-1")
	if err != nil || !ok {
		t.Fatalf("failed stop claim: ok=%t err=%v", ok, err)
	}
	if claim.Labels[railwayClaimDeploymentLabel] != "dep-1" {
		t.Fatalf("claim labels=%#v", claim.Labels)
	}
	api.stopErr = nil
	api.stopID = ""
	if err := backend.Stop(context.Background(), StopRequest{ID: "svc-1"}); err != nil {
		t.Fatalf("claimed retry err: %v", err)
	}
	if api.stopID != "dep-1" {
		t.Fatalf("retry stopped %q, want dep-1", api.stopID)
	}
}

func TestRailwayStopRejectsChangedDeployment(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	api := &fakeRailwayAPI{
		service:    railwayService{ID: "svc-1", Name: "api", ProjectID: "proj-1"},
		deployment: railwayDeployment{ID: "dep-1", Status: "BUILDING"},
		stopErr:    errors.New("retain claim"),
	}
	backend := newRailwayBackendForTest(api)
	if err := backend.ReclaimAndStop(context.Background(), StopRequest{ID: "svc-1"}); err == nil {
		t.Fatal("ReclaimAndStop unexpectedly succeeded")
	}
	api.stopErr = nil
	api.stopID = ""
	api.deployment = railwayDeployment{ID: "dep-2", Status: "BUILDING"}
	err := backend.Stop(context.Background(), StopRequest{ID: "svc-1"})
	if err == nil || !strings.Contains(err.Error(), "latest deployment changed") {
		t.Fatalf("Stop err=%v, want changed deployment rejection", err)
	}
	if api.stopID != "" {
		t.Fatalf("changed deployment stop called with %q", api.stopID)
	}
}

func TestRailwayStopRejectsClaimFromDifferentEnvironmentBeforeRemoteReads(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	api := &fakeRailwayAPI{
		service:    railwayService{ID: "svc-1", Name: "api", ProjectID: "proj-1"},
		deployment: railwayDeployment{ID: "dep-1", Status: "BUILDING"},
		stopErr:    errors.New("retain claim"),
	}
	backend := newRailwayBackendForTest(api)
	if err := backend.ReclaimAndStop(context.Background(), StopRequest{ID: "svc-1"}); err == nil {
		t.Fatal("ReclaimAndStop unexpectedly succeeded")
	}
	api.getServiceCalls = 0
	api.latestCalls = 0
	api.stopErr = nil
	api.stopID = ""
	backend.cfg.Railway.EnvironmentID = "env-other"
	err := backend.Stop(context.Background(), StopRequest{ID: "svc-1"})
	if err == nil || !strings.Contains(err.Error(), "does not match the configured endpoint") {
		t.Fatalf("Stop err=%v, want scope rejection", err)
	}
	if api.getServiceCalls != 0 || api.latestCalls != 0 || api.stopID != "" {
		t.Fatalf("scope mismatch touched Railway: service=%d latest=%d stop=%q", api.getServiceCalls, api.latestCalls, api.stopID)
	}
}

func TestRailwayReclaimRejectsServiceFromDifferentProject(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	api := &fakeRailwayAPI{
		service:    railwayService{ID: "svc-1", Name: "api", ProjectID: "proj-other"},
		deployment: railwayDeployment{ID: "dep-1", Status: "BUILDING"},
	}
	backend := newRailwayBackendForTest(api)
	err := backend.ReclaimAndStop(context.Background(), StopRequest{ID: "svc-1"})
	if err == nil || !strings.Contains(err.Error(), "does not belong") {
		t.Fatalf("ReclaimAndStop err=%v, want project rejection", err)
	}
	if api.latestCalls != 0 || api.stopID != "" {
		t.Fatalf("project mismatch touched deployment: latest=%d stop=%q", api.latestCalls, api.stopID)
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
