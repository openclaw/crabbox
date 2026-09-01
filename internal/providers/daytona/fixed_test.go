package daytona

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	api "github.com/daytonaio/daytona/libs/api-client-go"
	core "github.com/openclaw/crabbox/internal/cli"
)

func newFixedDaytonaFixture(t *testing.T) (*daytonaLifecycleFixture, *daytonaLeaseBackend, AcquireRequest) {
	t.Helper()
	f, b, repo := newDaytonaFixture(t)
	capability, ok := any(b).(interface {
		SupportsRequestedLeaseID() bool
		SupportsRequestedCheckpointID() bool
	})
	if !ok || !capability.SupportsRequestedLeaseID() || !capability.SupportsRequestedCheckpointID() {
		t.Fatal("Daytona cannot replay fixed checkpoint forks")
	}
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX ssh executable fixture")
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	b.cfg.Daytona.OrganizationID = "org-test"
	original := f.server.Config.Handler
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/snapshots/"):
			snapshot := &api.SnapshotDto{Id: "snapshot-exact-id", Name: "test-snapshot", State: api.SNAPSHOTSTATE_ACTIVE, Entrypoint: []string{}}
			snapshot.SetOrganizationId("org-test")
			_ = json.NewEncoder(w).Encode(snapshot)
		case strings.HasSuffix(r.URL.Path, "/ssh-access"):
			u, _ := url.Parse(f.server.URL)
			now := time.Now().UTC()
			_ = json.NewEncoder(w).Encode(api.NewSshAccessDto("access-fixture", "sandbox-test", "synthetic-ssh-token", now.Add(time.Hour), now, now, "ssh -p "+u.Port()+" synthetic-ssh-token@127.0.0.1"))
		default:
			original.ServeHTTP(w, r)
		}
	})
	return f, b, AcquireRequest{Repo: repo, Keep: true, RequestedLeaseID: "cbx_012345abcdef", RequestedCheckpointID: "chk_0123456789abcdef", RequestedSlug: "fixed-project"}
}

func fixedDaytonaCreateCount(f *daytonaLifecycleFixture) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, p := range f.paths {
		if p == "POST /sandbox" {
			count++
		}
	}
	return count
}

func TestDaytonaFixedAcquisitionReplaysAndRetainsTerminalIdentity(t *testing.T) {
	f, b, req := newFixedDaytonaFixture(t)
	first, err := b.Acquire(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	second, err := b.Acquire(t.Context(), req)
	if err != nil || first.LeaseID != req.RequestedLeaseID || second.Server.CloudID != first.Server.CloudID || fixedDaytonaCreateCount(f) != 1 {
		t.Fatalf("fixed replay allocated another lease: first=%s second=%s creates=%d err=%v", first.LeaseID, second.LeaseID, fixedDaytonaCreateCount(f), err)
	}
	if f.create.GetSnapshot() != "snapshot-exact-id" {
		t.Fatal("creation must pin the immutable snapshot ID")
	}
	claim, ok, err := core.ReadLeaseClaimWithPresence(first.LeaseID)
	if err != nil || !ok || claim.FixedCreateIntent == nil || claim.FixedCreateIntent.CheckpointID != req.RequestedCheckpointID {
		t.Fatalf("checkpoint binding missing: %v", err)
	}
	encoded, _ := json.Marshal(claim)
	if strings.Contains(string(encoded), "test-credential") || strings.Contains(string(encoded), "synthetic-ssh-token") {
		t.Fatal("credentials persisted in fixed claim")
	}
	if err := b.ReleaseLease(t.Context(), ReleaseLeaseRequest{Lease: second}); err != nil {
		t.Fatal(err)
	}
	if err := b.Stop(t.Context(), StopRequest{ID: first.LeaseID}); err != nil {
		t.Fatalf("terminal stop must be idempotent: %v", err)
	}
	if _, err := b.Acquire(t.Context(), req); err == nil || fixedDaytonaCreateCount(f) != 1 || f.deletes != 1 {
		t.Fatalf("terminal lease reused or deleted twice: err=%v creates=%d deletes=%d", err, fixedDaytonaCreateCount(f), f.deletes)
	}
}

func TestDaytonaFixedReplayDoesNotNeedSourceSnapshot(t *testing.T) {
	f, b, req := newFixedDaytonaFixture(t)
	fork := core.NativeCheckpointForkRequest{Config: &b.cfg, Record: core.NativeCheckpointForkRecord{
		Kind: core.CheckpointKindDaytona, ImageID: "snapshot-exact-id", Name: "test-snapshot", Direct: true,
		Metadata: map[string]string{"api_url": b.cfg.Daytona.APIURL, "organization": "org-test", "checkpoint": req.RequestedCheckpointID,
			"source": "source-sandbox", "snapshot_id": "snapshot-exact-id", "work_root": b.cfg.Daytona.WorkRoot, "user": "daytona"},
	}}
	if err := (Provider{}).ApplyNativeCheckpointForkConfig(fork); err != nil {
		t.Fatal(err)
	}
	req.CheckpointSource = &fork.Record
	first, err := b.Acquire(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	original := f.server.Config.Handler
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/snapshots/") {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{}`)
			return
		}
		original.ServeHTTP(w, r)
	})
	if err := (Provider{}).ApplyNativeCheckpointForkConfig(fork); err != nil {
		t.Fatalf("fork configuration required the retired source: %v", err)
	}
	replayed, err := b.Acquire(t.Context(), req)
	if err != nil || replayed.Server.CloudID != first.Server.CloudID || fixedDaytonaCreateCount(f) != 1 {
		t.Fatalf("source snapshot retirement prevented exact sandbox replay: resource=%s creates=%d err=%v", replayed.Server.CloudID, fixedDaytonaCreateCount(f), err)
	}
	fork.Record.Name = "changed-record"
	if _, err := b.Acquire(t.Context(), req); err == nil || !strings.Contains(err.Error(), "checkpoint source does not match") || fixedDaytonaCreateCount(f) != 1 {
		t.Fatalf("replay accepted a different source record: %v", err)
	}
}

