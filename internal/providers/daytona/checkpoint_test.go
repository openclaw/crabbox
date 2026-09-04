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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	api "github.com/daytonaio/daytona/libs/api-client-go"
	core "github.com/openclaw/crabbox/internal/cli"
)

type snapshotFixture struct {
	*daytonaLifecycleFixture
	snapshotMu                              sync.Mutex
	snapshot                                *api.SnapshotDto
	creates, starts, stops, removes, reads  int
	lostResponse, failSnapshot              bool
	snapshotResponseCanceled                chan struct{}
	stallSnapshotReads                      bool
	rejectCreate                            bool
	transientReadFailure, replaceInRecovery bool
	request                                 core.NativeCheckpointCreateRequest
}

func newSnapshotFixture(t *testing.T) *snapshotFixture {
	t.Helper()
	f, b, repo := newDaytonaLifecycleFixture(t)
	sandbox, leaseID, _, err := b.createDaytonaToolboxSandbox(t.Context(), repo, true, false, "")
	if err != nil {
		t.Fatal(err)
	}
	f.sandbox.SetOrganizationId("org-test")
	f.sandbox.SetUser("daytona")
	s := &snapshotFixture{daytonaLifecycleFixture: f, request: core.NativeCheckpointCreateRequest{Config: b.cfg, Server: core.Server{Provider: daytonaProvider, CloudID: sandbox.ID}, LeaseID: leaseID, CheckpointID: "chk_0123456789abcdef", RepoName: repo.Name, WaitTimeout: 5 * time.Second, Stderr: io.Discard}}
	oldInterval, oldRecovery := checkpointPollInterval, checkpointRecoveryTimeout
	checkpointPollInterval, checkpointRecoveryTimeout = time.Millisecond, 5*time.Second
	t.Cleanup(func() { checkpointPollInterval, checkpointRecoveryTimeout = oldInterval, oldRecovery })
	original := f.server.Config.Handler
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.snapshotMu.Lock()
		defer s.snapshotMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/process/execute"):
			_, _ = io.WriteString(w, `{"result":"","exitCode":0}`)
		case r.URL.Path == "/sandbox/sandbox-test/stop":
			s.stops++
			f.sandbox.SetState(api.SANDBOXSTATE_STOPPED)
			_ = json.NewEncoder(w).Encode(f.sandbox)
		case r.URL.Path == "/sandbox/sandbox-test/start":
			s.starts++
			if s.snapshot != nil && s.snapshot.GetState() != api.SNAPSHOTSTATE_ACTIVE && !daytonaSnapshotFailed(s.snapshot.GetState()) {
				t.Error("restarted before snapshot completed")
			}
			f.sandbox.SetState(api.SANDBOXSTATE_STARTED)
			_ = json.NewEncoder(w).Encode(f.sandbox)
		case r.URL.Path == "/sandbox/sandbox-test/snapshot":
			s.creates++
			if s.rejectCreate {
				w.WriteHeader(403)
				_, _ = io.WriteString(w, `{"message":"snapshot permission denied"}`)
				return
			}
			var body api.CreateSandboxSnapshot
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body.GetIncludeMemory() {
				t.Error("unexpected memory snapshot")
			}
			if f.sandbox.GetState() != api.SANDBOXSTATE_STOPPED {
				t.Error("snapshot source must be stopped")
			}
			s.snapshot = &api.SnapshotDto{Id: "snapshot-exact-id", Name: body.Name, State: api.SNAPSHOTSTATE_PENDING, Entrypoint: []string{}}
			s.snapshot.SetOrganizationId("org-test")
			s.snapshot.SetRegionIds([]string{f.sandbox.GetTarget()})
			if s.snapshotResponseCanceled != nil {
				s.snapshotMu.Unlock()
				<-r.Context().Done()
				s.snapshotMu.Lock()
				close(s.snapshotResponseCanceled)
				return
			}
			if s.lostResponse {
				w.WriteHeader(502)
				_, _ = io.WriteString(w, `{"message":"response lost"}`)
				return
			}
			_ = json.NewEncoder(w).Encode(f.sandbox)
		case strings.HasPrefix(r.URL.Path, "/snapshots/"):
			if s.snapshot == nil {
				w.WriteHeader(404)
				_, _ = io.WriteString(w, `{"message":"not found"}`)
				return
			}
			if r.Method == "DELETE" {
				if r.URL.Path != "/snapshots/snapshot-exact-id" {
					t.Error("deletion must use exact ID")
				}
				s.removes++
				s.snapshot = nil
				w.WriteHeader(204)
				return
			}
			s.reads++
			if s.reads == 3 && s.transientReadFailure {
				w.WriteHeader(503)
				_, _ = io.WriteString(w, `{"message":"transient read failure"}`)
				return
			}
			if s.reads > 3 && s.replaceInRecovery {
				s.snapshot.SetId("replacement-snapshot")
			}
			if s.reads == 1 {
				w.WriteHeader(404)
				_, _ = io.WriteString(w, `{"message":"not visible yet"}`)
				return
			}
			if s.reads > 2 {
				if s.stallSnapshotReads {
					s.snapshotMu.Unlock()
					<-r.Context().Done()
					s.snapshotMu.Lock()
					return
				}
				s.snapshot.SetState(api.SNAPSHOTSTATE_ACTIVE)
				if s.failSnapshot {
					s.snapshot.SetState(api.SNAPSHOTSTATE_ERROR)
				}
			}
			_ = json.NewEncoder(w).Encode(s.snapshot)
		default:
			original.ServeHTTP(w, r)
		}
	})
	return s
}

