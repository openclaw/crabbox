package daytona

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apidaytona "github.com/daytonaio/daytona/libs/api-client-go"
)

func TestDaytonaListCrabboxSandboxesUsesCursorPagination(t *testing.T) {
	var cursors []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sandbox" {
			t.Fatalf("path=%s, want /sandbox", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer api-token" {
			t.Fatalf("authorization=%q", got)
		}
		if got := r.Header.Get("X-Daytona-Organization-ID"); got != "org-123" {
			t.Fatalf("organization=%q", got)
		}
		if got := r.URL.Query().Get("limit"); got != "100" {
			t.Fatalf("limit=%q", got)
		}
		if got := r.URL.Query().Get("labels"); got != `{"crabbox":"true"}` {
			t.Fatalf("labels=%q", got)
		}
		cursors = append(cursors, r.URL.Query().Get("cursor"))
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("cursor") {
		case "":
			_, _ = w.Write([]byte(`{"items":[` + daytonaListItemJSON("sandbox-one", "slug-one") + `],"nextCursor":"cursor-2"}`))
		case "cursor-2":
			_, _ = w.Write([]byte(`{"items":[` + daytonaListItemJSON("sandbox-two", "slug-two") + `],"nextCursor":null}`))
		default:
			t.Fatalf("unexpected cursor=%q", r.URL.Query().Get("cursor"))
		}
	}))
	defer srv.Close()

	apiCfg := apidaytona.NewConfiguration()
	apiCfg.Servers = apidaytona.ServerConfigurations{{URL: srv.URL}}
	apiCfg.HTTPClient = srv.Client()
	client := &daytonaSDKClient{api: apidaytona.NewAPIClient(apiCfg), token: "api-token", orgID: "org-123"}

	got, err := client.ListCrabboxSandboxes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("sandboxes=%d, want 2", len(got))
	}
	if got[0].GetId() != "sandbox-one" || got[0].GetLabels()["slug"] != "slug-one" {
		t.Fatalf("first sandbox=%#v labels=%#v", got[0], got[0].GetLabels())
	}
	if got[1].GetId() != "sandbox-two" || got[1].GetLabels()["slug"] != "slug-two" {
		t.Fatalf("second sandbox=%#v labels=%#v", got[1], got[1].GetLabels())
	}
	resolved, leaseID, err := resolveDaytonaSandbox(context.Background(), client, Config{}, "slug-two")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.GetId() != "sandbox-two" || leaseID != "cbx_222222222222" {
		t.Fatalf("resolved=%s lease=%s, want sandbox-two cbx_222222222222", resolved.GetId(), leaseID)
	}
	if len(cursors) != 4 || cursors[0] != "" || cursors[1] != "cursor-2" || cursors[2] != "" || cursors[3] != "cursor-2" {
		t.Fatalf("cursors=%#v, want two cursor-paginated inventory scans", cursors)
	}
}

