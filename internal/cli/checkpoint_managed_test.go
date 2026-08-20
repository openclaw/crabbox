package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func managedCheckpointFixture(id string) coordinatorCheckpoint {
	checkpoint := coordinatorCheckpoint{
		ID:         id,
		Owner:      "alice@example.com",
		LeaseID:    "cbx_000000000001",
		Provider:   "aws",
		Scope:      coordinatorCheckpointScope{Region: "eu-west-1", AccountID: "123456789012"},
		Name:       "prepared-workspace",
		Strategy:   checkpointStrategyDiskSnapshot,
		NoReboot:   true,
		Image:      &coordinatorCheckpointImage{ID: "snap-0001", ResourceID: "snap-0001", Kind: checkpointKindAWSEBS, ImmutableID: "snap-0001", State: "available", SnapshotIDs: []string{"snap-0001"}},
		State:      "ready",
		Retention:  coordinatorCheckpointRetention{Mode: "manual"},
		Generation: 1,
		CreatedAt:  "2026-08-20T12:00:00Z",
		LastUsedAt: "2026-08-20T12:00:00Z",
		Target:     targetLinux,
		ServerType: "t3.small",
		Workdir:    "/work/source/my-app",
	}
	checkpoint.Repo.Name = "my-app"
	return checkpoint
}

func configureManagedCheckpointTest(t *testing.T, handler http.HandlerFunc) (*httptest.Server, checkpointStore) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	t.Setenv("CRABBOX_COORDINATOR", server.URL)
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "test-user-session")
	t.Setenv("CRABBOX_COORDINATOR_ADMIN_TOKEN", "")
	t.Setenv("CRABBOX_OWNER", "alice@example.com")
	t.Setenv("CRABBOX_ORG", "example-org")
	store, err := defaultCheckpointStore()
	if err != nil {
		t.Fatal(err)
	}
	return server, store
}

func TestCheckpointManagedListMergesRemoteInventoryAndKeepsJSONArray(t *testing.T) {
	checkpoint := managedCheckpointFixture("chk_remote_list")
	_, store := configureManagedCheckpointTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/checkpoints" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"checkpoints": []coordinatorCheckpoint{checkpoint}})
	})
	if _, err := store.Create(checkpointRecord{
		ID:        "chk_local_archive",
		Kind:      checkpointKindArchive,
		CreatedAt: "2026-08-19T12:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: io.Discard}
	if err := app.checkpointList(context.Background(), []string{"--json"}); err != nil {
		t.Fatal(err)
	}
	var records []checkpointRecord
	if err := json.Unmarshal(stdout.Bytes(), &records); err != nil {
		t.Fatalf("list did not preserve its bare JSON array: %v", err)
	}
	if len(records) != 2 || records[0].ID != checkpoint.ID || records[1].ID != "chk_local_archive" {
		t.Fatalf("merged inventory = %#v", records)
	}
	if !records[0].coordinatorManaged() || records[1].coordinatorManaged() {
		t.Fatalf("ownership boundary = %#v", records)
	}
	if _, _, err := store.Read(checkpoint.ID); err != nil {
		t.Fatalf("remote record was not hydrated: %v", err)
	}
}

func TestCheckpointManagedListLocalOnlyDoesNotContactCoordinator(t *testing.T) {
	calls := 0
	_, store := configureManagedCheckpointTest(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Error(w, "coordinator must not be contacted", http.StatusInternalServerError)
	})
	if _, err := store.Create(checkpointRecord{
		ID:        "chk_offline",
		Kind:      checkpointKindArchive,
		CreatedAt: "2026-08-20T12:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	if err := (App{Stdout: &stdout, Stderr: io.Discard}).checkpointList(context.Background(), []string{"--local-only", "--json"}); err != nil {
		t.Fatal(err)
	}
	if calls != 0 || !strings.Contains(stdout.String(), "chk_offline") {
		t.Fatalf("calls=%d output=%q", calls, stdout.String())
	}
}

func TestCheckpointManagedListPreservesLegacyCoordinatorCompatibility(t *testing.T) {
	_, store := configureManagedCheckpointTest(t, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	if _, err := store.Create(checkpointRecord{
		ID:        "chk_legacy",
		Kind:      checkpointKindAWSEBS,
		CreatedAt: "2026-08-20T12:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	if err := (App{Stdout: &stdout, Stderr: io.Discard}).checkpointList(context.Background(), []string{"--json"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "chk_legacy") {
		t.Fatalf("legacy checkpoint hidden: %q", stdout.String())
	}
}

func TestCheckpointManagedRemoteOnlyInspectHydratesCache(t *testing.T) {
	checkpoint := managedCheckpointFixture("chk_remote_inspect")
	_, store := configureManagedCheckpointTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/checkpoints/"+checkpoint.ID {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"checkpoint": checkpoint})
	})
	var stdout bytes.Buffer
	if err := (App{Stdout: &stdout, Stderr: io.Discard}).checkpointInspect(context.Background(), []string{checkpoint.ID, "--json"}); err != nil {
		t.Fatal(err)
	}
	var record checkpointRecord
	if err := json.Unmarshal(stdout.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	if record.ID != checkpoint.ID || !record.coordinatorManaged() {
		t.Fatalf("hydrated record = %#v", record)
	}
	if _, _, err := store.Read(checkpoint.ID); err != nil {
		t.Fatal(err)
	}
}

func TestCheckpointManagedDeleteRetainsCacheUntilCoordinatorConfirms(t *testing.T) {
	checkpoint := managedCheckpointFixture("chk_delete_managed")
	deleteCalls := 0
	server, store := configureManagedCheckpointTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/checkpoints/"+checkpoint.ID {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{"checkpoint": checkpoint})
			return
		}
		deleteCalls++
		if deleteCalls == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "checkpoint_delete_failed"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"checkpointID": checkpoint.ID, "deleted": true})
	})
	record, err := checkpointRecordFromCoordinator(checkpoint, checkpointCoordinatorOrigin(server.URL))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Write(record); err != nil {
		t.Fatal(err)
	}
	app := App{Stdout: io.Discard, Stderr: io.Discard}
	if err := app.checkpointDelete(context.Background(), []string{checkpoint.ID}); err == nil {
		t.Fatal("provider deletion failure was accepted")
	}
	if _, _, err := store.Read(checkpoint.ID); err != nil {
		t.Fatalf("recovery cache removed after provider failure: %v", err)
	}
	if err := app.checkpointDelete(context.Background(), []string{checkpoint.ID}); err != nil {
		t.Fatal(err)
	}
	paths, err := store.Paths(checkpoint.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(paths.Meta); !os.IsNotExist(err) {
		t.Fatalf("metadata remains after confirmed deletion: %v", err)
	}
}

