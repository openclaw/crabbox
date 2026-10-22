package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAdminLifetimeReadbackPreservesOriginalRootEvidence(t *testing.T) {
	for _, evidence := range []string{"absent", "null", "observed"} {
		t.Run(evidence, func(t *testing.T) {
			clearConfigEnv(t)
			response := map[string]any{
				"leaseId": "cbx_abcdef123456", "provider": "aws", "region": "eu-west-1",
				"instanceId": "i-0123456789abcdef0", "instanceState": "terminated",
				"rootVolumeId": "vol-0123456789abcdef0", "rootVolumePresent": false,
				"observedAt": "2026-10-20T12:00:00Z",
			}
			if evidence == "null" {
				response["rootIdentityEvidence"] = nil
			} else if evidence == "observed" {
				response["rootIdentityEvidence"] = map[string]string{
					"eventID":          "00000000-0000-4000-8000-000000000001",
					"eventTime":        "2026-10-20T11:00:00Z",
					"rawProviderEvent": "private-provider-payload",
				}
			}
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != http.MethodGet || r.URL.Path != "/v1/admin/leases/cbx_abcdef123456/lifetime-readback" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				if r.Header.Get("Authorization") != "Bearer fixture-admin" {
					t.Error("readback did not retain the configured admin identity")
				}
				if err := json.NewEncoder(w).Encode(response); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			t.Setenv("CRABBOX_COORDINATOR", server.URL)
			t.Setenv("CRABBOX_COORDINATOR_TOKEN", "fixture-user")
			t.Setenv("CRABBOX_COORDINATOR_ADMIN_TOKEN", "fixture-admin")
			var stdout, stderr bytes.Buffer
			if err := (App{Stdout: &stdout, Stderr: &stderr}).Run(t.Context(), []string{"admin", "lifetime-readback", "--id", "cbx_abcdef123456", "--json"}); err != nil {
				t.Fatalf("readback: %v; stderr=%s", err, stderr.String())
			}
			if calls != 1 {
				t.Fatalf("expected one read-only request, got %d", calls)
			}
			var output map[string]any
			if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
				t.Fatal(err)
			}
			if output["rootVolumePresent"] != false || output["instanceState"] != "terminated" {
				t.Fatalf("original provider observation changed: %s", stdout.String())
			}
			got, present := output["rootIdentityEvidence"]
			if evidence != "observed" {
				if present {
					t.Fatalf("invented original allocation evidence: %s", stdout.String())
				}
				return
			}
			fields, ok := got.(map[string]any)
			if !ok || len(fields) != 2 || fields["eventID"] != "00000000-0000-4000-8000-000000000001" || fields["eventTime"] != "2026-10-20T11:00:00Z" {
				t.Fatalf("original allocation evidence lost or changed: %s", stdout.String())
			}
			if strings.Contains(stdout.String(), "private-provider-payload") {
				t.Fatal("raw provider payload leaked into readback")
			}
		})
	}
}
