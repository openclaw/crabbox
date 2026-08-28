package ssh

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestStaticSSHArchitecturePrepareDefersOwnershipPublication(t *testing.T) {
	for _, cached := range []bool{false, true} {
		for _, reclaim := range []bool{false, true} {
			t.Run(map[bool]string{false: "claimed", true: "cached"}[cached]+map[bool]string{false: "/other-repo", true: "/reclaim"}[reclaim], func(t *testing.T) {
				b, _, repoA := staticArchitectureFixture(t, "linux", "normal", "")
				lease, err := b.Acquire(context.Background(), AcquireRequest{Repo: core.Repo{Root: repoA}})
				if err != nil {
					t.Fatal(err)
				}
				initial, _, _ := core.ReadLeaseClaimWithPresence(lease.LeaseID)
				if !cached {
					b = NewStaticSSHLeaseBackend(Provider{}.Spec(), b.Cfg, b.RT).(*staticLeaseBackend)
				}
				cacheBefore := b.acquired
				repoB := t.TempDir()
				runArchitectureProbe = func(context.Context, SSHTarget, string, int) (string, error) { return "v1|amd64|-|-|-", nil }
				prepared, err := b.Resolve(context.Background(), ResolveRequest{ID: lease.LeaseID, Repo: core.Repo{Root: repoB}, Reclaim: reclaim, Prepare: true})
				if err != nil {
					t.Fatal(err)
				}
				after, _, _ := core.ReadLeaseClaimWithPresence(lease.LeaseID)
				if !reflect.DeepEqual(after, initial) {
					t.Error("Prepare mutated the claim before repository authorization")
				}
				if !reflect.DeepEqual(b.acquired, cacheBefore) {
					t.Error("Prepare replaced cached evidence before repository authorization")
				}
				expected, exists, set := core.ServerLeaseClaimSnapshot(prepared.Server)
				if !set || !exists || !reflect.DeepEqual(expected, initial) {
					t.Error("Prepare replaced the exact original claim snapshot")
				}
				if prepared.Server.ServerType.Architecture != "amd64" {
					t.Fatal("Prepare did not return fresh evidence")
				}
				published, err := core.ClaimLeaseTargetForRepoConfigIfUnchanged(lease.LeaseID, serverSlug(prepared.Server), b.Cfg, prepared.Server, prepared.SSH, repoB, b.Cfg.IdleTimeout, reclaim, expected, exists)
				if reclaim {
					if err != nil {
						t.Fatalf("authorized publication failed: %v", err)
					}
					if published.RepoRoot != repoB || published.Labels["architecture"] != "amd64" {
						t.Fatal("reclaim did not publish fresh evidence for repo B")
					}
				} else {
					if err == nil || !strings.Contains(err.Error(), "claimed by repo") {
						t.Fatalf("unauthorized publication err=%v", err)
					}
					final, _, _ := core.ReadLeaseClaimWithPresence(lease.LeaseID)
					if !reflect.DeepEqual(final, initial) {
						t.Error("rejected repo B publication changed repo A's claim")
					}
				}
			})
		}
	}
}

func TestStaticSSHArchitectureRunOwnershipPublication(t *testing.T) {
	for _, tc := range []struct {
		name            string
		reclaim, legacy bool
	}{{name: "other-repo"}, {name: "reclaim", reclaim: true}, {name: "legacy-unowned", legacy: true}} {
		t.Run(tc.name, func(t *testing.T) {
			b, _, repoA := staticArchitectureFixture(t, "windows", "normal", "")
			lease, err := b.Acquire(context.Background(), AcquireRequest{Repo: core.Repo{Root: repoA}})
			if err != nil {
				t.Fatal(err)
			}
			initial, _, _ := core.ReadLeaseClaimWithPresence(lease.LeaseID)
			if tc.legacy {
				legacy := initial
				legacy.RepoRoot = ""
				if err := core.ReplaceLeaseClaimIfUnchanged(lease.LeaseID, initial, legacy); err != nil {
					t.Fatal(err)
				}
				initial, _, _ = core.ReadLeaseClaimWithPresence(lease.LeaseID)
			}
			repoB := t.TempDir()
			t.Chdir(repoB)
			// Getwd resolves macOS /tmp aliases exactly as run's repository discovery does.
			repoB, err = os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			configPath := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(configPath, []byte("provider: ssh\nstatic:\n  host: build.example.test\n"), 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("CRABBOX_CONFIG", configPath)
			t.Setenv("CRABBOX_PROVIDER", "ssh")
			profilePath := filepath.Join(t.TempDir(), "fixture.profile")
			if err := os.WriteFile(profilePath, []byte("FIXTURE_VALUE=neutral\n"), 0600); err != nil {
				t.Fatal(err)
			}
			calls := 0
			runArchitectureProbe = func(context.Context, SSHTarget, string, int) (string, error) {
				calls++
				return "v1|amd64|amd64|amd64|false", nil
			}
			var log bytes.Buffer
			// Reused native Windows rejects POSIX env helpers after core's claim
			// publication and Touch, but before SSH readiness/sync/workload.
			args := []string{"run", "--provider", "ssh", "--id", lease.LeaseID, "--static-host", "build.example.test", "--target", "windows", "--arch", "amd64", "--no-sync", "--no-hydrate", "--env-from-profile", profilePath, "--allow-env", "FIXTURE_VALUE", "--env-helper", "fixture"}
			if tc.reclaim {
				args = append(args, "--reclaim")
			}
			args = append(args, "--", "true")
			err = (core.App{Stdout: &log, Stderr: &log}).Run(context.Background(), args)
			after, exists, readErr := core.ReadLeaseClaimWithPresence(lease.LeaseID)
			if readErr != nil || !exists || calls != 1 {
				t.Fatalf("read=%v exists=%t probes=%d run=%v log=%s", readErr, exists, calls, err, &log)
			}
			if !tc.reclaim && !tc.legacy {
				if err == nil || !strings.Contains(err.Error(), "claimed by repo") {
					t.Errorf("expected repository-owner rejection, got %v", err)
				}
				if !reflect.DeepEqual(after, initial) {
					t.Error("rejected App.Run changed the other repository's claim")
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), "env-helper") {
					t.Errorf("did not pass claim publication and reach env-helper validation: %v", err)
				}
				if after.RepoRoot != repoB || after.Labels["architecture"] != "amd64" {
					t.Errorf("claim not adopted with fresh evidence: owner=%q architecture=%q run=%v", after.RepoRoot, after.Labels["architecture"], err)
				}
			}
			if err != nil && strings.Contains(err.Error(), "claim changed") {
				t.Error("core wrapper rejected the adapter's prematurely changed claim snapshot")
			}
		})
	}
}

