package applecontainer

import (
	"context"
	"encoding/json"
	"io"
	"runtime"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func fixedLifecycleFixture(t *testing.T) (*backend, *pinnedImageRunner, core.Config, core.AcquireRequest) {
	t.Helper()
	b, r, cfg := pinnedFixture(t)
	r.hook = func(r *pinnedImageRunner, req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		if req.Args[0] != "ls" && req.Args[0] != "inspect" {
			return core.LocalCommandResult{}, nil, false
		}
		entries := []any{}
		if r.configuration != nil && !r.deleted {
			entries = append(entries, map[string]any{"configuration": r.configuration, "status": r.state, "networks": []map[string]string{{"address": "192.0.2.10/24"}}})
		}
		data, _ := json.Marshal(entries)
		return core.LocalCommandResult{Stdout: string(data)}, nil, true
	}
	b.waitForSSH = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error { return nil }
	return b, r, cfg, core.AcquireRequest{RequestedLeaseID: "cbx_123456789abc", RequestedSlug: "fixed", Repo: core.Repo{Root: t.TempDir()}}
}

func TestFixedLifecycleReplayAndRelease(t *testing.T) {
	b, r, cfg, req := fixedLifecycleFixture(t)
	req.OnAcquired = func(lease core.LeaseTarget) error {
		claim, err := core.ReadLeaseClaim(lease.LeaseID)
		if err != nil {
			return err
		}
		if claim.FixedCreateIntent.State != "acquired" {
			t.Errorf("callback saw incomplete claim")
		}
		return nil
	}
	first, err := b.acquireFixed(t.Context(), req, cfg)
	if err != nil {
		t.Fatal(err)
	}
	restarted := newBackend(b.spec, cfg, b.rt).(*backend)
	restarted.waitForSSH = b.waitForSSH
	second, err := restarted.acquireFixed(t.Context(), req, cfg)
	if err != nil || first.LeaseID != req.RequestedLeaseID || second.Server.CloudID != first.Server.CloudID {
		t.Fatalf("replay=%v err=%v", second, err)
	}
	for _, verb := range []string{"create", "start"} {
		count := 0
		for _, call := range r.calls {
			if call.Args[0] == verb {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("%s calls=%d, want 1", verb, count)
		}
	}
	cfg.AppleContainer.Memory = "16g"
	if _, err := b.acquireFixed(t.Context(), req, cfg); err == nil || !strings.Contains(err.Error(), "lease_id_conflict") {
		t.Fatalf("changed intent: %v", err)
	}
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		return
	}
	previous, err := core.ReadLeaseClaim(req.RequestedLeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: second}); err != nil {
		t.Fatal(err)
	}
	if !r.deleted {
		t.Fatal("release did not delete owned container")
	}
	retained, err := restarted.RetainLeaseClaimAfterReleaseWithClaim(second, previous)
	if err != nil || !retained {
		t.Fatalf("terminal receipt retained=%v err=%v", retained, err)
	}
	if err := restarted.Cleanup(t.Context(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	calls := len(r.calls)
	if _, err := restarted.acquireFixed(t.Context(), req, restarted.configForRun()); err == nil || !strings.Contains(err.Error(), "terminal") {
		t.Fatalf("terminal replay: %v", err)
	}
	if len(r.calls) != calls {
		t.Fatal("terminal replay reached the provider")
	}
}

func TestFixedLifecycleRecoversStartFailure(t *testing.T) {
	b, r, cfg, req := fixedLifecycleFixture(t)
	native := r.hook
	failStart := true
	r.hook = func(r *pinnedImageRunner, request core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		if request.Args[0] == "start" && failStart {
			return core.LocalCommandResult{ExitCode: 1}, nil, true
		}
		return native(r, request)
	}
	if _, err := b.acquireFixed(t.Context(), req, cfg); err == nil {
		t.Fatal("injected start failure was ignored")
	}
	failStart = false
	lease, err := b.acquireFixed(t.Context(), req, cfg)
	if err != nil || lease.LeaseID != req.RequestedLeaseID || r.state != "running" {
		t.Fatalf("recover lease=%v err=%v state=%s", lease, err, r.state)
	}
	creates := 0
	for _, call := range r.calls {
		if call.Args[0] == "create" {
			creates++
		}
	}
	if creates != 1 {
		t.Fatalf("recovery created %d containers", creates)
	}
}

func TestFixedLifecycleRejectsMissingDuplicateAndChangedResources(t *testing.T) {
	for _, kind := range []string{"missing", "duplicate", "digest", "fingerprint", "stopped", "inventory-error"} {
		t.Run(kind, func(t *testing.T) {
			b, r, cfg, req := fixedLifecycleFixture(t)
			if _, err := b.acquireFixed(t.Context(), req, cfg); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "missing":
				r.deleted = true
			case "digest":
				r.configuration["image"].(map[string]any)["descriptor"] = map[string]string{"digest": "sha256:" + strings.Repeat("0", 64)}
			case "fingerprint":
				r.configuration["labels"].(map[string]string)["fixed_intent_sha256"] = strings.Repeat("0", 64)
			case "stopped":
				r.state = "stopped"
			case "duplicate", "inventory-error":
				native := r.hook
				r.hook = func(r *pinnedImageRunner, request core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
					result, err, handled := native(r, request)
					if request.Args[0] == "ls" {
						if kind == "inventory-error" {
							return core.LocalCommandResult{}, context.DeadlineExceeded, true
						}
						var entries []any
						if err := json.Unmarshal([]byte(result.Stdout), &entries); err != nil {
							t.Fatal(err)
						}
						data, _ := json.Marshal(append(entries, entries[0]))
						result.Stdout = string(data)
					}
					return result, err, handled
				}
			}
			calls := len(r.calls)
			if _, err := b.acquireFixed(t.Context(), req, cfg); err == nil {
				t.Fatal("replay accepted " + kind)
			}
			for _, call := range r.calls[calls:] {
				switch call.Args[0] {
				case "create", "run", "start", "delete":
					t.Fatalf("unsafe mutation after %s: %s", kind, call.Args[0])
				}
			}
		})
	}
}

func TestFixedCleanupRetainsUnresolvedAttempt(t *testing.T) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("apple-container Cleanup requires macOS on Apple silicon")
	}
	b, r, cfg, req := fixedLifecycleFixture(t)
	native := r.hook
	r.hook = func(r *pinnedImageRunner, request core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		if request.Args[0] == "start" {
			return core.LocalCommandResult{ExitCode: 1}, nil, true
		}
		return native(r, request)
	}
	if _, err := b.acquireFixed(t.Context(), req, cfg); err == nil {
		t.Fatal("injected start failure was ignored")
	}
	// Cleanup's inventory can predate completion of a native create whose
	// failed start leaves the durable attempt unchanged.
	r.hook = func(r *pinnedImageRunner, request core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		if request.Args[0] == "ls" {
			return core.LocalCommandResult{Stdout: "[]"}, nil, true
		}
		return native(r, request)
	}
	if err := b.Cleanup(t.Context(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	claim, err := core.ReadLeaseClaim(req.RequestedLeaseID)
	if err != nil || claim.FixedCreateIntent == nil || claim.FixedCreateIntent.State != "prepared" {
		t.Fatalf("cleanup discarded unresolved create evidence: intent=%+v err=%v", claim.FixedCreateIntent, err)
	}
	r.hook = native
	if _, err := b.acquireFixed(t.Context(), req, cfg); err != nil {
		t.Fatalf("pending acquisition cannot recover after cleanup: %v", err)
	}
}

func TestFixedReleaseRequiresMatchingResourceAndConfirmedAbsence(t *testing.T) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("apple-container release requires macOS on Apple silicon")
	}
	for _, kind := range []string{"fingerprint", "delete-retained", "inventory-invalid", "inventory-failure"} {
		t.Run(kind, func(t *testing.T) {
			b, r, cfg, req := fixedLifecycleFixture(t)
			lease, err := b.acquireFixed(t.Context(), req, cfg)
			if err != nil {
				t.Fatal(err)
			}
			native := r.hook
			r.hook = func(r *pinnedImageRunner, request core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
				if kind == "delete-retained" && request.Args[0] == "delete" {
					return core.LocalCommandResult{}, nil, true
				}
				if request.Args[0] == "ls" {
					if kind == "inventory-invalid" {
						return core.LocalCommandResult{Stdout: "null"}, nil, true
					}
					if kind == "inventory-failure" {
						return core.LocalCommandResult{}, context.DeadlineExceeded, true
					}
				}
				return native(r, request)
			}
			if kind == "fingerprint" {
				r.configuration["labels"].(map[string]string)["fixed_intent_sha256"] = strings.Repeat("0", 64)
			}
			if err := b.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease}); err == nil {
				t.Fatal("release accepted " + kind)
			}
			if kind == "fingerprint" && r.deleted {
				t.Fatal("release deleted a replacement container")
			}
			claim, err := core.ReadLeaseClaim(req.RequestedLeaseID)
			if err != nil || claim.FixedCreateIntent == nil || claim.FixedCreateIntent.State == "released" {
				t.Fatalf("unconfirmed release discarded cleanup authority: %+v %v", claim.FixedCreateIntent, err)
			}
		})
	}
}