func TestCheckpointManagedPolicyUpdatesRemoteOnlyRecords(t *testing.T) {
	checkpoint := managedCheckpointFixture("chk_remote_policy")
	_, store := configureManagedCheckpointTest(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/checkpoints/"+checkpoint.ID:
			_ = json.NewEncoder(w).Encode(map[string]any{"checkpoint": checkpoint})
		case r.Method == http.MethodPatch && r.URL.Path == "/v1/checkpoints/"+checkpoint.ID+"/retention":
			var body struct {
				Retention coordinatorCheckpointRetention `json:"retention"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				return
			}
			checkpoint.Retention = body.Retention
			_ = json.NewEncoder(w).Encode(map[string]any{"checkpoint": checkpoint})
		default:
			http.NotFound(w, r)
		}
	})
	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: io.Discard}
	if err := app.checkpointPolicy(context.Background(), []string{checkpoint.ID, "--expire-unused-after", "7d", "--json"}); err != nil {
		t.Fatal(err)
	}
	if checkpoint.Retention.Mode != "expire-unused" || checkpoint.Retention.UnusedForSeconds != 7*24*60*60 {
		t.Fatalf("retention = %#v", checkpoint.Retention)
	}
	record, _, err := store.Read(checkpoint.ID)
	if err != nil || record.Retention == nil || record.Retention.Mode != "expire-unused" {
		t.Fatalf("cached policy record = %#v, err=%v", record, err)
	}
}

func TestCheckpointManagedRejectsInvalidRetentionAndOperatorOwnedPolicies(t *testing.T) {
	for _, value := range []string{"0s", "-1h", "100ms", "4000d", "invalid"} {
		t.Run(value, func(t *testing.T) {
			if _, err := parseCheckpointRetentionDuration(value); err == nil {
				t.Fatalf("accepted retention %q", value)
			}
		})
	}
	t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	for _, mode := range [][]string{
		{"--mode", "archive", "--expire-unused-after", "1h"},
		{"--recipe-only", "--expire-unused-after", "1h"},
	} {
		if err := (App{Stdout: io.Discard, Stderr: io.Discard}).checkpointCreate(context.Background(), mode); err == nil || !strings.Contains(err.Error(), "brokered native") {
			t.Fatalf("args=%v error=%v", mode, err)
		}
	}
	_, store := configureManagedCheckpointTest(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("operator-owned checkpoint unexpectedly contacted coordinator: %s", r.URL.Path)
	})
	if _, err := store.Create(checkpointRecord{ID: "chk_operator", Kind: checkpointKindArchive, CreatedAt: time.Now().UTC().Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	if err := (App{Stdout: io.Discard, Stderr: io.Discard}).checkpointPolicy(context.Background(), []string{"chk_operator", "--manual"}); err == nil || !strings.Contains(err.Error(), "operator-managed") {
		t.Fatalf("policy error = %v", err)
	}
}

func TestCheckpointManagedUseClaimsAndLeaseRouteStayRequestScoped(t *testing.T) {
	checkpoint := managedCheckpointFixture("chk_fork_claim")
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	claimValue := hex.EncodeToString(random)
	var mu sync.Mutex
	actions := []string{}
	server, _ := configureManagedCheckpointTest(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/checkpoints/" + checkpoint.ID + "/use":
			var body struct {
				Action string `json:"action"`
				Claim  string `json:"claim"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				return
			}
			if body.Action != "begin" && body.Claim != claimValue {
				t.Errorf("unexpected checkpoint claim for action %s", body.Action)
			}
			mu.Lock()
			actions = append(actions, body.Action)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"checkpoint": checkpoint,
				"claim":      claimValue,
				"expiresAt":  time.Now().Add(2 * time.Minute).Format(time.RFC3339),
			})
		case "/v1/leases/from-checkpoint":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				return
			}
			if body["checkpointID"] != checkpoint.ID || body["checkpointUseClaim"] != claimValue {
				t.Error("checkpoint-backed lease did not carry its exact request-local use claim")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{ID: "cbx_000000000002", Provider: "aws"}})
		default:
			http.NotFound(w, r)
		}
	})
	coord := &CoordinatorClient{BaseURL: server.URL, Client: server.Client()}
	claim, err := coord.BeginCheckpointUse(context.Background(), checkpoint.ID)
	if err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig()
	cfg.Provider = "aws"
	cfg.AWSSnapshot = checkpoint.Image.ID
	if _, err := coord.CreateLease(withCheckpointLeaseClaim(context.Background(), checkpoint.ID, claim.Claim), cfg, "ssh-ed25519 public", false, "cbx_000000000002", "fork"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := coord.checkpointUseAction(context.Background(), checkpoint.ID, claim.Claim, "renew"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := coord.checkpointUseAction(context.Background(), checkpoint.ID, claim.Claim, "complete"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(actions, ",") != "begin,renew,complete" {
		t.Fatalf("actions = %v", actions)
	}
}

func TestCheckpointManagedLeaseRouteFailsClosedOnOlderCoordinator(t *testing.T) {
	server, _ := configureManagedCheckpointTest(t, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	coord := &CoordinatorClient{BaseURL: server.URL, Client: server.Client()}
	cfg := defaultConfig()
	cfg.Provider = "aws"
	cfg.AWSSnapshot = "snap-0001"
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		t.Fatal(err)
	}
	_, err := coord.CreateLease(withCheckpointLeaseClaim(context.Background(), "chk_legacy_route", hex.EncodeToString(tokenBytes)), cfg, "ssh-ed25519 public", false, "cbx_000000000002", "fork")
	if err == nil || !strings.Contains(err.Error(), "upgrade the coordinator") {
		t.Fatalf("older coordinator error = %v", err)
	}
}

func TestCheckpointManagedRejectsDifferentCoordinatorOrigin(t *testing.T) {
	checkpoint := managedCheckpointFixture("chk_wrong_origin")
	_, store := configureManagedCheckpointTest(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("wrong-origin checkpoint contacted coordinator: %s", r.URL.Path)
	})
	record, err := checkpointRecordFromCoordinator(checkpoint, "https://other.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Write(record); err != nil {
		t.Fatal(err)
	}
	if err := (App{Stdout: io.Discard, Stderr: io.Discard}).checkpointDelete(context.Background(), []string{record.ID}); err == nil || !strings.Contains(err.Error(), "belongs to coordinator") {
		t.Fatalf("origin mismatch error = %v", err)
	}
	if _, _, err := store.Read(record.ID); err != nil {
		t.Fatalf("wrong-origin recovery metadata was removed: %v", err)
	}
}

func TestCheckpointManagedEmptyLocalOnlyJSONIsArray(t *testing.T) {
	configureManagedCheckpointTest(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("local-only inventory contacted coordinator: %s", r.URL.Path)
	})
	var stdout bytes.Buffer
	if err := (App{Stdout: &stdout, Stderr: io.Discard}).checkpointList(context.Background(), []string{"--local-only", "--json"}); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(stdout.String()) != "[]" {
		t.Fatalf("empty local inventory = %q, want []", stdout.String())
	}
}

