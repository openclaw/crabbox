package tart

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
)

const cleanupVM = "crabbox-owned-test"
const cleanupLease = "cbx_cleanupowned"

func cleanupFixture(t *testing.T) (*backend, *recordingRunner, core.LeaseClaim) {
	t.Helper()
	testutil.IsolateUserDirs(t)
	storage := filepath.Join(t.TempDir(), "Tart store ")
	if err := os.Mkdir(storage, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TART_HOME", storage)
	claimTartLease(t, t.TempDir(), cleanupLease, cleanupVM, "stopped")
	claim, exists, err := core.ReadLeaseClaimWithPresence(cleanupLease)
	if err != nil || !exists {
		t.Fatalf("read fixture claim: exists=%v err=%v", exists, err)
	}
	data, err := json.Marshal([]tartInstance{{Name: cleanupVM, State: "stopped"}})
	if err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{"list": {Stdout: string(data)}}}
	b := newBackend(Provider{}.Spec(), core.BaseConfig(), core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)
	return b, runner, claim
}

func assertNoTartMutation(t *testing.T, runner *recordingRunner) {
	t.Helper()
	for _, call := range runner.calls {
		if call.Args[0] != "list" {
			t.Fatalf("unexpected Tart mutation: %s", call.Args[0])
		}
	}
}

