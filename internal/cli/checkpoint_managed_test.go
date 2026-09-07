package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

func TestCheckpointManagedDeleteRejectsReusedIdentity(t *testing.T) {
	for _, changed := range []string{"resource", "creation", "lease", "cache-without-image", "replacement-without-image", "both-without-image"} {
		t.Run(changed, func(t *testing.T) {
			checkpoint := managedCheckpointFixture("chk_reused_delete")
			remote := checkpoint
			image := *checkpoint.Image
			remote.Image = &image
			switch changed {
			case "resource":
				remote.Image.ID, remote.Image.ResourceID = "snap-replacement", "snap-replacement"
			case "creation":
				remote.CreatedAt = "2026-08-21T12:00:00Z"
			case "lease":
				remote.LeaseID = "cbx_000000000099"
			case "cache-without-image":
				checkpoint.State, checkpoint.Image = "creating", nil
				remote.CreatedAt = "2026-08-21T12:00:00Z"
			case "replacement-without-image":
				remote.State, remote.Image = "creating", nil
				remote.CreatedAt = "2026-08-21T12:00:00Z"
			case "both-without-image":
				checkpoint.State, checkpoint.Image = "failed", nil
				remote.State, remote.Image = "creating", nil
				remote.CreatedAt = "2026-08-21T12:00:00Z"
			}
			deletes := 0
			server, store := configureManagedCheckpointTest(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					deletes++
					_ = json.NewEncoder(w).Encode(map[string]any{"deleted": true, "checkpointID": remote.ID})
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"checkpoint": remote})
			})
			record, err := checkpointRecordFromCoordinator(checkpoint, checkpointCoordinatorOrigin(server.URL))
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Write(record); err != nil {
				t.Fatal(err)
			}
			paths, _ := store.Paths(record.ID)
			before, err := os.ReadFile(paths.Meta)
			if err != nil {
				t.Fatal(err)
			}
			err = (App{Stdout: io.Discard, Stderr: io.Discard}).checkpointDelete(context.Background(), []string{record.ID})
			if err == nil || !strings.Contains(err.Error(), "conflicting") || deletes != 0 {
				t.Fatalf("reused identity accepted: err=%v deletes=%d", err, deletes)
			}
			after, err := os.ReadFile(paths.Meta)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("stale recovery cache changed: %v", err)
			}
		})
	}
}

func TestCheckpointManagedDeleteAllowsSameCreationImagePublication(t *testing.T) {
	remote := managedCheckpointFixture("chk_pending_publication")
	deletes := 0
	server, store := configureManagedCheckpointTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deletes++
			_ = json.NewEncoder(w).Encode(map[string]any{"deleted": true, "checkpointID": remote.ID})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"checkpoint": remote})
	})
	pending := remote
	pending.State, pending.Image = "creating", nil
	record, err := checkpointRecordFromCoordinator(pending, checkpointCoordinatorOrigin(server.URL))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Write(record); err != nil {
		t.Fatal(err)
	}
	if err := (App{Stdout: io.Discard, Stderr: io.Discard}).checkpointDelete(context.Background(), []string{record.ID}); err != nil || deletes != 1 {
		t.Fatalf("same creation publication blocked: %v deletes=%d", err, deletes)
	}
}

func TestCheckpointManagedUnconfirmedJournalRequiresReadBeforeDeletion(t *testing.T) {
	for _, operation := range []string{"inspect", "list"} {
		t.Run(operation, func(t *testing.T) {
			remote := managedCheckpointFixture("chk_unconfirmed_journal")
			deletes := 0
			server, store := configureManagedCheckpointTest(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					deletes++
					_ = json.NewEncoder(w).Encode(map[string]any{"deleted": true, "checkpointID": remote.ID})
					return
				}
				if r.URL.Path == "/v1/checkpoints" {
					_ = json.NewEncoder(w).Encode(map[string]any{"checkpoints": []coordinatorCheckpoint{remote}})
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"checkpoint": remote})
			})
			journal := checkpointRecord{
				ID: remote.ID, Provider: remote.Provider, LeaseID: remote.LeaseID,
				Kind: checkpointKindAWSEBS, CreatedAt: "2026-08-19T12:00:00Z", LastUsedAt: "2026-08-19T12:00:00Z", CoordinatorState: "creating",
				Ownership: &checkpointOwnership{Mode: "coordinator", Origin: checkpointCoordinatorOrigin(server.URL)},
			}
			if err := store.Write(journal); err != nil {
				t.Fatal(err)
			}
			app := App{Stdout: io.Discard, Stderr: io.Discard}
			if err := app.checkpointDelete(context.Background(), []string{journal.ID}); err == nil || !strings.Contains(err.Error(), "inspect") || deletes != 0 {
				t.Fatalf("unconfirmed journal authorized deletion: %v deletes=%d", err, deletes)
			}
			args := []string{"checkpoint", operation, "--json"}
			if operation == "inspect" {
				args = append(args, journal.ID)
			}
			if err := app.Run(context.Background(), args); err != nil {
				t.Fatalf("journal hydration: %v", err)
			}
			current, _, err := store.Read(journal.ID)
			if err != nil || current.CreatedAt != remote.CreatedAt || current.Native.ImageID != remote.Image.ID {
				t.Fatalf("journal did not acquire coordinator identity: %#v %v", current, err)
			}
			if err := app.checkpointDelete(context.Background(), []string{journal.ID}); err != nil || deletes != 1 {
				t.Fatalf("confirmed identity deletion: %v deletes=%d", err, deletes)
			}
		})
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

