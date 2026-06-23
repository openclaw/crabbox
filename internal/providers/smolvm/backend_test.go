package smolvm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestProviderSpecAndAliases(t *testing.T) {
	p := Provider{}
	if p.Name() != providerName {
		t.Fatalf("Name=%q want %s", p.Name(), providerName)
	}
	for _, alias := range []string{"smol", "smolmachines", "smolfleet"} {
		got, err := core.ProviderFor(alias)
		if err != nil {
			t.Fatalf("ProviderFor(%q): %v", alias, err)
		}
		if got.Name() != providerName {
			t.Fatalf("ProviderFor(%q).Name=%q", alias, got.Name())
		}
	}
	spec := p.Spec()
	if spec.Kind != core.ProviderKindDelegatedRun {
		t.Fatalf("kind=%v want delegated-run", spec.Kind)
	}
	if spec.Coordinator != core.CoordinatorNever {
		t.Fatalf("coordinator=%v want never", spec.Coordinator)
	}
	if len(spec.Targets) != 1 || spec.Targets[0].OS != core.TargetLinux {
		t.Fatalf("targets=%#v want linux", spec.Targets)
	}
	if !hasFeature(spec.Features, core.FeatureArchiveSync) {
		t.Fatalf("features=%#v want archive sync", spec.Features)
	}
	if !hasFeature(spec.Features, core.FeatureRunSession) {
		t.Fatalf("features=%#v want run session", spec.Features)
	}
}

