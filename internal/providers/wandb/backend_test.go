package wandb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"google.golang.org/grpc/codes"
)

// wandbRecordingRunner mirrors the recording-runner pattern used by
// internal/providers/exedev/backend_test.go: tests pre-load `fn` to assert
// arguments and return canned stdout/exit codes without actually invoking
// python3.
type wandbRecordingRunner struct {
	calls []core.LocalCommandRequest
	fn    func(core.LocalCommandRequest) (core.LocalCommandResult, error)
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
	selected := func(name string) bool {
		cfg := core.BaseConfig()
		cfg.Provider = name
		fs := flag.NewFlagSet("name-contract", flag.ContinueOnError)
		fs.String("class", "", "")
		p := Provider{}
		values := p.RegisterFlags(fs, cfg)
		if err := fs.Parse([]string{"--class=standard"}); err != nil {
			t.Fatal(err)
		}
		err := p.ApplyFlags(&cfg, fs, values)
		if err == nil {
			return false
		}
		if err.Error() != "--class is not supported for provider=wandb" {
			t.Fatalf("unexpected selection error: %v", err)
		}
		return true
	}
	for _, name := range []string{"wandb", "WANDB", "  wandb  ", "weights-and-biases"} {
		if !selected(name) {
			t.Fatalf("isWandbProviderName(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"", "railway", "wandbx"} {
		if selected(name) {
			t.Fatalf("isWandbProviderName(%q) = true, want false", name)
		}
	}
}

func TestWandbTokenFlagIsNotRegistered(t *testing.T) {
	cfg := core.Config{}
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
	cfg := core.Config{Provider: providerName}
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
			cfg := core.Config{Provider: provider}
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
	cfg := core.Config{WorkRoot: "/preserve/me", SSHUser: "alice"}
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
	cfg := core.Config{}
	cfg.Wandb.MaxLifetimeSeconds = 1800
	cfg.TTL = time.Minute
	if got := wandbMaxLifetimeSeconds(cfg); got != 60 {
		t.Fatalf("wandbMaxLifetimeSeconds = %d, want 60", got)
	}
}

func TestWandbRunRequiresNoSync(t *testing.T) {
	t.Setenv("WANDB_API_KEY", "fake")
	backend := &wandbBackend{rt: core.Runtime{Stdout: io.Discard, Stderr: io.Discard}}
	_, err := backend.Run(context.Background(), core.RunRequest{Command: []string{"echo", "hi"}})
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
	backend := &wandbBackend{rt: core.Runtime{Stdout: io.Discard, Stderr: io.Discard}}
	_, err := backend.Run(context.Background(), core.RunRequest{NoSync: true, Command: []string{"echo", "hi"}})
	if err == nil || !strings.Contains(err.Error(), "W&B API key") {
		t.Fatalf("err = %v, want W&B API key rejection", err)
	}
}

func TestWandbRunRejectsUnsupportedOptions(t *testing.T) {
	t.Setenv("WANDB_API_KEY", "fake")
	for _, tc := range []struct {
		name string
		req  core.RunRequest
		want string
	}{
		{name: "reclaim", req: core.RunRequest{NoSync: true, Reclaim: true, Command: []string{"echo"}}, want: "--reclaim"},
		{name: "shell", req: core.RunRequest{NoSync: true, ShellMode: true, Command: []string{"echo"}}, want: "--shell"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := &wandbBackend{rt: core.Runtime{Stdout: io.Discard, Stderr: io.Discard}}
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
	stopCalls        int
	listValue        []wandbSandbox
	listErr          error
	listTags         []string
	listStatusFilter string
	statusID         string
	statusValue      wandbSandbox
	statusErr        error
	statusValues     []wandbSandbox
	statusCalls      int
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
	f.stopCalls++
	f.stopID = id
	f.stopMissingOK = missingOK
	return f.stopErr
}

func (f *fakeWandbAPI) List(_ context.Context, tags []string, statusFilter string) ([]wandbSandbox, error) {
	f.listTags = tags
	f.listStatusFilter = statusFilter
	return f.listValue, f.listErr
}

func (f *fakeWandbAPI) Status(_ context.Context, id string) (wandbSandbox, error) {
	f.statusID = id
	f.statusCalls++
	if len(f.statusValues) > 0 {
		value := f.statusValues[0]
		f.statusValues = f.statusValues[1:]
		return value, f.statusErr
	}
	return f.statusValue, f.statusErr
}

func newWandbBackendForTest(t *testing.T, api wandbAPI) *wandbBackend {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("WANDB_ENTITY_NAME", "test-entity")
	t.Setenv("WANDB_PROJECT", "test-project")
	cfg := core.Config{Provider: providerName}
	applyWandbDefaults(&cfg)
	return &wandbBackend{
		spec:   Provider{}.Spec(),
		cfg:    cfg,
		rt:     core.Runtime{Stdout: io.Discard, Stderr: io.Discard},
		client: api,
	}
}

func seedWandbClaim(t *testing.T, backend *wandbBackend, sandboxID string) core.LeaseClaim {
	t.Helper()
	scope, err := wandbProviderScope()
	if err != nil {
		t.Fatal(err)
	}
	claim, err := claimWandbSandbox(sandboxID, scope, backend.cfg)
	if err != nil {
		t.Fatalf("claim sandbox: %v", err)
	}
	return claim
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
	backend := newWandbBackendForTest(t, api)
	result, err := backend.Run(context.Background(), core.RunRequest{NoSync: true, Command: []string{"echo", "hello"}})
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
	if _, ok, err := resolveWandbClaim("sb-abc"); err != nil || ok {
		t.Fatalf("auto-stop left claim ok=%v err=%v", ok, err)
	}
}

func TestWandbRunRollsBackWhenClaimCannotBePersisted(t *testing.T) {
	api := &fakeWandbAPI{acquired: wandbSandbox{ID: "sb-rollback", Status: "running"}}
	backend := newWandbBackendForTest(t, api)
	blockedState := t.TempDir() + "/not-a-directory"
	if err := os.WriteFile(blockedState, []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", blockedState)

	_, err := backend.Run(context.Background(), core.RunRequest{NoSync: true, Command: []string{"true"}})
	if err == nil || !strings.Contains(err.Error(), "ownership claim") {
		t.Fatalf("Run err = %v, want claim persistence failure", err)
	}
	if api.execID != "" {
		t.Fatalf("claim failure reached Exec(%q)", api.execID)
	}
	if api.stopID != "sb-rollback" || !api.stopMissingOK {
		t.Fatalf("rollback stop id=%q missingOK=%v", api.stopID, api.stopMissingOK)
	}
}

func TestWandbRunClosesCachedClientAfterOperation(t *testing.T) {
	t.Setenv("WANDB_API_KEY", "fake")
	api := &closeableFakeWandbAPI{
		fakeWandbAPI: fakeWandbAPI{
			acquired: wandbSandbox{ID: "sb-abc", Status: "RUNNING"},
			execCode: 0,
		},
		closeErr: errors.New("connection close failed"),
	}
	backend := newWandbBackendForTest(t, api)
	if _, err := backend.Run(context.Background(), core.RunRequest{NoSync: true, Command: []string{"echo", "hello"}}); err != nil {
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
	api := &fakeWandbAPI{execCode: 0, listValue: []wandbSandbox{{ID: "sb-supplied"}}}
	backend := newWandbBackendForTest(t, api)
	seedWandbClaim(t, backend, "sb-supplied")
	result, err := backend.Run(context.Background(), core.RunRequest{
		ID:      "sb-supplied",
		NoSync:  true,
		Command: []string{"echo"},
		Env:     map[string]string{"CI": "true", "CRABBOX_LEASE_ID": "sb-supplied", "CRABBOX_RUN_ID": "run-fixture", "CRABBOX_SLUG": "fixture"},
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
	if !contains(api.listTags, "crabbox") || api.listStatusFilter != "all" {
		t.Fatalf("ownership list tags=%#v status=%q", api.listTags, api.listStatusFilter)
	}
}

func TestWandbRunRejectsUnownedExistingID(t *testing.T) {
	api := &fakeWandbAPI{listValue: []wandbSandbox{{ID: "sb-foreign"}}}
	backend := newWandbBackendForTest(t, api)
	_, err := backend.Run(context.Background(), core.RunRequest{ID: "sb-foreign", NoSync: true, Command: []string{"echo"}})
	if err == nil || !strings.Contains(err.Error(), "no matching local ownership claim") {
		t.Fatalf("Run err = %v, want ownership rejection", err)
	}
	if api.execID != "" || api.stopID != "" {
		t.Fatalf("unowned sandbox reached exec=%q stop=%q", api.execID, api.stopID)
	}
}

func TestWandbRunFailsClosedWhenOwnershipListFails(t *testing.T) {
	wantErr := errors.New("list unavailable")
	api := &fakeWandbAPI{listErr: wantErr}
	backend := newWandbBackendForTest(t, api)
	seedWandbClaim(t, backend, "sb-unknown")
	_, err := backend.Run(context.Background(), core.RunRequest{ID: "sb-unknown", NoSync: true, Command: []string{"echo"}})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Run err = %v, want %v", err, wantErr)
	}
	if api.execID != "" {
		t.Fatalf("ownership list failure reached Exec(%q)", api.execID)
	}
}

func TestWandbRunNonZeroExecMapsToExit(t *testing.T) {
	t.Setenv("WANDB_API_KEY", "fake")
	api := &fakeWandbAPI{acquired: wandbSandbox{ID: "sb-abc", Status: "RUNNING"}, execCode: 7}
	backend := newWandbBackendForTest(t, api)
	result, err := backend.Run(context.Background(), core.RunRequest{NoSync: true, Command: []string{"false"}})
	if err == nil {
		t.Fatal("Run accepted non-zero exec exit")
	}
	if result.ExitCode != 7 {
		t.Fatalf("exit = %d, want 7", result.ExitCode)
	}
}

func TestWandbStatusReturnsView(t *testing.T) {
	t.Setenv("WANDB_API_KEY", "fake")
	api := &fakeWandbAPI{
		listValue:   []wandbSandbox{{ID: "sb-abc"}},
		statusValue: wandbSandbox{ID: "sb-abc", Status: "RUNNING", CreatedAt: "2026-05-18T00:00:00Z"},
	}
	backend := newWandbBackendForTest(t, api)
	seedWandbClaim(t, backend, "sb-abc")
	view, err := backend.Status(context.Background(), core.StatusRequest{ID: "sb-abc"})
	if err != nil {
		t.Fatalf("Status err: %v", err)
	}
	if view.ID != "sb-abc" || view.Provider != providerName || !view.Ready || view.State != "running" {
		t.Fatalf("view = %#v", view)
	}
	if api.statusID != "sb-abc" || !contains(api.listTags, "crabbox") || api.listStatusFilter != "all" {
		t.Fatalf("status id=%q list tags=%#v filter=%q", api.statusID, api.listTags, api.listStatusFilter)
	}
}

func TestWandbStatusWaitPollsUntilRunning(t *testing.T) {
	api := &fakeWandbAPI{
		listValue: []wandbSandbox{{ID: "sb-abc"}},
		statusValues: []wandbSandbox{
			{ID: "sb-abc", Status: "CREATING"},
			{ID: "sb-abc", Status: "RUNNING", CreatedAt: "2026-05-18T00:00:00Z"},
		},
	}
	backend := newWandbBackendForTest(t, api)
	seedWandbClaim(t, backend, "sb-abc")

	view, err := backend.Status(context.Background(), core.StatusRequest{ID: "sb-abc", Wait: true, WaitTimeout: time.Second})
	if err != nil {
		t.Fatalf("Status err: %v", err)
	}
	if !view.Ready || view.State != "running" || view.Labels["created_at"] != "2026-05-18T00:00:00Z" {
		t.Fatalf("view=%#v, want final running status", view)
	}
	if api.statusCalls != 2 {
		t.Fatalf("status calls=%d, want creating then running", api.statusCalls)
	}
}

func TestWandbStatusWaitReturnsTerminalState(t *testing.T) {
	for _, state := range []string{"stopped", "failed", "terminated"} {
		t.Run(state, func(t *testing.T) {
			api := &fakeWandbAPI{
				listValue:   []wandbSandbox{{ID: "sb-abc"}},
				statusValue: wandbSandbox{ID: "sb-abc", Status: state},
			}
			backend := newWandbBackendForTest(t, api)
			seedWandbClaim(t, backend, "sb-abc")

			view, err := backend.Status(context.Background(), core.StatusRequest{ID: "sb-abc", Wait: true, WaitTimeout: time.Second})
			if err != nil {
				t.Fatalf("Status err: %v", err)
			}
			if view.Ready || view.State != state || api.statusCalls != 1 {
				t.Fatalf("view=%#v calls=%d, want immediate terminal state", view, api.statusCalls)
			}
		})
	}
}

func TestWandbStatusWaitHonorsTimeout(t *testing.T) {
	api := &fakeWandbAPI{
		listValue:   []wandbSandbox{{ID: "sb-abc"}},
		statusValue: wandbSandbox{ID: "sb-abc", Status: "CREATING"},
	}
	backend := newWandbBackendForTest(t, api)
	seedWandbClaim(t, backend, "sb-abc")

	_, err := backend.Status(context.Background(), core.StatusRequest{ID: "sb-abc", Wait: true, WaitTimeout: 10 * time.Millisecond})
	var exitErr core.ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 5 || !strings.Contains(err.Error(), "sb-abc") {
		t.Fatalf("Status err=%v, want sandbox-specific timeout exit", err)
	}
	if api.statusCalls != 1 {
		t.Fatalf("status calls=%d, want one bounded probe", api.statusCalls)
	}
}

func TestWandbStatusWithoutWaitReturnsImmediately(t *testing.T) {
	api := &fakeWandbAPI{
		listValue:   []wandbSandbox{{ID: "sb-abc"}},
		statusValue: wandbSandbox{ID: "sb-abc", Status: "CREATING"},
	}
	backend := newWandbBackendForTest(t, api)
	seedWandbClaim(t, backend, "sb-abc")

	view, err := backend.Status(context.Background(), core.StatusRequest{ID: "sb-abc"})
	if err != nil || view.State != "creating" || view.Ready || api.statusCalls != 1 {
		t.Fatalf("view=%#v err=%v calls=%d, want immediate creating status", view, err, api.statusCalls)
	}
}

func TestWandbStatusRequiresID(t *testing.T) {
	backend := newWandbBackendForTest(t, &fakeWandbAPI{})
	for _, id := range []string{"", "  \t"} {
		if _, err := backend.Status(context.Background(), core.StatusRequest{ID: id}); err == nil {
			t.Fatalf("Status accepted empty id %q", id)
		}
	}
}

func TestWandbStatusRejectsUnownedID(t *testing.T) {
	api := &fakeWandbAPI{listValue: []wandbSandbox{{ID: "sb-foreign"}}}
	backend := newWandbBackendForTest(t, api)
	_, err := backend.Status(context.Background(), core.StatusRequest{ID: "sb-foreign"})
	if err == nil || !strings.Contains(err.Error(), "no matching local ownership claim") {
		t.Fatalf("Status err = %v, want ownership rejection", err)
	}
	if api.statusID != "" {
		t.Fatalf("unowned sandbox reached Status(%q)", api.statusID)
	}
}

func TestWandbListEnumeratesSandboxes(t *testing.T) {
	api := &fakeWandbAPI{listValue: []wandbSandbox{
		{ID: "sb-1", Status: "RUNNING"},
		{ID: "sb-2", Status: "COMPLETED"},
	}}
	backend := newWandbBackendForTest(t, api)
	servers, err := backend.List(context.Background(), core.ListRequest{})
	if err != nil {
		t.Fatalf("List err: %v", err)
	}
	if len(servers) != 2 || servers[0].CloudID != "sb-1" || servers[1].CloudID != "sb-2" {
		t.Fatalf("List = %#v", servers)
	}
}

func TestWandbStopRequiresID(t *testing.T) {
	backend := newWandbBackendForTest(t, &fakeWandbAPI{})
	for _, id := range []string{"", "  \t"} {
		if err := backend.Stop(context.Background(), core.StopRequest{ID: id}); err == nil {
			t.Fatalf("Stop accepted empty id %q", id)
		}
	}
}

func TestWandbStopCallsClient(t *testing.T) {
	api := &fakeWandbAPI{listValue: []wandbSandbox{{ID: "sb-abc"}}}
	backend := newWandbBackendForTest(t, api)
	seedWandbClaim(t, backend, "sb-abc")
	if err := backend.Stop(context.Background(), core.StopRequest{ID: "sb-abc"}); err != nil {
		t.Fatalf("Stop err: %v", err)
	}
	if api.stopID != "sb-abc" {
		t.Fatalf("Stop called with %q, want sb-abc", api.stopID)
	}
	if api.stopMissingOK {
		t.Fatal("explicit Stop used missingOK=true")
	}
	if !contains(api.listTags, "crabbox") || api.listStatusFilter != "all" {
		t.Fatalf("ownership list tags=%#v status=%q", api.listTags, api.listStatusFilter)
	}
	if _, ok, err := resolveWandbClaim("sb-abc"); err != nil || ok {
		t.Fatalf("successful stop left claim ok=%v err=%v", ok, err)
	}
}

func TestWandbStopFailurePreservesClaim(t *testing.T) {
	wantErr := errors.New("stop unavailable")
	api := &fakeWandbAPI{listValue: []wandbSandbox{{ID: "sb-abc"}}, stopErr: wantErr}
	backend := newWandbBackendForTest(t, api)
	seedWandbClaim(t, backend, "sb-abc")

	err := backend.Stop(context.Background(), core.StopRequest{ID: "sb-abc"})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Stop err = %v, want %v", err, wantErr)
	}
	claim, ok, resolveErr := resolveWandbClaim("sb-abc")
	if resolveErr != nil || !ok || claim.CloudID != "sb-abc" {
		t.Fatalf("failed stop claim=%#v ok=%v err=%v", claim, ok, resolveErr)
	}
}

func TestWandbStopRemovesStaleClaimWhenSandboxIsGone(t *testing.T) {
	api := &fakeWandbAPI{statusErr: &wandbAPIError{Code: codes.NotFound, ExitCode: 4, Stderr: "Get: not found"}}
	backend := newWandbBackendForTest(t, api)
	seedWandbClaim(t, backend, "sb-abc")

	if err := backend.Stop(context.Background(), core.StopRequest{ID: "sb-abc"}); err != nil {
		t.Fatalf("Stop err: %v", err)
	}
	if api.statusID != "sb-abc" || api.stopID != "" {
		t.Fatalf("statusID=%q stopID=%q, want missing check without provider stop", api.statusID, api.stopID)
	}
	if _, ok, err := resolveWandbClaim("sb-abc"); err != nil || ok {
		t.Fatalf("stale claim remains ok=%v err=%v", ok, err)
	}
}

func TestWandbStopPreservesClaimWhenSandboxLostManagedTag(t *testing.T) {
	api := &fakeWandbAPI{statusValue: wandbSandbox{ID: "sb-abc", Status: "RUNNING"}}
	backend := newWandbBackendForTest(t, api)
	seedWandbClaim(t, backend, "sb-abc")

	err := backend.Stop(context.Background(), core.StopRequest{ID: "sb-abc"})
	if err == nil || !strings.Contains(err.Error(), "still exists but is not tagged as Crabbox-managed") {
		t.Fatalf("Stop err=%v, want untagged refusal", err)
	}
	if api.statusID != "sb-abc" || api.stopID != "" {
		t.Fatalf("untagged sandbox status=%q stop=%q", api.statusID, api.stopID)
	}
	if claim, ok, resolveErr := resolveWandbClaim("sb-abc"); resolveErr != nil || !ok || claim.CloudID != "sb-abc" {
		t.Fatalf("untagged sandbox claim=%#v ok=%v err=%v", claim, ok, resolveErr)
	}
}

func TestWandbStopPreservesClaimWhenOwnershipLookupFails(t *testing.T) {
	tests := []struct {
		name string
		api  *fakeWandbAPI
	}{
		{name: "list", api: &fakeWandbAPI{listErr: errors.New("list unavailable")}},
		{name: "status", api: &fakeWandbAPI{statusErr: errors.New("status unavailable")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend := newWandbBackendForTest(t, tt.api)
			seedWandbClaim(t, backend, "sb-unknown")

			if err := backend.Stop(context.Background(), core.StopRequest{ID: "sb-unknown"}); err == nil {
				t.Fatal("Stop succeeded despite ownership lookup failure")
			}
			if tt.api.stopID != "" {
				t.Fatalf("ownership lookup failure reached Stop(%q)", tt.api.stopID)
			}
			claim, ok, resolveErr := resolveWandbClaim("sb-unknown")
			if resolveErr != nil || !ok || claim.CloudID != "sb-unknown" {
				t.Fatalf("ownership lookup failure claim=%#v ok=%v err=%v", claim, ok, resolveErr)
			}
		})
	}
}

func TestWandbStatusMissingPreservesClaimAndReturnsExitError(t *testing.T) {
	api := &fakeWandbAPI{statusErr: &wandbAPIError{Code: codes.NotFound, ExitCode: 4, Stderr: "Get: not found"}}
	backend := newWandbBackendForTest(t, api)
	seedWandbClaim(t, backend, "sb-abc")

	_, err := backend.Status(context.Background(), core.StatusRequest{ID: "sb-abc"})
	var exitErr core.ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 4 {
		t.Fatalf("Status err=%v, want ExitError code 4", err)
	}
	if claim, ok, resolveErr := resolveWandbClaim("sb-abc"); resolveErr != nil || !ok || claim.CloudID != "sb-abc" {
		t.Fatalf("missing status claim=%#v ok=%v err=%v", claim, ok, resolveErr)
	}
}

func TestWandbStopRejectsClaimFromAnotherScope(t *testing.T) {
	api := &fakeWandbAPI{listValue: []wandbSandbox{{ID: "sb-abc"}}}
	backend := newWandbBackendForTest(t, api)
	seedWandbClaim(t, backend, "sb-abc")
	t.Setenv("WANDB_PROJECT", "another-project")

	err := backend.Stop(context.Background(), core.StopRequest{ID: "sb-abc"})
	if err == nil || !strings.Contains(err.Error(), "different endpoint, entity, or project") {
		t.Fatalf("Stop err = %v, want scope rejection", err)
	}
	if api.stopID != "" {
		t.Fatalf("scope mismatch reached Stop(%q)", api.stopID)
	}
}

func TestWandbStopRejectsUnownedID(t *testing.T) {
	api := &fakeWandbAPI{listValue: []wandbSandbox{{ID: "sb-foreign"}}}
	backend := newWandbBackendForTest(t, api)
	err := backend.Stop(context.Background(), core.StopRequest{ID: "sb-foreign"})
	if err == nil || !strings.Contains(err.Error(), "no matching local ownership claim") {
		t.Fatalf("Stop err = %v, want ownership rejection", err)
	}
	if api.stopID != "" {
		t.Fatalf("unowned sandbox reached Stop(%q)", api.stopID)
	}
}

func TestWandbDoctorReturnsInventoryResult(t *testing.T) {
	t.Setenv("WANDB_API_KEY", "fake")
	api := &fakeWandbAPI{versionValue: "coreweave.sandbox.v1beta2", listValue: []wandbSandbox{{ID: "sb-1"}}}
	doctor, err := core.ConfigureProviderDoctor(Provider{}, core.Config{}, core.Runtime{Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	// Inject the fake API into the configured backend so we don't dial the
	// real cwsandbox gateway.
	doctor.(*wandbBackend).client = api
	result, err := doctor.Doctor(context.Background(), core.DoctorRequest{})
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
	backend := newWandbBackendForTest(t, api)
	_, err := backend.Doctor(context.Background(), core.DoctorRequest{})
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
	backend := newWandbBackendForTest(t, api)
	backend.rt.Stderr = &stderr
	result, err := backend.Run(context.Background(), core.RunRequest{
		NoSync:        true,
		KeepOnFailure: true,
		Command:       []string{"false"},
	})
	if result.ExitCode != 7 {
		t.Fatalf("exit = %d, want 7", result.ExitCode)
	}
	var ee core.ExitError
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
	claim, ok, resolveErr := resolveWandbClaim("sb-abc")
	if resolveErr != nil || !ok || claim.CloudID != "sb-abc" || claim.ProviderScope == "" {
		t.Fatalf("kept sandbox claim=%#v ok=%v err=%v", claim, ok, resolveErr)
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
	backend := newWandbBackendForTest(t, api)
	if _, err := backend.Run(context.Background(), core.RunRequest{
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
	backend := newWandbBackendForTest(t, &fakeWandbAPI{})
	_, err := backend.Run(context.Background(), core.RunRequest{
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
	backend := newWandbBackendForTest(t, &fakeWandbAPI{})
	_, err := backend.Run(context.Background(), core.RunRequest{
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
	backend := newWandbBackendForTest(t, api)
	backend.rt.Stderr = &stderr
	if _, err := backend.Run(context.Background(), core.RunRequest{
		NoSync:     true,
		TimingJSON: true,
		Command:    []string{"false"},
	}); err == nil {
		t.Fatal("Run accepted non-zero exec exit")
	}
	var report core.TimingReport
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
	if report.RunStatus != "failed" || report.ErrorKind != "command-exit" {
		t.Fatalf("timing outcome status=%q kind=%q", report.RunStatus, report.ErrorKind)
	}
}

func TestWandbRunTimingJSONUsesExecErrorCode(t *testing.T) {
	t.Setenv("WANDB_API_KEY", "fake")
	api := &fakeWandbAPI{
		acquired: wandbSandbox{ID: "sb-abc", Status: "running"},
		execErr:  core.ExitError{Code: 69, Message: "unavailable"},
	}
	var stderr bytes.Buffer
	backend := newWandbBackendForTest(t, api)
	backend.rt.Stderr = &stderr
	if _, err := backend.Run(context.Background(), core.RunRequest{
		NoSync:     true,
		TimingJSON: true,
		Command:    []string{"echo", "hi"},
	}); err == nil {
		t.Fatal("Run accepted exec error")
	}
	var report core.TimingReport
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
	if report.RunStatus != "failed" || report.ErrorKind != "provider-error" {
		t.Fatalf("timing outcome status=%q kind=%q", report.RunStatus, report.ErrorKind)
	}
}

func TestWandbListAllIncludesStopped(t *testing.T) {
	api := &fakeWandbAPI{listValue: []wandbSandbox{{ID: "sb-done", Status: "completed"}}}
	backend := newWandbBackendForTest(t, api)
	if _, err := backend.List(context.Background(), core.ListRequest{All: true}); err != nil {
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
	backend := newWandbBackendForTest(t, &fakeWandbAPI{})
	err := backend.Warmup(context.Background(), core.WarmupRequest{})
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

// Fail only the timing record, so provisioning and recovery diagnostics remain observable.
type wandbTimingWriter struct {
	bytes.Buffer
	err error
}

func (w *wandbTimingWriter) Write(p []byte) (int, error) {
	if w.err != nil && bytes.HasPrefix(p, []byte("{")) {
		return 0, w.err
	}
	return w.Buffer.Write(p)
}

func TestWandbRunTerminalFailures(t *testing.T) {
	cleanupErr := errors.New("owned stop failed")
	writerErr := errors.New("timing output failed")
	transportErr := errors.New("exec transport failed")
	for _, tc := range []struct {
		name                     string
		commandCode              int
		execErr                  error
		cleanupErr               error
		writerErr                error
		keep, keepFailure, reuse bool
		wantCode                 int
		wantKind                 core.RunErrorKind
		wantKept                 bool
		wantStops                int
	}{
		{name: "success", wantKind: core.RunErrorNone, wantStops: 1},
		{name: "success cleanup fails", cleanupErr: cleanupErr, wantCode: 1, wantKind: core.RunErrorProvider, wantKept: true, wantStops: 1},
		{name: "command cleanup fails", commandCode: 7, cleanupErr: cleanupErr, wantCode: 7, wantKind: core.RunErrorCommandExit, wantKept: true, wantStops: 1},
		{name: "command writer fails", commandCode: 7, writerErr: writerErr, wantCode: 7, wantKind: core.RunErrorCommandExit, wantStops: 1},
		{name: "command cleanup and writer fail", commandCode: 7, cleanupErr: cleanupErr, writerErr: writerErr, wantCode: 7, wantKind: core.RunErrorCommandExit, wantKept: true, wantStops: 1},
		{name: "failure retention survives writer", commandCode: 7, writerErr: writerErr, keepFailure: true, wantCode: 7, wantKind: core.RunErrorCommandExit, wantKept: true},
		{name: "success writer fails after deletion", writerErr: writerErr, wantCode: 1, wantKind: core.RunErrorProvider, wantStops: 1},
		{name: "transport cleanup and writer fail", execErr: transportErr, cleanupErr: cleanupErr, writerErr: writerErr, wantCode: 1, wantKind: core.RunErrorProvider, wantKept: true, wantStops: 1},
		{name: "cancellation writer fails", execErr: context.Canceled, writerErr: writerErr, keepFailure: true, wantCode: 1, wantKind: core.RunErrorCanceled, wantKept: true},
		{name: "deadline writer fails", execErr: context.DeadlineExceeded, writerErr: writerErr, keepFailure: true, wantCode: 1, wantKind: core.RunErrorTimeout, wantKept: true},
		{name: "keep success", keep: true, wantKind: core.RunErrorNone, wantKept: true},
		{name: "reuse failure", reuse: true, commandCode: 7, wantCode: 7, wantKind: core.RunErrorCommandExit, wantKept: true},
		{name: "grpc unavailable", execErr: &wandbAPIError{ExitCode: 69, Code: codes.Unavailable}, writerErr: writerErr, keepFailure: true, wantCode: 69, wantKind: core.RunErrorProvider, wantKept: true},
		{name: "grpc permission", execErr: &wandbAPIError{ExitCode: 77, Code: codes.PermissionDenied}, cleanupErr: cleanupErr, wantCode: 77, wantKind: core.RunErrorProvider, wantKept: true, wantStops: 1},
		{name: "grpc deadline", execErr: &wandbAPIError{ExitCode: 124, Code: codes.DeadlineExceeded}, cleanupErr: cleanupErr, wantCode: 124, wantKind: core.RunErrorProvider, wantKept: true, wantStops: 1},
		{name: "grpc missing", execErr: &wandbAPIError{ExitCode: 4, Code: codes.NotFound}, cleanupErr: cleanupErr, wantCode: 4, wantKind: core.RunErrorProvider, wantKept: true, wantStops: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &fakeWandbAPI{acquired: wandbSandbox{ID: "sb-terminal", Status: "running"}, execCode: tc.commandCode, execErr: tc.execErr, stopErr: tc.cleanupErr}
			b := newWandbBackendForTest(t, api)
			writer := &wandbTimingWriter{err: tc.writerErr}
			b.rt.Stderr = writer
			req := core.RunRequest{NoSync: true, TimingJSON: true, Command: []string{"true"}, Keep: tc.keep, KeepOnFailure: tc.keepFailure}
			if tc.reuse {
				seedWandbClaim(t, b, "sb-terminal")
				req.ID = "sb-terminal"
				api.listValue = []wandbSandbox{{ID: "sb-terminal", Status: "running"}}
			}
			result, err := b.Run(context.Background(), req)
			// Match core's public normalization boundary for baseline controls.
			result = core.FinalizeRunResult(result, err)
			if result.ExitCode != tc.wantCode || result.ErrorKind != tc.wantKind {
				t.Errorf("outcome code=%d kind=%q want code=%d kind=%q; err=%v", result.ExitCode, result.ErrorKind, tc.wantCode, tc.wantKind, err)
			}
			if result.Session == nil || result.Session.Kept != tc.wantKept || result.Session.Reused != tc.reuse {
				t.Errorf("session=%+v want kept=%v reused=%v", result.Session, tc.wantKept, tc.reuse)
			}
			if api.stopCalls != tc.wantStops {
				t.Errorf("stop calls=%d want=%d", api.stopCalls, tc.wantStops)
			}
			_, exists, claimErr := resolveWandbClaim("sb-terminal")
			if claimErr != nil || exists != tc.wantKept {
				t.Errorf("claim exists=%v err=%v want=%v", exists, claimErr, tc.wantKept)
			}
			if tc.wantCode == 0 {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			} else {
				var public core.ExitError
				if !errors.As(err, &public) || public.Code != tc.wantCode {
					t.Errorf("public=%+v err=%v want code=%d", public, err, tc.wantCode)
				}
				for _, cause := range []error{tc.execErr, tc.cleanupErr, tc.writerErr} {
					if cause != nil && (!errors.Is(err, cause) || !strings.Contains(public.Message, cause.Error())) {
						t.Errorf("missing cause/display %q: public=%q err=%v", cause, public.Message, err)
					}
				}
				if tc.commandCode != 0 && !strings.Contains(public.Message, "sandbox exit=7") {
					t.Errorf("primary command diagnostic missing: %q", public.Message)
				}
			}
			if tc.writerErr == nil {
				var report core.TimingReport
				found := false
				for _, line := range strings.Split(writer.String(), "\n") {
					if strings.HasPrefix(line, "{") {
						if decodeErr := json.Unmarshal([]byte(line), &report); decodeErr != nil {
							t.Fatal(decodeErr)
						}
						found = true
					}
				}
				if !found || report.ExitCode != result.ExitCode || report.RunStatus != result.Status || report.ErrorKind != result.ErrorKind {
					t.Errorf("timing found=%v report=%+v result=%+v", found, report, result)
				}
			}
		})
	}
}

func TestWandbRunTypedTimingWriterPreservesPublicCode(t *testing.T) {
	writerErr := core.ExitError{Code: 69, Message: "custom timing writer unavailable"}
	api := &fakeWandbAPI{acquired: wandbSandbox{ID: "sb-writer", Status: "running"}}
	b := newWandbBackendForTest(t, api)
	b.rt.Stderr = &wandbTimingWriter{err: writerErr}
	result, err := b.Run(context.Background(), core.RunRequest{NoSync: true, TimingJSON: true, Command: []string{"true"}})
	var public core.ExitError
	if !errors.As(err, &public) || public.Code != 69 || !errors.Is(err, writerErr) {
		t.Fatalf("publicCode=%d err=%v; want original writer code69 and cause", public.Code, err)
	}
	if result.Session == nil || result.Session.Kept || api.stopCalls != 1 {
		t.Fatalf("typed writer changed actual deletion: session=%+v stops=%d", result.Session, api.stopCalls)
	}
}

func TestWandbExistingIDEnvironmentPolicy(t *testing.T) {
	for _, tc := range []struct {
		name           string
		env            map[string]string
		explicit, want bool
	}{
		{name: "reserved-only", env: map[string]string{"CRABBOX_LEASE_ID": "sb", "CRABBOX_RUN_ID": "run", "CRABBOX_SLUG": "slug"}, want: true},
		{name: "reserved-only-summary", env: map[string]string{"CRABBOX_RUN_ID": "run"}, explicit: true, want: true},
		{name: "reserved-casefold", env: map[string]string{"CrAbBoX_SlUg": "slug"}, want: true},
		{name: "implicit-defaults", env: map[string]string{"CI": "true", "NODE_OPTIONS": "fixture"}, want: true},
		{name: "reserved-plus-implicit-defaults", env: map[string]string{"CRABBOX_RUN_ID": "run", "CI": "true", "NODE_OPTIONS": "fixture"}, want: true},
		{name: "explicit-ci", env: map[string]string{"CRABBOX_RUN_ID": "run", "CI": "true"}, explicit: true},
		{name: "explicit-node", env: map[string]string{"NODE_OPTIONS": "fixture"}, explicit: true},
		{name: "custom", env: map[string]string{"CUSTOM": "fixture"}},
		{name: "prefix-lookalike", env: map[string]string{"CRABBOX_RUN_ID_EXTRA": "fixture"}},
		{name: "suffix-lookalike", env: map[string]string{"PREFIX_CRABBOX_RUN_ID": "fixture"}},
		{name: "padded-lookalike", env: map[string]string{" CRABBOX_RUN_ID": "fixture"}},
		{name: "lowercase-default-not-exception", env: map[string]string{"ci": "true"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := wandbExistingIDEnvCanBeOmitted(core.RunRequest{Env: tc.env, EnvSummary: tc.explicit})
			t.Logf("actual=%t desired=%t explicit=%t", got, tc.want, tc.explicit)
			if got != tc.want {
				t.Errorf("reuse environment accepted=%t want=%t", got, tc.want)
			}
		})
	}
}

func TestWandbBindingFlagContract(t *testing.T) {
	for _, selector := range []string{"wandb", " WEIGHTS-AND-BIASES ", "aws"} {
		for _, life := range []int{-2, 0, 45} {
			cfg := core.BaseConfig()
			cfg.Provider = selector
			fs := flag.NewFlagSet("contract", flag.ContinueOnError)
			values := RegisterWandbProviderFlags(fs, cfg)
			count := 0
			fs.VisitAll(func(*flag.Flag) { count++ })
			if count != 2 || fs.Lookup("wandb-image").DefValue != "" || fs.Lookup("wandb-max-lifetime").DefValue != "0" {
				t.Fatal("raw registration defaults changed")
			}
			cfg.Wandb.DefaultImage = "layered-image"
			cfg.Wandb.MaxLifetimeSeconds = 37
			before := cfg.Wandb
			if err := ApplyWandbProviderFlags(&cfg, fs, values); err != nil {
				t.Fatal(err)
			}
			if cfg.Wandb != before {
				t.Fatal("unvisited flags changed config")
			}
			if err := fs.Parse([]string{"--wandb-image=", fmt.Sprintf("--wandb-max-lifetime=%d", life)}); err != nil {
				t.Fatal(err)
			}
			snapshot := fmt.Sprintf("%#v", cfg)
			if err := ApplyWandbProviderFlags(&cfg, fs, struct{}{}); err != nil {
				t.Fatal(err)
			}
			if fmt.Sprintf("%#v", cfg) != snapshot {
				t.Fatal("wrong values type changed config")
			}
			if err := ApplyWandbProviderFlags(&cfg, fs, values); err != nil {
				t.Fatal(err)
			}
			if cfg.Wandb.DefaultImage != "" || cfg.Wandb.MaxLifetimeSeconds != life {
				t.Fatal("flag application introduced defaults/validation")
			}
		}
	}
	for _, args := range [][]string{{"--type=fixture", "--class="}, {"--type="}} {
		cfg := core.BaseConfig()
		cfg.Provider = " WEIGHTS-AND-BIASES "
		fs := flag.NewFlagSet("contract", flag.ContinueOnError)
		fs.String("class", "", "")
		fs.String("type", "", "")
		RegisterWandbProviderFlags(fs, cfg)
		if err := fs.Parse(args); err != nil {
			t.Fatal(err)
		}
		err := ApplyWandbProviderFlags(&cfg, fs, struct{}{})
		want := "--type is not supported for provider=wandb"
		if len(args) == 2 {
			want = "--class is not supported for provider=wandb"
		}
		var exitErr core.ExitError
		if err == nil || err.Error() != want || !errors.As(err, &exitErr) || exitErr.Code != 2 {
			t.Fatalf("guard err=%v want=%q", err, want)
		}
	}
}

func TestWandbBindingRuntimeDefaultsContract(t *testing.T) {
	for _, image := range []string{"", "  ", " image "} {
		for _, life := range []int{-2, 0, 37} {
			cfg := core.Config{Provider: "prior", WorkRoot: "/fixture/root", SSHUser: "fixture-user", SSHPort: "1234", SSHFallbackPorts: []string{"4567"}, Wandb: core.WandbConfig{APIKey: "inert-configured", DefaultImage: image, MaxLifetimeSeconds: life}}
			want := cfg
			want.Provider = "wandb"
			want.TargetOS = "linux"
			if image == "" {
				want.Wandb.DefaultImage = "ubuntu:24.04"
			}
			if life <= 0 {
				want.Wandb.MaxLifetimeSeconds = 1800
			}
			applyWandbDefaults(&cfg)
			if !reflect.DeepEqual(cfg, want) {
				t.Fatalf("runtime defaults image=%q life=%d got=%#v want=%#v", image, life, cfg, want)
			}
		}
	}
	cfg := core.Config{TargetOS: "macos"}
	applyWandbDefaults(&cfg)
	if cfg.TargetOS != "macos" {
		t.Fatal("nonempty target changed")
	}
	for _, tc := range []struct {
		life int
		ttl  time.Duration
		want int
	}{{0, 0, 1800}, {-2, -time.Second, 1800}, {37, 0, 37}, {0, time.Nanosecond, 1}, {0, 999 * time.Millisecond, 1}, {0, time.Second, 1}, {0, 1001 * time.Millisecond, 2}, {1, 1500 * time.Millisecond, 1}, {37, 36100 * time.Millisecond, 37}, {37, 35100 * time.Millisecond, 36}, {0, time.Hour, 1800}, {-2, time.Minute, 60}} {
		cfg := core.Config{TTL: tc.ttl, Wandb: core.WandbConfig{MaxLifetimeSeconds: tc.life}}
		if got := wandbMaxLifetimeSeconds(cfg); got != tc.want {
			t.Fatalf("life=%d ttl=%s got=%d want=%d", tc.life, tc.ttl, got, tc.want)
		}
	}
}