func TestCheckpointManagedListRejectsArchiveIdentityCollision(t *testing.T) {
	checkpoint := managedCheckpointFixture("chk_archive_collision")
	_, store := configureManagedCheckpointTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/checkpoints" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"checkpoints": []coordinatorCheckpoint{checkpoint}})
	})
	if _, err := store.Create(checkpointRecord{ID: checkpoint.ID, Kind: checkpointKindArchive}); err != nil {
		t.Fatal(err)
	}
	err := (App{Stdout: io.Discard, Stderr: io.Discard}).checkpointList(context.Background(), []string{"--json"})
	if err == nil || !strings.Contains(err.Error(), "collision") {
		t.Fatalf("archive collision error = %v", err)
	}
	local, _, err := store.Read(checkpoint.ID)
	if err != nil || local.Kind != checkpointKindArchive {
		t.Fatalf("operator archive overwritten: %#v, %v", local, err)
	}
}

func TestCheckpointManagedPruneIgnoresStaleManagedCache(t *testing.T) {
	checkpoint := managedCheckpointFixture("chk_stale_cache")
	server, store := configureManagedCheckpointTest(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("local pruning contacted coordinator: %s", r.URL.Path)
	})
	record, err := checkpointRecordFromCoordinator(checkpoint, checkpointCoordinatorOrigin(server.URL))
	if err != nil {
		t.Fatal(err)
	}
	record.LastUsedAt = "2000-01-01T00:00:00Z"
	if err := store.Write(record); err != nil {
		t.Fatal(err)
	}
	if err := (App{Stdout: io.Discard, Stderr: io.Discard}).checkpointPrune(context.Background(), []string{"--unused-for", "1h"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Read(record.ID); err != nil {
		t.Fatalf("managed cache removed by local prune: %v", err)
	}
}

func TestCheckpointManagedCorruptCacheDoesNotBlockRemoteInspect(t *testing.T) {
	checkpoint := managedCheckpointFixture("chk_corrupt_cache")
	_, store := configureManagedCheckpointTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/checkpoints/"+checkpoint.ID {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"checkpoint": checkpoint})
	})
	paths, err := store.Paths(checkpoint.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.Meta, []byte("not valid checkpoint metadata"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if err := (App{Stdout: &stdout, Stderr: &stderr}).checkpointInspect(context.Background(), []string{checkpoint.ID, "--json"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), checkpoint.ID) || !strings.Contains(stderr.String(), "warning") {
		t.Fatalf("inspect output=%q warning=%q", stdout.String(), stderr.String())
	}
	contents, err := os.ReadFile(paths.Meta)
	if err != nil || string(contents) != "not valid checkpoint metadata" {
		t.Fatalf("corrupt user metadata overwritten: %q, %v", contents, err)
	}
}

func TestCheckpointManagedUnwritableCacheDoesNotBlockRemoteInspect(t *testing.T) {
	checkpoint := managedCheckpointFixture("chk_unwritable_cache")
	_, store := configureManagedCheckpointTest(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"checkpoint": checkpoint})
	})
	if err := os.MkdirAll(filepath.Dir(store.root), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.root, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	if err := (App{Stdout: io.Discard, Stderr: &stderr}).checkpointInspect(context.Background(), []string{checkpoint.ID}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "warning") {
		t.Fatalf("missing best-effort cache warning: %q", stderr.String())
	}
}

