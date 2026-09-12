package tenki

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func requireFixedClaim(t *testing.T) LeaseClaim {
	t.Helper()
	claim, exists, err := core.ReadLeaseClaimWithPresence(testFixedTenkiID)
	if err != nil || !exists || claim.FixedCreateIntent == nil {
		t.Fatalf("fixed claim missing: %+v exists=%t err=%v", claim, exists, err)
	}
	return claim
}

func requireFixedConflict(t *testing.T, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "lease_id_conflict") {
		t.Fatalf("err=%v, want fixed identity conflict", err)
	}
}

func requireFixedAcquire(t *testing.T, b *tenkiBackend, req AcquireRequest) LeaseTarget {
	t.Helper()
	lease, err := b.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return lease
}

func requireFixedReleaseTarget(t *testing.T, b *tenkiBackend) LeaseTarget {
	t.Helper()
	lease, err := b.Resolve(context.Background(), ResolveRequest{ID: testFixedTenkiID, ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	return lease
}

func TestTenkiFixedReplayRejectsRequestDrift(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Config, *AcquireRequest)
	}{
		{"slug", func(_ *Config, r *AcquireRequest) { r.RequestedSlug = "different-operation" }},
		{"keep", func(_ *Config, r *AcquireRequest) { r.Keep = false }},
		{"ttl", func(c *Config, _ *AcquireRequest) { c.TTL += time.Second }},
		{"idle", func(c *Config, _ *AcquireRequest) { c.IdleTimeout += time.Second }},
		{"endpoint", func(c *Config, _ *AcquireRequest) { c.Tenki.Endpoint = "https://other.example" }},
		{"gateway", func(c *Config, _ *AcquireRequest) { c.Tenki.Gateway = "wss://other.example" }},
		{"cli", func(c *Config, _ *AcquireRequest) { c.Tenki.CLIPath = "/other/tenki" }},
		{"work root", func(c *Config, _ *AcquireRequest) { c.Tenki.WorkRoot = "/home/tenki/other" }},
		{"image", func(c *Config, _ *AcquireRequest) { c.Tenki.Image = "other/image" }},
		{"snapshot", func(c *Config, _ *AcquireRequest) { c.Tenki.Image, c.Tenki.Snapshot = "", "snapshot-other" }},
		{"cpu", func(c *Config, _ *AcquireRequest) { c.Tenki.CPUs++ }},
		{"memory", func(c *Config, _ *AcquireRequest) { c.Tenki.MemoryMB++ }},
		{"disk", func(c *Config, _ *AcquireRequest) { c.Tenki.DiskGB++ }},
		{"profile", func(c *Config, _ *AcquireRequest) { c.Profile = "other" }},
		{"pond", func(c *Config, _ *AcquireRequest) { c.Pond = "other" }},
		{"desktop", func(c *Config, _ *AcquireRequest) { c.Desktop = true }},
		{"cache", func(c *Config, _ *AcquireRequest) { c.Cache.PurgeOnRelease = true }},
		{"foreign repo even with reclaim", func(_ *Config, r *AcquireRequest) { r.Repo.Root += "/other"; r.Reclaim = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, f, req := newFixedTenkiTest(t)
			requireFixedAcquire(t, b, req)
			before := requireFixedClaim(t)
			calls := maps.Clone(f.calls)
			fresh := *b
			tc.change(&fresh.cfg, &req)
			_, err := fresh.Acquire(context.Background(), req)
			requireFixedConflict(t, err)
			if !reflect.DeepEqual(before, requireFixedClaim(t)) || !maps.Equal(calls, f.calls) {
				t.Fatalf("drift caused a claim/provider effect: calls=%v want=%v", f.calls, calls)
			}
		})
	}
}