func TestDaytonaSnapshotRequiresStopConsentAndOwnership(t *testing.T) {
	for _, test := range []string{"no-reboot", "claim", "labels", "organization", "broker", "name"} {
		t.Run(test, func(t *testing.T) {
			f := newSnapshotFixture(t)
			req := f.request
			switch test {
			case "no-reboot":
				req.NoReboot = true
			case "claim":
				core.RemoveLeaseClaim(req.LeaseID)
			case "labels":
				f.sandbox.Labels["lease"] = "cbx_aaaaaaaaaaaa"
			case "organization":
				f.sandbox.SetOrganizationId("")
			case "broker":
				req.Config.Coordinator = "https://broker.example"
			case "name":
				req.Name = "../invalid"
			}
			if _, err := (Provider{}).CreateNativeCheckpoint(t.Context(), req); err == nil {
				t.Fatal("expected refusal")
			}
			if f.stops+f.creates+f.starts != 0 {
				t.Fatal("refusal mutated provider")
			}
		})
	}
}

func TestDaytonaSnapshotLifecycleWaitsEvenWithoutWaitFlag(t *testing.T) {
	for _, stopped := range []bool{false, true} {
		t.Run(map[bool]string{false: "running", true: "stopped"}[stopped], func(t *testing.T) {
			f := newSnapshotFixture(t)
			if stopped {
				f.sandbox.SetState(api.SANDBOXSTATE_STOPPED)
			}
			result, err := (Provider{}).CreateNativeCheckpoint(t.Context(), f.request)
			if err != nil {
				t.Fatal(err)
			}
			if result.Image.ID != "snapshot-exact-id" || result.Image.State != "active" || result.Metadata["snapshot_id"] != result.Image.ID {
				t.Fatalf("result=%+v", result)
			}
			if f.creates != 1 || f.reads < 3 {
				t.Fatal("snapshot not captured and waited")
			}
			if stopped && (f.starts != 0 || f.stops != 0) || !stopped && (f.starts != 1 || f.stops != 1) {
				t.Fatal("source state not preserved")
			}
			resource := core.NativeCheckpointResourceRequest{LoadConfig: func() (Config, error) { return f.request.Config, nil }, Image: result.Image, Metadata: result.Metadata}
			verified, err := (Provider{}).VerifyNativeCheckpoint(t.Context(), resource)
			if err != nil || verified.NextAction != "fork_or_delete" {
				t.Fatalf("verify=%+v err=%v", verified, err)
			}
			cfg := f.request.Config
			configPath := filepath.Join(t.TempDir(), "malformed.yaml")
			if err := os.WriteFile(configPath, []byte("daytona: [\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("CRABBOX_CONFIG", configPath)
			err = (Provider{}).ApplyNativeCheckpointForkConfig(core.NativeCheckpointForkRequest{Config: &cfg, Record: core.NativeCheckpointForkRecord{Kind: result.Image.Kind, ImageID: result.Image.ID, Name: result.Image.Name, Direct: true, Metadata: result.Metadata}})
			if err != nil || cfg.Daytona.Snapshot != result.Image.ID || cfg.WorkRoot != f.request.Config.Daytona.WorkRoot {
				t.Fatalf("fork err=%v snapshot=%s workRoot=%s", err, cfg.Daytona.Snapshot, cfg.WorkRoot)
			}
			if err := (Provider{}).DeleteNativeCheckpoint(t.Context(), resource); err != nil {
				t.Fatal(err)
			}
			if f.removes != 1 {
				t.Fatal("snapshot not deleted")
			}
		})
	}
}

func TestDaytonaClassForkKeepsCapturedSnapshot(t *testing.T) {
	f := newSnapshotFixture(t)
	result, err := (Provider{}).CreateNativeCheckpoint(t.Context(), f.request)
	if err != nil {
		t.Fatal(err)
	}
	f.snapshot.SetCpu(2)
	f.snapshot.SetMem(4)
	f.snapshot.SetDisk(8)
	f.snapshot.SetSandboxClass("container")
	cfg := f.request.Config
	cfg.Class = "standard"
	if err := (Provider{}).ApplyNativeCheckpointForkConfig(core.NativeCheckpointForkRequest{Config: &cfg, Record: core.NativeCheckpointForkRecord{Kind: result.Image.Kind, ImageID: result.Image.ID, Name: result.Image.Name, Direct: true, Metadata: result.Metadata}}); err != nil {
		t.Fatal(err)
	}
	f.classSnapshot = f.snapshot
	claim, _, err := resolveLeaseClaimForProvider(f.request.LeaseID, daytonaProvider)
	if err != nil {
		t.Fatal(err)
	}
	if err := runDaytonaClassWarmup(t, cfg, Repo{Root: claim.RepoRoot}); err != nil {
		t.Fatal(err)
	}
	if f.create.GetSnapshot() != result.Image.ID || f.sandboxCreates != 2 {
		t.Fatalf("fork replaced captured filesystem: snapshot=%s creates=%d", f.create.GetSnapshot(), f.sandboxCreates)
	}
}

func TestDaytonaDirectCheckpointClassRestoresRoutingBeforeValidation(t *testing.T) {
	f := newSnapshotFixture(t)
	result, err := (Provider{}).CreateNativeCheckpoint(t.Context(), f.request)
	if err != nil {
		t.Fatal(err)
	}
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("direct checkpoint reached broker: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer broker.Close()
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	config := fmt.Sprintf("provider: daytona\nnetwork: public\ncoordinator: %s\ncoordinatorToken: fixture-broker-token\ndaytona:\n  apiUrl: %s\n", broker.URL, f.server.URL)
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRABBOX_CONFIG", configPath)
	t.Setenv("CRABBOX_DAYTONA_API_KEY", f.request.Config.Daytona.APIKey)
	stateDir, err := core.CrabboxStateDir()
	if err != nil {
		t.Fatal(err)
	}
	checkpointDir := filepath.Join(stateDir, "checkpoints", f.request.CheckpointID)
	if err := os.MkdirAll(checkpointDir, 0700); err != nil {
		t.Fatal(err)
	}
	// Load the existing public checkpoint artifact through the real fork parser.
	record := map[string]any{
		"id": f.request.CheckpointID, "kind": result.Image.Kind, "provider": daytonaProvider, "targetOs": "linux",
		"native": map[string]any{"direct": true, "provider": daytonaProvider, "imageId": result.Image.ID, "name": result.Image.Name, "metadata": result.Metadata},
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkpointDir, "checkpoint.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	err = (core.App{Stdout: &stdout, Stderr: io.Discard}).Run(t.Context(), []string{"checkpoint", "fork", f.request.CheckpointID, "--provider", "daytona", "--class", "standard", "--dry-run"})
	if err != nil || !strings.Contains(stdout.String(), "resource="+result.Image.ID) {
		t.Fatalf("direct checkpoint route: output=%q err=%v", stdout.String(), err)
	}
}

func TestDaytonaSnapshotFailureRetainsIdentityAndRestoresSource(t *testing.T) {
	for _, failure := range []string{"lost-response", "http-timeout", "http-timeout-unconfirmed", "snapshot-error", "unconfirmed"} {
		t.Run(failure, func(t *testing.T) {
			f := newSnapshotFixture(t)
			f.lostResponse = failure == "lost-response"
			f.failSnapshot = failure == "snapshot-error"
			httpTimeout := strings.HasPrefix(failure, "http-timeout")
			if httpTimeout {
				f.snapshotResponseCanceled = make(chan struct{})
				f.stallSnapshotReads = failure == "http-timeout-unconfirmed"
			}
			var stalled *snapshotDeadlineClient
			if failure == "unconfirmed" || httpTimeout {
				original := newDaytonaClient
				newDaytonaClient = func(cfg Config, rt Runtime) (daytonaAPI, error) {
					client, err := original(cfg, rt)
					if err != nil {
						return nil, err
					}
					if httpTimeout {
						client.(*daytonaSDKClient).api.GetConfig().HTTPClient.Timeout = 250 * time.Millisecond
						return client, nil
					}
					stalled = &snapshotDeadlineClient{daytonaSnapshotAPI: client.(daytonaSnapshotAPI)}
					return stalled, nil
				}
				t.Cleanup(func() { newDaytonaClient = original })
			}
			ctx := t.Context()
			if httpTimeout {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 5*time.Second)
				defer cancel()
			}
			result, err := (Provider{}).CreateNativeCheckpoint(ctx, f.request)
			if httpTimeout {
				if !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
					t.Fatalf("snapshot request did not reach its client timeout before the caller deadline: %v", err)
				}
				select {
				case <-f.snapshotResponseCanceled:
				case <-ctx.Done():
					t.Fatal("accepted snapshot request did not observe HTTP cancellation")
				}
			}
			f.snapshotMu.Lock()
			defer f.snapshotMu.Unlock()
			if err == nil || result.Image.ID == "" || result.Metadata["source"] == "" {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if f.creates != 1 {
				t.Fatal("allocation was retried")
			}
			if httpTimeout && (result.Image.ID != "snapshot-exact-id" || result.Image.Name != "crabbox-"+f.request.CheckpointID || result.Metadata["snapshot_id"] != result.Image.ID || result.Metadata["source"] != f.request.Server.CloudID) {
				t.Fatalf("accepted snapshot lost its exact recovery identity: %+v", result)
			}
			if failure == "unconfirmed" || f.stallSnapshotReads {
				if f.starts != 0 || f.stops != 1 || f.sandbox.GetState() != api.SANDBOXSTATE_STOPPED || !strings.Contains(err.Error(), "left stopped") {
					t.Fatalf("unsafe restart: %v", err)
				}
				if failure == "unconfirmed" && (stalled == nil || stalled.deadlineReads != 2 || !errors.Is(err, context.DeadlineExceeded)) {
					t.Fatalf("snapshot polling and recovery did not both observe the injected deadline: %v", err)
				}
				if f.stallSnapshotReads && (f.reads != 4 || result.Image.State != "pending") {
					t.Fatalf("snapshot polling and recovery did not preserve pending identity: reads=%d result=%+v", f.reads, result)
				}
			} else if f.starts != 1 || result.Image.ID != "snapshot-exact-id" {
				t.Fatalf("source not restored or identity lost: %+v %v", result, err)
			}
		})
	}
}

// Inject the failure after real HTTP setup and creation, not while ownership
// lookup or source stopping is competing with a short wall-clock deadline.
type snapshotDeadlineClient struct {
	daytonaSnapshotAPI
	created       bool
	deadlineReads int
}

func (c *snapshotDeadlineClient) CreateSnapshot(ctx context.Context, id, name string) error {
	err := c.daytonaSnapshotAPI.CreateSnapshot(ctx, id, name)
	c.created = true
	return err
}

func (c *snapshotDeadlineClient) GetSnapshot(ctx context.Context, id string) (*api.SnapshotDto, error) {
	if c.created {
		c.deadlineReads++
		return nil, context.DeadlineExceeded
	}
	return c.daytonaSnapshotAPI.GetSnapshot(ctx, id)
}

func TestDaytonaSnapshotWaitDeadlineRetainsLastIdentity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := &pendingSnapshotClient{snapshot: &api.SnapshotDto{Id: "snapshot-exact-id", Name: "pending", State: api.SNAPSHOTSTATE_PENDING}}
		client.snapshot.SetOrganizationId("org-test")
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		started := time.Now()
		result, err := waitDaytonaSnapshot(ctx, client, "pending", "org-test", "snapshot-exact-id")
		if !errors.Is(err, context.DeadlineExceeded) || result != client.snapshot || client.reads < 2 {
			t.Fatalf("deadline lost pending snapshot identity: result=%+v reads=%d err=%v", result, client.reads, err)
		}
		if elapsed := time.Since(started); elapsed != 5*time.Second {
			t.Fatalf("wait exceeded virtual deadline: %s", elapsed)
		}
	})
}