func TestCheckpointManagedAdmissionLimitErrorsPreserveCoordinatorDiagnostics(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		code    string
		message string
		invoke  func(*CoordinatorClient) error
	}{
		{
			name:    "checkpoint creation",
			path:    "/v1/checkpoints",
			code:    "checkpoint_limit_exceeded",
			message: "checkpoint admission limit exceeded: scope=org observed=20 limit=20",
			invoke: func(client *CoordinatorClient) error {
				_, _, err := client.CreateCheckpoint(
					context.Background(),
					checkpointRecord{ID: "chk_limited", LeaseID: "cbx_000000000001"},
					"prepared-workspace",
					checkpointStrategyDiskSnapshot,
					true,
					coordinatorCheckpointRetention{Mode: "manual"},
				)
				return err
			},
		},
		{
			name:    "checkpoint use claim",
			path:    "/v1/checkpoints/chk_limited/use",
			code:    "checkpoint_claim_limit_exceeded",
			message: "checkpoint use claim limit exceeded: scope=checkpoint observed=16 limit=16",
			invoke: func(client *CoordinatorClient) error {
				_, err := client.BeginCheckpointUse(context.Background(), "chk_limited")
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			server, _ := configureManagedCheckpointTest(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != http.MethodPost || r.URL.Path != test.path {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				_ = json.NewEncoder(w).Encode(map[string]string{
					"error":   test.code,
					"message": test.message,
				})
			})
			client := &CoordinatorClient{BaseURL: server.URL, Client: server.Client()}

			err := test.invoke(client)

			var coordinatorErr CoordinatorHTTPError
			if !errors.As(err, &coordinatorErr) || coordinatorErr.StatusCode != http.StatusTooManyRequests {
				t.Fatalf("coordinator admission error = %v", err)
			}
			if !strings.Contains(err.Error(), test.message) {
				t.Fatalf("coordinator admission diagnostics = %q, want %q", err, test.message)
			}
			if calls != 1 {
				t.Fatalf("coordinator admission requests = %d, want 1", calls)
			}
		})
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

func TestCheckpointManagedDeleteRetriesTombstoneAfterPreflightGet(t *testing.T) {
	checkpoint := managedCheckpointFixture("chk_lost_delete_response")
	deletes := 0
	server, store := configureManagedCheckpointTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			if r.URL.Path == "/v1/checkpoints" {
				_ = json.NewEncoder(w).Encode(map[string]any{"checkpoints": []coordinatorCheckpoint{}})
			} else {
				http.NotFound(w, r)
			}
			return
		}
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

func TestCheckpointManagedInspectAbsence(t *testing.T) {
	testCheckpointManagedInspectAbsence(t, "")
}

func TestCheckpointManagedInspectAbsenceBuiltBinary(t *testing.T) {
	binary, err := builtCLITestBinary()
	if err != nil {
		t.Fatal(err)
	}
	testCheckpointManagedInspectAbsence(t, binary)
}

func testCheckpointManagedInspectAbsence(t *testing.T, binary string) {
	t.Helper()
	for _, tc := range []struct {
		name         string
		local        string
		lookupStatus int
		status       int
		inventory    string
		want         string
	}{
		{name: "absent", status: 200, inventory: `{"checkpoints":[]}`, want: "forget"},
		{name: "capture binding", local: "capture", status: 200, inventory: `{"checkpoints":[]}`, want: "reconcile_capture"},
		{name: "surviving cache", local: "cache", status: 200, inventory: `{"checkpoints":[]}`},
		{name: "wrong origin", local: "origin", status: 200, inventory: `{"checkpoints":[]}`},
		{name: "corrupt cache", local: "corrupt", status: 200, inventory: `{"checkpoints":[]}`},
		{name: "corrupt capture claim", local: "corrupt claim", status: 200, inventory: `{"checkpoints":[]}`},
		{name: "lookup unauthorized", lookupStatus: 401},
		{name: "lookup forbidden", lookupStatus: 403},
		{name: "lookup unavailable", lookupStatus: 503},
		{name: "unsupported route", status: 404},
		{name: "unsupported method", status: 405},
		{name: "probe unauthorized", status: 401},
		{name: "probe forbidden", status: 403},
		{name: "probe unavailable", status: 503},
		{name: "malformed inventory", status: 200, inventory: `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const id = "chk_absence"
			var lookups, probes atomic.Int32
			server, store := configureManagedCheckpointTest(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("inspection mutated coordinator: %s %s", r.Method, r.URL.Path)
				}
				if tc.local == "origin" {
					t.Error("wrong-origin inspection contacted coordinator")
				}
				if r.URL.Path == "/v1/checkpoints" {
					probes.Add(1)
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, tc.inventory)
					return
				}
				if r.URL.Path != "/v1/checkpoints/"+id {
					t.Errorf("inspection queried another checkpoint: %s", r.URL.Path)
				}
				lookups.Add(1)
				code := tc.lookupStatus
				if code == 0 {
					code = http.StatusNotFound
				}
				w.WriteHeader(code)
				_, _ = io.WriteString(w, `{"error":"fixture_lookup_failure"}`)
			})
			paths, err := store.Paths(id)
			if err != nil {
				t.Fatal(err)
			}
			switch tc.local {
			case "cache", "origin":
				origin := checkpointCoordinatorOrigin(server.URL)
				if tc.local == "origin" {
					origin += "/other-deployment"
				}
				record, err := checkpointRecordFromCoordinator(managedCheckpointFixture(id), origin)
				if err != nil {
					t.Fatal(err)
				}
				if err := store.Write(record); err != nil {
					t.Fatal(err)
				}
			case "corrupt":
				if err := os.MkdirAll(paths.Dir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(paths.Meta, []byte("{"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "capture", "corrupt claim":
				const leaseID = "cbx_abcdef123456"
				if err := withDurableLeaseClaimLock(leaseID, func(claim *leaseClaim, _ bool, persist func() error) error {
					*claim = leaseClaim{LeaseID: leaseID, Provider: "aws", CheckpointCapture: &CheckpointCaptureBinding{ID: id}}
					return persist()
				}); err != nil {
					t.Fatal(err)
				}
				if tc.local == "corrupt claim" {
					path, err := leaseClaimPath(leaseID)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			stateRoot := os.Getenv("XDG_STATE_HOME")
			before, err := snapshotTestTree(stateRoot)
			if err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			env := []string{
				"HOME=" + root,
				"XDG_CONFIG_HOME=" + root,
				"XDG_STATE_HOME=" + stateRoot,
				"CRABBOX_CONFIG=" + filepath.Join(root, "missing.yaml"),
				"CRABBOX_COORDINATOR=" + server.URL,
				"CRABBOX_COORDINATOR_TOKEN=test-user-session",
			}
			for _, verify := range []bool{false, true} {
				for _, jsonOut := range []bool{false, true} {
					args := []string{id}
					if jsonOut {
						args = append(args, "--json")
					}
					if verify {
						args = append(args, "--verify")
					}
					var stdout bytes.Buffer
					if binary == "" {
						err = (App{Stdout: &stdout, Stderr: io.Discard}).checkpointInspect(context.Background(), args)
					} else {
						out, stderr, code := runDescribeTestBinary(binary, root, env, append([]string{"checkpoint", "inspect"}, args...)...)
						stdout.Write(out)
						err = nil
						if code != 0 {
							err = fmt.Errorf("exit %d: %s", code, stderr)
						}
						t.Logf("verify=%t json=%t exit=%d stdout=%s stderr=%s", verify, jsonOut, code, out, stderr)
					}
					if !jsonOut || tc.want == "" {
						if err == nil || stdout.Len() != 0 {
							t.Fatalf("uncertain or human absence accepted: err=%v stdout=%s", err, &stdout)
						}
					} else {
						var got missingCheckpointAudit
						if err != nil || json.Unmarshal(stdout.Bytes(), &got) != nil {
							t.Fatalf("missing JSON verdict: err=%v stdout=%s", err, &stdout)
						}
						providerState := "missing"
						if tc.want == "reconcile_capture" {
							providerState = "unknown"
						}
						if got != (missingCheckpointAudit{ID: id, LocalState: "missing", ProviderState: providerState, NextAction: tc.want}) {
							t.Fatalf("wrong absence verdict: %+v", got)
						}
					}
					after, snapshotErr := snapshotTestTree(stateRoot)
					if snapshotErr != nil || !reflect.DeepEqual(before, after) {
						t.Fatalf("inspection changed cache or capture binding: %v", snapshotErr)
					}
				}
			}
			wantLookups, wantProbes := int32(4), int32(4)
			if tc.local == "origin" {
				wantLookups, wantProbes = 0, 0
			} else if tc.lookupStatus != 0 {
				wantProbes = 0
			}
			if lookups.Load() != wantLookups || probes.Load() != wantProbes {
				t.Fatalf("unexpected GET counts: checkpoint=%d inventory=%d", lookups.Load(), probes.Load())
			}
			t.Logf("GET checkpoint=%d inventory=%d; local state unchanged", lookups.Load(), probes.Load())
		})
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

func TestCheckpointManagedFixedLeaseUsesExactCheckpointRoute(t *testing.T) {
	for _, supported := range []bool{true, false} {
		t.Run(strconv.FormatBool(supported), func(t *testing.T) {
			const leaseID = "cbx_000000000002"
			const checkpointID = "chk_fixed_managed"
			calls := 0
			server, _ := configureManagedCheckpointTest(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != http.MethodPut || r.URL.Path != "/v1/leases/"+leaseID+"/from-checkpoint" {
					t.Errorf("fixed checkpoint escaped its authenticated route: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
					return
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body["leaseID"] != leaseID || body["checkpointID"] != checkpointID || body["checkpointUseClaim"] != "fresh-use-claim" {
					t.Error("fixed checkpoint request lost its exact identity or use claim")
				}
				if !supported {
					http.NotFound(w, r)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{ID: leaseID, Provider: "aws", State: "active"}})
			})
			coord := &CoordinatorClient{BaseURL: server.URL, Client: server.Client(), checkpointSupportKnown: true, checkpointSupported: true}
			cfg := defaultConfig()
			cfg.Provider = "aws"
			cfg.AWSSnapshot = "snap-0001"
			acknowledged := false
			ctx := withCheckpointLeaseProvisioned(context.Background(), checkpointID, "fresh-use-claim", func() { acknowledged = true })
			lease, err := coord.EnsureLease(ctx, cfg, "ssh-ed25519 public", true, leaseID, "fixed-fork")
			if calls != 1 || acknowledged != supported {
				t.Fatalf("calls=%d acknowledged=%v; expected exactly one checkpoint request, acknowledgement only on success", calls, acknowledged)
			}
			if supported && (err != nil || lease.ID != leaseID) {
				t.Fatalf("fixed checkpoint lease=%#v error=%v", lease, err)
			}
			if !supported && err == nil {
				t.Fatal("old coordinator must reject without ordinary-create fallback")
			}
		})
	}
}

func TestCheckpointManagedFixedForkReachesCoordinatorAdmission(t *testing.T) {
	checkpoint := managedCheckpointFixture("chk_fixed_cli")
	const leaseID = "cbx_000000000002"
	puts := 0
	_, _ = configureManagedCheckpointTest(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/checkpoints/" + checkpoint.ID:
			_ = json.NewEncoder(w).Encode(map[string]any{"checkpoint": checkpoint})
		case "/v1/checkpoints/" + checkpoint.ID + "/use":
			_ = json.NewEncoder(w).Encode(map[string]any{"checkpoint": checkpoint, "claim": "cli-fork-claim", "expiresAt": time.Now().Add(time.Minute).Format(time.RFC3339)})
		case "/v1/leases/" + leaseID + "/from-checkpoint":
			puts++
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"error":"checkpoint_admission_test_refusal"}`)
		default:
			t.Errorf("unexpected fixed-fork operation: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	})
	testAWSBackendOverride = &testSSHBackend{spec: ProviderSpec{Name: "aws"}}
	t.Cleanup(func() { testAWSBackendOverride = nil })
	err := (App{Stdout: io.Discard, Stderr: io.Discard}).checkpointFork(context.Background(), []string{checkpoint.ID, "--lease-id", leaseID, "--json"})
	if puts != 1 || err == nil || !strings.Contains(err.Error(), "checkpoint_admission_test_refusal") {
		t.Fatalf("fixed fork must reach the coordinator-owned admission exactly once: puts=%d error=%v", puts, err)
	}
}

func TestCoordinatorFixedCheckpointRequiresMatchingContext(t *testing.T) {
	for _, contextCheckpoint := range []string{"", "chk_other"} {
		t.Run(contextCheckpoint, func(t *testing.T) {
			backend := &coordinatorLeaseBackend{}
			ctx := context.Background()
			if contextCheckpoint != "" {
				ctx = withCheckpointLeaseClaim(ctx, contextCheckpoint, "claim")
			}
			_, err := backend.Acquire(ctx, AcquireRequest{RequestedLeaseID: "cbx_000000000002", RequestedCheckpointID: "chk_requested"})
			if err == nil || !strings.Contains(err.Error(), "exact checkpoint use context") {
				t.Fatalf("mismatched checkpoint context was admitted: %v", err)
			}
		})
	}
}

type checkpointRenewalRaceBackend struct {
	*watchTestBackend
	coordinator *CoordinatorClient
	config      Config
	createCtx   chan context.Context
	lateSuccess bool
}

func (backend *checkpointRenewalRaceBackend) Acquire(ctx context.Context, _ AcquireRequest) (LeaseTarget, error) {
	backend.createCtx <- ctx
	requestCtx := ctx
	if backend.lateSuccess {
		// Model a provider returning an acquired lease after cancellation.
		var cancel context.CancelFunc
		requestCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
	}
	lease, err := backend.coordinator.CreateLease(requestCtx, backend.config, "ssh-ed25519 public", false, "cbx_000000000002", "fork")
	if err != nil {
		return LeaseTarget{}, err
	}
	return LeaseTarget{
		Server:  Server{Provider: "aws", CloudID: "i-000000000002", Labels: map[string]string{"lease": lease.ID}},
		SSH:     SSHTarget{Host: "checkpoint.example.test", TargetOS: targetWindows},
		LeaseID: lease.ID,
	}, nil
}

func TestCheckpointManagedRenewalCompletionRace(t *testing.T) {
	tests := []struct {
		name          string
		renewalStatus int
		renewalBody   string
		createFails   bool
		wantSuccess   bool
		wantRenewal   bool
		lateSuccess   bool
	}{
		{
			name:          "consumed claim waits for successful create response",
			renewalStatus: http.StatusConflict,
			renewalBody:   `{"error":"checkpoint_claim_invalid","message":"checkpoint use claim is invalid or expired"}`,
			wantSuccess:   true,
		},
		{
			name:          "different renewal conflict cancels in-flight create",
			renewalStatus: http.StatusConflict,
			renewalBody:   `{"error":"checkpoint_source_mismatch","message":"checkpoint_claim_invalid is not the structured error"}`,
			wantRenewal:   true,
		},
		{
			name:          "fatal renewal releases a late successful acquisition",
			renewalStatus: http.StatusConflict,
			renewalBody:   `{"error":"checkpoint_source_mismatch"}`,
			wantRenewal:   true,
			lateSuccess:   true,
		},
		{
			name:          "consumed claim does not turn failed create into success",
			renewalStatus: http.StatusConflict,
			renewalBody:   `{"error":"checkpoint_claim_invalid"}`,
			createFails:   true,
		},
		{
			name:          "claim-invalid body without conflict remains fatal",
			renewalStatus: http.StatusServiceUnavailable,
			renewalBody:   `{"error":"checkpoint_claim_invalid"}`,
			wantRenewal:   true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			checkpoint := managedCheckpointFixture("chk_renewal_completion_race")
			checkpoint.Target = targetWindows
			createStarted := make(chan struct{})
			renewalResponded := make(chan struct{})
			allowCreateResponse := make(chan struct{})
			var responseOnce sync.Once
			releaseCreateResponse := func() { responseOnce.Do(func() { close(allowCreateResponse) }) }
			var renewals atomic.Int32
			var aborts atomic.Int32
			server, _ := configureManagedCheckpointTest(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/checkpoints/" + checkpoint.ID + "/use":
					var action struct {
						Action string `json:"action"`
					}
					if err := json.NewDecoder(r.Body).Decode(&action); err != nil {
						t.Error(err)
						return
					}
					switch action.Action {
					case "begin":
						_ = json.NewEncoder(w).Encode(map[string]any{
							"checkpoint": checkpoint,
							"claim":      "request-local-checkpoint-claim",
							"expiresAt":  time.Now().Add(2 * time.Second).Format(time.RFC3339Nano),
						})
					case "renew":
						if renewals.Add(1) != 1 {
							t.Error("checkpoint renewal continued after its terminal response")
							return
						}
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(test.renewalStatus)
						_, _ = io.WriteString(w, test.renewalBody)
						if flusher, ok := w.(http.Flusher); ok {
							flusher.Flush()
						}
						close(renewalResponded)
					case "abort":
						aborts.Add(1)
						_ = json.NewEncoder(w).Encode(map[string]any{"checkpoint": checkpoint})
					default:
						t.Errorf("unexpected checkpoint claim action: %s", action.Action)
					}
				case "/v1/leases/from-checkpoint":
					close(createStarted)
					select {
					case <-r.Context().Done():
						return
					case <-allowCreateResponse:
					}
					if test.createFails {
						w.WriteHeader(http.StatusBadGateway)
						_, _ = io.WriteString(w, `{"error":"checkpoint_create_failed"}`)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{
						"lease": CoordinatorLease{ID: "cbx_000000000002", Provider: "aws"},
					})
				case "/v1/checkpoints/" + checkpoint.ID:
					_ = json.NewEncoder(w).Encode(map[string]any{"checkpoint": checkpoint})
				default:
					http.NotFound(w, r)
				}
			})
			record, err := checkpointRecordFromCoordinator(checkpoint, checkpointCoordinatorOrigin(server.URL))
			if err != nil {
				t.Fatal(err)
			}
			cfg := defaultConfig()
			cfg.Provider = "aws"
			cfg.TargetOS = targetWindows
			cfg.AWSSnapshot = checkpoint.Image.ID
			backend := &checkpointRenewalRaceBackend{
				watchTestBackend: newWatchTestBackend(),
				coordinator:      &CoordinatorClient{BaseURL: server.URL, Client: server.Client()},
				config:           cfg,
				createCtx:        make(chan context.Context, 1),
				lateSuccess:      test.lateSuccess,
			}
			type result struct {
				provision checkpointForkProvision
				err       error
			}
			finished := make(chan result, 1)
			done := make(chan struct{})
			ctx, cancel := context.WithCancel(context.Background())
			repo := Repo{Root: t.TempDir(), Name: "my-app"}
			t.Cleanup(func() {
				cancel()
				releaseCreateResponse()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Error("checkpoint provisioning goroutine did not stop")
				}
			})
			go func() {
				defer close(done)
				provision, provisionErr := (App{Stdout: io.Discard, Stderr: io.Discard}).provisionManagedCheckpointFork(
					ctx, cfg, backend, backend, repo,
					record, checkpointPaths{}, false, false, "", "fork", "", true, nil,
				)
				finished <- result{provision: provision, err: provisionErr}
			}()
			await := func(channel <-chan struct{}, description string) {
				t.Helper()
				select {
				case <-channel:
				case <-time.After(5 * time.Second):
					t.Fatalf("timed out waiting for %s", description)
				}
			}
			await(createStarted, "checkpoint-backed create to start")
			var createCtx context.Context
			select {
			case createCtx = <-backend.createCtx:
			case <-time.After(5 * time.Second):
				t.Fatal("timed out waiting for acquisition context")
			}
			await(renewalResponded, "coordinator renewal response")
			if test.wantRenewal {
				await(createCtx.Done(), "fatal renewal error to cancel the in-flight create")
			} else {
				select {
				case <-createCtx.Done():
					t.Fatalf("consumed claim canceled the in-flight create: %v", createCtx.Err())
				case <-time.After(50 * time.Millisecond):
				}
			}
			// Fatal cancellation must finish before the server can return success;
			// the explicit late-success case separately checks cleanup of that lease.
			if !test.wantRenewal || test.lateSuccess {
				releaseCreateResponse()
			}
			var got result
			select {
			case got = <-finished:
			case <-time.After(5 * time.Second):
				t.Fatal("checkpoint provisioning did not finish")
			}
			releaseCreateResponse()
			_, _, releases := backend.counts()
			wantReleases := 0
			if test.lateSuccess {
				wantReleases = 1
			}
			if renewals.Load() != 1 || releases != wantReleases {
				t.Fatalf("renewals=%d releases=%d; want one stopped renewal and %d releases", renewals.Load(), releases, wantReleases)
			}
			if test.lateSuccess && (backend.releaseLease.LeaseID != "cbx_000000000002" || backend.releaseCtx == nil || backend.releaseCtx.Err() != nil) {
				t.Fatalf("late acquisition needs fresh-context cleanup of its exact lease: lease=%#v context=%v", backend.releaseLease, backend.releaseCtx)
			}
			if test.wantSuccess {
				if got.err != nil || got.provision.Lease.LeaseID != "cbx_000000000002" || aborts.Load() != 0 {
					t.Fatalf("successful create: provision=%#v err=%v aborts=%d", got.provision, got.err, aborts.Load())
				}
				return
			}
			wantAborts := int32(1)
			if test.lateSuccess {
				wantAborts = 0
			}
			if got.err == nil || aborts.Load() != wantAborts {
				t.Fatalf("failed create: provision=%#v err=%v aborts=%d", got.provision, got.err, aborts.Load())
			}
			if test.wantRenewal != strings.Contains(got.err.Error(), "renew checkpoint use claim:") {
				t.Fatalf("renewal classification: error=%v wantRenewal=%t", got.err, test.wantRenewal)
			}
			if test.createFails && !strings.Contains(got.err.Error(), "checkpoint_create_failed") {
				t.Fatalf("actual create failure was not preserved: %v", got.err)
			}
		})
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
	}, nil)
	_, err := (coordinatorCheckpointDriver{}).Create(ctx, NativeCheckpointCreateRequest{
		Config:       cfg,
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

func TestCheckpointManagedListFallsBackWithoutChangingLocalCache(t *testing.T) {
	for _, failure := range []string{"offline", "500", "503"} {
		t.Run(failure, func(t *testing.T) {
			calls := 0
			server, store := configureManagedCheckpointTest(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != http.MethodGet || r.URL.Path != "/v1/checkpoints" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				code := http.StatusInternalServerError
				if failure == "503" {
					code = http.StatusServiceUnavailable
				}
				http.Error(w, "unavailable", code)
			})
			if failure == "offline" {
				server.Close()
			}
			remote := managedCheckpointFixture("chk_cached_offline")
			cached, err := checkpointRecordFromCoordinator(remote, checkpointCoordinatorOrigin(server.URL))
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Write(cached); err != nil {
				t.Fatal(err)
			}
			local, err := store.Create(checkpointRecord{ID: "chk_local_offline", Kind: checkpointKindArchive})
			if err != nil {
				t.Fatal(err)
			}
			before := map[string][]byte{}
			for _, id := range []string{cached.ID, local.ID} {
				paths, _ := store.Paths(id)
				before[id], err = os.ReadFile(paths.Meta)
				if err != nil {
					t.Fatal(err)
				}
			}
			var stdout, stderr bytes.Buffer
			if err := (App{Stdout: &stdout, Stderr: &stderr}).checkpointList(context.Background(), []string{"--json"}); err != nil {
				t.Fatal(err)
			}
			var records []checkpointRecord
			if err := json.Unmarshal(stdout.Bytes(), &records); err != nil || len(records) != 2 {
				t.Fatalf("inventory: %q, %v", stdout.String(), err)
			}
			if !strings.Contains(stderr.String(), "partial local inventory") {
				t.Fatalf("missing warning: %s", stderr.String())
			}
			for id, data := range before {
				paths, _ := store.Paths(id)
				after, err := os.ReadFile(paths.Meta)
				if err != nil || !bytes.Equal(data, after) {
					t.Fatalf("fallback changed cache %s: %v", id, err)
				}
			}
			if failure != "offline" && calls != 1 {
				t.Fatalf("requests = %d", calls)
			}
		})
	}
}

func TestCheckpointManagedListDoesNotHideAuthorizationOrMalformedInventory(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusOK} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			_, _ = configureManagedCheckpointTest(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(code)
				_, _ = io.WriteString(w, "invalid inventory")
			})
			var stdout bytes.Buffer
			err := (App{Stdout: &stdout, Stderr: io.Discard}).checkpointList(context.Background(), []string{"--json"})
			if err == nil || stdout.Len() != 0 {
				t.Fatalf("hidden failure: %v, %s", err, stdout.String())
			}
		})
	}
}