func TestClientUsesSmolvmRESTShape(t *testing.T) {
	var createBody map[string]any
	injectSeen := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") || strings.TrimPrefix(auth, "Bearer ") != "smk_key" {
			t.Fatalf("Authorization=%q want Bearer smk_key", auth)
		}
		switch r.URL.Path {
		case "/v1/machines":
			switch r.Method {
			case http.MethodPost:
				if err := json.NewDecoder(r.Body).Decode(&createBody); err != nil {
					t.Fatal(err)
				}
				_ = json.NewEncoder(w).Encode(testMachineResponse("mach_1", "crabbox-blue-123456789abc", "running"))
			case http.MethodGet:
				_ = json.NewEncoder(w).Encode([]map[string]any{testMachineResponse("mach_1", "crabbox-blue-123456789abc", "running")})
			default:
				t.Fatalf("unexpected method %s", r.Method)
			}
		case "/v1/machines/mach_1":
			if r.Method == http.MethodGet {
				_ = json.NewEncoder(w).Encode(testMachineResponse("mach_1", "crabbox-blue-123456789abc", "running"))
				return
			}
			if r.Method == http.MethodDelete {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			t.Fatalf("unexpected method %s on /v1/machines/mach_1", r.Method)
		case "/v1/machines/mach_1/exec":
			var body struct {
				Command string `json:"command"`
				CWD     string `json:"cwd"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			// Direct heredoc-based archive inject.
			if strings.Contains(body.Command, "crabbox-sync.tgz") || strings.Contains(body.Command, "smolvm-direct-archive-extract") {
				injectSeen = true
				_ = json.NewEncoder(w).Encode(map[string]any{"exitCode": 0, "stdout": "smolvm-direct-archive-extract: ok\n"})
				return
			}
			// Direct write (env profile etc) also uses base64 heredoc /exec.
			if strings.Contains(body.Command, "smolvm-direct-write") || strings.Contains(body.Command, "CRABBOX_WRITE_B64_EOF") {
				_ = json.NewEncoder(w).Encode(map[string]any{"exitCode": 0, "stdout": "smolvm-direct-write: ok\n"})
				return
			}
			if !strings.Contains(body.Command, "echo hi") || (body.CWD != "crabbox" && body.CWD != "/crabbox") {
				t.Fatalf("exec body=%#v", body)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"exitCode": 0, "stdout": "hi\n", "stderr": "warn\n"})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	client := &client{apiKey: "smk_key", base: srv.URL, http: srv.Client()}
	mach, err := client.CreateMachine(context.Background(), createRequest{Name: "crabbox-blue-123456789abc", Source: smolvmMachineSource{Type: "image", Reference: "ubuntu:24.04"}})
	if err != nil {
		t.Fatal(err)
	}
	if mach.ID != "mach_1" || createBody["name"] != "crabbox-blue-123456789abc" {
		t.Fatalf("create mach=%#v body=%v", mach, createBody)
	}
	if _, err := client.ListMachines(context.Background()); err != nil {
		t.Fatal(err)
	}
	if result, err := client.Exec(context.Background(), "mach_1", "echo hi", "crabbox"); err != nil || result.Output != "hi\n" {
		t.Fatalf("exec result=%#v err=%v", result, err)
	}
	var stdout bytes.Buffer
	code, err := client.ExecStream(context.Background(), "mach_1", "echo hi", "crabbox", &stdout)
	if err != nil || code != 0 || stdout.String() != "hi\nwarn\n" {
		t.Fatalf("stream code=%d stdout=%q err=%v", code, stdout.String(), err)
	}
	archive := filepath.Join(t.TempDir(), "archive.tgz")
	if err := os.WriteFile(archive, []byte("archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := client.InjectArchive(context.Background(), "mach_1", archive, "crabbox"); err != nil {
		t.Fatal(err)
	}
	if !injectSeen {
		t.Fatal("inject archive not seen")
	}
	if err := client.WriteFile(context.Background(), "mach_1", "/tmp/env.sh", "export A=1\n"); err != nil {
		t.Fatal(err)
	}
	if err := client.DeleteMachine(context.Background(), "mach_1"); err != nil {
		t.Fatal(err)
	}
}

func TestNewAPIRejectsBareHTTPBaseURL(t *testing.T) {
	cfg := Config{}
	cfg.Smolvm.APIKey = "smk_key"
	cfg.Smolvm.BaseURL = "http://api.smolmachines.com"
	if _, err := newAPI(cfg, Runtime{}); err == nil {
		t.Fatal("newAPI accepted plaintext http URL")
	}
}

func TestNewAPIRejectsUserinfoQueryAndFragment(t *testing.T) {
	for _, base := range []string{
		"https://user:pass@api.smolmachines.com",
		"https://api.smolmachines.com?key=value",
		"https://api.smolmachines.com#fragment",
	} {
		cfg := Config{}
		cfg.Smolvm.APIKey = "smk_key"
		cfg.Smolvm.BaseURL = base
		if _, err := newAPI(cfg, Runtime{}); err == nil {
			t.Fatalf("newAPI accepted %q", base)
		}
	}
}

func TestNewAPIAllowsLoopbackHTTPBaseURL(t *testing.T) {
	cfg := Config{}
	cfg.Smolvm.APIKey = "smk_key"
	cfg.Smolvm.BaseURL = "http://127.0.0.1:8080"
	if _, err := newAPI(cfg, Runtime{}); err != nil {
		t.Fatalf("loopback http rejected: %v", err)
	}
}

func TestNewAPIRejectsUntrustedHTTPSBaseURLByDefault(t *testing.T) {
	cfg := Config{}
	cfg.Smolvm.APIKey = "smk_key"
	cfg.Smolvm.BaseURL = "https://smolvm.attacker.example"
	if _, err := newAPI(cfg, Runtime{}); err == nil || !strings.Contains(err.Error(), "ALLOW_CUSTOM_BASE_URL") {
		t.Fatalf("newAPI error=%v, want custom endpoint opt-in requirement", err)
	}
}

func TestNewAPIAllowsExplicitCustomHTTPSBaseURL(t *testing.T) {
	t.Setenv("CRABBOX_SMOLVM_ALLOW_CUSTOM_BASE_URL", "1")
	cfg := Config{}
	cfg.Smolvm.APIKey = "smk_key"
	cfg.Smolvm.BaseURL = "https://smolvm.example.test"
	if _, err := newAPI(cfg, Runtime{}); err != nil {
		t.Fatalf("explicit custom endpoint rejected: %v", err)
	}
}

func TestNewAPINormalizesBaseURL(t *testing.T) {
	cfg := Config{}
	cfg.Smolvm.APIKey = "smk_key"
	cfg.Smolvm.BaseURL = " https://eu.smolmachines.com/base/ "
	apiClient, err := newAPI(cfg, Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	c, ok := apiClient.(*client)
	if !ok {
		t.Fatalf("api client=%T, want *client", apiClient)
	}
	if c.base != "https://eu.smolmachines.com/base" {
		t.Fatalf("base = %q, want normalized base URL", c.base)
	}
}

func TestExecExitCodePropagatesCamelAndSnake(t *testing.T) {
	cases := []struct {
		name string
		resp map[string]any
		want int
	}{
		{"camelCase nonzero", map[string]any{"exitCode": 7, "stdout": "x"}, 7},
		{"snake_case nonzero", map[string]any{"exit_code": 9, "stdout": "x"}, 9},
		{"camelCase zero stays zero", map[string]any{"exitCode": 0, "stdout": "ok"}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/machines/mach_1/exec" {
					t.Fatalf("unexpected path %s", r.URL.Path)
				}
				_ = json.NewEncoder(w).Encode(tc.resp)
			}))
			defer srv.Close()
			c := &client{apiKey: "smk_key", base: srv.URL, http: srv.Client()}
			res, err := c.Exec(context.Background(), "mach_1", "false", "/workspace")
			if err != nil {
				t.Fatal(err)
			}
			if res.ExitCode != tc.want {
				t.Fatalf("Exec ExitCode=%d want %d", res.ExitCode, tc.want)
			}
			code, err := c.ExecStream(context.Background(), "mach_1", "false", "/workspace", &bytes.Buffer{})
			if err != nil || code != tc.want {
				t.Fatalf("ExecStream code=%d want %d err=%v", code, tc.want, err)
			}
		})
	}
}

func TestCleanWorkdirAndCommand(t *testing.T) {
	if got, err := cleanWorkdir(" /workspace "); err != nil || got != "/workspace" {
		t.Fatalf("workdir=%q err=%v", got, err)
	}
	if got, err := workspaceFolder("/workspace/repo"); err != nil || got != "/workspace/repo" {
		t.Fatalf("workspaceFolder=%q err=%v", got, err)
	}
	for _, value := range []string{"", "repo", "/", "/tmp", "/workspace/../etc"} {
		if _, err := cleanWorkdir(value); err == nil {
			t.Fatalf("cleanWorkdir(%q) succeeded unexpectedly", value)
		}
	}
	command, err := buildCommand([]string{"go", "test", "./..."}, false)
	if err != nil {
		t.Fatal(err)
	}
	if command != "exec 'go' 'test' './...'" {
		t.Fatalf("command=%q", command)
	}
	env := shellEnvProfile(map[string]string{"B": "two", "A": "one two", "BAD; id >&2 #": "boom"})
	if env != "set -a\nA='one two'\nB='two'\nset +a\n" {
		t.Fatalf("env profile=%q", env)
	}
}

func TestWarmupRejectsActionsRunner(t *testing.T) {
	backend := NewBackend(Provider{}.Spec(), testConfig(), testRuntime()).(*backend)
	err := backend.Warmup(context.Background(), WarmupRequest{ActionsRunner: true})
	if err == nil || !strings.Contains(err.Error(), "--actions-runner is not supported") {
		t.Fatalf("err=%v, want actions-runner rejection", err)
	}
}

func TestRunCreatesExecsAndDeletesOneShotMachine(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := &fakeAPI{}
	withFakeAPI(t, fake)
	backend := NewBackend(Provider{}.Spec(), testConfig(), testRuntime()).(*backend)
	result, err := backend.Run(context.Background(), RunRequest{
		Repo:    Repo{Name: "repo", Root: t.TempDir()},
		Command: []string{"echo", "hello"},
		NoSync:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || result.Provider != providerName {
		t.Fatalf("result=%#v", result)
	}
	if result.Session == nil {
		t.Fatal("result.Session is nil")
	}
	if result.Session.Provider != providerName || result.Session.LeaseID != result.LeaseID || result.Session.Slug != result.Slug || result.Session.Reused || result.Session.Kept {
		t.Fatalf("session=%#v result=%#v", result.Session, result)
	}
	if result.Session.CleanupCommand != "crabbox stop --provider smolvm --id "+shellQuote(result.LeaseID) {
		t.Fatalf("cleanup command=%q", result.Session.CleanupCommand)
	}
	if fake.createReq.Name == "" || !strings.HasPrefix(fake.createReq.Name, "crabbox-") {
		t.Fatalf("create req=%#v", fake.createReq)
	}
	if !reflect.DeepEqual(fake.verbs, []string{"create", "exec", "stream", "delete"}) {
		t.Fatalf("verbs=%v", fake.verbs)
	}
	if !fake.createReq.Ephemeral || fake.createReq.TTLSeconds == 0 {
		t.Fatalf("create req should be ephemeral for one-shot run: %#v", fake.createReq)
	}
	if !strings.Contains(fake.execCommands[0], "mkdir -p") || strings.Contains(fake.execCommands[0], "rm -rf") {
		t.Fatalf("prepare command=%q", fake.execCommands[0])
	}
	if fake.streamFolders[0] == "" || !strings.Contains(fake.streamCommands[0], "echo") {
		t.Fatalf("stream folder=%q command=%q", fake.streamFolders[0], fake.streamCommands[0])
	}
}

func TestRunHonorsProviderKeepConfig(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := &fakeAPI{}
	withFakeAPI(t, fake)
	cfg := testConfig()
	cfg.Smolvm.Keep = true
	backend := NewBackend(Provider{}.Spec(), cfg, testRuntime()).(*backend)
	result, err := backend.Run(context.Background(), RunRequest{
		Repo:    Repo{Name: "repo", Root: t.TempDir()},
		Command: []string{"echo", "hello"},
		NoSync:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Session == nil || result.Session.Reused || !result.Session.Kept {
		t.Fatalf("session=%#v, want retained new machine", result.Session)
	}
	if fake.createReq.Ephemeral || fake.createReq.TTLSeconds != 0 {
		t.Fatalf("provider keep should create retained machine: %#v", fake.createReq)
	}
	if reflect.DeepEqual(fake.verbs, []string{"create", "exec", "stream", "delete"}) || fake.deletedID != "" {
		t.Fatalf("provider keep should not delete machine: verbs=%v deleted=%q", fake.verbs, fake.deletedID)
	}
}

func TestRunReturnsReusedMachineSession(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repo := t.TempDir()
	leaseID := "cbx_123456789abc"
	if err := core.ClaimLeaseForRepoProvider(leaseID, "blue", providerName, repo, time.Minute, false); err != nil {
		t.Fatal(err)
	}
	fake := &fakeAPI{machine: machineData{ID: "mach_1", Name: "crabbox-blue-123456789abc", State: "running"}}
	withFakeAPI(t, fake)
	backend := NewBackend(Provider{}.Spec(), testConfig(), testRuntime()).(*backend)
	result, err := backend.Run(context.Background(), RunRequest{
		ID:      leaseID,
		Repo:    Repo{Name: "repo", Root: repo},
		Command: []string{"echo", "hello"},
		NoSync:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Session == nil || result.Session.LeaseID != leaseID || result.Session.Slug != "blue" || !result.Session.Reused || !result.Session.Kept {
		t.Fatalf("session=%#v", result.Session)
	}
	if fake.deletedID != "" {
		t.Fatalf("reused machine should not be deleted, deleted=%q", fake.deletedID)
	}
}

func TestRunPreservesSessionAfterDeleteFailure(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := &fakeAPI{deleteErr: errors.New("delete denied")}
	withFakeAPI(t, fake)
	var stderr bytes.Buffer
	rt := testRuntime()
	rt.Stderr = &stderr
	backend := NewBackend(Provider{}.Spec(), testConfig(), rt).(*backend)
	result, err := backend.Run(context.Background(), RunRequest{
		Repo:    Repo{Name: "repo", Root: t.TempDir()},
		Command: []string{"echo", "hello"},
		NoSync:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Session == nil || !result.Session.Kept || result.Session.CleanupCommand == "" {
		t.Fatalf("session=%#v, want retained cleanup handle", result.Session)
	}
	if !strings.Contains(stderr.String(), "smolvm delete failed") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestWarmupCreatesRetainedMachine(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := &fakeAPI{}
	withFakeAPI(t, fake)
	backend := NewBackend(Provider{}.Spec(), testConfig(), testRuntime()).(*backend)
	if err := backend.Warmup(context.Background(), WarmupRequest{Repo: Repo{Name: "repo", Root: t.TempDir()}}); err != nil {
		t.Fatal(err)
	}
	if fake.createReq.Ephemeral || fake.createReq.TTLSeconds != 0 {
		t.Fatalf("warmup should create retained machine: %#v", fake.createReq)
	}
}

func TestRunKeepsWorkspaceSubdirectoryConsistent(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := &fakeAPI{}
	withFakeAPI(t, fake)
	cfg := testConfig()
	cfg.Smolvm.Workdir = "/workspace/repo"
	backend := NewBackend(Provider{}.Spec(), cfg, testRuntime()).(*backend)
	if _, err := backend.Run(context.Background(), RunRequest{
		Repo:    Repo{Name: "repo", Root: newGitRepo(t)},
		Command: []string{"pwd"},
		Env:     map[string]string{"A": "1"},
	}); err != nil {
		t.Fatal(err)
	}
	if len(fake.execCommands) == 0 || !strings.Contains(fake.execCommands[0], "mkdir -p '/workspace/repo'") {
		t.Fatalf("prepare command=%q", fake.execCommands)
	}
	if len(fake.injectTargets) != 1 || fake.injectTargets[0] != "/workspace/repo" {
		t.Fatalf("inject targets=%v", fake.injectTargets)
	}
	if len(fake.writePaths) != 1 || !strings.HasPrefix(fake.writePaths[0], "/workspace/repo/.crabbox-env-") {
		t.Fatalf("write paths=%v", fake.writePaths)
	}
	if len(fake.streamFolders) != 1 || fake.streamFolders[0] != "/workspace/repo" {
		t.Fatalf("stream folders=%v", fake.streamFolders)
	}
}

func TestSyncWorkspaceUsesInject(t *testing.T) {
	fake := &fakeAPI{}
	backend := NewBackend(Provider{}.Spec(), testConfig(), testRuntime()).(*backend)
	_, _, err := backend.syncWorkspace(context.Background(), fake, "mach_1", RunRequest{
		Repo: Repo{Name: "repo", Root: newGitRepo(t)},
	}, "/workspace", ".")
	if err != nil {
		t.Fatalf("sync err=%v", err)
	}
	if !reflect.DeepEqual(fake.verbs, []string{"exec", "inject"}) {
		t.Fatalf("verbs=%v", fake.verbs)
	}
}

func TestStatusMapsMachineName(t *testing.T) {
	fake := &fakeAPI{machine: machineData{
		ID: "mach_1", Name: "crabbox-blue-123456789abc", State: "running",
		Source:    smolvmMachineSource{Type: "image", Reference: "alpine"},
		Resources: smolvmMachineResources{CPUs: 4, MemoryMB: 8192},
	}}
	withFakeAPI(t, fake)
	cfg := testConfig()
	cfg.Smolvm.BaseURL = "https://eu.smolmachines.com"
	backend := NewBackend(Provider{}.Spec(), cfg, testRuntime()).(*backend)
	view, err := backend.Status(context.Background(), StatusRequest{ID: "mach_1"})
	if err != nil {
		t.Fatal(err)
	}
	if view.ID != "cbx_123456789abc" || view.Slug != "blue" || view.ServerID != "mach_1" || !view.Ready {
		t.Fatalf("view=%#v", view)
	}
	if view.Labels["image"] != "alpine" {
		t.Fatalf("labels=%v", view.Labels)
	}
	server := machineToServer(cfg, fake.machine)
	if server.PublicNet.IPv4.IP != "eu.smolmachines.com" {
		t.Fatalf("host=%q", server.PublicNet.IPv4.IP)
	}
}

func TestStatusRejectsNonCrabboxRawMachine(t *testing.T) {
	fake := &fakeAPI{machine: machineData{ID: "mach_1", Name: "external-machine", State: "running"}}
	withFakeAPI(t, fake)
	backend := NewBackend(Provider{}.Spec(), testConfig(), testRuntime()).(*backend)
	if _, err := backend.Status(context.Background(), StatusRequest{ID: "mach_1"}); err == nil {
		t.Fatal("expected non-Crabbox raw machine id to be rejected")
	}
}

func TestRunRawMachineIDEnforcesRepositoryClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_123456789abc"
	repoA := t.TempDir()
	repoB := t.TempDir()
	if err := core.ClaimLeaseForRepoProvider(leaseID, "blue", providerName, repoA, time.Minute, false); err != nil {
		t.Fatal(err)
	}
	fake := &fakeAPI{machine: machineData{ID: "mach_1", Name: "crabbox-blue-123456789abc", State: "running"}}
	withFakeAPI(t, fake)
	backend := NewBackend(Provider{}.Spec(), testConfig(), testRuntime()).(*backend)
	_, err := backend.Run(context.Background(), RunRequest{
		ID:      "mach_1",
		Repo:    Repo{Name: "repo-b", Root: repoB},
		Command: []string{"echo", "hello"},
		NoSync:  true,
	})
	if err == nil || !strings.Contains(err.Error(), "is claimed by repo") {
		t.Fatalf("Run error=%v, want cross-repository claim rejection", err)
	}
	if len(fake.verbs) != 0 {
		t.Fatalf("verbs=%v, want no machine mutation or execution", fake.verbs)
	}
}

func hasFeature(features core.FeatureSet, want core.Feature) bool {
	for _, feature := range features {
		if feature == want {
			return true
		}
	}
	return false
}

func testConfig() Config {
	return Config{
		Provider: providerName,
		Smolvm: SmolvmConfig{
			APIKey:   "smk_key",
			BaseURL:  "https://api.smolmachines.com",
			Image:    "ubuntu:24.04",
			Workdir:  "/workspace",
			CPUs:     2,
			MemoryMB: 4096,
			Network:  "blocked",
		},
	}
}

func testRuntime() Runtime {
	return Runtime{Stdout: io.Discard, Stderr: io.Discard}
}

func withFakeAPI(t *testing.T, fake *fakeAPI) {
	t.Helper()
	original := newAPI
	newAPI = func(Config, Runtime) (api, error) { return fake, nil }
	t.Cleanup(func() { newAPI = original })
}

type fakeAPI struct {
	verbs          []string
	createReq      createRequest
	machine        machineData
	execCommands   []string
	execFolders    []string
	streamCommands []string
	streamFolders  []string
	injectTargets  []string
	writePaths     []string
	writeContents  []string
	execResults    []execResult
	deletedID      string
	deleteErr      error
	injected       bool
}

func (f *fakeAPI) CreateMachine(_ context.Context, req createRequest) (machineData, error) {
	f.verbs = append(f.verbs, "create")
	f.createReq = req
	f.machine = machineData{ID: "mach_1", Name: req.Name, State: "running"}
	if ref := strings.TrimSpace(req.Source.Reference); ref != "" {
		f.machine.Source = req.Source
	} else if req.Source.Type != "" {
		f.machine.Source.Reference = req.Source.Type
	}
	f.machine.Resources = req.Resources
	return f.machine, nil
}

func (f *fakeAPI) GetMachine(context.Context, string) (machineData, error) {
	if f.machine.ID == "" {
		f.machine = machineData{ID: "mach_1", Name: "crabbox-blue-123456789abc", State: "running"}
	}
	return f.machine, nil
}

func (f *fakeAPI) ListMachines(context.Context) ([]machineData, error) {
	if f.machine.ID == "" {
		f.machine = machineData{ID: "mach_1", Name: "crabbox-blue-123456789abc", State: "running"}
	}
	return []machineData{f.machine}, nil
}

func (f *fakeAPI) DeleteMachine(_ context.Context, id string) error {
	f.verbs = append(f.verbs, "delete")
	f.deletedID = id
	return f.deleteErr
}

func (f *fakeAPI) StartMachine(context.Context, string) error { return nil }
func (f *fakeAPI) StopMachine(context.Context, string) error  { return nil }

func (f *fakeAPI) Exec(_ context.Context, _ string, command, folder string) (execResult, error) {
	f.verbs = append(f.verbs, "exec")
	f.execCommands = append(f.execCommands, command)
	f.execFolders = append(f.execFolders, folder)
	if len(f.execResults) == 0 {
		return execResult{ExitCode: 0}, nil
	}
	result := f.execResults[0]
	f.execResults = f.execResults[1:]
	return result, nil
}

func (f *fakeAPI) ExecStream(_ context.Context, _ string, command, folder string, stdout io.Writer) (int, error) {
	f.verbs = append(f.verbs, "stream")
	f.streamCommands = append(f.streamCommands, command)
	f.streamFolders = append(f.streamFolders, folder)
	_, _ = io.WriteString(stdout, "ok\n")
	return 0, nil
}

func (f *fakeAPI) InjectArchive(_ context.Context, _, _, targetDir string) error {
	f.verbs = append(f.verbs, "inject")
	f.injectTargets = append(f.injectTargets, targetDir)
	f.injected = true
	return nil
}

func (f *fakeAPI) WriteFile(_ context.Context, _, remotePath, content string) error {
	f.verbs = append(f.verbs, "write")
	f.writePaths = append(f.writePaths, remotePath)
	f.writeContents = append(f.writeContents, content)
	return nil
}

func newGitRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	runGit(t, root, "init")
	runGit(t, root, "config", "user.email", "alice@example.com")
	runGit(t, root, "config", "user.name", "Alice")
	if err := os.WriteFile(filepath.Join(root, "hello.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", "hello.txt")
	runGit(t, root, "commit", "-m", "initial")
	return root
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}

func testMachineResponse(id, name, state string) map[string]any {
	return map[string]any{
		"id": id, "name": name, "state": state,
		"source":    map[string]any{"type": "image", "reference": "ubuntu:24.04"},
		"resources": map[string]any{"cpus": 2, "memoryMb": 4096},
		"network":   map[string]any{"mode": "blocked"},
		"ephemeral": false, "createdAt": "2026-06-12T20:00:00Z", "updatedAt": "2026-06-12T20:00:00Z",
	}
}
