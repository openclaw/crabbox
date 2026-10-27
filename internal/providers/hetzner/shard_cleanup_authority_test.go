package hetzner

import (
	"bytes"
	"context"
	"io"

	"path/filepath"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

// shardCleanupHetznerBackend is the real Hetzner lease backend with remote SSH
// hygiene disabled so shard-cleanup authority tests observe ReleaseLease without
// waiting on unreachable Tailscale/egress timeouts.
type shardCleanupHetznerBackend struct {
	*hetznerLeaseBackend
	beforeRelease func()
	cleanups      int
}

func (b *shardCleanupHetznerBackend) ReleaseLease(ctx context.Context, req core.ReleaseLeaseRequest) error {
	if b.beforeRelease != nil {
		b.beforeRelease()
	}
	req.GuardedRemoteCleanup = func(context.Context, core.LeaseTarget) { b.cleanups++ }
	return b.hetznerLeaseBackend.ReleaseLease(ctx, req)
}

func (b *shardCleanupHetznerBackend) ReleaseLeaseConnectionCleanupSafe() bool { return false }

// TestShardCleanupHetznerReleaseAuthority drives the production shard cleanup
// release fence (CheckpointForkReleaseBindings → RefreshAuthorizedForkReleaseClaim
// → ReleaseLease) through the real Hetzner release owner. Only newHetznerClient
// is faked at the API boundary so requireExactHetznerClaim and
// deleteClaimedHetznerServer run for real.
func TestShardCleanupHetznerReleaseAuthority(t *testing.T) {
	cases := []struct {
		name string
		// afterCarry mutates the claim store after the authorized generation is
		// carried, as a concurrent reclaim would between run --keep and shard release.
		afterCarry  func(t *testing.T, leaseID string, authorized core.LeaseClaim)
		wantDeletes int
		wantRefuse  bool
		// wantReleaseErr is set when refresh allows ReleaseLease but the real
		// Hetzner claim check refuses before any destructive API call.
		wantReleaseErr string
	}{
		{
			name:        "authorized update releases",
			wantDeletes: 1,
		},
		{
			name: "same-second reclaim refuses",
			afterCarry: func(t *testing.T, leaseID string, authorized core.LeaseClaim) {
				t.Helper()
				next := authorized
				next.Labels = cloneLabels(authorized.Labels)
				if next.Labels == nil {
					next.Labels = map[string]string{}
				}
				next.Labels["run"] = "reclaimed"
				updated, err := core.ReplaceLeaseClaimIfUnchangedDurableReturning(leaseID, authorized, next)
				if err != nil {
					t.Fatal(err)
				}
				if updated.ClaimedAt != authorized.ClaimedAt {
					t.Fatalf("ClaimedAt=%q, want same-second %q", updated.ClaimedAt, authorized.ClaimedAt)
				}
			},
			wantRefuse: true,
		},
		{
			name: "away and back (ABA) refuses",
			afterCarry: func(t *testing.T, leaseID string, authorized core.LeaseClaim) {
				t.Helper()
				foreign := authorized
				foreign.RepoRoot = filepath.Join(t.TempDir(), "hijacked")
				if _, err := core.ReplaceLeaseClaimIfUnchangedDurableReturning(leaseID, authorized, foreign); err != nil {
					t.Fatal(err)
				}
				cur, err := core.ReadLeaseClaim(leaseID)
				if err != nil {
					t.Fatal(err)
				}
				back := authorized
				back.ClaimedAt = time.Now().UTC().Add(2 * time.Second).Format(time.RFC3339)
				if _, err := core.ReplaceLeaseClaimIfUnchangedDurableReturning(leaseID, cur, back); err != nil {
					t.Fatal(err)
				}
			},
			wantRefuse: true,
		},
		{
			name: "reassigned to another owner refuses",
			afterCarry: func(t *testing.T, leaseID string, authorized core.LeaseClaim) {
				t.Helper()
				next := authorized
				next.RepoRoot = filepath.Join(t.TempDir(), "hijacked")
				next.ClaimedAt = time.Now().UTC().Add(time.Second).Format(time.RFC3339)
				if _, err := core.ReplaceLeaseClaimIfUnchangedDurableReturning(leaseID, authorized, next); err != nil {
					t.Fatal(err)
				}
			},
			wantRefuse: true,
		},
		{
			name: "revoked claim refuses",
			afterCarry: func(t *testing.T, leaseID string, authorized core.LeaseClaim) {
				t.Helper()
				revoked := authorized
				revoked.CloudID = ""
				revoked.ClaimedAt = time.Now().UTC().Add(time.Second).Format(time.RFC3339)
				if _, err := core.ReplaceLeaseClaimIfUnchangedDurableReturning(leaseID, authorized, revoked); err != nil {
					t.Fatal(err)
				}
			},
			wantRefuse: true,
		},
		{
			// Real Hetzner ReleaseLease refuses when the claim file is gone
			// (requireExactHetznerClaim); it does not treat a carried snapshot as
			// authorization to delete (unlike the old AWS-style fake that called
			// CleanupLeaseClaimIfUnchangedAfter with expectedExists=false).
			name: "missing claim file refuses",
			afterCarry: func(t *testing.T, leaseID string, _ core.LeaseClaim) {
				t.Helper()
				core.RemoveLeaseClaim(leaseID)
				if _, exists, err := core.ReadLeaseClaimWithPresence(leaseID); err != nil || exists {
					t.Fatalf("claim still present exists=%v err=%v", exists, err)
				}
			},
			wantReleaseErr: "no exact local claim",
		},
	}

	for _, phase := range []string{"before preflight", "after preflight", "before deletion"} {
		for _, tc := range cases {
			t.Run(phase+"/"+tc.name, func(t *testing.T) {
				installHetznerClaimState(t)
				client := &fakeHetznerClient{}
				installHetznerTestHooks(t, client)

				const leaseID = "cbx_abcdef123456"
				server := acquiredHetznerTestServer(42, leaseID)
				client.createServer = server
				client.servers = map[int64]core.Server{42: server}
				server.Labels["provider_key"] = core.ProviderKeyForLease(leaseID)
				seedHetznerClaim(t, server)
				authorized, err := core.ReadLeaseClaim(leaseID)
				if err != nil {
					t.Fatal(err)
				}
				core.SetServerLeaseClaimSnapshot(&server, authorized, true)

				backend := &shardCleanupHetznerBackend{
					hetznerLeaseBackend: NewHetznerLeaseBackend(core.ProviderSpec{Name: providerName}, core.Config{Provider: providerName}, core.Runtime{Stderr: io.Discard}).(*hetznerLeaseBackend),
				}
				acquired, err := backend.Acquire(context.Background(), core.AcquireRequest{RequestedSlug: "test"})
				if err != nil {
					t.Fatal(err)
				}
				if !backend.matchesAcquiredLease(acquired.Server, leaseID) {
					t.Fatal("missing acquisition state")
				}
				target := core.SSHTarget{User: "crabbox", Host: "127.0.0.1", Port: "22", TargetOS: core.TargetLinux}
				cfg := core.Config{Provider: providerName}
				var stderr bytes.Buffer
				app := core.App{Stdout: io.Discard, Stderr: &stderr}
				release, carry := app.CheckpointForkReleaseBindings(&server, &target, leaseID, nil, backend, &cfg)
				carry(authorized)
				if tc.afterCarry != nil {
					mutate := func() { tc.afterCarry(t, leaseID, authorized) }
					switch phase {
					case "before preflight":
						mutate()
					case "after preflight":
						backend.beforeRelease = mutate
					case "before deletion":
						newHetznerClient = func() (hetznerClient, error) { mutate(); return client, nil }
					}
				}
				release(context.Background())

				if got := len(client.deletedServers); got != tc.wantDeletes {
					t.Fatalf("provider deletes=%d, want %d stderr=%q", got, tc.wantDeletes, stderr.String())
				}
				if backend.cleanups != tc.wantDeletes {
					t.Fatalf("remote cleanups=%d want=%d", backend.cleanups, tc.wantDeletes)
				}
				if tc.wantRefuse && !strings.Contains(stderr.String(), "refus") && !strings.Contains(stderr.String(), "claim changed") {
					t.Fatalf("stderr missing refusal:\n%s", stderr.String())
				}
				if tc.wantReleaseErr != "" && !strings.Contains(stderr.String(), "release failed") {
					t.Fatalf("stderr missing %q:\n%s", tc.wantReleaseErr, stderr.String())
				}
				if tc.wantDeletes == 0 && len(client.deletedKeys) != 0 {
					t.Fatalf("SSH key deletes=%v before claim check passed", client.deletedKeys)
				}
			})
		}
	}
}

func cloneLabels(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