func TestStaticSSHArchitecturePrepareWithoutRepository(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "owned", true: "legacy-unowned"}[legacy], func(t *testing.T) {
			b, _, repo := staticArchitectureFixture(t, "linux", "normal", "")
			lease, err := b.Acquire(context.Background(), AcquireRequest{Repo: core.Repo{Root: repo}})
			if err != nil {
				t.Fatal(err)
			}
			initial, _, _ := core.ReadLeaseClaimWithPresence(lease.LeaseID)
			if legacy {
				unowned := initial
				unowned.RepoRoot = ""
				if err := core.ReplaceLeaseClaimIfUnchanged(lease.LeaseID, initial, unowned); err != nil {
					t.Fatal(err)
				}
				initial, _, _ = core.ReadLeaseClaimWithPresence(lease.LeaseID)
				b = NewStaticSSHLeaseBackend(Provider{}.Spec(), b.Cfg, b.RT).(*staticLeaseBackend)
			}
			cacheBefore := b.acquired
			runArchitectureProbe = func(context.Context, SSHTarget, string, int) (string, error) { return "v1|amd64|-|-|-", nil }
			prepared, err := b.Resolve(context.Background(), ResolveRequest{ID: lease.LeaseID, Prepare: true, NoLocalStateMutations: true})
			if err != nil {
				t.Fatal(err)
			}
			after, _, _ := core.ReadLeaseClaimWithPresence(lease.LeaseID)
			if !reflect.DeepEqual(after, initial) || !reflect.DeepEqual(b.acquired, cacheBefore) {
				t.Fatal("repository-free Prepare changed ownership or cached evidence")
			}
			if prepared.Server.ServerType.Architecture != "amd64" {
				t.Fatal("missing fresh returned evidence")
			}
			// Pond snapshots the claim before Prepare, then publishes the returned
			// endpoint with that same snapshot. It must not adopt or refresh lifecycle.
			published, err := core.UpdateLeaseClaimEndpointIfUnchanged(lease.LeaseID, initial, prepared.Server, prepared.SSH)
			if err != nil {
				t.Fatal(err)
			}
			if published.RepoRoot != initial.RepoRoot || published.ClaimedAt != initial.ClaimedAt || published.LastUsedAt != initial.LastUsedAt || published.Labels["architecture"] != "amd64" {
				t.Fatal("endpoint publication changed ownership/lifecycle or lost fresh evidence")
			}
		})
	}
}

func TestStaticSSHArchitectureTouchRetainsPublishedEvidence(t *testing.T) {
	for _, publishedArchitecture := range []string{"arm64", "unknown"} {
		t.Run(publishedArchitecture, func(t *testing.T) {
			b, _, repo := staticArchitectureFixture(t, "linux", "normal", "")
			runArchitectureProbe = func(context.Context, SSHTarget, string, int) (string, error) {
				return "v1|" + publishedArchitecture + "|-|-|-", nil
			}
			lease, err := b.Acquire(context.Background(), AcquireRequest{Repo: core.Repo{Root: repo}})
			if err != nil {
				t.Fatal(err)
			}
			initial, _, _ := core.ReadLeaseClaimWithPresence(lease.LeaseID)
			runArchitectureProbe = func(context.Context, SSHTarget, string, int) (string, error) { return "v1|amd64|-|-|-", nil }
			prepared, err := b.Resolve(context.Background(), ResolveRequest{ID: lease.LeaseID, Prepare: true})
			if err != nil {
				t.Fatal(err)
			}
			touched, err := b.Touch(context.Background(), TouchRequest{Lease: prepared, State: "busy"})
			if err != nil {
				t.Fatal(err)
			}
			after, _, _ := core.ReadLeaseClaimWithPresence(lease.LeaseID)
			wantType := publishedArchitecture
			if wantType == "unknown" {
				wantType = ""
			}
			if touched.ServerType.Architecture != wantType || touched.Labels["architecture"] != publishedArchitecture || after.Labels["architecture"] != publishedArchitecture || after.Labels["architecture_observed_at"] != initial.Labels["architecture_observed_at"] {
				t.Fatal("Touch published or mixed in uncommitted prepared evidence")
			}
			if prepared.Server.ServerType.Architecture != "amd64" {
				t.Fatal("Touch modified returned preparation")
			}
		})
	}
}
