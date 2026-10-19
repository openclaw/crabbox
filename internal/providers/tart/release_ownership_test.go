package tart

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestReleaseLeaseTargetForms(t *testing.T) {
	for _, kind := range []string{"resolved", "lease-label", "instance-label", "lease-only", "slug-only", "missing-lease-only"} {
		t.Run(kind, func(t *testing.T) {
			b, runner, claim := cleanupFixture(t)
			lease, err := b.Resolve(t.Context(), core.ResolveRequest{ID: claim.LeaseID, ReleaseOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "lease-label":
				lease.LeaseID = ""
			case "instance-label":
				lease.Server.CloudID = ""
			case "lease-only":
				lease = core.LeaseTarget{LeaseID: claim.LeaseID}
			case "slug-only":
				lease = core.LeaseTarget{LeaseID: claim.Slug}
			case "missing-lease-only":
				lease = core.LeaseTarget{LeaseID: claim.LeaseID}
				runner.responses["list"] = core.LocalCommandResult{Stdout: "[]"}
				if err := os.RemoveAll(filepath.Join(claim.Labels["tart_storage"], "vms", cleanupVM)); err != nil {
					t.Fatal(err)
				}
			}
			if err := b.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
				t.Fatal(err)
			}
			if _, exists, err := core.ReadLeaseClaimWithPresence(claim.LeaseID); err != nil || exists {
				t.Fatalf("claim retained: exists=%v err=%v", exists, err)
			}
		})
	}
}

func TestReleaseFencesClaimThroughStopDeleteAndRetirement(t *testing.T) {
	b, runner, claim := cleanupFixture(t)
	runner.responses["list"] = core.LocalCommandResult{Stdout: `[{"Name":"` + cleanupVM + `","State":"running","Running":true}]`}
	started, done := make(chan struct{}), make(chan error, 1)
	var once sync.Once
	runner.onRun = func(req core.LocalCommandRequest) {
		if req.Args[0] != "stop" && req.Args[0] != "delete" {
			return
		}
		once.Do(func() {
			go func() {
				close(started)
				done <- core.WithLeaseClaimUnchanged(claim.LeaseID, claim, func() error { return errors.New("writer entered stale claim") })
			}()
			<-started
		})
		select {
		case err := <-done:
			t.Fatalf("claim writer passed %s fence: %v", req.Args[0], err)
		case <-time.After(50 * time.Millisecond):
		}
	}
	if err := b.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: releaseTarget(b, claim)}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil || strings.Contains(err.Error(), "writer entered") {
			t.Fatalf("stale writer accepted after release: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("writer did not finish after release")
	}
}

