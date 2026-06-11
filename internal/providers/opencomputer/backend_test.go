package opencomputer

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	osexec "os/exec"
	"path"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

// --- pure-function tests -----------------------------------------------------

func TestProviderSpec(t *testing.T) {
	p := Provider{}
	if p.Name() != "opencomputer" {
		t.Fatalf("Name=%q want opencomputer", p.Name())
	}
	if len(p.Aliases()) == 0 {
		t.Fatalf("expected aliases, got none")
	}
	spec := p.Spec()
	if spec.Kind != core.ProviderKindDelegatedRun {
		t.Fatalf("kind=%v want delegated run", spec.Kind)
	}
	if spec.Coordinator != core.CoordinatorNever {
		t.Fatalf("coordinator=%v want never", spec.Coordinator)
	}
	if len(spec.Targets) != 1 || spec.Targets[0].OS != core.TargetLinux {
		t.Fatalf("targets=%#v want [{linux}]", spec.Targets)
	}
}

func TestProviderForResolvesNameAndAliases(t *testing.T) {
	for _, name := range []string{"opencomputer", "oc", "open-computer"} {
		got, err := core.ProviderFor(name)
		if err != nil {
			t.Fatalf("ProviderFor(%q) err=%v", name, err)
		}
		if got.Name() != "opencomputer" {
			t.Fatalf("ProviderFor(%q).Name()=%q want opencomputer", name, got.Name())
		}
	}
}

