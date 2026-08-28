package daytona

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	api "github.com/daytonaio/daytona/libs/api-client-go"
	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
)

type daytonaLifecycleFixture struct {
	mu            sync.Mutex
	server        *httptest.Server
	sandbox       *api.Sandbox
	create        api.CreateSandbox
	createState   api.SandboxState
	lostCreate    bool
	recoveryDelay int
	recoveryReads int
	deletes       int
	activity      int
	autoStop      string
	autoStopError bool
	deleteError   bool
	paths         []string
}

func newDaytonaLifecycleFixture(t *testing.T) (*daytonaLifecycleFixture, *daytonaLeaseBackend, Repo) {
	t.Helper()
	testutil.IsolateUserDirs(t)
	f := &daytonaLifecycleFixture{createState: api.SANDBOXSTATE_STARTED}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.paths = append(f.paths, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/sandbox":
			items := []*api.Sandbox{}
			if f.sandbox != nil && f.sandbox.GetState() != api.SANDBOXSTATE_DESTROYED {
				items = append(items, f.sandbox)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"items": items, "nextCursor": nil})
		case r.Method == "POST" && r.URL.Path == "/sandbox":
			if err := json.NewDecoder(r.Body).Decode(&f.create); err != nil {
				t.Error(err)
			}
			f.sandbox = &api.Sandbox{}
			f.sandbox.SetId("sandbox-test")
			f.sandbox.SetName(f.create.GetName())
			f.sandbox.SetLabels(f.create.GetLabels())
			f.sandbox.SetState(f.createState)
			f.sandbox.SetToolboxProxyUrl(f.server.URL + "/toolbox")
			f.sandbox.SetAutoStopInterval(float32(f.create.GetAutoStopInterval()))
			if f.lostCreate {
				w.WriteHeader(http.StatusBadGateway)
				_, _ = io.WriteString(w, `{"message":"response lost after allocation"}`)
				return
			}
			_ = json.NewEncoder(w).Encode(f.sandbox)
		case r.Method == "GET" && r.URL.Path == "/sandbox/sandbox-test":
			_ = json.NewEncoder(w).Encode(f.sandbox)
		case r.Method == "GET" && f.sandbox != nil && r.URL.Path == "/sandbox/"+f.sandbox.GetName():
			f.recoveryReads++
			if f.recoveryReads <= f.recoveryDelay {
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `{"message":"allocation not visible yet"}`)
				return
			}
			_ = json.NewEncoder(w).Encode(f.sandbox)
		case r.Method == "DELETE" && r.URL.Path == "/sandbox/sandbox-test":
			f.deletes++
			if f.deleteError {
				w.WriteHeader(503)
				_, _ = io.WriteString(w, `{"message":"temporary cleanup failure"}`)
				return
			}
			f.sandbox.SetState(api.SANDBOXSTATE_DESTROYED)
			_ = json.NewEncoder(w).Encode(f.sandbox)
		case strings.HasSuffix(r.URL.Path, "/labels"):
			var body api.SandboxLabels
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.sandbox.SetLabels(body.GetLabels())
			_ = json.NewEncoder(w).Encode(f.sandbox)
		case strings.Contains(r.URL.Path, "/autostop/"):
			if f.autoStopError {
				w.WriteHeader(503)
				_, _ = io.WriteString(w, `{"message":"policy update failed"}`)
				return
			}
			f.autoStop = filepath.Base(r.URL.Path)
			_ = json.NewEncoder(w).Encode(f.sandbox)
		case strings.HasSuffix(r.URL.Path, "/last-activity"):
			f.activity++
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/files/upload"), strings.HasSuffix(r.URL.Path, "/files/bulk-upload"):
			if err := r.ParseMultipartForm(8 << 20); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			defer r.MultipartForm.RemoveAll()
			field, destination := "file", r.URL.Query().Get("path")
			if strings.HasSuffix(r.URL.Path, "/files/bulk-upload") {
				field, destination = "files[0].file", r.FormValue("files[0].path")
			}
			file, _, err := r.FormFile(field)
			if err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			defer file.Close()
			data, err := io.ReadAll(file)
			if err == nil {
				err = os.WriteFile(destination, data, 0600)
			}
			if err != nil {
				t.Error(err)
				w.WriteHeader(500)
				return
			}
			_, _ = io.WriteString(w, `{}`)
		case strings.HasSuffix(r.URL.Path, "/process/execute"):
			var body struct {
				Command string `json:"command"`
				Cwd     string `json:"cwd"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			command := exec.CommandContext(r.Context(), "bash", "-c", body.Command)
			command.Dir = body.Cwd
			out, err := command.CombinedOutput()
			code := 0
			if err != nil {
				code = 1
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"result": string(out), "exitCode": code})
		default:
			t.Errorf("unexpected Daytona request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(f.server.Close)
	cfg := baseConfig()
	cfg.Provider = daytonaProvider
	cfg.Daytona.APIKey = "test-credential"
	cfg.Daytona.APIURL = f.server.URL
	cfg.Daytona.Snapshot = "test-snapshot"
	cfg.Daytona.WorkRoot = t.TempDir()
	repo := Repo{Root: t.TempDir(), Name: "fixture"}
	if out, err := exec.Command("git", "init", "-q", repo.Root).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	backend := &daytonaLeaseBackend{cfg: cfg, rt: Runtime{HTTP: f.server.Client(), Stdout: io.Discard, Stderr: io.Discard}}
	return f, backend, repo
}

func TestDaytonaCreationIsPrivateAndHasNativeTTL(t *testing.T) {
	f, b, repo := newDaytonaLifecycleFixture(t)
	sandbox, leaseID, _, err := b.createDaytonaToolboxSandbox(t.Context(), repo, true, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if sandbox.Public || f.create.GetPublic() {
		t.Fatal("sandbox previews must remain private")
	}
	if got := f.create.AdditionalProperties["ttlMinutes"]; got != float64(90) {
		t.Fatalf("ttlMinutes=%v", got)
	}
	if f.create.GetAutoStopInterval() != 30 {
		t.Fatal("idle timeout missing")
	}
	if err := requireExactDaytonaResourceClaim(leaseID, sandbox.ID); err != nil {
		t.Fatal(err)
	}
}

func TestDaytonaAllocationFailureRollsBack(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(map[bool]string{false: "startup failure", true: "lost create response"}[lost], func(t *testing.T) {
			f, b, repo := newDaytonaLifecycleFixture(t)
			f.createState = api.SANDBOXSTATE_ERROR
			f.lostCreate = lost
			if lost {
				f.recoveryDelay = 2
			}
			_, leaseID, _, err := b.createDaytonaToolboxSandbox(t.Context(), repo, false, false, "")
			if err == nil {
				t.Fatal("expected allocation failure")
			}
			if f.deletes != 1 {
				t.Fatalf("deletes=%d, error=%v", f.deletes, err)
			}
			if lost && f.recoveryReads != 3 {
				t.Fatalf("recoveryReads=%d, want delayed allocation recovery", f.recoveryReads)
			}
			if _, exists, err := resolveLeaseClaimForProvider(leaseID, daytonaProvider); err != nil || exists {
				t.Fatalf("claim retained after confirmed cleanup: %v %v", exists, err)
			}
		})
	}
}

func TestDaytonaFailedRollbackRetainsRecoveryClaim(t *testing.T) {
	f, b, repo := newDaytonaLifecycleFixture(t)
	f.createState = api.SANDBOXSTATE_ERROR
	f.deleteError = true
	_, leaseID, _, err := b.createDaytonaToolboxSandbox(t.Context(), repo, false, false, "")
	if err == nil || !strings.Contains(err.Error(), "cleanup failed") || !strings.Contains(err.Error(), leaseID) {
		t.Fatalf("recovery error=%v", err)
	}
	if err := requireExactDaytonaResourceClaim(leaseID, "sandbox-test"); err != nil {
		t.Fatal(err)
	}
}

func TestDaytonaAllocationRecoveryRejectsForeignOwnership(t *testing.T) {
	sandbox := &api.Sandbox{}
	sandbox.SetId("foreign-sandbox")
	sandbox.SetName("expected-name")
	sandbox.SetLabels(map[string]string{"crabbox": "true", "provider": daytonaProvider, "lease": "cbx_222222222222"})
	client := &fakeDaytonaDoctorAPI{getSandboxes: map[string]*api.Sandbox{"expected-name": sandbox}}
	_, err := recoverDaytonaAllocation(t.Context(), client, "expected-name", "cbx_111111111111")
	if err == nil || !strings.Contains(err.Error(), "refusing recovery") || client.mutated {
		t.Fatalf("recovery must not accept or mutate a foreign allocation: err=%v mutated=%v", err, client.mutated)
	}
}

func TestDaytonaAllocationRecoveryIsBounded(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	client := &fakeDaytonaDoctorAPI{}
	_, err := recoverDaytonaAllocation(ctx, client, "missing-name", "cbx_111111111111")
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "allocation unconfirmed") || client.mutated {
		t.Fatalf("recovery must stop without retrying allocation: err=%v mutated=%v", err, client.mutated)
	}
}

func TestDaytonaDeleteWaitsForAlreadyDestroyingSandbox(t *testing.T) {
	f, b, repo := newDaytonaLifecycleFixture(t)
	sandbox, leaseID, _, err := b.createDaytonaSandbox(t.Context(), repo, true, false, "")
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.sandbox.SetState(api.SANDBOXSTATE_DESTROYING)
	f.deleteError = true
	f.mu.Unlock()
	timer := time.AfterFunc(20*time.Millisecond, func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.sandbox.SetState(api.SANDBOXSTATE_DESTROYED)
	})
	defer timer.Stop()
	client, err := newDaytonaClient(b.cfg, b.rt)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := deleteOwnedDaytonaSandbox(ctx, client, sandbox.GetId(), leaseID); err != nil {
		t.Fatal(err)
	}
	if f.deletes != 0 {
		t.Fatalf("must not repeat DELETE while already destroying, got %d calls", f.deletes)
	}
}

func TestDaytonaReadinessIgnoresStaleLabels(t *testing.T) {
	for _, state := range []string{"stopped", "archived", "error", "destroying", "starting"} {
		sandbox := &api.Sandbox{}
		sandbox.SetId("sandbox-test")
		sandbox.SetState(api.SandboxState(state))
		sandbox.SetLabels(map[string]string{"state": "ready"})
		view := daytonaStatusView("cbx_111111111111", sandbox, baseConfig())
		if view.Ready || view.State != state {
			t.Fatalf("provider state %s rendered %+v", state, view)
		}
	}
}

func TestDaytonaHeartbeatUpdatesProviderAndLabels(t *testing.T) {
	f, b, repo := newDaytonaLifecycleFixture(t)
	sandbox, leaseID, _, err := b.createDaytonaSandbox(t.Context(), repo, true, false, "")
	if err != nil {
		t.Fatal(err)
	}
	idle := 90 * time.Minute
	touched, err := b.Touch(t.Context(), TouchRequest{Lease: LeaseTarget{LeaseID: leaseID, Server: daytonaSandboxToServer(sandbox, b.cfg)}, State: "ready", IdleTimeoutOverride: &idle})
	if err != nil {
		t.Fatal(err)
	}
	if f.autoStop != "90" || touched.Labels["idle_timeout_secs"] != "5400" || f.activity != 1 {
		t.Fatalf("autoStop=%s labels=%v activity=%d", f.autoStop, touched.Labels, f.activity)
	}
}

func TestDaytonaHeartbeatPolicyFailureDoesNotPublishNewTimeout(t *testing.T) {
	f, b, repo := newDaytonaLifecycleFixture(t)
	sandbox, leaseID, _, err := b.createDaytonaSandbox(t.Context(), repo, true, false, "")
	if err != nil {
		t.Fatal(err)
	}
	f.autoStopError = true
	idle := 90 * time.Minute
	_, err = b.Touch(t.Context(), TouchRequest{Lease: LeaseTarget{LeaseID: leaseID, Server: daytonaSandboxToServer(sandbox, b.cfg)}, State: "ready", IdleTimeoutOverride: &idle})
	if err == nil || !strings.Contains(err.Error(), "auto-stop") {
		t.Fatalf("error=%v", err)
	}
	if f.sandbox.GetLabels()["idle_timeout_secs"] != "1800" {
		t.Fatal("published timeout that provider did not apply")
	}
}

func TestDaytonaStatusWaitFailsOnTerminalProviderState(t *testing.T) {
	f, b, repo := newDaytonaLifecycleFixture(t)
	_, leaseID, _, err := b.createDaytonaSandbox(t.Context(), repo, true, false, "")
	if err != nil {
		t.Fatal(err)
	}
	f.sandbox.SetState(api.SANDBOXSTATE_ERROR)
	view, err := b.Status(t.Context(), StatusRequest{ID: leaseID, Wait: true, WaitTimeout: time.Second})
	if err == nil || view.Ready || !strings.Contains(err.Error(), "terminal state=error") {
		t.Fatalf("view=%+v error=%v", view, err)
	}
}

func TestDaytonaActivityRefreshStopsWithRun(t *testing.T) {
	f, b, repo := newDaytonaLifecycleFixture(t)
	sandbox, _, _, err := b.createDaytonaSandbox(t.Context(), repo, true, false, "")
	if err != nil {
		t.Fatal(err)
	}
	sandbox.SetAutoStopInterval(0)
	b.cfg.IdleTimeout = 3 * time.Second
	stop, err := b.startDaytonaActivity(t.Context(), sandbox)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		f.mu.Lock()
		calls := f.activity
		f.mu.Unlock()
		if calls >= 2 {
			break
		}
		if time.Now().After(deadline) {
			stop()
			t.Fatal("activity was not refreshed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	stop()
	f.mu.Lock()
	calls := f.activity
	f.mu.Unlock()
	time.Sleep(1100 * time.Millisecond)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.activity != calls {
		t.Fatal("activity continued after run stopped")
	}
}

func TestDaytonaRunPreservesDependenciesAndPrunesDeletedSource(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executes Linux toolbox shell commands against local POSIX paths")
	}
	f, b, repo := newDaytonaLifecycleFixture(t)
	if err := os.WriteFile(filepath.Join(repo.Root, "source.txt"), []byte("first"), 0600); err != nil {
		t.Fatal(err)
	}
	_, leaseID, _, err := b.createDaytonaToolboxSandbox(t.Context(), repo, true, false, "")
	if err != nil {
		t.Fatal(err)
	}
	req := RunRequest{ID: leaseID, Repo: repo, ShellMode: true, Command: []string{"mkdir -p node_modules/example && printf installed > node_modules/example/index.js && test -f source.txt"}}
	if _, err := b.Run(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(repo.Root, "source.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo.Root, "next.txt"), []byte("second"), 0600); err != nil {
		t.Fatal(err)
	}
	req.Command = []string{"test -f node_modules/example/index.js && test ! -e source.txt && test -f next.txt"}
	if _, err := b.Run(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if f.deletes != 0 {
		t.Fatal("reused sandbox was deleted")
	}
}

func TestDaytonaArchivePruneRejectsUnsafePaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executes Linux toolbox shell commands against local POSIX paths")
	}
	for _, unsafe := range []string{"../outside", "/outside", "dir/../../outside", ".git/config"} {
		t.Run(unsafe, func(t *testing.T) {
			root := t.TempDir()
			meta := filepath.Join(root, ".crabbox")
			if err := os.Mkdir(meta, 0700); err != nil {
				t.Fatal(err)
			}
			for name, data := range map[string][]byte{"sync-manifest": []byte(unsafe + "\x00"), "sync-manifest.abc.new": {}, "sync-deleted.abc.new": {}} {
				if err := os.WriteFile(filepath.Join(meta, name), data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			out, err := exec.Command("bash", "-c", core.PruneArchiveSyncManifestCommand(root, "abc", false)).CombinedOutput()
			if err == nil || !strings.Contains(string(out), "unsafe overlay deletion path") {
				t.Fatalf("unsafe prune: %v %s", err, out)
			}
		})
	}
}

func TestDaytonaHTTPRedirectPolicy(t *testing.T) {
	for _, surface := range []string{"api", "process", "upload"} {
		t.Run(surface, func(t *testing.T) {
			destinationCalls := 0
			destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { destinationCalls++; w.WriteHeader(200) }))
			defer destination.Close()
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
			}))
			defer source.Close()
			cfg := baseConfig()
			cfg.Daytona.APIURL = source.URL
			cfg.Daytona.APIKey = "test-credential"
			var err error
			switch surface {
			case "api":
				client, e := newDaytonaClient(cfg, Runtime{})
				if e != nil {
					t.Fatal(e)
				}
				_, err = client.GetSandbox(t.Context(), "sandbox-test")
			case "process":
				dto := &api.Sandbox{}
				dto.SetId("sandbox-test")
				dto.SetToolboxProxyUrl(source.URL)
				sandbox, e := newDaytonaToolboxSandbox(cfg, Runtime{}, dto)
				if e != nil {
					t.Fatal(e)
				}
				_, err = newDaytonaCommandRunner(sandbox).ExecuteCommand(t.Context(), "true")
			case "upload":
				err = uploadDaytonaFileStream(t.Context(), source.Client(), source.URL+"?path=file", map[string]string{"Authorization": "Bearer test-credential"}, strings.NewReader("content"), "file")
			}
			// Streaming requests need not be replayable; either way the other origin is never contacted.
			if destinationCalls != 0 {
				t.Fatal("redirect contacted another origin")
			}
			if surface != "upload" && (err == nil || !strings.Contains(err.Error(), "cross-origin")) {
				t.Fatalf("redirect error=%v", err)
			}
		})
	}
}