func TestTenkiFixedReplayPreservesHeartbeatLabels(t *testing.T) {
	b, f, req := newFixedTenkiTest(t)
	first := requireFixedAcquire(t, b, req)
	before := requireFixedClaim(t)
	idle := 15 * time.Minute
	touched, err := b.Touch(context.Background(), TouchRequest{Lease: first, State: "ready", IdleTimeout: idle, IdleTimeoutOverride: &idle})
	if err != nil {
		t.Fatal(err)
	}
	labels := touched.Labels
	if labels["idle_timeout_secs"] != "900" || requireFixedClaim(t).IdleTimeoutSeconds != 900 {
		t.Fatalf("heartbeat did not persist the override: labels=%v claim=%+v", labels, requireFixedClaim(t))
	}
	req.RequestedSlug = " Build Linux " // Same normalized request, not the allocated slug.
	second := requireFixedAcquire(t, b, req)
	claim := requireFixedClaim(t)
	if second.Server.CloudID != first.Server.CloudID || f.calls["create"] != 1 || !reflect.DeepEqual(before.FixedCreateIntent, claim.FixedCreateIntent) {
		t.Fatalf("replay changed identity: claim=%+v calls=%v", claim, f.calls)
	}
	for _, key := range []string{"last_touched_at", "expires_at", "idle_timeout_secs", "idle_timeout"} {
		if claim.Labels[key] != labels[key] {
			t.Fatalf("replay reverted heartbeat %s: got=%s want=%s", key, claim.Labels[key], labels[key])
		}
	}
}

func TestTenkiFixedForeignClaimsAndUnownedInventory(t *testing.T) {
	for _, mode := range []string{"foreign provider", "ordinary claim", "foreign session", "duplicate inventory", "missing local attempt", "erased attempt"} {
		t.Run(mode, func(t *testing.T) {
			b, f, req := newFixedTenkiTest(t)
			switch mode {
			case "foreign provider", "ordinary claim":
				cfg := b.configForRun()
				if mode == "foreign provider" {
					cfg.Provider = "ssh"
				}
				if err := core.ClaimLeaseTargetForRepoConfig(req.RequestedLeaseID, req.RequestedSlug, cfg, Server{Provider: cfg.Provider, CloudID: "foreign"}, SSHTarget{}, req.Repo.Root, time.Minute, false); err != nil {
					t.Fatal(err)
				}
			case "foreign session":
				f.sessions["foreign"] = tenkiSession{ID: "foreign", Name: leaseProviderName(req.RequestedLeaseID, req.RequestedSlug), State: "RUNNING", Metadata: map[string]string{tenkiMetadataProvider: tenkiProvider, tenkiMetadataLease: req.RequestedLeaseID, tenkiMetadataSlug: req.RequestedSlug}}
			case "duplicate inventory", "missing local attempt":
				requireFixedAcquire(t, b, req)
				if mode == "duplicate inventory" {
					duplicate := cloneTenkiSession(f.sessions["session-1"])
					duplicate.ID = "duplicate"
					f.sessions[duplicate.ID] = duplicate
				} else {
					core.RemoveLeaseClaim(req.RequestedLeaseID)
				}
			case "erased attempt":
				f.createError = errors.New("lost response")
				if _, err := b.Acquire(context.Background(), req); err == nil {
					t.Fatal("expected lost response")
				}
				if err := core.WithDurableLeaseClaimLock(req.RequestedLeaseID, func(claim *LeaseClaim, _ bool, persist func() error) error {
					claim.FixedCreateIntent.Attempt = nil
					return persist()
				}); err != nil {
					t.Fatal(err)
				}
				f.sessions = map[string]tenkiSession{}
				f.createError = nil
			}
			creates, ssh := f.calls["create"], f.calls["ssh-command"]
			req.Reclaim = true
			_, err := b.Acquire(context.Background(), req)
			requireFixedConflict(t, err)
			if f.calls["create"] != creates || f.calls["terminate"] != 0 || f.calls["ssh-command"] != ssh {
				t.Fatalf("unsafe calls: %v", f.calls)
			}
		})
	}
}

func TestTenkiFixedCreateFailuresRetainAttempt(t *testing.T) {
	for _, tc := range []struct {
		name         string
		output       string
		commandError bool
		knownID      bool
	}{
		{"malformed JSON", "{", false, false},
		{"missing ID", "{}", false, false},
		{"partial JSON with ID", `{"id":"session-1","bad":`, false, false},
		{"error with ID", `{"id":"session-1"}`, true, true},
		{"wrong returned ID", `{"id":"other"}`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, f, req := newFixedTenkiTest(t)
			req.Keep = false
			f.createOutput = &tc.output
			if tc.commandError {
				f.createError = errors.New("output failed")
			}
			if _, err := b.Acquire(context.Background(), req); err == nil {
				t.Fatal("expected create failure")
			}
			claim := requireFixedClaim(t)
			attempt, err := b.fixedAttempt(claim)
			if err != nil || attempt == nil || (attempt.SessionID != "") != tc.knownID || claim.CloudID != "" {
				t.Fatalf("attempt=%+v claim=%+v err=%v", attempt, claim, err)
			}
			f.createOutput, f.createError = nil, nil
			invisible := []tenkiSession{}
			f.listOverride = &invisible
			f.hook = func(_ context.Context, r LocalCommandRequest) (LocalCommandResult, error, bool) {
				if r.Args[1] == "get" {
					return LocalCommandResult{ExitCode: 1}, errors.New("API key not found"), true
				}
				return LocalCommandResult{}, nil, false
			}
			for i := 0; i < 2; i++ {
				fresh := *b
				if _, err := fresh.Acquire(context.Background(), req); err == nil {
					t.Fatal("uncertain create replay succeeded")
				}
			}
			if f.calls["create"] != 1 || f.calls["terminate"] != 0 || f.calls["ssh-command"] != 0 {
				t.Fatalf("unsafe calls=%v", f.calls)
			}
			if !reflect.DeepEqual(claim, requireFixedClaim(t)) {
				t.Fatal("failed replay changed durable evidence")
			}
		})
	}
}

