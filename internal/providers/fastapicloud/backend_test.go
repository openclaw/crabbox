package fastapicloud

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFastAPICloudProviderSpec(t *testing.T) {
	spec := Provider{}.Spec()
	if spec.Name != providerName {
		t.Fatalf("spec.Name = %q, want %q", spec.Name, providerName)
	}
	if spec.Kind != "service-control" {
		t.Fatalf("spec.Kind = %q, want service-control", spec.Kind)
	}
	aliases := Provider{}.Aliases()
	if len(aliases) != 2 || aliases[0] != "fastapicloud" || aliases[1] != "fastapi" {
		t.Fatalf("aliases = %#v, want [fastapicloud fastapi]", aliases)
	}
}

func TestFastAPICloudClientRequiresToken(t *testing.T) {
	cfg := Config{}
	cfg.FastAPICloud.APIURL = "https://api.fastapicloud.com/api/v1"
	if _, err := newFastAPICloudClient(cfg, Runtime{}); err == nil {
		t.Fatal("newFastAPICloudClient accepted empty token")
	}
}

func TestFastAPICloudClientRejectsBareHTTPURL(t *testing.T) {
	cfg := Config{}
	cfg.FastAPICloud.Token = "test-token"
	cfg.FastAPICloud.APIURL = "http://api.fastapicloud.com/api/v1"
	if _, err := newFastAPICloudClient(cfg, Runtime{}); err == nil {
		t.Fatal("newFastAPICloudClient accepted plaintext http URL")
	}
}

func TestFastAPICloudTokenFlagIsNotRegistered(t *testing.T) {
	cfg := Config{}
	cfg.FastAPICloud.Token = "secret-token"
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	RegisterFastAPICloudProviderFlags(fs, cfg)
	for _, name := range []string{"fastapi-cloud-token", "fastapi-cloud-api-token", "fastapi-cloud-key", "fastapi-cloud-api-key"} {
		if fs.Lookup(name) != nil {
			t.Fatalf("FastAPI Cloud token surfaced as a flag --%s", name)
		}
	}
	for _, name := range []string{"fastapi-cloud-url", "fastapi-cloud-app-id", "fastapi-cloud-team-id"} {
		if fs.Lookup(name) == nil {
			t.Fatalf("%s flag missing", name)
		}
	}
}

