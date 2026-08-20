package shared

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestCleanupServersUsesSingleBatchCutoff(t *testing.T) {
	clock := &testCleanupClock{now: time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)}
	boundary := clock.now.Add(time.Minute)
	servers := []core.Server{
		testCleanupServer("old", clock.now.Add(-time.Hour)),
		testCleanupServer("boundary-a", boundary),
		testCleanupServer("boundary-b", boundary),
	}

	var deleted []string
	backend := DirectSSHBackend{
		RT: core.Runtime{Stderr: &bytes.Buffer{}, Clock: clock},
		Delete: func(ctx context.Context, cfg core.Config, server core.Server) error {
			deleted = append(deleted, server.Name)
			if server.Name == "old" {
				clock.now = boundary.Add(time.Second)
			}
			return nil
		},
	}

	if err := backend.CleanupServers(context.Background(), core.CleanupRequest{}, servers); err != nil {
		t.Fatalf("CleanupServers returned error: %v", err)
	}

	if len(deleted) != 1 || deleted[0] != "old" {
		t.Fatalf("deleted=%v, want only old", deleted)
	}
}

func TestCleanupServersSkipsDeletesAndDryRuns(t *testing.T) {
	clock := &testCleanupClock{now: time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)}
	var stderr bytes.Buffer
	var deleted []string
	backend := DirectSSHBackend{
		RT: core.Runtime{Stderr: &stderr, Clock: clock},
		Delete: func(ctx context.Context, cfg core.Config, server core.Server) error {
			deleted = append(deleted, server.Name)
			return nil
		},
	}

	servers := []core.Server{
		testCleanupServerWithLabels("kept", map[string]string{
			"keep":       "true",
			"expires_at": clock.now.Add(-time.Hour).Format(time.RFC3339Nano),
		}),
		testCleanupServerWithLabels("failed", map[string]string{
			"state": "failed",
		}),
	}
	if err := backend.CleanupServers(context.Background(), core.CleanupRequest{}, servers); err != nil {
		t.Fatalf("CleanupServers returned error: %v", err)
	}
	if strings.Join(deleted, ",") != "failed" {
		t.Fatalf("deleted=%v, want failed", deleted)
	}
	if got := stderr.String(); !strings.Contains(got, "skip server id=kept name=kept reason=keep=true") || !strings.Contains(got, "delete server id=failed name=failed") {
		t.Fatalf("stderr=%q, want skip and delete lines", got)
	}

	stderr.Reset()
	deleted = nil
	if err := backend.CleanupServers(context.Background(), core.CleanupRequest{DryRun: true}, []core.Server{
		testCleanupServer("dry-run", clock.now.Add(-time.Hour)),
	}); err != nil {
		t.Fatalf("CleanupServers dry-run returned error: %v", err)
	}
	if len(deleted) != 0 {
		t.Fatalf("dry-run deleted=%v, want none", deleted)
	}
	if got := stderr.String(); !strings.Contains(got, "delete server id=dry-run name=dry-run") {
		t.Fatalf("dry-run stderr=%q, want delete plan", got)
	}
}

func TestCleanupServersRequiresDeleteAndPropagatesDeleteErrors(t *testing.T) {
	clock := &testCleanupClock{now: time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)}
	server := testCleanupServer("old", clock.now.Add(-time.Hour))
	backend := DirectSSHBackend{
		SpecValue: core.ProviderSpec{Name: "test-provider"},
		RT:        core.Runtime{Stderr: io.Discard, Clock: clock},
	}
	err := backend.CleanupServers(context.Background(), core.CleanupRequest{}, []core.Server{server})
	if err == nil || !strings.Contains(err.Error(), "provider=test-provider cleanup backend has no delete capability") {
		t.Fatalf("err=%v, want missing delete capability", err)
	}

	deleteErr := errors.New("delete failed")
	backend.Delete = func(context.Context, core.Config, core.Server) error {
		return deleteErr
	}
	err = backend.CleanupServers(context.Background(), core.CleanupRequest{}, []core.Server{server})
	if !errors.Is(err, deleteErr) {
		t.Fatalf("err=%v, want deleteErr", err)
	}
}

