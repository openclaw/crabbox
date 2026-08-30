package tart

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
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
	t.Setenv("TART_HOME", t.TempDir())
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
	key, err := testboxKeyPath(cleanupLease)
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