func TestCheckpointInventoryTimeoutDistinguishesCallerCancellation(t *testing.T) {
	if !checkpointInventoryUnavailable(context.Background(), context.DeadlineExceeded) {
		t.Fatal("internal deadline did not permit cached inventory")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if checkpointInventoryUnavailable(ctx, context.DeadlineExceeded) {
		t.Fatal("caller cancellation hidden")
	}
	if checkpointInventoryUnavailable(context.Background(), errors.New("invalid inventory")) {
		t.Fatal("malformed inventory hidden")
	}
}

func TestCheckpointManagedCachePreservesCaptureAndRejectsActiveCapture(t *testing.T) {
	server, store := configureManagedCheckpointTest(t, func(w http.ResponseWriter, r *http.Request) { t.Error("cache write contacted coordinator") })
	record, err := checkpointRecordFromCoordinator(managedCheckpointFixture("chk_capture_cache"), checkpointCoordinatorOrigin(server.URL))
	if err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"retired", "submitting"} {
		t.Run(phase, func(t *testing.T) {
			existing := record
			existing.Capture = &NativeCheckpointCapture{Phase: phase}
			if err := store.Write(existing); err != nil {
				t.Fatal(err)
			}
			err := writeSafeManagedCheckpointCache(store, record)
			if (err != nil) != (phase == "submitting") {
				t.Fatalf("refresh = %v", err)
			}
			current, _, err := store.Read(record.ID)
			if err != nil || current.Capture == nil || current.Capture.Phase != phase {
				t.Fatalf("capture lost: %#v, %v", current.Capture, err)
			}
		})
	}
}