func TestCleanupServersSkipsIneligibleAndContinues(t *testing.T) {
	clock := &testCleanupClock{now: time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)}
	var stderr bytes.Buffer
	var deleted []string
	backend := DirectSSHBackend{
		RT: core.Runtime{Stderr: &stderr, Clock: clock},
		CleanupEligible: func(_ context.Context, server core.Server) (bool, error) {
			return server.Name == "claimed", nil
		},
		Delete: func(_ context.Context, _ core.Config, server core.Server) error {
			deleted = append(deleted, server.Name)
			return nil
		},
	}
	servers := []core.Server{
		testCleanupServer("claimless", clock.now.Add(-time.Hour)),
		testCleanupServer("claimed", clock.now.Add(-time.Hour)),
	}
	if err := backend.CleanupServers(context.Background(), core.CleanupRequest{}, servers); err != nil {
		t.Fatal(err)
	}
	if strings.Join(deleted, ",") != "claimed" {
		t.Fatalf("deleted=%v, want claimed", deleted)
	}
	if !strings.Contains(stderr.String(), "skip server id=claimless name=claimless reason=no-exact-local-claim") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestCleanupServersPassesPreparedServerToDelete(t *testing.T) {
	clock := &testCleanupClock{now: time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)}
	backend := DirectSSHBackend{
		RT: core.Runtime{Stderr: io.Discard, Clock: clock},
		PrepareCleanup: func(_ context.Context, server core.Server) (core.Server, bool, CleanupSkipReason, error) {
			server.Labels = CloneLabels(server.Labels)
			server.Labels["prepared"] = "true"
			return server, true, "", nil
		},
		Delete: func(_ context.Context, _ core.Config, server core.Server) error {
			if server.Labels["prepared"] != "true" {
				t.Fatalf("Delete server=%#v, want prepared server", server)
			}
			return nil
		},
	}
	if err := backend.CleanupServers(context.Background(), core.CleanupRequest{}, []core.Server{testCleanupServer("prepared", clock.now.Add(-time.Hour))}); err != nil {
		t.Fatal(err)
	}
}

func TestCleanupServersEmitsPrepareSkipReason(t *testing.T) {
	clock := &testCleanupClock{now: time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)}
	var stderr bytes.Buffer
	backend := DirectSSHBackend{
		RT: core.Runtime{Stderr: &stderr, Clock: clock},
		PrepareCleanup: func(_ context.Context, server core.Server) (core.Server, bool, CleanupSkipReason, error) {
			return server, false, CleanupSkipReason("provider-scope-mismatch"), nil
		},
		Delete: func(context.Context, core.Config, core.Server) error {
			t.Fatal("Delete called for skipped server")
			return nil
		},
	}
	if err := backend.CleanupServers(context.Background(), core.CleanupRequest{}, []core.Server{testCleanupServer("skipped", clock.now.Add(-time.Hour))}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "reason=provider-scope-mismatch") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestCleanupServersDryRunPreparesWithoutDelete(t *testing.T) {
	clock := &testCleanupClock{now: time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)}
	prepared := 0
	backend := DirectSSHBackend{
		RT: core.Runtime{Stderr: io.Discard, Clock: clock},
		PrepareCleanup: func(_ context.Context, server core.Server) (core.Server, bool, CleanupSkipReason, error) {
			prepared++
			return server, true, "", nil
		},
		Delete: func(context.Context, core.Config, core.Server) error {
			t.Fatal("Delete called during dry-run")
			return nil
		},
	}
	if err := backend.CleanupServers(context.Background(), core.CleanupRequest{DryRun: true}, []core.Server{testCleanupServer("dry", clock.now.Add(-time.Hour))}); err != nil {
		t.Fatal(err)
	}
	if prepared != 1 {
		t.Fatalf("prepared=%d, want 1", prepared)
	}
}