func TestDaytonaNativeForkAttestsSourceBeforeAllocation(t *testing.T) {
	for _, fixed := range []bool{false, true} {
		for _, drift := range []string{"none", "id", "name", "organization", "general", "state"} {
			t.Run(fmt.Sprintf("fixed=%t/%s", fixed, drift), func(t *testing.T) {
				f, b, req := newFixedDaytonaFixture(t)
				if !fixed {
					req.RequestedLeaseID, req.RequestedCheckpointID = "", ""
				}
				req.CheckpointSource = &core.NativeCheckpointForkRecord{ImageID: "snapshot-exact-id", Name: "test-snapshot",
					Metadata: map[string]string{"organization": "org-test"}}
				snapshot := &api.SnapshotDto{Id: "snapshot-exact-id", Name: "test-snapshot", State: api.SNAPSHOTSTATE_ACTIVE, Entrypoint: []string{}}
				snapshot.SetOrganizationId("org-test")
				switch drift {
				case "id":
					snapshot.Id = "replacement-id"
				case "name":
					snapshot.Name = "replacement-name"
				case "organization":
					snapshot.SetOrganizationId("other-org")
				case "general":
					snapshot.SetGeneral(true)
				case "state":
					snapshot.State = api.SNAPSHOTSTATE_PENDING
				}
				original := f.server.Config.Handler
				f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if strings.HasPrefix(r.URL.Path, "/snapshots/") {
						w.Header().Set("Content-Type", "application/json")
						_ = json.NewEncoder(w).Encode(snapshot)
						return
					}
					original.ServeHTTP(w, r)
				})
				_, err := b.Acquire(t.Context(), req)
				if drift == "none" {
					if err != nil || fixedDaytonaCreateCount(f) != 1 || f.create.GetSnapshot() != req.CheckpointSource.ImageID {
						t.Fatalf("exact source did not reach pinned allocation: creates=%d snapshot=%s err=%v", fixedDaytonaCreateCount(f), f.create.GetSnapshot(), err)
					}
				} else if err == nil || fixedDaytonaCreateCount(f) != 0 {
					t.Fatalf("unattested native source reached allocation: creates=%d err=%v", fixedDaytonaCreateCount(f), err)
				}
			})
		}
	}
}

