package fastapicloud

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

func TestFastAPICloudClientRejectsUnsafeAPIURLComponents(t *testing.T) {
	for _, test := range []struct {
		name   string
		apiURL string
		secret string
	}{
		{name: "userinfo", apiURL: "https://user:url-secret@api.fastapicloud.com/api/v1", secret: "url-secret"},
		{name: "query", apiURL: "https://api.fastapicloud.com/api/v1?token=query-secret", secret: "query-secret"},
		{name: "empty query", apiURL: "https://api.fastapicloud.com/api/v1?"},
		{name: "fragment", apiURL: "https://api.fastapicloud.com/api/v1#fragment-secret", secret: "fragment-secret"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := Config{FastAPICloud: FastAPICloudConfig{Token: "test-token", APIURL: test.apiURL}}
			_, err := newFastAPICloudClient(cfg, Runtime{})
			if err == nil || !strings.Contains(err.Error(), "must not contain userinfo, query parameters, or a fragment") {
				t.Fatalf("newFastAPICloudClient error = %v, want unsafe URL rejection", err)
			}
			if test.secret != "" && strings.Contains(err.Error(), test.secret) {
				t.Fatalf("newFastAPICloudClient error leaked URL secret: %v", err)
			}
		})
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
		w.Header().Set("Content-Type", "application/json")
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
			if r.URL.Query().Get("limit") != "100" || r.URL.Query().Get("skip") != "0" {
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

func TestFastAPICloudClientRefusesCrossOriginRedirectBeforeReplay(t *testing.T) {
	targetRequests := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetRequests++
		t.Errorf("redirect target received %s %s auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
	}))
	defer target.Close()
	trusted := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/stolen?token=redirect-secret#fragment-secret", http.StatusTemporaryRedirect)
	}))
	defer trusted.Close()

	api, err := newFastAPICloudClient(
		Config{FastAPICloud: FastAPICloudConfig{Token: "test-token", APIURL: trusted.URL}},
		Runtime{HTTP: trusted.Client()},
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = api.GetApp(context.Background(), "app-1")
	if err == nil || !strings.Contains(err.Error(), "refused cross-origin redirect") {
		t.Fatalf("GetApp error = %v, want cross-origin refusal", err)
	}
	for _, secret := range []string{"redirect-secret", "fragment-secret"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("GetApp error leaked redirect URL secret %q: %v", secret, err)
		}
	}
	if targetRequests != 0 {
		t.Fatalf("redirect target received %d requests, want 0", targetRequests)
	}
}

