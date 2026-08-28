package ssh

import (
	"bytes"
	"context"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func staticOwnershipCacheSnapshot(b *staticLeaseBackend) LeaseTarget {
	lease := b.acquired
	lease.Server.Labels = maps.Clone(lease.Server.Labels)
	return lease
}

func trackStaticOwnershipPreparation(t *testing.T, b *staticLeaseBackend, expected core.LeaseClaim, exists bool, output string) (*int, *int) {
	t.Helper()
	cacheBefore := staticOwnershipCacheSnapshot(b)
	checkUnpublished := func() {
		t.Helper()
		claim, present, err := core.ReadLeaseClaimWithPresence(b.Cfg.Static.ID)
		if err != nil || present != exists || !reflect.DeepEqual(claim, expected) {
			t.Fatalf("preparation published claim before successful assertion: present=%t err=%v", present, err)
		}
		if !reflect.DeepEqual(b.acquired, cacheBefore) {
			t.Fatal("preparation published cached evidence before successful assertion")
		}
	}
	readiness, probes := 0, 0
	ready := waitForSSH
	waitForSSH = func(ctx context.Context, target *SSHTarget, log io.Writer) error {
		readiness++
		checkUnpublished()
		return ready(ctx, target, log)
	}
	runArchitectureProbe = func(context.Context, SSHTarget, string, int) (string, error) {
		probes++
		checkUnpublished()
		return output, nil
	}
	return &readiness, &probes
}

func TestStaticSSHArchitecturePrepareDefersOwnershipPublication(t *testing.T) {
	for _, cached := range []bool{false, true} {
		for _, tc := range []struct {
			name                         string
			sameOwner, reclaim, mismatch bool
		}{
			{name: "other-repo"},
			{name: "other-repo-mismatch", mismatch: true},
			{name: "same-owner", sameOwner: true},
			{name: "same-owner-mismatch", sameOwner: true, mismatch: true},
			{name: "reclaim", reclaim: true},
			{name: "reclaim-mismatch", reclaim: true, mismatch: true},
		} {
			t.Run(map[bool]string{false: "claimed", true: "cached"}[cached]+"/"+tc.name, func(t *testing.T) {
				b, _, repoA := staticArchitectureFixture(t, "linux", "normal", "")
				lease, err := b.Acquire(context.Background(), AcquireRequest{Repo: core.Repo{Root: repoA}})
				if err != nil {
					t.Fatal(err)
				}
				initial, _, _ := core.ReadLeaseClaimWithPresence(lease.LeaseID)
				if !cached {
					b = NewStaticSSHLeaseBackend(Provider{}.Spec(), b.Cfg, b.RT).(*staticLeaseBackend)
				}
				cacheBefore := staticOwnershipCacheSnapshot(b)
				repoB := t.TempDir()
				if tc.sameOwner {
					repoB = repoA
				}
				if tc.mismatch {
					b.Cfg.Architecture = "arm64"
					core.MarkArchitectureExplicit(&b.Cfg)
				}
				readiness, probes := trackStaticOwnershipPreparation(t, b, initial, true, "v1|amd64|-|-|-")
				prepared, err := b.Resolve(context.Background(), ResolveRequest{ID: lease.LeaseID, Repo: core.Repo{Root: repoB}, Reclaim: tc.reclaim, Prepare: true})
				after, _, _ := core.ReadLeaseClaimWithPresence(lease.LeaseID)
				if !reflect.DeepEqual(after, initial) {
					t.Error("Prepare mutated the claim before repository authorization")
				}
				if !reflect.DeepEqual(b.acquired, cacheBefore) {
					t.Error("Prepare replaced cached evidence before repository authorization")
				}
				if !tc.sameOwner && !tc.reclaim {
					if err == nil || !strings.Contains(err.Error(), "claimed by repo") || *readiness != 0 || *probes != 0 {
						t.Fatalf("foreign owner must be rejected before SSH: readiness=%d probes=%d err=%v", *readiness, *probes, err)
					}
					return
				}
				if *readiness != 1 || *probes != 1 {
					t.Fatalf("authorized Prepare did not refresh: readiness=%d probes=%d err=%v", *readiness, *probes, err)
				}
				if tc.mismatch {
					if err == nil || !strings.Contains(err.Error(), "architecture=arm64 assertion failed: observed=amd64") {
						t.Fatalf("assertion failure=%v", err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				expected, exists, set := core.ServerLeaseClaimSnapshot(prepared.Server)
				if !set || !exists || !reflect.DeepEqual(expected, initial) {
					t.Error("Prepare replaced the exact original claim snapshot")
				}
				if prepared.Server.ServerType.Architecture != "amd64" {
					t.Fatal("Prepare did not return fresh evidence")
				}
				published, err := core.ClaimLeaseTargetForRepoConfigIfUnchanged(lease.LeaseID, serverSlug(prepared.Server), b.Cfg, prepared.Server, prepared.SSH, repoB, b.Cfg.IdleTimeout, tc.reclaim, expected, exists)
				if err != nil {
					t.Fatalf("authorized publication failed: %v", err)
				}
				if published.RepoRoot != repoB || published.Labels["architecture"] != "amd64" {
					t.Fatal("authorized publication did not retain fresh evidence for requested repo")
				}
			})
		}
	}
}

func TestStaticSSHArchitectureRunOwnershipPublication(t *testing.T) {
	for _, tc := range []struct {
		name                                 string
		sameOwner, reclaim, legacy, mismatch bool
		hostOverride                         bool
	}{
		{name: "other-repo"},
		{name: "same-owner", sameOwner: true},
		{name: "same-owner-mismatch", sameOwner: true, mismatch: true},
		{name: "reclaim", reclaim: true},
		{name: "reclaim-mismatch", reclaim: true, mismatch: true},
		{name: "legacy-unowned", legacy: true},
		{name: "host-override-other-repo", hostOverride: true},
		{name: "host-override-other-repo-mismatch", hostOverride: true, mismatch: true},
		{name: "host-override-same-owner", hostOverride: true, sameOwner: true},
		{name: "host-override-reclaim", hostOverride: true, reclaim: true},
		{name: "host-override-reclaim-mismatch", hostOverride: true, reclaim: true, mismatch: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, _, repoA := staticArchitectureFixture(t, "windows", "normal", "")
			t.Chdir(repoA)
			repoA, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
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
			if tc.sameOwner {
				repoB = repoA
			}
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
			cacheBefore := staticOwnershipCacheSnapshot(b)
			readiness, probes := trackStaticOwnershipPreparation(t, b, initial, true, "v1|amd64|amd64|amd64|false")
			host := b.Cfg.Static.Host
			if tc.hostOverride {
				host = "other.example.test"
			}
			ready := waitForSSH
			waitForSSH = func(ctx context.Context, target *SSHTarget, log io.Writer) error {
				if target.Host != host {
					t.Fatalf("readiness used %q instead of selected host %q", target.Host, host)
				}
				return ready(ctx, target, log)
			}
			probe := runArchitectureProbe
			runArchitectureProbe = func(ctx context.Context, target SSHTarget, command string, limit int) (string, error) {
				if target.Host != host {
					t.Fatalf("probe used %q instead of selected host %q", target.Host, host)
				}
				return probe(ctx, target, command, limit)
			}
			var log bytes.Buffer
			// Reused native Windows rejects POSIX env helpers after core's claim
			// publication and Touch, but before run's later SSH readiness/sync/workload.
			assertion := "amd64"
			if tc.mismatch {
				assertion = "arm64"
			}
			args := []string{"run", "--provider", "ssh", "--id", lease.LeaseID, "--static-host", host, "--target", "windows", "--arch", assertion, "--no-sync", "--no-hydrate", "--env-from-profile", profilePath, "--allow-env", "FIXTURE_VALUE", "--env-helper", "fixture"}
			if tc.reclaim {
				args = append(args, "--reclaim")
			}
			args = append(args, "--", "true")
			err = (core.App{Stdout: &log, Stderr: &log}).Run(context.Background(), args)
			after, exists, readErr := core.ReadLeaseClaimWithPresence(lease.LeaseID)
			if readErr != nil || !exists {
				t.Fatalf("read=%v exists=%t run=%v log=%s", readErr, exists, err, &log)
			}
			if !reflect.DeepEqual(b.acquired, cacheBefore) {
				t.Error("App.Run changed the prior backend's cache")
			}
			if !tc.sameOwner && !tc.reclaim && !tc.legacy {
				if err == nil || !strings.Contains(err.Error(), "claimed by repo") || *readiness != 0 || *probes != 0 {
					t.Errorf("foreign owner must be rejected before SSH: readiness=%d probes=%d err=%v", *readiness, *probes, err)
				}
				if !reflect.DeepEqual(after, initial) {
					t.Error("rejected App.Run changed the other repository's claim")
				}
			} else {
				if *readiness != 1 || *probes != 1 {
					t.Errorf("authorized App.Run did not refresh: readiness=%d probes=%d err=%v", *readiness, *probes, err)
				}
				if tc.mismatch {
					if err == nil || !strings.Contains(err.Error(), "architecture=arm64 assertion failed: observed=amd64") || !reflect.DeepEqual(after, initial) {
						t.Fatalf("failed App.Run assertion changed claim or wrong failure: %v", err)
					}
					return
				}
				if err == nil || !strings.Contains(err.Error(), "env-helper") {
					t.Errorf("did not pass claim publication and reach env-helper validation: %v", err)
				}
				if after.RepoRoot != repoB || after.Labels["architecture"] != "amd64" || after.StaticHost != host {
					t.Errorf("claim not adopted with fresh evidence: owner=%q architecture=%q host=%q run=%v", after.RepoRoot, after.Labels["architecture"], after.StaticHost, err)
				}
			}
			if err != nil && strings.Contains(err.Error(), "claim changed") {
				t.Error("core wrapper rejected the adapter's prematurely changed claim snapshot")
			}
		})
	}
}

func TestStaticSSHArchitectureConfiguredHostOwnershipPreflight(t *testing.T) {
	for _, tc := range []struct {
		name                                        string
		sameOwner, reclaim, noRepo, missing, byHost bool
		race                                        string
	}{
		{name: "other-repo"},
		{name: "configured-host-other-repo", byHost: true},
		{name: "same-owner", sameOwner: true},
		{name: "reclaim", reclaim: true},
		{name: "admin", noRepo: true},
		{name: "unclaimed", missing: true},
		{name: "same-owner-replaced-during-probe", sameOwner: true, race: "replace"},
		{name: "reclaim-removed-during-probe", reclaim: true, race: "remove"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, _, repoA := staticArchitectureFixture(t, "linux", "normal", "")
			if !tc.missing {
				if _, err := b.Acquire(context.Background(), AcquireRequest{Repo: core.Repo{Root: repoA}}); err != nil {
					t.Fatal(err)
				}
			}
			initial, exists, err := core.ReadLeaseClaimWithPresence(b.Cfg.Static.ID)
			if err != nil || exists == tc.missing {
				t.Fatalf("initial claim: exists=%t err=%v", exists, err)
			}
			b.Cfg.Static.Host = "other.example.test"
			b.Cfg.Architecture = "amd64"
			core.MarkArchitectureExplicit(&b.Cfg)
			repo := t.TempDir()
			if tc.sameOwner {
				repo = repoA
			} else if tc.noRepo {
				repo = ""
			}
			id := b.Cfg.Static.ID
			if tc.byHost {
				id = b.Cfg.Static.Host
			}
			offline, err := b.Resolve(context.Background(), ResolveRequest{ID: id})
			if err != nil {
				t.Fatal(err)
			}
			if _, present, set := core.ServerLeaseClaimSnapshot(offline.Server); present || set || offline.LeaseID != b.Cfg.Static.ID || offline.SSH.Host != b.Cfg.Static.Host {
				t.Fatal("fixture did not select the configured endpoint without a claim snapshot")
			}
			cacheBefore := staticOwnershipCacheSnapshot(b)
			readiness, probes := trackStaticOwnershipPreparation(t, b, initial, exists, "v1|amd64|-|-|-")
			wantClaim, wantPresent := initial, exists
			probe := runArchitectureProbe
			runArchitectureProbe = func(ctx context.Context, target SSHTarget, command string, limit int) (string, error) {
				if target.Host != b.Cfg.Static.Host {
					t.Fatalf("probe ignored configured host: %q", target.Host)
				}
				output, err := probe(ctx, target, command, limit)
				switch tc.race {
				case "replace":
					replacement := initial
					replacement.RepoRoot = t.TempDir()
					if err := core.ReplaceLeaseClaimIfUnchanged(b.Cfg.Static.ID, initial, replacement); err != nil {
						t.Fatal(err)
					}
				case "remove":
					core.RemoveLeaseClaim(b.Cfg.Static.ID)
				}
				if tc.race != "" {
					var readErr error
					wantClaim, wantPresent, readErr = core.ReadLeaseClaimWithPresence(b.Cfg.Static.ID)
					if readErr != nil || wantPresent != (tc.race == "replace") {
						t.Fatalf("failed to stage claim race: present=%t err=%v", wantPresent, readErr)
					}
				}
				return output, err
			}
			prepared, err := b.Resolve(context.Background(), ResolveRequest{ID: id, Repo: core.Repo{Root: repo}, Reclaim: tc.reclaim, Prepare: true})
			after, present, readErr := core.ReadLeaseClaimWithPresence(b.Cfg.Static.ID)
			if readErr != nil || !reflect.DeepEqual(b.acquired, cacheBefore) {
				t.Fatalf("preparation changed cache or claim became unreadable: %v", readErr)
			}
			if present != wantPresent || !reflect.DeepEqual(after, wantClaim) {
				t.Fatal("preparation published the configured endpoint or overwrote its claim")
			}
			denied := !tc.sameOwner && !tc.reclaim && !tc.noRepo && !tc.missing
			if denied {
				if err == nil || !strings.Contains(err.Error(), "claimed by repo") || *readiness != 0 || *probes != 0 {
					t.Fatalf("configured endpoint bypassed known owner: readiness=%d probes=%d err=%v", *readiness, *probes, err)
				}
				return
			}
			if *readiness != 1 || *probes != 1 {
				t.Fatalf("authorized configured endpoint was not observed: readiness=%d probes=%d err=%v", *readiness, *probes, err)
			}
			if tc.race != "" {
				if err == nil || !strings.Contains(err.Error(), "claim changed") {
					t.Fatalf("configured preparation did not preserve the raced claim: present=%t err=%v", present, err)
				}
				return
			}
			if err != nil || prepared.LeaseID != b.Cfg.Static.ID || prepared.SSH.Host != b.Cfg.Static.Host || prepared.Server.ServerType.Architecture != "amd64" {
				t.Fatalf("configured endpoint preparation failed: %v", err)
			}
			if _, present, set := core.ServerLeaseClaimSnapshot(prepared.Server); present || set {
				t.Fatal("ownership preflight attached an identity snapshot for a different endpoint")
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
			cacheBefore := staticOwnershipCacheSnapshot(b)
			readiness, probes := trackStaticOwnershipPreparation(t, b, initial, true, "v1|amd64|-|-|-")
			prepared, err := b.Resolve(context.Background(), ResolveRequest{ID: lease.LeaseID, Prepare: true, NoLocalStateMutations: true})
			if err != nil {
				t.Fatal(err)
			}
			after, _, _ := core.ReadLeaseClaimWithPresence(lease.LeaseID)
			if !reflect.DeepEqual(after, initial) || !reflect.DeepEqual(b.acquired, cacheBefore) {
				t.Fatal("repository-free Prepare changed ownership or cached evidence")
			}
			if prepared.Server.ServerType.Architecture != "amd64" || *readiness != 1 || *probes != 1 {
				t.Fatalf("missing fresh returned evidence: readiness=%d probes=%d", *readiness, *probes)
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

func TestStaticSSHArchitecturePrepareRejectsStaleCachedClaim(t *testing.T) {
	for _, change := range []string{"owner", "revision", "provider", "remove"} {
		for _, caller := range []string{"same-owner", "reclaim", "admin"} {
			t.Run(change+"/"+caller, func(t *testing.T) {
				b, _, repo := staticArchitectureFixture(t, "linux", "normal", "")
				lease, err := b.Acquire(context.Background(), AcquireRequest{Repo: core.Repo{Root: repo}})
				if err != nil {
					t.Fatal(err)
				}
				initial, _, _ := core.ReadLeaseClaimWithPresence(lease.LeaseID)
				replacement := initial
				switch change {
				case "owner":
					replacement.RepoRoot = t.TempDir()
				case "provider":
					replacement.Provider = "other-provider"
				case "revision":
					// A same-value rewrite must invalidate the cached revision too.
				case "remove":
					core.RemoveLeaseClaim(lease.LeaseID)
				}
				if change != "remove" {
					if err := core.ReplaceLeaseClaimIfUnchanged(lease.LeaseID, initial, replacement); err != nil {
						t.Fatal(err)
					}
				}
				current, exists, err := core.ReadLeaseClaimWithPresence(lease.LeaseID)
				if err != nil || exists != (change != "remove") || (exists && current.Revision == initial.Revision) {
					t.Fatalf("claim replacement failed: exists=%t err=%v", exists, err)
				}
				cacheBefore := staticOwnershipCacheSnapshot(b)
				readiness, probes := trackStaticOwnershipPreparation(t, b, current, exists, "v1|amd64|-|-|-")
				req := ResolveRequest{ID: lease.LeaseID, Repo: core.Repo{Root: repo}, Prepare: true}
				if caller == "reclaim" {
					req.Reclaim = true
				} else if caller == "admin" {
					req.Repo = core.Repo{}
					req.NoLocalStateMutations = true
				}
				_, err = b.Resolve(context.Background(), req)
				after, present, readErr := core.ReadLeaseClaimWithPresence(lease.LeaseID)
				if readErr != nil || present != exists || !reflect.DeepEqual(after, current) || !reflect.DeepEqual(b.acquired, cacheBefore) {
					t.Error("stale preparation changed claim or cached evidence")
				}
				if err == nil || !strings.Contains(err.Error(), "claim changed") || *readiness != 0 || *probes != 0 {
					t.Fatalf("stale cached claim must fail before SSH: readiness=%d probes=%d err=%v", *readiness, *probes, err)
				}
			})
		}
	}
}

func TestStaticSSHArchitectureAcquireOwnershipPreflight(t *testing.T) {
	for _, tc := range []struct {
		name                                            string
		unclaimed, sameOwner, reclaim, noRepo, mismatch bool
	}{
		{name: "other-repo"},
		{name: "other-repo-mismatch", mismatch: true},
		{name: "same-owner", sameOwner: true},
		{name: "same-owner-mismatch", sameOwner: true, mismatch: true},
		{name: "reclaim", reclaim: true},
		{name: "reclaim-mismatch", reclaim: true, mismatch: true},
		{name: "unclaimed", unclaimed: true},
		{name: "unclaimed-mismatch", unclaimed: true, mismatch: true},
		{name: "no-repo", noRepo: true},
		{name: "no-repo-mismatch", noRepo: true, mismatch: true},
		{name: "unclaimed-no-repo", unclaimed: true, noRepo: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, _, repoA := staticArchitectureFixture(t, "linux", "normal", "")
			if !tc.unclaimed {
				if _, err := b.Acquire(context.Background(), AcquireRequest{Repo: core.Repo{Root: repoA}}); err != nil {
					t.Fatal(err)
				}
			}
			initial, exists, err := core.ReadLeaseClaimWithPresence(b.Cfg.Static.ID)
			if err != nil || exists == tc.unclaimed {
				t.Fatalf("initial claim: exists=%t err=%v", exists, err)
			}
			repo := t.TempDir()
			if tc.sameOwner {
				repo = repoA
			} else if tc.noRepo {
				repo = ""
			}
			if tc.mismatch {
				b.Cfg.Architecture = "arm64"
				core.MarkArchitectureExplicit(&b.Cfg)
			}
			cacheBefore := staticOwnershipCacheSnapshot(b)
			readiness, probes := trackStaticOwnershipPreparation(t, b, initial, exists, "v1|amd64|-|-|-")
			lease, err := b.Acquire(context.Background(), AcquireRequest{Repo: core.Repo{Root: repo}, Reclaim: tc.reclaim})
			after, present, readErr := core.ReadLeaseClaimWithPresence(b.Cfg.Static.ID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			denied := !tc.unclaimed && !tc.sameOwner && !tc.reclaim && !tc.noRepo
			if denied || tc.mismatch || tc.noRepo {
				if present != exists || !reflect.DeepEqual(after, initial) {
					t.Error("rejected or repository-free acquisition changed claim")
				}
			}
			if denied || tc.mismatch {
				if !reflect.DeepEqual(b.acquired, cacheBefore) {
					t.Error("rejected acquisition changed cached evidence")
				}
			}
			if denied {
				if err == nil || !strings.Contains(err.Error(), "claimed by repo") || *readiness != 0 || *probes != 0 {
					t.Fatalf("foreign owner must be rejected before SSH: readiness=%d probes=%d err=%v", *readiness, *probes, err)
				}
				return
			}
			if *readiness != 1 || *probes != 1 {
				t.Fatalf("authorized acquisition did not refresh: readiness=%d probes=%d err=%v", *readiness, *probes, err)
			}
			if tc.mismatch {
				if err == nil || !strings.Contains(err.Error(), "architecture=arm64 assertion failed: observed=amd64") {
					t.Fatalf("assertion failure=%v", err)
				}
				return
			}
			if err != nil || lease.Server.ServerType.Architecture != "amd64" {
				t.Fatalf("authorized acquisition failed: %v", err)
			}
			if !tc.noRepo && (!present || after.RepoRoot != repo || after.Labels["architecture"] != "amd64") {
				t.Fatal("acquisition did not publish fresh evidence for requested owner")
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