func TestDaytonaFixedLostCreateResponseNeverResubmits(t *testing.T) {
	f, b, req := newFixedDaytonaFixture(t)
	f.lostCreate = true
	if _, err := b.Acquire(t.Context(), req); err == nil {
		t.Fatal("lost response must retain the unresolved attempt")
	}
	claim, exists, err := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
	if err != nil || !exists || claim.FixedCreateIntent == nil || len(claim.FixedCreateIntent.Attempt) == 0 || f.deletes != 0 {
		t.Fatal("lost response did not preserve the create attempt")
	}
	f.lostCreate = false
	restarted := &daytonaLeaseBackend{cfg: b.cfg, rt: b.rt}
	lease, err := restarted.Acquire(t.Context(), req)
	if err != nil || lease.LeaseID != req.RequestedLeaseID || fixedDaytonaCreateCount(f) != 1 {
		t.Fatalf("recovery allocated again: %v creates=%d", err, fixedDaytonaCreateCount(f))
	}
}

func TestDaytonaFixedStopFinalizesOnlyNeverSubmittedIntent(t *testing.T) {
	for _, state := range []string{"unsubmitted", "ambiguous", "held", "invalid"} {
		t.Run(state, func(t *testing.T) {
			f, b, req := newFixedDaytonaFixture(t)
			client, err := newDaytonaClient(b.cfg, b.rt)
			if err != nil {
				t.Fatal(err)
			}
			scope, _, err := fixedDaytonaContext(client)
			if err != nil {
				t.Fatal(err)
			}
			interrupted := errors.New("interrupted before provider attempt")
			_, err = core.AcquireFixedLease(core.FixedAcquireOptions{Kind: fixedDaytonaLeaseKind, LeaseID: req.RequestedLeaseID, CheckpointID: req.RequestedCheckpointID, RepoRoot: req.Repo.Root, TTL: b.cfg.TTL, IdleTimeout: b.cfg.IdleTimeout},
				func(context.Context, *core.LeaseClaim, bool) (core.FixedLeaseBinding, error) {
					return core.FixedLeaseBinding{ProviderScope: scope, Fingerprint: strings.Repeat("a", 64), Slug: req.RequestedSlug}, nil
				}, func(context.Context, *core.LeaseClaim, *core.FixedCreateIntent, func() error) (LeaseTarget, error) {
					return LeaseTarget{}, interrupted
				}, t.Context())
			if !errors.Is(err, interrupted) {
				t.Fatal(err)
			}
			claim, _, _ := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
			if state != "unsubmitted" {
				next := claim
				intent := *claim.FixedCreateIntent
				next.FixedCreateIntent = &intent
				switch state {
				case "ambiguous":
					intent.Attempt = map[string]string{"name": "possibly-created"}
				case "held":
					next.CheckpointCapture = &core.CheckpointCaptureBinding{ID: "chk_held", Revision: "held", BoundRevision: claim.Revision}
				case "invalid":
					intent.Version++
				}
				if _, err := core.ReplaceLeaseClaimIfUnchangedDurableReturning(claim.LeaseID, claim, next); err != nil {
					t.Fatal(err)
				}
			}
			factory := newDaytonaClient
			calls := 0
			newDaytonaClient = func(Config, Runtime) (daytonaAPI, error) {
				calls++
				return nil, errors.New("no provider credentials")
			}
			t.Cleanup(func() { newDaytonaClient = factory })
			err = b.Stop(t.Context(), StopRequest{ID: claim.LeaseID})
			after, exists, readErr := core.ReadLeaseClaimWithPresence(claim.LeaseID)
			if readErr != nil || !exists || len(f.paths) != 0 {
				t.Fatal("stop lost the durable receipt or called the provider")
			}
			if state == "unsubmitted" {
				if err != nil || calls != 0 || after.FixedCreateIntent.State != "released" {
					t.Fatalf("never-submitted intent could not be finalized without credentials: %v", err)
				}
				newDaytonaClient = factory
				if _, err := b.Acquire(t.Context(), req); err == nil || fixedDaytonaCreateCount(f) != 0 {
					t.Fatal("finalized ID allocated a replacement")
				}
			} else if err == nil || after.FixedCreateIntent.State != "prepared" || state == "held" && after.CheckpointCapture == nil {
				t.Fatal("ambiguous, invalid, or held intent was finalized")
			}
		})
	}
}