func TestCheckpointExpiryRejectsRetirementBeforeAccess(t *testing.T) {
	t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	for _, option := range []string{"--retire-source", "--prepare-only", "--discard-failed", "--checkpoint-id=chk_retirement"} {
		err := (App{Stdout: io.Discard, Stderr: io.Discard}).checkpointCreate(context.Background(), []string{"--expire-unused-after=1h", option})
		if err == nil || !strings.Contains(err.Error(), "cannot be combined with source retirement") {
			t.Fatalf("%s: %v", option, err)
		}
	}
}

func TestCheckpointManagedCreatePersistsOwnershipBeforeProviderMutation(t *testing.T) {
	for _, outcome := range []string{"ambiguous", "legacy", "journal-failure"} {
		t.Run(outcome, func(t *testing.T) {
			var ownership []bool
			posts := 0
			server, _ := configureManagedCheckpointTest(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					posts++
				}
				if len(ownership) == 0 || !ownership[0] {
					t.Error("request preceded durable managed ownership")
				}
				if outcome == "legacy" {
					if r.URL.Path == "/v1/checkpoints" {
						http.NotFound(w, r)
						return
					}
					if len(ownership) != 2 || ownership[1] {
						t.Error("legacy create retained managed ownership")
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"image": CoordinatorImage{ID: "ami-test", State: "available"}})
					return
				}
				http.Error(w, "ambiguous provider result", http.StatusServiceUnavailable)
			})
			t.Setenv("CRABBOX_COORDINATOR_ADMIN_TOKEN", "test-admin")
			cfg := defaultConfig()
			cfg.Provider, cfg.Coordinator, cfg.TargetOS = "aws", server.URL, targetWindows
			cfg.WindowsMode = windowsModeNormal
			record := checkpointRecord{ID: "chk_create_recovery", Provider: "aws", LeaseID: "cbx_000000000001"}
			ctx := withCheckpointCreateContext(context.Background(), record, checkpointRetentionFromDuration(0), func(managed bool) error {
				ownership = append(ownership, managed)
				if outcome == "journal-failure" {
					return errors.New("journal unavailable")
				}
				return nil
			})
			image, err := (coordinatorCheckpointDriver{}).Create(ctx, NativeCheckpointCreateRequest{
				Config: cfg, Server: Server{Provider: "aws", CloudID: "i-test"},
				Target:       SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeNormal},
				CheckpointID: record.ID, LeaseID: record.LeaseID, Strategy: checkpointStrategyImage,
			})
			if outcome == "legacy" {
				if err != nil || image.ID != "ami-test" || posts != 2 {
					t.Fatalf("legacy result: %#v, %v, posts=%d", image, err, posts)
				}
			} else if outcome == "journal-failure" {
				if err == nil || !strings.Contains(err.Error(), "journal unavailable") || posts != 0 {
					t.Fatalf("journal failure: %v, posts=%d", err, posts)
				}
			} else if err == nil || posts != 1 || len(ownership) != 1 || !ownership[0] {
				t.Fatalf("ambiguous ownership: %v, posts=%d, journal=%v", err, posts, ownership)
			}
		})
	}
}