func TestCheckpointManagedDeleteRetriesTombstoneWithoutPreflightGet(t *testing.T) {
	checkpoint := managedCheckpointFixture("chk_lost_delete_response")
	deletes := 0
	server, store := configureManagedCheckpointTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/v1/checkpoints/"+checkpoint.ID {
			t.Errorf("unexpected preflight request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		deletes++
		_ = json.NewEncoder(w).Encode(map[string]any{"checkpointID": checkpoint.ID, "deleted": true})
	})
	record, err := checkpointRecordFromCoordinator(checkpoint, checkpointCoordinatorOrigin(server.URL))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Write(record); err != nil {
		t.Fatal(err)
	}
	if err := (App{Stdout: io.Discard, Stderr: io.Discard}).checkpointDelete(context.Background(), []string{checkpoint.ID}); err != nil {
		t.Fatal(err)
	}
	if deletes != 1 {
		t.Fatalf("delete requests = %d, want 1", deletes)
	}
}

func TestCheckpointManagedCurrentCoordinatorPreservesResourceNotFound(t *testing.T) {
	_, _ = configureManagedCheckpointTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/checkpoints" {
			_ = json.NewEncoder(w).Encode(map[string]any{"checkpoints": []coordinatorCheckpoint{}})
			return
		}
		http.NotFound(w, r)
	})
	err := (App{Stdout: io.Discard, Stderr: io.Discard}).checkpointInspect(context.Background(), []string{"chk_hidden_owner"})
	if err == nil || strings.Contains(err.Error(), "upgrade the coordinator") {
		t.Fatalf("current owner-scoped missing checkpoint misclassified: %v", err)
	}
}

