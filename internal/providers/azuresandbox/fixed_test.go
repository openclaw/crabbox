package azuresandbox

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
)

type fixture struct {
	created                           createRequest
	box                               *sandbox
	creates, deletes                  int
	loseCreateResponse, hideInventory bool
}

func (f *fixture) request(t *testing.T, r *http.Request) (*http.Response, error) {
	t.Helper()
	if r.Method == "PUT" && strings.HasSuffix(r.URL.Path, "/sandboxes") {
		f.creates++
		var body createRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		f.created = body
		// Match the live ACA service's label limit, including on private images.
		for _, value := range body.Labels {
			if len(value) > 63 {
				return response(400, "label value exceeds 63 characters"), nil
			}
		}
		f.box = &sandbox{ID: "unique-resource", State: "Running", Labels: body.Labels}
		if f.loseCreateResponse {
			return response(500, "uncertain"), nil
		}
		data, _ := json.Marshal(f.box)
		return response(200, string(data)), nil
	}
	if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/sandboxes") {
		boxes := []sandbox{}
		if f.box != nil && !f.hideInventory {
			boxes = append(boxes, *f.box)
		}
		data, _ := json.Marshal(boxes)
		return response(200, string(data)), nil
	}
	if r.Method == "DELETE" {
		f.deletes++
		f.box = nil
		return response(204, ""), nil
	}
	if f.box == nil {
		return response(404, ""), nil
	}
	data, _ := json.Marshal(f.box)
	return response(200, string(data)), nil
}

func TestPreparedDiskKeepsFreshLeaseOwnershipAndRejectsSourceChange(t *testing.T) {
	f := &fixture{}
	b, req := fixtureBackend(t, f)
	b.cfg.AzureSandbox.DiskID = "prepared-disk-id"
	if _, err := b.acquire(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	wire, err := json.Marshal(f.created.SourcesRef)
	if err != nil || string(wire) != `{"diskImage":{"id":"prepared-disk-id"}}` {
		t.Fatalf("private disk reference = %s, error = %v", wire, err)
	}
	if f.created.Labels["crabbox_lease"] != req.RequestedLeaseID || f.created.Labels["crabbox_attempt"] == "" || f.created.Lifecycle["autoSuspendPolicy"] == nil {
		t.Fatal("prepared disk lost fresh ownership or lifecycle")
	}
	b.cfg.AzureSandbox.DiskID = "replacement-disk-id"
	if _, err := b.acquire(t.Context(), req); err == nil || f.creates != 1 {
		t.Fatal("lease replay changed its prepared disk")
	}
	if err := b.Stop(t.Context(), core.StopRequest{ID: req.RequestedLeaseID}); err != nil {
		t.Fatal(err)
	}
}

func TestPreparedDiskRejectsConflictingPublicSource(t *testing.T) {
	f := &fixture{}
	b, req := fixtureBackend(t, f)
	b.cfg.AzureSandbox.DiskID, b.cfg.AzureSandbox.Disk = "prepared-disk-id", "other-public-image"
	if _, err := b.acquire(t.Context(), req); err == nil || f.creates != 0 {
		t.Fatal("ambiguous disk source allocated")
	}
}

func fixtureBackend(t *testing.T, f *fixture) (*backend, core.FixedWarmupRequest) {
	t.Helper()
	testutil.IsolateUserDirs(t)
	cfg := core.BaseConfig()
	cfg.Provider, cfg.TTL, cfg.IdleTimeout = providerName, 4*time.Hour, 15*time.Minute
	cfg.AzureSandbox = core.AzureSandboxConfig{Region: "westus3", Subscription: "subscription", ResourceGroup: "workers", Group: "sandboxes", Disk: "ubuntu"}
	b := &backend{cfg: cfg, rt: core.Runtime{Stdout: io.Discard, Stderr: io.Discard}, api: fixtureClient(t, func(r *http.Request) (*http.Response, error) { return f.request(t, r) })}
	return b, core.FixedWarmupRequest{RequestedLeaseID: "cbx_abcdef123456", WarmupRequest: core.WarmupRequest{Repo: core.Repo{Root: t.TempDir()}, Keep: true, RequestedSlug: "sandbox"}}
}

func TestFixedSandboxReplayAndExactCleanup(t *testing.T) {
	f := &fixture{}
	b, req := fixtureBackend(t, f)
	for range 2 {
		if _, err := b.acquire(t.Context(), req); err != nil {
			t.Fatal(err)
		}
	}
	if f.creates != 1 {
		t.Fatal("replay allocated twice")
	}
	if _, err := b.Status(t.Context(), core.StatusRequest{ID: req.RequestedLeaseID}); err != nil {
		t.Fatal(err)
	}
	if err := b.Stop(t.Context(), core.StopRequest{ID: req.RequestedLeaseID}); err != nil {
		t.Fatal(err)
	}
	if f.deletes != 1 {
		t.Fatal("wrong deletion count")
	}
	if _, err := b.acquire(t.Context(), req); err == nil {
		t.Fatal("released lease recreated")
	}
	if err := b.Stop(t.Context(), core.StopRequest{ID: req.RequestedLeaseID}); err != nil {
		t.Fatal(err)
	}
	if f.deletes != 1 {
		t.Fatal("terminal replay deleted twice")
	}
}

func TestUncertainCreateAdoptsOriginalAndNeverResubmits(t *testing.T) {
	f := &fixture{loseCreateResponse: true, hideInventory: true}
	b, req := fixtureBackend(t, f)
	if _, err := b.acquire(t.Context(), req); err == nil {
		t.Fatal("uncertain create accepted")
	}
	if _, err := b.acquire(t.Context(), req); err == nil {
		t.Fatal("empty inventory settled uncertainty")
	}
	if err := b.Stop(t.Context(), core.StopRequest{ID: req.RequestedLeaseID}); err == nil {
		t.Fatal("empty inventory proved deletion")
	}
	if f.creates != 1 || f.deletes != 0 {
		t.Fatal("uncertain attempt repeated or deleted")
	}
	f.hideInventory = false
	if _, err := b.acquire(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if f.creates != 1 {
		t.Fatal("adoption allocated a new resource")
	}
}

func TestSandboxMalformedInventoryCannotCreateOrRelease(t *testing.T) {
	f := &fixture{}
	b, req := fixtureBackend(t, f)
	b.api.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/sandboxes") {
			return response(200, `{}`), nil
		}
		return f.request(t, r)
	})
	if _, err := b.acquire(t.Context(), req); err == nil || f.creates != 0 {
		t.Fatalf("malformed inventory admitted creation: err=%v creates=%d", err, f.creates)
	}
	if err := b.Stop(t.Context(), core.StopRequest{ID: req.RequestedLeaseID}); err == nil {
		t.Fatal("malformed inventory settled absence")
	}
	claim, err := b.claim(req.RequestedLeaseID)
	if err != nil || claim.FixedCreateIntent.State == "released" || f.deletes != 0 {
		t.Fatalf("uncertain claim lost: %+v err=%v deletes=%d", claim, err, f.deletes)
	}
}