func TestCheckpointRetirementUsesHostOwnedCoordinatorImage(t *testing.T) {
	calls := 0
	server, _ := configureManagedCheckpointTest(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodPost || r.URL.Path != "/v1/images" {
			t.Errorf("retirement used %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"image": CoordinatorImage{ID: "ami-retirement", State: "available"}})
	})
	t.Setenv("CRABBOX_COORDINATOR_ADMIN_TOKEN", "test-admin")
	cfg := defaultConfig()
	cfg.Provider, cfg.Coordinator, cfg.TargetOS = "aws", server.URL, targetWindows
	cfg.WindowsMode = windowsModeNormal
	image, err := (coordinatorCheckpointDriver{}).Create(context.Background(), NativeCheckpointCreateRequest{
		Config: cfg, Server: Server{Provider: "aws", CloudID: "i-test"},
		Target:       SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeNormal},
		CheckpointID: "chk_host_capture", LeaseID: "cbx_000000000001", Strategy: checkpointStrategyImage,
		Capture: &NativeCheckpointCapture{Phase: "submitting"},
	})
	if err != nil || image.ID != "ami-retirement" || image.managedCheckpoint != nil || calls != 1 {
		t.Fatalf("host capture: %#v, %v, calls=%d", image, err, calls)
	}
}