func TestBuildCommandAutoWrapsShellMetacharacters(t *testing.T) {
	got, err := buildCommand([]string{"pnpm install && pnpm test"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] != "bash" || got[1] != "-lc" {
		t.Fatalf("command=%#v want bash -lc wrapping", got)
	}
	if got[2] != "pnpm install && pnpm test" {
		t.Fatalf("script=%q want unquoted shell expression", got[2])
	}
}

func TestBuildCommandAutoWrapsLeadingEnvAssignment(t *testing.T) {
	got, err := buildCommand([]string{"FOO=bar", "pnpm", "test"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] != "bash" {
		t.Fatalf("command=%#v want bash wrapping for FOO=bar", got)
	}
}

func TestBuildCommandShellMode(t *testing.T) {
	got, err := buildCommand([]string{"pnpm install && pnpm test"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"bash", "-lc", "pnpm install && pnpm test"}) {
		t.Fatalf("command=%#v", got)
	}
}

func TestBuildCommandPassThrough(t *testing.T) {
	got, err := buildCommand([]string{"pnpm", "test"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"pnpm", "test"}) {
		t.Fatalf("command=%#v", got)
	}
}

func TestBuildCommandRejectsEmpty(t *testing.T) {
	if _, err := buildCommand(nil, false); err == nil {
		t.Fatalf("expected error for empty command")
	}
}

func TestOpenComputerWorkdirRejectsRelative(t *testing.T) {
	cfg := newTestConfig("")
	cfg.OpenComputer.Workdir = "relative/path"
	if _, err := openComputerWorkdir(cfg); err == nil {
		t.Fatalf("expected rejection of relative workdir")
	}
}

func TestOpenComputerWorkdirRejectsBroadPaths(t *testing.T) {
	for _, workdir := range []string{"/", "/tmp", "/workspace", "/workspace/.."} {
		t.Run(workdir, func(t *testing.T) {
			cfg := newTestConfig("")
			cfg.OpenComputer.Workdir = workdir
			if _, err := openComputerWorkdir(cfg); err == nil || !strings.Contains(err.Error(), "too broad") {
				t.Fatalf("err=%v, want too broad rejection", err)
			}
		})
	}
}

func TestOpenComputerWorkdirCleansAndDefaults(t *testing.T) {
	cfg := newTestConfig("")
	cfg.OpenComputer.Workdir = " /workspace/crabbox/../project "
	if got, err := openComputerWorkdir(cfg); err != nil || got != "/workspace/project" {
		t.Fatalf("got=%q err=%v want /workspace/project", got, err)
	}
	cfg.OpenComputer.Workdir = ""
	if got, err := openComputerWorkdir(cfg); err != nil || got != "/workspace/crabbox" {
		t.Fatalf("default=%q err=%v want /workspace/crabbox", got, err)
	}
}

func TestIsReadyState(t *testing.T) {
	for state, want := range map[string]bool{
		"running": true, "  Running ": true, "ready": true,
		"starting": false, "hibernated": false, "terminated": false, "": false,
	} {
		if got := isReadyState(state); got != want {
			t.Errorf("isReadyState(%q)=%v want %v", state, got, want)
		}
	}
}

func TestIsTerminalState(t *testing.T) {
	for state, want := range map[string]bool{
		"terminated": true, "stopped": true, "failed": true, "error": true, "killed": true,
		"running": false, "starting": false,
	} {
		if got := isTerminalState(state); got != want {
			t.Errorf("isTerminalState(%q)=%v want %v", state, got, want)
		}
	}
}

func TestResolveLeaseIDRejectsUnclaimed(t *testing.T) {
	if _, _, _, err := resolveLeaseID("not-a-known-slug", "", false, 0); err == nil || !strings.Contains(err.Error(), "not claimed by Crabbox") {
		t.Fatalf("err=%v, want rejection", err)
	}
}

func TestResolveLeaseIDRejectsLeasePrefixWithoutClaim(t *testing.T) {
	if _, _, _, err := resolveLeaseID("ocbx_sb-unknown", "", false, 0); err == nil || !strings.Contains(err.Error(), "not claimed by Crabbox") {
		t.Fatalf("err=%v, want rejection", err)
	}
}

func TestResolveLeaseIDRequiresIdentifier(t *testing.T) {
	if _, _, _, err := resolveLeaseID("", "", false, 0); err == nil {
		t.Fatalf("expected error for empty id")
	}
}

func TestResolveLeaseIDFallsBackForSluglessClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "ocbx_sb-known123"
	if err := claimLeaseForRepoProvider(leaseID, "", providerName, "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	gotLease, sandboxID, slug, err := resolveLeaseID(leaseID, "", false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if gotLease != leaseID || sandboxID != "sb-known123" || slug != newLeaseSlug(leaseID) {
		t.Fatalf("lease=%q sandbox=%q slug=%q", gotLease, sandboxID, slug)
	}
}

func TestResolveLeaseIDPrefersExactLeaseOverCollidingSlug(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	exactLeaseID := "ocbx_sb-z-exact"
	if err := claimLeaseForRepoProvider(exactLeaseID, "exact", providerName, "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if err := claimLeaseForRepoProvider("ocbx_sb-a-other", exactLeaseID, providerName, "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	leaseID, sandboxID, _, err := resolveLeaseID(exactLeaseID, "", false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if leaseID != exactLeaseID || sandboxID != "sb-z-exact" {
		t.Fatalf("resolved lease=%q sandbox=%q", leaseID, sandboxID)
	}
}

func TestNewSandboxName(t *testing.T) {
	if name := newSandboxName(Repo{Name: "carbbox"}); !strings.HasPrefix(name, "crabbox-carbbox-") {
		t.Fatalf("name=%q", name)
	}
	if name := newSandboxName(Repo{Name: "crabbox-app"}); strings.HasPrefix(name, "crabbox-crabbox-") || !strings.HasPrefix(name, "crabbox-app-") {
		t.Fatalf("name=%q double/!prefixed", name)
	}
	if name := newSandboxName(Repo{Name: strings.Repeat("very-long-repo-name-", 8)}); len(name) > 63 || strings.HasSuffix(name, "-") {
		t.Fatalf("name len=%d %q", len(name), name)
	}
}

func TestSpecAllowsForceSyncLargeAndSyncOnly(t *testing.T) {
	spec := Provider{}.Spec()
	if err := core.RejectDelegatedSyncOptionsForSpec(spec, RunRequest{ForceSyncLarge: true}); err != nil {
		t.Fatalf("--force-sync-large should be allowed, got %v", err)
	}
	if err := core.RejectDelegatedSyncOptionsForSpec(spec, RunRequest{SyncOnly: true}); err != nil {
		t.Fatalf("--sync-only should be allowed, got %v", err)
	}
	if err := core.RejectDelegatedSyncOptionsForSpec(spec, RunRequest{ChecksumSync: true}); err == nil {
		t.Fatalf("--checksum should be rejected")
	}
}

// --- fake API server ---------------------------------------------------------

type recordedRequest struct {
	method string
	path   string
	query  string
	body   string
}

type execRecord struct {
	req  execRunRequest
	body string
}

// fakeAPI is an httptest-backed OpenComputer API. It records every request and
// lets tests script exec/run replies by call order.
type fakeAPI struct {
	mu            sync.Mutex
	server        *httptest.Server
	requests      []recordedRequest
	execs         []execRecord
	execReply     []execRunResult // popped in order; last/zero reused when empty
	sandboxID     string
	listState     string
	listStatus    int
	getStatusCode int // when non-zero, GET /api/sandboxes/:id returns this code
	blockDelete   bool
	deleteStatus  int
	uploadStatus  int
	blockUpload   bool
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	f := &fakeAPI{sandboxID: "sb-test01", listState: "running"}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeAPI) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.requests = append(f.requests, recordedRequest{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, body: string(body)})
	f.mu.Unlock()

	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/sandboxes":
		writeJSON(w, map[string]any{"sandboxID": f.sandboxID, "status": "running"})
	case r.Method == http.MethodGet && r.URL.Path == "/api/sandboxes":
		if f.listStatus != 0 {
			w.WriteHeader(f.listStatus)
			_, _ = w.Write([]byte(`{"error":"list denied"}`))
			return
		}
		writeJSON(w, []map[string]any{})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/sandboxes/"):
		if f.getStatusCode != 0 {
			w.WriteHeader(f.getStatusCode)
			_, _ = w.Write([]byte(`{"error":"boom"}`))
			return
		}
		writeJSON(w, map[string]any{"sandboxID": f.sandboxID, "status": f.listState})
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/sandboxes/"):
		if f.blockDelete {
			<-r.Context().Done()
			return
		}
		if f.deleteStatus != 0 {
			w.WriteHeader(f.deleteStatus)
			_, _ = w.Write([]byte(`{"error":"cleanup denied"}`))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/files"):
		if f.blockUpload {
			<-r.Context().Done()
			return
		}
		if f.uploadStatus != 0 {
			w.WriteHeader(f.uploadStatus)
			_, _ = w.Write([]byte(`{"error":"upload denied"}`))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/exec/run"):
		var req execRunRequest
		_ = json.Unmarshal(body, &req)
		f.mu.Lock()
		f.execs = append(f.execs, execRecord{req: req, body: string(body)})
		reply := execRunResult{}
		if len(f.execReply) > 0 {
			reply = f.execReply[0]
			f.execReply = f.execReply[1:]
		}
		f.mu.Unlock()
		writeJSON(w, reply)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeAPI) calls(method, pathContains string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.requests {
		if r.method == method && strings.Contains(r.path, pathContains) {
			n++
		}
	}
	return n
}

func (f *fakeAPI) callsExact(method, path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.requests {
		if r.method == method && r.path == path {
			n++
		}
	}
	return n
}

func (f *fakeAPI) allExecs() []execRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]execRecord(nil), f.execs...)
}

func (f *fakeAPI) firstRequest(method, path string) (recordedRequest, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, req := range f.requests {
		if req.method == method && req.path == path {
			return req, true
		}
	}
	return recordedRequest{}, false
}

func newTestConfig(apiURL string) Config {
	cfg := Config{}
	cfg.OpenComputer.APIURL = apiURL
	cfg.OpenComputer.Workdir = "/workspace/crabbox"
	return cfg
}

// newAPIBackend wires a backend to the fake API and isolates it from the real
// ~/.oc/config.json and lease store.
func newAPIBackend(t *testing.T, f *fakeAPI) *openComputerBackend {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir()) // no real ~/.oc/config.json
	t.Setenv("CRABBOX_OPENCOMPUTER_API_KEY", "osb_testkey")
	rt := Runtime{Stdout: io.Discard, Stderr: io.Discard, HTTP: f.server.Client()}
	return NewOpenComputerBackend(Provider{}.Spec(), newTestConfig(f.server.URL), rt).(*openComputerBackend)
}