func TestCleanupRejectsMissingAndInconsistentOwnership(t *testing.T) {
	for _, kind := range []string{"missing", "legacy", "provider", "cloud-id", "scope", "lease-label", "slug-label", "instance-label", "storage", "duplicate", "missing-marker", "replaced-marker", "symlink-marker", "recreated-vm"} {
		t.Run(kind, func(t *testing.T) {
			b, runner, original := cleanupFixture(t)
			expected := original
			root := original.Labels["tart_storage"]
			marker := filepath.Join(root, "vms", cleanupVM, tartOwnershipFile)
			switch kind {
			case "missing":
				core.RemoveLeaseClaim(cleanupLease)
			case "legacy":
				expected.CloudImmutableID = ""
				delete(expected.Labels, "tart_storage")
			case "provider":
				expected.Provider = "other"
			case "cloud-id":
				expected.CloudID = "crabbox-other"
			case "scope":
				expected.ProviderScope = "instance:crabbox-other"
			case "lease-label":
				expected.Labels["lease"] = "cbx_other"
			case "slug-label":
				expected.Labels["slug"] = "other"
			case "instance-label":
				expected.Labels["instance"] = "crabbox-other"
			case "storage":
				t.Setenv("TART_HOME", t.TempDir())
			case "duplicate":
				claimTartLease(t, t.TempDir(), "cbx_duplicate", cleanupVM, "stopped")
			case "missing-marker":
				if err := os.Remove(marker); err != nil {
					t.Fatal(err)
				}
			case "replaced-marker":
				if err := os.WriteFile(marker, []byte(strings.Repeat("0", 64)+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "symlink-marker":
				if err := os.Rename(marker, marker+".saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(marker+".saved", marker); err != nil {
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
			}
			if kind != "missing" {
				// Deliberately malformed durable input exercises admission, rather
				// than relying on claim writers to construct invalid identities.
				writeCleanupClaim(t, expected)
			}
			if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
				t.Fatal(err)
			}
			assertNoTartMutation(t, runner)
			if kind != "missing" {
				if err := core.VerifyLeaseClaimUnchanged(cleanupLease, expected); err != nil {
					t.Fatalf("claim changed: %v", err)
				}
			}
		})
	}
}

func writeCleanupClaim(t *testing.T, claim core.LeaseClaim) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(os.Getenv("XDG_STATE_HOME"), "crabbox", "claims", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var current core.LeaseClaim
		if err := json.Unmarshal(data, &current); err != nil {
			t.Fatal(err)
		}
		if current.LeaseID != claim.LeaseID {
			continue
		}
		data, err = json.Marshal(claim)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Fatal("fixture claim file not found")
}

func TestCleanupRevalidatesInventoryAndIdentity(t *testing.T) {
	for _, kind := range []string{"running", "missing", "marker", "list-error"} {
		t.Run(kind, func(t *testing.T) {
			b, runner, claim := cleanupFixture(t)
			lists := 0
			runner.onRun = func(req core.LocalCommandRequest) {
				if req.Args[0] != "list" {
					return
				}
				lists++
				if lists != 2 {
					return
				}
				switch kind {
				case "running":
					runner.responses["list"] = core.LocalCommandResult{Stdout: `[{"Name":"` + cleanupVM + `","State":"running","Running":true}]`}
				case "missing":
					runner.responses["list"] = core.LocalCommandResult{Stdout: `[]`}
				case "marker":
					if err := os.Remove(filepath.Join(claim.Labels["tart_storage"], "vms", cleanupVM, tartOwnershipFile)); err != nil {
						t.Fatal(err)
					}
				case "list-error":
					runner.errors = map[string]error{"list": errors.New("inventory unavailable")}
				}
			}
			if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err == nil {
				t.Fatal("cleanup accepted changed observation")
			}
			assertNoTartMutation(t, runner)
			if err := core.VerifyLeaseClaimUnchanged(cleanupLease, claim); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCleanupRejectsChangedClaimBeforeMutation(t *testing.T) {
	b, runner, claim := cleanupFixture(t)
	if _, err := core.UpdateLeaseClaimLabelsIfUnchangedAfter(cleanupLease, claim, map[string]string{"keep": "true"}, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := b.cleanupInstance(context.Background(), b.configForRun(), tartInstance{Name: cleanupVM, State: "stopped"}, claim, claim.Labels["tart_storage"]); err == nil {
		t.Fatal("cleanup accepted stale claim snapshot")
	}
	assertNoTartMutation(t, runner)
}

func TestCleanupRechecksMarkerAfterStop(t *testing.T) {
	b, runner, claim := cleanupFixture(t)
	claim.LastUsedAt = time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)
	writeCleanupClaim(t, claim)
	runner.responses["list"] = core.LocalCommandResult{Stdout: `[{"Name":"` + cleanupVM + `","State":"running","Running":true}]`}
	stopped := false
	runner.onRun = func(req core.LocalCommandRequest) {
		if req.Args[0] == "stop" {
			stopped = true
			if err := os.Remove(filepath.Join(claim.Labels["tart_storage"], "vms", cleanupVM, tartOwnershipFile)); err != nil {
				t.Fatal(err)
			}
		}
		if req.Args[0] == "delete" {
			t.Fatal("deleted after ownership changed during stop")
		}
	}
	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err == nil {
		t.Fatal("cleanup accepted missing marker after stop")
	}
	if !stopped {
		t.Fatal("test did not reach stop")
	}
	if err := core.VerifyLeaseClaimUnchanged(cleanupLease, claim); err != nil {
		t.Fatal(err)
	}
}

func TestCleanupPreservesOrphanClaimFromDifferentStore(t *testing.T) {
	b, runner, claim := cleanupFixture(t)
	t.Setenv("TART_HOME", t.TempDir())
	runner.responses["list"] = core.LocalCommandResult{Stdout: `[]`}
	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if err := core.VerifyLeaseClaimUnchanged(cleanupLease, claim); err != nil {
		t.Fatal(err)
	}
}

func TestTouchPreservesCleanupOwnershipThroughClaimUpdate(t *testing.T) {
	b, runner, claim := cleanupFixture(t)
	server := b.serverFromInstance(tartInstance{Name: cleanupVM, State: "stopped"}, claim, b.configForRun())
	touched, err := b.Touch(context.Background(), core.TouchRequest{
		Lease: core.LeaseTarget{LeaseID: cleanupLease, Server: server}, State: "stopped",
	})
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := core.ReadLeaseClaim(cleanupLease)
	if err != nil || !maps.Equal(persisted.Labels, touched.Labels) || persisted.Revision == claim.Revision {
		t.Fatalf("Touch did not publish its labels: claim=%+v err=%v", persisted, err)
	}
	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	for _, call := range runner.calls {
		if call.Args[0] == "delete" {
			return
		}
	}
	t.Fatal("touch lost the storage/marker binding needed to clean up the owned VM")
}

type heartbeatClock struct{ now time.Time }

func (c heartbeatClock) Now() time.Time { return c.now }

func TestTartTouchRetainsLegacyLeaseWithoutAdoption(t *testing.T) {
	testutil.IsolateUserDirs(t)
	storage := t.TempDir()
	t.Setenv("TART_HOME", storage)
	name := "crabbox-legacy-heartbeat"
	leaseID := "cbx_legacyheartbeat"
	vmDir := filepath.Join(storage, "vms", name)
	if err := os.MkdirAll(vmDir, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(vmDir, "config.json")
	if err := os.WriteFile(configPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := core.Server{Provider: providerName, CloudID: name, Name: name, Labels: map[string]string{
		"provider": providerName, "instance": name, "lease": leaseID, "slug": "legacy-heartbeat", "state": "ready",
	}}
	if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(leaseID, "legacy-heartbeat", providerName, instanceScope(name), "", t.TempDir(), 30*time.Minute, false, server, core.SSHTarget{}); err != nil {
		t.Fatal(err)
	}
	claim, err := core.ReadLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{}
	b := newBackend(Provider{}.Spec(), core.BaseConfig(), core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)
	lease := core.LeaseTarget{LeaseID: leaseID, Server: b.serverFromInstance(tartInstance{Name: name, State: "running"}, claim, b.configForRun())}
	if err := b.AuthorizeStatusTouchClaim(context.Background(), lease, claim); err == nil {
		t.Fatal("public admission accepted a legacy ownership binding")
	}
	if _, err := b.Touch(context.Background(), core.TouchRequest{Lease: lease, State: "ready"}); err == nil {
		t.Fatal("legacy lease was renewed without ownership evidence")
	}
	after, err := core.ReadLeaseClaim(leaseID)
	if err != nil || !reflect.DeepEqual(after, claim) {
		t.Fatalf("legacy claim changed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(vmDir, tartOwnershipFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy touch created an ownership marker: %v", err)
	}
	if data, err := os.ReadFile(configPath); err != nil || string(data) != "{}\n" || len(runner.calls) != 0 {
		t.Fatalf("legacy VM was changed: config=%q err=%v calls=%d", data, err, len(runner.calls))
	}
}

func TestTartHeartbeatPersistsAcrossFreshResolve(t *testing.T) {
	for _, mode := range []string{"preserve", "replace"} {
		t.Run(mode, func(t *testing.T) {
			b, runner, original := cleanupFixture(t)
			now := time.Now().UTC().Truncate(time.Second)
			claim := original
			claim.Labels = maps.Clone(original.Labels)
			claim.Labels["state"] = "ready"
			claim.Labels["ttl_secs"] = "3600"
			claim.Labels["image"] = "fixture-image"
			for _, key := range []string{"created_at", "last_touched_at", "expires_at", "idle_timeout", "idle_timeout_secs"} {
				delete(claim.Labels, key)
			}
			claim.ClaimedAt = now.Add(-20 * time.Minute).Format(time.RFC3339)
			claim.LastUsedAt = now.Add(-5 * time.Minute).Format(time.RFC3339)
			claim.IdleTimeoutSeconds = 1800
			if err := core.ReplaceLeaseClaimIfUnchanged(cleanupLease, original, claim); err != nil {
				t.Fatal(err)
			}
			claim, err := core.ReadLeaseClaim(cleanupLease)
			if err != nil {
				t.Fatal(err)
			}
			inventory, err := json.Marshal([]tartInstance{{Name: cleanupVM, State: "running", Running: true}})
			if err != nil {
				t.Fatal(err)
			}
			runner.responses["list"] = core.LocalCommandResult{Stdout: string(inventory)}
			runner.responses["ip"] = core.LocalCommandResult{Stdout: "192.0.2.10\n"}
			b.rt.Clock = heartbeatClock{now: now}
			resolve := core.ResolveRequest{ID: cleanupLease, StatusOnly: true, NoLocalStateMutations: true, Repo: core.Repo{Root: t.TempDir()}}
			lease, err := b.Resolve(context.Background(), resolve)
			if err != nil {
				t.Fatal(err)
			}
			if lease.SSH.Host != "192.0.2.10" || lease.SSH.Port != sshPort || lease.SSH.ReadyCheck == "" || !lease.SSH.SSHConfigProxy {
				t.Fatalf("read-only Resolve lost its SSH target: %+v", lease.SSH)
			}
			if lease.Server.Labels["idle_timeout_secs"] != "1800" || lease.Server.Labels["created_at"] != core.LeaseLabelTime(now.Add(-20*time.Minute)) || lease.Server.Labels["last_touched_at"] != core.LeaseLabelTime(now.Add(-5*time.Minute)) || lease.Server.Labels["expires_at"] != "" {
				t.Fatalf("legacy projection changed persisted policy: %v", lease.Server.Labels)
			}
			beforeTouch, err := core.ReadLeaseClaim(cleanupLease)
			if err != nil || !reflect.DeepEqual(beforeTouch, claim) {
				t.Fatalf("read-only Resolve changed the claim: %v", err)
			}
			req := core.TouchRequest{Lease: lease, State: "ready", IdleTimeout: 5 * time.Minute}
			wantIdle := 1800
			wantExpiry := now.Add(30 * time.Minute)
			if mode == "replace" {
				override := 90 * time.Minute
				req.IdleTimeoutOverride = &override
				wantIdle = 5400
				wantExpiry = now.Add(40 * time.Minute)
			}
			touched, err := b.Touch(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			persisted, err := core.ReadLeaseClaim(cleanupLease)
			if err != nil {
				t.Fatal(err)
			}
			if persisted.IdleTimeoutSeconds != wantIdle || persisted.LastUsedAt != now.Format(time.RFC3339) || persisted.Revision == claim.Revision || !maps.Equal(persisted.Labels, touched.Labels) || touched.Labels["expires_at"] != core.LeaseLabelTime(wantExpiry) || touched.Labels["image"] != "fixture-image" {
				t.Fatalf("unexpected committed heartbeat: %+v", persisted)
			}
			snapshot, exists, set := core.ServerLeaseClaimSnapshot(touched)
			if !set || !exists || !reflect.DeepEqual(snapshot, persisted) {
				t.Fatal("Touch did not return the committed claim snapshot")
			}
			cfg := b.cfg
			cfg.IdleTimeout = 5 * time.Minute
			fresh := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner, Clock: heartbeatClock{now: now.Add(15 * time.Minute)}}).(*backend)
			resolved, err := fresh.Resolve(context.Background(), resolve)
			if err != nil {
				t.Fatal(err)
			}
			if resolved.LeaseID != cleanupLease || resolved.Server.ImmutableID != claim.CloudImmutableID || resolved.Server.Labels["idle_timeout_secs"] != touched.Labels["idle_timeout_secs"] || resolved.Server.Labels["expires_at"] != touched.Labels["expires_at"] || resolved.Server.Labels["last_touched_at"] != touched.Labels["last_touched_at"] {
				t.Fatalf("fresh Resolve lost committed policy: %+v", resolved.Server)
			}
			afterResolve, err := core.ReadLeaseClaim(cleanupLease)
			if err != nil || !reflect.DeepEqual(afterResolve, persisted) {
				t.Fatalf("fresh read-only Resolve changed the claim: %v", err)
			}
			runResolve := core.ResolveRequest{ID: cleanupLease, Repo: core.Repo{Root: claim.RepoRoot}}
			forRun, err := fresh.Resolve(context.Background(), runResolve)
			if err != nil {
				t.Fatal(err)
			}
			afterRunResolve, err := core.ReadLeaseClaim(cleanupLease)
			if err != nil {
				t.Fatal(err)
			}
			runSnapshot, exists, set := core.ServerLeaseClaimSnapshot(forRun.Server)
			if !set || !exists || !reflect.DeepEqual(runSnapshot, afterRunResolve) || afterRunResolve.IdleTimeoutSeconds != wantIdle {
				t.Fatal("run Resolve returned a stale snapshot or replaced stored idle policy")
			}
			for _, call := range runner.calls {
				if call.Args[0] != "list" && call.Args[0] != "ip" {
					t.Fatalf("heartbeat/status issued a non-observation command: %s", call.Args[0])
				}
			}
		})
	}
}

func TestTartTouchRejectsCanceledOrOutdatedSnapshot(t *testing.T) {
	b, _, claim := cleanupFixture(t)
	lease := core.LeaseTarget{LeaseID: cleanupLease, Server: b.serverFromInstance(tartInstance{Name: cleanupVM, State: "stopped"}, claim, b.configForRun())}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := b.Touch(ctx, core.TouchRequest{Lease: lease, State: "stopped"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled touch error=%v", err)
	}
	afterCancel, err := core.ReadLeaseClaim(cleanupLease)
	if err != nil || !reflect.DeepEqual(afterCancel, claim) {
		t.Fatalf("canceled touch changed claim: %v", err)
	}
	if _, err := b.Touch(context.Background(), core.TouchRequest{Lease: lease, State: "stopped"}); err != nil {
		t.Fatal(err)
	}
	committed, err := core.ReadLeaseClaim(cleanupLease)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Touch(context.Background(), core.TouchRequest{Lease: lease, State: "ready"}); err == nil {
		t.Fatal("accepted an outdated claim snapshot")
	}
	afterStale, err := core.ReadLeaseClaim(cleanupLease)
	if err != nil || !reflect.DeepEqual(afterStale, committed) {
		t.Fatalf("outdated touch changed claim: %v", err)
	}
}

func TestTartOwnershipMarkerCannotAdoptOrFollowSymlinks(t *testing.T) {
	_, _, claim := cleanupFixture(t)
	root := claim.Labels["tart_storage"]
	if _, _, err := createTartVMIdentity(cleanupVM); err == nil {
		t.Fatal("existing ownership marker overwritten")
	}
	if err := verifyTartVMIdentity(cleanupVM, root, claim.CloudImmutableID); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", ".", "..", "../outside", `..\outside`} {
		if _, err := tartVMDirectory(root, name); err == nil {
			t.Fatalf("accepted unsafe name %q", name)
		}
	}
	if err := os.Symlink(filepath.Join(root, "vms", cleanupVM), filepath.Join(root, "vms", "crabbox-alias")); err != nil {
		t.Fatal(err)
	}
	if _, err := readTartVMIdentity(root, "crabbox-alias"); err == nil {
		t.Fatal("followed instance directory symlink")
	}
}

func TestCleanupFencesClaimThroughDeleteAndRetainsKey(t *testing.T) {
	b, runner, claim := cleanupFixture(t)
	key, err := core.TestboxKeyPath(cleanupLease)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(key), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, []byte("fixture key"), 0o600); err != nil {
		t.Fatal(err)
	}
	started, done := make(chan struct{}), make(chan error, 1)
	var once sync.Once
	runner.onRun = func(req core.LocalCommandRequest) {
		if req.Args[0] != "delete" {
			return
		}
		once.Do(func() {
			go func() {
				close(started)
				done <- core.WithLeaseClaimUnchanged(cleanupLease, claim, func() error { return errors.New("writer entered stale claim") })
			}()
			<-started
			select {
			case err := <-done:
				t.Fatalf("claim writer passed deletion fence: %v", err)
			case <-time.After(50 * time.Millisecond):
			}
		})
	}
	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil || strings.Contains(err.Error(), "writer entered") {
			t.Fatalf("stale writer accepted: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("writer did not finish after cleanup")
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(cleanupLease); err != nil || exists {
		t.Fatalf("claim remains: exists=%v err=%v", exists, err)
	}
	if _, err := os.Stat(key); err != nil {
		t.Fatalf("key removed without key-creation fence: %v", err)
	}
}
