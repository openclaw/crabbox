package cli

import (
	"encoding/json"
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
