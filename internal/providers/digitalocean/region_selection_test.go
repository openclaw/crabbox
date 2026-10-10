package digitalocean

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func regionCatalog(t *testing.T, b *digitalOceanLeaseBackend, api *fakeDigitalOceanAPI, regions *[]string, available bool) *int {
	t.Helper()
	reads := new(int)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/sizes" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", 500)
			return
		}
		*reads++
		json.NewEncoder(w).Encode(map[string]any{"sizes": []any{map[string]any{
			"slug": b.Cfg.ServerType, "available": available, "regions": *regions,
		}}})
	}))
	t.Cleanup(server.Close)
	c := newDigitalOceanTestClient(t, server, "fixture-token")
	checked := &checkedDigitalOceanAPI{fakeDigitalOceanAPI: api, validate: c.ResolveSizeRegion}
	b.clientFactory = func(core.Runtime) (digitalOceanAPI, error) { return checked, nil }
	return reads
}

func TestAcquireRegionPreference(t *testing.T) {
	order := []string{"sfo3", "sfo2", "sfo1", "tor1", "nyc3", "nyc1", "nyc2", "lon1", "ams3", "fra1", "sgp1", "blr1", "syd1"}
	for i, want := range order {
		t.Run(want, func(t *testing.T) {
			api := &fakeDigitalOceanAPI{}
			b := newTestBackend(t, api)
			regions := slices.Clone(order[i:])
			slices.Reverse(regions)
			reads := regionCatalog(t, b, api, &regions, true)
			lease, err := b.Acquire(t.Context(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}})
			if err != nil {
				t.Fatal(err)
			}
			if got := api.createRequests[0].cfg.DigitalOcean.Region; got != want {
				t.Fatalf("region=%q want %q", got, want)
			}
			claim, err := core.ReadLeaseClaim(lease.LeaseID)
			if err != nil || claim.Labels["region"] != want || *reads != 1 {
				t.Fatalf("claim region=%q reads=%d err=%v", claim.Labels["region"], *reads, err)
			}
		})
	}
}

func TestAcquireRegionExplicitAndUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name, region string
		regions      []string
		available    bool
		wantError    bool
	}{
		{"explicit", "ams3", []string{"sfo3", "ams3"}, true, false},
		{"explicit unavailable", "nyc3", []string{"sfo2", "ams3"}, true, true},
		{"no preferred region", "", []string{"other1"}, true, true},
		{"disabled", "", []string{"sfo3", "nyc3"}, false, true},
	} {
		for _, fixed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/fixed=%t", tc.name, fixed), func(t *testing.T) {
				api := &fakeDigitalOceanAPI{}
				b := newTestBackend(t, api)
				if tc.region != "" {
					b.Cfg.DigitalOcean.Region = tc.region
				}
				regionCatalog(t, b, api, &tc.regions, tc.available)
				req := core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}}
				if fixed {
					req.RequestedLeaseID = "cbx_abcdef123470"
				}
				_, err := b.Acquire(t.Context(), req)
				if tc.wantError {
					if err == nil || !strings.Contains(err.Error(), "digitalocean_size_unavailable_in_region") || len(api.createRequests) != 0 {
						t.Fatalf("creates=%d err=%v", len(api.createRequests), err)
					}
				} else if err != nil || api.createRequests[0].cfg.DigitalOcean.Region != tc.region {
					t.Fatalf("explicit region changed: err=%v", err)
				}
			})
		}
	}
}

func TestFixedRegionReplayUsesRecordedRegion(t *testing.T) {
	for _, lostReply := range []bool{false, true} {
		t.Run(fmt.Sprint(lostReply), func(t *testing.T) {
			api := &fakeDigitalOceanAPI{}
			if lostReply {
				api.fixedReplyErr = errors.New("reply lost")
			}
			b := newTestBackend(t, api)
			regions := []string{"nyc3", "sfo2"}
			reads := regionCatalog(t, b, api, &regions, true)
			req := core.AcquireRequest{RequestedLeaseID: "cbx_abcdef123471", Repo: core.Repo{Root: t.TempDir()}}
			_, err := b.Acquire(t.Context(), req)
			if (err != nil) != lostReply {
				t.Fatalf("first acquire: %v", err)
			}
			claim, err := core.ReadLeaseClaim(req.RequestedLeaseID)
			if err != nil || claim.Labels["region"] != "sfo2" {
				t.Fatalf("recorded region=%q err=%v", claim.Labels["region"], err)
			}
			regions = []string{"sfo3"}
			replay := NewDigitalOceanLeaseBackend(Provider{}.Spec(), b.Cfg, b.RT).(*digitalOceanLeaseBackend)
			replay.clientFactory, replay.waitSSH = b.clientFactory, b.waitSSH
			lease, err := replay.Acquire(t.Context(), req)
			if err != nil || len(api.createRequests) != 1 || *reads != 1 {
				t.Fatalf("replay: creates=%d reads=%d err=%v", len(api.createRequests), *reads, err)
			}
			after, err := core.ReadLeaseClaim(req.RequestedLeaseID)
			if err != nil || after.Labels["region"] != "sfo2" || after.FixedCreateIntent.Fingerprint != claim.FixedCreateIntent.Fingerprint {
				t.Fatalf("replay changed intent: %v", err)
			}
			replay.Cfg.DigitalOcean.Region = "sfo3"
			if _, err := replay.Acquire(t.Context(), req); err == nil || !strings.Contains(err.Error(), "lease_id_conflict") {
				t.Fatalf("changed explicit region accepted: %v", err)
			}
			if err := replay.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestFixedRegionLegacyDefaultReplay(t *testing.T) {
	api := &fakeDigitalOceanAPI{}
	b := newTestBackend(t, api)
	b.Cfg.DigitalOcean.Region = "nyc3"
	req := core.AcquireRequest{RequestedLeaseID: "cbx_abcdef123472", RequestedSlug: "legacy", Repo: core.Repo{Root: t.TempDir()}}
	if _, err := b.Acquire(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	// Older records and remote tags did not store a separate region label.
	if err := core.WithDurableLeaseClaimLock(req.RequestedLeaseID, func(claim *core.LeaseClaim, _ bool, persist func() error) error {
		delete(claim.Labels, "region")
		return persist()
	}); err != nil {
		t.Fatal(err)
	}
	for i := range api.droplets {
		labels := labelsFromTags(api.droplets[i].Tags)
		delete(labels, "region")
		api.droplets[i].Tags = tagsFromLabels(labels)
	}
	b.Cfg.DigitalOcean.Region = ""
	if _, err := b.Acquire(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if len(api.createRequests) != 1 {
		t.Fatal("legacy replay allocated a replacement")
	}
}
