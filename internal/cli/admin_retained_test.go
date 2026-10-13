package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAdminRetainedReconcile(t *testing.T) {
	for _, jsonOutput := range []bool{false, true} {
		t.Run(map[bool]string{false: "human", true: "json"}[jsonOutput], func(t *testing.T) {
			clearConfigEnv(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/v1/admin/retained-reconcile" || r.Header.Get("Authorization") != "Bearer fixture-admin" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				_, _ = w.Write([]byte(`{"settings":{"mode":"on","delete":false,"budget":200,"lookupBudget":64,"graceSeconds":3600},"cycles":2,"scanned":400,"counters":{"aws":{"confirmed":20}},"backoff":{}}`))
			}))
			defer server.Close()
			t.Setenv("CRABBOX_COORDINATOR", server.URL)
			t.Setenv("CRABBOX_COORDINATOR_TOKEN", "fixture-user")
			t.Setenv("CRABBOX_COORDINATOR_ADMIN_TOKEN", "fixture-admin")
			args := []string{"admin", "retained-reconcile"}
			if jsonOutput {
				args = append(args, "--json")
			}
			var stdout, stderr bytes.Buffer
			if err := (App{Stdout: &stdout, Stderr: &stderr}).Run(t.Context(), args); err != nil {
				t.Fatal(err)
			}
			if jsonOutput {
				var result CoordinatorRetainedReconcile
				if err := json.Unmarshal(stdout.Bytes(), &result); err != nil || (result.Counters["aws"]["confirmed"] != 20 || result.Settings.LookupBudget != 64 || result.Settings.GraceSeconds != 3600) {
					t.Fatalf("invalid JSON: %s", stdout.String())
				}
			} else if !strings.Contains(stdout.String(), "aws confirmed=20") {
				t.Fatal(stdout.String())
			}
		})
	}
}
func TestAdminRetainedRejectsArguments(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := (App{Stdout: &stdout, Stderr: &stderr}).Run(t.Context(), []string{"admin", "retained-reconcile", "extra"}); err == nil {
		t.Fatal("accepted unexpected argument")
	}
}
