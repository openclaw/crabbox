package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCoordinatorJSONPreservesStoredProviderIdentity(t *testing.T) {
	isolateTestUserDirs(t)
	previous, hadPrevious := providerRegistry["koyeb"]
	providerRegistry["koyeb"] = coordinatorKoyebTargetProvider{}
	t.Cleanup(func() {
		if hadPrevious {
			providerRegistry["koyeb"] = previous
		} else {
			delete(providerRegistry, "koyeb")
		}
	})
	for _, test := range []struct {
		name, state, app, scope, region string
	}{
		{"provisioning_second_app", "provisioning", "66666666-6666-4666-8666-666666666666", "koyeb:context:v1:stored-second-app", "was"},
		{"released_first_app", "released", "44444444-4444-4444-8444-444444444444", "koyeb:context:v1:stored-first-app", "was"},
		{"legacy_unbound", "released", "", "", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			const leaseID = "cbx_abcdef123456"
			stored := map[string]any{
				"id": leaseID, "provider": "koyeb", "target": "linux", "state": test.state,
				"providerProject": test.app, "providerScope": test.scope, "region": test.region,
			}
			if test.state == "released" {
				stored["cleanupStatus"] = "complete"
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer user-token" {
					t.Errorf("unexpected coordinator request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				switch r.URL.Path {
				case "/v1/leases":
					_ = json.NewEncoder(w).Encode(map[string]any{"leases": []any{stored}})
				case "/v1/leases/" + leaseID:
					_ = json.NewEncoder(w).Encode(map[string]any{"lease": stored})
				default:
					t.Errorf("unexpected coordinator path %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			cfg := Config{Provider: "koyeb", TargetOS: targetLinux, Coordinator: server.URL, CoordToken: "user-token"}
			backend := &coordinatorLeaseBackend{cfg: cfg, coord: mustNewCoordinatorClient(t, cfg)}
			leases, err := backend.coord.listLeases(t.Context(), "", 1000, "", "koyeb")
			if err != nil {
				t.Fatal(err)
			}
			if len(leases) != 1 {
				t.Fatalf("listed leases=%#v", leases)
			}
			if test.state == "provisioning" {
				listed, err := backend.ListJSON(t.Context(), ListRequest{})
				if err != nil {
					t.Fatal(err)
				}
				var ok bool
				leases, ok = listed.([]CoordinatorLease)
				if !ok || len(leases) != 1 {
					t.Fatalf("current listed leases=%#v", listed)
				}
			}
			status, err := backend.Status(t.Context(), StatusRequest{ID: leaseID})
			if err != nil {
				t.Fatal(err)
			}
			for source, value := range map[string]any{"list": leases[0], "status": status} {
				encoded, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				var got map[string]any
				if err := json.Unmarshal(encoded, &got); err != nil {
					t.Fatal(err)
				}
				for field, want := range map[string]string{"providerProject": test.app, "providerScope": test.scope, "region": test.region} {
					actual, present := got[field]
					if want == "" {
						if present {
							t.Errorf("%s %s=%#v, want absent legacy metadata", source, field, actual)
						}
					} else if actual != want {
						t.Errorf("%s %s=%#v, want stored %q", source, field, actual, want)
					}
				}
				if strings.Contains(string(encoded), "user-token") {
					t.Errorf("%s exposed coordinator credentials", source)
				}
			}
		})
	}
}
