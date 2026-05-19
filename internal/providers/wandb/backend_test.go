package wandb

import (
	"context"
	"errors"
	"flag"
	"io"
	"strings"
	"testing"
)

// wandbRecordingRunner mirrors the recording-runner pattern used by
// internal/providers/exedev/backend_test.go: tests pre-load `fn` to assert
// arguments and return canned stdout/exit codes without actually invoking
// python3.
type wandbRecordingRunner struct {
	calls []LocalCommandRequest
	fn    func(LocalCommandRequest) (LocalCommandResult, error)
}

func (r *wandbRecordingRunner) Run(_ context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
	r.calls = append(r.calls, req)
	if r.fn != nil {
		return r.fn(req)
	}
	return LocalCommandResult{}, nil
}

func TestWandbProviderSpec(t *testing.T) {
	spec := Provider{}.Spec()
	if spec.Name != providerName {
		t.Fatalf("spec.Name = %q, want %q", spec.Name, providerName)
	}
	if spec.Kind != "delegated-run" {
		t.Fatalf("spec.Kind = %q, want delegated-run", spec.Kind)
	}
	aliases := Provider{}.Aliases()
	if len(aliases) != 1 || aliases[0] != "weights-and-biases" {
		t.Fatalf("aliases = %#v, want [weights-and-biases]", aliases)
	}
}