// --- API-backed flow tests ---------------------------------------------------

func TestRunCreatesExecsAndKillsEphemeral(t *testing.T) {
	f := newFakeAPI(t)
	f.execReply = []execRunResult{{ExitCode: 0}, {ExitCode: 0, Stdout: "hello\n"}} // mkdir, user cmd
	backend := newAPIBackend(t, f)
	res, err := backend.Run(context.Background(), RunRequest{
		Repo: Repo{Name: "carbbox", Root: t.TempDir()}, Command: []string{"echo", "hello"}, NoSync: true,
	})
	if err != nil {
		t.Fatalf("Run err=%v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d", res.ExitCode)
	}
	if f.callsExact(http.MethodPost, "/api/sandboxes") != 1 {
		t.Fatalf("want 1 create, got %d", f.callsExact(http.MethodPost, "/api/sandboxes"))
	}
	if f.calls(http.MethodDelete, "/api/sandboxes/") != 1 {
		t.Fatalf("want 1 kill, got %d", f.calls(http.MethodDelete, "/api/sandboxes/"))
	}
	// The user command is the last exec; it must carry cmd + cwd.
	execs := f.allExecs()
	last := execs[len(execs)-1].req
	if last.Cmd != "echo" || !reflect.DeepEqual(last.Args, []string{"hello"}) {
		t.Fatalf("user exec cmd=%q args=%v", last.Cmd, last.Args)
	}
	if last.Cwd != "/workspace/crabbox" {
		t.Fatalf("user exec cwd=%q", last.Cwd)
	}
	if last.Timeout != openComputerExecTimeoutSecs {
		t.Fatalf("user exec timeout=%d want %d", last.Timeout, openComputerExecTimeoutSecs)
	}
}

func TestRunCleanupCannotBlockForever(t *testing.T) {
	f := newFakeAPI(t)
	f.blockDelete = true
	f.execReply = []execRunResult{{ExitCode: 0}, {ExitCode: 0}}
	backend := newAPIBackend(t, f)
	var stderr bytes.Buffer
	backend.rt.Stderr = &stderr
	backend.cleanupTimeoutOverride = 20 * time.Millisecond
	started := time.Now()

	res, err := backend.Run(context.Background(), RunRequest{
		Repo: Repo{Name: "carbbox", Root: t.TempDir()}, Command: []string{"true"}, NoSync: true,
	})
	if err != nil {
		t.Fatalf("Run err=%v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d", res.ExitCode)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("Run took %s, cleanup should be bounded", elapsed)
	}
	if f.calls(http.MethodDelete, "/api/sandboxes/") != 1 {
		t.Fatalf("want 1 kill, got %d", f.calls(http.MethodDelete, "/api/sandboxes/"))
	}
	if !strings.Contains(stderr.String(), "context deadline exceeded") {
		t.Fatalf("stderr=%q, want cleanup deadline warning", stderr.String())
	}
}

