package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A one-shot run releases a destructive coordinator lease without waiting for
// teardown. The guest runtime goes with the resource, so an accepted teardown
// must not turn a successful command into a cleanup failure.
func TestNativeRuntimeScopeCoordinatorAcceptedTeardown(t *testing.T) {
	configureCoordinatorReleaseTestTiming(t, time.Second, 0)
	deleting, keeping := true, false
	for _, tc := range []struct {
		name     string
		lease    CoordinatorLease
		unretire bool
		retained string
	}{
		{name: "pending", lease: CoordinatorLease{State: "released", CleanupStartedAt: "2026-08-31T00:00:00Z", ReleaseDeletesServer: &deleting}},
		{name: "pending_status", lease: CoordinatorLease{State: "released", CleanupStatus: "pending"}},
		{name: "expiry_completed", lease: CoordinatorLease{State: "expired", CleanupCompletedAt: "2026-08-31T00:10:00Z"}},
		{name: "cleanup_failed", lease: CoordinatorLease{State: "released", CleanupStatus: "failed", CleanupError: "synthetic provider failure", ReleaseDeletesServer: &deleting}, retained: "lease release was not confirmed"},
		{name: "retry_scheduled", lease: CoordinatorLease{State: "released", CleanupRetryAt: "2026-08-31T00:05:00Z", ReleaseDeletesServer: &deleting}, retained: "lease release was not confirmed"},
		{name: "pending_with_unretired_stage", lease: CoordinatorLease{State: "released", CleanupStatus: "pending"}, unretire: true, retained: "file removal not verified"},
		{name: "retained_resource", lease: CoordinatorLease{State: "released", ReleaseDeletesServer: &keeping}, retained: "lease release was not confirmed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearConfigEnv(t)
			const id = "cbx_abcdef123456"
			managedStopLocalState(t, id)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, "/release") {
					http.NotFound(w, r)
					return
				}
				lease := tc.lease
				lease.ID, lease.Provider = id, "aws"
				_ = json.NewEncoder(w).Encode(map[string]any{"lease": lease})
			}))
			defer server.Close()
			backend := coordinatorReleaseTestBackend(server, io.Discard)

			s := testNativeRuntimeScope()
			removed := 0
			s.remove = func(context.Context, *remoteNativeRuntime) error { removed++; return nil }
			ctx := runtimeLeaseContext(t.Context(), id)
			runtime, err := s.ensure(ctx, SSHTarget{})
			if err != nil {
				t.Fatal(err)
			}
			if tc.unretire {
				s.retain(runtime, errors.New("command stage retirement unconfirmed"))
			}

			outcome, releaseErr := backend.ReleaseLeaseWithOutcome(t.Context(), ReleaseLeaseRequest{
				Lease:                           LeaseTarget{LeaseID: id, Server: Server{Provider: "aws"}},
				Force:                           true,
				DeferProviderCleanupObservation: true,
			})
			if releaseErr != nil || outcome.Terminal {
				t.Fatalf("outcome=%+v err=%v", outcome, releaseErr)
			}
			err = s.afterRelease(ctx, id, backend, outcome, releaseErr)
			if tc.retained == "" && err != nil {
				t.Fatalf("accepted teardown reported a cleanup failure: %v", err)
			}
			if tc.retained != "" && (err == nil || !strings.Contains(err.Error(), tc.retained) || !strings.Contains(err.Error(), runtime.path)) {
				t.Fatalf("retention not visible: %v", err)
			}
			if err := s.finish(ctx); err != nil {
				t.Fatal(err)
			}
			if removed != 0 {
				t.Fatalf("removed runtime from a lease without confirmed release: %d", removed)
			}
		})
	}
}

func TestNativeRuntimeScopeAcceptedTeardownWithReleaseError(t *testing.T) {
	for _, preserves := range []bool{false, true} {
		s := testNativeRuntimeScope()
		s.remove = func(context.Context, *remoteNativeRuntime) error {
			t.Error("removed runtime after a failed release")
			return nil
		}
		ctx := runtimeLeaseContext(t.Context(), "lease-a")
		if _, err := s.ensure(ctx, SSHTarget{}); err != nil {
			t.Fatal(err)
		}
		err := s.afterRelease(ctx, "lease-a", runtimeReleasePolicy{preserves: preserves}, ReleaseLeaseOutcome{TeardownAccepted: true}, errors.New("claim sync failed"))
		if err == nil || !strings.Contains(err.Error(), "/tmp/example-runtime/crabbox") || !strings.Contains(err.Error(), "claim sync failed") {
			t.Fatalf("preserves=%t: release error hidden by accepted teardown: %v", preserves, err)
		}
	}
}
