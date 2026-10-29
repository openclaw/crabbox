package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAdminRetireCreatePreviewAndApply(t *testing.T) {
	for _, mode := range []string{"preview", "retired", "pending", "identity-mismatch", "inconsistent-cleanup"} {
		t.Run(mode, func(t *testing.T) {
			clearConfigEnv(t)
			id, revision := "cbx_abcdef123456", strings.Repeat("a", 64)
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != "/v1/admin/leases/"+id+"/retire-create" || r.Header.Get("Authorization") != "Bearer fixture-admin" {
					t.Errorf("wrong route or authentication")
				}
				if mode == "preview" {
					if r.Method != http.MethodGet {
						t.Errorf("preview mutated")
					}
				} else {
					if r.Method != http.MethodPost {
						t.Errorf("expected POST")
					}
					var input map[string]string
					if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
						t.Fatal(err)
					}
					if input["revision"] != revision || input["owner"] != "alice@example.com" || input["org"] != "fixture-org" || input["reason"] != "lost token" || input["createdBefore"] != "2026-01-01T00:00:00Z" {
						t.Errorf("incorrect recovery preconditions: %v", input)
					}
				}
				result := CoordinatorCreateRetirement{LeaseID: id, Revision: revision, Binding: "unbound", AttemptState: "canceled", Fenced: true, Cleanup: "not-allocated"}
				if mode == "pending" {
					result.Binding, result.Cleanup = "bound", "pending"
				}
				if mode == "identity-mismatch" {
					result.LeaseID = "cbx_111111111111"
				}
				if mode == "inconsistent-cleanup" {
					result.Binding = "absent"
				}
				_ = json.NewEncoder(w).Encode(result)
			}))
			defer server.Close()
			t.Setenv("CRABBOX_COORDINATOR", server.URL)
			t.Setenv("CRABBOX_COORDINATOR_TOKEN", "fixture-user")
			t.Setenv("CRABBOX_COORDINATOR_ADMIN_TOKEN", "fixture-admin")
			args := []string{"admin", "retire-create", "--id", id, "--json"}
			if mode != "preview" {
				args = append(args, "--force", "--revision", revision, "--owner", "alice@example.com", "--org", "fixture-org", "--created-before", "2026-01-01T00:00:00Z", "--reason", "lost token")
			}
			var stdout, stderr bytes.Buffer
			err := (App{Stdout: &stdout, Stderr: &stderr}).Run(t.Context(), args)
			wantError := mode == "pending" || mode == "identity-mismatch" || mode == "inconsistent-cleanup"
			if (err != nil) != wantError {
				t.Fatalf("error=%v, wantError=%t", err, wantError)
			}
			if calls != 1 {
				t.Fatalf("unexpected replay: %d requests", calls)
			}
			if mode == "pending" && !strings.Contains(stdout.String(), `"cleanup":"pending"`) {
				t.Fatal("pending cleanup not visible")
			}
			if mode == "identity-mismatch" && stdout.Len() != 0 {
				t.Fatal("printed mismatched identity")
			}
		})
	}
}

func TestAdminRetireCreateRejectsIncompleteAuthority(t *testing.T) {
	for _, args := range [][]string{
		{}, {"--id", "friendly"}, {"--id", "cbx_abcdef123456", "--force"},
		{"--id", "cbx_abcdef123456", "--revision", strings.Repeat("a", 64)},
		{"--id", "cbx_abcdef123456", "--force", "--revision", strings.Repeat("a", 64), "--owner", "alice@example.com", "--org", "fixture-org", "--created-before", "2999-01-01T00:00:00Z", "--reason", "lost token"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			clearConfigEnv(t)
			var stdout, stderr bytes.Buffer
			if err := (App{Stdout: &stdout, Stderr: &stderr}).Run(t.Context(), append([]string{"admin", "retire-create"}, args...)); err == nil {
				t.Fatal("accepted incomplete retirement authority")
			}
		})
	}
}
