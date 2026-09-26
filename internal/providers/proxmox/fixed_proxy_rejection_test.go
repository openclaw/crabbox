package proxmox

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestProxmoxFixedCloneProxyRejectionRetainsCustody(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		for _, response := range []string{"html", "gateway-json", "task-data"} {
			t.Run(fmt.Sprintf("%d/%s", code, response), func(t *testing.T) {
				backend, _, req := fixedProxmoxFixture(t)
				clones := 0
				api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var data any
					switch r.URL.Path {
					case "/api2/json/access/permissions":
						data = map[string]any{r.URL.Query().Get("path"): map[string]int{"VM.Audit": 1}}
					case "/api2/json/cluster/resources":
						data = []any{}
					case "/api2/json/cluster/nextid":
						data = 417
					case "/api2/json/nodes/pve1/qemu/9400/clone":
						// A gateway can replace the reply after the upstream accepted
						// a clone but before its VM becomes visible in inventory.
						clones++
						if response == "html" {
							http.Error(w, "<html>gateway access denied</html>", code)
							return
						}
						w.Header().Set("Content-Type", "application/json")
						if response == "task-data" {
							w.Header().Set("Server", "pve-api-daemon/3.0")
							data = "UPID:accepted-clone"
						}
						w.WriteHeader(code)
						_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "message": "gateway access denied"})
						return
					default:
						t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
						http.Error(w, "unexpected", 500)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
				}))
				t.Cleanup(api.Close)
				backend.Cfg.Proxmox.APIURL = api.URL
				newClient = func(cfg core.Config) (proxmoxClient, error) { return core.NewProxmoxClient(cfg) }
				_, err := backend.Acquire(t.Context(), req)
				if err == nil {
					t.Fatal("expected ambiguous clone response")
				}
				claim, exists, readErr := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
				if readErr != nil || !exists || claim.CloudID != "417" || claim.FixedCreateIntent == nil || claim.FixedCreateIntent.State != "prepared" {
					t.Fatalf("proxy response erased clone custody: exists=%t claim=%+v read=%v acquire=%v", exists, claim, readErr, err)
				}
				if !strings.Contains(err.Error(), "claim retained") {
					t.Fatalf("missing recovery advice: %v", err)
				}
				if _, err := backend.Acquire(t.Context(), req); err == nil || clones != 1 {
					t.Fatalf("ambiguous clone resubmitted: clones=%d err=%v", clones, err)
				}
			})
		}
	}
}
