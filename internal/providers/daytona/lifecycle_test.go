package daytona

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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
	commands      []daytonaTestCommand
	uploads       []string
	uploadError   bool
	uploadCancel  context.CancelFunc
	privateUpload bool
}

type daytonaTestCommand struct {
	Command string            `json:"command"`
	Cwd     string            `json:"cwd"`
	Envs    map[string]string `json:"envs"`
	Timeout int               `json:"timeout"`
}

func newDaytonaLifecycleFixture(t *testing.T) (*daytonaLifecycleFixture, *daytonaLeaseBackend, Repo) {
	t.Helper()
	f, backend, repo := newDaytonaFixture(t)
	if out, err := exec.Command("git", "init", "-q", repo.Root).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	return f, backend, repo
}

func newDaytonaFixture(t *testing.T) (*daytonaLifecycleFixture, *daytonaLeaseBackend, Repo) {
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
			f.sandbox.SetOrganizationId("org-test")
			f.sandbox.SetSnapshot(f.create.GetSnapshot())
			if f.create.GetSnapshot() == "snapshot-exact-id" {
				f.sandbox.SetSnapshot("test-snapshot")
			}
			f.sandbox.SetUser(f.create.GetUser())
			f.sandbox.SetTarget(f.create.GetTarget())
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
		case r.Method == "POST" && r.URL.Path == "/sandbox/sandbox-test/start":
			f.sandbox.SetState(api.SANDBOXSTATE_STARTED)
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
			f.uploads = append(f.uploads, destination)
			if parent, statErr := os.Stat(filepath.Dir(destination)); statErr == nil {
				f.privateUpload = parent.Mode().Perm() == 0700
			}
			if err == nil {
				err = os.WriteFile(destination, data, 0600)
			}
			if err != nil {
				t.Error(err)
				w.WriteHeader(500)
				return
			}
			if f.uploadCancel != nil {
				f.uploadCancel()
			}
			if f.uploadError {
				w.WriteHeader(http.StatusBadGateway)
				_, _ = io.WriteString(w, `{"message":"upload response failed"}`)
				return
			}
			_, _ = io.WriteString(w, `{}`)
		case strings.HasSuffix(r.URL.Path, "/process/execute"):
			var body daytonaTestCommand
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.commands = append(f.commands, body)
			command := exec.CommandContext(r.Context(), "bash", "-c", body.Command)
			command.Dir = body.Cwd
			command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
			for name, value := range body.Envs {
				command.Env = append(command.Env, name+"="+value)
			}
			out, err := command.CombinedOutput()
			code := 0
			if err != nil {
				code = 1
				var ee *exec.ExitError
				if errors.As(err, &ee) {
					code = ee.ExitCode()
				}
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
	if root, err := filepath.EvalSymlinks(cfg.Daytona.WorkRoot); err == nil {
		cfg.Daytona.WorkRoot = root
	} else {
		t.Fatal(err)
	}
	repo := Repo{Root: t.TempDir(), Name: "fixture"}
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
	sandbox, leaseID, _, err := b.createDaytonaSandbox(t.Context(), AcquireRequest{Repo: repo, Keep: true})
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
	sandbox, leaseID, _, err := b.createDaytonaSandbox(t.Context(), AcquireRequest{Repo: repo, Keep: true})
	if err != nil {
		t.Fatal(err)
	}
	metadata := map[string]string{"provider_fingerprint": strings.Repeat("a", 64), "optional_identity": ""}
	for key, value := range metadata {
		sandbox.GetLabels()[key] = value
	}
	idle := 90 * time.Minute
	touched, err := b.Touch(t.Context(), TouchRequest{Lease: LeaseTarget{LeaseID: leaseID, Server: daytonaSandboxToServer(sandbox, b.cfg)}, State: "ready", IdleTimeoutOverride: &idle})
	if err != nil {
		t.Fatal(err)
	}
	if f.autoStop != "90" || touched.Labels["idle_timeout_secs"] != "5400" || f.activity != 1 {
		t.Fatalf("autoStop=%s labels=%v activity=%d", f.autoStop, touched.Labels, f.activity)
	}
	for key, value := range metadata {
		if f.sandbox.GetLabels()[key] != value || touched.Labels[key] != value {
			t.Errorf("heartbeat changed provider metadata %s: remote=%q response=%q want=%q", key, f.sandbox.GetLabels()[key], touched.Labels[key], value)
		}
	}
}

func TestDaytonaHeartbeatPolicyFailureDoesNotPublishNewTimeout(t *testing.T) {
	f, b, repo := newDaytonaLifecycleFixture(t)
	sandbox, leaseID, _, err := b.createDaytonaSandbox(t.Context(), AcquireRequest{Repo: repo, Keep: true})
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
	_, leaseID, _, err := b.createDaytonaSandbox(t.Context(), AcquireRequest{Repo: repo, Keep: true})
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
	sandbox, _, _, err := b.createDaytonaSandbox(t.Context(), AcquireRequest{Repo: repo, Keep: true})
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

func TestDaytonaRunUploadedScriptPreservesStandaloneContract(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executes Linux toolbox shell commands against local POSIX paths")
	}
	for _, shebang := range []bool{false, true} {
		t.Run(fmt.Sprintf("shebang=%t", shebang), func(t *testing.T) {
			f, b, repo := newDaytonaLifecycleFixture(t)
			_, leaseID, _, err := b.createDaytonaToolboxSandbox(t.Context(), repo, true, false, "")
			if err != nil {
				t.Fatal(err)
			}
			workdir := filepath.Join(b.cfg.Daytona.WorkRoot, leaseID, repo.Name)
			body := "set -eu\nprintf '%s\\n' \"$PWD\" \"$0\" \"$1\" \"$SAFE_ENV\"\nexit \"$2\"\n"
			if shebang {
				body = "#!/bin/sh\n" + body
			} else {
				body = "[[ -n $BASH_VERSION ]] || exit 89\n" + body
			}
			data := []byte(body)
			remotePath := fmt.Sprintf(".crabbox/scripts/%x-check.sh", sha256.Sum256(data))
			script := &core.RunScriptSpec{Source: "stdin", Data: data, RemotePath: remotePath, Shebang: shebang}
			arg := "a ' literal $(touch unexpected)\nsecond line"
			var stdout bytes.Buffer
			b.rt.Stdout = &stdout
			ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
			defer cancel()
			result, runErr := b.Run(ctx, RunRequest{ID: leaseID, Repo: repo, NoSync: true, ScriptRequested: true, Script: script,
				Command: []string{arg, "23"}, Env: map[string]string{"SAFE_ENV": "synthetic-value"}})
			var ee core.ExitError
			if !core.AsExitError(runErr, &ee) || ee.Code != 23 || result.ExitCode != 23 {
				t.Fatalf("script exit result=%+v error=%v", result, runErr)
			}
			lines := strings.SplitN(stdout.String(), "\n", 3)
			if len(lines) != 3 || lines[0] != workdir ||
				(lines[1] != remotePath && lines[1] != filepath.Join(workdir, remotePath)) || lines[2] != arg+"\nsynthetic-value\n" {
				t.Fatalf("standalone script PWD, identity, args, or env changed: %q", stdout.String())
			}
			for _, command := range f.commands {
				if strings.Contains(command.Command, body) || strings.Contains(command.Command, "synthetic-value") {
					t.Fatal("script body or environment value entered process command text")
				}
				if command.Cwd == workdir && (command.Timeout <= 0 || command.Timeout > 45) {
					t.Fatalf("command timeout=%d", command.Timeout)
				}
			}
			file := filepath.Join(workdir, remotePath)
			if got, err := os.ReadFile(file); err != nil || !bytes.Equal(got, data) {
				t.Fatalf("standalone script bytes changed: %v", err)
			}
			if info, err := os.Stat(file); err != nil || info.Mode().Perm() != 0700 {
				t.Fatalf("script mode info=%v error=%v", info, err)
			}
			entries, err := os.ReadDir(filepath.Dir(file))
			if err != nil || len(entries) != 1 {
				t.Fatalf("temporary upload state remains: %v error=%v", entries, err)
			}
			if _, err := os.Stat(filepath.Join(workdir, "unexpected")); !os.IsNotExist(err) {
				t.Fatal("literal script argument was evaluated")
			}
			if f.deletes != 0 {
				t.Fatal("reused sandbox was deleted")
			}
			if !f.privateUpload {
				t.Fatal("script bytes were uploaded before private staging existed")
			}
		})
	}
}

func TestDaytonaRunFiltersProviderCredentials(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executes Linux toolbox shell commands against local POSIX paths")
	}
	f, b, repo := newDaytonaLifecycleFixture(t)
	_, leaseID, _, err := b.createDaytonaToolboxSandbox(t.Context(), repo, true, false, "")
	if err != nil {
		t.Fatal(err)
	}
	input := map[string]string{"SAFE_ENV": "value", "CRABBOX_RUN_ID": "run-fixture"}
	for _, key := range []string{"DAYTONA_API_KEY", "CRABBOX_DAYTONA_API_KEY", "DAYTONA_CRABBOX_KEY", "DAYTONA_JWT_TOKEN", "CRABBOX_DAYTONA_JWT_TOKEN", "CRABBOX_COORDINATOR_TOKEN"} {
		input[key] = "synthetic-control-credential"
	}
	if _, err := b.Run(t.Context(), RunRequest{ID: leaseID, Repo: repo, NoSync: true, Command: []string{"true"}, Env: input}); err != nil {
		t.Fatal(err)
	}
	env := f.commands[len(f.commands)-1].Envs
	if len(env) != 2 || env["SAFE_ENV"] != "value" || env["CRABBOX_RUN_ID"] != "run-fixture" {
		t.Fatalf("provider credentials were forwarded or command metadata lost: names=%v", reflect.ValueOf(env).MapKeys())
	}
	if len(input) != 8 {
		t.Fatal("request environment was mutated")
	}
}