type pendingSnapshotClient struct {
	daytonaSnapshotAPI
	snapshot *api.SnapshotDto
	reads    int
}

func (c *pendingSnapshotClient) GetSnapshot(context.Context, string) (*api.SnapshotDto, error) {
	c.reads++
	return c.snapshot, nil
}

func TestDaytonaSnapshotDeletionRejectsIdentityAndScopeDrift(t *testing.T) {
	for _, drift := range []string{"api", "organization", "id", "name", "general", "missing", "unconfirmed"} {
		t.Run(drift, func(t *testing.T) {
			f := newSnapshotFixture(t)
			result, err := (Provider{}).CreateNativeCheckpoint(t.Context(), f.request)
			if err != nil {
				t.Fatal(err)
			}
			resource := core.NativeCheckpointResourceRequest{LoadConfig: func() (Config, error) { return f.request.Config, nil }, Image: result.Image, Metadata: result.Metadata}
			switch drift {
			case "api":
				resource.Metadata["api_url"] = "https://other.example/api"
			case "organization":
				f.snapshot.SetOrganizationId("other-org")
			case "id":
				f.snapshot.SetId("replacement")
			case "name":
				f.snapshot.SetName("replacement")
			case "general":
				f.snapshot.SetGeneral(true)
			case "missing":
				f.snapshot = nil
			case "unconfirmed":
				delete(resource.Metadata, "snapshot_id")
			}
			if err := (Provider{}).DeleteNativeCheckpoint(t.Context(), resource); err == nil {
				t.Fatal("expected refusal")
			}
			if f.removes != 0 {
				t.Fatal("deleted mismatched snapshot")
			}
		})
	}
}

