package azuresandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func pinSnapshotFixture(t *testing.T, b *backend, change func(*snapshotBinding, map[string]any)) snapshotBinding {
	t.Helper()
	root := t.TempDir()
	runtime := preparedRuntime{RuntimeImage: "registry.example/image@sha256:" + strings.Repeat("a", 64), BuildID: "fixture-build", RuntimeVersion: "1.0.0", EnabledPluginIDs: []string{"fixture"}, NodeBootstrapSHA256: strings.Repeat("b", 64), WorkerBundleSHA256: strings.Repeat("c", 64), BundleHash: strings.Repeat("d", 64), CacheHitVerified: true}
	binding := snapshotBinding{Schema: "crabbox.azure-sandbox-snapshot.v1", ProviderScope: (Provider{}).ClaimScope(b.cfg), SnapshotID: "snapshot-id", SourceSandboxID: "clean-source", SourceDiskID: "disk-id", CapturedAt: "2026-01-01T00:00:00Z", ValidUntil: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano), VerifierFile: "verifier.json", Runtime: runtime}
	binding.Lifecycle.AutoSuspend.Enabled, binding.Lifecycle.AutoSuspend.Interval, binding.Lifecycle.AutoSuspend.Mode = true, int(b.cfg.IdleTimeout.Seconds()), "Disk"
	binding.Lifecycle.AutoDelete.Enabled, binding.Lifecycle.AutoDelete.Interval = true, int(b.cfg.TTL.Seconds())
	attestation := map[string]any{"schema": "crabbox.pre-enrollment-state.v1", "credentialEnvironmentAbsent": true, "credentialAndSessionPathsAbsent": true, "workerStateLimitedToPreparedCaches": true, "workspaceEmpty": true}
	if change != nil {
		change(&binding, attestation)
	}
	write := func(name string, value any) string {
		t.Helper()
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), data, 0600); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		return hex.EncodeToString(sum[:])
	}
	binding.VerifierSHA256 = write("verifier.json", map[string]any{"preEnrollment": attestation, "checkedCredentialPathsAbsent": true, "preparedRuntime": runtime})
	b.cfg.AzureSandbox.DiskID, b.cfg.AzureSandbox.SnapshotID = "disk-id", "snapshot-id"
	b.cfg.AzureSandbox.SnapshotReceipt = filepath.Join(root, "binding.json")
	b.cfg.AzureSandbox.SnapshotGeneration = write("binding.json", binding)
	return binding
}

func snapshotTransport(t *testing.T, b *backend, f *fixture, binding snapshotBinding, change func(*sandbox)) {
	b.api.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "/snapshots/") {
			data, _ := json.Marshal(snapshotMetadata{ID: binding.SnapshotID, SandboxID: binding.SourceSandboxID, CreatedAt: binding.CapturedAt})
			return response(200, string(data)), nil
		}
		if strings.Contains(r.URL.Path, "/diskimages/") {
			return response(200, `{"id":"disk-id","status":{"state":"Ready"}}`), nil
		}
		if r.Method == "PUT" {
			result, err := f.request(t, r)
			f.box.SourcesRef = sourceReferences{Snapshot: &snapshotReference{ID: binding.SnapshotID}, DiskImage: &diskReference{ID: binding.SourceDiskID}}
			f.box.Lifecycle = binding.Lifecycle
			if change != nil {
				change(f.box)
			}
			if f.loseCreateResponse {
				return result, err
			}
			data, _ := json.Marshal(f.box)
			return response(200, string(data)), nil
		}
		return f.request(t, r)
	})
}