func TestCleanupServersRechecksPreparedServerAndClaimEligibility(t *testing.T) {
	clock := &testCleanupClock{now: time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)}
	for _, test := range []struct {
		name    string
		prepare func(core.Server) core.Server
		reason  string
	}{
		{
			name: "refreshed server is kept",
			prepare: func(server core.Server) core.Server {
				server.Labels = CloneLabels(server.Labels)
				server.Labels["keep"] = "true"
				return server
			},
			reason: "keep=true",
		},
		{
			name: "prepared claim was renewed",
			prepare: func(server core.Server) core.Server {
				claim := core.LeaseClaim{
					LeaseID:  "cbx_aaaaaaaaaaaa",
					Provider: "example",
					Revision: "renewed",
					Labels: map[string]string{
						"lease":      "cbx_aaaaaaaaaaaa",
						"keep":       "false",
						"expires_at": clock.now.Add(time.Hour).Format(time.RFC3339Nano),
					},
				}
				server.Labels = CloneLabels(server.Labels)
				server.Labels["lease"] = claim.LeaseID
				core.SetServerLeaseClaimSnapshot(&server, claim, true)
				return server
			},
			reason: "not expired",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stderr bytes.Buffer
			backend := DirectSSHBackend{
				RT: core.Runtime{Stderr: &stderr, Clock: clock},
				PrepareCleanup: func(_ context.Context, server core.Server) (core.Server, bool, CleanupSkipReason, error) {
					return test.prepare(server), true, "", nil
				},
				Delete: func(context.Context, core.Config, core.Server) error {
					t.Fatal("Delete called after prepared cleanup eligibility changed")
					return nil
				},
			}
			if err := backend.CleanupServers(context.Background(), core.CleanupRequest{}, []core.Server{testCleanupServer("renewed", clock.now.Add(-time.Hour))}); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(stderr.String(), "reason="+test.reason) {
				t.Fatalf("stderr=%q, want reason=%s", stderr.String(), test.reason)
			}
		})
	}
}

func TestCleanupServersRejectsSimultaneousPreparationHooks(t *testing.T) {
	backend := DirectSSHBackend{
		SpecValue: core.ProviderSpec{Name: "test-provider"},
		RT:        core.Runtime{Stderr: io.Discard},
		PrepareCleanup: func(_ context.Context, server core.Server) (core.Server, bool, CleanupSkipReason, error) {
			return server, true, "", nil
		},
		CleanupEligible: func(context.Context, core.Server) (bool, error) { return true, nil },
	}
	err := backend.CleanupServers(context.Background(), core.CleanupRequest{}, nil)
	if err == nil || !strings.Contains(err.Error(), "cannot configure both PrepareCleanup and CleanupEligible") {
		t.Fatalf("err=%v", err)
	}
}

func TestCleanupClaimEligible(t *testing.T) {
	if eligible, err := CleanupClaimEligible(nil); err != nil || !eligible {
		t.Fatalf("nil error eligible=%v err=%v", eligible, err)
	}
	if eligible, err := CleanupClaimEligible(core.Exit(2, "claim mismatch")); err != nil || eligible {
		t.Fatalf("claim mismatch eligible=%v err=%v", eligible, err)
	}
	want := errors.New("read claim")
	if eligible, err := CleanupClaimEligible(want); eligible || !errors.Is(err, want) {
		t.Fatalf("read failure eligible=%v err=%v", eligible, err)
	}
}

func TestServerWithDefaultLabelCopiesLabels(t *testing.T) {
	original := core.Server{Labels: map[string]string{"lease": "cbx_123456789abc"}}
	updated := ServerWithDefaultLabel(original, "provider_account", "account:test")
	if updated.Labels["provider_account"] != "account:test" || updated.Labels["lease"] != original.Labels["lease"] {
		t.Fatalf("updated labels=%v", updated.Labels)
	}
	if _, ok := original.Labels["provider_account"]; ok {
		t.Fatalf("original labels mutated: %v", original.Labels)
	}
	existing := ServerWithDefaultLabel(updated, "provider_account", "account:other")
	if existing.Labels["provider_account"] != "account:test" {
		t.Fatalf("existing label overwritten: %v", existing.Labels)
	}
}