func TestTenkiFixedGetFailureAfterCreateRetainsReturnedID(t *testing.T) {
	b, f, req := newFixedTenkiTest(t)
	f.hook = func(_ context.Context, r LocalCommandRequest) (LocalCommandResult, error, bool) {
		if r.Args[1] == "get" {
			return LocalCommandResult{ExitCode: 1}, errors.New("detail unavailable"), true
		}
		return LocalCommandResult{}, nil, false
	}
	if _, err := b.Acquire(context.Background(), req); err == nil {
		t.Fatal("expected detail error")
	}
	claim := requireFixedClaim(t)
	attempt, err := b.fixedAttempt(claim)
	if err != nil || attempt.SessionID != "session-1" || claim.CloudID != "" {
		t.Fatalf("claim=%+v attempt=%+v err=%v", claim, attempt, err)
	}
	invisible := []tenkiSession{}
	f.listOverride = &invisible
	if _, err := b.Acquire(context.Background(), req); err == nil {
		t.Fatal("expected detail error on replay")
	}
	f.hook = nil
	lease := requireFixedAcquire(t, b, req)
	if lease.Server.CloudID != "session-1" || f.calls["create"] != 1 {
		t.Fatalf("lease=%+v calls=%v", lease, f.calls)
	}
}

func TestTenkiFixedDetailAttestationBeforeSSH(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*tenkiSession)
	}{
		{"ID", func(s *tenkiSession) { s.ID = "foreign" }},
		{"empty ID", func(s *tenkiSession) { s.ID = "" }},
		{"name", func(s *tenkiSession) { s.Name = "foreign" }},
		{"provider", func(s *tenkiSession) { s.Metadata[tenkiMetadataProvider] = "foreign" }},
		{"lease", func(s *tenkiSession) { s.Metadata[tenkiMetadataLease] = "cbx_000000000001" }},
		{"slug", func(s *tenkiSession) { s.Metadata[tenkiMetadataSlug] = "foreign" }},
		{"token", func(s *tenkiSession) { s.Metadata[tenkiMetadataAttempt] = "foreign" }},
		{"fingerprint", func(s *tenkiSession) { s.Metadata[tenkiMetadataIntent] = "foreign" }},
		{"image", func(s *tenkiSession) { s.SourceImageRef = "foreign" }},
		{"CPU", func(s *tenkiSession) { s.CPUCores++ }},
		{"memory", func(s *tenkiSession) { s.MemoryMB++ }},
		{"disk", func(s *tenkiSession) { s.DiskSizeGB++ }},
		{"keep", func(s *tenkiSession) { s.Sticky = false }},
		{"terminal", func(s *tenkiSession) { s.State = "TERMINATED" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, f, req := newFixedTenkiTest(t)
			f.hook = func(_ context.Context, r LocalCommandRequest) (LocalCommandResult, error, bool) {
				if r.Args[1] != "get" {
					return LocalCommandResult{}, nil, false
				}
				s := cloneTenkiSession(f.sessions["session-1"])
				tc.change(&s)
				data, _ := json.Marshal(s)
				return LocalCommandResult{Stdout: string(data)}, nil, true
			}
			_, err := b.Acquire(context.Background(), req)
			requireFixedConflict(t, err)
			if f.calls["create"] != 1 || f.calls["ssh-command"] != 0 || f.calls["resume"] != 0 || f.calls["terminate"] != 0 || requireFixedClaim(t).CloudID != "" {
				t.Fatalf("unattested session used: calls=%v", f.calls)
			}
		})
	}
}

