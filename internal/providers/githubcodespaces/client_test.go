package githubcodespaces

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestClientCreateCodespaceRequestShape(t *testing.T) {
	var captureMu sync.Mutex
	var gotMethod, gotPath, gotAuth string
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		captureMu.Lock()
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotBody = body
		captureMu.Unlock()
		if r.Header.Get("Accept") != "application/vnd.github+json" || r.Header.Get("X-GitHub-Api-Version") != "2022-11-28" {
			t.Fatalf("missing GitHub headers: %#v", r.Header)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":7,"name":"codespace-1","display_name":"Crabbox","state":"Available","environment_id":"env_1","owner":{"id":42,"login":"alice"},"repository":{"id":1001,"full_name":"example-org/my-app"},"machine":{"name":"standardLinux32gb"}}`))
	}))
	defer server.Close()

	c := newClient(GitHubCodespacesConfig{APIURL: server.URL}, Runtime{HTTP: server.Client()}, "ghp_this_token_value_is_redacted")
	created, err := c.createCodespace(context.Background(), createCodespaceRequest{
		Repo:             "example-org/my-app",
		Ref:              "main",
		Machine:          "standardLinux32gb",
		DevcontainerPath: ".devcontainer/devcontainer.json",
		WorkingDirectory: "/workspaces/my-app",
		Geo:              "UsWest",
		IdleTimeout:      90 * time.Second,
		RetentionPeriod:  24 * time.Hour,
		DisplayName:      "Crabbox",
	})
	if err != nil {
		t.Fatal(err)
	}
	captureMu.Lock()
	defer captureMu.Unlock()
	if gotMethod != http.MethodPost || gotPath != "/repos/example-org/my-app/codespaces" {
		t.Fatalf("request=%s %s", gotMethod, gotPath)
	}
	if gotAuth != "Bearer ghp_this_token_value_is_redacted" {
		t.Fatalf("auth=%q", gotAuth)
	}
	if gotBody["ref"] != "main" ||
		gotBody["machine"] != "standardLinux32gb" ||
		gotBody["devcontainer_path"] != ".devcontainer/devcontainer.json" ||
		gotBody["working_directory"] != "/workspaces/my-app" ||
		gotBody["geo"] != "UsWest" ||
		gotBody["idle_timeout_minutes"].(float64) != 2 ||
		gotBody["retention_period_minutes"].(float64) != 1440 ||
		gotBody["display_name"] != "Crabbox" {
		t.Fatalf("body=%#v", gotBody)
	}
	if _, ok := gotBody["location"]; ok {
		t.Fatalf("body used legacy location key: %#v", gotBody)
	}
	if created.ID != 7 || created.Owner.ID != 42 || created.Owner.Login != "alice" || created.Repository.FullName != "example-org/my-app" || created.EnvironmentID != "env_1" || created.Machine.Name != "standardLinux32gb" {
		t.Fatalf("created=%#v", created)
	}
}

func TestClientCreateCodespaceIncludesExplicitZeroRetention(t *testing.T) {
	var captureMu sync.Mutex
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		captureMu.Lock()
		gotBody = body
		captureMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":7,"name":"codespace-1"}`))
	}))
	defer server.Close()

	c := newClient(GitHubCodespacesConfig{APIURL: server.URL}, Runtime{HTTP: server.Client()}, "redacted")
	if _, err := c.createCodespace(context.Background(), createCodespaceRequest{
		Repo:            "example-org/my-app",
		RetentionPeriod: 0,
		RetentionSet:    true,
	}); err != nil {
		t.Fatal(err)
	}
	captureMu.Lock()
	defer captureMu.Unlock()
	if value, ok := gotBody["retention_period_minutes"]; !ok || value.(float64) != 0 {
		t.Fatalf("body=%#v", gotBody)
	}
}

