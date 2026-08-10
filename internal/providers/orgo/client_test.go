package orgo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRunBashExitCodeFieldPresence(t *testing.T) {
	tests := []struct {
		name string
		body string
		want int
	}{
		{
			name: "explicit camel zero wins over snake fallback",
			body: `{"stdout":"ok\n","exitCode":0,"exit_code":7}`,
			want: 0,
		},
		{
			name: "snake fallback",
			body: `{"stdout":"ok\n","exit_code":7}`,
			want: 7,
		},
		{
			name: "missing exit code defaults to success",
			body: `{"stdout":"ok\n"}`,
			want: 0,
		},
		{
			name: "explicit failure without exit code",
			body: `{"stdout":"ok\n","success":false}`,
			want: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					t.Fatalf("method=%s", r.Method)
				}
				if r.URL.Path != "/computers/computer_test/bash" {
					t.Fatalf("path=%s", r.URL.Path)
				}
				if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
					t.Fatalf("authorization=%q", got)
				}
				var req map[string]string
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Fatalf("decode request: %v", err)
				}
				if got := req["command"]; got != "printf ok" {
					t.Fatalf("command=%q", got)
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintln(w, tt.body)
			}))
			t.Cleanup(server.Close)

			client := &orgoHTTPClient{baseURL: server.URL, apiKey: "test-key", http: server.Client()}
			var stdout, stderr bytes.Buffer
			code, err := client.RunBash(context.Background(), "computer_test", "printf ok", &stdout, &stderr)
			if err != nil {
				t.Fatal(err)
			}
			if code != tt.want {
				t.Fatalf("exit=%d, want %d", code, tt.want)
			}
			if stdout.String() != "ok\n" {
				t.Fatalf("stdout=%q", stdout.String())
			}
			if stderr.String() != "" {
				t.Fatalf("stderr=%q", stderr.String())
			}
		})
	}
}

func TestStartComputerUsesOfficialActionEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method=%s", r.Method)
		}
		if r.URL.Path != "/computers/computer_test/start" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		fmt.Fprint(w, `{"success":true,"status":"starting"}`)
	}))
	t.Cleanup(server.Close)

	client := &orgoHTTPClient{baseURL: server.URL, apiKey: "test-key", http: server.Client()}
	if err := client.StartComputer(context.Background(), "computer_test"); err != nil {
		t.Fatal(err)
	}
}

func TestStartComputerRejectsUnsuccessfulResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"success":false,"status":"stopped"}`)
	}))
	t.Cleanup(server.Close)

	client := &orgoHTTPClient{baseURL: server.URL, apiKey: "test-key", http: server.Client()}
	if err := client.StartComputer(context.Background(), "computer_test"); err == nil || !strings.Contains(err.Error(), "did not start") {
		t.Fatalf("err=%v", err)
	}
}

func TestDeleteResourceRejectsUnsuccessfulResponse(t *testing.T) {
	for _, tt := range []struct {
		name string
		path string
		run  func(*orgoHTTPClient) error
	}{
		{name: "computer", path: "/computers/computer_test", run: func(client *orgoHTTPClient) error {
			return client.DeleteComputer(context.Background(), "computer_test")
		}},
		{name: "workspace", path: "/workspaces/workspace_test", run: func(client *orgoHTTPClient) error {
			return client.DeleteWorkspace(context.Background(), "workspace_test")
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodDelete || r.URL.Path != tt.path {
					t.Fatalf("request=%s %s", r.Method, r.URL.Path)
				}
				fmt.Fprint(w, `{"success":false,"status":"still_running"}`)
			}))
			t.Cleanup(server.Close)
			client := &orgoHTTPClient{baseURL: server.URL, apiKey: "test-key", http: server.Client()}
			if err := tt.run(client); err == nil || !strings.Contains(err.Error(), "did not delete") {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestGetWorkspaceReadsOfficialDesktopsField(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/workspaces/workspace_test" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"workspace_test","desktops":[{"id":"computer_test","status":"running"}]}`)
	}))
	t.Cleanup(server.Close)

	client := &orgoHTTPClient{baseURL: server.URL, apiKey: "test-key", http: server.Client()}
	workspace, err := client.GetWorkspace(context.Background(), "workspace_test")
	if err != nil {
		t.Fatal(err)
	}
	if len(workspace.Computers) != 1 || workspace.Computers[0].ID != "computer_test" {
		t.Fatalf("computers=%#v", workspace.Computers)
	}
	computers := orgoComputersForWorkspace(workspace)
	if computers[0].WorkspaceID != "workspace_test" {
		t.Fatalf("workspace id=%q", computers[0].WorkspaceID)
	}
}