func TestTenkiFixedReadinessReattestsBeforeResumeAndAfterSSH(t *testing.T) {
	for _, state := range []string{"PAUSING", "RESUMING", "PAUSED", "SSH ready"} {
		t.Run(state, func(t *testing.T) {
			b, f, req := newFixedTenkiTest(t)
			req.Keep = false
			requireFixedAcquire(t, b, req)
			sshCalls := f.calls["ssh-command"]
			getCalls := f.calls["get"]
			s := f.sessions["session-1"]
			if state == "SSH ready" {
				waitForSSHReadyFunc = func(context.Context, *SSHTarget, io.Writer, string, time.Duration) error {
					s.Metadata[tenkiMetadataAttempt] = "foreign"
					f.sessions[s.ID] = s
					return nil
				}
			} else {
				s.State = state
				f.sessions[s.ID] = s
				f.hook = func(_ context.Context, r LocalCommandRequest) (LocalCommandResult, error, bool) {
					if r.Args[1] != "get" || f.calls["get"] <= getCalls+1 {
						return LocalCommandResult{}, nil, false
					}
					bad := cloneTenkiSession(s)
					bad.ID, bad.State = "wrong-session", "PAUSED"
					data, _ := json.Marshal(bad)
					return LocalCommandResult{Stdout: string(data)}, nil, true
				}
			}
			_, err := b.Acquire(context.Background(), req)
			requireFixedConflict(t, err)
			if f.calls["create"] != 1 || f.calls["terminate"] != 0 || requireFixedClaim(t).CloudID != "session-1" {
				t.Fatalf("unsafe readiness cleanup: %v", f.calls)
			}
			wantResume := 0
			if state == "PAUSED" {
				wantResume = 1
			}
			if state == "SSH ready" {
				sshCalls++
			}
			if f.calls["resume"] != wantResume || f.calls["ssh-command"] != sshCalls {
				t.Fatalf("unattested poll used for resume/SSH: %v", f.calls)
			}
		})
	}
}

func TestTenkiFixedCancellationRetainsEvidence(t *testing.T) {
	for _, phase := range []string{"before create", "during create", "readiness"} {
		t.Run(phase, func(t *testing.T) {
			b, f, req := newFixedTenkiTest(t)
			req.Keep = false
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch phase {
			case "before create":
				cancel()
			case "during create":
				f.hook = func(_ context.Context, r LocalCommandRequest) (LocalCommandResult, error, bool) {
					if r.Args[1] == "create" {
						cancel()
					}
					return LocalCommandResult{}, nil, false
				}
			case "readiness":
				waitForSSHReadyFunc = func(ctx context.Context, _ *SSHTarget, _ io.Writer, _ string, _ time.Duration) error {
					cancel()
					return ctx.Err()
				}
			}
			_, err := b.Acquire(ctx, req)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err=%v, want cancellation", err)
			}
			if f.calls["terminate"] != 0 {
				t.Fatal("cancellation deleted fixed session")
			}
			claim, exists, err := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
			if phase == "before create" {
				if err != nil || exists || len(f.calls) != 0 {
					t.Fatalf("pre-cancel mutated state: claim=%+v err=%v calls=%v", claim, err, f.calls)
				}
				return
			}
			if err != nil || !exists || claim.FixedCreateIntent == nil || len(claim.FixedCreateIntent.Attempt) == 0 {
				t.Fatalf("lost attempt: %+v err=%v", claim, err)
			}
			if phase == "during create" && claim.CloudID != "" {
				t.Fatal("bound session before detail attestation")
			}
			if phase == "readiness" && claim.CloudID != "session-1" {
				t.Fatal("readiness preceded immutable binding")
			}
			f.hook = nil
			waitForSSHReadyFunc = func(context.Context, *SSHTarget, io.Writer, string, time.Duration) error { return nil }
			requireFixedAcquire(t, b, req)
			if f.calls["create"] != 1 {
				t.Fatalf("resubmitted after cancellation: %v", f.calls)
			}
		})
	}
}