func TestFastAPICloudClientFollowsSameOriginRedirect(t *testing.T) {
	var redirectedAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/apps/app-1":
			http.Redirect(w, r, "/redirected", http.StatusTemporaryRedirect)
		case "/redirected":
			redirectedAuth = r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(fastAPICloudApp{ID: "app-1"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	api, err := newFastAPICloudClient(
		Config{FastAPICloud: FastAPICloudConfig{Token: "test-token", APIURL: server.URL + "/api/v1"}},
		Runtime{HTTP: server.Client()},
	)
	if err != nil {
		t.Fatal(err)
	}
	app, err := api.GetApp(context.Background(), "app-1")
	if err != nil {
		t.Fatal(err)
	}
	if app.ID != "app-1" || redirectedAuth != "Bearer test-token" {
		t.Fatalf("app=%#v auth=%q, want app-1 with bearer token", app, redirectedAuth)
	}
}

func TestFastAPICloudClientPreservesCallerRedirectPolicy(t *testing.T) {
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
	api, err := newFastAPICloudClient(
		Config{FastAPICloud: FastAPICloudConfig{Token: "test-token", APIURL: server.URL}},
		Runtime{HTTP: httpClient},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = api.GetApp(context.Background(), "app-1")
	if !errors.Is(err, callerErr) || callerChecks != 1 {
		t.Fatalf("GetApp error = %v, caller checks = %d", err, callerChecks)
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

func TestFastAPICloudWarmupRejected(t *testing.T) {
	backend := &fastAPICloudBackend{spec: Provider{}.Spec(), cfg: Config{}, client: panicFastAPICloudAPI{}}
	err := backend.Warmup(context.Background(), WarmupRequest{})
	if err == nil || !strings.Contains(err.Error(), "does not support warmup") {
		t.Fatalf("err = %v, want warmup rejection", err)
	}
}

func TestFastAPICloudStopRejected(t *testing.T) {
	backend := &fastAPICloudBackend{spec: Provider{}.Spec(), cfg: Config{}, client: panicFastAPICloudAPI{}}
	err := backend.Stop(context.Background(), StopRequest{})
	if err == nil || !strings.Contains(err.Error(), "does not support stop") {
		t.Fatalf("err = %v, want stop rejection", err)
	}
}

func TestFastAPICloudListWithTeamID(t *testing.T) {
	fake := &fakeFastAPICloudAPI{
		apps: []fastAPICloudApp{
			{ID: "app-1", TeamID: "team-1", Slug: "one", Name: "One", URL: "https://one.fastapicloud.app"},
			{ID: "app-2", TeamID: "team-1", Slug: "two", Name: "Two", URL: "https://two.fastapicloud.app"},
		},
	}
	cfg := Config{}
	cfg.FastAPICloud.TeamID = "team-1"
	backend := &fastAPICloudBackend{spec: Provider{}.Spec(), cfg: cfg, client: fake}
	views, err := backend.List(context.Background(), ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 2 || views[0].CloudID != "app-1" || views[1].CloudID != "app-2" {
		t.Fatalf("views = %#v, want two apps for the team", views)
	}
	if views[0].Provider != providerName || views[0].Labels["url"] == "" {
		t.Fatalf("view metadata = %#v, want provider + url labels", views[0])
	}
	if fake.listCalls != 1 || fake.getCalls != 0 {
		t.Fatalf("calls list=%d get=%d, want list only", fake.listCalls, fake.getCalls)
	}
}

// Guards the pagination fix: a full first page with the "count" field absent must
// not be mistaken for the final page.
func TestFastAPICloudClientListAppsPaginatesWithoutCount(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/apps/" {
			http.NotFound(w, r)
			return
		}
		var data []fastAPICloudApp
		switch r.URL.Query().Get("skip") {
		case "0":
			for i := 0; i < 100; i++ {
				data = append(data, fastAPICloudApp{ID: fmt.Sprintf("app-%d", i), TeamID: "team-1"})
			}
		case "100":
			for i := 100; i < 150; i++ {
				data = append(data, fastAPICloudApp{ID: fmt.Sprintf("app-%d", i), TeamID: "team-1"})
			}
		}
		// Deliberately omit "count" to exercise the count-absent termination path.
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(fastAPICloudListResponse[fastAPICloudApp]{Data: data})
	}))
	defer server.Close()
	cfg := Config{}
	cfg.FastAPICloud.Token = "test-token"
	cfg.FastAPICloud.APIURL = server.URL + "/api/v1"
	client, err := newFastAPICloudClient(cfg, Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	apps, err := client.ListApps(context.Background(), "team-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 150 {
		t.Fatalf("got %d apps, want 150 (pagination must not stop early when count is absent)", len(apps))
	}
}

// Guards the ordering fix: LatestDeployment must select the newest by created_at,
// not assume the API returns newest-first.
func TestFastAPICloudClientLatestDeploymentPicksNewest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/apps/app-1/deployments/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(fastAPICloudListResponse[fastAPICloudDeployment]{
			Data: []fastAPICloudDeployment{
				{ID: "dep-old", AppID: "app-1", Status: fastAPICloudStatusSuccess, CreatedAt: "2026-06-01T10:00:00Z"},
				{ID: "dep-new", AppID: "app-1", Status: fastAPICloudStatusSuccess, CreatedAt: "2026-06-21T10:00:00Z"},
				{ID: "dep-mid", AppID: "app-1", Status: fastAPICloudStatusSuccess, CreatedAt: "2026-06-10T10:00:00Z"},
			},
			Count: 3,
		})
	}))
	defer server.Close()
	cfg := Config{}
	cfg.FastAPICloud.Token = "test-token"
	cfg.FastAPICloud.APIURL = server.URL + "/api/v1"
	client, err := newFastAPICloudClient(cfg, Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	dep, ok, err := client.LatestDeployment(context.Background(), "app-1")
	if err != nil || !ok {
		t.Fatalf("LatestDeployment err=%v ok=%v", err, ok)
	}
	if dep.ID != "dep-new" {
		t.Fatalf("latest = %q, want dep-new (newest by created_at)", dep.ID)
	}
}