func TestRunClearsClaimWhenAcquiredSandboxAlreadyMissingAtCleanup(t *testing.T) {
	f := newFakeAPI(t)
	f.deleteStatus = http.StatusNotFound
	f.execReply = []execRunResult{{ExitCode: 0}, {ExitCode: 0}}
	backend := newAPIBackend(t, f)
	if _, err := backend.Run(context.Background(), RunRequest{
		Repo: Repo{Name: "carbbox", Root: t.TempDir()}, Command: []string{"true"}, NoSync: true,
	}); err != nil {
		t.Fatalf("Run err=%v", err)
	}
	if _, ok, err := resolveLeaseClaim(leasePrefix + f.sandboxID); err != nil || ok {
		t.Fatalf("acquired missing sandbox claim remains: ok=%t err=%v", ok, err)
	}
}

func TestRunForwardsEnvInExecBodyOffArgv(t *testing.T) {
	f := newFakeAPI(t)
	f.execReply = []execRunResult{{ExitCode: 0}, {ExitCode: 0, Stdout: "ok\n"}}
	backend := newAPIBackend(t, f)
	var stderr bytes.Buffer
	backend.rt.Stderr = &stderr
	_, err := backend.Run(context.Background(), RunRequest{
		Repo:       Repo{Name: "carbbox", Root: t.TempDir()},
		Command:    []string{"printenv", "SECRET_TOKEN"},
		NoSync:     true,
		Env:        map[string]string{"SECRET_TOKEN": "super-secret"},
		EnvSummary: true,
		Options:    core.LeaseOptions{EnvAllow: []string{"SECRET_TOKEN"}},
	})
	if err != nil {
		t.Fatalf("Run err=%v", err)
	}
	execs := f.allExecs()
	user := execs[len(execs)-1]
	// Env is delivered in the request body's envs map...
	if user.req.Envs["SECRET_TOKEN"] != "super-secret" {
		t.Fatalf("exec body missing envs: %#v", user.req.Envs)
	}
	// ...and never in cmd/args (argv).
	if user.req.Cmd == "super-secret" || strings.Contains(strings.Join(user.req.Args, " "), "super-secret") {
		t.Fatalf("secret leaked into exec argv: cmd=%q args=%v", user.req.Cmd, user.req.Args)
	}
	if !strings.Contains(stderr.String(), "env forwarding provider=opencomputer") || !strings.Contains(stderr.String(), "secret=true") {
		t.Fatalf("stderr=%q, want redacted env summary", stderr.String())
	}
	if strings.Contains(stderr.String(), "super-secret") {
		t.Fatalf("secret leaked into stderr: %q", stderr.String())
	}
}

func TestRunUsesConfiguredExecTimeout(t *testing.T) {
	f := newFakeAPI(t)
	f.execReply = []execRunResult{{ExitCode: 0}, {ExitCode: 0}}
	backend := newAPIBackend(t, f)
	backend.cfg.OpenComputer.ExecTimeoutSecs = 123
	if _, err := backend.Run(context.Background(), RunRequest{
		Repo: Repo{Name: "carbbox", Root: t.TempDir()}, Command: []string{"true"}, NoSync: true,
	}); err != nil {
		t.Fatalf("Run err=%v", err)
	}
	for _, exec := range f.allExecs() {
		if exec.req.Timeout != 123 {
			t.Fatalf("exec timeout=%d want 123: %#v", exec.req.Timeout, exec.req)
		}
	}
}