func TestClientListAcceptsResponseAboveLegacyOneMiBLimit(t *testing.T) {
	displayName := strings.Repeat("x", (1<<20)+1024)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RequestURI() != "/user/codespaces?per_page=30" {
			t.Fatalf("request=%q", r.URL.RequestURI())
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{"codespaces": []map[string]any{{"name": "large-space", "display_name": displayName}}}); err != nil {
			t.Fatal(err)
		}
	}))
	defer server.Close()

	c := newClient(GitHubCodespacesConfig{APIURL: server.URL}, Runtime{HTTP: server.Client()}, "redacted")
	items, err := c.listCodespaces(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("items=%d", len(items))
	}
	if items[0].Name != "large-space" || items[0].DisplayName != displayName {
		t.Fatalf("name=%q display_len=%d", items[0].Name, len(items[0].DisplayName))
	}
}

func TestClientCurrentUserUsesConfiguredAPIBase(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/user" || r.Header.Get("Authorization") != "Bearer ghp_this_token_value_is_redacted" {
			t.Fatalf("request path=%q auth_present=%t", r.URL.Path, r.Header.Get("Authorization") != "")
		}
		_, _ = w.Write([]byte(`{"id":42,"login":"alice"}`))
	}))
	defer server.Close()

	c := newClient(GitHubCodespacesConfig{APIURL: server.URL + "/api/v3"}, Runtime{HTTP: server.Client()}, "ghp_this_token_value_is_redacted")
	user, err := c.currentUser(context.Background())
	if err != nil || user.ID != 42 || user.Login != "alice" {
		t.Fatalf("user=%#v err=%v", user, err)
	}
}

func TestGitStatusDeletionSafetyRequiresAllFields(t *testing.T) {
	for _, test := range []struct {
		name    string
		payload string
		wantErr bool
	}{
		{name: "complete clean", payload: `{"name":"cs-safe","git_status":{"ahead":0,"has_unpushed_changes":false,"has_uncommitted_changes":false}}`},
		{name: "missing ahead", payload: `{"name":"cs-unknown","git_status":{"has_unpushed_changes":false,"has_uncommitted_changes":false}}`, wantErr: true},
		{name: "missing unpushed", payload: `{"name":"cs-unknown","git_status":{"ahead":0,"has_uncommitted_changes":false}}`, wantErr: true},
		{name: "missing uncommitted", payload: `{"name":"cs-unknown","git_status":{"ahead":0,"has_unpushed_changes":false}}`, wantErr: true},
		{name: "missing object", payload: `{"name":"cs-unknown"}`, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var item codespace
			if err := json.Unmarshal([]byte(test.payload), &item); err != nil {
				t.Fatal(err)
			}
			err := validateDeleteSafe(item)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateDeleteSafe() err=%v wantErr=%t", err, test.wantErr)
			}
		})
	}
}

func TestFlexibleRefsDecodeRESTObjectsAndGHStrings(t *testing.T) {
	for _, data := range []string{
		`{"repository":{"full_name":"example-org/my-app"},"machine":{"name":"standardLinux32gb"}}`,
		`{"repository":"example-org/my-app","machine":"standardLinux32gb"}`,
	} {
		var item codespace
		if err := json.Unmarshal([]byte(data), &item); err != nil {
			t.Fatalf("decode %s: %v", data, err)
		}
		if item.Repository.FullName != "example-org/my-app" {
			t.Fatalf("repository=%q", item.Repository.FullName)
		}
		if item.Machine.Name != "standardLinux32gb" {
			t.Fatalf("machine=%q", item.Machine.Name)
		}
	}
}

