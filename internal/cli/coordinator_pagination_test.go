package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCoordinatorCurrentLeasesPagesPastEmptyFilteredPage(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		query := r.URL.Query()
		if query.Get("limit") != "100" || query.Get("projection") != "summary" || query.Get("pagination") != "keyset-v1" || query.Get("view") != "current" || query.Get("provider") != "aws" {
			t.Errorf("query=%s", r.URL.RawQuery)
		}
		response := map[string]any{"pagination": "keyset-v1", "leases": []CoordinatorLease{}}
		switch query.Get("cursor") {
		case "":
			response["leases"] = []CoordinatorLease{{ID: "older", CreatedAt: "2026-01-01"}}
			response["nextCursor"] = "page/2+opaque="
		case "page/2+opaque=":
			response["nextCursor"] = "page3"
		case "page3":
			response["leases"] = []CoordinatorLease{{ID: "newer", CreatedAt: "2026-02-01"}}
		default:
			t.Errorf("unexpected cursor=%q", query.Get("cursor"))
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()
	cfg := baseConfig()
	cfg.Coordinator, cfg.CoordToken = server.URL, "synthetic-token"
	leases, truncated, err := mustNewCoordinatorClient(t, cfg).CurrentLeases(t.Context(), "aws")
	if err != nil || truncated || calls != 3 || len(leases) != 2 || leases[0].ID != "newer" || leases[1].ID != "older" {
		t.Fatalf("leases=%v truncated=%v calls=%d err=%v", leases, truncated, calls, err)
	}
}

func TestCoordinatorCurrentLeasesLegacyLimit(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		count := 100
		if calls == 2 {
			if r.URL.Query().Get("limit") != "500" {
				t.Errorf("query=%s", r.URL.RawQuery)
			}
			count = 500
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"leases": make([]CoordinatorLease, count)})
	}))
	defer server.Close()
	cfg := baseConfig()
	cfg.Coordinator, cfg.CoordToken = server.URL, "synthetic-token"
	leases, truncated, err := mustNewCoordinatorClient(t, cfg).CurrentLeases(t.Context(), "aws")
	if err != nil || !truncated || calls != 2 || len(leases) != 500 {
		t.Fatalf("len=%d truncated=%v calls=%d err=%v", len(leases), truncated, calls, err)
	}
}

func TestCoordinatorCurrentLeasesRejectsNonadvancingCursor(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_ = json.NewEncoder(w).Encode(map[string]any{"pagination": "keyset-v1", "nextCursor": "same", "leases": []CoordinatorLease{}})
	}))
	defer server.Close()
	cfg := baseConfig()
	cfg.Coordinator, cfg.CoordToken = server.URL, "synthetic-token"
	_, _, err := mustNewCoordinatorClient(t, cfg).CurrentLeases(t.Context(), "aws")
	if err == nil || !strings.Contains(err.Error(), "did not advance") || calls != 2 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

func TestListMachinesActiveAdminLeasesPagination(t *testing.T) {
	calls := 0
	var stderr strings.Builder
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/pool" {
			_ = json.NewEncoder(w).Encode(map[string]any{"machines": []CoordinatorMachine{{}}})
			return
		}
		calls++
		q := r.URL.Query()
		if r.URL.Path != "/v1/admin/leases" || q.Get("state") != "active" || q.Get("limit") != "100" || q.Get("pagination") != "keyset-v1" || q.Get("projection") != "summary" {
			t.Errorf("unexpected request %s", r.URL)
		}
		res := map[string]any{"pagination": "keyset-v1", "leases": []CoordinatorLease{}}
		switch q.Get("cursor") {
		case "":
			res["leases"] = []CoordinatorLease{{ID: "first", State: "active"}}
			res["nextCursor"] = "page/2+opaque="
		case "page/2+opaque=":
			res["nextCursor"] = "third"
		case "third":
			res["leases"] = []CoordinatorLease{{ID: "last", State: "active"}}
		default:
			t.Errorf("unexpected cursor: %q", q.Get("cursor"))
		}
		_ = json.NewEncoder(w).Encode(res)
	}))
	defer server.Close()
	cfg := baseConfig()
	cfg.Coordinator, cfg.CoordAdminToken = server.URL, "synthetic-admin"
	backend := &coordinatorLeaseBackend{cfg: cfg, rt: Runtime{Stderr: &stderr}}
	machines, ids, err := backend.listMachines(t.Context())
	if err != nil || len(machines) != 1 || len(ids) != 2 || calls != 3 || stderr.Len() != 0 {
		t.Fatalf("machines=%v ids=%v calls=%d err=%v warning=%s", machines, ids, calls, err, &stderr)
	}
	if _, ok := ids["last"]; !ok {
		t.Fatal("last page missing")
	}
}