func TestRunRequiresAPIKey(t *testing.T) {
	f := newFakeAPI(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CRABBOX_OPENCOMPUTER_API_KEY", "")
	t.Setenv("OPENCOMPUTER_API_KEY", "")
	rt := Runtime{Stdout: io.Discard, Stderr: io.Discard, HTTP: f.server.Client()}
	backend := NewOpenComputerBackend(Provider{}.Spec(), newTestConfig(f.server.URL), rt).(*openComputerBackend)
	_, err := backend.Run(context.Background(), RunRequest{Repo: Repo{Name: "x", Root: t.TempDir()}, Command: []string{"true"}, NoSync: true})
	if err == nil || !strings.Contains(err.Error(), "API key") {
		t.Fatalf("err=%v, want API-key error", err)
	}
	if f.callsExact(http.MethodPost, "/api/sandboxes") != 0 {
		t.Fatalf("must not create a sandbox without a key")
	}
}

func TestRunPerformsArchiveSyncViaFileAPI(t *testing.T) {
	f := newFakeAPI(t)
	f.execReply = []execRunResult{{ExitCode: 0}, {ExitCode: 0}, {ExitCode: 0, Stdout: "done\n"}} // mkdir, extract, user
	backend := newAPIBackend(t, f)
	_, err := backend.Run(context.Background(), RunRequest{Repo: Repo{Name: "carbbox", Root: newGitRepo(t)}, Command: []string{"true"}})
	if err != nil {
		t.Fatalf("Run err=%v", err)
	}
	if f.calls(http.MethodPut, "/files") != 1 {
		t.Fatalf("want 1 file upload, got %d", f.calls(http.MethodPut, "/files"))
	}
	var sawExtract bool
	for _, e := range f.allExecs() {
		if strings.Contains(strings.Join(e.req.Args, " "), "tar -xzf") {
			sawExtract = true
		}
	}
	if !sawExtract {
		t.Fatalf("expected a tar extract exec after upload")
	}
}

func TestSyncHonorsConfiguredTimeout(t *testing.T) {
	f := newFakeAPI(t)
	f.blockUpload = true
	backend := newAPIBackend(t, f)
	backend.cfg.Sync.Timeout = 50 * time.Millisecond
	started := time.Now()

	_, err := backend.Run(context.Background(), RunRequest{
		Repo: Repo{Name: "carbbox", Root: newGitRepo(t)}, Command: []string{"true"}, Keep: true,
	})
	if err == nil || !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("Run err=%v, want sync timeout", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("Run took %s, sync timeout should bound upload", elapsed)
	}
}

func TestSyncPreflightGuardsFullArchiveCandidate(t *testing.T) {
	cfg := newTestConfig("")
	cfg.Sync.FailBytes = 100
	manifest := SyncManifest{
		Files:        []string{"large.bin", "small.txt"},
		Bytes:        1000,
		Changed:      []string{"small.txt"},
		ChangedBytes: 1,
	}
	var stderr bytes.Buffer
	err := checkOpenComputerSyncPreflight(manifest, cfg, false, &stderr)
	if err == nil || !strings.Contains(err.Error(), "sync candidate too large") {
		t.Fatalf("preflight err=%v stderr=%q, want full candidate rejection", err, stderr.String())
	}
}

func TestSyncDeleteDoesNotTouchWorkspaceBeforeUploadSucceeds(t *testing.T) {
	f := newFakeAPI(t)
	f.uploadStatus = http.StatusServiceUnavailable
	backend := newAPIBackend(t, f)
	backend.cfg.Sync.Delete = true
	_, err := backend.Run(context.Background(), RunRequest{
		Repo: Repo{Name: "carbbox", Root: newGitRepo(t)}, Command: []string{"true"}, Keep: true,
	})
	if err == nil || !strings.Contains(err.Error(), "upload denied") {
		t.Fatalf("Run err=%v, want upload failure", err)
	}
	if execs := f.allExecs(); len(execs) != 0 {
		t.Fatalf("remote workspace touched before upload succeeded: %#v", execs)
	}
}

func TestSyncDeleteStagesBeforeReplacingWorkspace(t *testing.T) {
	f := newFakeAPI(t)
	backend := newAPIBackend(t, f)
	backend.cfg.Sync.Delete = true
	if _, err := backend.Run(context.Background(), RunRequest{
		Repo: Repo{Name: "carbbox", Root: newGitRepo(t)}, Command: []string{"true"},
	}); err != nil {
		t.Fatalf("Run err=%v", err)
	}
	execs := f.allExecs()
	extractIndex, replaceIndex := -1, -1
	backupCleanupIndex := -1
	for i, exec := range execs {
		command := strings.Join(exec.req.Args, " ")
		if strings.Contains(command, "rm -rf '/workspace/crabbox'") {
			t.Fatalf("sync deleted live workspace directly: %q", command)
		}
		if strings.Contains(command, "tar -xzf") && strings.Contains(command, ".crabbox-sync-") {
			extractIndex = i
		}
		if strings.Contains(command, "if mv ") && strings.Contains(command, "/workspace/crabbox") {
			replaceIndex = i
		}
		if strings.Contains(command, ".previous") && strings.Contains(command, "rm -rf ") && !strings.Contains(command, "if mv ") {
			backupCleanupIndex = i
		}
	}
	if extractIndex < 0 || replaceIndex <= extractIndex {
		t.Fatalf("execs=%#v, want staged extract before replacement", execs)
	}
	if backupCleanupIndex <= replaceIndex {
		t.Fatalf("execs=%#v, want surfaced backup cleanup after committed swap", execs)
	}
}

func TestSyncDeleteWarnsWhenPreviousWorkspaceCleanupFails(t *testing.T) {
	f := newFakeAPI(t)
	f.execReply = []execRunResult{
		{ExitCode: 0}, {ExitCode: 0}, {ExitCode: 0},
		{ExitCode: 1, Stderr: "permission denied"},
		{ExitCode: 0}, {ExitCode: 0},
	}
	backend := newAPIBackend(t, f)
	backend.cfg.Sync.Delete = true
	var stderr bytes.Buffer
	backend.rt.Stderr = &stderr
	if _, err := backend.Run(context.Background(), RunRequest{
		Repo: Repo{Name: "carbbox", Root: newGitRepo(t)}, Command: []string{"true"},
	}); err != nil {
		t.Fatalf("Run err=%v", err)
	}
	if !strings.Contains(stderr.String(), "previous workspace cleanup failed") || !strings.Contains(stderr.String(), ".previous") {
		t.Fatalf("stderr=%q, want actionable backup cleanup warning", stderr.String())
	}
}

func TestNoSyncDoesNotDeleteRetainedWorkspace(t *testing.T) {
	f := newFakeAPI(t)
	f.execReply = []execRunResult{{ExitCode: 0}, {ExitCode: 0}}
	backend := newAPIBackend(t, f)
	backend.cfg.Sync.Delete = true
	leaseID := leasePrefix + f.sandboxID
	if err := claimLeaseForRepoProvider(leaseID, "retained", providerName, "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Run(context.Background(), RunRequest{
		ID: leaseID, Repo: Repo{Name: "carbbox", Root: "/repo"}, Command: []string{"true"}, NoSync: true,
	}); err != nil {
		t.Fatalf("Run err=%v", err)
	}
	for _, exec := range f.allExecs() {
		if strings.Contains(strings.Join(exec.req.Args, " "), "rm -rf") {
			t.Fatalf("--no-sync deleted retained workspace: %#v", exec.req)
		}
	}
}