func TestWandbIsProviderName(t *testing.T) {
	for _, name := range []string{"wandb", "WANDB", "  wandb  ", "weights-and-biases"} {
		if !isWandbProviderName(name) {
			t.Fatalf("isWandbProviderName(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"", "railway", "wandbx"} {
		if isWandbProviderName(name) {
			t.Fatalf("isWandbProviderName(%q) = true, want false", name)
		}
	}
}

func TestWandbTokenFlagIsNotRegistered(t *testing.T) {
	cfg := Config{}
	cfg.Wandb.APIKey = "secret-token"
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	RegisterWandbProviderFlags(fs, cfg)
	for _, name := range []string{
		"wandb-token", "wandb-api-token", "wandb-key", "wandb-api-key",
		"wandb-secret", "weights-and-biases-token",
	} {
		if fs.Lookup(name) != nil {
			t.Fatalf("wandb API key surfaced as a flag --%s", name)
		}
	}
	if fs.Lookup("wandb-python") == nil {
		t.Fatal("wandb-python flag missing")
	}
	if fs.Lookup("wandb-image") == nil {
		t.Fatal("wandb-image flag missing")
	}
	if fs.Lookup("wandb-max-lifetime") == nil {
		t.Fatal("wandb-max-lifetime flag missing")
	}
}

func TestWandbFlagsApply(t *testing.T) {
	cfg := Config{Provider: providerName}
	cfg.Wandb.Python = "python3"
	cfg.Wandb.DefaultImage = "ubuntu:24.04"
	cfg.Wandb.MaxLifetimeSeconds = 1800
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	values := RegisterWandbProviderFlags(fs, cfg)
	if err := fs.Parse([]string{"--wandb-python", "/opt/py/bin/python3", "--wandb-image", "ubuntu:22.04", "--wandb-max-lifetime", "3600"}); err != nil {
		t.Fatal(err)
	}
	if err := ApplyWandbProviderFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.Wandb.Python != "/opt/py/bin/python3" {
		t.Fatalf("Python = %q", cfg.Wandb.Python)
	}
	if cfg.Wandb.DefaultImage != "ubuntu:22.04" {
		t.Fatalf("DefaultImage = %q", cfg.Wandb.DefaultImage)
	}
	if cfg.Wandb.MaxLifetimeSeconds != 3600 {
		t.Fatalf("MaxLifetimeSeconds = %d", cfg.Wandb.MaxLifetimeSeconds)
	}
}

func TestWandbFlagsRejectClassAndType(t *testing.T) {
	for _, provider := range []string{providerName, "weights-and-biases"} {
		t.Run(provider, func(t *testing.T) {
			cfg := Config{Provider: provider}
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			fs.String("class", "", "class")
			fs.String("type", "", "type")
			values := RegisterWandbProviderFlags(fs, cfg)
			if err := fs.Parse([]string{"--class", "beast"}); err != nil {
				t.Fatal(err)
			}
			err := ApplyWandbProviderFlags(&cfg, fs, values)
			if err == nil || !strings.Contains(err.Error(), "--class is not supported") {
				t.Fatalf("err = %v, want class rejection", err)
			}
		})
	}
}

func TestWandbDefaultsDoNotTouchSSHOrWorkRoot(t *testing.T) {
	cfg := Config{WorkRoot: "/preserve/me", SSHUser: "alice"}
	applyWandbDefaults(&cfg)
	if cfg.WorkRoot != "/preserve/me" {
		t.Fatalf("WorkRoot=%q, want preserved (delegated-run must not touch SSH/WorkRoot)", cfg.WorkRoot)
	}
	if cfg.SSHUser != "alice" {
		t.Fatalf("SSHUser=%q, want preserved", cfg.SSHUser)
	}
	if cfg.Wandb.Python != "python3" {
		t.Fatalf("Python=%q", cfg.Wandb.Python)
	}
	if cfg.Wandb.DefaultImage != "ubuntu:24.04" {
		t.Fatalf("DefaultImage=%q", cfg.Wandb.DefaultImage)
	}
	if cfg.Wandb.MaxLifetimeSeconds != 1800 {
		t.Fatalf("MaxLifetimeSeconds=%d", cfg.Wandb.MaxLifetimeSeconds)
	}
}

func TestWandbRunRequiresNoSync(t *testing.T) {
	t.Setenv("WANDB_API_KEY", "fake")
	backend := &wandbBackend{rt: Runtime{Stdout: io.Discard, Stderr: io.Discard}}
	_, err := backend.Run(context.Background(), RunRequest{Command: []string{"echo", "hi"}})
	if err == nil || !strings.Contains(err.Error(), "--no-sync") {
		t.Fatalf("err = %v, want --no-sync rejection", err)
	}
}

func TestWandbRunRequiresAPIKey(t *testing.T) {
	t.Setenv("WANDB_API_KEY", "")
	backend := &wandbBackend{rt: Runtime{Stdout: io.Discard, Stderr: io.Discard}}
	_, err := backend.Run(context.Background(), RunRequest{NoSync: true, Command: []string{"echo", "hi"}})
	if err == nil || !strings.Contains(err.Error(), "WANDB_API_KEY") {
		t.Fatalf("err = %v, want WANDB_API_KEY rejection", err)
	}
}

func TestWandbRunRejectsUnsupportedOptions(t *testing.T) {
	t.Setenv("WANDB_API_KEY", "fake")
	for _, tc := range []struct {
		name string
		req  RunRequest
		want string
	}{
		{name: "reclaim", req: RunRequest{NoSync: true, Reclaim: true, Command: []string{"echo"}}, want: "--reclaim"},
		{name: "shell", req: RunRequest{NoSync: true, ShellMode: true, Command: []string{"echo"}}, want: "--shell"},
		{name: "env summary", req: RunRequest{NoSync: true, EnvSummary: true, Env: map[string]string{"K": "v"}, Command: []string{"echo"}}, want: "environment"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := &wandbBackend{rt: Runtime{Stdout: io.Discard, Stderr: io.Discard}}
			_, err := backend.Run(context.Background(), tc.req)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %s", err, tc.want)
			}
		})
	}
}

// fakeWandbAPI is the in-memory client used by happy-path tests.
type fakeWandbAPI struct {
	versionValue string
	versionErr   error
	acquired     wandbSandbox
	acquireErr   error
	acquireReq   wandbAcquireRequest
	execCmd      []string
	execID       string
	execCode     int
	execErr      error
	stopID       string
	stopErr      error
	listValue    []wandbSandbox
	listErr      error
	statusValue  wandbSandbox
	statusErr    error
}

func (f *fakeWandbAPI) Version(_ context.Context) (string, error) {
	return f.versionValue, f.versionErr
}

func (f *fakeWandbAPI) Acquire(_ context.Context, req wandbAcquireRequest) (wandbSandbox, error) {
	f.acquireReq = req
	return f.acquired, f.acquireErr
}

func (f *fakeWandbAPI) Exec(_ context.Context, req wandbExecRequest) (int, error) {
	f.execID = req.SandboxID
	f.execCmd = req.Command
	return f.execCode, f.execErr
}