func newTestFastAPICloudClient(t *testing.T, server *httptest.Server) fastAPICloudAPI {
	t.Helper()
	cfg := Config{}
	cfg.FastAPICloud.Token = "secret-tok"
	cfg.FastAPICloud.APIURL = server.URL + "/api/v1"
	client, err := newFastAPICloudClient(cfg, Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestFastAPICloudClientUnauthorizedNoTokenLeak(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret-tok" {
			http.Error(w, "bad bearer", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"detail":"Invalid credentials"}`)
	}))
	defer server.Close()
	_, err := newTestFastAPICloudClient(t, server).GetApp(context.Background(), "app-1")
	var apiErr *fastAPICloudAPIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *fastAPICloudAPIError", err)
	}
	if apiErr.StatusCode != 401 || !strings.Contains(apiErr.Body, "Invalid credentials") {
		t.Fatalf("apiErr = %#v, want 401 with detail body", apiErr)
	}
	if strings.Contains(err.Error(), "secret-tok") {
		t.Fatal("token leaked into error message")
	}
}

func TestFastAPICloudAPIErrorMessage(t *testing.T) {
	if got := (&fastAPICloudAPIError{Status: "502 Bad Gateway"}).Error(); got != "502 Bad Gateway" {
		t.Fatalf("empty-body Error() = %q", got)
	}
	if got := (&fastAPICloudAPIError{Status: "500 X", Body: "boom"}).Error(); got != "500 X: boom" {
		t.Fatalf("non-empty Error() = %q", got)
	}
}

func TestFastAPICloudClientMalformedJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":`)
	}))
	defer server.Close()
	_, err := newTestFastAPICloudClient(t, server).GetApp(context.Background(), "app-1")
	if err == nil || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("err = %v, want decode error", err)
	}
	if errors.Unwrap(err) == nil {
		t.Fatal("decode error must wrap the underlying json error")
	}
}

func TestFastAPICloudClientResponseTooLarge(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("a"), fastAPICloudMaxResponseBytes+1))
	}))
	defer server.Close()
	_, err := newTestFastAPICloudClient(t, server).GetApp(context.Background(), "app-1")
	if err == nil || !strings.Contains(err.Error(), "response exceeds") {
		t.Fatalf("err = %v, want size-limit error", err)
	}
	var apiErr *fastAPICloudAPIError
	if errors.As(err, &apiErr) {
		t.Fatal("size-limit failure must not be a *fastAPICloudAPIError")
	}
}

func TestFastAPICloudClientContextCancelled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()
	client := newTestFastAPICloudClient(t, server)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := client.GetApp(ctx, "app-1")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	var apiErr *fastAPICloudAPIError
	if errors.As(err, &apiErr) {
		t.Fatal("transport cancellation must not be a *fastAPICloudAPIError")
	}
}

func TestFastAPICloudClientEscapesAppID(t *testing.T) {
	var gotPath, gotRaw string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotRaw = r.URL.EscapedPath(), r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x"}`)
	}))
	defer server.Close()
	if _, err := newTestFastAPICloudClient(t, server).GetApp(context.Background(), "a/b?c#d"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotPath, "a%2Fb%3Fc%23d") {
		t.Fatalf("escaped path = %q, want the app id percent-escaped", gotPath)
	}
	if gotRaw != "" {
		t.Fatalf("raw query = %q, want none (a malicious id must not inject a query)", gotRaw)
	}
}