func TestDaytonaSnapshotRejectedRequestRestartsWithoutWaitingForMissingSnapshot(t *testing.T) {
	f := newSnapshotFixture(t)
	f.rejectCreate = true
	result, err := (Provider{}).CreateNativeCheckpoint(t.Context(), f.request)
	if err == nil || result.Image.ID != "" || f.starts != 1 || f.creates != 1 {
		t.Fatalf("result=%+v err=%v starts=%d creates=%d", result, err, f.starts, f.creates)
	}
}

func TestDaytonaCheckpointCLILeavesStoppedSourceStopped(t *testing.T) {
	f := newSnapshotFixture(t)
	f.sandbox.SetState(api.SANDBOXSTATE_STOPPED)
	claim, _, err := resolveLeaseClaimForProvider(f.request.LeaseID, daytonaProvider)
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(claim.RepoRoot)
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	config := fmt.Sprintf("provider: daytona\ntarget: linux\ndaytona:\n  apiUrl: %s\n  apiKey: test-credential\n", f.server.URL)
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRABBOX_CONFIG", configPath)
	t.Setenv("CRABBOX_DAYTONA_API_KEY", "test-credential")
	var stdout, stderr bytes.Buffer
	app := core.App{Stdout: &stdout, Stderr: &stderr}
	if err := app.Run(t.Context(), []string{"checkpoint", "create", "--provider", "daytona", "--id", f.request.LeaseID, "--mode", "native", "--json", "--reclaim"}); err != nil {
		t.Fatalf("create: %v %s", err, stderr.String())
	}
	var record struct{ ID, Kind string }
	if err := json.Unmarshal(stdout.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	if record.Kind != core.CheckpointKindDaytona || record.ID == "" || f.starts != 0 || f.stops != 0 {
		t.Fatalf("record=%+v starts=%d stops=%d", record, f.starts, f.stops)
	}
	stdout.Reset()
	if err := app.Run(t.Context(), []string{"checkpoint", "inspect", record.ID, "--verify", "--json"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), `"providerState":"active"`) {
		t.Fatal(stdout.String())
	}
	metaPath := filepath.Join(os.Getenv("XDG_STATE_HOME"), "crabbox", "checkpoints", record.ID, "checkpoint.json")
	before, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte("daytona: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reads := f.reads
	stdout.Reset()
	if err := app.Run(t.Context(), []string{"checkpoint", "inspect", record.ID, "--verify", "--json"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), `"providerState":"unknown"`) || !strings.Contains(stdout.String(), "parse config") {
		t.Fatalf("invalid config did not fail verification: %s", &stdout)
	}
	if err := app.Run(t.Context(), []string{"checkpoint", "delete", record.ID}); err == nil || !strings.Contains(err.Error(), "parse config") {
		t.Fatalf("invalid config did not fail deletion: %v", err)
	}
	after, err := os.ReadFile(metaPath)
	if err != nil || !bytes.Equal(before, after) || f.reads != reads || f.removes != 0 {
		t.Fatalf("invalid config changed provider or record: reads=%d removes=%d err=%v", f.reads-reads, f.removes, err)
	}
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := app.Run(t.Context(), []string{"checkpoint", "delete", record.ID}); err != nil {
		t.Fatal(err)
	}
	if f.removes != 1 {
		t.Fatal("snapshot not deleted through CLI")
	}
}

func TestDaytonaSnapshotRecoveryPreservesIdentityAndHandlesTerminalFailure(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		t.Run(map[bool]string{false: "terminal failure", true: "replacement"}[replacement], func(t *testing.T) {
			f := newSnapshotFixture(t)
			f.transientReadFailure = true
			f.replaceInRecovery = replacement
			f.failSnapshot = !replacement
			result, err := (Provider{}).CreateNativeCheckpoint(t.Context(), f.request)
			if err == nil || result.Image.ID != "snapshot-exact-id" || result.Metadata["snapshot_id"] != "snapshot-exact-id" {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if replacement {
				if f.starts != 0 || !strings.Contains(err.Error(), "identity changed") {
					t.Fatalf("replacement authorized restart or escaped detection: starts=%d err=%v", f.starts, err)
				}
			} else if f.starts != 1 || result.Image.State != "error" || !strings.Contains(err.Error(), "state=error") {
				t.Fatalf("terminal recovery failure did not restore source: starts=%d result=%+v err=%v", f.starts, result, err)
			}
		})
	}
}