func (f *fakeWandbAPI) Stop(_ context.Context, id string, _ int, _ bool) error {
	f.stopID = id
	return f.stopErr
}

func (f *fakeWandbAPI) List(_ context.Context, _ []string, _ string) ([]wandbSandbox, error) {
	return f.listValue, f.listErr
}

func (f *fakeWandbAPI) Status(_ context.Context, _ string) (wandbSandbox, error) {
	return f.statusValue, f.statusErr
}

func newWandbBackendForTest(api wandbAPI) *wandbBackend {
	cfg := Config{Provider: providerName}
	applyWandbDefaults(&cfg)
	return &wandbBackend{
		spec:   Provider{}.Spec(),
		cfg:    cfg,
		rt:     Runtime{Stdout: io.Discard, Stderr: io.Discard},
		client: api,
	}
}

func TestWandbRunHappyPathAcquireExecStop(t *testing.T) {
	t.Setenv("WANDB_API_KEY", "fake")
	api := &fakeWandbAPI{
		acquired: wandbSandbox{ID: "sb-abc", Status: "RUNNING"},
		execCode: 0,
	}
	backend := newWandbBackendForTest(api)
	result, err := backend.Run(context.Background(), RunRequest{NoSync: true, Command: []string{"echo", "hello"}})
	if err != nil {
		t.Fatalf("Run err: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit = %d, want 0", result.ExitCode)
	}
	if api.acquireReq.Image != "ubuntu:24.04" {
		t.Fatalf("Acquire image = %q, want ubuntu:24.04", api.acquireReq.Image)
	}
	if api.acquireReq.MaxLifetimeSecs != 1800 {
		t.Fatalf("Acquire MaxLifetimeSecs = %d, want 1800", api.acquireReq.MaxLifetimeSecs)
	}
	if !contains(api.acquireReq.Tags, "crabbox") {
		t.Fatalf("Acquire tags = %#v, want crabbox tag", api.acquireReq.Tags)
	}
	if api.execID != "sb-abc" {
		t.Fatalf("Exec id = %q, want sb-abc", api.execID)
	}
	if len(api.execCmd) != 2 || api.execCmd[0] != "echo" || api.execCmd[1] != "hello" {
		t.Fatalf("Exec cmd = %#v", api.execCmd)
	}
	if api.stopID != "sb-abc" {
		t.Fatalf("Stop id = %q, want sb-abc (auto-stop after run)", api.stopID)
	}
}

func TestWandbRunWithExistingIDSkipsAcquireAndStop(t *testing.T) {
	t.Setenv("WANDB_API_KEY", "fake")
	api := &fakeWandbAPI{execCode: 0}
	backend := newWandbBackendForTest(api)
	if _, err := backend.Run(context.Background(), RunRequest{ID: "sb-supplied", NoSync: true, Command: []string{"echo"}}); err != nil {
		t.Fatalf("Run err: %v", err)
	}
	if api.acquireReq.Image != "" {
		t.Fatalf("Acquire should not be called when --id is supplied; got %#v", api.acquireReq)
	}
	if api.execID != "sb-supplied" {
		t.Fatalf("Exec id = %q, want sb-supplied", api.execID)
	}
	if api.stopID != "" {
		t.Fatalf("Stop should not be called for user-supplied id; got %q", api.stopID)
	}
}

func TestWandbRunNonZeroExecMapsToExit(t *testing.T) {
	t.Setenv("WANDB_API_KEY", "fake")
	api := &fakeWandbAPI{acquired: wandbSandbox{ID: "sb-abc", Status: "RUNNING"}, execCode: 7}
	backend := newWandbBackendForTest(api)
	result, err := backend.Run(context.Background(), RunRequest{NoSync: true, Command: []string{"false"}})
	if err == nil {
		t.Fatal("Run accepted non-zero exec exit")
	}
	if result.ExitCode != 7 {
		t.Fatalf("exit = %d, want 7", result.ExitCode)
	}
}

