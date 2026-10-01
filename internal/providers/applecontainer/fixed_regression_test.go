package applecontainer

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
)

func TestFixedPinnedDigestMismatchDoesNotReenterClaimLock(t *testing.T) {
	const child = "CRABBOX_TEST_FIXED_DIGEST_CHILD"
	if os.Getenv(child) != "1" {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestFixedPinnedDigestMismatchDoesNotReenterClaimLock$")
		cmd.Env = append(os.Environ(), child+"=1")
		out, err := cmd.CombinedOutput()
		if ctx.Err() != nil {
			t.Fatal("fixed digest rollback deadlocked while holding the claim lock")
		}
		if err != nil {
			t.Fatalf("digest rejection: %v\n%s", err, out)
		}
		return
	}
	b, r, cfg := pinnedFixture(t)
	r.hook = func(r *pinnedImageRunner, req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		if req.Args[0] == "inspect" {
			r.configuration["image"].(map[string]any)["descriptor"] = map[string]string{"digest": "sha256:" + strings.Repeat("0", 64)}
		}
		return core.LocalCommandResult{}, nil, false
	}
	err := core.WithDurableLeaseClaimLock("cbx_123456789abc", func(_ *core.LeaseClaim, _ bool, _ func() error) error {
		_, err := b.createContainerWithFixedIntentUnderLeaseLock(t.Context(), cfg, "crabbox-fixed", "cbx_123456789abc", "fixed", "public-fixture", false, "fingerprint")
		return err
	})
	var retained *retainedImageContainerError
	if !errors.As(err, &retained) || commandWasCalled(r.calls, "start") || r.deleted {
		t.Fatalf("unverified fixed target must be retained without starting: err=%v deleted=%v", err, r.deleted)
	}
}

func TestFixedAcquireRejectsUnverifiedBinding(t *testing.T) {
	testutil.IsolateUserDirs(t)
	const leaseID = "cbx_123456789abc"
	r := &recordingRunner{responses: map[string]core.LocalCommandResult{
		"ls":      {Stdout: "[]"},
		"run":     {Stdout: "unrelated-container"},
		"inspect": {Stdout: sampleInspectJSON("unrelated-container", "fixed", leaseID)},
	}}
	b := testBackend(r)
	_, err := b.acquireFixed(t.Context(), core.AcquireRequest{RequestedLeaseID: leaseID, RequestedSlug: "fixed", Repo: core.Repo{Root: t.TempDir()}}, b.configForRun())
	if err == nil || !strings.Contains(err.Error(), "lease_id_conflict") {
		t.Fatalf("unexpected acquisition error: %v", err)
	}
	claim, err := core.ReadLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.CloudID != "" || claim.FixedCreateIntent.Attempt["container_id"] != "" {
		t.Fatalf("unverified native response became cleanup authority: cloudID=%q attempt=%v", claim.CloudID, claim.FixedCreateIntent.Attempt)
	}
}

func TestFixedAcquireRetriesVerifiedStoppedStart(t *testing.T) {
	b, r, cfg := pinnedFixture(t)
	starts := 0
	r.hook = func(r *pinnedImageRunner, req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		if req.Args[0] == "ls" && r.configuration != nil {
			data, _ := json.Marshal([]any{map[string]any{"configuration": r.configuration, "status": "stopped"}})
			return core.LocalCommandResult{Stdout: string(data)}, nil, true
		}
		if req.Args[0] == "start" {
			starts++
			return core.LocalCommandResult{ExitCode: 1}, nil, true
		}
		return core.LocalCommandResult{}, nil, false
	}
	req := core.AcquireRequest{RequestedLeaseID: "cbx_123456789abc", RequestedSlug: "fixed", Repo: core.Repo{Root: t.TempDir()}}
	for i := 0; i < 2; i++ {
		if _, err := b.acquireFixed(t.Context(), req, cfg); err == nil {
			t.Fatal("injected start failure was ignored")
		}
	}
	if starts != 2 {
		t.Fatalf("start attempts=%d, want retry to reverify and restart the same pending container", starts)
	}
	creates := 0
	for _, c := range r.calls {
		if c.Args[0] == "create" {
			creates++
		}
	}
	if creates != 1 {
		t.Fatalf("create attempts=%d, want 1", creates)
	}
}

func TestFixedReplayRejectsChangedPinnedDigest(t *testing.T) {
	b, r, cfg := pinnedFixture(t)
	r.hook = func(r *pinnedImageRunner, req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		if req.Args[0] == "ls" && r.configuration != nil {
			data, _ := json.Marshal([]any{map[string]any{"configuration": r.configuration, "status": "stopped"}})
			return core.LocalCommandResult{Stdout: string(data)}, nil, true
		}
		if req.Args[0] == "start" {
			return core.LocalCommandResult{ExitCode: 1}, nil, true
		}
		return core.LocalCommandResult{}, nil, false
	}
	req := core.AcquireRequest{RequestedLeaseID: "cbx_123456789abc", RequestedSlug: "fixed", Repo: core.Repo{Root: t.TempDir()}}
	if _, err := b.acquireFixed(t.Context(), req, cfg); err == nil {
		t.Fatal("injected start failure was ignored")
	}
	r.configuration["image"].(map[string]any)["descriptor"] = map[string]string{"digest": "sha256:" + strings.Repeat("0", 64)}
	_, err := b.acquireFixed(t.Context(), req, cfg)
	if err == nil || !strings.Contains(err.Error(), "lease_id_conflict") {
		t.Fatalf("changed pinned digest was accepted for replay: %v", err)
	}
	claim, err := core.ReadLeaseClaim(req.RequestedLeaseID)
	if err != nil || claim.CloudID != "" {
		t.Fatalf("changed image became bound: cloudID=%q err=%v", claim.CloudID, err)
	}
}