func TestTenkiFixedResolvePreservesIntentAndReadOnlyInspection(t *testing.T) {
	for _, state := range []string{"RUNNING", "PAUSED", "TERMINATED"} {
		t.Run(state, func(t *testing.T) {
			b, f, req := newFixedTenkiTest(t)
			lease := requireFixedAcquire(t, b, req)
			s := f.sessions["session-1"]
			s.State = state
			f.sessions[s.ID] = s
			before := requireFixedClaim(t)
			sshCalls := f.calls["ssh-command"]
			for _, id := range []string{lease.LeaseID, req.RequestedSlug, lease.Server.CloudID} {
				got, err := b.Resolve(context.Background(), ResolveRequest{ID: id, StatusOnly: true, NoLocalStateMutations: true, IncludeDiagnostics: true})
				if err != nil {
					t.Fatal(err)
				}
				if got.LeaseID != req.RequestedLeaseID || got.Server.CloudID != "session-1" {
					t.Fatalf("wrong inspection identity: %+v", got)
				}
				if state == "RUNNING" {
					if got.SSH.Host == "" || got.SSH.NetworkKind != networkPublic {
						t.Fatalf("missing inspection SSH: %+v", got)
					}
					sshCalls++
				} else if got.SSH.Host != "" {
					t.Fatal("nonready inspection obtained SSH")
				}
				if !reflect.DeepEqual(before, requireFixedClaim(t)) {
					t.Fatal("read-only inspect changed fixed evidence")
				}
			}
			if f.calls["ssh-command"] != sshCalls || f.calls["resume"] != 0 {
				t.Fatalf("inspection side effect: %v", f.calls)
			}
			if state == "TERMINATED" {
				_, err := b.Resolve(context.Background(), ResolveRequest{ID: lease.LeaseID, Repo: req.Repo, Reclaim: true})
				requireFixedConflict(t, err)
				return
			}
			got, err := b.Resolve(context.Background(), ResolveRequest{ID: lease.LeaseID, Repo: req.Repo})
			if err != nil {
				t.Fatal(err)
			}
			after := requireFixedClaim(t)
			if got.Server.CloudID != "session-1" || !reflect.DeepEqual(before.FixedCreateIntent, after.FixedCreateIntent) || after.CloudImmutableID != before.CloudImmutableID {
				t.Fatal("command reuse overwrote fixed identity")
			}
		})
	}
}

func TestTenkiFixedResolveUnboundAttemptAndForeignMetadata(t *testing.T) {
	b, f, req := newFixedTenkiTest(t)
	f.createError = errors.New("lost response")
	if _, err := b.Acquire(context.Background(), req); err == nil {
		t.Fatal("expected error")
	}
	before := requireFixedClaim(t)
	status, err := b.Resolve(context.Background(), ResolveRequest{ID: req.RequestedLeaseID, StatusOnly: true, NoLocalStateMutations: true, IncludeDiagnostics: true})
	if err != nil || status.Server.CloudID != "session-1" || status.SSH.Host != "" || f.calls["ssh-command"] != 0 {
		t.Fatalf("status=%+v err=%v calls=%v", status, err, f.calls)
	}
	if !reflect.DeepEqual(before, requireFixedClaim(t)) {
		t.Fatal("inspection bound uncertain claim")
	}
	lease := requireFixedReleaseTarget(t, b)
	if err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if f.calls["terminate"] != 1 {
		t.Fatalf("release calls=%v", f.calls)
	}
	core.RemoveLeaseClaim(req.RequestedLeaseID)
	for _, id := range []string{req.RequestedLeaseID, "session-1"} {
		_, err := b.Resolve(context.Background(), ResolveRequest{ID: id, Repo: req.Repo, Reclaim: true})
		requireFixedConflict(t, err)
	}
}