func TestResolveDaytonaSandboxDirectLookupErrorHandling(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/sandbox":
			_, _ = w.Write([]byte(`{"items":[],"nextCursor":null}`))
		case "/sandbox/direct-one":
			_, _ = w.Write([]byte(daytonaListItemJSON("direct-one", "direct-slug")))
		case "/sandbox/unowned":
			unowned := strings.Replace(daytonaListItemJSON("unowned", "unowned"), `"provider": "daytona", `, "", 1)
			_, _ = w.Write([]byte(unowned))
		case "/sandbox/missing":
			http.Error(w, `{"message":"sandbox not found"}`, http.StatusNotFound)
		case "/sandbox/denied":
			http.Error(w, `{"authorization":"Bearer api-token","message":"bad token api-token"}`, http.StatusUnauthorized)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	apiCfg := apidaytona.NewConfiguration()
	apiCfg.Servers = apidaytona.ServerConfigurations{{URL: srv.URL}}
	apiCfg.HTTPClient = srv.Client()
	client := &daytonaSDKClient{api: apidaytona.NewAPIClient(apiCfg), token: "api-token"}

	got, leaseID, err := resolveDaytonaSandbox(context.Background(), client, Config{}, "direct-one")
	if err != nil {
		t.Fatal(err)
	}
	if got.GetId() != "direct-one" || leaseID != "cbx_111111111111" {
		t.Fatalf("direct lookup=%s lease=%s, want direct-one cbx_111111111111", got.GetId(), leaseID)
	}

	_, _, err = resolveDaytonaSandbox(context.Background(), client, Config{}, "missing")
	var exitErr ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 4 || !strings.Contains(err.Error(), "daytona sandbox not found: missing") {
		t.Fatalf("missing err=%v, want local not-found exit", err)
	}

	_, _, err = resolveDaytonaSandbox(context.Background(), client, Config{}, "denied")
	if err == nil || !strings.Contains(err.Error(), "daytona get sandbox: 401 Unauthorized") {
		t.Fatalf("denied err=%v, want preserved get-sandbox failure", err)
	}
	if strings.Contains(err.Error(), "api-token") {
		t.Fatalf("denied err=%v still contains token", err)
	}
	if strings.Contains(err.Error(), "sandbox not found") {
		t.Fatalf("denied err=%v, want no not-found rewrite", err)
	}

	_, _, err = resolveDaytonaSandbox(context.Background(), client, Config{}, "unowned")
	if !errors.As(err, &exitErr) || exitErr.Code != 4 || !strings.Contains(err.Error(), "is not owned by Crabbox") {
		t.Fatalf("unowned err=%v, want ownership refusal", err)
	}
}

func TestParseDaytonaCLIAuthConfigRejectsUnknownActiveProfile(t *testing.T) {
	_, err := parseDaytonaCLIAuthConfig([]byte(`{
  "activeProfile": "missing",
  "profiles": [
    {"id": "dev", "name": "dev", "api": {"url": "https://dev.example/api", "key": "dev-key"}},
    {"id": "prod", "name": "prod", "api": {"url": "https://prod.example/api", "key": "prod-key"}}
  ]
}`))
	if err == nil {
		t.Fatal("expected unknown active profile error")
	}
	if !strings.Contains(err.Error(), `active profile "missing" was not found`) {
		t.Fatalf("err=%v, want missing active profile", err)
	}
}

func TestParseDaytonaCLIAuthConfigFallsBackWhenNoActiveProfile(t *testing.T) {
	auth, err := parseDaytonaCLIAuthConfig([]byte(`{
  "profiles": [
    {
      "id": "dev",
      "name": "dev",
      "activeOrganizationId": "org-dev",
      "api": {"url": "https://dev.example/api", "key": "dev-key"}
    },
    {
      "id": "prod",
      "name": "prod",
      "api": {"url": "https://prod.example/api", "key": "prod-key"}
    }
  ]
}`))
	if err != nil {
		t.Fatal(err)
	}
	if auth.APIKey != "dev-key" || auth.OrganizationID != "org-dev" || auth.APIURL != "https://dev.example/api" {
		t.Fatalf("auth=%#v", auth)
	}
}

func daytonaListItemJSON(id, slug string) string {
	leaseID := "cbx_111111111111"
	if strings.HasSuffix(id, "two") {
		leaseID = "cbx_222222222222"
	}
	return `{
		"id": "` + id + `",
		"organizationId": "org-123",
		"name": "` + id + `",
		"target": "us",
		"user": "daytona",
		"env": {},
		"public": false,
		"networkBlockAll": false,
		"cpu": 2,
		"gpu": 0,
		"memory": 4,
		"disk": 20,
		"labels": {"crabbox": "true", "provider": "daytona", "lease": "` + leaseID + `", "slug": "` + slug + `"},
		"toolboxProxyUrl": "https://toolbox.example/` + id + `",
		"state": "started"
	}`
}