func TestClientLifecycleOperationsRequestShape(t *testing.T) {
	var callsMu sync.Mutex
	var calls []string
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callsMu.Lock()
		calls = append(calls, r.Method+" "+r.URL.RequestURI())
		callsMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.RequestURI() == "/api/v3/user/codespaces?per_page=30":
			w.Header().Set("Link", `<`+server.URL+`/api/v3/user/codespaces?per_page=30&page=2>; rel="next"`)
			_, _ = w.Write([]byte(`{"codespaces":[{"name":"space-1","state":"Available","repository":"example-org/my-app","machine":"standardLinux32gb"}]}`))
		case r.Method == http.MethodGet && r.URL.RequestURI() == "/api/v3/user/codespaces?per_page=30&page=2":
			_, _ = w.Write([]byte(`{"codespaces":[{"name":"space-2","state":"Shutdown","repository":"example-org/my-app","machine":"standardLinux32gb"}]}`))
		case r.Method == http.MethodGet && r.URL.RequestURI() == "/api/v3/user/codespaces/space-1":
			_, _ = w.Write([]byte(`{"name":"space-1","state":"Available","repository":"example-org/my-app","machine":"standardLinux32gb"}`))
		case r.Method == http.MethodPost && r.URL.RequestURI() == "/api/v3/user/codespaces/space-1/start":
			_, _ = w.Write([]byte(`{"name":"space-1","state":"Starting","repository":"example-org/my-app","machine":"standardLinux32gb"}`))
		case r.Method == http.MethodPost && r.URL.RequestURI() == "/api/v3/user/codespaces/space-2/start":
			w.WriteHeader(http.StatusNotModified)
		case r.Method == http.MethodPost && r.URL.RequestURI() == "/api/v3/user/codespaces/space-1/stop":
			w.WriteHeader(http.StatusAccepted)
		case r.Method == http.MethodPost && r.URL.RequestURI() == "/api/v3/user/codespaces/space-2/stop":
			w.WriteHeader(http.StatusNotModified)
		case r.Method == http.MethodDelete && r.URL.RequestURI() == "/api/v3/user/codespaces/space-1":
			w.WriteHeader(http.StatusAccepted)
		case r.Method == http.MethodDelete && r.URL.RequestURI() == "/api/v3/user/codespaces/space-2":
			w.WriteHeader(http.StatusNotModified)
		case r.Method == http.MethodGet && r.URL.RequestURI() == "/api/v3/repos/example-org/my-app/codespaces/machines?ref=main":
			_, _ = w.Write([]byte(`{"machines":[{"name":"standardLinux32gb","display_name":"Standard"}]}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.RequestURI())
		}
	}))
	defer server.Close()

	c := newClient(GitHubCodespacesConfig{APIURL: server.URL + "/api/v3"}, Runtime{HTTP: server.Client()}, "token")
	listed, err := c.listCodespaces(context.Background())
	if err != nil || len(listed) != 2 || listed[0].Name != "space-1" || listed[1].Name != "space-2" {
		t.Fatalf("listed=%#v err=%v", listed, err)
	}
	if got, err := c.getCodespace(context.Background(), "space-1"); err != nil || got.Name != "space-1" {
		t.Fatalf("get=%#v err=%v", got, err)
	}
	if got, err := c.startCodespace(context.Background(), "space-1"); err != nil || got.State != "Starting" {
		t.Fatalf("start=%#v err=%v", got, err)
	}
	if got, err := c.startCodespace(context.Background(), "space-2"); err != nil || got.Name != "space-2" {
		t.Fatalf("start no-op=%#v err=%v", got, err)
	}
	if err := c.stopCodespace(context.Background(), "space-1"); err != nil {
		t.Fatal(err)
	}
	if err := c.stopCodespace(context.Background(), "space-2"); err != nil {
		t.Fatal(err)
	}
	if err := c.deleteCodespace(context.Background(), "space-1"); err != nil {
		t.Fatal(err)
	}
	if err := c.deleteCodespace(context.Background(), "space-2"); err != nil {
		t.Fatal(err)
	}
	machines, err := c.listMachines(context.Background(), "example-org/my-app", "main")
	if err != nil || len(machines) != 1 || machines[0].Name != "standardLinux32gb" {
		t.Fatalf("machines=%#v err=%v", machines, err)
	}
	want := strings.Join([]string{
		"GET /api/v3/user/codespaces?per_page=30",
		"GET /api/v3/user/codespaces?per_page=30&page=2",
		"GET /api/v3/user/codespaces/space-1",
		"POST /api/v3/user/codespaces/space-1/start",
		"POST /api/v3/user/codespaces/space-2/start",
		"POST /api/v3/user/codespaces/space-1/stop",
		"POST /api/v3/user/codespaces/space-2/stop",
		"DELETE /api/v3/user/codespaces/space-1",
		"DELETE /api/v3/user/codespaces/space-2",
		"GET /api/v3/repos/example-org/my-app/codespaces/machines?ref=main",
	}, "\n")
	callsMu.Lock()
	gotCalls := append([]string(nil), calls...)
	callsMu.Unlock()
	if got := strings.Join(gotCalls, "\n"); got != want {
		t.Fatalf("calls:\n%s\nwant:\n%s", got, want)
	}
}

func TestClientListCodespacesRejectsCrossOriginPagination(t *testing.T) {
	var callsMu sync.Mutex
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callsMu.Lock()
		calls = append(calls, r.Method+" "+r.URL.RequestURI())
		callsMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Link", `<https://attacker.example/user/codespaces?page=2>; rel="next"`)
		_, _ = w.Write([]byte(`{"codespaces":[{"name":"space-1","state":"Available","repository":"example-org/my-app","machine":"standardLinux32gb"}]}`))
	}))
	defer server.Close()

	c := newClient(GitHubCodespacesConfig{APIURL: server.URL + "/api/v3"}, Runtime{HTTP: server.Client()}, "token")
	_, err := c.listCodespaces(context.Background())
	if err == nil || !strings.Contains(err.Error(), "outside configured API base") {
		t.Fatalf("err=%v", err)
	}
	callsMu.Lock()
	gotCalls := append([]string(nil), calls...)
	callsMu.Unlock()
	if got := strings.Join(gotCalls, "\n"); got != "GET /api/v3/user/codespaces?per_page=30" {
		t.Fatalf("calls=%q", got)
	}
}