func TestReleaseCancellationWhileClaimLocked(t *testing.T) {
	b, runner, claim := cleanupFixture(t)
	if err := core.WithLeaseClaimUnchanged(claim.LeaseID, claim, func() error {
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()
		err := b.ReleaseLease(ctx, core.ReleaseLeaseRequest{Lease: releaseTarget(b, claim)})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("release did not honor cancellation while waiting for claim: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	assertNoTartMutation(t, runner)
	if err := core.VerifyLeaseClaimUnchanged(claim.LeaseID, claim); err != nil {
		t.Fatal(err)
	}
}

func releaseTarget(b *backend, claim core.LeaseClaim) core.LeaseTarget {
	return core.LeaseTarget{LeaseID: claim.LeaseID, Server: b.serverFromInstance(tartInstance{Name: cleanupVM, State: "running"}, claim, b.configForRun())}
}

func releaseKey(t *testing.T, leaseID string) string {
	t.Helper()
	key, err := core.PrepareStoredTestboxKeyPath(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, []byte("synthetic SSH key"), 0o600); err != nil {
		t.Fatal(err)
	}
	return key
}

func TestReleaseRejectsChangedOwnershipAndAuthority(t *testing.T) {
	for _, kind := range []string{"no-snapshot", "no-claim", "legacy", "duplicate", "store", "missing-marker", "replaced-marker", "recreated-vm", "new-revision", "target-name", "target-identity", "target-lease"} {
		t.Run(kind, func(t *testing.T) {
			b, runner, claim := cleanupFixture(t)
			lease := releaseTarget(b, claim)
			key := releaseKey(t, claim.LeaseID)
			marker := filepath.Join(claim.Labels["tart_storage"], "vms", cleanupVM, tartOwnershipFile)
			switch kind {
			case "no-snapshot":
				lease.Server = core.Server{CloudID: cleanupVM, Labels: map[string]string{"lease": cleanupLease}}
			case "no-claim":
				core.RemoveLeaseClaim(claim.LeaseID)
			case "legacy":
				claim.CloudImmutableID = ""
				writeCleanupClaim(t, claim)
				lease = releaseTarget(b, claim)
			case "duplicate":
				claimTartLease(t, t.TempDir(), "cbx_duplicate", cleanupVM, "stopped")
			case "store":
				t.Setenv("TART_HOME", t.TempDir())
			case "missing-marker":
				if err := os.Remove(marker); err != nil {
					t.Fatal(err)
				}
			case "replaced-marker":
				if err := os.WriteFile(marker, []byte(strings.Repeat("0", 64)+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "recreated-vm":
				if err := os.RemoveAll(filepath.Dir(marker)); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Dir(marker), 0o700); err != nil {
					t.Fatal(err)
				}
				if _, _, err := createTartVMIdentity(cleanupVM); err != nil {
					t.Fatal(err)
				}
			case "new-revision":
				if _, err := core.UpdateLeaseClaimLabelsIfUnchangedAfter(claim.LeaseID, claim, map[string]string{"keep": "true"}, nil); err != nil {
					t.Fatal(err)
				}
			case "target-name":
				lease.Server.CloudID = "crabbox-other"
			case "target-identity":
				lease.Server.ImmutableID = strings.Repeat("0", 64)
			case "target-lease":
				lease.LeaseID = "cbx_other"
			}
			expected, exists, err := core.ReadLeaseClaimWithPresence(claim.LeaseID)
			if err != nil {
				t.Fatal(err)
			}
			err = b.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease})
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), "release") {
				t.Fatalf("expected clear release refusal, got %v", err)
			}
			assertNoTartMutation(t, runner)
			if exists {
				if err := core.VerifyLeaseClaimUnchanged(claim.LeaseID, expected); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := os.Stat(key); err != nil {
				t.Fatalf("release removed key: %v", err)
			}
		})
	}
}

func TestReleaseRequiresFreshStorageAbsence(t *testing.T) {
	for _, kind := range []string{"absent", "present-directory", "replacement", "different-store", "new-revision", "inventory-error"} {
		t.Run(kind, func(t *testing.T) {
			b, runner, claim := cleanupFixture(t)
			lease := releaseTarget(b, claim)
			lease.Server.Status = "missing"
			key := releaseKey(t, claim.LeaseID)
			runner.responses["list"] = core.LocalCommandResult{Stdout: "[]"}
			dir := filepath.Join(claim.Labels["tart_storage"], "vms", cleanupVM)
			if kind != "present-directory" && kind != "replacement" {
				if err := os.RemoveAll(dir); err != nil {
					t.Fatal(err)
				}
			}
			switch kind {
			case "replacement":
				if err := os.WriteFile(filepath.Join(dir, tartOwnershipFile), []byte(strings.Repeat("0", 64)+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "different-store":
				t.Setenv("TART_HOME", t.TempDir())
			case "new-revision":
				if _, err := core.UpdateLeaseClaimLabelsIfUnchangedAfter(claim.LeaseID, claim, map[string]string{"keep": "true"}, nil); err != nil {
					t.Fatal(err)
				}
			case "inventory-error":
				runner.errors = map[string]error{"list": errors.New("inventory unavailable")}
			}
			expected, err := core.ReadLeaseClaim(claim.LeaseID)
			if err != nil {
				t.Fatal(err)
			}
			err = b.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease})
			if kind == "absent" {
				if err != nil {
					t.Fatal(err)
				}
				if _, exists, err := core.ReadLeaseClaimWithPresence(claim.LeaseID); err != nil || exists {
					t.Fatalf("claim retained: exists=%v err=%v", exists, err)
				}
				if len(runner.calls) == 0 {
					t.Fatal("absence was not refreshed")
				}
			} else {
				if err == nil {
					t.Fatal("release accepted unverified absence")
				}
				if err := core.VerifyLeaseClaimUnchanged(claim.LeaseID, expected); err != nil {
					t.Fatal(err)
				}
			}
			assertNoTartMutation(t, runner)
			if _, err := os.Stat(key); err != nil {
				t.Fatalf("release removed potentially reused key: %v", err)
			}
		})
	}
}

func TestReleaseOwnedVMAndRetainsRetryAuthority(t *testing.T) {
	for _, kind := range []string{"owned-running", "owned-stopped", "stop-error", "delete-error", "marker-after-stop"} {
		t.Run(kind, func(t *testing.T) {
			b, runner, claim := cleanupFixture(t)
			// Explicit release must work even when automatic cleanup is ineligible.
			claim.Labels["keep"] = "true"
			claim.Labels["expires_at"] = core.LeaseLabelTime(time.Now().Add(time.Hour))
			writeCleanupClaim(t, claim)
			lease := releaseTarget(b, claim)
			key := releaseKey(t, claim.LeaseID)
			if kind != "owned-stopped" {
				runner.responses["list"] = core.LocalCommandResult{Stdout: `[{"Name":"` + cleanupVM + `","State":"running","Running":true}]`}
			}
			if kind == "stop-error" {
				runner.errors = map[string]error{"stop": errors.New("stop failed")}
			}
			if kind == "delete-error" {
				runner.errors = map[string]error{"delete": errors.New("delete failed")}
			}
			if kind == "marker-after-stop" {
				runner.onRun = func(req core.LocalCommandRequest) {
					if req.Args[0] == "stop" {
						if err := os.Remove(filepath.Join(claim.Labels["tart_storage"], "vms", cleanupVM, tartOwnershipFile)); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			err := b.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease})
			if strings.HasPrefix(kind, "owned-") {
				if err != nil {
					t.Fatal(err)
				}
				if _, exists, err := core.ReadLeaseClaimWithPresence(claim.LeaseID); err != nil || exists {
					t.Fatalf("claim retained: exists=%v err=%v", exists, err)
				}
			} else {
				if err == nil {
					t.Fatal("release ignored failed operation or changed ownership")
				}
				if err := core.VerifyLeaseClaimUnchanged(claim.LeaseID, claim); err != nil {
					t.Fatal(err)
				}
			}
			var stopped, deleted bool
			for _, call := range runner.calls {
				stopped = stopped || call.Args[0] == "stop"
				deleted = deleted || call.Args[0] == "delete"
			}
			if stopped != (kind != "owned-stopped") || deleted != (kind != "stop-error" && kind != "marker-after-stop") {
				t.Fatalf("unexpected commands: stop=%v delete=%v", stopped, deleted)
			}
			if _, err := os.Stat(key); err != nil {
				t.Fatalf("release removed potentially reused key: %v", err)
			}
		})
	}
}