func TestDaytonaFixedReplayRejectsIntentAndResourceDrift(t *testing.T) {
	for _, drift := range []string{"checkpoint", "snapshot", "organization", "credential", "endpoint", "uuid", "labels", "missing"} {
		t.Run(drift, func(t *testing.T) {
			f, b, req := newFixedDaytonaFixture(t)
			if _, err := b.Acquire(t.Context(), req); err != nil {
				t.Fatal(err)
			}
			switch drift {
			case "checkpoint":
				req.RequestedCheckpointID = "chk_ffffffffffffffff"
			case "snapshot":
				b.cfg.Daytona.Snapshot = "different-snapshot"
			case "organization":
				b.cfg.Daytona.OrganizationID = "other-org"
			case "credential":
				b.cfg.Daytona.APIKey = "other-credential"
			case "endpoint":
				b.cfg.Daytona.APIURL += "/other-api"
			case "uuid":
				f.sandbox.SetId("replacement-sandbox")
			case "labels":
				f.sandbox.Labels["lease"] = "cbx_ffffffffffff"
			case "missing":
				original := f.server.Config.Handler
				f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if strings.HasPrefix(r.URL.Path, "/sandbox/") {
						w.WriteHeader(404)
						_, _ = io.WriteString(w, `{}`)
						return
					}
					original.ServeHTTP(w, r)
				})
			}
			if _, err := b.Acquire(t.Context(), req); err == nil || fixedDaytonaCreateCount(f) != 1 || f.deletes != 0 {
				t.Fatalf("unsafe replay: drift=%s err=%v creates=%d deletes=%d", drift, err, fixedDaytonaCreateCount(f), f.deletes)
			}
		})
	}
}

func TestDaytonaFixedRawIdentityStopRetainsReceipt(t *testing.T) {
	for _, byName := range []bool{false, true} {
		t.Run(map[bool]string{false: "uuid", true: "provider name"}[byName], func(t *testing.T) {
			f, b, req := newFixedDaytonaFixture(t)
			lease, err := b.Acquire(t.Context(), req)
			if err != nil {
				t.Fatal(err)
			}
			id := lease.Server.CloudID
			if byName {
				id = lease.Server.Name
			}
			if err := b.Stop(t.Context(), StopRequest{ID: id}); err != nil {
				t.Fatal(err)
			}
			claim, exists, err := core.ReadLeaseClaimWithPresence(lease.LeaseID)
			if err != nil || !exists || claim.FixedCreateIntent == nil || claim.FixedCreateIntent.State != "released" {
				t.Fatal("raw identity stop lost terminal receipt")
			}
			if _, err := b.Acquire(t.Context(), req); err == nil || fixedDaytonaCreateCount(f) != 1 {
				t.Fatal("raw identity stop allowed another create")
			}
		})
	}
}

func TestDaytonaFixedRawIdentityAttestsExactResourceAfterInventory(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		t.Run(fmt.Sprintf("replacement=%t", replacement), func(t *testing.T) {
			f, b, req := newFixedDaytonaFixture(t)
			lease, err := b.Acquire(t.Context(), req)
			if err != nil {
				t.Fatal(err)
			}
			original := f.server.Config.Handler
			f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" && r.URL.Path == "/sandbox" {
					w.Header().Set("Content-Type", "application/json")
					item := *f.sandbox
					item.Snapshot = nil
					if replacement {
						item.SetId("replacement-sandbox")
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"items": []api.Sandbox{item}, "nextCursor": nil})
					return
				}
				original.ServeHTTP(w, r)
			})
			lookup := lease.Server.CloudID
			if replacement {
				lookup = "replacement-sandbox"
			}
			view, err := b.Status(t.Context(), StatusRequest{ID: lookup})
			if replacement {
				if err == nil {
					t.Fatal("inventory UUID mismatch adopted the claimed sandbox")
				}
				return
			}
			if err != nil || view.ServerID != lease.Server.CloudID {
				t.Fatalf("partial inventory prevented exact resource attestation: %v", err)
			}
		})
	}
}

func TestDaytonaFixedAPIKeyDefaultOrganizationMatchesCheckpointHeader(t *testing.T) {
	_, b, req := newFixedDaytonaFixture(t)
	lease, err := b.Acquire(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	// Native checkpoint fork pins the source org header; ordinary API-key
	// commands can omit it while resolving the same provider organization.
	b.cfg.Daytona.OrganizationID = ""
	if _, err := b.Status(t.Context(), StatusRequest{ID: lease.LeaseID}); err != nil {
		t.Fatal(err)
	}
	if err := b.Stop(t.Context(), StopRequest{ID: lease.LeaseID}); err != nil {
		t.Fatal(err)
	}
}

func TestDaytonaFixedLifecycleRejectsWrongOrganizationBeforeAbsence(t *testing.T) {
	f, b, req := newFixedDaytonaFixture(t)
	lease, err := b.Acquire(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	b.cfg.Daytona.OrganizationID = "other-org"
	original := f.server.Config.Handler
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Daytona-Organization-Id") == "other-org" {
			w.WriteHeader(404)
			_, _ = io.WriteString(w, `{}`)
			return
		}
		original.ServeHTTP(w, r)
	})
	if err := b.Stop(t.Context(), StopRequest{ID: lease.LeaseID}); err == nil {
		t.Fatal("wrong organization absence authorized cleanup")
	}
	claim, exists, err := core.ReadLeaseClaimWithPresence(lease.LeaseID)
	if err != nil || !exists || claim.FixedCreateIntent.State != "acquired" || f.deletes != 0 {
		t.Fatal("wrong organization changed ownership")
	}
}