func TestListMachinesActiveAdminLeasesLegacyAndFailureWarnings(t *testing.T) {
	for _, tc := range []struct {
		name    string
		count   int
		failure bool
		warning string
	}{
		{name: "legacy complete", count: 12},
		{name: "legacy truncated", count: 500, warning: "warning: active lease lookup reached the legacy coordinator limit; orphan status may be incomplete\n"},
		{name: "failure", failure: true, warning: "warning: active lease lookup failed; orphan status unavailable: coordinator GET /v1/admin/leases?limit=100&pagination=keyset-v1&projection=summary&state=active: http 403: forbidden\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			var stderr strings.Builder
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/pool" {
					_ = json.NewEncoder(w).Encode(map[string]any{"machines": []CoordinatorMachine{{}}})
					return
				}
				calls++
				if tc.failure {
					http.Error(w, "forbidden", http.StatusForbidden)
					return
				}
				if calls == 1 {
					if r.URL.Query().Get("pagination") != "keyset-v1" {
						t.Error("missing negotiation")
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"leases": []CoordinatorLease{}})
					return
				}
				if calls != 2 || r.URL.Query().Get("limit") != "500" || r.URL.Query().Get("pagination") != "" || r.URL.Query().Get("state") != "active" {
					t.Errorf("unexpected legacy call %d %s", calls, r.URL)
				}
				leases := make([]CoordinatorLease, tc.count)
				for i := range leases {
					leases[i] = CoordinatorLease{ID: fmt.Sprintf("lease-%d", i), State: "active"}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"leases": leases})
			}))
			defer server.Close()
			cfg := baseConfig()
			cfg.Coordinator, cfg.CoordAdminToken = server.URL, "synthetic-admin"
			backend := &coordinatorLeaseBackend{cfg: cfg, rt: Runtime{Stderr: &stderr}}
			machines, ids, err := backend.listMachines(t.Context())
			if err != nil || len(machines) != 1 || len(ids) != tc.count {
				t.Fatalf("machines=%v ids=%d err=%v", machines, len(ids), err)
			}
			if stderr.String() != tc.warning {
				t.Fatalf("warning=%q want %q", stderr.String(), tc.warning)
			}
			if tc.failure && (ids != nil || calls != 1) {
				t.Fatalf("ids=%v calls=%d", ids, calls)
			}
		})
	}
}

func TestCoordinatorActiveAdminLeasesRejectsBrokenPagination(t *testing.T) {
	for _, changeMode := range []bool{false, true} {
		t.Run(fmt.Sprint(changeMode), func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				mode := "keyset-v1"
				if changeMode && calls > 1 {
					mode = ""
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"pagination": mode, "nextCursor": "same"})
			}))
			defer server.Close()
			cfg := baseConfig()
			cfg.Coordinator, cfg.CoordToken = server.URL, "synthetic-token"
			_, _, err := mustNewCoordinatorClient(t, cfg).ActiveAdminLeases(t.Context())
			if err == nil || calls != 2 {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
		})
	}
}
