package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestUsageCommittedCostCompatibility(t *testing.T) {
	for _, committed := range []string{"", `,"committedUSD":0`, `,"committedUSD":1.25`} {
		for _, jsonOut := range []bool{false, true} {
			t.Run(fmt.Sprintf("field=%s/json=%t", committed, jsonOut), func(t *testing.T) {
				clearConfigEnv(t)
				t.Chdir(t.TempDir())
				t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/v1/usage" || r.URL.Query().Get("month") != "2026-05" {
						t.Errorf("unexpected request: %s", r.URL)
					}
					fmt.Fprintf(w, `{"usage":{"month":"2026-05","scope":"user","leases":1,"activeLeases":0,"runtimeSeconds":3600,"estimatedUSD":1.25,"reservedUSD":10%s,"byProvider":[{"key":"aws","leases":1,"activeLeases":0,"runtimeSeconds":3600,"estimatedUSD":1.25,"reservedUSD":10%s}]},"limits":{"maxMonthlyUSD":100}}`, committed, committed)
				}))
				defer server.Close()
				t.Setenv("CRABBOX_COORDINATOR", server.URL)
				t.Setenv("CRABBOX_COORDINATOR_TOKEN", "synthetic-usage-token")
				var out, stderr bytes.Buffer
				args := []string{"usage", "--month", "2026-05"}
				if jsonOut {
					args = append(args, "--json")
				}
				if err := (App{Stdout: &out, Stderr: &stderr}).Run(context.Background(), args); err != nil {
					t.Fatal(err)
				}
				if jsonOut {
					var response struct{ Usage map[string]any }
					if err := json.Unmarshal(out.Bytes(), &response); err != nil {
						t.Fatal(err)
					}
					if response.Usage["reservedUSD"] != float64(10) {
						t.Fatalf("reservation changed: %s", &out)
					}
					value, present := response.Usage["committedUSD"]
					group := response.Usage["byProvider"].([]any)[0].(map[string]any)
					if present != (committed != "") || value != group["committedUSD"] {
						t.Fatalf("committed field compatibility lost: %s", &out)
					}
					wantValue := float64(0)
					if strings.Contains(committed, "1.25") {
						wantValue = 1.25
					}
					if committed != "" && value != wantValue {
						t.Fatalf("wrong committed value: %s", &out)
					}
				} else {
					if !strings.Contains(out.String(), "reserved=$10.00") || !strings.Contains(out.String(), "fleet=$100.00") {
						t.Fatalf("legacy output lost: %s", &out)
					}
					want := ""
					if committed != "" {
						want = " committed=$0.00"
						if strings.Contains(committed, "1.25") {
							want = " committed=$1.25"
						}
					}
					if want == "" && strings.Contains(out.String(), "committed=") || want != "" && strings.Count(out.String(), want) != 2 {
						t.Fatalf("committed rendering mismatch: %s", &out)
					}
				}
			})
		}
	}
}