func TestDaytonaScriptUploadFailureCleansStagingWithoutExecuting(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executes Linux toolbox shell commands against local POSIX paths")
	}
	for _, cancelUpload := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancellation=%t", cancelUpload), func(t *testing.T) {
			f, b, repo := newDaytonaLifecycleFixture(t)
			_, leaseID, _, err := b.createDaytonaToolboxSandbox(t.Context(), repo, true, false, "")
			if err != nil {
				t.Fatal(err)
			}
			workdir := filepath.Join(b.cfg.Daytona.WorkRoot, leaseID, repo.Name)
			data := []byte("printf x >> ran\n")
			script := &core.RunScriptSpec{Source: "stdin", Data: data, RemotePath: fmt.Sprintf(".crabbox/scripts/%x-check.sh", sha256.Sum256(data))}
			req := RunRequest{ID: leaseID, Repo: repo, NoSync: true, Script: script}
			if _, err := b.Run(t.Context(), req); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if cancelUpload {
				f.uploadCancel = cancel
			} else {
				f.uploadError = true
			}
			if _, err := b.Run(ctx, req); err == nil {
				t.Fatal("failed upload reported success")
			}
			if got, err := os.ReadFile(filepath.Join(workdir, "ran")); err != nil || string(got) != "x" {
				t.Fatalf("failed upload executed user script: %q %v", got, err)
			}
			if got, err := os.ReadFile(filepath.Join(workdir, script.RemotePath)); err != nil || !bytes.Equal(got, data) {
				t.Fatalf("failed upload damaged previous script: %q %v", got, err)
			}
			entries, err := os.ReadDir(filepath.Join(workdir, ".crabbox", "scripts"))
			if err != nil || len(entries) != 1 {
				t.Fatalf("staged input leaked after failure: %v %v", entries, err)
			}
			if f.deletes != 0 {
				t.Fatal("reused sandbox deleted after upload failure")
			}
		})
	}
}

