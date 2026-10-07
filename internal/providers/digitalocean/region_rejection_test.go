package digitalocean

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type sizeValidator interface {
	ValidateSizeRegion(context.Context, core.Config) error
}

type checkedDigitalOceanAPI struct {
	*fakeDigitalOceanAPI
	validate func(context.Context, core.Config) error
}

func (f *checkedDigitalOceanAPI) ValidateSizeRegion(ctx context.Context, cfg core.Config) error {
	return f.validate(ctx, cfg)
}
func (f *fakeDigitalOceanAPI) ValidateSizeRegion(context.Context, core.Config) error { return nil }

func TestSizeRegionPreflight(t *testing.T) {
	for _, tc := range []struct {
		name, region, catalog string
		wantError             bool
	}{
		{"available", "sfo2", `{"slug":"s-1vcpu-1gb","available":true,"regions":["sfo2","ams3"]}`, false},
		{"unavailable region", "nyc3", `{"slug":"s-1vcpu-1gb","available":true,"regions":["sfo2","ams3"]}`, true},
		{"disabled size", "sfo2", `{"slug":"s-1vcpu-1gb","available":false,"regions":[]}`, true},
		{"unknown size", "sfo2", `{"slug":"different","available":true,"regions":["sfo2"]}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pages := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/sizes" {
					t.Errorf("unexpected mutation/request: %s %s", r.Method, r.URL.Path)
					http.Error(w, "unexpected", 500)
					return
				}
				pages++
				if r.URL.Query().Get("page") == "1" {
					fmt.Fprint(w, `{"sizes":[],"links":{"pages":{"next":"https://api.example.test/sizes?page=2"}}}`)
					return
				}
				fmt.Fprintf(w, `{"sizes":[%s]}`, tc.catalog)
			}))
			defer server.Close()
			client := newDigitalOceanTestClient(t, server, "fixture-token")
			validator, ok := any(client).(sizeValidator)
			if !ok {
				t.Fatal("DigitalOcean client has no size/region preflight")
			}
			cfg := core.BaseConfig()
			cfg.ServerType = "s-1vcpu-1gb"
			cfg.DigitalOcean.Region = tc.region
			err := validator.ValidateSizeRegion(t.Context(), cfg)
			if (err != nil) != tc.wantError {
				t.Fatalf("err=%v", err)
			}
			if err != nil {
				for _, text := range []string{"digitalocean_size_unavailable_in_region", cfg.ServerType, tc.region, "digitalocean.region", "CRABBOX_DIGITALOCEAN_REGION"} {
					if !strings.Contains(err.Error(), text) {
						t.Errorf("missing %q in %v", text, err)
					}
				}
				if tc.name == "unavailable region" && (!strings.Contains(err.Error(), "ams3") || !strings.Contains(err.Error(), "sfo2")) {
					t.Fatalf("available regions omitted: %v", err)
				}
			}
			if pages != 2 {
				t.Fatalf("pages=%d", pages)
			}
		})
	}
}

func TestAcquireSizePreflightBeforeCreate(t *testing.T) {
	for _, fixed := range []bool{false, true} {
		t.Run(fmt.Sprint(fixed), func(t *testing.T) {
			api := &fakeDigitalOceanAPI{}
			b := newTestBackend(t, api)
			cause := errors.New("digitalocean_size_unavailable_in_region")
			checked := &checkedDigitalOceanAPI{fakeDigitalOceanAPI: api, validate: func(context.Context, core.Config) error { return cause }}
			b.clientFactory = func(core.Runtime) (digitalOceanAPI, error) { return checked, nil }
			req := core.AcquireRequest{RequestedSlug: "region", Repo: core.Repo{Root: t.TempDir()}}
			if fixed {
				req.RequestedLeaseID = "cbx_abcdef123461"
			}
			if _, err := b.Acquire(t.Context(), req); !errors.Is(err, cause) {
				t.Fatalf("error=%v", err)
			}
			if len(api.createRequests) != 0 {
				t.Fatal("created before size preflight")
			}
			if fixed {
				lease, err := b.Resolve(t.Context(), core.ResolveRequest{ID: req.RequestedLeaseID, ReleaseOnly: true})
				if err != nil {
					t.Fatal(err)
				}
				if err := b.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestFixedDefiniteRejectionRetryOrStop(t *testing.T) {
	for _, retry := range []bool{false, true} {
		t.Run(fmt.Sprint(retry), func(t *testing.T) {
			cause := errors.New("size rejected")
			api := &fakeDigitalOceanAPI{createErr: &core.FixedCreateRejected{Err: cause}}
			b := newTestBackend(t, api)
			req := core.AcquireRequest{RequestedLeaseID: "cbx_abcdef123462", RequestedSlug: "reject", Repo: core.Repo{Root: t.TempDir()}}
			if _, err := b.Acquire(t.Context(), req); !errors.Is(err, cause) {
				t.Fatalf("err=%v", err)
			}
			claim, exists, err := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
			if err != nil || !exists || len(claim.FixedCreateIntent.Attempt) != 0 || len(claim.FixedCreateIntent.FailedAttempts) != 1 {
				t.Fatalf("no durable rejected attempt: %+v %v", claim, err)
			}
			if retry {
				api.createErr = nil
				if _, err := b.Acquire(t.Context(), req); err != nil {
					t.Fatal(err)
				}
				if len(api.createRequests) != 2 {
					t.Fatalf("creates=%d", len(api.createRequests))
				}
			} else {
				b.clientFactory = func(core.Runtime) (digitalOceanAPI, error) {
					t.Error("rejected stop called provider")
					return nil, errors.New("offline")
				}
				lease, err := b.Resolve(t.Context(), core.ResolveRequest{ID: req.RequestedLeaseID, ReleaseOnly: true})
				if err != nil {
					t.Fatal(err)
				}
				if err := b.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
					t.Fatal(err)
				}
				claim, err = core.ReadLeaseClaim(req.RequestedLeaseID)
				if err != nil || claim.FixedCreateIntent.State != "released" {
					t.Fatalf("not released: %+v %v", claim, err)
				}
			}
		})
	}
}

func TestCreateRejectionClassification(t *testing.T) {
	for _, code := range []int{400, 401, 403, 404, 408, 409, 422, 429, 500, 503} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			deleted := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == "GET" && r.URL.Path == "/account/keys":
					fmt.Fprint(w, `{"ssh_keys":[]}`)
				case r.Method == "POST" && r.URL.Path == "/account/keys":
					json.NewEncoder(w).Encode(map[string]any{"ssh_key": sshKey{ID: 123, Name: providerKeyForLease("cbx_abcdef123463"), PublicKey: "ssh-ed25519 fixture"}})
				case r.Method == "POST" && r.URL.Path == "/droplets":
					w.WriteHeader(code)
					fmt.Fprint(w, `{"message":"Size is not available in this region."}`)
				case r.Method == "GET" && r.URL.Path == "/tags":
					fmt.Fprint(w, `{"tags":[]}`)
				case r.Method == "GET" && r.URL.Path == "/droplets":
					fmt.Fprint(w, `{"droplets":[]}`)
				case r.Method == "DELETE" && r.URL.Path == "/account/keys/123":
					deleted = true
					w.WriteHeader(204)
				default:
					t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
					http.Error(w, "unexpected", 500)
				}
			}))
			defer server.Close()
			c := newDigitalOceanTestClient(t, server, "fixture-token")
			c.reconcileTimeout = 5 * time.Millisecond
			c.reconcileInterval = time.Millisecond
			cfg := core.BaseConfig()
			cfg.ServerType = "s-1vcpu-1gb"
			_, err := c.CreateFixedDroplet(t.Context(), cfg, "ssh-ed25519 fixture", "cbx_abcdef123463", "reject", false, time.Now(), map[string]string{})
			definite := code < 500 && code != 408 && code != 409 && code != 429
			var rejected *core.FixedCreateRejected
			if errors.As(err, &rejected) != definite || deleted != definite {
				t.Fatalf("code=%d rejected=%t deleted=%t error=%v", code, rejected != nil, deleted, err)
			}
		})
	}
}

func TestRejectedStopRefusesChangedClaim(t *testing.T) {
	api := &fakeDigitalOceanAPI{createErr: &core.FixedCreateRejected{Err: errors.New("rejected")}}
	b := newTestBackend(t, api)
	req := core.AcquireRequest{RequestedLeaseID: "cbx_abcdef123464", RequestedSlug: "race", Repo: core.Repo{Root: t.TempDir()}}
	_, _ = b.Acquire(t.Context(), req)
	lease, err := b.Resolve(t.Context(), core.ResolveRequest{ID: req.RequestedLeaseID, ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	api.createErr = nil
	if _, err := b.Acquire(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if err := b.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease}); err == nil {
		t.Fatal("stale rejected snapshot released a new attempt")
	}
	if len(api.deleted) != 0 || len(api.deletedKeyIDs) != 0 {
		t.Fatal("stale release mutated resources")
	}
}

func TestFixedReplaySkipsSizePreflight(t *testing.T) {
	api := &fakeDigitalOceanAPI{}
	b := newTestBackend(t, api)
	calls := 0
	checked := &checkedDigitalOceanAPI{fakeDigitalOceanAPI: api, validate: func(context.Context, core.Config) error {
		calls++
		if calls > 1 {
			return errors.New("catalog changed")
		}
		return nil
	}}
	b.clientFactory = func(core.Runtime) (digitalOceanAPI, error) { return checked, nil }
	req := core.AcquireRequest{RequestedLeaseID: "cbx_abcdef123465", RequestedSlug: "replay", Repo: core.Repo{Root: t.TempDir()}}
	if _, err := b.Acquire(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Acquire(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(api.createRequests) != 1 {
		t.Fatalf("preflights=%d creates=%d", calls, len(api.createRequests))
	}
}

func TestSizeCatalogFailureRetiresUnsubmittedAttempt(t *testing.T) {
	api := &fakeDigitalOceanAPI{}
	b := newTestBackend(t, api)
	cause := &digitalOceanAPIError{Operation: "GET /sizes", Status: 503, Body: "temporarily unavailable"}
	checked := &checkedDigitalOceanAPI{fakeDigitalOceanAPI: api, validate: func(context.Context, core.Config) error { return cause }}
	b.clientFactory = func(core.Runtime) (digitalOceanAPI, error) { return checked, nil }
	req := core.AcquireRequest{RequestedLeaseID: "cbx_abcdef123466", RequestedSlug: "catalog", Repo: core.Repo{Root: t.TempDir()}}
	if _, err := b.Acquire(t.Context(), req); !errors.Is(err, cause) {
		t.Fatalf("err=%v", err)
	}
	claim, err := core.ReadLeaseClaim(req.RequestedLeaseID)
	if err != nil || !isRejectedFixed(claim) || len(api.createRequests) != 0 {
		t.Fatalf("preflight retained uncertain custody: %+v %v", claim, err)
	}
}
