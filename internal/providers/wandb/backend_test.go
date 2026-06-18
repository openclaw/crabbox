package wandb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"strings"
	"testing"
	"time"
)

// wandbRecordingRunner mirrors the recording-runner pattern used by
// internal/providers/exedev/backend_test.go: tests pre-load `fn` to assert
// arguments and return canned stdout/exit codes without actually invoking
// python3.
type wandbRecordingRunner struct {
	calls []LocalCommandRequest
	fn    func(LocalCommandRequest) (LocalCommandResult, error)
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
	// --wandb-python no longer exists (the python shim was replaced by a
	// native gRPC client); guard against a regression that would silently
	// reintroduce it.
	if fs.Lookup("wandb-python") != nil {
		t.Fatal("--wandb-python flag must not exist after the gRPC rewrite")
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
	cfg.Wandb.DefaultImage = "ubuntu:24.04"
	cfg.Wandb.MaxLifetimeSeconds = 1800
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	values := RegisterWandbProviderFlags(fs, cfg)
	if err := fs.Parse([]string{"--wandb-image", "ubuntu:22.04", "--wandb-max-lifetime", "3600"}); err != nil {
		t.Fatal(err)
	}
	if err := ApplyWandbProviderFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
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
	if cfg.Wandb.DefaultImage != "ubuntu:24.04" {
		t.Fatalf("DefaultImage=%q", cfg.Wandb.DefaultImage)
	}
	if cfg.Wandb.MaxLifetimeSeconds != 1800 {
		t.Fatalf("MaxLifetimeSeconds=%d", cfg.Wandb.MaxLifetimeSeconds)
	}
}

func TestWandbMaxLifetimeHonorsTTL(t *testing.T) {
	cfg := Config{}
	cfg.Wandb.MaxLifetimeSeconds = 1800
	cfg.TTL = time.Minute
	if got := wandbMaxLifetimeSeconds(cfg); got != 60 {
		t.Fatalf("wandbMaxLifetimeSeconds = %d, want 60", got)
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
	t.Setenv("CRABBOX_WANDB_API_KEY", "")
	// Point HOME at an empty temp dir so resolveAuth's ~/.netrc fallback
	// can't silently satisfy this test on a developer machine where
	// `wandb login` already wrote real credentials to ~/.netrc.
	t.Setenv("HOME", t.TempDir())
	backend := &wandbBackend{rt: Runtime{Stdout: io.Discard, Stderr: io.Discard}}
	_, err := backend.Run(context.Background(), RunRequest{NoSync: true, Command: []string{"echo", "hi"}})
	if err == nil || !strings.Contains(err.Error(), "W&B API key") {
		t.Fatalf("err = %v, want W&B API key rejection", err)
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
	versionValue     string
	versionErr       error
	acquired         wandbSandbox
	acquireErr       error
	acquireReq       wandbAcquireRequest
	execCmd          []string
	execID           string
	execCode         int
	execErr          error
	stopID           string
	stopMissingOK    bool
	stopErr          error
	listValue        []wandbSandbox
	listErr          error
	listTags         []string
	listStatusFilter string
	statusValue      wandbSandbox
	statusErr        error
}

type closeableFakeWandbAPI struct {
	fakeWandbAPI
	closes   int
	closeErr error
}

func (f *closeableFakeWandbAPI) Close() error {
	f.closes++
	return f.closeErr
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

func (f *fakeWandbAPI) Stop(_ context.Context, id string, _ int, missingOK bool) error {
	f.stopID = id
	f.stopMissingOK = missingOK
	return f.stopErr
}

func (f *fakeWandbAPI) List(_ context.Context, tags []string, statusFilter string) ([]wandbSandbox, error) {
	f.listTags = tags
	f.listStatusFilter = statusFilter
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

func TestWandbProviderAdvertisesRunSession(t *testing.T) {
	if !(Provider{}).Spec().Features.Has("run-session") {
		t.Fatalf("features=%#v want run session", Provider{}.Spec().Features)
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
	if result.Session == nil {
		t.Fatal("missing session handle")
	}
	if got := result.Session; got.Provider != providerName || got.LeaseID != "sb-abc" || got.Slug != "sb-abc" || got.Reused || got.Kept || got.CleanupCommand != "crabbox stop --provider wandb --id 'sb-abc'" {
		t.Fatalf("session = %#v", got)
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

func TestWandbRunClosesCachedClientAfterOperation(t *testing.T) {
	t.Setenv("WANDB_API_KEY", "fake")
	api := &closeableFakeWandbAPI{
		fakeWandbAPI: fakeWandbAPI{
			acquired: wandbSandbox{ID: "sb-abc", Status: "RUNNING"},
			execCode: 0,
		},
	}
	backend := newWandbBackendForTest(api)
	if _, err := backend.Run(context.Background(), RunRequest{NoSync: true, Command: []string{"echo", "hello"}}); err != nil {
		t.Fatalf("Run err: %v", err)
	}
	if api.stopID != "sb-abc" {
		t.Fatalf("Stop id = %q, want sb-abc before client close", api.stopID)
	}
	if api.closes != 1 {
		t.Fatalf("client closes=%d, want 1", api.closes)
	}
	if backend.client != nil {
		t.Fatal("backend retained closed client")
	}
	if err := backend.Close(); err != nil {
		t.Fatalf("second Close err: %v", err)
	}
	if api.closes != 1 {
		t.Fatalf("second Close changed closes=%d, want 1", api.closes)
	}
}

func TestWandbRunWithExistingIDSkipsAcquireAndStop(t *testing.T) {
	t.Setenv("WANDB_API_KEY", "fake")
	api := &fakeWandbAPI{execCode: 0}
	backend := newWandbBackendForTest(api)
	result, err := backend.Run(context.Background(), RunRequest{
		ID:      "sb-supplied",
		NoSync:  true,
		Command: []string{"echo"},
		Env:     map[string]string{"CI": "true"},
	})
	if err != nil {
		t.Fatalf("Run err: %v", err)
	}
	if result.Session == nil {
		t.Fatal("missing session handle")
	}
	if got := result.Session; got.LeaseID != "sb-supplied" || got.Slug != "sb-supplied" || !got.Reused || !got.Kept || got.CleanupCommand != "crabbox stop --provider wandb --id 'sb-supplied'" {
		t.Fatalf("session = %#v", got)
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
	if api.stopMissingOK {
		t.Fatal("explicit Stop used missingOK=true")
	}
}

func TestWandbDoctorReturnsInventoryResult(t *testing.T) {
	t.Setenv("WANDB_API_KEY", "fake")
	api := &fakeWandbAPI{versionValue: "coreweave.sandbox.v1beta2", listValue: []wandbSandbox{{ID: "sb-1"}}}
	doctor, err := Provider{}.ConfigureDoctor(Config{}, Runtime{Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	// Inject the fake API into the configured backend so we don't dial the
	// real cwsandbox gateway.
	doctor.(*wandbBackend).client = api
	result, err := doctor.Doctor(context.Background(), DoctorRequest{})
	if err != nil {
		t.Fatalf("Doctor err: %v", err)
	}
	if result.Provider != providerName {
		t.Fatalf("Doctor.Provider = %q, want %q", result.Provider, providerName)
	}
	if !strings.Contains(result.Message, "leases=1") {
		t.Fatalf("Doctor message = %q, want leases=1", result.Message)
	}
}

func TestWandbDoctorSurfacesAuthError(t *testing.T) {
	t.Setenv("WANDB_API_KEY", "fake")
	api := &fakeWandbAPI{versionErr: errors.New("UNAUTHENTICATED: invalid key")}
	backend := newWandbBackendForTest(api)
	_, err := backend.Doctor(context.Background(), DoctorRequest{})
	if err == nil {
		t.Fatal("Doctor accepted a Version() failure")
	}
	// Doctor now returns the underlying error untouched so cli.ExitError's
	// mapped exit code survives. Verify the message carries through.
	if !strings.Contains(err.Error(), "UNAUTHENTICATED") {
		t.Fatalf("err = %v, want the underlying Version error to be surfaced", err)
	}
}

func TestWandbKeepOnFailureRetainsSandbox(t *testing.T) {
	t.Setenv("WANDB_API_KEY", "fake")
	api := &fakeWandbAPI{
		acquired: wandbSandbox{ID: "sb-abc", Status: "running"},
		execCode: 7,
	}
	var stderr bytes.Buffer
	backend := newWandbBackendForTest(api)
	backend.rt.Stderr = &stderr
	result, err := backend.Run(context.Background(), RunRequest{
		NoSync:        true,
		KeepOnFailure: true,
		Command:       []string{"false"},
	})
	if result.ExitCode != 7 {
		t.Fatalf("exit = %d, want 7", result.ExitCode)
	}
	var ee ExitError
	if !errors.As(err, &ee) || ee.Code != 7 {
		t.Fatalf("err = %v, want ExitError code 7", err)
	}
	if api.stopID != "" {
		t.Fatalf("Stop called despite --keep-on-failure: id=%q", api.stopID)
	}
	if !strings.Contains(stderr.String(), "keep-on-failure: kept lease=") {
		t.Fatalf("missing keep-on-failure hint: %s", stderr.String())
	}
	if result.Session == nil || !result.Session.Kept {
		t.Fatalf("session = %#v, want kept failure handle", result.Session)
	}
}

func TestWandbCleanupCommandQuotesSandboxID(t *testing.T) {
	got := wandbCleanupCommand("sb-'quoted")
	want := `crabbox stop --provider wandb --id 'sb-'\''quoted'`
	if got != want {
		t.Fatalf("cleanup command = %q, want %q", got, want)
	}
}

func TestWandbRunForwardsEnvToAcquire(t *testing.T) {
	t.Setenv("WANDB_API_KEY", "fake")
	api := &fakeWandbAPI{
		acquired: wandbSandbox{ID: "sb-abc", Status: "running"},
		execCode: 0,
	}
	backend := newWandbBackendForTest(api)
	if _, err := backend.Run(context.Background(), RunRequest{
		NoSync:  true,
		Command: []string{"echo", "hi"},
		Env:     map[string]string{"FOO": "bar"},
	}); err != nil {
		t.Fatalf("Run err: %v", err)
	}
	if api.acquireReq.EnvironmentVars["FOO"] != "bar" {
		t.Fatalf("EnvironmentVars = %#v, want FOO=bar", api.acquireReq.EnvironmentVars)
	}
}

func TestWandbRunRejectsIDWithEnv(t *testing.T) {
	t.Setenv("WANDB_API_KEY", "fake")
	backend := newWandbBackendForTest(&fakeWandbAPI{})
	_, err := backend.Run(context.Background(), RunRequest{
		ID:         "sb-existing",
		NoSync:     true,
		Command:    []string{"echo"},
		Env:        map[string]string{"FOO": "bar"},
		EnvSummary: true,
	})
	if err == nil || !strings.Contains(err.Error(), "cannot forward env vars to an existing sandbox") {
		t.Fatalf("err = %v, want --id + env rejection", err)
	}
}

func TestWandbRunRejectsIDWithConfiguredEnv(t *testing.T) {
	t.Setenv("WANDB_API_KEY", "fake")
	backend := newWandbBackendForTest(&fakeWandbAPI{})
	_, err := backend.Run(context.Background(), RunRequest{
		ID:      "sb-existing",
		NoSync:  true,
		Command: []string{"echo"},
		Env:     map[string]string{"CUSTOM_TOKEN": "secret"},
	})
	if err == nil || !strings.Contains(err.Error(), "cannot forward env vars to an existing sandbox") {
		t.Fatalf("err = %v, want configured env rejection", err)
	}
}

func TestWandbRunEmitsTimingJSONOnFailure(t *testing.T) {
	t.Setenv("WANDB_API_KEY", "fake")
	api := &fakeWandbAPI{
		acquired: wandbSandbox{ID: "sb-abc", Status: "running"},
		execCode: 7,
	}
	var stderr bytes.Buffer
	backend := newWandbBackendForTest(api)
	backend.rt.Stderr = &stderr
	if _, err := backend.Run(context.Background(), RunRequest{
		NoSync:     true,
		TimingJSON: true,
		Command:    []string{"false"},
	}); err == nil {
		t.Fatal("Run accepted non-zero exec exit")
	}
	var report timingReport
	found := false
	for _, line := range strings.Split(strings.TrimSpace(stderr.String()), "\n") {
		if !strings.HasPrefix(line, "{") {
			continue
		}
		if err := json.Unmarshal([]byte(line), &report); err != nil {
			t.Fatalf("decode timing JSON: %v (line=%q)", err, line)
		}
		found = true
		break
	}
	if !found {
		t.Fatalf("missing timing JSON in stderr: %s", stderr.String())
	}
	if report.Provider != providerName || report.Slug != "sb-abc" || report.ExitCode != 7 {
		t.Fatalf("timing report = %#v", report)
	}
}

func TestWandbRunTimingJSONUsesExecErrorCode(t *testing.T) {
	t.Setenv("WANDB_API_KEY", "fake")
	api := &fakeWandbAPI{
		acquired: wandbSandbox{ID: "sb-abc", Status: "running"},
		execErr:  ExitError{Code: 69, Message: "unavailable"},
	}
	var stderr bytes.Buffer
	backend := newWandbBackendForTest(api)
	backend.rt.Stderr = &stderr
	if _, err := backend.Run(context.Background(), RunRequest{
		NoSync:     true,
		TimingJSON: true,
		Command:    []string{"echo", "hi"},
	}); err == nil {
		t.Fatal("Run accepted exec error")
	}
	var report timingReport
	for _, line := range strings.Split(strings.TrimSpace(stderr.String()), "\n") {
		if !strings.HasPrefix(line, "{") {
			continue
		}
		if err := json.Unmarshal([]byte(line), &report); err != nil {
			t.Fatalf("decode timing JSON: %v (line=%q)", err, line)
		}
		break
	}
	if report.ExitCode != 69 {
		t.Fatalf("timing exit = %d, want 69; stderr=%s", report.ExitCode, stderr.String())
	}
}

func TestWandbListAllIncludesStopped(t *testing.T) {
	api := &fakeWandbAPI{listValue: []wandbSandbox{{ID: "sb-done", Status: "completed"}}}
	backend := newWandbBackendForTest(api)
	if _, err := backend.List(context.Background(), ListRequest{All: true}); err != nil {
		t.Fatalf("List err: %v", err)
	}
	if api.listStatusFilter != "all" {
		t.Fatalf("list status filter = %q, want all", api.listStatusFilter)
	}
	if !contains(api.listTags, "crabbox") {
		t.Fatalf("list tags = %#v, want crabbox tag", api.listTags)
	}
}

func TestWandbWarmupRejected(t *testing.T) {
	backend := newWandbBackendForTest(&fakeWandbAPI{})
	err := backend.Warmup(context.Background(), WarmupRequest{})
	if err == nil || !strings.Contains(err.Error(), "does not support warmup") {
		t.Fatalf("err = %v, want warmup rejection", err)
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