func TestDaytonaScriptRejectsSymlinkedStagingParent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executes Linux toolbox shell commands against local POSIX paths")
	}
	for _, parent := range []string{".crabbox", ".crabbox/scripts"} {
		t.Run(parent, func(t *testing.T) {
			f, b, repo := newDaytonaLifecycleFixture(t)
			_, leaseID, _, err := b.createDaytonaToolboxSandbox(t.Context(), repo, true, false, "")
			if err != nil {
				t.Fatal(err)
			}
			workdir := filepath.Join(b.cfg.Daytona.WorkRoot, leaseID, repo.Name)
			link := filepath.Join(workdir, parent)
			if err := os.MkdirAll(filepath.Dir(link), 0700); err != nil {
				t.Fatal(err)
			}
			outside := t.TempDir()
			if err := os.Symlink(outside, link); err != nil {
				t.Fatal(err)
			}
			_, err = b.Run(t.Context(), RunRequest{ID: leaseID, Repo: repo, NoSync: true,
				Script: &core.RunScriptSpec{Source: "stdin", Data: []byte("true\n"), RemotePath: ".crabbox/scripts/check.sh"}})
			if err == nil || len(f.uploads) != 0 {
				t.Fatalf("symlink accepted: uploads=%v error=%v", f.uploads, err)
			}
			entries, err := os.ReadDir(outside)
			if err != nil || len(entries) != 0 {
				t.Fatalf("outside staging directory was modified: %v %v", entries, err)
			}
		})
	}
}

func TestDaytonaScriptMayDeleteItsUploadedCopy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executes Linux toolbox shell commands against local POSIX paths")
	}
	_, b, repo := newDaytonaLifecycleFixture(t)
	_, leaseID, _, err := b.createDaytonaToolboxSandbox(t.Context(), repo, true, false, "")
	if err != nil {
		t.Fatal(err)
	}
	// Snapshot preparation scrubs its own uploaded script and command environment.
	data := []byte("#!/bin/sh\nmkdir -p .crabbox/env\nexec rm -rf -- .crabbox/env .crabbox/scripts\n")
	result, err := b.Run(t.Context(), RunRequest{ID: leaseID, Repo: repo, NoSync: true,
		Script: &core.RunScriptSpec{Source: "stdin", Data: data, Shebang: true,
			RemotePath: fmt.Sprintf(".crabbox/scripts/%x-scrub.sh", sha256.Sum256(data))}})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("self-scrubbing script failed: result=%+v error=%v", result, err)
	}
	meta := filepath.Join(b.cfg.Daytona.WorkRoot, leaseID, repo.Name, ".crabbox")
	if entries, err := os.ReadDir(meta); err != nil || len(entries) != 0 {
		t.Fatalf("script or environment state survived self-scrub: %v %v", entries, err)
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