func TestSnapshotFreshOwnershipReplayAndCleanup(t *testing.T) {
	f := &fixture{loseCreateResponse: true}
	b, req := fixtureBackend(t, f)
	binding := pinSnapshotFixture(t, b, nil)
	snapshotTransport(t, b, f, binding, nil)
	if _, err := b.acquire(t.Context(), req); err == nil {
		t.Fatal("lost create response was accepted")
	}
	if _, err := b.acquire(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if f.creates != 1 || f.created.SourcesRef.Snapshot.ID != binding.SnapshotID || f.created.SourcesRef.DiskImage != nil || f.created.Resources != nil || f.created.Lifecycle != nil {
		t.Fatal("restore request changed or replay created twice")
	}
	if f.box.Labels["crabbox_lease"] != req.RequestedLeaseID || f.box.Labels["crabbox_attempt"] == "" || len(f.box.Labels["crabbox_fingerprint"]) != 52 {
		t.Fatal("fresh ownership missing")
	}
	wire, _ := json.Marshal(f.created)
	for _, forbidden := range []string{"environment", "connections", "preEnrollment", "fixture-build", "clean-source"} {
		if strings.Contains(string(wire), forbidden) {
			t.Fatalf("restore payload contains %s", forbidden)
		}
	}
	// Cleanup depends on the persisted attempt, not receipt files or source availability.
	if err := os.Remove(b.cfg.AzureSandbox.SnapshotReceipt); err != nil {
		t.Fatal(err)
	}
	if err := b.Stop(t.Context(), core.StopRequest{ID: req.RequestedLeaseID}); err != nil {
		t.Fatal(err)
	}
	if f.deletes != 1 {
		t.Fatal("owned clone was not deleted exactly once")
	}
}

func TestSnapshotRejectsStaleOrUncleanBindingBeforeCreate(t *testing.T) {
	for _, scenario := range []string{"expired", "future-capture", "schema", "disk", "group", "runtime", "credentials", "sessions", "cache", "workspace", "generation", "lifecycle", "remote-snapshot"} {
		t.Run(scenario, func(t *testing.T) {
			f := &fixture{}
			b, req := fixtureBackend(t, f)
			binding := pinSnapshotFixture(t, b, func(s *snapshotBinding, a map[string]any) {
				switch scenario {
				case "expired":
					s.ValidUntil = "2000-01-01T00:00:00Z"
				case "future-capture":
					s.CapturedAt = time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339Nano)
				case "schema":
					a["schema"] = "unknown.v1"
				case "disk":
					s.SourceDiskID = "stale-disk"
				case "group":
					s.ProviderScope = "foreign"
				case "runtime":
					s.Runtime.BuildID = "stale-build"
				case "credentials":
					a["credentialEnvironmentAbsent"] = false
				case "sessions":
					a["credentialAndSessionPathsAbsent"] = false
				case "cache":
					a["workerStateLimitedToPreparedCaches"] = false
				case "workspace":
					a["workspaceEmpty"] = false
				case "lifecycle":
					s.Lifecycle.AutoSuspend.Mode = "Memory"
				}
			})
			if scenario == "generation" {
				b.cfg.AzureSandbox.SnapshotGeneration = strings.Repeat("0", 64)
			}
			if scenario == "remote-snapshot" {
				binding.SourceSandboxID = "different-source"
			}
			snapshotTransport(t, b, f, binding, nil)
			if _, err := b.acquire(t.Context(), req); err == nil || f.creates != 0 {
				t.Fatalf("invalid source created: %v creates=%d", err, f.creates)
			}
		})
	}
}

func TestSnapshotRestoreRefusesWrongReadbackWithoutReplacement(t *testing.T) {
	for _, scenario := range []string{"disk", "snapshot", "lifecycle"} {
		t.Run(scenario, func(t *testing.T) {
			f := &fixture{}
			b, req := fixtureBackend(t, f)
			binding := pinSnapshotFixture(t, b, nil)
			snapshotTransport(t, b, f, binding, func(s *sandbox) {
				switch scenario {
				case "disk":
					s.SourcesRef.DiskImage.ID = "wrong"
				case "snapshot":
					s.SourcesRef.Snapshot.ID = "wrong"
				case "lifecycle":
					s.Lifecycle.AutoDelete.Enabled = false
				}
			})
			for range 2 {
				if _, err := b.acquire(t.Context(), req); err == nil {
					t.Fatal("bad restore admitted")
				}
			}
			if f.creates != 1 || f.deletes != 0 {
				t.Fatal("uncertain restore replaced or destroyed")
			}
			// Invalid source readback must not strand an exactly owned clone.
			if err := os.Remove(b.cfg.AzureSandbox.SnapshotReceipt); err != nil {
				t.Fatal(err)
			}
			if err := b.Stop(t.Context(), core.StopRequest{ID: req.RequestedLeaseID}); err != nil {
				t.Fatal(err)
			}
			if err := b.Stop(t.Context(), core.StopRequest{ID: req.RequestedLeaseID}); err != nil {
				t.Fatal(err)
			}
			claim, err := b.claim(req.RequestedLeaseID)
			if err != nil || claim.FixedCreateIntent.State != "released" || f.box != nil || f.deletes != 1 {
				t.Fatalf("bad restore cleanup did not settle: err=%v deletes=%d", err, f.deletes)
			}
		})
	}
}

