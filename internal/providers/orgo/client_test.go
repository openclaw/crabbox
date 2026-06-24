package orgo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

func TestHTTPErrorRedactsAPIKeyFromResponseBody(t *testing.T) {
	const apiKey = "orgo-secret-token"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `upstream echoed Authorization: Bearer orgo-secret-token and raw orgo-secret-token`, http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)

	client := &orgoHTTPClient{baseURL: server.URL, apiKey: apiKey, http: server.Client()}
	_, err := client.ListWorkspaces(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	got := err.Error()
	if strings.Contains(got, apiKey) {
		t.Fatalf("error leaked api key: %q", got)
	}
	if !strings.Contains(got, "Bearer [REDACTED]") || !strings.Contains(got, "raw [REDACTED]") {
		t.Fatalf("error was not redacted as expected: %q", got)
	}
}

func TestNewOrgoClientRejectsInsecureNonLoopbackAPIBase(t *testing.T) {
	t.Setenv("CRABBOX_ORGO_API_KEY", "test-key")
	t.Setenv("CRABBOX_ORGO_API_BASE", "http://api.example.test")
	if _, err := newOrgoClient(Config{}, Runtime{}); err == nil || !strings.Contains(err.Error(), "must use https") {
		t.Fatalf("err=%v, want HTTPS requirement", err)
	}
}