func TestCreateSandboxForwardsPartialSizing(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cpu      int
		memoryMB int
	}{
		{name: "cpu only", cpu: 2},
		{name: "memory only", memoryMB: 4096},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeAPI(t)
			backend := newAPIBackend(t, f)
			backend.cfg.OpenComputer.CPU = tc.cpu
			backend.cfg.OpenComputer.MemoryMB = tc.memoryMB
			api, err := newOCAPIClient(backend.cfg, backend.rt)
			if err != nil {
				t.Fatal(err)
			}
			leaseID, _, _, err := backend.createSandbox(context.Background(), api, Repo{Name: "carbbox", Root: t.TempDir()}, false, "")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { removeLeaseClaim(leaseID) })
			recorded, ok := f.firstRequest(http.MethodPost, "/api/sandboxes")
			if !ok {
				t.Fatal("missing create request")
			}
			var req createSandboxRequest
			if err := json.Unmarshal([]byte(recorded.body), &req); err != nil {
				t.Fatal(err)
			}
			if req.CPUCount != tc.cpu || req.MemoryMB != tc.memoryMB {
				t.Fatalf("create sizing=%d/%d want %d/%d", req.CPUCount, req.MemoryMB, tc.cpu, tc.memoryMB)
			}
		})
	}
}