func TestFastAPICloudClientSendsBearerAndUsesRESTPaths(t *testing.T) {
	var got []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			http.Error(w, "missing bearer", http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Accept") != "application/json" {
			http.Error(w, "missing accept", http.StatusBadRequest)
			return
		}
		got = append(got, r.URL.String())
		switch r.URL.Path {
		case "/api/v1/apps/app-1":
			_ = json.NewEncoder(w).Encode(fastAPICloudApp{
				ID:     "app-1",
				TeamID: "team-1",
				Slug:   "my-app",
				Name:   "My App",
				URL:    "https://my-app.fastapicloud.app",
			})
		case "/api/v1/apps/":
			if r.URL.Query().Get("team_id") != "team-1" || r.URL.Query().Get("limit") != "100" || r.URL.Query().Get("skip") != "0" {
				http.Error(w, fmt.Sprintf("query = %s", r.URL.RawQuery), http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(fastAPICloudListResponse[fastAPICloudApp]{
				Data:  []fastAPICloudApp{{ID: "app-1", TeamID: "team-1", Slug: "my-app", Name: "My App"}},
				Count: 1,
			})
		case "/api/v1/apps/app-1/deployments/":
			if r.URL.Query().Get("limit") != "1" || r.URL.Query().Get("skip") != "0" {
				http.Error(w, fmt.Sprintf("query = %s", r.URL.RawQuery), http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(fastAPICloudListResponse[fastAPICloudDeployment]{
				Data: []fastAPICloudDeployment{{
					ID:        "dep-1",
					AppID:     "app-1",
					Slug:      "my-app-abc",
					Status:    fastAPICloudStatusSuccess,
					CreatedAt: "2026-06-21T10:00:00Z",
				}},
				Count: 1,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := Config{}
	cfg.FastAPICloud.Token = "test-token"
	cfg.FastAPICloud.APIURL = server.URL + "/api/v1"
	client, err := newFastAPICloudClient(cfg, Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	app, err := client.GetApp(context.Background(), "app-1")
	if err != nil {
		t.Fatal(err)
	}
	apps, err := client.ListApps(context.Background(), "team-1")
	if err != nil {
		t.Fatal(err)
	}
	deployment, ok, err := client.LatestDeployment(context.Background(), "app-1")
	if err != nil {
		t.Fatal(err)
	}
	if app.ID != "app-1" || len(apps) != 1 || apps[0].ID != "app-1" || !ok || deployment.ID != "dep-1" {
		t.Fatalf("unexpected client data app=%#v apps=%#v deployment=%#v ok=%t", app, apps, deployment, ok)
	}
	if len(got) != 3 {
		t.Fatalf("got paths = %#v, want 3 requests", got)
	}
}

func TestFastAPICloudClientSurfacesNon2xxAsAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden by token", http.StatusForbidden)
	}))
	defer server.Close()

	cfg := Config{}
	cfg.FastAPICloud.Token = "wrong-token"
	cfg.FastAPICloud.APIURL = server.URL
	client, err := newFastAPICloudClient(cfg, Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.GetApp(context.Background(), "app-1")
	if err == nil {
		t.Fatal("GetApp accepted 403 response")
	}
	apiErr, ok := err.(*fastAPICloudAPIError)
	if !ok {
		t.Fatalf("err = %T, want *fastAPICloudAPIError", err)
	}
	if apiErr.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", apiErr.StatusCode)
	}
	if !strings.Contains(apiErr.Body, "forbidden by token") {
		t.Fatalf("body = %q, want forbidden snippet", apiErr.Body)
	}
}

func TestFastAPICloudRunRejectsBeforeAPI(t *testing.T) {
	backend := &fastAPICloudBackend{
		spec:   Provider{}.Spec(),
		cfg:    Config{},
		client: panicFastAPICloudAPI{},
	}
	_, err := backend.Run(context.Background(), RunRequest{NoSync: true, Command: []string{"pytest"}})
	if err == nil || !strings.Contains(err.Error(), "cannot execute arbitrary run commands") {
		t.Fatalf("err = %v, want arbitrary command rejection", err)
	}
}

func TestFastAPICloudListRequiresAppOrTeam(t *testing.T) {
	backend := &fastAPICloudBackend{
		spec:   Provider{}.Spec(),
		cfg:    Config{},
		client: &fakeFastAPICloudAPI{},
	}
	_, err := backend.List(context.Background(), ListRequest{})
	if err == nil || !strings.Contains(err.Error(), "requires --fastapi-cloud-team-id or --fastapi-cloud-app-id") {
		t.Fatalf("err = %v, want app/team requirement", err)
	}
}

func TestFastAPICloudListWithAppID(t *testing.T) {
	fake := &fakeFastAPICloudAPI{
		app: fastAPICloudApp{ID: "app-1", TeamID: "team-1", Slug: "my-app", Name: "My App", URL: "https://my-app.fastapicloud.app"},
	}
	cfg := Config{}
	cfg.FastAPICloud.AppID = "app-1"
	backend := &fastAPICloudBackend{spec: Provider{}.Spec(), cfg: cfg, client: fake}
	views, err := backend.List(context.Background(), ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].CloudID != "app-1" || views[0].Provider != providerName || views[0].Labels["url"] == "" {
		t.Fatalf("views = %#v, want one FastAPI Cloud app", views)
	}
	if fake.getCalls != 1 || fake.listCalls != 0 {
		t.Fatalf("calls get=%d list=%d, want get only", fake.getCalls, fake.listCalls)
	}
}

func TestFastAPICloudStatusMapsDeploymentReadiness(t *testing.T) {
	fake := &fakeFastAPICloudAPI{
		app:           fastAPICloudApp{ID: "app-1", TeamID: "team-1", Slug: "my-app", Name: "My App", URL: "https://my-app.fastapicloud.app"},
		deployment:    fastAPICloudDeployment{ID: "dep-1", AppID: "app-1", Slug: "my-app-abc", Status: fastAPICloudStatusSuccess, URL: "https://deployment.example"},
		hasDeployment: true,
	}
	cfg := Config{}
	cfg.FastAPICloud.AppID = "app-1"
	backend := &fastAPICloudBackend{spec: Provider{}.Spec(), cfg: cfg, client: fake}
	view, err := backend.Status(context.Background(), StatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if view.ID != "app-1" || view.State != "ready" || !view.Ready || view.Host != "my-app.fastapicloud.app" {
		t.Fatalf("view = %#v, want ready app status", view)
	}
	if view.Labels["deploymentId"] != "dep-1" || view.Labels["deploymentStatus"] != "success" {
		t.Fatalf("labels = %#v, want deployment metadata", view.Labels)
	}
}

func TestFastAPICloudStatusMapsFailure(t *testing.T) {
	fake := &fakeFastAPICloudAPI{
		app:           fastAPICloudApp{ID: "app-1", TeamID: "team-1", Slug: "my-app", Name: "My App"},
		deployment:    fastAPICloudDeployment{ID: "dep-1", AppID: "app-1", Status: fastAPICloudStatusVerifyingFailed},
		hasDeployment: true,
	}
	cfg := Config{}
	cfg.FastAPICloud.AppID = "app-1"
	backend := &fastAPICloudBackend{spec: Provider{}.Spec(), cfg: cfg, client: fake}
	view, err := backend.Status(context.Background(), StatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if view.State != "failed" || view.Ready {
		t.Fatalf("view = %#v, want failed and not ready", view)
	}
}

func TestApplyFastAPICloudProviderFlags(t *testing.T) {
	cfg := Config{Provider: providerName}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	values := RegisterFastAPICloudProviderFlags(fs, Config{})
	if err := fs.Parse([]string{
		"--fastapi-cloud-url", "http://localhost:8000/api/v1",
		"--fastapi-cloud-app-id", "app-1",
		"--fastapi-cloud-team-id", "team-1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := ApplyFastAPICloudProviderFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.FastAPICloud.APIURL != "http://localhost:8000/api/v1" || cfg.FastAPICloud.AppID != "app-1" || cfg.FastAPICloud.TeamID != "team-1" {
		t.Fatalf("cfg.FastAPICloud = %#v", cfg.FastAPICloud)
	}
}

func TestApplyFastAPICloudProviderFlagsRejectsClassAndType(t *testing.T) {
	for _, flagName := range []string{"class", "type"} {
		cfg := Config{Provider: providerName}
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		fs.String(flagName, "", "")
		values := RegisterFastAPICloudProviderFlags(fs, Config{})
		if err := fs.Parse([]string{"--" + flagName, "small"}); err != nil {
			t.Fatal(err)
		}
		err := ApplyFastAPICloudProviderFlags(&cfg, fs, values)
		if err == nil || !strings.Contains(err.Error(), "--"+flagName+" is not supported") {
			t.Fatalf("err = %v, want --%s rejection", err, flagName)
		}
	}
}

type fakeFastAPICloudAPI struct {
	app           fastAPICloudApp
	apps          []fastAPICloudApp
	deployment    fastAPICloudDeployment
	hasDeployment bool
	getCalls      int
	listCalls     int
	latestCalls   int
}

func (f *fakeFastAPICloudAPI) GetApp(context.Context, string) (fastAPICloudApp, error) {
	f.getCalls++
	return f.app, nil
}

func (f *fakeFastAPICloudAPI) ListApps(context.Context, string) ([]fastAPICloudApp, error) {
	f.listCalls++
	return append([]fastAPICloudApp(nil), f.apps...), nil
}

func (f *fakeFastAPICloudAPI) LatestDeployment(context.Context, string) (fastAPICloudDeployment, bool, error) {
	f.latestCalls++
	return f.deployment, f.hasDeployment, nil
}

type panicFastAPICloudAPI struct{}

func (panicFastAPICloudAPI) GetApp(context.Context, string) (fastAPICloudApp, error) {
	panic("GetApp should not be called")
}

func (panicFastAPICloudAPI) ListApps(context.Context, string) ([]fastAPICloudApp, error) {
	panic("ListApps should not be called")
}

func (panicFastAPICloudAPI) LatestDeployment(context.Context, string) (fastAPICloudDeployment, bool, error) {
	panic("LatestDeployment should not be called")
}
