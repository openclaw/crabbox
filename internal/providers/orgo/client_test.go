package orgo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