func TestCheckpointManagedMixedCredentialsRequireExplicitAdmin(t *testing.T) {
	checkpoint := managedCheckpointFixture("chk_admin_owned")
	_, _ = configureManagedCheckpointTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/checkpoints" {
			_ = json.NewEncoder(w).Encode(map[string]any{"checkpoints": []coordinatorCheckpoint{}})
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-admin-session" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"checkpoint": checkpoint})
	})
	t.Setenv("CRABBOX_COORDINATOR_ADMIN_TOKEN", "test-admin-session")
	app := App{Stdout: io.Discard, Stderr: io.Discard}
	if err := app.checkpointInspect(context.Background(), []string{checkpoint.ID}); err == nil {
		t.Fatal("ordinary credential silently fell back to privileged admin access")
	}
	if err := app.checkpointInspect(context.Background(), []string{checkpoint.ID, "--admin"}); err != nil {
		t.Fatalf("explicit admin access failed: %v", err)
	}
}

func TestCheckpointManagedPendingInventoryDoesNotHideHealthyRecords(t *testing.T) {
	healthy := managedCheckpointFixture("chk_inventory_healthy")
	pending := managedCheckpointFixture("chk_inventory_failed")
	pending.State = "failed"
	pending.Image = nil
	configureManagedCheckpointTest(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"checkpoints": []coordinatorCheckpoint{healthy, pending}})
	})
	var stdout bytes.Buffer
	if err := (App{Stdout: &stdout, Stderr: io.Discard}).checkpointList(context.Background(), []string{"--json"}); err != nil {
		t.Fatal(err)
	}
	var records []checkpointRecord
	if err := json.Unmarshal(stdout.Bytes(), &records); err != nil || len(records) != 2 {
		t.Fatalf("mixed failed/healthy inventory = %q, %v", stdout.String(), err)
	}
}

func TestCheckpointManagedPromotionRetirementNeverDeletesProviderImage(t *testing.T) {
	calls := 0
	configureManagedCheckpointTest(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodDelete || r.URL.Path != "/v1/images/ami-owned/promote" {
			t.Errorf("unsafe image mutation request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("region") != "eu-west-1" {
			t.Errorf("retirement region = %q", r.URL.Query().Get("region"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"imageID": "ami-owned", "retired": 3})
	})
	t.Setenv("CRABBOX_COORDINATOR_ADMIN_TOKEN", "test-admin-session")
	var stdout bytes.Buffer
	if err := (App{Stdout: &stdout, Stderr: io.Discard}).imageDelete(context.Background(), []string{"ami-owned", "--retire-promotions", "--region", "eu-west-1"}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || !strings.Contains(stdout.String(), "retired") {
		t.Fatalf("calls=%d output=%q", calls, stdout.String())
	}
}

func TestCheckpointManagedPromotionRetirementFailsClosedOnOlderCoordinator(t *testing.T) {
	calls := 0
	configureManagedCheckpointTest(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.NotFound(w, r)
	})
	t.Setenv("CRABBOX_COORDINATOR_ADMIN_TOKEN", "test-admin-session")
	err := (App{Stdout: io.Discard, Stderr: io.Discard}).imageDelete(context.Background(), []string{"ami-owned", "--retire-promotions"})
	if err == nil || !strings.Contains(err.Error(), "upgrade") || calls != 1 {
		t.Fatalf("older coordinator retirement error=%v calls=%d", err, calls)
	}
}

func TestCheckpointManagedAdminOnlyConfigurationUsesOneEffectiveCredential(t *testing.T) {
	checkpoint := managedCheckpointFixture("chk_admin_only")
	configureManagedCheckpointTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-admin-session" {
			t.Errorf("checkpoint request used another credential: %q", r.Header.Get("Authorization"))
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"checkpoint": checkpoint})
	})
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "")
	t.Setenv("CRABBOX_COORDINATOR_ADMIN_TOKEN", "test-admin-session")
	if err := (App{Stdout: io.Discard, Stderr: io.Discard}).checkpointInspect(context.Background(), []string{checkpoint.ID}); err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig()
	if err := bindCheckpointCoordinatorCredential(context.Background(), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.CoordToken != "test-admin-session" {
		t.Fatalf("lease provisioning did not retain the checkpoint principal: %q", cfg.CoordToken)
	}
}

