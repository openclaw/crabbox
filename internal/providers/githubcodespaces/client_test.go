package githubcodespaces

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestClientCreateCodespaceRequestShape(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		if r.Header.Get("Accept") != "application/vnd.github+json" || r.Header.Get("X-GitHub-Api-Version") != "2022-11-28" {
			t.Fatalf("missing GitHub headers: %#v", r.Header)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"codespace-1","display_name":"Crabbox","state":"Available","environment_id":"env_1","repository":{"full_name":"example-org/my-app"},"machine":{"name":"standardLinux32gb"}}`))
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
		gotBody["location"] != "UsWest" ||
		gotBody["idle_timeout_minutes"].(float64) != 2 ||
		gotBody["retention_period_minutes"].(float64) != 1440 ||
		gotBody["display_name"] != "Crabbox" {
		t.Fatalf("body=%#v", gotBody)
	}
	if created.Repository.FullName != "example-org/my-app" || created.EnvironmentID != "env_1" || created.Machine.Name != "standardLinux32gb" {
		t.Fatalf("created=%#v", created)
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

func TestGitHubAPIErrorRedactsTokensAndReportsRetryAfter(t *testing.T) {
	err := githubAPIError(http.StatusForbidden, "3", `{"message":"bad token ghp_this_token_value_is_redacted"}`)
	if err == nil {
		t.Fatal("expected error")
	}
	text := err.Error()
	if strings.Contains(text, "ghp_this_token_value_is_redacted") {
		t.Fatalf("token leaked: %s", text)
	}
	for _, want := range []string{"status=403", "retry_after=3s", "check gh auth"} {
		if !strings.Contains(text, want) {
			t.Fatalf("error %q missing %q", text, want)
		}
	}
}