func TestClientRejectsRedirectWithoutForwardingAuthorization(t *testing.T) {
	var sinkMu sync.Mutex
	sinkCalls := 0
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sinkMu.Lock()
		sinkCalls++
		sinkMu.Unlock()
		if got := r.Header.Get("Authorization"); got != "" {
			t.Fatalf("redirect leaked authorization=%q", got)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer sink.Close()

	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, sink.URL+r.URL.RequestURI(), http.StatusFound)
	}))
	defer origin.Close()

	c := newClient(GitHubCodespacesConfig{APIURL: origin.URL}, Runtime{HTTP: origin.Client()}, "ghp_this_token_value_is_redacted")
	_, err := c.listCodespaces(context.Background())
	if err == nil || !strings.Contains(err.Error(), "status=302") {
		t.Fatalf("err=%v", err)
	}
	sinkMu.Lock()
	gotSinkCalls := sinkCalls
	sinkMu.Unlock()
	if gotSinkCalls != 0 {
		t.Fatalf("redirect target calls=%d", gotSinkCalls)
	}
}

func TestClientRejectsInsecureNonLoopbackAPIBase(t *testing.T) {
	if err := validateGitHubCodespacesAPIBase("http://api.example.test"); err == nil || !strings.Contains(err.Error(), "must use https") {
		t.Fatalf("err=%v", err)
	}
	if err := validateGitHubCodespacesAPIBase("http://127.0.0.1:8080/api/v3"); err != nil {
		t.Fatalf("loopback URL: %v", err)
	}
}

func TestGitHubAPIErrorRedactsTokensAndReportsRetryAfter(t *testing.T) {
	err := githubAPIError(http.StatusForbidden, "3", `{"message":"bad token ghp_this_token_value_is_redacted"}`)
	if err == nil {
		t.Fatal("expected error")
	}
	text := err.Error()
	if strings.Contains(text, "ghp_this_token_value_is_redacted") {
		t.Fatalf("token leaked: %s", text)
	}
	for _, want := range []string{"status=403", "retry_after=3s", "check gh auth", "codespace scope"} {
		if !strings.Contains(text, want) {
			t.Fatalf("error %q missing %q", text, want)
		}
	}
}