func TestCheckpointManagedCoordinatorIdentityIncludesNormalizedBasePath(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  string
	}{
		{"https://example.test/a/", "https://example.test/a"},
		{"https://example.test/a/../b?ignored=yes#fragment", "https://example.test/b"},
		{"https://example.test:8443/nested/coordinator/", "https://example.test:8443/nested/coordinator"},
	} {
		if got := checkpointCoordinatorOrigin(tc.input); got != tc.want {
			t.Errorf("checkpointCoordinatorOrigin(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestCheckpointManagedRejectsSameHostDifferentCoordinatorBasePath(t *testing.T) {
	checkpoint := managedCheckpointFixture("chk_foreign_base_path")
	server, store := configureManagedCheckpointTest(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("foreign deployment was contacted: %s %s", r.Method, r.URL.Path)
	})
	t.Setenv("CRABBOX_COORDINATOR", server.URL+"/deployment-b")
	record, err := checkpointRecordFromCoordinator(checkpoint, checkpointCoordinatorOrigin(server.URL+"/deployment-a"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Write(record); err != nil {
		t.Fatal(err)
	}
	app := App{Stdout: io.Discard, Stderr: io.Discard}
	for _, operation := range []func() error{
		func() error { return app.checkpointInspect(context.Background(), []string{record.ID}) },
		func() error { return app.checkpointDelete(context.Background(), []string{record.ID}) },
	} {
		if err := operation(); err == nil || !strings.Contains(err.Error(), "belongs to coordinator") {
			t.Fatalf("same-host deployment mismatch = %v", err)
		}
	}
}

func TestCheckpointManagedPolicyPreservesUnreadableCache(t *testing.T) {
	checkpoint := managedCheckpointFixture("chk_corrupt_policy_cache")
	_, store := configureManagedCheckpointTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			checkpoint.Retention = coordinatorCheckpointRetention{Mode: "manual"}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"checkpoint": checkpoint})
	})
	paths, err := store.Paths(checkpoint.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	const corrupt = "preserve this unreadable checkpoint cache"
	if err := os.WriteFile(paths.Meta, []byte(corrupt), 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	if err := (App{Stdout: io.Discard, Stderr: &stderr}).checkpointPolicy(context.Background(), []string{checkpoint.ID, "--manual"}); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(paths.Meta)
	if err != nil || string(contents) != corrupt {
		t.Fatalf("policy rewrote unreadable cache: %q, %v", contents, err)
	}
	if !strings.Contains(stderr.String(), "warning") {
		t.Fatalf("missing cache preservation warning: %q", stderr.String())
	}
}

func TestCheckpointManagedLeaseSuccessStopsClaimRenewalImmediately(t *testing.T) {
	checkpoint := managedCheckpointFixture("chk_renewal_cutoff")
	server, _ := configureManagedCheckpointTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/leases/from-checkpoint" {
			t.Errorf("unexpected post-success claim operation: %s", r.URL.Path)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{ID: "cbx_000000000002", Provider: "aws"}})
	})
	coord := &CoordinatorClient{BaseURL: server.URL, Client: server.Client()}
	cfg := defaultConfig()
	cfg.Provider = "aws"
	cfg.AWSSnapshot = checkpoint.Image.ID
	readinessCtx, cancelReadiness := context.WithCancel(context.Background())
	t.Cleanup(cancelReadiness)
	renewalCtx, stopRenewal := context.WithCancel(context.Background())
	t.Cleanup(stopRenewal)
	callbackCalls := 0
	ctx := withCheckpointLeaseProvisioned(readinessCtx, checkpoint.ID, "request-local-claim", func() {
		callbackCalls++
		stopRenewal()
	})
	if _, err := coord.CreateLease(ctx, cfg, "ssh-ed25519 public", false, "cbx_000000000002", "fork"); err != nil {
		t.Fatal(err)
	}
	if callbackCalls != 1 {
		t.Fatalf("successful coordinator provisioning notification count = %d", callbackCalls)
	}
	if renewalCtx.Err() == nil || readinessCtx.Err() != nil {
		t.Fatalf("renewal must stop while SSH readiness remains active: renewal=%v readiness=%v", renewalCtx.Err(), readinessCtx.Err())
	}
}