func TestSandboxDeleteRequiresAbsenceReadback(t *testing.T) {
	for _, readback := range []string{"unavailable", "still-present", "absent"} {
		t.Run(readback, func(t *testing.T) {
			f := &fixture{}
			b, req := fixtureBackend(t, f)
			if _, err := b.acquire(t.Context(), req); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			readsAfterDelete := 0
			b.api.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				if r.Method == http.MethodDelete {
					f.deletes++
					return response(202, ""), nil
				}
				if f.deletes > 0 {
					readsAfterDelete++
					switch readback {
					case "unavailable":
						return response(503, ""), nil
					case "still-present":
						cancel()
					case "absent":
						f.box = nil
					}
				}
				return f.request(t, r)
			})
			err := b.Stop(ctx, core.StopRequest{ID: req.RequestedLeaseID})
			if (err == nil) != (readback == "absent") || readsAfterDelete != 1 {
				t.Fatalf("stop err=%v readbacks=%d", err, readsAfterDelete)
			}
			claim, err := b.claim(req.RequestedLeaseID)
			if err != nil {
				t.Fatal(err)
			}
			if readback == "absent" {
				if claim.FixedCreateIntent.State != "released" {
					t.Fatal("verified absence did not release claim")
				}
			} else if claim.FixedCreateIntent.State != "acquired" || claim.FixedCreateIntent.Journal.Phase != "deleting" {
				t.Fatal("unverified deletion did not retain its cleanup admission")
			}
		})
	}
}

func TestWrongAttemptCannotBeAdoptedOrDeleted(t *testing.T) {
	for _, label := range []string{"crabbox_attempt", "crabbox_fingerprint", "crabbox_lease"} {
		t.Run(label, func(t *testing.T) {
			f := &fixture{}
			b, req := fixtureBackend(t, f)
			if _, err := b.acquire(t.Context(), req); err != nil {
				t.Fatal(err)
			}
			f.box.Labels[label] = "other-owner"
			if _, err := b.acquire(t.Context(), req); err == nil {
				t.Fatal("adopted another owner")
			}
			if err := b.Stop(t.Context(), core.StopRequest{ID: req.RequestedLeaseID}); err == nil {
				t.Fatal("deleted another owner")
			}
			if f.deletes != 0 {
				t.Fatal("foreign resource deletion")
			}
		})
	}
}

func TestSandboxSlugResolutionIsScopedAndRejectsAmbiguity(t *testing.T) {
	for _, scenario := range []string{"ambiguous", "other-scope", "other-provider"} {
		t.Run(scenario, func(t *testing.T) {
			f := &fixture{}
			b, req := fixtureBackend(t, f)
			if _, err := b.acquire(t.Context(), req); err != nil {
				t.Fatal(err)
			}
			original, err := b.claim(req.RequestedLeaseID)
			if err != nil {
				t.Fatal(err)
			}
			second := original
			second.LeaseID = "cbx_123456abcdef"
			if scenario == "other-scope" {
				second.ProviderScope += "-other"
			}
			if scenario == "other-provider" {
				second.Provider = "other-provider"
			}
			if err := core.WithDurableLeaseClaimLockContext(t.Context(), second.LeaseID, func(c *core.LeaseClaim, _ bool, persist func() error) error {
				*c = second
				return persist()
			}); err != nil {
				t.Fatal(err)
			}
			resolved, err := b.claim(original.Slug)
			if scenario == "ambiguous" {
				if err == nil {
					t.Fatal("ambiguous slug resolved")
				}
			} else {
				if err != nil || resolved.LeaseID != original.LeaseID {
					t.Fatalf("scope changed resolution: %+v %v", resolved, err)
				}
				if _, err := b.claim(second.LeaseID); err == nil {
					t.Fatal("foreign exact ID resolved")
				}
			}
		})
	}
}