func TestTenkiFixedReleaseRetainsSingleUseReceipt(t *testing.T) {
	b, f, req := newFixedTenkiTest(t)
	requireFixedAcquire(t, b, req)
	lease := requireFixedReleaseTarget(t, b)
	previous := requireFixedClaim(t)
	outcome, err := b.ReleaseLeaseWithOutcome(context.Background(), ReleaseLeaseRequest{Lease: lease})
	if err != nil || !outcome.Terminal {
		t.Fatalf("outcome=%+v err=%v", outcome, err)
	}
	terminal := requireFixedClaim(t)
	if terminal.FixedCreateIntent.State != "released" || terminal.CloudID != "session-1" || terminal.SSHHost != "" || len(terminal.FixedCreateIntent.Attempt) != 0 {
		t.Fatalf("invalid terminal receipt: %+v", terminal)
	}
	if retained, err := b.RetainLeaseClaimAfterReleaseWithClaim(lease, previous); err != nil || !retained {
		t.Fatalf("receipt not retained: retained=%t err=%v", retained, err)
	}
	calls := maps.Clone(f.calls)
	f.hook = func(context.Context, LocalCommandRequest) (LocalCommandResult, error, bool) {
		return LocalCommandResult{}, errors.New("terminal replay must not use Tenki"), true
	}
	for i := 0; i < 2; i++ {
		lease = requireFixedReleaseTarget(t, b)
		outcome, err := b.ReleaseLeaseWithOutcome(context.Background(), ReleaseLeaseRequest{Lease: lease})
		if err != nil || !outcome.Terminal {
			t.Fatalf("repeated stop outcome=%+v err=%v", outcome, err)
		}
		if retained, err := b.RetainLeaseClaimAfterReleaseWithClaim(lease, terminal); err != nil || !retained {
			t.Fatalf("retention=%t err=%v", retained, err)
		}
	}
	status, err := b.Resolve(context.Background(), ResolveRequest{ID: req.RequestedLeaseID, StatusOnly: true, IncludeDiagnostics: true, NoLocalStateMutations: true})
	if err != nil || status.Server.Status != "terminated" || status.SSH.Host != "" {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	_, err = b.Acquire(context.Background(), req)
	requireFixedConflict(t, err)
	_, err = b.Resolve(context.Background(), ResolveRequest{ID: req.RequestedLeaseID, Reclaim: true})
	requireFixedConflict(t, err)
	if !maps.Equal(calls, f.calls) || !reflect.DeepEqual(terminal, requireFixedClaim(t)) {
		t.Fatalf("terminal replay mutated state: calls=%v want=%v", f.calls, calls)
	}
}

func TestTenkiFixedReleaseUncertaintyAndWrongAcknowledgement(t *testing.T) {
	for _, mode := range []string{"terminate error", "get error", "running", "wrong ID", "missing ID", "wrong token", "wrong project", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			b, f, req := newFixedTenkiTest(t)
			requireFixedAcquire(t, b, req)
			lease := requireFixedReleaseTarget(t, b)
			before := requireFixedClaim(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f.hook = func(_ context.Context, r LocalCommandRequest) (LocalCommandResult, error, bool) {
				if r.Args[1] == "terminate" && mode == "terminate error" {
					return LocalCommandResult{ExitCode: 1}, errors.New("workspace not found"), true
				}
				if r.Args[1] != "get" || f.calls["terminate"] == 0 {
					return LocalCommandResult{}, nil, false
				}
				if mode == "get error" {
					return LocalCommandResult{ExitCode: 1}, errors.New("API key not found"), true
				}
				s := cloneTenkiSession(f.sessions["session-1"])
				switch mode {
				case "running":
					s.State = "RUNNING"
				case "wrong ID":
					s.ID = "foreign"
				case "missing ID":
					s.ID = ""
				case "wrong token":
					s.Metadata[tenkiMetadataAttempt] = "foreign"
				case "wrong project":
					s.ProjectID = "foreign"
				case "canceled":
					cancel()
				}
				data, _ := json.Marshal(s)
				return LocalCommandResult{Stdout: string(data)}, nil, true
			}
			outcome, err := b.ReleaseLeaseWithOutcome(ctx, ReleaseLeaseRequest{Lease: lease})
			if err == nil || outcome.Terminal {
				t.Fatalf("uncertain release finalized: outcome=%+v err=%v", outcome, err)
			}
			if !reflect.DeepEqual(before, requireFixedClaim(t)) {
				t.Fatal("uncertain release erased or changed bound evidence")
			}
			f.hook = nil
			lease = requireFixedReleaseTarget(t, b)
			if err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: lease}); err != nil {
				t.Fatal(err)
			}
			want := 1
			if mode == "terminate error" {
				want = 2
			}
			if f.calls["terminate"] != want || requireFixedClaim(t).FixedCreateIntent.State != "released" {
				t.Fatalf("unexpected retry: calls=%v", f.calls)
			}
		})
	}
}