func TestAcquireAttemptsRetry(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		lease := core.LeaseTarget{LeaseID: "lease-1"}
		got, err := AcquireAttemptsRetry(core.Runtime{Stderr: io.Discard}, false, func() (core.LeaseTarget, error) {
			return lease, nil
		})
		if err != nil {
			t.Fatalf("AcquireAttemptsRetry err=%v", err)
		}
		if got.LeaseID != lease.LeaseID {
			t.Fatalf("lease=%#v, want %#v", got, lease)
		}
	})

	t.Run("retryable bootstrap failure succeeds", func(t *testing.T) {
		var stderr bytes.Buffer
		attempts := 0
		lease := core.LeaseTarget{LeaseID: "lease-2"}
		got, err := AcquireAttemptsRetry(core.Runtime{Stderr: &stderr}, false, func() (core.LeaseTarget, error) {
			attempts++
			if attempts == 1 {
				return core.LeaseTarget{}, bootstrapWaitErr()
			}
			return lease, nil
		})
		if err != nil {
			t.Fatalf("AcquireAttemptsRetry err=%v", err)
		}
		if got.LeaseID != lease.LeaseID || attempts != 2 {
			t.Fatalf("lease=%#v attempts=%d, want lease-2 after 2 attempts", got, attempts)
		}
		if !strings.Contains(stderr.String(), "warning: bootstrap failed; retrying with fresh lease") {
			t.Fatalf("stderr=%q, want retry warning", stderr.String())
		}
	})

	t.Run("retryable bootstrap failure stops after configured attempts", func(t *testing.T) {
		for _, keep := range []bool{false, true} {
			attempts := 0
			_, err := AcquireAttemptsRetry(core.Runtime{Stderr: io.Discard}, keep, func() (core.LeaseTarget, error) {
				attempts++
				return core.LeaseTarget{}, bootstrapWaitErr()
			})
			if err == nil || !core.IsBootstrapWaitError(err) {
				t.Fatalf("keep=%t err=%v, want bootstrap wait error", keep, err)
			}
			if want := core.AcquireAttempts(keep); attempts != want {
				t.Fatalf("keep=%t attempts=%d, want %d", keep, attempts, want)
			}
		}
	})

	t.Run("non retryable failure stops immediately", func(t *testing.T) {
		var stderr bytes.Buffer
		wantErr := errors.New("quota denied")
		attempts := 0
		_, err := AcquireAttemptsRetry(core.Runtime{Stderr: &stderr}, false, func() (core.LeaseTarget, error) {
			attempts++
			return core.LeaseTarget{}, wantErr
		})
		if !errors.Is(err, wantErr) {
			t.Fatalf("err=%v, want %v", err, wantErr)
		}
		if attempts != 1 {
			t.Fatalf("attempts=%d, want 1", attempts)
		}
		if stderr.Len() != 0 {
			t.Fatalf("stderr=%q, want no retry warning", stderr.String())
		}
	})
}

type testCleanupClock struct {
	now time.Time
}

func (c *testCleanupClock) Now() time.Time {
	return c.now
}

func testCleanupServer(name string, expiresAt time.Time) core.Server {
	return testCleanupServerWithLabels(name, map[string]string{
		"keep":       "false",
		"expires_at": expiresAt.Format(time.RFC3339Nano),
	})
}

func testCleanupServerWithLabels(name string, labels map[string]string) core.Server {
	return core.Server{CloudID: name, Name: name, Labels: labels}
}

func bootstrapWaitErr() error {
	return core.Exit(5, "timed out waiting for SSH: test")
}