func TestCheckpointManagedDeleteWithoutCacheStillDeletesRemote(t *testing.T) {
	checkpoint := managedCheckpointFixture("chk_uncached_delete")
	calls := 0
	server, store := configureManagedCheckpointTest(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{"checkpoint": checkpoint})
			return
		}
		if r.Method != http.MethodDelete || r.URL.Path != "/v1/checkpoints/"+checkpoint.ID {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"deleted": true, "checkpointID": checkpoint.ID})
	})
	record, err := checkpointRecordFromCoordinator(checkpoint, checkpointCoordinatorOrigin(server.URL))
	if err != nil {
		t.Fatal(err)
	}
	if err := (App{Stdout: io.Discard, Stderr: io.Discard}).deleteManagedCheckpoint(context.Background(), store, record); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("remote deletion requests = %d", calls)
	}
	if _, _, err := store.Read(record.ID); !isCheckpointNotFound(err) {
		t.Fatalf("created a cache on deletion: %v", err)
	}
}

func TestCheckpointManagedDeleteDistinguishesUnsupportedAPIFromAbsence(t *testing.T) {
	for _, code := range []int{200, 404, 405} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			_, _ = configureManagedCheckpointTest(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("unexpected mutation: %s", r.Method)
				}
				if r.URL.Path == "/v1/checkpoints" && code == 200 {
					_, _ = io.WriteString(w, `{"checkpoints":[]}`)
					return
				}
				w.WriteHeader(codeIfInventory(r.URL.Path, code))
				_, _ = io.WriteString(w, `{"error":"not_found"}`)
			})
			var stdout bytes.Buffer
			err := (App{Stdout: &stdout, Stderr: io.Discard}).checkpointDelete(context.Background(), []string{"chk_missing_cache"})
			if code == 200 {
				if err != nil || !strings.Contains(stdout.String(), "checkpoint absent") {
					t.Fatalf("confirmed absence: %v %q", err, stdout.String())
				}
			} else if err == nil || !strings.Contains(err.Error(), "upgrade the coordinator") || stdout.Len() != 0 {
				t.Fatalf("unsupported route: %v %q", err, stdout.String())
			}
		})
	}
}