func TestTenkiFixedReleaseAndResolveRejectChangedIdentity(t *testing.T) {
	for _, mode := range []string{"claim revision", "target ID", "target immutable ID", "target slug", "scope", "route", "live token", "live project", "bound missing", "list error"} {
		t.Run(mode, func(t *testing.T) {
			b, f, req := newFixedTenkiTest(t)
			requireFixedAcquire(t, b, req)
			lease := requireFixedReleaseTarget(t, b)
			s := cloneTenkiSession(f.sessions["session-1"])
			switch mode {
			case "claim revision":
				claim := requireFixedClaim(t)
				labels := maps.Clone(claim.Labels)
				labels["state"] = "busy"
				if _, err := core.UpdateLeaseClaimLabelsIfUnchanged(req.RequestedLeaseID, claim, labels); err != nil {
					t.Fatal(err)
				}
			case "target ID":
				lease.Server.CloudID = "foreign"
			case "target immutable ID":
				lease.Server.ImmutableID = "foreign"
			case "target slug":
				lease.Server.Labels = maps.Clone(lease.Server.Labels)
				lease.Server.Labels["slug"] = "foreign"
			case "scope":
				b.cfg.Tenki.Endpoint = "https://other.example"
			case "route":
				b.cfg.Tenki.Gateway = "wss://other.example"
			case "live token":
				s.Metadata[tenkiMetadataAttempt] = "foreign"
				f.sessions[s.ID] = s
			case "live project":
				s.ProjectID = "foreign"
				f.sessions[s.ID] = s
			case "bound missing":
				f.sessions = map[string]tenkiSession{}
			case "list error":
				f.hook = func(context.Context, LocalCommandRequest) (LocalCommandResult, error, bool) {
					return LocalCommandResult{ExitCode: 1}, errors.New("inventory unavailable"), true
				}
			}
			before := requireFixedClaim(t)
			if err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: lease}); err == nil {
				t.Fatal("unsafe release succeeded")
			}
			if !strings.HasPrefix(mode, "target") && mode != "claim revision" {
				if _, err := b.Resolve(context.Background(), ResolveRequest{ID: req.RequestedLeaseID, Repo: req.Repo}); err == nil {
					t.Fatal("unsafe reuse succeeded")
				}
				if _, err := b.Acquire(context.Background(), req); err == nil {
					t.Fatal("unsafe acquisition succeeded")
				}
			}
			if f.calls["terminate"] != 0 || f.calls["create"] != 1 || f.calls["ssh-command"] != 1 || !reflect.DeepEqual(before, requireFixedClaim(t)) {
				t.Fatalf("changed identity used: %v", f.calls)
			}
		})
	}
}

func TestTenkiFixedCallbackRunsOutsideLockAndRetainsFailure(t *testing.T) {
	b, f, req := newFixedTenkiTest(t)
	req.Keep = false
	callbackErr := errors.New("callback failed")
	req.OnAcquired = func(lease LeaseTarget) error {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := core.WithDurableLeaseClaimLockContext(ctx, lease.LeaseID, func(claim *LeaseClaim, exists bool, _ func() error) error {
			if !exists || claim.CloudID != lease.Server.CloudID || claim.FixedCreateIntent.State != "acquired" {
				return fmt.Errorf("callback saw uncommitted identity")
			}
			return nil
		}); err != nil {
			return err
		}
		return callbackErr
	}
	_, err := b.Acquire(context.Background(), req)
	if !errors.Is(err, callbackErr) {
		t.Fatalf("callback failed or deadlocked: %v", err)
	}
	if requireFixedClaim(t).CloudID != "session-1" || f.calls["terminate"] != 0 {
		t.Fatal("callback failure deleted evidence")
	}
	req.OnAcquired = nil
	requireFixedAcquire(t, b, req)
	if f.calls["create"] != 1 {
		t.Fatal("callback retry submitted create again")
	}
}

func TestTenkiFixedAttemptIsDurableBeforeSubmissionAndSSH(t *testing.T) {
	b, f, req := newFixedTenkiTest(t)
	f.hook = func(_ context.Context, r LocalCommandRequest) (LocalCommandResult, error, bool) {
		if r.Args[1] != "create" {
			return LocalCommandResult{}, nil, false
		}
		claim := requireFixedClaim(t)
		attempt, err := b.fixedAttempt(claim)
		if err != nil || attempt == nil || attempt.SessionID != "" || claim.CloudID != "" {
			t.Fatalf("create preceded durable attempt: claim=%+v attempt=%+v err=%v", claim, attempt, err)
		}
		args := strings.Join(r.Args, " ")
		for _, value := range []string{"--metadata " + tenkiMetadataAttempt + "=" + attempt.Token, "--metadata " + tenkiMetadataIntent + "=" + claim.FixedCreateIntent.Fingerprint, "--name " + attempt.Name} {
			if !strings.Contains(args, value) {
				t.Fatalf("submitted request differs from durable attempt: missing %q", value)
			}
		}
		return LocalCommandResult{}, nil, false
	}
	waitForSSHReadyFunc = func(context.Context, *SSHTarget, io.Writer, string, time.Duration) error {
		claim := requireFixedClaim(t)
		if claim.CloudID != "session-1" || claim.CloudImmutableID != "session-1" {
			t.Fatalf("SSH preceded immutable binding: %+v", claim)
		}
		return nil
	}
	requireFixedAcquire(t, b, req)
}