func TestFastAPICloudClientEncodesTeamID(t *testing.T) {
	var gotTeam, gotRaw string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTeam, gotRaw = r.URL.Query().Get("team_id"), r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(fastAPICloudListResponse[fastAPICloudApp]{})
	}))
	defer server.Close()
	if _, err := newTestFastAPICloudClient(t, server).ListApps(context.Background(), "team a&b=c"); err != nil {
		t.Fatal(err)
	}
	if gotTeam != "team a&b=c" {
		t.Fatalf("decoded team_id = %q", gotTeam)
	}
	if !strings.Contains(gotRaw, "team_id=team+a%26b%3Dc") {
		t.Fatalf("raw query = %q, want percent-encoded team_id", gotRaw)
	}
}

func TestFastAPICloudClientLoopbackHTTPAccepted(t *testing.T) {
	for _, u := range []string{
		"http://localhost:8000/api/v1",
		"http://127.0.0.1:8000/api/v1",
		"http://[::1]:8000/api/v1",
		"http://LOCALHOST:8000/api/v1",
	} {
		cfg := Config{}
		cfg.FastAPICloud.Token = "t"
		cfg.FastAPICloud.APIURL = u
		if _, err := newFastAPICloudClient(cfg, Runtime{}); err != nil {
			t.Fatalf("loopback %q rejected: %v", u, err)
		}
	}
}

func TestFastAPICloudClientLoopbackSpoofRejected(t *testing.T) {
	for _, test := range []struct {
		apiURL string
		want   string
	}{
		{apiURL: "http://localhost@evil.com/api/v1", want: "must not contain userinfo"},
		{apiURL: "http://127.0.0.1.evil.com/api/v1", want: "must use HTTPS"},
		{apiURL: "http://localhost.evil.com/api/v1", want: "must use HTTPS"},
		{apiURL: "http://0x7f000001/api/v1", want: "must use HTTPS"},
	} {
		cfg := Config{}
		cfg.FastAPICloud.Token = "t"
		cfg.FastAPICloud.APIURL = test.apiURL
		_, err := newFastAPICloudClient(cfg, Runtime{})
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("spoofed loopback %q: err = %v, want %q", test.apiURL, err, test.want)
		}
	}
}

func TestFastAPICloudClientInvalidURL(t *testing.T) {
	for _, u := range []string{"://nohost", "https://", "not a url"} {
		cfg := Config{}
		cfg.FastAPICloud.Token = "t"
		cfg.FastAPICloud.APIURL = u
		_, err := newFastAPICloudClient(cfg, Runtime{})
		if err == nil || !strings.Contains(err.Error(), "must be an absolute HTTPS URL") {
			t.Fatalf("invalid url %q: err = %v, want absolute HTTPS URL rejection", u, err)
		}
	}
}

func TestFastAPICloudDeploymentStatusMapping(t *testing.T) {
	cases := []struct {
		status fastAPICloudDeploymentStatus
		state  string
		ready  bool
	}{
		{"waiting_upload", "waiting_upload", false},
		{"upload_cancelled", "failed", false},
		{"ready_for_build", "ready_for_build", false},
		{"building", "building", false},
		{"extracting", "extracting", false},
		{"extracting_failed", "failed", false},
		{"building_image", "building_image", false},
		{"building_image_failed", "failed", false},
		{"deploying", "deploying", false},
		{"deploying_failed", "failed", false},
		{"verifying", "verifying", false},
		{"verifying_failed", "failed", false},
		{"verifying_skipped", "ready", true},
		{"success", "ready", true},
		{"expired", "expired", false},
		{"failed", "failed", false},
		{"", "unknown", false},
		{"SUCCESS", "ready", true}, // normalization is case-insensitive
	}
	for _, c := range cases {
		if got := c.status.State(); got != c.state {
			t.Errorf("State(%q) = %q, want %q", c.status, got, c.state)
		}
		if got := c.status.IsReady(); got != c.ready {
			t.Errorf("IsReady(%q) = %v, want %v", c.status, got, c.ready)
		}
	}
}