func TestDaytonaFixedReclaimCannotDowngradeOrphan(t *testing.T) {
	f, b, req := newFixedDaytonaFixture(t)
	lease, err := b.Acquire(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	core.RemoveLeaseClaim(lease.LeaseID)
	if _, err := b.Resolve(t.Context(), ResolveRequest{ID: lease.Server.CloudID, Repo: req.Repo, Reclaim: true}); err == nil {
		t.Fatal("orphan fixed resource was reclaimed as ordinary")
	}
	if _, err := b.Touch(t.Context(), TouchRequest{Lease: lease, State: "ready"}); err == nil {
		t.Fatal("orphan fixed resource was touched without authority")
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(lease.LeaseID); err != nil || exists || f.deletes != 0 {
		t.Fatal("orphan fixed claim was recreated")
	}
}

func TestDaytonaFixedReplayCannotResumeHeldCheckpointSource(t *testing.T) {
	f, b, req := newFixedDaytonaFixture(t)
	lease, err := b.Acquire(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	claim, _, _ := core.ReadLeaseClaimWithPresence(lease.LeaseID)
	held := claim
	held.CheckpointCapture = &core.CheckpointCaptureBinding{ID: "chk_held", Revision: "held", BoundRevision: claim.Revision}
	if _, err := core.ReplaceLeaseClaimIfUnchangedDurableReturning(lease.LeaseID, claim, held); err != nil {
		t.Fatal(err)
	}
	f.sandbox.SetState(api.SANDBOXSTATE_STOPPED)
	before := len(f.paths)
	if _, err := b.Acquire(t.Context(), req); err == nil {
		t.Fatal("held source was resumed by fixed replay")
	}
	for _, p := range f.paths[before:] {
		if !strings.HasPrefix(p, "GET ") {
			t.Fatalf("held source mutation: %s", p)
		}
	}
}

func TestDaytonaFixedReleaseRejectsChangedClaim(t *testing.T) {
	f, b, req := newFixedDaytonaFixture(t)
	lease, err := b.Acquire(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	claim, _, _ := core.ReadLeaseClaimWithPresence(lease.LeaseID)
	reclaimed := claim
	reclaimed.RepoRoot = t.TempDir()
	if _, err := core.ReplaceLeaseClaimIfUnchangedDurableReturning(lease.LeaseID, claim, reclaimed); err != nil {
		t.Fatal(err)
	}
	if err := b.ReleaseLease(t.Context(), ReleaseLeaseRequest{Lease: lease}); err == nil || f.deletes != 0 {
		t.Fatal("stale handle released a changed claim")
	}
}

func TestDaytonaFixedContextUsesTheHTTPClientCredentialSnapshot(t *testing.T) {
	_, b, req := newFixedDaytonaFixture(t)
	b.cfg.Daytona.APIKey = ""
	dir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "daytona", "config.json")
	if err := os.MkdirAll(filepath.Dir(configPath), 0700); err != nil {
		t.Fatal(err)
	}
	writeProfile := func(key string) {
		profile := daytonaCLIProfile{ID: "fixture", ActiveOrganizationID: "org-test"}
		profile.API.Key, profile.API.URL = key, b.cfg.Daytona.APIURL
		data, _ := json.Marshal(daytonaCLIConfig{ActiveProfile: "fixture", Profiles: []daytonaCLIProfile{profile}})
		if err := os.WriteFile(configPath, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeProfile("original-fixture-credential")
	factory := newDaytonaClient
	rotate := true
	newDaytonaClient = func(cfg Config, rt Runtime) (daytonaAPI, error) {
		client, err := factory(cfg, rt)
		if rotate {
			rotate = false
			writeProfile("rotated-fixture-credential")
		}
		return client, err
	}
	t.Cleanup(func() { newDaytonaClient = factory })
	lease, err := b.Acquire(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	writeProfile("original-fixture-credential")
	if _, err := b.Status(t.Context(), StatusRequest{ID: lease.LeaseID}); err != nil {
		t.Fatalf("claim was bound to a credential the creating client never used: %v", err)
	}
}

func TestDaytonaFixedHeartbeatUsesBoundProviderScope(t *testing.T) {
	f, b, req := newFixedDaytonaFixture(t)
	lease, err := b.Acquire(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	data := fmt.Sprintf("provider: daytona\ntarget: linux\ndaytona:\n  apiUrl: %q\n  apiKey: test-credential\n  organizationId: org-test\n  snapshot: test-snapshot\n", b.cfg.Daytona.APIURL)
	if err := os.WriteFile(configPath, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRABBOX_CONFIG", configPath)
	t.Setenv("CRABBOX_DAYTONA_API_KEY", "test-credential")
	t.Setenv("CRABBOX_DAYTONA_API_URL", b.cfg.Daytona.APIURL)
	var stdout, stderr bytes.Buffer
	if err := (core.App{Stdout: &stdout, Stderr: &stderr}).Run(t.Context(), []string{"heartbeat", "--provider", "daytona", "--id", lease.LeaseID, "--idle-timeout", "45m", "--json"}); err != nil {
		t.Fatal(err)
	}
	if f.autoStop != "45" || f.activity != 1 || !strings.Contains(stdout.String(), lease.LeaseID) {
		t.Fatalf("heartbeat did not update owned sandbox: %s", stdout.String())
	}
	if _, err := b.Status(t.Context(), StatusRequest{ID: lease.LeaseID}); err != nil {
		t.Fatalf("heartbeat destroyed fixed ownership: %v", err)
	}
	if _, err := b.Acquire(t.Context(), req); err != nil || fixedDaytonaCreateCount(f) != 1 {
		t.Fatalf("heartbeat prevented replay of the original allocation: %v", err)
	}
}

func TestDaytonaFixedHeldSourceRejectsReuseAndTouch(t *testing.T) {
	for _, op := range []string{"ssh-reclaim", "ssh-no-local-write", "run-reclaim", "touch"} {
		t.Run(op, func(t *testing.T) {
			f, b, req := newFixedDaytonaFixture(t)
			lease, err := b.Acquire(t.Context(), req)
			if err != nil {
				t.Fatal(err)
			}
			claim, _, _ := core.ReadLeaseClaimWithPresence(lease.LeaseID)
			held := claim
			held.CheckpointCapture = &core.CheckpointCaptureBinding{ID: "chk_held", Revision: "held", BoundRevision: claim.Revision}
			held, err = core.ReplaceLeaseClaimIfUnchangedDurableReturning(lease.LeaseID, claim, held)
			if err != nil {
				t.Fatal(err)
			}
			core.SetServerLeaseClaimSnapshot(&lease.Server, held, true)
			f.sandbox.SetState(api.SANDBOXSTATE_STOPPED)
			before := len(f.paths)
			switch op {
			case "ssh-reclaim", "ssh-no-local-write":
				_, err = b.Resolve(t.Context(), ResolveRequest{ID: lease.LeaseID, Repo: req.Repo, Reclaim: op == "ssh-reclaim", NoLocalStateMutations: op == "ssh-no-local-write"})
			case "run-reclaim":
				_, err = b.Run(t.Context(), RunRequest{ID: lease.LeaseID, Repo: req.Repo, Reclaim: true, NoSync: true, Command: []string{"true"}})
			case "touch":
				_, err = b.Touch(t.Context(), TouchRequest{Lease: lease, State: "ready"})
			}
			if err == nil {
				t.Fatal("held checkpoint source admitted use")
			}
			for _, p := range f.paths[before:] {
				if !strings.HasPrefix(p, "GET ") {
					t.Fatalf("held source mutation: %s", p)
				}
			}
			after, _, _ := core.ReadLeaseClaimWithPresence(lease.LeaseID)
			if after.CheckpointCapture == nil {
				t.Fatal("reuse cleared the checkpoint hold")
			}
			if _, err := b.Status(t.Context(), StatusRequest{ID: lease.LeaseID}); err != nil {
				t.Fatalf("read-only inspection must remain available: %v", err)
			}
		})
	}
}

func TestDaytonaFixedRejectedCreateResponsePinsObservedUUID(t *testing.T) {
	f, b, req := newFixedDaytonaFixture(t)
	original := f.server.Config.Handler
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/sandbox" {
			w.Header().Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			original.ServeHTTP(recorder, r)
			f.sandbox.SetUser("unexpected-user")
			_ = json.NewEncoder(w).Encode(f.sandbox)
			return
		}
		original.ServeHTTP(w, r)
	})
	if _, err := b.Acquire(t.Context(), req); err == nil {
		t.Fatal("mismatched create response was accepted")
	}
	claim, _, _ := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
	if claim.CloudID != "sandbox-test" || claim.CloudImmutableID != "" {
		t.Fatal("raw observed identity was discarded or treated as attested")
	}
	f.sandbox.SetUser("daytona")
	f.sandbox.SetId("replacement-sandbox")
	if _, err := b.Acquire(t.Context(), req); err == nil || fixedDaytonaCreateCount(f) != 1 {
		t.Fatal("failed validation allowed adoption of a different UUID")
	}
}

func TestDaytonaFixedResumeRejectsChangedClaim(t *testing.T) {
	for _, mutation := range []string{"missing", "ordinary", "prepared"} {
		t.Run(mutation, func(t *testing.T) {
			f, b, req := newFixedDaytonaFixture(t)
			lease, err := b.Acquire(t.Context(), req)
			if err != nil {
				t.Fatal(err)
			}
			client, err := newDaytonaClient(b.cfg, b.rt)
			if err != nil {
				t.Fatal(err)
			}
			sandbox, _, err := resolveDaytonaSandbox(t.Context(), client, b.cfg, lease.LeaseID)
			if err != nil {
				t.Fatal(err)
			}
			claim, _, _ := core.ReadLeaseClaimWithPresence(lease.LeaseID)
			switch mutation {
			case "missing":
				core.RemoveLeaseClaim(lease.LeaseID)
			default:
				next := claim
				if mutation == "ordinary" {
					next.FixedCreateIntent = nil
				} else {
					intent := *claim.FixedCreateIntent
					intent.State = "prepared"
					next.FixedCreateIntent = &intent
				}
				if _, err := core.ReplaceLeaseClaimIfUnchangedDurableReturning(lease.LeaseID, claim, next); err != nil {
					t.Fatal(err)
				}
			}
			sandbox.SetState(api.SANDBOXSTATE_STOPPED)
			before := len(f.paths)
			if _, err := ensureDaytonaRunning(t.Context(), client, sandbox, lease.LeaseID); err == nil || len(f.paths) != before {
				t.Fatal("resume admitted a replaced acquisition claim")
			}
		})
	}
}

func TestDaytonaSnapshotMetadataUsesCapturingClientContext(t *testing.T) {
	f := newSnapshotFixture(t)
	f.sandbox.SetState(api.SANDBOXSTATE_STOPPED)
	f.request.Config.Daytona.APIKey = ""
	f.request.Config.Daytona.APIURL = ""
	f.request.Config.Daytona.OrganizationID = ""
	dir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "daytona", "config.json")
	if err := os.MkdirAll(filepath.Dir(configPath), 0700); err != nil {
		t.Fatal(err)
	}
	writeProfile := func(endpoint, organization string) {
		profile := daytonaCLIProfile{ID: "fixture", ActiveOrganizationID: organization}
		profile.API.Key, profile.API.URL = "test-credential", endpoint
		data, _ := json.Marshal(daytonaCLIConfig{ActiveProfile: "fixture", Profiles: []daytonaCLIProfile{profile}})
		if err := os.WriteFile(configPath, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeProfile(f.server.URL, "org-test")
	factory := newDaytonaClient
	newDaytonaClient = func(cfg Config, rt Runtime) (daytonaAPI, error) {
		client, err := factory(cfg, rt)
		writeProfile("https://rotated.invalid", "rotated-org")
		return client, err
	}
	t.Cleanup(func() { newDaytonaClient = factory })
	result, err := (Provider{}).CreateNativeCheckpoint(t.Context(), f.request)
	if err != nil || result.Metadata["api_url"] != f.server.URL || result.Metadata["organization"] != "org-test" {
		t.Fatalf("snapshot metadata did not describe the capturing client: metadata=%v err=%v", result.Metadata, err)
	}
}