func TestCheckpointManagedSafeCacheWriteRejectsOperatorOwnership(t *testing.T) {
	checkpoint := managedCheckpointFixture("chk_safe_cache_collision")
	server, store := configureManagedCheckpointTest(t, func(w http.ResponseWriter, r *http.Request) {})
	if _, err := store.Create(checkpointRecord{ID: checkpoint.ID, Kind: checkpointKindArchive}); err != nil {
		t.Fatal(err)
	}
	managed, err := checkpointRecordFromCoordinator(checkpoint, checkpointCoordinatorOrigin(server.URL))
	if err != nil {
		t.Fatal(err)
	}
	if err := writeSafeManagedCheckpointCache(store, managed); err == nil {
		t.Fatal("managed refresh overwrote an operator-owned checkpoint")
	}
	retained, _, err := store.Read(checkpoint.ID)
	if err != nil || retained.Kind != checkpointKindArchive {
		t.Fatalf("operator checkpoint changed: %#v, %v", retained, err)
	}
}

func TestCheckpointManagedForkRefreshPreservesUnreadableCache(t *testing.T) {
	checkpoint := managedCheckpointFixture("chk_corrupt_fork_refresh")
	server, store := configureManagedCheckpointTest(t, func(w http.ResponseWriter, r *http.Request) {})
	managed, err := checkpointRecordFromCoordinator(checkpoint, checkpointCoordinatorOrigin(server.URL))
	if err != nil {
		t.Fatal(err)
	}
	paths, err := store.Paths(managed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	const unreadable = "do not overwrite uncertain local ownership"
	if err := os.WriteFile(paths.Meta, []byte(unreadable), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeSafeManagedCheckpointCache(store, managed); err == nil {
		t.Fatal("successful fork refresh accepted unreadable local ownership")
	}
	contents, err := os.ReadFile(paths.Meta)
	if err != nil || string(contents) != unreadable {
		t.Fatalf("fork refresh changed unreadable metadata: %q, %v", contents, err)
	}
}

func TestCheckpointManagedExpiryRejectsOlderCoordinatorBeforeSourcePreparation(t *testing.T) {
	requests := []string{}
	server, _ := configureManagedCheckpointTest(t, func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		http.NotFound(w, r)
	})
	cfg := defaultConfig()
	cfg.Provider = "aws"
	cfg.Coordinator = server.URL
	record := checkpointRecord{ID: "chk_preflight_expiry", Provider: "aws", LeaseID: "cbx_000000000001"}
	ctx := withCheckpointCreateContext(context.Background(), record, coordinatorCheckpointRetention{
		Mode: "expire-unused", UnusedForSeconds: 3600,
	})
	_, err := (coordinatorCheckpointDriver{}).Create(ctx, checkpointNativeCreateRequest{
		Cfg:          cfg,
		Server:       Server{Provider: "aws", CloudID: "i-000000000001"},
		Target:       SSHTarget{Host: "source-must-not-be-contacted.invalid"},
		CheckpointID: record.ID,
		LeaseID:      record.LeaseID,
		Strategy:     checkpointStrategyDiskSnapshot,
	})
	if err == nil || !strings.Contains(err.Error(), "upgrade the coordinator") {
		t.Fatalf("pre-mutation support error = %v", err)
	}
	if len(requests) != 1 || requests[0] != "GET /v1/checkpoints" {
		t.Fatalf("unexpected pre-mutation requests = %#v", requests)
	}
}