func TestListWorkspacesReadsLiveProjectsEnvelope(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/workspaces" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"projects":[{"id":"workspace_test","name":"Test"}]}`)
	}))
	t.Cleanup(server.Close)

	client := &orgoHTTPClient{baseURL: server.URL, apiKey: "test-key", http: server.Client()}
	workspaces, err := client.ListWorkspaces(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(workspaces) != 1 || workspaces[0].ID != "workspace_test" {
		t.Fatalf("workspaces=%#v", workspaces)
	}
}

func TestOrgoClientRedactsReflectedAPIKey(t *testing.T) {
	const apiKey = "orgo-secret-token"
	client := &orgoHTTPClient{
		baseURL: "https://api.orgo.test",
		apiKey:  apiKey,
		http: &http.Client{Transport: orgoRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			if got := req.Header.Get("Authorization"); got != "Bearer "+apiKey {
				t.Fatalf("authorization=%q", got)
			}
			return &http.Response{
				StatusCode: http.StatusUnauthorized,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"message":"credential ` + apiKey + ` rejected","request":"safe-orgo-context"}`)),
				Request:    req,
			}, nil
		})},
	}
	_, err := client.GetWorkspace(context.Background(), "workspace_test")
	if err == nil {
		t.Fatal("GetWorkspace accepted unauthorized response")
	}
	var apiErr *orgoHTTPError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err=%T, want *orgoHTTPError", err)
	}
	if apiErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status=%d, want %d", apiErr.StatusCode, http.StatusUnauthorized)
	}
	for _, value := range []string{apiErr.Body, err.Error()} {
		if strings.Contains(value, apiKey) {
			t.Fatalf("API key leaked in %q", value)
		}
		if !strings.Contains(value, "[redacted]") || !strings.Contains(value, "safe-orgo-context") {
			t.Fatalf("redacted error lost useful context: %q", value)
		}
	}
}

func TestOrgoClientRedactsAPIKeyAcrossResponseLimit(t *testing.T) {
	const apiKey = "zxqv-orgo-boundary-secret-token"
	const safeContext = "safe-orgo-boundary-context|"
	prefixLen := orgoMaxResponseBodyBytes - len("[redacted]") - len(safeContext)
	body := safeContext + strings.Repeat("x", prefixLen) + apiKey + " trailing provider detail"
	client := &orgoHTTPClient{
		baseURL: "https://api.orgo.test",
		apiKey:  apiKey,
		http: &http.Client{Transport: orgoRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusUnauthorized,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(body)),
				Request:    req,
			}, nil
		})},
	}

	_, err := client.GetWorkspace(context.Background(), "workspace_test")
	var apiErr *orgoHTTPError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err=%v, want *orgoHTTPError", err)
	}
	if len(apiErr.Body) != orgoMaxResponseBodyBytes {
		t.Fatalf("diagnostic bytes=%d, want %d", len(apiErr.Body), orgoMaxResponseBodyBytes)
	}
	for _, leaked := range []string{apiKey, apiKey[:10]} {
		if strings.Contains(apiErr.Body, leaked) {
			t.Fatalf("API key fragment %q leaked across response limit", leaked)
		}
	}
	if !strings.Contains(apiErr.Body, safeContext) || !strings.HasSuffix(apiErr.Body, "[redacted]") {
		t.Fatalf("bounded diagnostic lost safe context or redaction marker")
	}
}

func TestOrgoClientRepeatedRedactionsDoNotExposeBoundaryFragment(t *testing.T) {
	const apiKey = "zxqv-orgo-repeated-boundary-secret-token"
	const safeContext = "safe-orgo-repeated-boundary-context|"
	prefix := safeContext + apiKey + "|" + apiKey + "|"
	body := prefix + strings.Repeat("x", orgoMaxResponseBodyBytes-len(prefix)) + apiKey
	client := &orgoHTTPClient{
		baseURL: "https://api.orgo.test",
		apiKey:  apiKey,
		http: &http.Client{Transport: orgoRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusUnauthorized,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(body)),
				Request:    req,
			}, nil
		})},
	}

	_, err := client.GetWorkspace(context.Background(), "workspace_test")
	var apiErr *orgoHTTPError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err=%v, want *orgoHTTPError", err)
	}
	if len(apiErr.Body) != orgoMaxResponseBodyBytes {
		t.Fatalf("diagnostic bytes=%d, want %d", len(apiErr.Body), orgoMaxResponseBodyBytes)
	}
	for prefixLen := 1; prefixLen <= len(apiKey); prefixLen++ {
		if strings.HasSuffix(apiErr.Body, apiKey[:prefixLen]) {
			t.Fatalf("API key prefix of %d bytes leaked at response boundary", prefixLen)
		}
	}
	if !strings.Contains(apiErr.Body, safeContext) || strings.Count(apiErr.Body, "[redacted]") != 2 {
		t.Fatalf("bounded diagnostic lost safe context or complete redactions")
	}
}

func TestOrgoClientMasksOverlappingAPIKeyMatches(t *testing.T) {
	apiKey := strings.Repeat("a", 24)
	const safeContext = "safe-orgo-overlap-context|"
	body := safeContext + strings.Repeat("a", 2*len(apiKey)-1)
	client := &orgoHTTPClient{
		baseURL: "https://api.orgo.test",
		apiKey:  apiKey,
		http: &http.Client{Transport: orgoRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusUnauthorized,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(body)),
				Request:    req,
			}, nil
		})},
	}

	_, err := client.GetWorkspace(context.Background(), "workspace_test")
	var apiErr *orgoHTTPError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err=%v, want *orgoHTTPError", err)
	}
	if strings.Contains(apiErr.Body, apiKey[:len(apiKey)-1]) {
		t.Fatalf("overlapping API key fragment leaked in %q", apiErr.Body)
	}
	if !strings.Contains(apiErr.Body, safeContext) || !strings.Contains(apiErr.Body, "[redacted]") {
		t.Fatalf("overlap-safe diagnostic lost context or redaction marker: %q", apiErr.Body)
	}
}

func TestOrgoActionErrorsRedactReflectedAPIKey(t *testing.T) {
	const apiKey = "orgo-semantic-secret-token"
	const safeContext = "safe-orgo-semantic-context"
	for _, tt := range []struct {
		name   string
		method string
		path   string
		run    func(*orgoHTTPClient) error
	}{
		{
			name:   "start computer",
			method: http.MethodPost,
			path:   "/computers/computer_test/start",
			run: func(client *orgoHTTPClient) error {
				return client.StartComputer(context.Background(), "computer_test")
			},
		},
		{
			name:   "delete computer",
			method: http.MethodDelete,
			path:   "/computers/computer_test",
			run: func(client *orgoHTTPClient) error {
				return client.DeleteComputer(context.Background(), "computer_test")
			},
		},
		{
			name:   "delete workspace",
			method: http.MethodDelete,
			path:   "/workspaces/workspace_test",
			run: func(client *orgoHTTPClient) error {
				return client.DeleteWorkspace(context.Background(), "workspace_test")
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := &orgoHTTPClient{
				baseURL: "https://api.orgo.test",
				apiKey:  apiKey,
				http: &http.Client{Transport: orgoRoundTripFunc(func(req *http.Request) (*http.Response, error) {
					if req.Method != tt.method || req.URL.Path != tt.path {
						t.Fatalf("request=%s %s, want %s %s", req.Method, req.URL.Path, tt.method, tt.path)
					}
					if got := req.Header.Get("Authorization"); got != "Bearer "+apiKey {
						t.Fatalf("authorization=%q", got)
					}
					body := `{"success":false,"status":"credential ` + apiKey + ` rejected; ` + safeContext + `"}`
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     make(http.Header),
						Body:       io.NopCloser(strings.NewReader(body)),
						Request:    req,
					}, nil
				})},
			}

			err := tt.run(client)
			if err == nil {
				t.Fatal("semantic failure was accepted")
			}
			if strings.Contains(err.Error(), apiKey) || !strings.Contains(err.Error(), "[redacted]") || !strings.Contains(err.Error(), safeContext) {
				t.Fatalf("semantic diagnostic=%q", err)
			}
		})
	}
}

type orgoRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn orgoRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func TestNewOrgoClientRejectsInsecureNonLoopbackAPIBase(t *testing.T) {
	t.Setenv("CRABBOX_ORGO_API_KEY", "test-key")
	t.Setenv("CRABBOX_ORGO_API_BASE", "http://api.example.test")
	if _, err := newOrgoClient(Config{}, Runtime{}); err == nil || !strings.Contains(err.Error(), "must use https") {
		t.Fatalf("err=%v, want HTTPS requirement", err)
	}
}

func TestNewOrgoClientUsesResolvedConfigBeforeAmbientAPIBase(t *testing.T) {
	t.Setenv("CRABBOX_ORGO_API_KEY", "test-key")
	t.Setenv("CRABBOX_ORGO_API_BASE", "https://ambient.example.test")
	client, err := newOrgoClient(Config{Orgo: OrgoConfig{APIBase: "https://flag-selected.example.test"}}, Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if got := client.(*orgoHTTPClient).baseURL; got != "https://flag-selected.example.test" {
		t.Fatalf("base URL=%q", got)
	}
}