func TestWandbStatusReturnsView(t *testing.T) {
	t.Setenv("WANDB_API_KEY", "fake")
	api := &fakeWandbAPI{statusValue: wandbSandbox{ID: "sb-abc", Status: "RUNNING", CreatedAt: "2026-05-18T00:00:00Z"}}
	backend := newWandbBackendForTest(api)
	view, err := backend.Status(context.Background(), StatusRequest{ID: "sb-abc"})
	if err != nil {
		t.Fatalf("Status err: %v", err)
	}
	if view.ID != "sb-abc" || view.Provider != providerName || !view.Ready || view.State != "running" {
		t.Fatalf("view = %#v", view)
	}
}

func TestWandbListEnumeratesSandboxes(t *testing.T) {
	api := &fakeWandbAPI{listValue: []wandbSandbox{
		{ID: "sb-1", Status: "RUNNING"},
		{ID: "sb-2", Status: "COMPLETED"},
	}}
	backend := newWandbBackendForTest(api)
	servers, err := backend.List(context.Background(), ListRequest{})
	if err != nil {
		t.Fatalf("List err: %v", err)
	}
	if len(servers) != 2 || servers[0].CloudID != "sb-1" || servers[1].CloudID != "sb-2" {
		t.Fatalf("List = %#v", servers)
	}
}

func TestWandbStopRequiresID(t *testing.T) {
	backend := newWandbBackendForTest(&fakeWandbAPI{})
	if err := backend.Stop(context.Background(), StopRequest{}); err == nil {
		t.Fatal("Stop accepted empty id")
	}
}

func TestWandbStopCallsClient(t *testing.T) {
	api := &fakeWandbAPI{}
	backend := newWandbBackendForTest(api)
	if err := backend.Stop(context.Background(), StopRequest{ID: "sb-abc"}); err != nil {
		t.Fatalf("Stop err: %v", err)
	}
	if api.stopID != "sb-abc" {
		t.Fatalf("Stop called with %q, want sb-abc", api.stopID)
	}
}