func codeIfInventory(path string, code int) int {
	if path == "/v1/checkpoints" {
		return code
	}
	return http.StatusNotFound
}

func TestCheckpointManagedListRejectsMissingInventoryArray(t *testing.T) {
	for _, body := range []string{`{}`, `null`, `{"checkpoints":null}`} {
		t.Run(body, func(t *testing.T) {
			server, store := configureManagedCheckpointTest(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, body) })
			record, _ := checkpointRecordFromCoordinator(managedCheckpointFixture("chk_inventory_shape"), checkpointCoordinatorOrigin(server.URL))
			if err := store.Write(record); err != nil {
				t.Fatal(err)
			}
			paths, _ := store.Paths(record.ID)
			before, _ := os.ReadFile(paths.Meta)
			var stdout bytes.Buffer
			err := (App{Stdout: &stdout, Stderr: io.Discard}).checkpointList(context.Background(), []string{"--json"})
			after, _ := os.ReadFile(paths.Meta)
			if err == nil || stdout.Len() != 0 || !bytes.Equal(before, after) {
				t.Fatalf("invalid response accepted or cache changed: %v %q", err, stdout.String())
			}
		})
	}
}

func TestCheckpointManagedLookupRejectsAnotherCheckpointID(t *testing.T) {
	other := managedCheckpointFixture("chk_other_resource")
	mutations := 0
	_, store := configureManagedCheckpointTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			mutations++
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"checkpoint": other})
	})
	err := (App{Stdout: io.Discard, Stderr: io.Discard}).checkpointDelete(context.Background(), []string{"chk_requested_resource"})
	if err == nil || !strings.Contains(err.Error(), "requested checkpoint") || mutations != 0 {
		t.Fatalf("wrong resource accepted: %v mutations=%d", err, mutations)
	}
	if _, _, err := store.Read(other.ID); !isCheckpointNotFound(err) {
		t.Fatalf("cached wrong response: %v", err)
	}
}

func TestCheckpointManagedListRecoversOnlyAuthoritativeCorruptCache(t *testing.T) {
	for _, scenario := range []string{"no-coordinator", "unrecovered", "recovered"} {
		t.Run(scenario, func(t *testing.T) {
			remote := managedCheckpointFixture("chk_corrupt_inventory")
			_, store := configureManagedCheckpointTest(t, func(w http.ResponseWriter, r *http.Request) {
				records := []coordinatorCheckpoint{}
				if scenario == "recovered" {
					records = append(records, remote)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"checkpoints": records})
			})
			if scenario == "no-coordinator" {
				t.Setenv("CRABBOX_COORDINATOR", "")
			}
			paths, _ := store.Paths(remote.ID)
			if err := os.MkdirAll(paths.Dir, 0o700); err != nil {
				t.Fatal(err)
			}
			const corrupt = "preserve corrupt published checkpoint"
			if err := os.WriteFile(paths.Meta, []byte(corrupt), 0o600); err != nil {
				t.Fatal(err)
			}
			var stdout bytes.Buffer
			err := (App{Stdout: &stdout, Stderr: io.Discard}).checkpointList(context.Background(), []string{"--json"})
			if scenario == "recovered" {
				if err != nil || !strings.Contains(stdout.String(), remote.ID) {
					t.Fatalf("recovery: %v %q", err, stdout.String())
				}
			} else if err == nil || stdout.Len() != 0 {
				t.Fatalf("corruption hidden: %v %q", err, stdout.String())
			}
			after, err := os.ReadFile(paths.Meta)
			if err != nil || string(after) != corrupt {
				t.Fatalf("corrupt bytes changed: %v", err)
			}
		})
	}
}