func TestFastAPICloudStatusNoDeployment(t *testing.T) {
	fake := &fakeFastAPICloudAPI{
		app: fastAPICloudApp{
			ID:        "app-1",
			TeamID:    "team-1",
			Slug:      "my-app",
			Directory: "src",
			URL:       "https://my-app.fastapicloud.app",
			Region:    "us-east",
			UpdatedAt: "2024-01-01T00:00:00Z",
		},
		hasDeployment: false,
	}
	cfg := Config{}
	cfg.FastAPICloud.AppID = "app-1"
	backend := &fastAPICloudBackend{spec: Provider{}.Spec(), cfg: cfg, client: fake}
	view, err := backend.Status(context.Background(), StatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if view.State != "no-deployment" || view.Ready {
		t.Fatalf("view = %#v, want no-deployment and not ready", view)
	}
	// App-level labels must still be populated when there is no deployment.
	for key, want := range map[string]string{
		"teamId":    "team-1",
		"slug":      "my-app",
		"directory": "src",
		"url":       "https://my-app.fastapicloud.app",
		"region":    "us-east",
		"updatedAt": "2024-01-01T00:00:00Z",
	} {
		if view.Labels[key] != want {
			t.Fatalf("Labels[%q] = %q, want %q", key, view.Labels[key], want)
		}
	}
	// No deployment-scoped labels may appear when no deployment exists.
	for _, key := range []string{"deploymentId", "deploymentSlug", "deploymentStatus", "deploymentCreatedAt", "deploymentUrl", "deploymentDashboardUrl"} {
		if _, ok := view.Labels[key]; ok {
			t.Fatalf("Labels contains %q with no deployment present", key)
		}
	}
}

func TestFastAPICloudHostFromAppURL(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"https host", "https://my-app.fastapicloud.app", "my-app.fastapicloud.app"},
		{"ipv6 loopback strips brackets", "http://[::1]:8000", "::1"},
		{"non-url falls back to raw", "not a url", "not a url"},
		{"empty", "", ""},
		{"whitespace trimmed", "  staging.example  ", "staging.example"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hostFromAppURL(tc.raw); got != tc.want {
				t.Fatalf("hostFromAppURL(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestFastAPICloudClientRejectsNonJSONContentType(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<html>not json</html>"))
	}))
	defer server.Close()
	client := newTestFastAPICloudClient(t, server)
	_, err := client.GetApp(context.Background(), "app-1")
	if err == nil || !strings.Contains(err.Error(), "application/json") {
		t.Fatalf("err = %v, want a content-type mismatch error", err)
	}
}

func TestFastAPICloudStatusAppNotFound(t *testing.T) {
	fake := &fakeFastAPICloudAPI{getErr: errors.New("app not found")}
	cfg := Config{}
	cfg.FastAPICloud.AppID = "missing"
	backend := &fastAPICloudBackend{spec: Provider{}.Spec(), cfg: cfg, client: fake}
	_, err := backend.Status(context.Background(), StatusRequest{})
	if err == nil || !strings.Contains(err.Error(), "app not found") {
		t.Fatalf("err = %v, want propagated app lookup error", err)
	}
	if fake.latestCalls != 0 {
		t.Fatalf("latestCalls = %d, want 0 (must not query deployments once the app lookup fails)", fake.latestCalls)
	}
}

func TestFastAPICloudStatusRequiresAppID(t *testing.T) {
	backend := &fastAPICloudBackend{spec: Provider{}.Spec(), cfg: Config{}, client: panicFastAPICloudAPI{}}
	_, err := backend.Status(context.Background(), StatusRequest{})
	if err == nil || !strings.Contains(err.Error(), "status requires") {
		t.Fatalf("err = %v, want app-id requirement before any API call", err)
	}
}

type fakeFastAPICloudAPI struct {
	app           fastAPICloudApp
	apps          []fastAPICloudApp
	deployment    fastAPICloudDeployment
	hasDeployment bool
	getErr        error
	getCalls      int
	listCalls     int
	latestCalls   int
}

func (f *fakeFastAPICloudAPI) GetApp(context.Context, string) (fastAPICloudApp, error) {
	f.getCalls++
	if f.getErr != nil {
		return fastAPICloudApp{}, f.getErr
	}
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
