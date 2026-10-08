package azuresandbox

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
)

type credentialFunc func(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error)

func (f credentialFunc) GetToken(ctx context.Context, o policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return f(ctx, o)
}

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func fixtureClient(t *testing.T, transport transportFunc) *client {
	t.Helper()
	c, err := newClient("westus3", "subscription", "workers", "sandboxes", credentialFunc(func(_ context.Context, o policy.TokenRequestOptions) (azcore.AccessToken, error) {
		if len(o.Scopes) != 1 || o.Scopes[0] != "https://dynamicsessions.io/.default" {
			t.Fatalf("unexpected audience: %v", o.Scopes)
		}
		return azcore.AccessToken{Token: "synthetic-token", ExpiresOn: time.Now().Add(time.Hour)}, nil
	}), transport)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func response(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func TestSandboxWireOperations(t *testing.T) {
	steps := []struct{ method, suffix, body string }{
		{"PUT", "/sandboxes", `{"id":"sandbox-id","state":"Running","labels":{"attempt":"one"}}`},
		{"GET", "/sandboxes/sandbox-id", `{"id":"sandbox-id","state":"Running"}`},
		{"POST", "/sandboxes/sandbox-id/executeShellCommand", `{"exitCode":7,"stdout":"out","stderr":"err"}`},
		{"PUT", "/sandboxes/sandbox-id/files", ""},
		{"DELETE", "/sandboxes/sandbox-id", ""},
	}
	i := 0
	c := fixtureClient(t, func(r *http.Request) (*http.Response, error) {
		if i >= len(steps) {
			t.Fatal("unexpected retry")
		}
		step := steps[i]
		i++
		if r.Method != step.method || r.URL.Path != "/subscriptions/subscription/resourceGroups/workers/sandboxGroups/sandboxes"+step.suffix {
			t.Fatalf("wrong request: %s %s", r.Method, r.URL.Path)
		}
		if r.URL.Query().Get("api-version") != "2026-02-01-preview" || r.Header.Get("Authorization") != "Bearer synthetic-token" {
			t.Fatal("missing API version or authentication")
		}
		if r.Method == "POST" {
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body["command"] != "exit 7" || body["workingDirectory"] != "/workspace" {
				t.Fatalf("wrong exec body: %v", body)
			}
		}
		if strings.HasSuffix(r.URL.Path, "/files") {
			b, _ := io.ReadAll(r.Body)
			if string(b) != "payload" || r.URL.Query().Get("path") != "/workspace/input" || r.URL.Query().Get("createDirs") != "true" {
				t.Fatal("wrong file upload")
			}
		}
		return response(200, step.body), nil
	})
	ctx := context.Background()
	if _, err := c.Create(ctx, createRequest{Labels: map[string]string{"attempt": "one"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(ctx, "sandbox-id"); err != nil {
		t.Fatal(err)
	}
	r, err := c.Exec(ctx, "sandbox-id", "exit 7", "/workspace")
	if err != nil || r.ExitCode == nil || *r.ExitCode != 7 || r.Stdout != "out" || r.Stderr != "err" {
		t.Fatalf("command result: %+v, %v", r, err)
	}
	if err := c.Upload(ctx, "sandbox-id", "/workspace/input", strings.NewReader("payload")); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, "sandbox-id"); err != nil {
		t.Fatal(err)
	}
	if i != len(steps) {
		t.Fatal("missing operation")
	}
}

func TestUnknownCreateAndCommandAreNeverRetried(t *testing.T) {
	for _, status := range []int{302, 401, 429, 500} {
		calls := 0
		c := fixtureClient(t, func(r *http.Request) (*http.Response, error) {
			calls++
			res := response(status, "private service response")
			res.Header.Set("Location", "https://other.example/token")
			return res, nil
		})
		_, err := c.Create(context.Background(), createRequest{})
		var apiErr *apiError
		if !errors.As(err, &apiErr) || apiErr.Status != status || calls != 1 || strings.Contains(err.Error(), "private") {
			t.Fatalf("wrong uncertain outcome: %v, calls=%d", err, calls)
		}
	}
	c := fixtureClient(t, func(*http.Request) (*http.Response, error) { return response(200, `{}`), nil })
	if _, err := c.Create(context.Background(), createRequest{}); err == nil {
		t.Fatal("accepted missing ID")
	}
	if _, err := c.Exec(context.Background(), "sandbox-id", "true", ""); err == nil {
		t.Fatal("accepted missing exit status")
	}
}

func TestInventoryPaginationStaysInGroup(t *testing.T) {
	for _, next := range []string{
		"https://other.example/sandboxes", "https://management.westus3.azuredevcompute.io/subscriptions/subscription/resourceGroups/workers/sandboxGroups/other/sandboxes",
	} {
		calls := 0
		c := fixtureClient(t, func(*http.Request) (*http.Response, error) {
			calls++
			b, _ := json.Marshal(map[string]any{"value": []any{}, "nextLink": next})
			return response(200, string(b)), nil
		})
		if _, err := c.List(context.Background()); err == nil || calls != 1 {
			t.Fatal("followed foreign continuation")
		}
	}
	calls := 0
	c := fixtureClient(t, func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return response(200, `{"value":[{"id":"one"}],"nextLink":"https://management.westus3.azuredevcompute.io/subscriptions/subscription/resourceGroups/workers/sandboxGroups/sandboxes/sandboxes?skip=one"}`), nil
		}
		if r.URL.Query().Get("skip") != "one" {
			t.Fatal("lost continuation")
		}
		return response(200, `[{"id":"two"}]`), nil
	})
	items, err := c.List(context.Background())
	if err != nil || len(items) != 2 || items[1].ID != "two" {
		t.Fatalf("inventory: %+v %v", items, err)
	}
}

func TestSandboxInventoryRejectsMissingResourceArray(t *testing.T) {
	for _, body := range []string{`null`, `{}`, `{"value":null}`, `{"error":{"code":"Unavailable"}}`} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, body)
			}))
			defer server.Close()
			c := fixtureClient(t, nil)
			c.endpoint, c.http = server.URL, server.Client()
			if _, err := c.List(t.Context()); err == nil {
				t.Fatal("malformed inventory was accepted as proven empty")
			}
		})
	}
}