func TestCreateSandboxReportsCleanupFailureAndSandboxID(t *testing.T) {
	f := newFakeAPI(t)
	f.deleteStatus = http.StatusInternalServerError
	backend := newAPIBackend(t, f)
	claimsPath := path.Join(os.Getenv("XDG_STATE_HOME"), "crabbox", "claims")
	if err := os.MkdirAll(path.Dir(claimsPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claimsPath, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	api, err := newOCAPIClient(backend.cfg, backend.rt)
	if err != nil {
		t.Fatal(err)
	}
	leaseID, sandboxID, _, err := backend.createSandbox(context.Background(), api, Repo{Name: "carbbox", Root: t.TempDir()}, false, "taken")
	if err == nil {
		t.Fatal("expected claim setup and cleanup failure")
	}
	if leaseID != leasePrefix+f.sandboxID || sandboxID != f.sandboxID {
		t.Fatalf("lease=%q sandbox=%q", leaseID, sandboxID)
	}
	for _, want := range []string{"read claims directory", "cleanup failed", f.sandboxID, "cleanup denied"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err=%v, want %q", err, want)
		}
	}
}

func TestSyncOnlySkipsUserCommand(t *testing.T) {
	f := newFakeAPI(t)
	f.execReply = []execRunResult{{ExitCode: 0}, {ExitCode: 0}} // mkdir, extract
	backend := newAPIBackend(t, f)
	if _, err := backend.Run(context.Background(), RunRequest{Repo: Repo{Name: "carbbox", Root: newGitRepo(t)}, Command: []string{"echo", "should-not-run"}, SyncOnly: true}); err != nil {
		t.Fatalf("Run err=%v", err)
	}
	for _, e := range f.allExecs() {
		if e.req.Cmd == "echo" {
			t.Fatalf("--sync-only must not run the user command: %#v", e.req)
		}
	}
	if f.calls(http.MethodDelete, "/api/sandboxes/") != 1 {
		t.Fatalf("sync-only should still tear down")
	}
}

func TestRunSurfacesNonZeroExit(t *testing.T) {
	f := newFakeAPI(t)
	f.execReply = []execRunResult{{ExitCode: 0}, {ExitCode: 7}} // mkdir, user cmd exits 7
	backend := newAPIBackend(t, f)
	res, err := backend.Run(context.Background(), RunRequest{Repo: Repo{Name: "carbbox", Root: t.TempDir()}, Command: []string{"false"}, NoSync: true})
	if res.ExitCode != 7 {
		t.Fatalf("exit=%d want 7", res.ExitCode)
	}
	ee, ok := err.(ExitError)
	if !ok || ee.Code != 7 {
		t.Fatalf("err=%v want ExitError code 7", err)
	}
}

func TestKeepRetainsSandbox(t *testing.T) {
	f := newFakeAPI(t)
	f.execReply = []execRunResult{{ExitCode: 0}, {ExitCode: 0}}
	backend := newAPIBackend(t, f)
	if _, err := backend.Run(context.Background(), RunRequest{Repo: Repo{Name: "carbbox", Root: t.TempDir()}, Command: []string{"true"}, NoSync: true, Keep: true}); err != nil {
		t.Fatalf("Run err=%v", err)
	}
	if f.calls(http.MethodDelete, "/api/sandboxes/") != 0 {
		t.Fatalf("kill must not run with --keep")
	}
}

func TestWarmupRejectsActionsRunnerBeforeCreate(t *testing.T) {
	f := newFakeAPI(t)
	backend := newAPIBackend(t, f)
	err := backend.Warmup(context.Background(), WarmupRequest{ActionsRunner: true})
	if err == nil || !strings.Contains(err.Error(), "--actions-runner is not supported") {
		t.Fatalf("Warmup err=%v", err)
	}
	if f.callsExact(http.MethodPost, "/api/sandboxes") != 0 {
		t.Fatal("unsupported actions runner warmup created a sandbox")
	}
}

func TestStopRejectsUnclaimedID(t *testing.T) {
	f := newFakeAPI(t)
	backend := newAPIBackend(t, f)
	err := backend.Stop(context.Background(), StopRequest{ID: "sb-not-claimed"})
	if err == nil || !strings.Contains(err.Error(), "not claimed by Crabbox") {
		t.Fatalf("err=%v want unclaimed rejection", err)
	}
}

func TestStopClearsClaimWhenSandboxAlreadyDeleted(t *testing.T) {
	f := newFakeAPI(t)
	f.deleteStatus = http.StatusNotFound
	backend := newAPIBackend(t, f)
	leaseID := leasePrefix + f.sandboxID
	if err := claimLeaseForRepoProvider(leaseID, "gone", providerName, "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	backend.cfg.OpenComputer.ForgetMissing = true
	if err := backend.Stop(context.Background(), StopRequest{ID: leaseID}); err != nil {
		t.Fatalf("Stop err=%v", err)
	}
	if _, ok, err := resolveLeaseClaim(leaseID); err != nil || ok {
		t.Fatalf("claim remains after idempotent stop: ok=%t err=%v", ok, err)
	}
}

func TestStopPreservesClaimForAmbiguousMissingSandbox(t *testing.T) {
	f := newFakeAPI(t)
	f.deleteStatus = http.StatusNotFound
	backend := newAPIBackend(t, f)
	leaseID := leasePrefix + f.sandboxID
	if err := claimLeaseForRepoProvider(leaseID, "possibly-other-account", providerName, "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	err := backend.Stop(context.Background(), StopRequest{ID: leaseID})
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("Stop err=%v, want ambiguous missing error", err)
	}
	if f.calls(http.MethodDelete, "/api/sandboxes/") != 1 {
		t.Fatal("stop did not attempt remote deletion")
	}
	if _, ok, err := resolveLeaseClaim(leaseID); err != nil || !ok {
		t.Fatalf("claim removed without explicit forget: ok=%t err=%v", ok, err)
	}
}

// TestAPIURLPrecedenceHonorsOCConfig asserts the oc config file's api_url is
// used before the built-in default, and that an explicit Crabbox setting wins.
func TestAPIURLPrecedenceHonorsOCConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CRABBOX_OPENCOMPUTER_API_KEY", "")
	t.Setenv("OPENCOMPUTER_API_KEY", "")
	if err := os.MkdirAll(home+"/.oc", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(home+"/.oc/config.json", []byte(`{"api_url":"https://oc-file.example","api_key":"osb_fromfile"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// No explicit Crabbox API URL → oc config api_url wins over the default.
	api, err := newOCAPIClient(newTestConfig(""), Runtime{})
	if err != nil {
		t.Fatalf("newOCAPIClient err=%v", err)
	}
	if api.baseURL != "https://oc-file.example" {
		t.Fatalf("baseURL=%q want oc-config api_url", api.baseURL)
	}
	if api.apiKey != "osb_fromfile" {
		t.Fatalf("apiKey not read from oc config: %q", api.apiKey)
	}
	// Explicit Crabbox API URL takes precedence over the oc config file.
	api, err = newOCAPIClient(newTestConfig("https://explicit.example"), Runtime{})
	if err != nil {
		t.Fatalf("newOCAPIClient err=%v", err)
	}
	if api.baseURL != "https://explicit.example" {
		t.Fatalf("baseURL=%q want explicit override", api.baseURL)
	}
}

func TestListFetchesClaimedHibernatedSandbox(t *testing.T) {
	f := newFakeAPI(t)
	f.listState = "hibernated"
	backend := newAPIBackend(t, f)
	claimsDir := path.Join(os.Getenv("XDG_STATE_HOME"), "crabbox", "claims")
	if err := os.MkdirAll(claimsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path.Join(claimsDir, "cbx_unrelated.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Unclaimed sandboxes are not inventory and do not trigger remote calls.
	views, err := backend.List(context.Background(), ListRequest{})
	if err != nil {
		t.Fatalf("List err=%v", err)
	}
	if len(views) != 0 {
		t.Fatalf("want 0 unclaimed, got %d", len(views))
	}
	if f.calls(http.MethodGet, "/api/sandboxes") != 0 {
		t.Fatal("unclaimed list unexpectedly queried remote inventory")
	}
	// The collection endpoint omits hibernated sandboxes, so List must fetch
	// each locally claimed sandbox by ID.
	if err := claimLeaseForRepoProviderPond("ocbx_"+f.sandboxID, "slug", providerName, "alpha", "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	views, err = backend.List(context.Background(), ListRequest{})
	if err != nil {
		t.Fatalf("List err=%v", err)
	}
	if len(views) != 1 || views[0].CloudID != f.sandboxID || views[0].Status != "hibernated" || views[0].Labels["pond"] != "alpha" {
		t.Fatalf("views=%#v", views)
	}
	if f.callsExact(http.MethodGet, "/api/sandboxes/"+f.sandboxID) != 1 {
		t.Fatalf("want one status fetch, requests=%#v", f.requests)
	}
	status, err := backend.Status(context.Background(), StatusRequest{ID: "slug"})
	if err != nil {
		t.Fatalf("Status err=%v", err)
	}
	if status.Pond != "alpha" || status.Labels["pond"] != "alpha" {
		t.Fatalf("status=%#v, want top-level pond and pond label", status)
	}
}

func TestListKeepsAmbiguousMissingClaimVisible(t *testing.T) {
	f := newFakeAPI(t)
	f.getStatusCode = http.StatusNotFound
	backend := newAPIBackend(t, f)
	if err := claimLeaseForRepoProvider(leasePrefix+f.sandboxID, "ambiguous", providerName, "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	views, err := backend.List(context.Background(), ListRequest{})
	if err != nil {
		t.Fatalf("List err=%v", err)
	}
	if len(views) != 1 || views[0].Status != "missing-or-inaccessible" || views[0].Labels["slug"] != "ambiguous" {
		t.Fatalf("views=%#v", views)
	}
}

func TestListSkipsMalformedClaimAndKeepsValidInventory(t *testing.T) {
	f := newFakeAPI(t)
	backend := newAPIBackend(t, f)
	if err := claimLeaseForRepoProvider(leasePrefix+f.sandboxID, "valid", providerName, "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	claimsDir := path.Join(os.Getenv("XDG_STATE_HOME"), "crabbox", "claims")
	if err := os.WriteFile(path.Join(claimsDir, leasePrefix+"broken.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	views, err := backend.List(context.Background(), ListRequest{})
	if err != nil {
		t.Fatalf("List err=%v", err)
	}
	if len(views) != 1 || views[0].Labels["slug"] != "valid" {
		t.Fatalf("views=%#v", views)
	}
}

func TestDoctorProbesControlPlaneWithoutClaims(t *testing.T) {
	f := newFakeAPI(t)
	f.listStatus = http.StatusUnauthorized
	backend := newAPIBackend(t, f)
	_, err := backend.Doctor(context.Background(), DoctorRequest{})
	if err == nil || !strings.Contains(err.Error(), "list denied") {
		t.Fatalf("Doctor err=%v, want control-plane failure", err)
	}
	if f.callsExact(http.MethodGet, "/api/sandboxes") != 1 {
		t.Fatalf("want one collection probe, requests=%#v", f.requests)
	}
}

// TestStatusSurfacesAPIError asserts a failing GET is returned, not masked as
// a not-ready status.
func TestStatusSurfacesAPIError(t *testing.T) {
	f := newFakeAPI(t)
	f.getStatusCode = http.StatusInternalServerError
	backend := newAPIBackend(t, f)
	if err := claimLeaseForRepoProvider("ocbx_"+f.sandboxID, "slug", providerName, "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	_, err := backend.Status(context.Background(), StatusRequest{ID: "ocbx_" + f.sandboxID})
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("err=%v, want surfaced API error", err)
	}
}

func newGitRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", root},
		{"-C", root, "config", "user.email", "t@e.com"},
		{"-C", root, "config", "user.name", "t"},
		{"-C", root, "commit", "-q", "--allow-empty", "-m", "init"},
	} {
		if out, err := osexec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return root
}