func TestTenkiFixedReplayIgnoresClockAndGeneratedProviderKey(t *testing.T) {
	b, f, req := newFixedTenkiTest(t)
	requireFixedAcquire(t, b, req)
	previous := tenkiNow
	now := time.Now().Add(time.Minute)
	tenkiNow = func() time.Time { return now }
	t.Cleanup(func() { tenkiNow = previous })
	b.cfg.ProviderKey = "renewed-provider-key"
	requireFixedAcquire(t, b, req)
	if f.calls["create"] != 1 {
		t.Fatalf("clock changed fixed identity: %v", f.calls)
	}
	tenkiNow = func() time.Time { return now.Add(2 * time.Hour) }
	calls := maps.Clone(f.calls)
	_, err := b.Acquire(context.Background(), req)
	requireFixedConflict(t, err)
	if !maps.Equal(calls, f.calls) {
		t.Fatal("expired intent called provider")
	}
}

func TestTenkiFixedTouchRejectsStaleAndTerminalClaims(t *testing.T) {
	b, f, req := newFixedTenkiTest(t)
	first := requireFixedAcquire(t, b, req)
	idle := 15 * time.Minute
	touch := TouchRequest{Lease: first, State: "ready", IdleTimeoutOverride: &idle}
	if _, err := b.Touch(context.Background(), touch); err != nil {
		t.Fatal(err)
	}
	before := requireFixedClaim(t)
	if _, err := b.Touch(context.Background(), touch); err == nil {
		t.Fatal("stale heartbeat succeeded")
	}
	if !reflect.DeepEqual(before, requireFixedClaim(t)) {
		t.Fatal("stale heartbeat changed evidence")
	}
	lease := requireFixedReleaseTarget(t, b)
	if err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	terminal := requireFixedReleaseTarget(t, b)
	before = requireFixedClaim(t)
	if _, err := b.Touch(context.Background(), TouchRequest{Lease: terminal, State: "ready", IdleTimeoutOverride: &idle}); err == nil {
		t.Fatal("heartbeat revived a terminal claim")
	}
	if !reflect.DeepEqual(before, requireFixedClaim(t)) || f.calls["create"] != 1 || f.calls["resume"] != 0 || f.calls["ssh-command"] != 1 {
		t.Fatalf("heartbeat side effects: %v", f.calls)
	}
}

func TestTenkiFixedTerminalReceiptRejectsTampering(t *testing.T) {
	b, f, req := newFixedTenkiTest(t)
	requireFixedAcquire(t, b, req)
	lease := requireFixedReleaseTarget(t, b)
	before := requireFixedClaim(t)
	if err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if err := core.WithDurableLeaseClaimLock(req.RequestedLeaseID, func(claim *LeaseClaim, _ bool, persist func() error) error {
		claim.CloudID, claim.CloudImmutableID, claim.Labels["tenki_session_id"] = "foreign", "foreign", "foreign"
		return persist()
	}); err != nil {
		t.Fatal(err)
	}
	calls := maps.Clone(f.calls)
	retained, err := b.RetainLeaseClaimAfterReleaseWithClaim(lease, before)
	if retained {
		t.Fatal("changed terminal receipt accepted")
	}
	requireFixedConflict(t, err)
	if !maps.Equal(calls, f.calls) {
		t.Fatal("terminal verification used provider")
	}
}

func TestTenkiFixedConcurrentSameIDCallers(t *testing.T) {
	b, f, req := newFixedTenkiTest(t)
	var wg sync.WaitGroup
	results := make(chan LeaseTarget, 4)
	errorsCh := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fresh := *b
			lease, err := fresh.Acquire(context.Background(), req)
			results <- lease
			errorsCh <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	for lease := range results {
		if lease.LeaseID != req.RequestedLeaseID || lease.Server.CloudID != "session-1" {
			t.Fatalf("different concurrent identity: %+v", lease)
		}
	}
	if f.calls["create"] != 1 || f.calls["terminate"] != 0 {
		t.Fatalf("concurrent calls=%v", f.calls)
	}
}