func TestSnapshotPrerequisiteFailurePreservesCreateAdmission(t *testing.T) {
	for _, failure := range []string{"snapshot-unavailable", "disk-unavailable", "disk-not-ready"} {
		for _, recoverBy := range []string{"retry", "stop"} {
			t.Run(failure+"/"+recoverBy, func(t *testing.T) {
				f := &fixture{}
				b, req := fixtureBackend(t, f)
				binding := pinSnapshotFixture(t, b, nil)
				snapshotTransport(t, b, f, binding, nil)
				inner := b.api.http.Transport
				unhealthy := true
				b.api.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
					if unhealthy {
						if failure == "snapshot-unavailable" && strings.Contains(r.URL.Path, "/snapshots/") {
							return response(503, ""), nil
						}
						if strings.Contains(r.URL.Path, "/diskimages/") {
							switch failure {
							case "disk-unavailable":
								return response(503, ""), nil
							case "disk-not-ready":
								return response(200, `{"id":"disk-id","status":{"state":"Creating"}}`), nil
							}
						}
					}
					return inner.RoundTrip(r)
				})
				if _, err := b.acquire(t.Context(), req); err == nil || f.creates != 0 {
					t.Fatalf("failed prerequisite allocated: err=%v creates=%d", err, f.creates)
				}
				before, err := b.claim(req.RequestedLeaseID)
				if err != nil {
					t.Fatal(err)
				}
				attempt := before.FixedCreateIntent.Attempt
				if attempt["submission"] != "pending" || before.FixedCreateIntent.Journal.Phase != "prepared" || attempt["nonce"] == "" {
					t.Fatal("read-only prerequisite consumed create admission")
				}
				unhealthy = false
				wantDeletes := 0
				if recoverBy == "retry" {
					if _, err := b.acquire(t.Context(), req); err != nil {
						t.Fatal(err)
					}
					after, err := b.claim(req.RequestedLeaseID)
					if err != nil {
						t.Fatal(err)
					}
					if f.creates != 1 || after.FixedCreateIntent.Attempt["nonce"] != attempt["nonce"] || after.FixedCreateIntent.Attempt["expires_at"] != attempt["expires_at"] || after.FixedCreateIntent.Attempt["submission"] != "submitted" {
						t.Fatal("retry replaced the original attempt or failed to record submission")
					}
					wantDeletes = 1
				}
				if err := b.Stop(t.Context(), core.StopRequest{ID: req.RequestedLeaseID}); err != nil {
					t.Fatal(err)
				}
				if err := b.Stop(t.Context(), core.StopRequest{ID: req.RequestedLeaseID}); err != nil {
					t.Fatal(err)
				}
				claim, err := b.claim(req.RequestedLeaseID)
				if err != nil || claim.FixedCreateIntent.State != "released" || f.box != nil || f.deletes != wantDeletes {
					t.Fatalf("prerequisite recovery did not settle: err=%v deletes=%d", err, f.deletes)
				}
			})
		}
	}
}

func TestSnapshotWrongSourceStillRequiresExactOwnerForCleanup(t *testing.T) {
	for _, label := range []string{"crabbox_lease", "crabbox_fingerprint", "crabbox_attempt"} {
		t.Run(label, func(t *testing.T) {
			f := &fixture{}
			b, req := fixtureBackend(t, f)
			binding := pinSnapshotFixture(t, b, nil)
			snapshotTransport(t, b, f, binding, nil)
			if _, err := b.acquire(t.Context(), req); err != nil {
				t.Fatal(err)
			}
			f.box.SourcesRef.Snapshot.ID = "wrong"
			f.box.Labels[label] = "foreign-owner"
			if err := b.Stop(t.Context(), core.StopRequest{ID: req.RequestedLeaseID}); err == nil || f.deletes != 0 {
				t.Fatalf("foreign restore was deleted: err=%v deletes=%d", err, f.deletes)
			}
		})
	}
}

func TestSnapshotLifecycleUnknownOutcomeIsNeverReposted(t *testing.T) {
	for _, applied := range []bool{false, true} {
		t.Run(map[bool]string{false: "unconfirmed", true: "applied-response-lost"}[applied], func(t *testing.T) {
			f := &fixture{}
			b, req := fixtureBackend(t, f)
			binding := pinSnapshotFixture(t, b, nil)
			snapshotTransport(t, b, f, binding, func(s *sandbox) { s.Lifecycle = snapshotLifecycle{} })
			inner := b.api.http.Transport
			posts := 0
			b.api.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Path, "/lifecycle") {
					posts++
					var policy snapshotLifecycle
					if json.NewDecoder(r.Body).Decode(&policy) != nil || policy != binding.Lifecycle {
						t.Fatal("wrong lifecycle")
					}
					if applied {
						f.box.Lifecycle = policy
					}
					return response(503, ""), nil
				}
				return inner.RoundTrip(r)
			})
			if _, err := b.acquire(t.Context(), req); err == nil {
				t.Fatal("lost setter response accepted")
			}
			_, err := b.acquire(t.Context(), req)
			if (err == nil) != applied || posts != 1 || f.creates != 1 {
				t.Fatalf("reconciliation err=%v posts=%d creates=%d", err, posts, f.creates)
			}
			if err := b.Stop(t.Context(), core.StopRequest{ID: req.RequestedLeaseID}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSnapshotConfigPreservesDiskOnlyFingerprint(t *testing.T) {
	config := core.AzureSandboxConfig{Subscription: "s", ResourceGroup: "r", Group: "g", Region: "westus3", Disk: "ubuntu", DiskID: "disk"}
	legacy := struct{ Subscription, ResourceGroup, Group, Region, ClientID, Disk, DiskID string }{config.Subscription, config.ResourceGroup, config.Group, config.Region, config.ClientID, config.Disk, config.DiskID}
	a, err := core.FixedIntentFingerprint("fixture", config)
	if err != nil {
		t.Fatal(err)
	}
	b, err := core.FixedIntentFingerprint("fixture", legacy)
	if err != nil || a != b {
		t.Fatal("new optional config changed disk-only persisted fingerprint")
	}
}
