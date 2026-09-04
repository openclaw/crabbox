package cli

import (
	"context"
	"net/http"
	"testing"
)

func TestCoordinatorAsyncPreferenceOnlyOnCreateRoutes(t *testing.T) {
	t.Setenv("CRABBOX_OWNER", "alice@example.com")
	for _, test := range []struct {
		name, method, path string
		call               func(context.Context, *CoordinatorClient, Config) error
	}{
		{"ordinary", http.MethodPost, "/v1/leases", func(ctx context.Context, c *CoordinatorClient, cfg Config) error {
			_, err := c.CreateLeaseWithAttempt(ctx, cfg, "synthetic-public-key", false, "cbx_create", "create", "synthetic-attempt")
			return err
		}},
		{"capability-aware", http.MethodPost, "/v1/leases/capability-aware", func(ctx context.Context, c *CoordinatorClient, cfg Config) error {
			cfg.imageRequirements = imageRequirements{Runtimes: map[string]string{"node": "24"}}
			_, err := c.CreateLease(ctx, cfg, "synthetic-public-key", false, "cbx_create", "create")
			return err
		}},
		{"from-checkpoint", http.MethodPost, "/v1/leases/from-checkpoint", func(ctx context.Context, c *CoordinatorClient, cfg Config) error {
			ctx = withCheckpointLeaseClaim(ctx, "chk_example", "synthetic-claim")
			_, err := c.CreateLease(ctx, cfg, "synthetic-public-key", false, "cbx_create", "create")
			return err
		}},
		{"fixed", http.MethodPut, "/v1/leases/cbx_create", func(ctx context.Context, c *CoordinatorClient, cfg Config) error {
			_, err := c.EnsureLease(ctx, cfg, "synthetic-public-key", true, "cbx_create", "create")
			return err
		}},
		{"fixed-from-checkpoint", http.MethodPut, "/v1/leases/cbx_create/from-checkpoint", func(ctx context.Context, c *CoordinatorClient, cfg Config) error {
			ctx = withCheckpointLeaseClaim(ctx, "chk_example", "synthetic-claim")
			_, err := c.EnsureLease(ctx, cfg, "synthetic-public-key", true, "cbx_create", "create")
			return err
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			client := &CoordinatorClient{BaseURL: "http://coordinator.test", Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != test.method || r.URL.Path != test.path || r.Header.Get("Prefer") != "respond-async" {
					t.Errorf("request=%s %s Prefer=%q", r.Method, r.URL.Path, r.Header.Get("Prefer"))
				}
				return coordinatorAsyncResponse(http.StatusAccepted, map[string]any{"lease": CoordinatorLease{ID: "cbx_create", State: "provisioning"}})
			})}}
			if err := test.call(context.Background(), client, Config{Provider: "azure", TargetOS: targetWindows}); err != nil || calls != 1 {
				t.Fatalf("err=%v calls=%d", err, calls)
			}
		})
	}
	for _, test := range []struct{ method, path string }{
		{http.MethodGet, "/v1/leases/cbx_create"},
		{http.MethodPost, "/v1/leases/cbx_create/heartbeat"},
		{http.MethodPost, "/v1/leases/cbx_create/release"},
		{http.MethodPost, "/v1/leases/cbx_create/cancel-create"},
		{http.MethodPut, "/v1/leases/cbx_create/registration"},
		{http.MethodPut, "/v1/leases/cbx_create/share"},
		{http.MethodPost, "/v1/leases/cbx_create/tailscale"},
		{http.MethodPost, "/v1/images"},
		{http.MethodPost, "/v1/checkpoints"},
	} {
		t.Run(test.path, func(t *testing.T) {
			client := &CoordinatorClient{BaseURL: "http://coordinator.test", Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if got := r.Header.Get("Prefer"); got != "" {
					t.Errorf("%s %s got create preference %q", r.Method, r.URL.Path, got)
				}
				return coordinatorAsyncResponse(http.StatusOK, map[string]any{})
			})}}
			if err := client.do(context.Background(), test.method, test.path, nil, nil); err != nil {
				t.Fatal(err)
			}
		})
	}
}