func TestWandbDoctorReturnsCLIResult(t *testing.T) {
	t.Setenv("WANDB_API_KEY", "fake")
	api := &fakeWandbAPI{versionValue: "0.23.0", listValue: []wandbSandbox{{ID: "sb-1"}}}
	doctor, err := Provider{}.ConfigureDoctor(Config{}, Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: &wandbRecordingRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	// Inject the fake API into the configured backend so we don't shell out.
	doctor.(*wandbBackend).client = api
	result, err := doctor.Doctor(context.Background(), DoctorRequest{})
	if err != nil {
		t.Fatalf("Doctor err: %v", err)
	}
	if result.Provider != providerName {
		t.Fatalf("Doctor.Provider = %q, want %q", result.Provider, providerName)
	}
	if !strings.Contains(result.Message, "cli=ready") {
		t.Fatalf("Doctor message = %q, want cli=ready (CLIDoctorResult shape)", result.Message)
	}
	if !strings.Contains(result.Message, "cwsandbox=0.23.0") {
		t.Fatalf("Doctor message = %q, want runtime token for cwsandbox version", result.Message)
	}
	if !strings.Contains(result.Message, "leases=1") {
		t.Fatalf("Doctor message = %q, want leases=1", result.Message)
	}
}

func TestWandbDoctorRejectsOldCwsandbox(t *testing.T) {
	t.Setenv("WANDB_API_KEY", "fake")
	api := &fakeWandbAPI{versionValue: "0.19.5"}
	backend := newWandbBackendForTest(api)
	_, err := backend.Doctor(context.Background(), DoctorRequest{})
	if err == nil || !strings.Contains(err.Error(), "0.20.0") {
		t.Fatalf("err = %v, want minimum-version rejection", err)
	}
}

func TestWandbDoctorRequiresAPIKey(t *testing.T) {
	t.Setenv("WANDB_API_KEY", "")
	api := &fakeWandbAPI{versionValue: "0.23.0"}
	backend := newWandbBackendForTest(api)
	_, err := backend.Doctor(context.Background(), DoctorRequest{})
	if err == nil || !strings.Contains(err.Error(), "WANDB_API_KEY") {
		t.Fatalf("err = %v, want WANDB_API_KEY rejection", err)
	}
}

func TestWandbDoctorSurfacesShimError(t *testing.T) {
	t.Setenv("WANDB_API_KEY", "fake")
	api := &fakeWandbAPI{versionErr: errors.New("shim broken")}
	backend := newWandbBackendForTest(api)
	_, err := backend.Doctor(context.Background(), DoctorRequest{})
	if err == nil || !strings.Contains(err.Error(), "shim broken") {
		t.Fatalf("err = %v, want surfaced shim error", err)
	}
}

// TestWandbClientSurfacesNonZeroAsAPIError exercises the real Runtime.Exec
// runner path: a recording runner returns a non-zero exit + stderr message, and
// the client must surface that as *wandbAPIError so callers can branch on it.
func TestWandbClientSurfacesNonZeroAsAPIError(t *testing.T) {
	runner := &wandbRecordingRunner{fn: func(_ LocalCommandRequest) (LocalCommandResult, error) {
		return LocalCommandResult{ExitCode: 1, Stderr: `{"error":"WANDB_API_KEY not set"}`}, errors.New("exit status 1")
	}}
	client := &wandbShimClient{cfg: Config{Wandb: WandbConfig{Python: "python3"}}, rt: Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
	_, err := client.runShimCapture(context.Background(), []string{"version"})
	if err == nil {
		t.Fatal("client accepted non-zero shim exit")
	}
	var apiErr *wandbAPIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %T, want *wandbAPIError", err)
	}
	if apiErr.ExitCode != 1 {
		t.Fatalf("ExitCode = %d, want 1", apiErr.ExitCode)
	}
	if !strings.Contains(apiErr.Stderr, "WANDB_API_KEY not set") {
		t.Fatalf("Stderr = %q, want WANDB_API_KEY message", apiErr.Stderr)
	}
}

func TestWandbClientInvokesShimWithPythonAndSubcommand(t *testing.T) {
	runner := &wandbRecordingRunner{fn: func(req LocalCommandRequest) (LocalCommandResult, error) {
		if req.Name != "python3" {
			t.Errorf("Name = %q, want python3", req.Name)
		}
		if len(req.Args) < 2 || !strings.HasSuffix(req.Args[0], ".py") {
			t.Errorf("Args[0] = %q, want shim path", req.Args[0])
		}
		if req.Args[1] != "version" {
			t.Errorf("Args[1] = %q, want version", req.Args[1])
		}
		return LocalCommandResult{Stdout: ""}, nil
	}}
	client := &wandbShimClient{cfg: Config{Wandb: WandbConfig{Python: "python3"}}, rt: Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
	// Use the captured runner stdout via a custom write to confirm we feed the
	// version subcommand. The shim returns valid JSON in real life; the test
	// runner returns empty stdout so we expect an unmarshal error.
	if _, err := client.Version(context.Background()); err == nil {
		t.Fatal("Version accepted empty stdout from runner")
	}
}

func TestWandbClientCapturesStdoutForVersion(t *testing.T) {
	runner := &wandbRecordingRunner{fn: func(req LocalCommandRequest) (LocalCommandResult, error) {
		// Simulate the shim emitting JSON to stdout.
		if req.Stdout != nil {
			_, _ = req.Stdout.Write([]byte(`{"cwsandbox":"0.23.0"}`))
		}
		return LocalCommandResult{}, nil
	}}
	client := &wandbShimClient{cfg: Config{Wandb: WandbConfig{Python: "python3"}}, rt: Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
	version, err := client.Version(context.Background())
	if err != nil {
		t.Fatalf("Version err: %v", err)
	}
	if version != "0.23.0" {
		t.Fatalf("version = %q, want 0.23.0", version)
	}
}

func TestWandbVersionMeetsMinimum(t *testing.T) {
	for _, tc := range []struct {
		have, want string
		ok         bool
	}{
		{"0.23.0", "0.20.0", true},
		{"0.20.0", "0.20.0", true},
		{"0.20.1", "0.20.0", true},
		{"1.0.0", "0.20.0", true},
		{"0.19.99", "0.20.0", false},
		{"0.0.0", "0.20.0", false},
		{"", "0.20.0", false},
	} {
		if got := versionMeetsMinimum(tc.have, tc.want); got != tc.ok {
			t.Errorf("versionMeetsMinimum(%q, %q) = %v, want %v", tc.have, tc.want, got, tc.ok)
		}
	}
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