func TestCheckpointListKeepsLocalRecordsWhenConfigurationCannotLoad(t *testing.T) {
	for _, scenario := range []string{"malformed-file", "unregistered-provider"} {
		t.Run(scenario, func(t *testing.T) {
			_, store := configureManagedCheckpointTest(t, func(w http.ResponseWriter, r *http.Request) {
				t.Error("invalid configuration must not contact a coordinator")
			})
			if scenario == "malformed-file" {
				if err := os.WriteFile(os.Getenv("CRABBOX_CONFIG"), []byte("unrelated: [\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				t.Setenv("CRABBOX_PROVIDER", "missing-provider")
			}
			record := checkpointRecord{ID: "chk_local_config", Kind: checkpointKindArchive, CreatedAt: "2026-08-19T12:00:00Z"}
			if _, err := store.Create(record); err != nil {
				t.Fatal(err)
			}
			paths, err := store.Paths(record.ID)
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(paths.Meta)
			if err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			err = (App{Stdout: &stdout, Stderr: &stderr}).checkpointList(context.Background(), []string{"--json"})
			if err != nil || !strings.Contains(stdout.String(), record.ID) || !strings.Contains(stderr.String(), "partial local inventory") {
				t.Fatalf("local inventory: err=%v stdout=%q stderr=%q", err, &stdout, &stderr)
			}
			after, err := os.ReadFile(paths.Meta)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("local metadata changed: %v", err)
			}
		})
	}
}

func TestCheckpointManagedDeleteRecoversUnavailableCache(t *testing.T) {
	for _, scenario := range []string{"corrupt", "unwritable"} {
		t.Run(scenario, func(t *testing.T) {
			remote := managedCheckpointFixture("chk_delete_recovery")
			deletes := 0
			_, store := configureManagedCheckpointTest(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					deletes++
					_ = json.NewEncoder(w).Encode(map[string]any{"deleted": true, "checkpointID": remote.ID})
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"checkpoint": remote})
			})
			paths, _ := store.Paths(remote.ID)
			preserved := paths.Meta
			if scenario == "unwritable" {
				preserved = store.root
			}
			if err := os.MkdirAll(filepath.Dir(preserved), 0o700); err != nil {
				t.Fatal(err)
			}
			const contents = "unavailable local cache evidence"
			if err := os.WriteFile(preserved, []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			app := App{Stdout: &stdout, Stderr: &stderr}
			if err := app.checkpointDelete(context.Background(), []string{remote.ID, "--dry-run"}); err != nil {
				t.Fatal(err)
			}
			if deletes != 0 || !strings.Contains(stdout.String(), "would delete checkpoint") {
				t.Fatalf("dry run: %d %q", deletes, stdout.String())
			}
			if err := app.checkpointDelete(context.Background(), []string{remote.ID}); err != nil {
				t.Fatal(err)
			}
			after, err := os.ReadFile(preserved)
			if deletes != 1 || err != nil || string(after) != contents || !strings.Contains(stderr.String(), "preserving") {
				t.Fatalf("delete recovery: calls=%d err=%v bytes=%q warnings=%q", deletes, err, after, stderr.String())
			}
		})
	}
}

func TestCheckpointManagedDeleteDoesNotBypassBusyCapture(t *testing.T) {
	calls := 0
	server, store := configureManagedCheckpointTest(t, func(w http.ResponseWriter, r *http.Request) { calls++ })
	record, _ := checkpointRecordFromCoordinator(managedCheckpointFixture("chk_busy_managed"), checkpointCoordinatorOrigin(server.URL))
	if err := store.Write(record); err != nil {
		t.Fatal(err)
	}
	if err := store.WithLock(record.ID, func() error {
		err := (App{Stdout: io.Discard, Stderr: io.Discard}).checkpointDelete(context.Background(), []string{record.ID})
		var busy checkpointBusyError
		if !errors.As(err, &busy) {
			t.Fatalf("busy operation bypassed: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("busy capture made %d requests", calls)
	}
}

func TestCheckpointManagedVerificationRejectsForeignCoordinator(t *testing.T) {
	for _, localOnly := range []bool{false, true} {
		t.Run(strconv.FormatBool(localOnly), func(t *testing.T) {
			calls := 0
			_, store := configureManagedCheckpointTest(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != "/v1/checkpoints" {
					t.Errorf("queried foreign checkpoint: %s", r.URL.Path)
				}
				_, _ = io.WriteString(w, `{"checkpoints":[]}`)
			})
			record, _ := checkpointRecordFromCoordinator(managedCheckpointFixture("chk_foreign_verify"), "https://other-coordinator.example")
			if err := store.Write(record); err != nil {
				t.Fatal(err)
			}
			args := []string{"--verify", "--json"}
			if localOnly {
				args = append(args, "--local-only")
			}
			var stdout bytes.Buffer
			if err := (App{Stdout: &stdout, Stderr: io.Discard}).checkpointList(context.Background(), args); err != nil {
				t.Fatal(err)
			}
			var audits []checkpointAudit
			if err := json.Unmarshal(stdout.Bytes(), &audits); err != nil {
				t.Fatal(err)
			}
			if len(audits) != 1 || audits[0].ProviderState != "unknown" || !strings.Contains(audits[0].Error, "belongs to coordinator") {
				t.Fatalf("foreign audit: %s", stdout.String())
			}
			if localOnly && calls != 0 || !localOnly && calls != 1 {
				t.Fatalf("wrong-authority requests: %d", calls)
			}
		})
	}
}

func TestCheckpointManagedMutationsRejectResponseIDMismatch(t *testing.T) {
	for _, operation := range []string{"create", "retention"} {
		t.Run(operation, func(t *testing.T) {
			other := managedCheckpointFixture("chk_wrong_response")
			server, _ := configureManagedCheckpointTest(t, func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"checkpoint": other, "image": CoordinatorImage{ID: "snap-wrong"}})
			})
			client := &CoordinatorClient{BaseURL: server.URL, Client: server.Client()}
			var err error
			if operation == "create" {
				_, _, err = client.CreateCheckpoint(context.Background(), checkpointRecord{ID: "chk_requested"}, "fixture", checkpointStrategyDiskSnapshot, true, checkpointRetentionFromDuration(0))
			} else {
				_, err = client.UpdateCheckpointRetention(context.Background(), "chk_requested", checkpointRetentionFromDuration(0))
			}
			if err == nil || !strings.Contains(err.Error(), "requested checkpoint") {
				t.Fatalf("accepted wrong response: %v", err)
			}
		})
	}
}
